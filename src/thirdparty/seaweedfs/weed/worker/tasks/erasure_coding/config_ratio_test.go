package erasure_coding

import (
	"context"
	"fmt"
	"testing"

	"github.com/seaweedfs/seaweedfs/weed/admin/topology"
	"github.com/seaweedfs/seaweedfs/weed/pb/master_pb"
	"github.com/seaweedfs/seaweedfs/weed/storage/erasure_coding"
	"github.com/seaweedfs/seaweedfs/weed/worker/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The fork targets an arbitrary k+m scheme (README: e.g. 6+4 over 10 nodes),
// so the worker task must expose the ratio as configuration instead of using
// the upstream compiled-in 10+4 constants everywhere.

func TestNewDefaultConfigTargets64Scheme(t *testing.T) {
	cfg := NewDefaultConfig()
	assert.Equal(t, 6, cfg.DataShards, "default data shards must follow the fork's 6+4 target scheme")
	assert.Equal(t, 4, cfg.ParityShards, "default parity shards must follow the fork's 6+4 target scheme")
	assert.Equal(t, 6, cfg.EffectiveDataShards())
	assert.Equal(t, 4, cfg.EffectiveParityShards())
}

func TestEffectiveShardCountsFallBackToCompiledDefaults(t *testing.T) {
	cfg := &Config{} // unset ratio fields
	assert.Equal(t, erasure_coding.DataShardsCount, cfg.EffectiveDataShards(),
		"<=0 data shards must fall back to the compiled-in default")
	assert.Equal(t, erasure_coding.ParityShardsCount, cfg.EffectiveParityShards(),
		"<=0 parity shards must fall back to the compiled-in default")

	// Custom ratio is passed through verbatim.
	custom := &Config{DataShards: 3, ParityShards: 2}
	assert.Equal(t, 3, custom.EffectiveDataShards())
	assert.Equal(t, 2, custom.EffectiveParityShards())
}

func TestEffectiveShardCountsOnNilConfig(t *testing.T) {
	var cfg *Config
	assert.Equal(t, erasure_coding.DataShardsCount, cfg.EffectiveDataShards())
	assert.Equal(t, erasure_coding.ParityShardsCount, cfg.EffectiveParityShards())
}

func TestConfigSpecExposesShardRatioFields(t *testing.T) {
	spec := GetConfigSpec()
	byName := make(map[string]int)
	for i, f := range spec.Fields {
		byName[f.JSONName] = i
	}
	for _, name := range []string{"data_shards", "parity_shards"} {
		idx, ok := byName[name]
		require.True(t, ok, "config spec must expose the %s field so the ratio is editable via the admin UI", name)
		assert.Equal(t, name, spec.Fields[idx].Name)
	}
	assert.Equal(t, 6, spec.Fields[byName["data_shards"]].DefaultValue)
	assert.Equal(t, 4, spec.Fields[byName["parity_shards"]].DefaultValue)
}

// TestDetectionStuckSourceHonorsConfiguredRatio guards the #9448 orphan-source
// detection against a hardcoded total: a volume whose shard set is complete
// for the CONFIGURED ratio (here 3+2 = 5, but 10 shards present) must be
// recognized as fully encoded and skipped. If detection compared against the
// compiled-in 10+4 total (14) instead, it would keep re-proposing an encode
// that collides with the mounted shards.
func TestDetectionStuckSourceHonorsConfiguredRatio(t *testing.T) {
	const volumeID uint32 = 77
	const presentShards = 10 // >= 3+2 total, < compiled 10+4 total (14)

	activeTopology := buildStuckSourceTopology(t, volumeID, presentShards)
	clusterInfo := &types.ClusterInfo{ActiveTopology: activeTopology}
	metrics := buildStuckSourceMetrics(volumeID, "127.0.0.1:8080")

	cfg := &Config{DataShards: 3, ParityShards: 2}
	results, hasMore, err := Detection(context.Background(), metrics, clusterInfo, cfg, 0)
	require.NoError(t, err)
	require.False(t, hasMore)
	require.Empty(t, results, "a volume whose shard set is complete for the configured ratio must not be re-proposed for encoding")
}

// TestDetectionMinNodeGateHonorsConfiguredParity checks that the safety gate
// (never encode when the cluster has fewer nodes than parity shards) uses the
// configured parity count, not the compiled-in constant: with parity=6 the
// gate must fire on a 5-node cluster and swallow the candidate volume.
func TestDetectionMinNodeGateHonorsConfiguredParity(t *testing.T) {
	const volumeID uint32 = 78
	const nodeCount = 5

	activeTopology := buildSmallClusterTopology(t, nodeCount, volumeID)
	clusterInfo := &types.ClusterInfo{ActiveTopology: activeTopology}
	metrics := buildStuckSourceMetrics(volumeID, fmt.Sprintf("127.0.0.1:%d", 8080))

	cfg := &Config{DataShards: 3, ParityShards: 6}
	results, hasMore, err := Detection(context.Background(), metrics, clusterInfo, cfg, 0)
	require.NoError(t, err)
	require.False(t, hasMore)
	require.Empty(t, results, "a cluster with fewer nodes than the configured parity shards must not produce EC proposals")
}

// buildSmallClusterTopology creates `nodeCount` data nodes (one hdd disk each)
// and places a single regular volume replica on the first node. The volume
// metric satisfies the EC criteria (quiet, >95% full, >=30MB).
func buildSmallClusterTopology(t *testing.T, nodeCount int, volumeID uint32) *topology.ActiveTopology {
	t.Helper()
	activeTopology := topology.NewActiveTopology(nodeCount)
	nodes := make([]*master_pb.DataNodeInfo, 0, nodeCount)
	for i := 0; i < nodeCount; i++ {
		diskInfo := &master_pb.DiskInfo{
			DiskId:         0,
			MaxVolumeCount: 100,
		}
		if i == 0 {
			diskInfo.VolumeInfos = []*master_pb.VolumeInformationMessage{{
				Id:       volumeID,
				DiskId:   0,
				DiskType: "hdd",
				Size:     200 * 1024 * 1024,
			}}
		}
		nodes = append(nodes, &master_pb.DataNodeInfo{
			Id:        fmt.Sprintf("127.0.0.1:%d", 8080+i),
			DiskInfos: map[string]*master_pb.DiskInfo{"hdd": diskInfo},
		})
	}
	require.NoError(t, activeTopology.UpdateTopology(&master_pb.TopologyInfo{
		DataCenterInfos: []*master_pb.DataCenterInfo{{
			Id: "dc1",
			RackInfos: []*master_pb.RackInfo{{
				Id:            "rack1",
				DataNodeInfos: nodes,
			}},
		}},
	}))
	return activeTopology
}
