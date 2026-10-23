package redundancy

import (
	"sort"

	"github.com/seaweedfs/seaweedfs/weed/pb/master_pb"
)

// Volume aggregates the EC shard placement of one encoded volume.
type Volume struct {
	Collection   string
	VolumeID     uint32
	DataShards   int // k, from the .vif-reported ratio (10 for stock 10+4)
	ParityShards int // m
	// ShardsByNode maps node address (ip:port) -> local shard indices.
	ShardsByNode map[string][]int
}

// Total returns k+m.
func (v *Volume) Total() int { return v.DataShards + v.ParityShards }

// VolumeKey identifies an EC volume in the placement history.
type VolumeKey struct {
	Collection string
	VolumeID   uint32
}

// Key returns the history key of the volume.
func (v *Volume) Key() VolumeKey {
	return VolumeKey{Collection: v.Collection, VolumeID: v.VolumeID}
}

// clone returns a deep copy of the volume's placement (safe to mutate).
func (v *Volume) clone() *Volume {
	cp := &Volume{
		Collection:   v.Collection,
		VolumeID:     v.VolumeID,
		DataShards:   v.DataShards,
		ParityShards: v.ParityShards,
		ShardsByNode: make(map[string][]int, len(v.ShardsByNode)),
	}
	for node, shards := range v.ShardsByNode {
		cp.ShardsByNode[node] = append([]int(nil), shards...)
	}
	return cp
}

// Merge overlays a fresh topology snapshot over the recorded placement history.
// Volumes that only exist in history (their nodes vanished from the master
// topology) keep their shard layout, so offline nodes and the shards they
// hosted stay known — this is what lets the set-cover recommendation name the
// exact PCs to power back on. Fresh per-node shard sets overwrite the history;
// for a node present in both, only the current set counts.
func Merge(history, fresh []*Volume) []*Volume {
	byKey := make(map[volKey]*Volume)
	for _, v := range history {
		if v == nil {
			continue
		}
		byKey[volKey{v.Collection, v.VolumeID}] = v.clone()
	}
	for _, v := range fresh {
		if v == nil {
			continue
		}
		key := volKey{v.Collection, v.VolumeID}
		old, ok := byKey[key]
		if !ok {
			byKey[key] = v.clone()
			continue
		}
		for node, shards := range v.ShardsByNode {
			old.ShardsByNode[node] = append([]int(nil), shards...)
		}
	}
	out := make([]*Volume, 0, len(byKey))
	for _, v := range byKey {
		out = append(out, v)
	}
	return out
}

// Status classification.
const (
	StateOK       = "OK"
	StateDegraded = "DEGRADED"
	StateAtRisk   = "AT_RISK"
)

// Report is the redundancy state of one EC volume.
type Report struct {
	Collection   string   `json:"collection"`
	VolumeID     uint32   `json:"volume_id"`
	State        string   `json:"state"`
	DataShards   int      `json:"data_shards"`
	ParityShards int      `json:"parity_shards"`
	Available    int      `json:"available_shards"`
	Total        int      `json:"total_shards"`
	NodesToStart []string `json:"nodes_to_start,omitempty"`
	OfflineNodes []string `json:"offline_nodes,omitempty"`
}

// Collect builds EC volume views from the master topology.
func Collect(topo *master_pb.TopologyInfo) []*Volume {
	byKey := make(map[volKey]*Volume)
	for _, dci := range topo.GetDataCenterInfos() {
		for _, rack := range dci.GetRackInfos() {
			for _, dn := range rack.GetDataNodeInfos() {
				addr := dataNodeAddr(dn)
				for _, disk := range dn.GetDiskInfos() {
					if disk == nil {
						continue
					}
					for _, eci := range disk.GetEcShardInfos() {
						key := volKey{Collection: eci.GetCollection(), VolumeID: eci.GetId()}
						v, ok := byKey[key]
						if !ok {
							d := int(eci.GetDataShards())
							p := int(eci.GetParityShards())
							if d <= 0 {
								d = 10 // stock 10+4 (pre-patch servers report 0)
							}
							if p <= 0 {
								p = 4
							}
							v = &Volume{
								Collection:   key.Collection,
								VolumeID:     key.VolumeID,
								DataShards:   d,
								ParityShards: p,
								ShardsByNode: make(map[string][]int),
							}
							byKey[key] = v
						}
						v.ShardsByNode[addr] = append(v.ShardsByNode[addr], bitsSet(eci.GetEcIndexBits())...)
					}
				}
			}
		}
	}
	out := make([]*Volume, 0, len(byKey))
	for _, v := range byKey {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Collection != out[j].Collection {
			return out[i].Collection < out[j].Collection
		}
		return out[i].VolumeID < out[j].VolumeID
	})
	return out
}

// Evaluate classifies every volume. online reports whether a node address
// (ip:port) is currently online; offline nodes' shards count as unavailable.
func Evaluate(volumes []*Volume, online func(nodeAddr string) bool) []*Report {
	reports := make([]*Report, 0, len(volumes))
	for _, v := range volumes {
		reports = append(reports, classify(v, online))
	}
	return reports
}

func classify(v *Volume, online func(addr string) bool) *Report {
	// present = every shard index seen in the placement, online or not.
	present := make(map[int]bool)
	available := make(map[int]bool)
	var offline []string
	for node, shards := range v.ShardsByNode {
		isOnline := online(node)
		if !isOnline {
			offline = append(offline, node)
		}
		for _, s := range shards {
			present[s] = true
			if isOnline {
				available[s] = true
			}
		}
	}
	sort.Strings(offline)

	rep := &Report{
		Collection:   v.Collection,
		VolumeID:     v.VolumeID,
		DataShards:   v.DataShards,
		ParityShards: v.ParityShards,
		Available:    len(available),
		Total:        v.Total(),
		OfflineNodes: offline,
	}
	switch {
	case rep.Available >= rep.Total:
		rep.State = StateOK
	case rep.Available >= rep.DataShards:
		rep.State = StateDegraded
	default:
		rep.State = StateAtRisk
		rep.NodesToStart = recommendNodes(v, available, rep, online)
	}
	_ = present
	return rep
}

// recommendNodes runs a greedy set cover over offline nodes: pick the node
// whose shards close the most uncovered data-shard positions until k are
// covered. With one shard per node this selects exactly the missing PCs.
func recommendNodes(v *Volume, available map[int]bool, rep *Report, online func(addr string) bool) []string {
	need := rep.DataShards - rep.Available
	if need <= 0 {
		return nil
	}
	contrib := make(map[string][]int)
	for node, shards := range v.ShardsByNode {
		if online(node) {
			continue
		}
		var add []int
		for _, s := range shards {
			if !available[s] {
				add = append(add, s)
			}
		}
		if len(add) > 0 {
			contrib[node] = add
		}
	}

	covered := make(map[int]bool)
	for s := range available {
		covered[s] = true
	}
	var chosen []string
	for len(covered) < v.DataShards && len(contrib) > 0 {
		best, bestCount := "", -1
		for node, shards := range contrib {
			c := 0
			for _, s := range shards {
				if !covered[s] {
					c++
				}
			}
			if c > bestCount {
				best, bestCount = node, c
			}
		}
		if bestCount <= 0 {
			break
		}
		chosen = append(chosen, best)
		for _, s := range contrib[best] {
			covered[s] = true
		}
		delete(contrib, best)
	}
	sort.Strings(chosen)
	return chosen
}

type volKey struct {
	Collection string
	VolumeID   uint32
}

// dataNodeAddr returns the node's cluster address (ip:port); Id is set by the
// volume server and equals ip:port, falling back to Address otherwise.
func dataNodeAddr(dn *master_pb.DataNodeInfo) string {
	if id := dn.GetId(); id != "" {
		return id
	}
	return dn.GetAddress()
}

func bitsSet(b uint32) []int {
	var out []int
	for i := 0; i < 32; i++ {
		if b&(1<<i) != 0 {
			out = append(out, i)
		}
	}
	return out
}
