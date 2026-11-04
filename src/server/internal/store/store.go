package store

import (
	"encoding/json"
	"net"
	"os"
	"sort"
	"strconv"
	"sync"
	"time"
)

// duplicateWindow is how long a distinct control-plane request source IP is
// remembered when detecting several machines sharing one node_id (which
// happens when a VM image with an initialized agent is cloned: every clone
// carries the same /var/lib/s3lite/agent/agent.json). Agents heartbeat every
// few seconds, so two minutes is plenty for both machines to be seen; a node
// whose IP legitimately changed (DHCP renewal) stops being flagged when its
// old address expires from the window.
const duplicateWindow = 2 * time.Minute

// Node is the control plane's view of a registered storage node.
type Node struct {
	NodeID        string `json:"node_id"`
	CapacityBytes int64  `json:"capacity_bytes"`
	Zone          string `json:"zone"`
	IP            string `json:"ip"`
	OS            string `json:"os"`
	OSVersion     string `json:"os_version,omitempty"`
	Arch          string `json:"arch"`
	Rack          string `json:"rack"`
	VolumePort    int    `json:"volume_port"`
	FriendlyName  string `json:"friendly_name,omitempty"`
	Stability     string `json:"stability,omitempty"` // "stable" | "unstable"
	// Preferred marks a node the operator trusts most (rarely switched off):
	// the SeaweedFS master places new volume replicas on preferred nodes
	// first, falling back to regular nodes when they are out of free slots.
	// Applied to the pre-EC (replicated) phase only.
	Preferred     bool      `json:"preferred,omitempty"`
	RegisteredAt  time.Time `json:"registered_at"`
	LastHeartbeat time.Time `json:"last_heartbeat"`
	Alive         bool      `json:"alive"`
	Status        string    `json:"status"`           // computed: "online" | "offline" (heartbeat TTL)
	Health        string    `json:"health,omitempty"` // agent-reported: "online" | "degraded"
	DiskFree      int64     `json:"disk_free"`
	DiskUsed      int64     `json:"disk_used"`
	VolumesCount  int       `json:"volumes_count"`
	EcShardsCount int       `json:"ec_shards_count"`

	// Recent control-plane request source IPs (register/heartbeat). More
	// than one distinct source IP within duplicateWindow means the node_id
	// is used by several machines at once. Internal only (json:"-"), the
	// duplicate verdict is exposed via Registry.DuplicateNodeIDs.
	SourceIPs map[string]time.Time `json:"-"`

	// Diagnostics reported by the agent in heartbeats (dashboard node details).
	AgentVersion string `json:"agent_version,omitempty"`
	WeedVersion  string `json:"weed_version,omitempty"`
	// AgentOutdated is computed by the dashboard API (not stored): the node
	// reports an agent version older than the newest one seen in any
	// heartbeat cluster-wide. The dashboard highlights such nodes yellow.
	AgentOutdated   bool   `json:"agent_outdated,omitempty"`
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

	// statePath persists the operator-set Preferred flags across control
	// plane restarts (JSON, see SetStatePath). Flags remembered for node
	// ids that are not (yet) registered live in preferredPending and are
	// applied when the node registers.
	statePath        string
	preferredPending map[string]bool
}

func NewRegistry(portBase int, heartbeatTTL time.Duration) *Registry {
	return &Registry{
		nodes:        make(map[string]*Node),
		nextRack:     1,
		portBase:     portBase,
		heartbeatTTL: heartbeatTTL,
	}
}

// RecordSourceIP notes that a control-plane request (register or heartbeat)
// for the node arrived from the given client IP. Entries older than
// duplicateWindow are pruned, so a node that legitimately changed its IP
// (DHCP renewal) stops being flagged after the window passes.
func (r *Registry) RecordSourceIP(id, ip string) {
	if ip == "" {
		return
	}
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	n, ok := r.nodes[id]
	if !ok {
		return
	}
	if n.SourceIPs == nil {
		n.SourceIPs = make(map[string]time.Time)
	}
	n.SourceIPs[ip] = now
	for k, t := range n.SourceIPs {
		if now.Sub(t) > duplicateWindow {
			delete(n.SourceIPs, k)
		}
	}
}

// DuplicateNodeIDs returns node IDs that were recently seen from more than
// one distinct source IP, i.e. most likely used by several machines at once.
func (r *Registry) DuplicateNodeIDs() []string {
	now := time.Now()
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []string
	for _, n := range r.nodes {
		cnt := 0
		for _, t := range n.SourceIPs {
			if now.Sub(t) <= duplicateWindow {
				cnt++
			}
		}
		if cnt > 1 {
			out = append(out, n.NodeID)
		}
	}
	sort.Strings(out)
	return out
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
		// A preferred flag remembered before the node registered (state file
		// loaded at startup) applies to the fresh registration.
		if r.preferredPending[id] {
			n.Preferred = true
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
	r.persistStateLocked()
	return true
}

// SetPreferred flags a node as a preferred replica target (operator action,
// dashboard toggle). Returns false if the node id is unknown.
func (r *Registry) SetPreferred(id string, preferred bool) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	n, ok := r.nodes[id]
	if !ok {
		return false
	}
	n.Preferred = preferred
	if !preferred {
		delete(r.preferredPending, id)
	} else {
		if r.preferredPending == nil {
			r.preferredPending = make(map[string]bool)
		}
		r.preferredPending[id] = true
	}
	r.persistStateLocked()
	return true
}

// PreferredNodeAddrs returns the "ip:port" volume server addresses of all
// preferred nodes — the format the master's -preferredNodesFile expects.
func (r *Registry) PreferredNodeAddrs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []string
	for _, n := range r.nodes {
		if n.Preferred && n.IP != "" {
			out = append(out, net.JoinHostPort(n.IP, strconv.Itoa(n.VolumePort)))
		}
	}
	sort.Strings(out)
	return out
}

// SetStatePath enables persistence of the Preferred flags in a JSON file and
// loads the existing state. Flags remembered for not-yet-registered node ids
// are applied when the node registers.
func (r *Registry) SetStatePath(path string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.statePath = path
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			// callers log; keep defaults
		}
		return
	}
	var state struct {
		Preferred map[string]bool `json:"preferred"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return
	}
	r.preferredPending = make(map[string]bool, len(state.Preferred))
	for id, on := range state.Preferred {
		if !on {
			continue
		}
		if n, ok := r.nodes[id]; ok {
			n.Preferred = true
		} else {
			r.preferredPending[id] = true
		}
	}
}

// persistStateLocked writes the Preferred flags (registered and pending).
// Callers must hold r.mu.
func (r *Registry) persistStateLocked() {
	if r.statePath == "" {
		return
	}
	state := struct {
		Preferred map[string]bool `json:"preferred"`
	}{Preferred: make(map[string]bool)}
	for id, n := range r.nodes {
		if n.Preferred {
			state.Preferred[id] = true
		}
	}
	for id := range r.preferredPending {
		state.Preferred[id] = true
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return
	}
	if err := os.WriteFile(r.statePath, data, 0o644); err != nil {
		return
	}
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
