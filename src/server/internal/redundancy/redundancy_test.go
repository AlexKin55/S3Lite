package redundancy

import (
	"testing"
)

// buildVol returns a small 6+4 volume view with the given per-node shards.
func buildVol(nodes map[string][]int) *Volume {
	return &Volume{
		Collection:   "c",
		VolumeID:     1,
		DataShards:   6,
		ParityShards: 4,
		ShardsByNode: nodes,
	}
}

// onlineOnly returns an "is online" predicate that accepts exactly the listed
// node addresses (or the sentinel "*" to accept anything).
func onlineOnly(addrs ...string) func(string) bool {
	if len(addrs) == 1 && addrs[0] == "*" {
		return func(string) bool { return true }
	}
	set := make(map[string]bool, len(addrs))
	for _, a := range addrs {
		set[a] = true
	}
	return func(a string) bool { return set[a] }
}

func TestMergeKeepsOfflineNodes(t *testing.T) {
	hist := []*Volume{buildVol(map[string][]int{
		"n1": {0, 1, 2},
		"n2": {3, 4},
	})}
	fresh := []*Volume{buildVol(map[string][]int{
		"n2": {3, 4}, // n1 vanished from the topology
	})}

	merged := Merge(hist, fresh)
	if len(merged) != 1 {
		t.Fatalf("expected 1 merged volume, got %d", len(merged))
	}
	v := merged[0]
	if _, ok := v.ShardsByNode["n1"]; !ok {
		t.Fatal("offline node n1 was dropped by Merge; history must keep it")
	}

	// The classifier must now report n1 as offline and recommend it.
	reports := Evaluate(merged, onlineOnly("n2"))
	if len(reports) != 1 {
		t.Fatalf("expected 1 report, got %d", len(reports))
	}
	r := reports[0]
	if r.State != StateAtRisk {
		t.Fatalf("expected AT_RISK with 3/10 shards, got %s (available %d)", r.State, r.Available)
	}
	if len(r.OfflineNodes) != 1 || r.OfflineNodes[0] != "n1" {
		t.Fatalf("expected offline [n1], got %v", r.OfflineNodes)
	}
	if len(r.NodesToStart) != 1 || r.NodesToStart[0] != "n1" {
		t.Fatalf("expected nodes_to_start [n1], got %v", r.NodesToStart)
	}
}

func TestMergeFreshOverridesHistory(t *testing.T) {
	hist := []*Volume{buildVol(map[string][]int{"n1": {0, 1}, "n2": {2}})}
	fresh := []*Volume{buildVol(map[string][]int{"n1": {0, 5}})} // n1's set changed

	v := Merge(hist, fresh)[0]
	shards, ok := v.ShardsByNode["n1"]
	if !ok {
		t.Fatal("n1 must remain in the merged volume")
	}
	if len(shards) != 2 || !contains(shards, 5) || contains(shards, 1) {
		t.Fatalf("fresh n1 shard set must override history, got %v", shards)
	}
	// n2 survives from history.
	if _, ok := v.ShardsByNode["n2"]; !ok {
		t.Fatal("n2 must survive from history")
	}
}

func TestEvaluateFullVolumeIsOK(t *testing.T) {
	// One volume, all 10 shards placed on two online nodes.
	v := buildVol(map[string][]int{"n1": {0, 1, 2, 3, 4}, "n2": {5, 6, 7, 8, 9}})
	r := Evaluate([]*Volume{v}, onlineOnly("*"))[0]
	if r.State != StateOK {
		t.Fatalf("expected OK with 10/10 shards, got %s (%d/10)", r.State, r.Available)
	}
	if len(r.OfflineNodes) != 0 || len(r.NodesToStart) != 0 {
		t.Fatalf("expected no advice for a healthy volume, got offline=%v start=%v", r.OfflineNodes, r.NodesToStart)
	}
}

func contains(s []int, x int) bool {
	for _, v := range s {
		if v == x {
			return true
		}
	}
	return false
}
