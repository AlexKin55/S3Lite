package main

import (
	"net/http"

	"s3lite.dev/server/internal/redundancy"
)

// handleDashboard serves a self-contained browser dashboard for operators.
// It polls the public API endpoints (/api/v1/cluster/status, /api/v1/volumes/risk,
// /api/v1/nodes) and renders the cluster health, EC volume states and the
// "which PCs to power on" recommendations. No external assets are needed.
func (s *server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(dashboardHTML))
}

// dashboardAPI returns aggregated JSON used by the dashboard's fetch() calls:
// a mix of cluster state and volume reports from the live monitors.
func (s *server) dashboardAPI(w http.ResponseWriter, r *http.Request) {
	reports := s.lastReports.Load().([]*redundancy.Report)
	nodes := s.reg.All()

	var okC, degC, riskC int
	var totalCap, usedCap int64
	for _, n := range nodes {
		totalCap += n.CapacityBytes
		usedCap += n.DiskUsed
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
	payload := map[string]any{
		"nodes":   nodes,
		"volumes": reports,
		"ec":      map[string]int{"ok": okC, "degraded": degC, "at_risk": riskC, "total": len(reports)},
		"capacity": map[string]int64{
			"total_bytes": totalCap,
			"used_bytes":  usedCap,
		},
	}
	writeJSON(w, http.StatusOK, payload)
}

const dashboardHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>S3Lite — control plane</title>
<style>
  body { font-family: system-ui, sans-serif; margin: 24px; background:#0f172a; color:#e2e8f0; }
  h1 { font-size: 22px; }
  h2 { font-size: 16px; margin-top: 28px; color:#94a3b8; text-transform: uppercase; letter-spacing:.08em; }
  table { border-collapse: collapse; width: 100%; margin-top: 8px; }
  th, td { text-align:left; padding:6px 10px; border-bottom:1px solid #1e293b; font-size: 13px;}
  th { color:#94a3b8; }
  .ok { color:#4ade80; font-weight:600; }
  .degraded { color:#facc15; font-weight:600; }
  .at-risk { color:#f87171; font-weight:600; }
  .card { background:#1e293b; border-radius:10px; padding:14px 18px; margin:10px 0; }
  .grid { display:flex; gap:14px; flex-wrap:wrap; }
  .stat { background:#1e293b; border-radius:10px; padding:12px 18px; }
  .stat b { font-size: 26px; display:block; }
  button { background:#0ea5e9; color:#fff; border:0; border-radius:6px; padding:8px 14px; cursor:pointer; font-size:13px;}
  a { color:#0ea5e9; }
</style>
</head>
<body>
<h1>🛰 S3Lite — control plane</h1>
<div class="grid" id="stats"></div>

<h2>EC volumes</h2>
<div class="card">
 <table id="volumes">
   <thead><tr>
     <th>Collection</th><th>Volume</th><th>State</th><th>Shards</th><th>Offline nodes</th><th>Power on to restore</th>
   </tr></thead>
   <tbody></tbody>
 </table>
</div>

<h2>Registered nodes</h2>
<div class="card">
 <table id="nodes">
   <thead><tr>
     <th>Node</th><th>IP:port</th><th>Rack</th><th>Zone</th><th>Status</th><th>Heartbeat</th><th>Disk free</th><th>Volumes</th><th>EC shards</th>
   </tr></thead>
   <tbody></tbody>
 </table>
</div>

<h2>Actions</h2>
<div class="card">
  <button onclick="testNotify()">Test notification</button>
  <span id="notify-result"></span>
</div>

<script>
function fmtBytes(b){ if(!b) return '0 B'; const u=['B','KB','MB','GB','TB']; const i=Math.min(u.length-1,Math.floor(Math.log(b)/Math.log(1024)));
  return (b/Math.pow(1024,i)).toFixed(i?1:0)+' '+u[i]; }
async function load(){
  const j = await (await fetch('/api/v1/dashboard')).json();
  const ec = j.ec, cap = j.capacity;
  document.getElementById('stats').innerHTML =
    '<div class="stat"><b>'+j.nodes.length+'</b>nodes</div>'+
    '<div class="stat"><b>'+ec.ok+'</b>ok</div>'+
    '<div class="stat"><b>'+ec.degraded+'</b>degraded</div>'+
    '<div class="stat"><b>'+ec.at_risk+'</b>at risk</div>'+
    '<div class="stat"><b>'+fmtBytes(cap.used_bytes)+'</b>used / '+fmtBytes(cap.total_bytes)+'</div>';
  const vt=j.volumes.map(v=>'<tr><td>'+v.collection+'</td><td>'+v.volume_id+'</td>'+
    '<td class="'+v.state.toLowerCase()+'">'+v.state+'</td>'+
    '<td>'+v.available_shards+'/'+v.total_shards+'</td>'+
    '<td>'+(v.offline_nodes||[]).join(', ')+'</td>'+
    '<td>'+(v.nodes_to_start||[]).join(', ')+'</td></tr>').join('');
  document.querySelector('#volumes tbody').innerHTML = vt || '<tr><td colspan=6>— no EC volumes —</td></tr>';
  const nt=j.nodes.map(n=>'<tr><td>'+n.node_id+'</td><td>'+n.ip+':'+n.volume_port+'</td>'+
    '<td>'+n.rack+'</td><td>'+n.zone+'</td>'+
    '<td class="'+(n.alive?'ok':'at-risk')+'">'+n.status+'</td>'+
    '<td>'+(n.last_heartbeat||'').slice(0,19)+'</td>'+
    '<td>'+fmtBytes(n.disk_free)+'</td><td>'+n.volumes_count+'</td><td>'+n.ec_shards_count+'</td></tr>').join('');
  document.querySelector('#nodes tbody').innerHTML = nt || '<tr><td colspan=9>— no nodes —</td></tr>';
}
async function testNotify(){
  const r = await fetch('/api/v1/admin/notify/test', {method:'POST'});
  document.getElementById('notify-result').textContent = r.ok ? 'sent ✓' : 'failed';
}
setInterval(load, 5000); load();
</script>
</body>
</html>`
