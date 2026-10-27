package store

import (
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
	FriendlyName  string    `json:"friendly_name,omitempty"`
	Stability     string    `json:"stability,omitempty"` // "stable" | "unstable"
	RegisteredAt  time.Time `json:"registered_at"`
	LastHeartbeat time.Time `json:"last_heartbeat"`
	Alive         bool      `json:"alive"`
	Status        string    `json:"status"`
	DiskFree      int64     `json:"disk_free"`
	DiskUsed      int64     `json:"disk_used"`
	VolumesCount  int       `json:"volumes_count"`
	EcShardsCount int       `json:"ec_shards_count"`
}

// HeartbeatUpdate carries the mutable per-node stats from heartbeats.
type HeartbeatUpdate struct {
	Status            string
	DiskFree          int64
	DiskUsed          int64
	VolumeServerAlive bool
	VolumesCount      int
	EcShardsCount     int
}

// Registry is a thread-safe in-memory node registry.
// A PostgreSQL-backed implementation will replace this in production.
type Registry struct {
	mu           sync.RWMutex
	nodes        map[string]*Node
	nextRack     int
	portBase     int
	heartbeatTTL time.Duration
}

func NewRegistry(portBase int, heartbeatTTL time.Duration) *Registry {
	return &Registry{
		nodes:        make(map[string]*Node),
		nextRack:     1,
		portBase:     portBase,
		heartbeatTTL: heartbeatTTL,
	}
}

// Register adds a node or updates its network identity on re-registration.
// A fresh node receives the next rack number and volume port.
func (r *Registry) Register(id string, capacity int64, zone, ip, osName, arch string) (*Node, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	n, exists := r.nodes[id]
	if !exists {
		n = &Node{
			NodeID:        id,
			CapacityBytes: capacity,
			Zone:          zone,
			Rack:          "rack" + itoa(r.nextRack),
			VolumePort:    r.portBase + r.nextRack - 1,
		}
		r.nextRack++
		r.nodes[id] = n
	}
	n.IP = ip
	n.OS = osName
	n.Arch = arch
	// The agent just proved it is alive by registering, so treat the
	// registration moment as its most recent proof-of-life. Without this a
	// freshly registered node would show a zero last_heartbeat until its
	// first heartbeat arrives (the agent sends one every few seconds).
	now := time.Now()
	n.RegisteredAt = now
	n.LastHeartbeat = now
	n.Alive = true
	n.Status = "online"
	return n, !exists
}

// UpdateHeartbeat applies a heartbeat payload and re-evaluates liveness.
func (r *Registry) UpdateHeartbeat(id string, hb HeartbeatUpdate) (*Node, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n, ok := r.nodes[id]
	if !ok {
		return nil, false
	}
	now := time.Now()
	n.LastHeartbeat = now
	n.Alive = hb.VolumeServerAlive
	n.Status = hb.Status
	n.DiskFree = hb.DiskFree
	n.DiskUsed = hb.DiskUsed
	n.VolumesCount = hb.VolumesCount
	n.EcShardsCount = hb.EcShardsCount
	return n, true
}

// Get returns a node by id.
func (r *Registry) Get(id string) (*Node, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	n, ok := r.nodes[id]
	return n, ok
}

// All returns a snapshot of all registered nodes.
func (r *Registry) All() []*Node {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Node, 0, len(r.nodes))
	for _, n := range r.nodes {
		out = append(out, n)
	}
	return out
}

// Delete permanently removes a registered node from the registry.
// The node disappears from the dashboard entirely (also when offline).
// Returns false if no node with the given id is registered.
func (r *Registry) Delete(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.nodes[id]; !ok {
		return false
	}
	delete(r.nodes, id)
	return true
}

// OnlineIDs returns the node IDs whose agent heartbeat is fresh and whose
// volume server was reported alive.
func (r *Registry) OnlineIDs(now time.Time) map[string]bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]bool, len(r.nodes))
	for id, n := range r.nodes {
		if now.Sub(n.LastHeartbeat) <= r.heartbeatTTL && n.Alive {
			out[id] = true
		}
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := ""
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}
	return digits
}
