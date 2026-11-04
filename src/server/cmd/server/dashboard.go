package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os/exec"
	"path"
	"regexp"
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

// observeAgentVersion records an agent version reported by a heartbeat,
// keeping the highest one seen so far (numeric dotted comparison). The
// dashboard flags nodes whose agent reports an older version.
func (s *server) observeAgentVersion(v string) {
	if v == "" {
		return
	}
	s.agentVerMu.Lock()
	defer s.agentVerMu.Unlock()
	if s.newestAgentVersionSeen == "" || versionLess(s.newestAgentVersionSeen, v) {
		s.newestAgentVersionSeen = v
	}
}

// newestAgentVersion returns the highest agent version reported by any
// heartbeat since server start ("" when none was reported yet).
func (s *server) newestAgentVersion() string {
	s.agentVerMu.Lock()
	defer s.agentVerMu.Unlock()
	return s.newestAgentVersionSeen
}

// versionLess compares dotted version strings ("0.0.4" < "0.0.10").
// Segments are compared numerically; non-numeric tails (e.g. "-rc1") are
// ignored and missing segments count as 0.
func versionLess(a, b string) bool {
	if i := strings.IndexByte(a, '-'); i >= 0 {
		a = a[:i]
	}
	if i := strings.IndexByte(b, '-'); i >= 0 {
		b = b[:i]
	}
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var ai, bi int
		if i < len(pa) {
			ai = versionSeg(pa[i])
		}
		if i < len(pb) {
			bi = versionSeg(pb[i])
		}
		if ai != bi {
			return ai < bi
		}
	}
	return false
}

// versionSeg parses the leading decimal digits of a version segment; a
// segment without digits (or an empty one) counts as 0.
func versionSeg(seg string) int {
	n := 0
	for _, c := range seg {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
	}
	return n
}

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
	outdatedC := 0
	for i := range nodes {
		if nodes[i].AgentOutdated {
			outdatedC++
		}
	}
	payload := map[string]any{
		"nodes":      nodes,
		"health":     health,
		"duplicates": dupNodeIDs,
		// Newest agent version seen in any heartbeat; nodes reporting an
		// older version carry agent_outdated=true and are highlighted.
		"agent_latest_version": s.newestAgentVersion(),
		"agents_outdated":      outdatedC,
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

// fsckSummaryRegexp parses volume.fsck's final line:
// "Total\t\tentries:<n>\torphan:<n>\t<pct>%\t<n>B".
var fsckSummaryRegexp = regexp.MustCompile(`orphan:(\d+)\t[0-9.]+%\t(\d+)B`)

// handleFsck runs "weed shell volume.fsck" on the server where the control
// plane lives and returns its report: orphan chunks (data on the volume
// servers that no filer entry references — e.g. an upload interrupted between
// the chunk write and the metadata commit). Report-only unless the body is
// {"purge":true}, which adds -forcePurging to delete the found chunks (the
// disk space is reclaimed by the next vacuum compaction).
//
// A scan reads every volume index and the whole filer store, so it can run
// for minutes: the run is single-flight (409 on a second click), capped by
// -fsckTimeout, and the response carries the captured output for the
// dashboard to show.
func (s *server) handleFsck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST required"})
		return
	}
	// Single flight: a second click while a scan is running gets 409, the
	// dashboard also disables its button.
	if s.fsckRunning.Swap(true) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "fsck is already running"})
		return
	}
	defer s.fsckRunning.Store(false)

	var req struct {
		Purge bool `json:"purge"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req)
	}
	command := "volume.fsck"
	if req.Purge {
		command = "volume.fsck -forcePurging"
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.fsckTimeout)
	defer cancel()
	start := time.Now()

	// The shell needs the admin lock before a mutating/checking command;
	// piped non-interactively the lock command runs first.
	script := fmt.Sprintf("lock\n%s\n", command)
	cmd := exec.CommandContext(ctx, s.weedBin, "shell",
		"-master", s.volumeCfg.Master,
		"-filer", s.filerAddr)
	cmd.Stdin = strings.NewReader(script)
	out, err := cmd.CombinedOutput()

	resp := map[string]any{
		"cmd":         command,
		"duration_ms": time.Since(start).Milliseconds(),
	}
	if len(out) > 16*1024 {
		out = out[len(out)-16*1024:] // keep the tail: the summary is at the end
	}
	resp["output"] = string(out)

	switch {
	case ctx.Err() == context.DeadlineExceeded:
		resp["error"] = fmt.Sprintf("fsck did not finish within %s; try again or raise -fsckTimeout", s.fsckTimeout)
	case err != nil:
		resp["error"] = fmt.Sprintf("weed shell failed: %v", err)
	}

	// Summarize for the dashboard regardless of the exit status: the scan
	// often completes and prints the Total line even when the shell then
	// reports a benign error on exit.
	if m := fsckSummaryRegexp.FindSubmatch(out); m != nil {
		resp["orphan_chunks"] = json.Number(string(m[1]))
		resp["orphan_bytes"] = json.Number(string(m[2]))
	} else if strings.Contains(string(out), "no orphan data") {
		resp["orphan_chunks"] = 0
		resp["orphan_bytes"] = 0
	}

	status := http.StatusOK
	if _, hasErr := resp["error"]; hasErr {
		status = http.StatusBadGateway
	}
	log.Printf("fsck: cmd=%q purge=%v duration=%s err=%v", command, req.Purge, time.Since(start).Round(time.Second), err)
	writeJSON(w, status, resp)
}
