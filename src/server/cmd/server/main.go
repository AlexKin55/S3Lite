package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"s3lite.dev/server/internal/config"

	"github.com/seaweedfs/seaweedfs/weed/pb/master_pb"

	"s3lite.dev/server/internal/masterapi"
	"s3lite.dev/server/internal/notify"
	"s3lite.dev/server/internal/redundancy"
	"s3lite.dev/server/internal/store"
)

// server bundles the control plane components.
type server struct {
	reg      *store.Registry
	master   *masterapi.Client
	notifier *notify.Notifier

	// Filer HTTP address, used to list the existing S3 buckets (/buckets/).
	// The "clean orphaned data" action refuses to run when the filer cannot
	// confirm the bucket list (fail-safe: no guessing which collections are
	// orphans).
	filerAddr string

	// Fixed volume parameters handed to agents at registration.
	volumeCfg VolumeTemplate

	// preferredNodesFile, when set, is kept in sync with the registry's
	// preferred (operator-trusted) nodes; the SeaweedFS master watches it
	// via -preferredNodesFile and prefers those servers for new replicas.
	preferredNodesFile string
	// preferredPolicyFile, when set, is read at startup and lists node ids
	// that are always preferred (operator-trusted, cannot be unmarked via UI).
	preferredPolicyFile string
	// adminURL is the base URL of the SeaweedFS admin server; used by the
	// manual EC encode endpoint to trigger immediate detection + dispatch
	// via the admin's plugin API.
	adminURL string

	// Volume size limit (MB) reported by the master; used to derive the
	// per-node max volume count from the node's capacity quota.
	volSizeLimitMB int

	// Heartbeat TTL used to decide the online/offline node state shown in the
	// dashboard: a node is online while its heartbeat is fresher than this.
	hbTTL time.Duration

	// Volume servers currently listed by the master; drives the cluster-wide
	// read-only policy (minWriteNodes) consumed by s3proxy.
	lastActiveNodes atomic.Int64
	// Last emitted writable verdict, for transition notifications.
	lastWritable atomic.Bool
	// Refuse S3 writes while fewer volume servers than this are online
	// (cluster read-only). 0 disables the guard. Should be >= the EC scheme's
	// data+parity shard count so a degraded cluster stops accumulating
	// single-copy data that cannot be encoded yet.
	minWriteNodes int

	// Dashboard fsck button: the weed binary to run 'weed shell volume.fsck'
	// with, the run's wall-clock cap, and the single-flight flag guarding
	// against double clicks while a minutes-long scan is in progress.
	weedBin     string
	fsckTimeout time.Duration
	fsckRunning atomic.Bool

	// Agent version tracking: the newest agent version seen in any heartbeat.
	// Nodes reporting an older version are highlighted on the dashboard.
	agentVerMu sync.Mutex
	// newestAgentVersionSeen guards under agentVerMu; empty until the first
	// heartbeat with a version string arrives.
	newestAgentVersionSeen string

	// Download errors reported by s3proxy when a client fails to download
	// an object because required EC nodes are offline. Keyed by
	// "collection:volumeID". Used by the dashboard console to show the
	// operator which nodes to power on. Entries expire after downloadErrTTL.
	downloadErrMu sync.Mutex
	downloadErrors map[string]downloadErrorInfo
	// downloadErrTTL is how long a download error is retained before being
	// forgotten (default 10 minutes).
	downloadErrTTL time.Duration

	// EC encoding operations currently in progress or recently completed.
	// Keyed by volume ID string. Updated when the manual encode endpoint is
	// called and when the admin API reports progress.
	ecOpsMu sync.Mutex
	ecOps   map[string]ecOpInfo
}

// ecOpInfo describes a single erasure-coding operation tracked by the
// control plane for dashboard visibility.
type ecOpInfo struct {
	VolumeID   *uint32 `json:"volume_id,omitempty"`
	Status     string  `json:"status"` // requested, encoding, done, failed
	StartedAt  string  `json:"started_at"`
	FinishedAt string  `json:"finished_at,omitempty"`
	Error      string  `json:"error,omitempty"`
}

// downloadErrorInfo records a failed download attempt: which volume was
// inaccessible and which nodes need to be powered on to restore it.
type downloadErrorInfo struct {
	Collection   string   `json:"collection"`
	VolumeID     uint32   `json:"volume_id"`
	NodesToStart []string `json:"nodes_to_start"`
	FirstSeen    string   `json:"first_seen"`
	LastSeen     string   `json:"last_seen"`
	Count        int      `json:"count"`
}

// VolumeTemplate is the static part of the volume config sent to agents.
type VolumeTemplate struct {
	Master     string
	DataCenter string
	RackBase   string
	MaxVolumes int
	DiskType   string
	PortBase   int
}

func main() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "server: load config: %v\n", err)
		os.Exit(1)
	}

	reg := store.NewRegistry(cfg.PortBase, time.Duration(cfg.HeartbeatTTL)*time.Second)
	notifier := notify.New(time.Hour)
	notifier.Add(notify.LogSender{})
	if cfg.WebhookURL != "" {
		notifier.Add(notify.NewWebhookSender(cfg.WebhookURL))
	}

	srv := &server{
		reg:       reg,
		master:    masterapi.New(cfg.Master, 10*time.Second),
		notifier:  notifier,
		filerAddr: cfg.Filer,
		hbTTL:     time.Duration(cfg.HeartbeatTTL) * time.Second,
		volumeCfg: VolumeTemplate{
			Master:     cfg.Master,
			DataCenter: cfg.DataCenter,
			RackBase:   cfg.RackBase,
			MaxVolumes: cfg.MaxVolumes,
			DiskType:   cfg.DiskType,
			PortBase:   cfg.PortBase,
		},
		minWriteNodes: cfg.MinWriteNodes,
		weedBin:       cfg.WeedBin,
		fsckTimeout:   cfg.FsckTimeout,
	}
	srv.preferredNodesFile = cfg.PreferredNodesFile
	srv.preferredPolicyFile = cfg.PreferredPolicyFile
	srv.adminURL = cfg.AdminURL
	srv.downloadErrors = make(map[string]downloadErrorInfo)
	srv.downloadErrTTL = 10 * time.Minute
	srv.ecOps = make(map[string]ecOpInfo)
	if cfg.StateFile != "" {
		reg.SetStatePath(cfg.StateFile)
	}
	if cfg.PreferredPolicyFile != "" {
		// Load operator-declared preferred node ids from /etc/s3lite/policy.conf.
		// These are sticky: they cannot be unmarked via the web UI.
		reg.LoadPreferredPolicy(cfg.PreferredPolicyFile)
	}
	if srv.preferredNodesFile != "" {
		// Publish the loaded state so the master picks up the preference
		// without waiting for the first toggle.
		srv.writePreferredNodesFile()
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.monitorLoop(ctx, time.Duration(cfg.PollSec)*time.Second)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/nodes", srv.handleNodes)
	mux.HandleFunc("/api/v1/nodes/preferred", srv.handleNodesPreferred)
	mux.HandleFunc("/api/v1/nodes/", srv.handleNodePath)
	mux.HandleFunc("/api/v1/cluster/status", srv.handleClusterStatus)
	mux.HandleFunc("/api/v1/download/chains", srv.handleDownloadChains)
	mux.HandleFunc("/api/v1/download/errors", srv.handleDownloadErrors)
	mux.HandleFunc("/api/v1/volumes/risk", srv.handleVolumesRisk)
	mux.HandleFunc("/api/v1/admin/notify/test", srv.handleNotifyTest)
	mux.HandleFunc("/api/v1/maintenance/encode", srv.handleMaintenanceEncode)
	mux.HandleFunc("/api/v1/maintenance/encode/status", srv.handleEncodeStatus)
	mux.HandleFunc("/api/v1/dashboard", srv.dashboardAPI)
	mux.HandleFunc("/api/v1/orphans", srv.handleOrphansList)
	mux.HandleFunc("/api/v1/orphans/cleanup", srv.handleOrphansCleanup)
	mux.HandleFunc("/api/v1/fsck", srv.handleFsck)
	mux.HandleFunc("/", srv.handleDashboard)
	mux.HandleFunc("/metrics", srv.handleMetrics)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	scheme := "http"
	if cfg.TLSCert != "" {
		scheme = "https"
	}
	log.Printf("control plane on %s://%s, master=%s, poll=%ds, hbTTL=%ds",
		scheme, cfg.Listen, cfg.Master, cfg.PollSec, cfg.HeartbeatTTL)
	if cfg.TLSCert != "" {
		err = http.ListenAndServeTLS(cfg.Listen, cfg.TLSCert, cfg.TLSKey, mux)
	} else {
		err = http.ListenAndServe(cfg.Listen, mux)
	}
	if err != nil {
		if opErr, ok := err.(*net.OpError); ok {
			if errors.Is(opErr.Err, syscall.EADDRINUSE) {
				_, port, errSplit := net.SplitHostPort(cfg.Listen)
				if errSplit == nil {
					proc := listeningProcess(port)
					log.Fatalf("control plane on %s://%s: bind: address %s already in use (process: %s)", scheme, cfg.Listen, cfg.Listen, proc)
				}
			}
		}
		log.Fatal(err)
	}
}

// monitorLoop polls the master topology and updates the writable/read-only state.
func (s *server) monitorLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(time.Second) // first run immediately
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			ticker.Reset(interval)
			s.evaluate(ctx)
		}
	}
}

func (s *server) evaluate(ctx context.Context) {
	topo, limitMB, err := s.master.Topology(ctx)
	if err != nil {
		log.Printf("monitor: topology fetch failed: %v", err)
		return
	}
	s.volSizeLimitMB = int(limitMB)
	activeNodes := countTopologyNodes(topo)
	s.lastActiveNodes.Store(int64(activeNodes))
	s.notifyWritableChange(activeNodes)
}

// countTopologyNodes returns how many volume servers the master currently lists.
func countTopologyNodes(topo *master_pb.TopologyInfo) int {
	n := 0
	for _, dci := range topo.GetDataCenterInfos() {
		for _, rack := range dci.GetRackInfos() {
			n += len(rack.GetDataNodeInfos())
		}
	}
	return n
}

// clusterWritable implements the read-only policy: writes are allowed while at
// least minWriteNodes volume servers are online. minWriteNodes <= 0 disables
// the guard (always writable).
func clusterWritable(activeNodes, minWriteNodes int) bool {
	return minWriteNodes <= 0 || activeNodes >= minWriteNodes
}

// notifyWritableChange emits a notification when the cluster crosses the
// read-only threshold in either direction.
func (s *server) notifyWritableChange(activeNodes int) {
	writable := clusterWritable(activeNodes, s.minWriteNodes)
	if s.lastWritable.Swap(writable) == writable {
		return
	}
	if writable {
		s.notifier.Notify("cluster_writable", notify.Event{
			Kind:    "cluster_writable",
			Summary: "Cluster is writable again",
			Detail:  fmt.Sprintf("%d volume servers online (threshold %d).", activeNodes, s.minWriteNodes),
			At:      time.Now(),
		})
	} else {
		s.notifier.Notify("cluster_writable", notify.Event{
			Kind:    "cluster_read_only",
			Summary: "Cluster is now READ-ONLY: not enough nodes",
			Detail: fmt.Sprintf("Only %d volume servers online, writes are refused until %d are back.",
				activeNodes, s.minWriteNodes),
			At: time.Now(),
		})
	}
}

// ---- HTTP handlers ----

// POST /api/v1/nodes — register; GET /api/v1/nodes — list.
func (s *server) handleNodes(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, map[string]any{"nodes": s.nodeViews()})
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		NodeID        string `json:"node_id"`
		CapacityBytes int64  `json:"capacity_bytes"`
		Zone          string `json:"zone"`
		IP            string `json:"ip"`
		OS            string `json:"os"`
		OSVersion     string `json:"os_version"`
		Arch          string `json:"arch"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.NodeID == "" {
		http.Error(w, "node_id required", http.StatusBadRequest)
		return
	}
	n, isNew := s.reg.Register(req.NodeID, req.CapacityBytes, req.Zone, req.IP, req.OS, req.OSVersion, req.Arch)
	// Remember the TCP source address: several distinct source IPs for one
	// node_id mean several machines share it (e.g. cloned VM images); the
	// dashboard shows a warning banner in that case.
	s.reg.RecordSourceIP(req.NodeID, clientIP(r))
	if isNew {
		log.Printf("node registered: %s (%s) os=%s/%s arch=%s rack=%s port=%d zone=%s",
			n.NodeID, n.IP, req.OS, req.OSVersion, req.Arch, n.Rack, n.VolumePort, n.Zone)
	}
	s.notifier.Notify("node_registered_"+n.NodeID, notify.Event{
		Kind:    "node_registered",
		Summary: "Node registered: " + n.NodeID,
		Detail:  fmt.Sprintf("%s rack=%s port=%d", n.IP, n.Rack, n.VolumePort),
		At:      time.Now(),
	})
	// Publish the preferred nodes list for the master's -preferredNodesFile.
	// A node may carry a preferred flag from the persisted state file
	// (preferredPending), so the file must be updated on every registration,
	// not only at startup or on manual toggle.
	s.writePreferredNodesFile()
	// Derive the per-node volume count from the node's capacity quota and the
	// master's volume size limit, so a node can never store more than its
	// configured STORAGE_SIZE (maxVolumes * volumeSizeLimitMB <= capacity).
	maxVol := s.volumeCfg.MaxVolumes
	if s.volSizeLimitMB > 0 && req.CapacityBytes > 0 {
		if n := int(req.CapacityBytes / (int64(s.volSizeLimitMB) * 1024 * 1024)); n > 0 {
			maxVol = n
		} else {
			// Capacity below one volume: grant a single slot instead of
			// silently falling back to the default maxVolumes, which would
			// let the node hold maxVolumes * volumeSizeLimitMB — far beyond
			// its quota. The one volume can still overflow the quota; fix
			// the config (raise the node's STORAGE_SIZE, or recompile weed
			// with a smaller DefaultVolumeSizeLimitMB).
			maxVol = 1
			log.Printf("node %s: capacity %d bytes is below one volume (%d MB); granting 1 volume slot, quota may still be exceeded",
				req.NodeID, req.CapacityBytes, s.volSizeLimitMB)
		}
	}
	// Store the max volume count on the node so the dashboard can show slot
	// capacity alongside current usage.
	s.reg.SetMaxVolumes(n.NodeID, maxVol)
	writeJSON(w, http.StatusOK, map[string]any{
		"node_id": n.NodeID,
		"volume": map[string]any{
			"master":            s.volumeCfg.Master,
			"data_center":       s.volumeCfg.DataCenter,
			"rack":              n.Rack,
			"max_volumes":       maxVol,
			"volume_size_limit_mb": s.volSizeLimitMB,
			"disk_type":         s.volumeCfg.DiskType,
			"weed_url":          "",
			"volume_port":       n.VolumePort,
		},
	})
}

func (s *server) handleNodePath(w http.ResponseWriter, r *http.Request) {
	parts := splitPath(r.URL.Path)
	if len(parts) < 4 { // api/v1/nodes/<id>
		http.NotFound(w, r)
		return
	}
	nodeID := parts[3]
	if len(parts) == 4 {
		switch r.Method {
		case http.MethodGet:
			s.handleNodeGet(w, r, nodeID)
		case http.MethodDelete:
			s.handleNodeDelete(w, r, nodeID)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}
	switch parts[4] {
	case "heartbeat":
		s.handleHeartbeat(w, r, nodeID)
	case "config":
		s.handleNodeConfig(w, r, nodeID)
	case "preferred":
		s.handleNodePreferred(w, r, nodeID)
	case "agent":
		if len(parts) == 6 && parts[5] == "update" {
			s.handleAgentUpdate(w, r, nodeID)
			return
		}
		http.NotFound(w, r)
	case "restart":
		s.handleNodeRestart(w, r, nodeID)
	default:
		http.NotFound(w, r)
	}
}

// POST /api/v1/nodes/{id}/agent/update — schedule an agent self-update.
// The update_agent command is delivered to the agent with its next heartbeat
// response; the agent then reinstalls the s3lite-agent package in place
// (preserving the node configuration) and restarts its service.
func (s *server) handleAgentUpdate(w http.ResponseWriter, r *http.Request, nodeID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	n, ok := s.reg.RequestAgentUpdate(nodeID)
	if !ok {
		http.Error(w, "unknown node", http.StatusNotFound)
		return
	}
	log.Printf("agent update requested: %s (reported version %s)", nodeID, n.AgentVersion)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "node_id": nodeID})
}

// POST /api/v1/nodes/{id}/restart — schedule an agent + volume server
// restart on the node. The restart_agent command is delivered with the
// node's next heartbeat response; the agent restarts itself and the volume
// server (weed volume) in place.
func (s *server) handleNodeRestart(w http.ResponseWriter, r *http.Request, nodeID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	n, ok := s.reg.RequestNodeRestart(nodeID)
	if !ok {
		http.Error(w, "unknown node", http.StatusNotFound)
		return
	}
	log.Printf("node restart requested: %s (IP %s, version %s)", nodeID, n.IP, n.AgentVersion)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "node_id": nodeID})
}

// POST /api/v1/nodes/{id}/preferred — toggle the preferred-replica flag.
// Preferred ("reliable") nodes receive new volume replicas first during the
// pre-EC phase; the master falls back to regular nodes when they run out of
// free slots. The flag list is published to the master via the file set with
// -preferredNodesFile; the master re-reads it periodically (no restart).
func (s *server) handleNodePreferred(w http.ResponseWriter, r *http.Request, nodeID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Preferred *bool `json:"preferred"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Preferred == nil {
		http.Error(w, `bad request: need {"preferred": true|false}`, http.StatusBadRequest)
		return
	}
	// Policy-preferred nodes (from /etc/s3lite/policy.conf) cannot be
	// unmarked through the web UI.
	if !*req.Preferred && s.reg.IsPolicyPreferred(nodeID) {
		http.Error(w, "policy-preferred node cannot be unmarked", http.StatusForbidden)
		return
	}
	if !s.reg.SetPreferred(nodeID, *req.Preferred) {
		http.Error(w, "unknown node", http.StatusNotFound)
		return
	}
	log.Printf("node %s preferred=%v", nodeID, *req.Preferred)
	s.writePreferredNodesFile()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "node_id": nodeID, "preferred": *req.Preferred})
}

// GET /api/v1/nodes/preferred — list preferred node addresses (ip:port).
// Used by volume servers to know which nodes to prefer for EC reads and
// parity reconstruction offload.
func (s *server) handleNodesPreferred(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	addrs := s.reg.PreferredNodeAddrs()
	writeJSON(w, http.StatusOK, map[string]any{"addresses": addrs})
}

// POST /api/v1/maintenance/encode — triggers immediate EC encoding + shard
// distribution via the admin's plugin API (detect + run for erasure_coding).
// Bypasses the 5-minute quiet period and the worker's 5-minute scan interval.
func (s *server) handleMaintenanceEncode(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	adminURL := s.adminURL
	if adminURL == "" {
		adminURL = "http://127.0.0.1:12535"
	}
	adminURL = strings.TrimRight(adminURL, "/")

	reqBody, _ := json.Marshal(map[string]any{})
	httpReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost,
		adminURL+"/api/plugin/job-types/erasure_coding/run", bytes.NewReader(reqBody))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "admin request failed: " + err.Error()})
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "read admin response failed: " + err.Error()})
		return
	}

	if resp.StatusCode != http.StatusOK {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "admin returned " + resp.Status + ": " + string(body)})
		return
	}

	var adminResp map[string]any
	if err := json.Unmarshal(body, &adminResp); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"raw": string(body)})
		return
	}
	opID := fmt.Sprintf("enc-%d", time.Now().UnixNano())
	op := ecOpInfo{
		Status:    "requested",
		StartedAt: time.Now().Format(time.RFC3339),
	}
	if volID, ok := adminResp["volume_id"]; ok {
		if v, err := parseVolumeID(volID); err == nil {
			op.VolumeID = &v
		}
	}
	if errStr, ok := adminResp["error"]; ok {
		op.Status = "failed"
		op.Error = fmt.Sprint(errStr)
		op.FinishedAt = time.Now().Format(time.RFC3339)
	}
	s.ecOpsMu.Lock()
	s.ecOps[opID] = op
	s.ecOpsMu.Unlock()

	writeJSON(w, http.StatusOK, adminResp)
}

// GET /api/v1/maintenance/encode/status — returns the list of erasure-coding
// operations tracked by the control plane (requested, in progress, done,
// failed).
func (s *server) handleEncodeStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.ecOpsMu.Lock()
	ops := make([]ecOpInfo, 0, len(s.ecOps))
	for _, op := range s.ecOps {
		ops = append(ops, op)
	}
	s.ecOpsMu.Unlock()
	sort.Slice(ops, func(i, j int) bool {
		return ops[i].StartedAt > ops[j].StartedAt
	})
	writeJSON(w, http.StatusOK, map[string]any{"operations": ops})
}

// writePreferredNodesFile publishes the preferred node address list for the
// SeaweedFS master (-preferredNodesFile watches it).
func (s *server) writePreferredNodesFile() {
	if s.preferredNodesFile == "" {
		return
	}
	addrs := s.reg.PreferredNodeAddrs()
	var b strings.Builder
	b.WriteString("# written by s3lite-server: preferred replica targets\n")
	for _, a := range addrs {
		b.WriteString(a)
		b.WriteString("\n")
	}
	if err := os.WriteFile(s.preferredNodesFile, []byte(b.String()), 0o644); err != nil {
		log.Printf("write preferred nodes file %s: %v", s.preferredNodesFile, err)
		return
	}
	log.Printf("preferred nodes list published: %d node(s) -> %s", len(addrs), s.preferredNodesFile)
}

// POST /api/v1/nodes/{id}/heartbeat.
func (s *server) handleHeartbeat(w http.ResponseWriter, r *http.Request, nodeID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Status            string `json:"status"`
		DiskFree          int64  `json:"disk_free"`
		DiskUsed          int64  `json:"disk_used"`
		VolumeServerAlive bool   `json:"volume_server_alive"`
		VolumesCount      int    `json:"volumes_count"`
		EcShardsCount     int    `json:"ec_shards_count"`
		CpuLoad           int    `json:"cpu_load"`
		AgentVersion      string `json:"agent_version"`
		WeedVersion       string `json:"weed_version"`
		AgentUptimeSec    int64  `json:"agent_uptime_sec"`
		VolumeUptimeSec   int64  `json:"volume_uptime_sec"`
		RestartsTotal     int    `json:"restarts_total"`
		StatsAgeSec       int64  `json:"stats_age_sec"`
		LogTail           string `json:"log_tail"`
		UpdateStatus      string `json:"update_status"`
		UpdateDetail      string `json:"update_detail"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	// See handleRegister: track where heartbeats for this node_id come from.
	s.reg.RecordSourceIP(nodeID, clientIP(r))
	_, ok := s.reg.UpdateHeartbeat(nodeID, store.HeartbeatUpdate{
		Status:            req.Status,
		DiskFree:          req.DiskFree,
		DiskUsed:          req.DiskUsed,
		VolumeServerAlive: req.VolumeServerAlive,
		VolumesCount:      req.VolumesCount,
		EcShardsCount:     req.EcShardsCount,
		CpuLoad:           req.CpuLoad,
		AgentVersion:      req.AgentVersion,
		WeedVersion:       req.WeedVersion,
		AgentUptimeSec:    req.AgentUptimeSec,
		VolumeUptimeSec:   req.VolumeUptimeSec,
		RestartsTotal:     req.RestartsTotal,
		StatsAgeSec:       req.StatsAgeSec,
		LogTail:           req.LogTail,
		UpdateStatus:      req.UpdateStatus,
		UpdateDetail:      req.UpdateDetail,
	})
	if !ok {
		http.Error(w, "unknown node", http.StatusNotFound)
		return
	}
	// Track the newest agent version across the cluster: nodes reporting an
	// older one are highlighted on the dashboard.
	s.observeAgentVersion(req.AgentVersion)
	resp := map[string]any{"ok": true}
	// Deliver a pending agent-update command exactly once: the agent
	// reinstalls its package (configs preserved) and restarts its service.
	if s.reg.TakeAgentUpdateCommand(nodeID) {
		resp["commands"] = []string{"update_agent"}
	}
	// Deliver a pending restart command exactly once: the agent restarts
	// itself and the volume server in place.
	if s.reg.TakeRestartCommand(nodeID) {
		if cmds, ok := resp["commands"].([]string); ok {
			resp["commands"] = append(cmds, "restart_agent")
		} else {
			resp["commands"] = []string{"restart_agent"}
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// GET /api/v1/nodes/{id}/config.
func (s *server) handleNodeConfig(w http.ResponseWriter, r *http.Request, nodeID string) {
	n, ok := s.reg.Get(nodeID)
	if !ok {
		http.Error(w, "unknown node", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"volume": map[string]any{
			"master":      s.volumeCfg.Master,
			"data_center": s.volumeCfg.DataCenter,
			"rack":        n.Rack,
			"max_volumes": s.volumeCfg.MaxVolumes,
			"disk_type":   s.volumeCfg.DiskType,
			"weed_url":    "",
			"volume_port": n.VolumePort,
		},
	})
}

// GET /api/v1/nodes/{id} — single node with computed online/offline state.
func (s *server) handleNodeGet(w http.ResponseWriter, r *http.Request, nodeID string) {
	n, ok := s.reg.Get(nodeID)
	if !ok {
		http.Error(w, "unknown node", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, s.nodeView(n))
}

// DELETE /api/v1/nodes/{id} — permanently remove a node from the registry.
// A deleted node disappears from the dashboard entirely (also when offline)
// and re-appears only after its agent registers again.
func (s *server) handleNodeDelete(w http.ResponseWriter, r *http.Request, nodeID string) {
	if !s.reg.Delete(nodeID) {
		http.Error(w, "unknown node", http.StatusNotFound)
		return
	}
	log.Printf("node deleted by admin: %s", nodeID)
	s.notifier.Notify("node_deleted_"+nodeID, notify.Event{
		Kind:    "node_deleted",
		Summary: "Node deleted: " + nodeID,
		Detail:  "Node was removed from the control plane by an administrator. It will re-appear only if its agent registers again.",
		At:      time.Now(),
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// GET /api/v1/cluster/status.
func (s *server) handleClusterStatus(w http.ResponseWriter, r *http.Request) {
	totalCap, usedCap := int64(0), int64(0)
	for _, n := range s.reg.All() {
		totalCap += n.CapacityBytes
		usedCap += n.DiskUsed
	}
	activeNodes := int(s.lastActiveNodes.Load())
	writeJSON(w, http.StatusOK, map[string]any{
		"nodes":           len(s.reg.All()),
		"active_nodes":    activeNodes,
		"min_write_nodes": s.minWriteNodes,
		"writable":        clusterWritable(activeNodes, s.minWriteNodes),
		"capacity": map[string]int64{
			"total_bytes": totalCap,
			"used_bytes":  usedCap,
			"free_bytes":  totalCap - usedCap,
		},
	})
}

// GET /api/v1/download/chains — reactive: calculate node chains to power on
// for a volume that failed to download due to offline nodes.
// Query params: collection, volume_id.
func (s *server) handleDownloadChains(w http.ResponseWriter, r *http.Request) {
	collection := r.URL.Query().Get("collection")
	volumeStr := r.URL.Query().Get("volume_id")
	if collection == "" || volumeStr == "" {
		http.Error(w, "collection and volume_id required", http.StatusBadRequest)
		return
	}
	var volumeID uint32
	_, err := fmt.Sscanf(volumeStr, "%d", &volumeID)
	if err != nil {
		http.Error(w, "invalid volume_id", http.StatusBadRequest)
		return
	}
	topo, _, err := s.master.Topology(r.Context())
	if err != nil {
		log.Printf("download/chains: topology fetch failed: %v", err)
		http.Error(w, "topology fetch failed", http.StatusInternalServerError)
		return
	}
	volumes := redundancy.Collect(topo)
	online := make(map[string]bool)
	for _, n := range s.reg.All() {
		if s.nodeStatus(n) == "online" {
			online[n.IP] = true
		}
	}
	chains := redundancy.CalcDownloadChains(volumes, online, collection, volumeID)
	writeJSON(w, http.StatusOK, map[string]any{"chains": chains})
}

// POST /api/v1/download/errors — s3proxy reports a failed download here so
// the dashboard console can show the operator which nodes to power on.
// Body: {"collection": "bucket", "volume_id": 123, "nodes_to_start": ["node1", "node2"]}.
func (s *server) handleDownloadErrors(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Collection    string   `json:"collection"`
		VolumeID      uint32   `json:"volume_id"`
		NodesToStart  []string `json:"nodes_to_start"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Collection == "" || req.VolumeID == 0 {
		http.Error(w, "collection and volume_id required", http.StatusBadRequest)
		return
	}
	key := req.Collection + ":" + strconv.Itoa(int(req.VolumeID))
	now := time.Now().UTC().Format(time.RFC3339)
	s.downloadErrMu.Lock()
	info, exists := s.downloadErrors[key]
	if !exists {
		info = downloadErrorInfo{
			Collection:   req.Collection,
			VolumeID:     req.VolumeID,
			NodesToStart: req.NodesToStart,
			FirstSeen:    now,
			Count:        0,
		}
	}
	info.NodesToStart = req.NodesToStart
	info.LastSeen = now
	info.Count++
	s.downloadErrors[key] = info
	s.downloadErrMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// GET /api/v1/volumes/risk — reactive: check EC volume download risk.
// With collection+volume_id query params, checks only that volume
// (point-check before download). Without params, returns all AT_RISK
// volumes (used by s3proxy on download failure when volume info is
// unavailable). No background state — topology and redundancy are
// calculated on each request.
func (s *server) handleVolumesRisk(w http.ResponseWriter, r *http.Request) {
	topo, _, err := s.master.Topology(r.Context())
	if err != nil {
		log.Printf("volumes/risk: topology fetch failed: %v", err)
		http.Error(w, "topology fetch failed", http.StatusInternalServerError)
		return
	}
	volumes := redundancy.Collect(topo)
	online := make(map[string]bool)
	for _, n := range s.reg.All() {
		if s.nodeStatus(n) == "online" {
			online[n.IP] = true
		}
	}
	collection := r.URL.Query().Get("collection")
	volumeStr := r.URL.Query().Get("volume_id")

	pointCheck := collection != "" || volumeStr != ""

	out := make([]map[string]any, 0)
	for _, v := range volumes {
		// Point-check mode: only the requested volume.
		if collection != "" && v.Collection != collection {
			continue
		}
		if volumeStr != "" {
			var volumeID uint32
			if _, err := fmt.Sscanf(volumeStr, "%d", &volumeID); err != nil {
				http.Error(w, "invalid volume_id", http.StatusBadRequest)
				return
			}
			if v.VolumeID != volumeID {
				continue
			}
		}

		available := 0
		for node, shards := range v.ShardsByNode {
			if online[node] {
				available += len(shards)
			}
		}
		if available >= v.DataShards {
			// Volume is downloadable. In point-check mode report "no risk".
			if pointCheck {
				writeJSON(w, http.StatusOK, map[string]any{"volumes": []map[string]any{}})
				return
			}
			continue
		}
		chains := redundancy.CalcDownloadChains(volumes, online, v.Collection, v.VolumeID)
		nodesToStart := []string{}
		if len(chains) > 0 {
			nodesToStart = chains[0].Nodes
		}
		out = append(out, map[string]any{
			"state":         "AT_RISK",
			"collection":    v.Collection,
			"volume_id":     v.VolumeID,
			"nodes_to_start": nodesToStart,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"volumes": out})
}

// POST /api/v1/admin/notify/test.
func (s *server) handleNotifyTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.notifier.Notify("test_"+time.Now().String(), notify.Event{
		Kind:    "test",
		Summary: "Test notification from S3Lite control plane",
		Detail:  "If you see this, the notification channels work.",
		At:      time.Now(),
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// GET /metrics — lightweight text metrics.
func (s *server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	activeNodes := int(s.lastActiveNodes.Load())
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprintf(w, "# HELP s3lite_nodes_registered Registered nodes in the control plane.\n")
	fmt.Fprintf(w, "# TYPE s3lite_nodes_registered gauge\n")
	fmt.Fprintf(w, "s3lite_nodes_registered %d\n", len(s.reg.All()))
	fmt.Fprintf(w, "# HELP s3lite_active_volume_servers Volume servers currently listed by the master.\n")
	fmt.Fprintf(w, "# TYPE s3lite_active_volume_servers gauge\n")
	fmt.Fprintf(w, "s3lite_active_volume_servers %d\n", activeNodes)
	fmt.Fprintf(w, "# HELP s3lite_cluster_writable Whether S3 writes are currently allowed (min_write_nodes policy).\n")
	fmt.Fprintf(w, "# TYPE s3lite_cluster_writable gauge\n")
	if clusterWritable(activeNodes, s.minWriteNodes) {
		fmt.Fprintf(w, "s3lite_cluster_writable 1\n")
	} else {
		fmt.Fprintf(w, "s3lite_cluster_writable 0\n")
	}
}

// ---- helpers ----

func splitPath(p string) []string {
	var parts []string
	cur := ""
	for _, c := range p {
		if c == '/' {
			if cur != "" {
				parts = append(parts, cur)
				cur = ""
			}
			continue
		}
		cur += string(c)
	}
	if cur != "" {
		parts = append(parts, cur)
	}
	return parts
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// nodeStatus computes the agent state shown in the dashboard: "online" while
// the agent heartbeat is fresh (within the TTL), otherwise "offline".
func (s *server) nodeStatus(n *store.Node) string {
	if time.Since(n.LastHeartbeat) <= s.hbTTL {
		return "online"
	}
	return "offline"
}

// volumeStatus computes the volume server state reported by the agent:
// "online" when the volume server answers /status, "offline" otherwise.
// Independent of agent heartbeat freshness so the dashboard can distinguish
// "agent online, volume dead" from "agent offline".
func (s *server) volumeStatus(n *store.Node) string {
	if n.Alive {
		return "online"
	}
	return "offline"
}

// nodeView returns a snapshot copy of the node with recomputed agent and
// volume statuses. Agent status comes from heartbeat freshness; volume
// status comes from the agent's last VolumeServerAlive report.
func (s *server) nodeView(n *store.Node) store.Node {
	v := *n
	v.Status = s.nodeStatus(n)
	v.VolumeStatus = s.volumeStatus(n)
	v.Alive = v.Status == "online"
	// A node is flagged when its agent reports a version older than the
	// newest one observed anywhere in the cluster (see observeAgentVersion).
	if latest := s.newestAgentVersion(); latest != "" && n.AgentVersion != "" && versionLess(n.AgentVersion, latest) {
		v.AgentOutdated = true
	}
	return v
}

// nodeViews returns a snapshot of all registered nodes with computed state.
// clientIP extracts the remote client IP from the request (port stripped).
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// listeningProcess returns "pid cmdline" of the process listening on the given
// TCP port, by scanning /proc/net/tcp and /proc/net/tcp6. Empty string when the
// process cannot be determined.
func listeningProcess(port string) string {
	inodes := tcpListeningInodes(port)
	if len(inodes) == 0 {
		return ""
	}
	pid := findPidBySocketInode(inodes)
	if pid == "" {
		return "unknown"
	}
	cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%s/cmdline", pid))
	if err != nil {
		return pid
	}
	parts := bytes.Split(cmdline, []byte{0})
	cmd := ""
	for _, p := range parts {
		if len(p) > 0 {
			if cmd != "" {
				cmd += " "
			}
			cmd += string(p)
		}
	}
	if cmd == "" {
		cmd = fmt.Sprintf("pid %s", pid)
	}
	return fmt.Sprintf("%s (%s)", pid, cmd)
}

// tcpListeningInodes parses /proc/net/tcp and /proc/net/tcp6 and returns the
// inode numbers of sockets in LISTEN state on the given port.
func tcpListeningInodes(port string) []string {
	var inodes []string
	portHex := fmt.Sprintf("%04X", mustParsePort(port))
	for _, file := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		data, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 10 {
				continue
			}
			localAddr := fields[1]
			state := fields[3]
			inode := fields[9]
			if state != "0A" { // LISTEN
				continue
			}
			parts := strings.Split(localAddr, ":")
			if len(parts) != 2 {
				continue
			}
			if strings.EqualFold(parts[1], portHex) {
				inodes = append(inodes, inode)
			}
		}
	}
	return inodes
}

func mustParsePort(port string) int {
	n, err := strconv.Atoi(port)
	if err != nil {
		return 0
	}
	return n
}

// findPidBySocketInode scans /proc/[0-9]*/fd for symlinks whose target matches
// "socket:[inode]" for any of the given inodes.
func findPidBySocketInode(inodes []string) string {
	inodeSet := make(map[string]struct{}, len(inodes))
	for _, ino := range inodes {
		inodeSet[ino] = struct{}{}
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		fds, err := os.ReadDir(fmt.Sprintf("/proc/%s/fd", e.Name()))
		if err != nil {
			continue
		}
		for _, fd := range fds {
			target, err := os.Readlink(fmt.Sprintf("/proc/%s/fd/%s", e.Name(), fd.Name()))
			if err != nil {
				continue
			}
			if target == "" {
				continue
			}
			for ino := range inodeSet {
				if strings.Contains(target, fmt.Sprintf("socket:[%s]", ino)) {
					return e.Name()
				}
			}
		}
	}
	return ""
}

func (s *server) nodeViews() []store.Node {
	all := s.reg.All()
	views := make([]store.Node, 0, len(all))
	for _, n := range all {
		views = append(views, s.nodeView(n))
	}
	return views
}

// parseVolumeID extracts a numeric volume ID from an interface{} value
// returned by the admin API (may be float64, string, or json.Number).
func parseVolumeID(v any) (uint32, error) {
	switch n := v.(type) {
	case float64:
		return uint32(n), nil
	case string:
		var id uint64
		if _, err := fmt.Sscanf(n, "%d", &id); err != nil {
			return 0, err
		}
		return uint32(id), nil
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return 0, err
		}
		return uint32(i), nil
	default:
		return 0, fmt.Errorf("unsupported volume id type: %T", v)
	}
}
