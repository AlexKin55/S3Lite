package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"sync/atomic"
	"time"

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

	// Fixed volume parameters handed to agents at registration.
	volumeCfg VolumeTemplate

	// Latest redundancy evaluation snapshot.
	lastReports atomic.Value // []redundancy.Report
	lastUpdate  atomic.Value // time.Time
	lastTopoEC  atomic.Int64 // number of EC volumes in the last snapshot
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
	listen := flag.String("listen", ":8099", "control plane HTTP listen address")
	masterHTTP := flag.String("master", "192.168.1.9:9333", "SeaweedFS master HTTP address")
	pollSec := flag.Int("poll", 15, "redundancy poll interval (seconds)")
	hbTTL := flag.Int("heartbeatTTL", 60, "agent heartbeat TTL (seconds)")
	webhookURL := flag.String("webhook", "", "webhook URL for notifications (empty = log only)")
	dc := flag.String("dataCenter", "dc1", "data center name for volume servers")
	rackBase := flag.String("rackBase", "rack", "rack name prefix")
	maxVol := flag.Int("maxVolumes", 8, "max volumes for volume servers")
	diskType := flag.String("diskType", "hdd", "disk type for volume servers")
	portBase := flag.Int("portBase", 8080, "starting port for agent volume servers")
	flag.Parse()

	reg := store.NewRegistry(*portBase, time.Duration(*hbTTL)*time.Second)
	notifier := notify.New(time.Hour)
	notifier.Add(notify.LogSender{})
	if *webhookURL != "" {
		notifier.Add(notify.NewWebhookSender(*webhookURL))
	}

	srv := &server{
		reg:      reg,
		master:   masterapi.New(*masterHTTP, 10*time.Second),
		notifier: notifier,
		volumeCfg: VolumeTemplate{
			Master:     *masterHTTP,
			DataCenter: *dc,
			RackBase:   *rackBase,
			MaxVolumes: *maxVol,
			DiskType:   *diskType,
			PortBase:   *portBase,
		},
	}
	srv.lastReports.Store([]*redundancy.Report{})
	srv.lastUpdate.Store(time.Time{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.monitorLoop(ctx, time.Duration(*pollSec)*time.Second)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/nodes", srv.handleNodes)
	mux.HandleFunc("/api/v1/nodes/", srv.handleNodePath)
	mux.HandleFunc("/api/v1/cluster/status", srv.handleClusterStatus)
	mux.HandleFunc("/api/v1/volumes/risk", srv.handleVolumesRisk)
	mux.HandleFunc("/api/v1/admin/notify/test", srv.handleNotifyTest)
	mux.HandleFunc("/metrics", srv.handleMetrics)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	log.Printf("control plane on %s, master=%s, poll=%ds, hbTTL=%ds", *listen, *masterHTTP, *pollSec, *hbTTL)
	if err := http.ListenAndServe(*listen, mux); err != nil {
		log.Fatal(err)
	}
}

// monitorLoop polls the master topology and evaluates redundancy.
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
	topo, _, err := s.master.Topology(ctx)
	if err != nil {
		log.Printf("monitor: topology fetch failed: %v", err)
		return
	}
	online := s.buildOnline(topo)
	volumes := redundancy.Collect(topo)
	reports := redundancy.Evaluate(volumes, func(addr string) bool { return online[addr] })

	prev := s.lastReports.Load().([]*redundancy.Report)
	s.lastReports.Store(reports)
	s.lastUpdate.Store(time.Now())
	s.lastTopoEC.Store(int64(len(volumes)))

	// Notify on AT_RISK transitions and on changed recommendation sets.
	for i := range reports {
		r := reports[i]
		if r.State != redundancy.StateAtRisk {
			continue
		}
		var prevState string
		var prevNodes []string
		prevState = ""
		for j := range prev {
			if prev[j].Collection == r.Collection && prev[j].VolumeID == r.VolumeID {
				prevState = prev[j].State
				prevNodes = prev[j].NodesToStart
			}
		}
		changed := prevState != redundancy.StateAtRisk ||
			!sameStrings(prevNodes, r.NodesToStart)
		if !changed {
			continue
		}
		key := fmt.Sprintf("%s/%d/atrisk", r.Collection, r.VolumeID)
		s.notifier.Notify(key, notify.Event{
			Kind:    "volume_at_risk",
			Summary: fmt.Sprintf("Volume %d (%s) AT_RISK: %d/%d shards available",
				r.VolumeID, r.Collection, r.Available, r.Total),
			Detail: fmt.Sprintf("Data is unrecoverable right now. Restore by turning on: %v",
				r.NodesToStart),
			At: time.Now(),
		})
	}
}

// buildOnline returns node address -> online judgment. Store nodes are judged
// by their agent heartbeat (fresh and alive); topology-only nodes (running
// without an agent) are considered online while the master lists them.
func (s *server) buildOnline(topo *master_pb.TopologyInfo) map[string]bool {
	// Node addresses present in the topology.
	inTopo := make(map[string]bool)
	for _, dci := range topo.GetDataCenterInfos() {
		for _, rack := range dci.GetRackInfos() {
			for _, dn := range rack.GetDataNodeInfos() {
				addr := dn.GetId()
				if addr == "" {
					addr = dn.GetAddress()
				}
				inTopo[addr] = true
			}
		}
	}

	online := make(map[string]bool)
	now := time.Now()
	for _, n := range s.reg.All() {
		addr := fmt.Sprintf("%s:%d", n.IP, n.VolumePort)
		if now.Sub(n.LastHeartbeat) <= time.Duration(60)*time.Second && n.Alive {
			online[addr] = true
		}
	}
	// Topology-only nodes are treated as online.
	for addr := range inTopo {
		if _, known := online[addr]; !known {
			online[addr] = true
		}
	}
	return online
}

// ---- HTTP handlers ----

// POST /api/v1/nodes — register; GET /api/v1/nodes — list.
func (s *server) handleNodes(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, map[string]any{"nodes": s.reg.All()})
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
	n, isNew := s.reg.Register(req.NodeID, req.CapacityBytes, req.Zone, req.IP, req.OS, req.Arch)
	if isNew {
		log.Printf("node registered: %s (%s) rack=%s port=%d zone=%s",
			n.NodeID, n.IP, n.Rack, n.VolumePort, n.Zone)
	}
	s.notifier.Notify("node_registered_"+n.NodeID, notify.Event{
		Kind:    "node_registered",
		Summary: "Node registered: " + n.NodeID,
		Detail:  fmt.Sprintf("%s rack=%s port=%d", n.IP, n.Rack, n.VolumePort),
		At:      time.Now(),
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"node_id": n.NodeID,
		"volume": map[string]any{
			"master":       s.volumeCfg.Master,
			"data_center":  s.volumeCfg.DataCenter,
			"rack":         n.Rack,
			"max_volumes":  s.volumeCfg.MaxVolumes,
			"disk_type":    s.volumeCfg.DiskType,
			"weed_url":     "",
			"volume_port":  n.VolumePort,
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
		http.NotFound(w, r)
		return
	}
	switch parts[4] {
	case "heartbeat":
		s.handleHeartbeat(w, r, nodeID)
	case "config":
		s.handleNodeConfig(w, r, nodeID)
	default:
		http.NotFound(w, r)
	}
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
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	_, ok := s.reg.UpdateHeartbeat(nodeID, store.HeartbeatUpdate{
		Status:            req.Status,
		DiskFree:          req.DiskFree,
		DiskUsed:          req.DiskUsed,
		VolumeServerAlive: req.VolumeServerAlive,
		VolumesCount:      req.VolumesCount,
		EcShardsCount:     req.EcShardsCount,
	})
	if !ok {
		http.Error(w, "unknown node", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
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

// GET /api/v1/cluster/status.
func (s *server) handleClusterStatus(w http.ResponseWriter, r *http.Request) {
	reports := s.lastReports.Load().([]*redundancy.Report)
	var okC, degC, riskC int
	totalCap, usedCap := int64(0), int64(0)
	for _, n := range s.reg.All() {
		totalCap += n.CapacityBytes
		usedCap += n.DiskUsed
	}
	for _, rep := range reports {
		switch rep.State {
		case redundancy.StateOK:
			okC++
		case redundancy.StateDegraded:
			degC++
		case redundancy.StateAtRisk:
			riskC++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"nodes":       len(s.reg.All()),
		"ec_volumes":  map[string]int{"ok": okC, "degraded": degC, "at_risk": riskC, "total": len(reports)},
		"capacity":    map[string]int64{"total_bytes": totalCap, "used_bytes": usedCap},
		"updated_at":  s.lastUpdate.Load().(time.Time),
	})
}

// GET /api/v1/volumes/risk — classified volumes with nodes_to_start.
func (s *server) handleVolumesRisk(w http.ResponseWriter, r *http.Request) {
	reports := s.lastReports.Load().([]*redundancy.Report)
	writeJSON(w, http.StatusOK, map[string]any{"volumes": reports})
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
	reports := s.lastReports.Load().([]*redundancy.Report)
	risk, deg, okc := 0, 0, 0
	for _, rep := range reports {
		switch rep.State {
		case redundancy.StateOK:
			okc++
		case redundancy.StateDegraded:
			deg++
		case redundancy.StateAtRisk:
			risk++
		}
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprintf(w, "# HELP s3lite_nodes_registered Registered nodes in the control plane.\n")
	fmt.Fprintf(w, "# TYPE s3lite_nodes_registered gauge\n")
	fmt.Fprintf(w, "s3lite_nodes_registered %d\n", len(s.reg.All()))
	fmt.Fprintf(w, "# TYPE s3lite_ec_volumes_by_state gauge\n")
	fmt.Fprintf(w, "s3lite_ec_volumes_by_state{state=\"ok\"} %d\n", okc)
	fmt.Fprintf(w, "s3lite_ec_volumes_by_state{state=\"degraded\"} %d\n", deg)
	fmt.Fprintf(w, "s3lite_ec_volumes_by_state{state=\"at_risk\"} %d\n", risk)
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

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
