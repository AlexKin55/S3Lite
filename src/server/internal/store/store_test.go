package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPreferredFlagsAndPersistence(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")

	reg := NewRegistry(8080, time.Minute)
	reg.SetStatePath(statePath)

	if _, isNew := reg.Register("nodeA", 100<<20, "", "192.168.1.10", "linux", "1", "amd64"); !isNew {
		t.Fatal("nodeA must register as new")
	}
	if _, isNew := reg.Register("nodeB", 100<<20, "", "192.168.1.11", "linux", "1", "amd64"); !isNew {
		t.Fatal("nodeB must register as new")
	}

	if !reg.SetPreferred("nodeA", true) {
		t.Fatal("SetPreferred(nodeA, true) failed")
	}
	if reg.SetPreferred("unknown", true) {
		t.Fatal("SetPreferred on unknown node must fail")
	}

	addrs := reg.PreferredNodeAddrs()
	if len(addrs) != 1 || addrs[0] != "192.168.1.10:8080" {
		t.Fatalf("PreferredNodeAddrs() = %v, want [192.168.1.10:8080]", addrs)
	}

	// State persists across a fresh registry (in-memory nodes are lost on a
	// restart): the flags stay pending and are re-applied when the nodes
	// re-register with the control plane.
	reg2 := NewRegistry(8080, time.Minute)
	reg2.SetStatePath(statePath)
	reg2.Register("nodeA", 100<<20, "", "192.168.1.10", "linux", "1", "amd64")
	reg2.Register("nodeB", 100<<20, "", "192.168.1.11", "linux", "1", "amd64")
	if n, ok := reg2.Get("nodeA"); !ok || !n.Preferred {
		t.Fatalf("nodeA preferred flag not restored: %+v", n)
	}
	if n, ok := reg2.Get("nodeB"); !ok || n.Preferred {
		t.Fatalf("nodeB must not be preferred")
	}

	// A flag toggled on an unknown node is rejected (the dashboard only
	// offers registered nodes).
	if reg2.SetPreferred("nodeC", true) {
		t.Fatal("SetPreferred on unknown node must fail")
	}
	if _, isNew := reg2.Register("nodeC", 1, "", "192.168.1.12", "linux", "1", "amd64"); !isNew {
		t.Fatal("nodeC must register as new")
	}
	if n, ok := reg2.Get("nodeC"); !ok || n.Preferred {
		t.Fatalf("nodeC must not inherit any preferred flag: %+v", n)
	}

	// Un-flagging removes the node from the list and the state file.
	if !reg.SetPreferred("nodeA", false) {
		t.Fatal("SetPreferred(nodeA, false) failed")
	}
	if got := reg.PreferredNodeAddrs(); len(got) != 0 {
		t.Fatalf("PreferredNodeAddrs() after clear = %v, want empty", got)
	}
}

func TestPolicyPreferredCannotBeUnmarked(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")

	reg := NewRegistry(8080, time.Minute)
	reg.SetStatePath(statePath)

	if _, isNew := reg.Register("policy-node", 100<<20, "", "192.168.1.20", "linux", "1", "amd64"); !isNew {
		t.Fatal("policy-node must register as new")
	}

	// Load policy from a temp file: policy-node is declared preferred.
	policyPath := filepath.Join(dir, "policy.conf")
	if err := os.WriteFile(policyPath, []byte("# comment\npolicy-node\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reg.LoadPreferredPolicy(policyPath)

	n, ok := reg.Get("policy-node")
	if !ok || !n.Preferred {
		t.Fatalf("policy-node must be preferred after LoadPreferredPolicy: %+v", n)
	}
	if !reg.IsPolicyPreferred("policy-node") {
		t.Fatal("IsPolicyPreferred(policy-node) must be true")
	}

	// Dashboard toggle off is rejected for policy nodes: returns true (no-op)
	// but the node stays preferred.
	if !reg.SetPreferred("policy-node", false) {
		t.Fatal("SetPreferred(policy-node, false) must not error")
	}
	if n, ok := reg.Get("policy-node"); !ok || !n.Preferred {
		t.Fatalf("policy-node must remain preferred after attempted unmark: %+v", n)
	}
	if !reg.IsPolicyPreferred("policy-node") {
		t.Fatal("IsPolicyPreferred(policy-node) must still be true")
	}

	// Policy flag survives a registry restart: reloading state re-applies it.
	reg2 := NewRegistry(8080, time.Minute)
	reg2.SetStatePath(statePath)
	reg2.LoadPreferredPolicy(policyPath)
	reg2.Register("policy-node", 100<<20, "", "192.168.1.20", "linux", "1", "amd64")
	if n, ok := reg2.Get("policy-node"); !ok || !n.Preferred {
		t.Fatalf("policy-node preferred flag not restored after restart: %+v", n)
	}
}
