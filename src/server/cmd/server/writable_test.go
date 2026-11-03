package main

import (
	"testing"

	"github.com/seaweedfs/seaweedfs/weed/pb/master_pb"
)

// The cluster read-only policy: writes are allowed while at least
// minWriteNodes volume servers are online. The threshold should be set to the
// EC scheme size (data+parity, e.g. 10 for 6+4) so a degraded cluster stops
// accumulating single-copy data that cannot be encoded yet.

func TestClusterWritable(t *testing.T) {
	cases := []struct {
		name         string
		active       int
		minWrite     int
		wantWritable bool
	}{
		{"exactly at threshold", 10, 10, true},
		{"above threshold", 20, 10, true},
		{"below threshold", 7, 10, false},
		{"zero active", 0, 10, false},
		{"guard disabled", 0, 0, true},
		{"negative guard disabled", 3, -1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := clusterWritable(tc.active, tc.minWrite); got != tc.wantWritable {
				t.Fatalf("clusterWritable(%d, %d) = %v, want %v", tc.active, tc.minWrite, got, tc.wantWritable)
			}
		})
	}
}

func TestCountTopologyNodes(t *testing.T) {
	topo := &master_pb.TopologyInfo{
		DataCenterInfos: []*master_pb.DataCenterInfo{
			{Id: "dc1", RackInfos: []*master_pb.RackInfo{
				{Id: "rack1", DataNodeInfos: []*master_pb.DataNodeInfo{
					{Id: "n1"}, {Id: "n2"},
				}},
				{Id: "rack2", DataNodeInfos: []*master_pb.DataNodeInfo{
					{Id: "n3"},
				}},
			}},
			{Id: "dc2", RackInfos: []*master_pb.RackInfo{
				{Id: "rack3", DataNodeInfos: []*master_pb.DataNodeInfo{
					{Id: "n4"},
				}},
			}},
		},
	}
	if got := countTopologyNodes(topo); got != 4 {
		t.Fatalf("countTopologyNodes = %d, want 4", got)
	}
	if got := countTopologyNodes(nil); got != 0 {
		t.Fatalf("countTopologyNodes(nil) = %d, want 0", got)
	}
}
