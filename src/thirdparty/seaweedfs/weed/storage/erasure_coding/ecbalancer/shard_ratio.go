package ecbalancer

import (
	"github.com/seaweedfs/seaweedfs/weed/pb/master_pb"
	"github.com/seaweedfs/seaweedfs/weed/storage/erasure_coding"
)

// shardDataShards returns the data-shard count of the volume an EC shard belongs
// to, used to size the shard's disk footprint. The per-volume ratio reported on
// the heartbeat (from the .vif EC context) wins; 0 falls back to the standard
// scheme (what a pre-patch volume server, or a 10+4 volume, reports).
func shardDataShards(eci *master_pb.VolumeEcShardInformationMessage) int {
	if d := int(eci.GetDataShards()); d > 0 {
		return d
	}
	return erasure_coding.DataShardsCount
}

// VolumeShardRatio returns the RAW per-volume (dataShards, parityShards) reported
// on an EC shard's heartbeat, with 0 meaning "not reported" (falls back to the
// collection ratio — the standard scheme). The volume server fills these fields
// from the .vif EC context, so a mixed-ratio collection is spread by each
// volume's own data/parity split.
func VolumeShardRatio(eci *master_pb.VolumeEcShardInformationMessage) (dataShards, parityShards int) {
	if d := int(eci.GetDataShards()); d > 0 {
		dataShards = d
	}
	if p := int(eci.GetParityShards()); p > 0 {
		parityShards = p
	}
	return dataShards, parityShards
}
