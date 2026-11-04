package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"path"
	"strings"
	"time"

	"s3lite.dev/server/internal/masterapi"
	"s3lite.dev/server/internal/redundancy"
)

// dashboardHTML is the self-contained browser dashboard, embedded at build
// time from dashboard.html (see the go:embed directive below).
//
//go:embed dashboard.html
var dashboardHTML string

// handleDashboard serves a self-contained browser dashboard for operators.
// It polls /api/v1/dashboard and renders the cluster health, EC volume states
// and the registered nodes with their online/offline state. The nodes table
// also allows deleting a node; a deleted node disappears entirely (also when
// offline). No external assets are needed.
func (s *server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(dashboardHTML))
}

// dashboardAPI returns aggregated JSON used by the dashboard's fetch() calls:
// nodes (with computed online/offline state), node counters, cluster capacity,
// a summary cluster health badge and the volume reports from the live monitors.
func (s *server) dashboardAPI(w http.ResponseWriter, r *http.Request) {
	reports := s.lastReports.Load().([]*redundancy.Report)
	nodes := s.nodeViews()
	// Node IDs recently seen from more than one machine (e.g. cloned VM
	// images ship the same /var/lib/s3lite/agent/agent.json to every clone).
	dupNodeIDs := s.reg.DuplicateNodeIDs()

	var okC, degC, riskC, onlineC, degradedC, offlineC int
	var totalCap, usedCap, freeCap int64
	for _, n := range nodes {
		totalCap += n.CapacityBytes
		usedCap += n.DiskUsed
		freeCap += n.DiskFree
		if n.Status == "online" {
			onlineC++
		} else {
			offlineC++
		}
		if n.Health == "degraded" {
			degradedC++
		}
	}
	for _, rep := range reports {
		switch rep.State {
		case redundancy.StateOK:
			okC++
		case redundancy.StateDegraded:
			degC++
		case redundancy.StateAtRisk:
			riskC++
		}
	}
	// Summary cluster health, derived from actual EC data availability rather
	// than raw node counts: with an k+m scheme (e.g. 6+4) a few offline nodes
	// only reduce redundancy, so they are "degraded". Critical means some
	// volume has burned through all m parity shards (available <= k, i.e.
	// zero redundancy left) or already lost data (AT_RISK), or node IDs are
	// duplicated (shard placement is no longer trustworthy).
	exhaustedC := 0
	for _, rep := range reports {
		if rep.Available <= rep.DataShards && rep.Available < rep.Total {
			exhaustedC++
		}
	}
	health := "healthy"
	switch {
	case exhaustedC > 0 || riskC > 0 || len(dupNodeIDs) > 0:
		health = "critical"
	case offlineC > 0 || degradedC > 0 || degC > 0:
		health = "degraded"
	}
	payload := map[string]any{
		"nodes":      nodes,
		"health":     health,
		"duplicates": dupNodeIDs,
		"node_status": map[string]int{
			"online":   onlineC,
			"degraded": degradedC,
			"offline":  offlineC,
		},
		"volumes": reports,
		"ec":      map[string]int{"ok": okC, "degraded": degC, "at_risk": riskC, "total": len(reports)},
		"capacity": map[string]int64{
			"total_bytes": totalCap,
			"used_bytes":  usedCap,
			"free_bytes":  freeCap,
		},
	}
	writeJSON(w, http.StatusOK, payload)
}

// filerBuckets returns the set of bucket names currently present on the filer
// (/buckets/ directory entries). A SeaweedFS S3 bucket maps 1:1 to a
// collection with the same name, so this list defines which collections are
// still reachable through the S3 API.
func (s *server) filerBuckets(ctx context.Context) (map[string]bool, error) {
	target := "http://" + s.filerAddr + "/buckets/"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	// Without this header the filer serves its HTML UI instead of JSON.
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", target, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %d", target, resp.StatusCode)
	}
	var listing struct {
		Entries []struct {
			Name     string `json:"Name"`
			FullPath string `json:"FullPath"`
		} `json:"Entries"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&listing); err != nil {
		return nil, fmt.Errorf("decode bucket list from %s: %w", target, err)
	}
	buckets := make(map[string]bool, len(listing.Entries))
	for _, e := range listing.Entries {
		name := e.Name
		if name == "" && e.FullPath != "" {
			name = path.Base(e.FullPath)
		}
		if name == "" || strings.HasPrefix(name, ".") {
			continue // ".system" and friends are filer-internal directories
		}
		buckets[name] = true
	}
	return buckets, nil
}

// orphanCollections returns collections that hold volume data on the cluster
// but have no bucket on the filer — typically the result of a filer metadata
// reset (e.g. the filer store directory was recreated by an update). Such
// data cannot be reached through the S3 API anymore.
//
// Fail-safe: when the filer bucket list cannot be fetched, an error is
// returned instead of a guess — a live bucket whose list we simply failed to
// read must never look like an orphan.
func (s *server) orphanCollections(ctx context.Context) ([]masterapi.CollectionUsage, error) {
	usage, err := s.master.CollectionUsage(ctx)
	if err != nil {
		return nil, err
	}
	buckets, err := s.filerBuckets(ctx)
	if err != nil {
		return nil, fmt.Errorf("cannot confirm the bucket list on the filer: %w", err)
	}
	out := make([]masterapi.CollectionUsage, 0)
	for _, u := range usage {
		if u.Name == "" || strings.HasPrefix(u.Name, ".") {
			continue // default collection and internal metadata collections
		}
		if buckets[u.Name] {
			continue // still a live S3 bucket
		}
		out = append(out, u)
	}
	return out, nil
}

// handleOrphansList reports the collections whose data is orphaned (no
// matching bucket on the filer), so the dashboard can show what a cleanup
// would delete before the operator confirms it.
func (s *server) handleOrphansList(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	orphans, err := s.orphanCollections(ctx)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	var total int64
	for _, o := range orphans {
		total += o.Size
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"orphans":     orphans,
		"total_bytes": total,
	})
}

// handleOrphansCleanup permanently deletes every orphaned collection (all its
// volumes and EC shards) via the master admin API, freeing the node storage.
func (s *server) handleOrphansCleanup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST required"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	orphans, err := s.orphanCollections(ctx)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	// Single pass: the master deletes the collection volumes it currently
	// sees in its topology. If some nodes are still re-registering, pressing
	// the button again cleans up the remainder.
	var (
		deleted = make([]string, 0)
		freed   int64
		errs    = make([]string, 0)
	)
	for _, o := range orphans {
		if delErr := s.master.DeleteCollection(ctx, o.Name); delErr != nil {
			errs = append(errs, o.Name+": "+delErr.Error())
			continue
		}
		deleted = append(deleted, o.Name)
		freed += o.Size
		log.Printf("orphans: deleted collection %q (%d volumes, %d EC shards, %d bytes)",
			o.Name, o.Volumes, o.EcShards, o.Size)
	}
	status := http.StatusOK
	if len(deleted) == 0 && len(errs) > 0 {
		status = http.StatusBadGateway
	}
	writeJSON(w, status, map[string]any{
		"deleted":     deleted,
		"freed_bytes": freed,
		"errors":      errs,
	})
}
