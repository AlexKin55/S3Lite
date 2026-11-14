package storage

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/seaweedfs/seaweedfs/weed/pb"
	"github.com/seaweedfs/seaweedfs/weed/stats"
)

func TestStorePreferredNodes(t *testing.T) {
	store := NewStore(nil, "localhost", 8080, 18080, "http://localhost:8080", "store-id",
		[]string{}, nil, nil, "", 0, nil, nil, 0, stats.DefaultDiskIOProbeConfig())

	// No preferred nodes loaded: passthrough, no change in order.
	nodes := []pb.ServerAddress{"10.0.0.2:8080", "10.0.0.1:8080", "10.0.0.3:8080"}
	if got := store.SortNodesByPreference(nodes); len(got) != 3 {
		t.Fatalf("SortNodesByPreference with no prefs returned %d nodes, want 3", len(got))
	}

	// Load preferred nodes from a temp file.
	dir := t.TempDir()
	policyPath := filepath.Join(dir, "preferred-nodes.txt")
	if err := os.WriteFile(policyPath, []byte("# comment\n10.0.0.1:8080\n10.0.0.3:8080\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	store.LoadPreferredNodesFile(policyPath)

	if !store.IsPreferredNode("10.0.0.1:8080") {
		t.Fatal("IsPreferredNode(10.0.0.1:8080) must be true")
	}
	if store.IsPreferredNode("10.0.0.2:8080") {
		t.Fatal("IsPreferredNode(10.0.0.2:8080) must be false")
	}
	if !store.IsPreferredNode("10.0.0.3:8080") {
		t.Fatal("IsPreferredNode(10.0.0.3:8080) must be true")
	}

	// Sorting: preferred nodes come first, original order preserved within each group.
	sorted := store.SortNodesByPreference(nodes)
	if len(sorted) != 3 {
		t.Fatalf("SortNodesByPreference returned %d nodes, want 3", len(sorted))
	}
	if sorted[0] != "10.0.0.1:8080" || sorted[1] != "10.0.0.3:8080" || sorted[2] != "10.0.0.2:8080" {
		t.Fatalf("SortNodesByPreference = %v, want [10.0.0.1:8080 10.0.0.3:8080 10.0.0.2:8080]", sorted)
	}

	// Empty file path is a no-op: preferred nodes remain unchanged.
	store.LoadPreferredNodesFile("")
	if !store.IsPreferredNode("10.0.0.1:8080") {
		t.Fatal("IsPreferredNode must remain true after LoadPreferredNodesFile(\"\")")
	}
	sorted = store.SortNodesByPreference(nodes)
	if sorted[0] != "10.0.0.1:8080" || sorted[1] != "10.0.0.3:8080" || sorted[2] != "10.0.0.2:8080" {
		t.Fatalf("SortNodesByPreference after no-op = %v, want [10.0.0.1:8080 10.0.0.3:8080 10.0.0.2:8080]", sorted)
	}
}
