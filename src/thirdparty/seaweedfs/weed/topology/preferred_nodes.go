package topology

// Preferred replica placement: the operator (via the S3Lite control plane)
// flags volume servers that rarely go offline. New volume replicas are placed
// on preferred servers first; regular servers only pick up the remainder when
// the preferred ones have no free slots or are filtered out by the replica
// placement constraints (data center / rack spread).
//
// The preference is a strong weight boost in PickNodesByWeight, so the normal
// placement code path is unchanged for clusters without any preferred nodes,
// and fallback happens naturally when a preferred node runs out of space.

import (
	"bufio"
	"bytes"
	"os"
	"strings"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/glog"
)

// PreferredNodeWeightBoost multiplies the free-slot weight of a preferred
// node — and of any rack / data center containing one — in PickNodesByWeight.
// The raw weights are small (a handful of free volume slots per node), so the
// boosted weight dominates the weighted draw until the node runs out of free
// slots (weight 0 candidates are skipped), which is the fallback to regular
// nodes.
const PreferredNodeWeightBoost = 1_000_000

// SetPreferredDataNodes replaces the preferred volume server set. Addresses
// are volume server URLs ("ip:port", as reported in the master topology).
// Empty input clears the preference. The flags are applied to the live
// topology immediately; data nodes that register later are flagged by the
// next applyPreferredNodes pass (the file watcher re-applies periodically).
func (t *Topology) SetPreferredDataNodes(addrs []string) {
	set := make(map[string]struct{}, len(addrs))
	for _, a := range addrs {
		if a = strings.TrimSpace(a); a != "" {
			set[a] = struct{}{}
		}
	}

	t.preferredNodesLock.Lock()
	changed := len(set) != len(t.preferredNodes)
	if !changed {
		for a := range set {
			if _, ok := t.preferredNodes[a]; !ok {
				changed = true
				break
			}
		}
	}
	t.preferredNodes = set
	t.preferredNodesLock.Unlock()

	if changed {
		t.applyPreferredNodes()
	}
}

// PreferredDataNodes returns the current preferred volume server addresses.
func (t *Topology) PreferredDataNodes() []string {
	t.preferredNodesLock.RLock()
	defer t.preferredNodesLock.RUnlock()
	out := make([]string, 0, len(t.preferredNodes))
	for a := range t.preferredNodes {
		out = append(out, a)
	}
	return out
}

func (t *Topology) isPreferredDataNode(addr string) bool {
	t.preferredNodesLock.RLock()
	defer t.preferredNodesLock.RUnlock()
	_, ok := t.preferredNodes[addr]
	return ok
}

// applyPreferredNodes walks the topology and (re)sets the preferred flag on
// every data node from the current preferred set.
func (t *Topology) applyPreferredNodes() {
	var walk func(node Node)
	walk = func(node Node) {
		if dn, ok := node.GetValue().(*DataNode); ok {
			dn.SetPreferred(t.isPreferredDataNode(dn.Url()))
			return
		}
		for _, c := range node.Children() {
			walk(c)
		}
	}
	for _, dc := range t.Children() {
		walk(dc)
	}
}

// containsPreferredDataNode reports whether node is a preferred data node or
// (for racks and data centers) contains one.
func containsPreferredDataNode(node Node) bool {
	if dn, ok := node.GetValue().(*DataNode); ok {
		return dn.IsPreferred()
	}
	for _, c := range node.Children() {
		if containsPreferredDataNode(c) {
			return true
		}
	}
	return false
}

// WatchPreferredNodesFile keeps the preferred set in sync with a file holding
// one volume server address ("ip:port") per line; empty lines and #-comments
// are ignored. The file is read immediately and re-read every interval, so
// the control plane can update the preference without restarting the master.
// A missing file simply clears the preference (once loaded successfully, a
// read error keeps the last known set).
func WatchPreferredNodesFile(t *Topology, path string, interval time.Duration) {
	if interval <= 0 {
		interval = 15 * time.Second
	}
	load := func() {
		data, err := os.ReadFile(path)
		if err != nil {
			if !os.IsNotExist(err) {
				glog.Warningf("read preferred nodes file %s: %v", path, err)
			}
			return
		}
		var addrs []string
		sc := bufio.NewScanner(bytes.NewReader(data))
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			addrs = append(addrs, line)
		}
		t.SetPreferredDataNodes(addrs)
		// Re-apply even when the set is unchanged: data nodes that registered
		// since the last pass need their flag set.
		t.applyPreferredNodes()
	}
	glog.V(0).Infof("watching preferred nodes file %s (interval %s)", path, interval)
	load()
	go func() {
		for range time.Tick(interval) {
			load()
		}
	}()
}
