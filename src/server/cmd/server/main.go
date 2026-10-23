package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"
)

// Node is the control plane's view of a registered storage node.
type Node struct {
	NodeID        string    `json:"node_id"`
	CapacityBytes int64     `json:"capacity_bytes"`
	Zone          string    `json:"zone"`
	IP            string    `json:"ip"`
	OS            string    `json:"os"`
	Arch          string    `json:"arch"`
	Rack          string    `json:"rack"`
	VolumePort    int       `json:"volume_port"`
	RegisteredAt  time.Time `json:"registered_at"`
	LastHeartbeat time.Time `json:"last_heartbeat"`
	Alive         bool      `json:"alive"`
	Status        string    `json:"status"`
	DiskFree      int64     `json:"disk_free"`
	DiskUsed      int64     `json:"disk_used"`
	VolumesCount  int       `json:"volumes_count"`
	EcShardsCount int       `json:"ec_shards_count"`
}

// registry is a minimal in-memory node registry (PostgreSQL comes in Stage 4).
type registry struct {
	mu    sync.RWMutex
	nodes map[string]*Node
	// rack counter for node assignment
	nextRack int
}

func newRegistry() *registry {
	return &registry{nodes: make(map[string]*Node), nextRack: 1}
}

func (r *registry) upsert(n *Node) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nodes[n.NodeID] = n
}

func (r *registry) get(id string) (*Node, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	n, ok := r.nodes[id]
	return n, ok
}

func (r *registry) all() []*Node {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Node, 0, len(r.nodes))
	for _, n := range r.nodes {
		out = append(out, n)
	}
	return out
}

// server holds the volume parameters the control plane hands to agents.
type server struct {
	reg      *registry
	master   string // master address for volume servers
	dc       string
	rackBase string
	maxVol   int
	diskType string
	portBase int
}

func main() {
	listen := flag.String("listen", ":8099", "control plane HTTP listen address")
	master := flag.String("master", "192.168.1.9:9333", "SeaweedFS master address handed to agents")
	dc := flag.String("dataCenter", "dc1", "data center name for volume servers")
	rackBase := flag.String("rackBase", "rack", "rack name prefix for auto-assigned racks")
	maxVol := flag.Int("maxVolumes", 8, "max volumes for volume servers")
	diskType := flag.String("diskType", "hdd", "disk type for volume servers")
	portBase := flag.Int("portBase", 8080, "starting port for agent volume servers")
	flag.Parse()

	srv := &server{
		reg:      newRegistry(),
		master:   *master,
		dc:       *dc,
		rackBase: *rackBase,
		maxVol:   *maxVol,
		diskType: *diskType,
		portBase: *portBase,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/nodes", srv.handleRegister)
	mux.HandleFunc("/api/v1/nodes/", srv.handleNodePath)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	log.Printf("control plane listening on %s, master=%s", *listen, *master)
	if err := http.ListenAndServe(*listen, mux); err != nil {
		log.Fatal(err)
	}
}

// POST /api/v1/nodes — node registration; returns the volume config.
// GET /api/v1/nodes — list registered nodes.
func (s *server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		s.handleNodesList(w, r)
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

	// Re-registration keeps the already assigned rack/port; a fresh node gets new ones.
	n, existed := s.reg.get(req.NodeID)
	if !existed {
		s.reg.mu.Lock()
		s.reg.nextRack++
		rackNo := s.reg.nextRack - 1
		port := s.portBase + rackNo - 1
		s.reg.mu.Unlock()
		n = &Node{
			NodeID:        req.NodeID,
			CapacityBytes: req.CapacityBytes,
			Zone:          req.Zone,
			Rack:          fmt.Sprintf("%s%d", s.rackBase, rackNo),
			VolumePort:    port,
		}
	}
	n.IP = req.IP
	n.OS = req.OS
	n.Arch = req.Arch
	n.RegisteredAt = time.Now()
	n.Alive = true
	n.Status = "online"
	s.reg.upsert(n)

	resp := map[string]any{
		"node_id": n.NodeID,
		"volume": map[string]any{
			"master":       s.master,
			"data_center":  s.dc,
			"rack":         n.Rack,
			"max_volumes":  s.maxVol,
			"disk_type":    s.diskType,
			"weed_url":     "",
			"volume_port":  n.VolumePort,
		},
	}
	writeJSON(w, http.StatusOK, resp)
}

// GET /api/v1/nodes — list registered nodes.
func (s *server) handleNodesList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"nodes": s.reg.all()})
}

func (s *server) handleNodePath(w http.ResponseWriter, r *http.Request) {
	// path: /api/v1/nodes/<id>/heartbeat or /api/v1/nodes/<id>/config
	parts := splitPath(r.URL.Path)
	if len(parts) < 3 { // ["api", "v1", "nodes"]
		if r.URL.Path == "/api/v1/nodes" && r.Method == http.MethodGet {
			s.handleNodesList(w, r)
			return
		}
		http.NotFound(w, r)
		return
	}
	nodeID := parts[3]

	if len(parts) == 4 {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/nodes":
			s.handleNodesList(w, r)
		default:
			http.NotFound(w, r)
		}
		return
	}
	action := parts[4]
	switch action {
	case "heartbeat":
		s.handleHeartbeat(w, r, nodeID)
	case "config":
		s.handleNodeConfig(w, r, nodeID)
	default:
		http.NotFound(w, r)
	}
}

// POST /api/v1/nodes/{id}/heartbeat
func (s *server) handleHeartbeat(w http.ResponseWriter, r *http.Request, nodeID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	n, ok := s.reg.get(nodeID)
	if !ok {
		http.Error(w, "unknown node", http.StatusNotFound)
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
	s.reg.mu.Lock()
	n.LastHeartbeat = time.Now()
	n.Alive = req.VolumeServerAlive
	n.Status = req.Status
	n.DiskFree = req.DiskFree
	n.DiskUsed = req.DiskUsed
	n.VolumesCount = req.VolumesCount
	n.EcShardsCount = req.EcShardsCount
	s.reg.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// GET /api/v1/nodes/{id}/config — current volume config for the node.
func (s *server) handleNodeConfig(w http.ResponseWriter, r *http.Request, nodeID string) {
	n, ok := s.reg.get(nodeID)
	if !ok {
		http.Error(w, "unknown node", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"volume": map[string]any{
			"master":      s.master,
			"data_center": s.dc,
			"rack":        n.Rack,
			"max_volumes": s.maxVol,
			"disk_type":   s.diskType,
			"weed_url":    "",
			"volume_port": n.VolumePort,
		},
	})
}

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
