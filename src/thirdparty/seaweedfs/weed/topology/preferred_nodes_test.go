package topology

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/seaweedfs/seaweedfs/weed/storage/super_block"
)

// Layout with explicit IPs so preferred matching by "ip:port" works: server112
// has the most free slots, server111 is full, server121/server122 are smaller.
var preferredLayout = `
{
  "dc1":{
    "rack1":{
      "server111":{
        "ip":"10.0.0.111",
        "volumes":[
          {"id":1, "size":12312},
          {"id":2, "size":12312},
          {"id":3, "size":12312}
        ],
        "limit":3
      },
      "server112":{
        "ip":"10.0.0.112",
        "volumes":[
          {"id":4, "size":12312},
          {"id":5, "size":12312},
          {"id":6, "size":12312}
        ],
        "limit":10
      }
    },
    "rack2":{
      "server121":{
        "ip":"10.0.0.121",
        "volumes":[
          {"id":7, "size":12312},
          {"id":8, "size":12312},
          {"id":9, "size":12312}
        ],
        "limit":4
      },
      "server122":{
        "ip":"10.0.0.122",
        "volumes":[],
        "limit":4
      }
    }
  }
}
`

func findPreferredDataNode(t *testing.T, topo *Topology, url string) *DataNode {
	t.Helper()
	var found *DataNode
	var walk func(node Node)
	walk = func(node Node) {
		if dn, ok := node.GetValue().(*DataNode); ok {
			if dn.Url() == url {
				found = dn
			}
			return
		}
		for _, c := range node.Children() {
			walk(c)
		}
	}
	for _, dc := range topo.Children() {
		walk(dc)
	}
	if found == nil {
		t.Fatalf("data node %q not found in topology", url)
	}
	return found
}

func TestPreferredDataNodesPickedFirst(t *testing.T) {
	topo := setup(preferredLayout)
	vg := NewDefaultVolumeGrowth()
	rp, _ := super_block.NewReplicaPlacementFromString("000")
	option := &VolumeGrowOption{
		Collection:       "",
		ReplicaPlacement: rp,
	}

	const preferredURL = "10.0.0.112:0"
	preferred := findPreferredDataNode(t, topo, preferredURL)
	if preferred.IsPreferred() {
		t.Fatalf("server112 must not be preferred before the set is applied")
	}

	topo.SetPreferredDataNodes([]string{preferredURL})
	if !preferred.IsPreferred() {
		t.Fatalf("SetPreferredDataNodes did not flag server112")
	}
	// other nodes untouched
	if dn := findPreferredDataNode(t, topo, "10.0.0.121:0"); dn.IsPreferred() {
		t.Fatalf("server121 must not be preferred")
	}

	// While the preferred node has free slots, every single-replica placement
	// must land on it, regardless of the weighted random draw.
	for i := 0; i < 25; i++ {
		servers, _, err := vg.findEmptySlotsForOneVolume(topo, option, false)
		if err != nil {
			t.Fatalf("pick %d: %v", i, err)
		}
		if len(servers) != 1 {
			t.Fatalf("pick %d: expected 1 server, got %d", i, len(servers))
		}
		if servers[0].Url() != preferredURL {
			t.Fatalf("pick %d: expected preferred server %s, got %s", i, preferredURL, servers[0].Url())
		}
	}

	// Clearing the preference releases the boost.
	topo.SetPreferredDataNodes(nil)
	if preferred.IsPreferred() {
		t.Fatalf("SetPreferredDataNodes(nil) must clear the preferred flag")
	}
}

func TestPreferredDataNodesFallbackWhenFull(t *testing.T) {
	topo := setup(preferredLayout)
	vg := NewDefaultVolumeGrowth()
	rp, _ := super_block.NewReplicaPlacementFromString("000")
	option := &VolumeGrowOption{
		Collection:       "",
		ReplicaPlacement: rp,
	}

	// The full server is preferred: it must be skipped (no free slots) and the
	// placement must fall back to a regular node instead of failing.
	topo.SetPreferredDataNodes([]string{"10.0.0.111:0"})

	servers, _, err := vg.findEmptySlotsForOneVolume(topo, option, false)
	if err != nil {
		t.Fatalf("fallback pick failed: %v", err)
	}
	if len(servers) != 1 {
		t.Fatalf("expected 1 server, got %d", len(servers))
	}
	if servers[0].Url() == "10.0.0.111:0" {
		t.Fatalf("full preferred server must not receive a replica")
	}
	if servers[0].IsPreferred() {
		t.Fatalf("fallback server must not be preferred")
	}
}

func TestWatchPreferredNodesFile(t *testing.T) {
	topo := setup(preferredLayout)
	path := filepath.Join(t.TempDir(), "preferred-nodes.txt")
	if err := os.WriteFile(path, []byte("# comment\n\n10.0.0.112:0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	WatchPreferredNodesFile(topo, path, 100*1000) // not the ticker: initial load only
	preferred := findPreferredDataNode(t, topo, "10.0.0.112:0")
	if !preferred.IsPreferred() {
		t.Fatalf("file watcher did not apply the preferred flag")
	}
	if got := topo.PreferredDataNodes(); len(got) != 1 || got[0] != "10.0.0.112:0" {
		t.Fatalf("PreferredDataNodes() = %v, want [10.0.0.112:0]", got)
	}

	// Removing the file clears nothing (last set kept); rewriting to empty
	// clears the preference on the next load.
	if err := os.WriteFile(path, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	WatchPreferredNodesFile(topo, path, 100*1000)
	if preferred.IsPreferred() {
		t.Fatalf("empty file must clear the preferred flag")
	}
}
