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
	OSVersion     string    `json:"os_version,omitempty"`
	Arch          string    `json:"arch"`
	Rack          string    `json:"rack"`
	VolumePort    int       `json:"volume_port"`
	FriendlyName  string    `json:"friendly_name,omitempty"`
	Stability     string    `json:"stability,omitempty"` // "stable" | "unstable"
	RegisteredAt  time.Time `json:"registered_at"`
	LastHeartbeat time.Time `json:"last_heartbeat"`
	Alive         bool      `json:"alive"`
	Status        string    `json:"status"`           // computed: "online" | "offline" (heartbeat TTL)
	Health        string    `json:"health,omitempty"` // agent-reported: "online" | "degraded"
	DiskFree      int64     `json:"disk_free"`
	DiskUsed      int64     `json:"disk_used"`
	VolumesCount  int       `json:"volumes_count"`
	EcShardsCount int       `json:"ec_shards_count"`

	// Diagnostics reported by the agent in heartbeats (dashboard node details).
	AgentVersion    string `json:"agent_version,omitempty"`
	WeedVersion     string `json:"weed_version,omitempty"`
	AgentUptimeSec  int64  `json:"agent_uptime_sec,omitempty"`
	VolumeUptimeSec int64  `json:"volume_uptime_sec,omitempty"`
	RestartsTotal   int    `json:"restarts_total,omitempty"`
	StatsAgeSec     int64  `json:"stats_age_sec,omitempty"`
	LogTail         string `json:"log_tail,omitempty"`

	// Agent self-update state (dashboard "Update agent" button). Status
	// flow: "requested" (admin) -> "dispatched" (command delivered with a
	// heartbeat response) -> "running" (agent is reinstalling the package)
	// -> "done"/"failed" (agent-reported; the control plane also marks
	// "done" when a fresh heartbeat arrives after the restart, because the
	// update replaces the reporting agent process).
	UpdateRequestedAt time.Time `json:"update_requested_at,omitempty"`
	UpdateStatus      string    `json:"update_status,omitempty"`
	UpdateDetail      string    `json:"update_detail,omitempty"`
}

// HeartbeatUpdate carries the mutable per-node stats from heartbeats.
type HeartbeatUpdate struct {
	Status            string // agent health: "online" | "degraded"
	DiskFree          int64
	DiskUsed          int64
	VolumeServerAlive bool
	VolumesCount      int
	EcShardsCount     int
	AgentVersion      string
	WeedVersion       string
	AgentUptimeSec    int64
	VolumeUptimeSec   int64
	RestartsTotal     int
	StatsAgeSec       int64
	LogTail           string
	UpdateStatus      string // agent self-update progress: "running"|"done"|"failed"
	UpdateDetail      string
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
func (r *Registry) Register(id string, capacity int64, zone, ip, osName, osVersion, arch string) (*Node, bool) {
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
	n.OSVersion = osVersion
	n.Arch = arch
	// The agent just proved it is alive by registering, so treat the
	// registration moment as its most recent proof-of-life. Without this a
	// freshly registered node would show a zero last_heartbeat until its
	// first heartbeat arrives (the agent sends one every few seconds).
	// RegisteredAt is set only once so the dashboard shows the true node age
	// even after re-registrations (heartbeat-error recovery, IP changes).
	now := time.Now()
	if !exists {
		n.RegisteredAt = now
	}
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
	n.Health = hb.Status
	n.DiskFree = hb.DiskFree
	n.DiskUsed = hb.DiskUsed
	n.VolumesCount = hb.VolumesCount
	n.EcShardsCount = hb.EcShardsCount
	n.AgentVersion = hb.AgentVersion
	n.WeedVersion = hb.WeedVersion
	n.AgentUptimeSec = hb.AgentUptimeSec
	n.VolumeUptimeSec = hb.VolumeUptimeSec
	n.RestartsTotal = hb.RestartsTotal
	n.StatsAgeSec = hb.StatsAgeSec
	n.LogTail = hb.LogTail
	// Agent self-update progress. A heartbeat without an update status while
	// the update is "running" means the update helper restarted the agent
	// service and the fresh agent is reporting again: the update completed.
	if hb.UpdateStatus != "" {
		n.UpdateStatus = hb.UpdateStatus
		n.UpdateDetail = hb.UpdateDetail
	} else if n.UpdateStatus == "running" {
		n.UpdateStatus = "done"
		n.UpdateDetail = "agent restarted; now reporting version " + hb.AgentVersion
	}
	return n, true
}

// RequestAgentUpdate schedules an agent self-update: the update_agent
// command is delivered to the agent with its next heartbeat response.
func (r *Registry) RequestAgentUpdate(id string) (*Node, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n, ok := r.nodes[id]
	if !ok {
		return nil, false
	}
	n.UpdateRequestedAt = time.Now()
	n.UpdateStatus = "requested"
	n.UpdateDetail = ""
	return n, true
}

// TakeAgentUpdateCommand atomically marks a pending agent-update request as
// dispatched and reports it, so the command is delivered exactly once.
func (r *Registry) TakeAgentUpdateCommand(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	n, ok := r.nodes[id]
	if !ok || n.UpdateStatus != "requested" {
		return false
	}
	n.UpdateStatus = "dispatched"
	return true
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
