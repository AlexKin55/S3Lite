package main

import (
	_ "embed"
	"net/http"

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
	// Summary cluster health: critical when a node is offline or an EC volume
	// is at risk, degraded when something needs attention, else healthy.
	health := "healthy"
	switch {
	case offlineC > 0 || riskC > 0 || len(dupNodeIDs) > 0:
		health = "critical"
	case degradedC > 0 || degC > 0:
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
