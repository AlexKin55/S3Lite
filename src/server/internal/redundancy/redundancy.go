package redundancy

import (
	"net"
	"sort"
	"strconv"

	"github.com/seaweedfs/seaweedfs/weed/pb/master_pb"
)

// Volume represents an EC volume with its shard placement.
type Volume struct {
	Collection   string
	VolumeID     uint32
	DataShards   int // k
	ParityShards int // m
	// ShardsByNode maps node address (ip:port) to local shard indices.
	ShardsByNode map[string][]int
}

// Chain is a set of nodes that, if powered on, would restore downloadability.
type Chain struct {
	Nodes  []string // node IDs to power on
	Shards int      // number of shards this chain would add
}

// Collect builds EC volume views from the master topology (reactive, no state).
func Collect(topo *master_pb.TopologyInfo) []*Volume {
	byKey := make(map[string]*Volume)
	for _, dci := range topo.GetDataCenterInfos() {
		for _, rack := range dci.GetRackInfos() {
			for _, dn := range rack.GetDataNodeInfos() {
				addr := dataNodeAddr(dn)
				for _, disk := range dn.GetDiskInfos() {
					if disk == nil {
						continue
					}
					for _, eci := range disk.GetEcShardInfos() {
						key := eci.GetCollection() + "/" + strconv.Itoa(int(eci.GetId()))
						v, ok := byKey[key]
						if !ok {
							d := int(eci.GetDataShards())
							p := int(eci.GetParityShards())
							if d <= 0 {
								d = 10
							}
							if p <= 0 {
								p = 4
							}
							v = &Volume{
								Collection:   eci.GetCollection(),
								VolumeID:     eci.GetId(),
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

// CalcDownloadChains calculates recovery chains on-demand (reactive, no background state).
// volumes - EC volumes from current topology
// onlineAddrs - set of node addresses currently online (ip:port)
// collection, volumeID - the volume that failed to download
// Returns up to 5 alternative chains of nodes to power on.
func CalcDownloadChains(volumes []*Volume, onlineAddrs map[string]bool, collection string, volumeID uint32) []Chain {
	for _, v := range volumes {
		if v.Collection != collection || v.VolumeID != volumeID {
			continue
		}
		available := 0
		var offlineNodes []struct {
			addr  string
			shard int
		}
		for node, shards := range v.ShardsByNode {
			if onlineAddrs[node] {
				available += len(shards)
			} else {
				for _, s := range shards {
					offlineNodes = append(offlineNodes, struct {
						addr  string
						shard int
					}{addr: node, shard: s})
				}
			}
		}
		need := v.DataShards - available
		if need <= 0 {
			return nil // already downloadable
		}
		return calculateChains(offlineNodes, need, 5)
	}
	return nil // volume not found
}

// calculateChains finds up to maxChains combinations of offline nodes
// whose shards together cover at least need shards.
func calculateChains(nodes []struct {
	addr  string
	shard int
}, need int, maxChains int) []Chain {
	if need <= 0 {
		return nil
	}

	type nodeShards struct {
		addr   string
		shards map[int]bool
	}
	nsMap := make(map[string]*nodeShards)
	for _, n := range nodes {
		ns, ok := nsMap[n.addr]
		if !ok {
			ns = &nodeShards{addr: n.addr, shards: make(map[int]bool)}
			nsMap[n.addr] = ns
		}
		ns.shards[n.shard] = true
	}
	var offline []*nodeShards
	for _, ns := range nsMap {
		offline = append(offline, ns)
	}
	sort.Slice(offline, func(i, j int) bool {
		return len(offline[i].shards) > len(offline[j].shards)
	})

	var result []Chain
	used := make(map[int]bool)
	var current []string

	var backtrack func(start int, covered int)
	backtrack = func(start int, covered int) {
		if covered >= need {
			result = append(result, Chain{
				Nodes:  append([]string(nil), current...),
				Shards: covered,
			})
			return
		}
		if len(result) >= maxChains {
			return
		}
		for i := start; i < len(offline); i++ {
			if len(result) >= maxChains {
				return
			}
			ns := offline[i]
			newlyCovered := 0
			for s := range ns.shards {
				if !used[s] {
					newlyCovered++
				}
			}
			if newlyCovered == 0 {
				continue
			}
			current = append(current, ns.addr)
			for s := range ns.shards {
				used[s] = true
			}
			backtrack(i+1, covered+newlyCovered)
			for s := range ns.shards {
				delete(used, s)
			}
			current = current[:len(current)-1]
		}
	}

	backtrack(0, 0)
	return result
}

// dataNodeAddr returns the node's IP address (without port) so it can be
// matched against the control-plane node registry which uses IPs as keys.
// Port numbers differ between the master topology and the agent registry,
// so matching on IP only is reliable.
func dataNodeAddr(dn *master_pb.DataNodeInfo) string {
	addr := dn.GetAddress()
	if addr == "" {
		addr = dn.GetId()
	}
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}

// bitsSet returns indices of set bits in a 32-bit mask.
func bitsSet(b uint32) []int {
	var out []int
	for i := 0; i < 32; i++ {
		if b&(1<<i) != 0 {
			out = append(out, i)
		}
	}
	return out
}
