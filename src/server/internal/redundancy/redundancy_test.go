package redundancy

import (
	"testing"

	"github.com/seaweedfs/seaweedfs/weed/pb/master_pb"
)

// buildTestVolume creates a Volume with k data shards and m parity shards,
// placing shards on the given nodes. Node keys are IP-only (no port) to
// match dataNodeAddr() which strips ports for registry matching.
func buildTestVolume(collection string, volumeID uint32, k, m int, shardsByNode map[string][]int) *Volume {
	return &Volume{
		Collection:   collection,
		VolumeID:     volumeID,
		DataShards:   k,
		ParityShards: m,
		ShardsByNode: shardsByNode,
	}
}

// buildTestTopology creates a minimal TopologyInfo with one datanode
// listing EC shard infos for the given volumes.
func buildTestTopology(vols ...*Volume) *master_pb.TopologyInfo {
	disk := &master_pb.DiskInfo{}
	for _, v := range vols {
		ecIndexBits := uint32(0)
		for _, s := range v.ShardsByNode["node-1"] {
			ecIndexBits |= 1 << s
		}
		disk.EcShardInfos = append(disk.EcShardInfos, &master_pb.VolumeEcShardInformationMessage{
			Id:           v.VolumeID,
			Collection:   v.Collection,
			DataShards:   uint32(v.DataShards),
			ParityShards: uint32(v.ParityShards),
			EcIndexBits:  ecIndexBits,
		})
	}

	dn := &master_pb.DataNodeInfo{
		Id:        "node-1",
		Address:   "node-1",
		DiskInfos: map[string]*master_pb.DiskInfo{"disk1": disk},
	}

	return &master_pb.TopologyInfo{
		DataCenterInfos: []*master_pb.DataCenterInfo{
			{Id: "dc1", RackInfos: []*master_pb.RackInfo{
				{Id: "rack1", DataNodeInfos: []*master_pb.DataNodeInfo{dn}},
			}},
		},
	}
}

func TestCalcDownloadChains_EnoughShards(t *testing.T) {
	// 6+4 scheme, all shards on one online node => available=6 >= k=6 => no chains needed
	v := buildTestVolume("bucket-a", 1, 6, 4, map[string][]int{
		"node-1": {0, 1, 2, 3, 4, 5},
	})
	online := map[string]bool{"node-1": true}

	chains := CalcDownloadChains([]*Volume{v}, online, "bucket-a", 1)
	if len(chains) != 0 {
		t.Fatalf("expected no chains when enough shards available, got %d", len(chains))
	}
}

func TestCalcDownloadChains_NotEnoughShards(t *testing.T) {
	// 6+4 scheme, only 4 shards online => available=4 < k=6 => need 2 more shards
	// node-2 has shards 6,7 => chain [node-2] gives 2 shards => enough
	v := buildTestVolume("bucket-a", 1, 6, 4, map[string][]int{
		"node-1": {0, 1, 2, 3},
		"node-2": {6, 7},
		"node-3": {4, 5},
	})
	online := map[string]bool{"node-1": true}

	chains := CalcDownloadChains([]*Volume{v}, online, "bucket-a", 1)
	if len(chains) == 0 {
		t.Fatal("expected chains when not enough shards available")
	}
	// First chain should prefer node with most shards (node-3 has 2 shards, node-2 has 2 shards)
	// Either node-2 or node-3 alone provides 2 shards which meets need=2
	found := false
	for _, ch := range chains {
		if (len(ch.Nodes) == 1 && (ch.Nodes[0] == "node-2" || ch.Nodes[0] == "node-3")) && ch.Shards >= 2 {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected single-node chain with >=2 shards, got %+v", chains)
	}
}

func TestCalcDownloadChains_VolumeNotFound(t *testing.T) {
	v := buildTestVolume("bucket-a", 1, 6, 4, map[string][]int{
		"node-1": {0, 1, 2, 3, 4, 5},
	})
	online := map[string]bool{"node-1": true}

	chains := CalcDownloadChains([]*Volume{v}, online, "bucket-a", 999)
	if len(chains) != 0 {
		t.Fatalf("expected no chains for non-existent volume, got %d", len(chains))
	}
}

func TestCollect(t *testing.T) {
	topo := buildTestTopology(
		buildTestVolume("bucket-a", 1, 6, 4, map[string][]int{"node-1": {0, 1, 2, 3, 4, 5}}),
		buildTestVolume("bucket-b", 2, 3, 2, map[string][]int{"node-1": {0, 1, 2}}),
	)

	vols := Collect(topo)
	if len(vols) != 2 {
		t.Fatalf("expected 2 volumes, got %d", len(vols))
	}

	if vols[0].Collection != "bucket-a" || vols[0].VolumeID != 1 {
		t.Fatalf("expected bucket-a vol 1, got %s/%d", vols[0].Collection, vols[0].VolumeID)
	}
	if vols[0].DataShards != 6 || vols[0].ParityShards != 4 {
		t.Fatalf("expected 6+4, got %d+%d", vols[0].DataShards, vols[0].ParityShards)
	}
	if len(vols[0].ShardsByNode["node-1"]) != 6 {
		t.Fatalf("expected 6 shards on node-1, got %d", len(vols[0].ShardsByNode["node-1"]))
	}

	if vols[1].Collection != "bucket-b" || vols[1].VolumeID != 2 {
		t.Fatalf("expected bucket-b vol 2, got %s/%d", vols[1].Collection, vols[1].VolumeID)
	}
	if vols[1].DataShards != 3 || vols[1].ParityShards != 2 {
		t.Fatalf("expected 3+2, got %d+%d", vols[1].DataShards, vols[1].ParityShards)
	}
}
