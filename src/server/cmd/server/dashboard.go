package main

import (
	"net/http"

	"s3lite.dev/server/internal/redundancy"
)

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
// nodes (with computed online/offline state), node counters, cluster capacity
// and the volume reports from the live monitors.
func (s *server) dashboardAPI(w http.ResponseWriter, r *http.Request) {
	reports := s.lastReports.Load().([]*redundancy.Report)
	nodes := s.nodeViews()

	var okC, degC, riskC, onlineC, offlineC int
	var totalCap, usedCap int64
	for _, n := range nodes {
		totalCap += n.CapacityBytes
		usedCap += n.DiskUsed
		if n.Status == "online" {
			onlineC++
		} else {
			offlineC++
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
	payload := map[string]any{
		"nodes": nodes,
		"node_status": map[string]int{
			"online":  onlineC,
			"offline": offlineC,
		},
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
  .hint { font-size:12px; text-transform:none; letter-spacing:0; color:#64748b; margin-left:8px; }
  table { border-collapse: collapse; width: 100%; margin-top: 8px; }
  th, td { text-align:left; padding:6px 10px; border-bottom:1px solid #1e293b; font-size: 13px;}
  th { color:#94a3b8; }
  .ok { color:#4ade80; font-weight:600; }
  .degraded { color:#facc15; font-weight:600; }
  .at-risk { color:#f87171; font-weight:600; }
  .offline { color:#f87171; }
  .card { background:#1e293b; border-radius:10px; padding:14px 18px; margin:10px 0; }
  .grid { display:flex; gap:14px; flex-wrap:wrap; }
  .stat { background:#1e293b; border-radius:10px; padding:12px 18px; }
  .stat b { font-size: 26px; display:block; }
  .stat.ok b { color:#4ade80; }
  .stat.risk b { color:#f87171; }
  .badge { display:inline-block; padding:2px 10px; border-radius:999px; font-size:12px; font-weight:600; }
  .badge.ok { background:#052e16; color:#4ade80; }
  .badge.offline { background:#450a0a; color:#f87171; }
  button { background:#0ea5e9; color:#fff; border:0; border-radius:6px; padding:8px 14px; cursor:pointer; font-size:13px;}
  button.del { background:#7f1d1d; padding:4px 10px; }
  button.del:hover { background:#ef4444; }
  .result { margin-top:8px; font-size:13px; color:#94a3b8; min-height:16px; }
  a { color:#0ea5e9; }
</style>
</head>
<body>
<h1>🛰 S3Lite — control plane</h1>
<div class="grid" id="stats"></div>

<h2>Registered nodes <span class="hint">status computed from heartbeat freshness (TTL)</span></h2>
<div class="card">
 <table id="nodes">
   <thead><tr>
     <th>Node</th><th>IP:port</th><th>Status</th><th>Last heartbeat</th><th>Disk free</th><th>Volumes</th><th>EC shards</th><th></th>
   </tr></thead>
   <tbody></tbody>
 </table>
 <div id="del-result" class="result"></div>
</div>

<h2>EC volumes</h2>
<div class="card">
 <table id="volumes">
   <thead><tr>
     <th>Collection</th><th>Volume</th><th>State</th><th>Shards</th><th>Offline nodes</th><th>Power on to restore</th>
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
function esc(s){ return String(s==null?'':s).replace(/&/g,'&').replace(/</g,'<').replace(/>/g,'>').replace(/"/g,'"'); }
function fmtBytes(b){ if(!b) return '0 B'; const u=['B','KB','MB','GB','TB']; const i=Math.min(u.length-1,Math.floor(Math.log(b)/Math.log(1024)));
  return (b/Math.pow(1024,i)).toFixed(i?1:0)+' '+u[i]; }
function ago(t){ if(!t) return 'never'; const d=new Date(t);
  if(isNaN(d.getTime()) || d.getFullYear() < 2000) return 'never';
  const s=Math.floor((Date.now()-d.getTime())/1000);
  if(s<0) return 'now'; if(s<60) return s+'s ago'; if(s<3600) return Math.floor(s/60)+'m ago';
  return Math.floor(s/3600)+'h ago'; }
async function load(){
  const j = await (await fetch('/api/v1/dashboard')).json();
  const ec = j.ec, cap = j.capacity, ns = j.node_status, nodes = j.nodes;
  document.getElementById('stats').innerHTML =
    '<div class="stat"><b>'+nodes.length+'</b>nodes</div>'+
    '<div class="stat ok"><b>'+ns.online+'</b>online</div>'+
    '<div class="stat risk"><b>'+ns.offline+'</b>offline</div>'+
    '<div class="stat"><b>'+ec.ok+'</b>EC ok</div>'+
    '<div class="stat"><b>'+ec.at_risk+'</b>EC at risk</div>'+
    '<div class="stat"><b>'+fmtBytes(cap.used_bytes)+'</b>used / '+fmtBytes(cap.total_bytes)+'</div>';
  const nt=nodes.map(n=>{
    const cl = n.status === 'online' ? 'ok' : 'offline';
    const badge = '<span class="badge '+cl+'">'+n.status+'</span>';
    return '<tr><td>'+esc(n.node_id)+'</td><td>'+esc(n.ip)+':'+n.volume_port+'</td>'+
      '<td>'+badge+'</td>'+
      '<td>'+ago(n.last_heartbeat)+'</td>'+
      '<td>'+fmtBytes(n.disk_free)+'</td>'+
      '<td>'+n.volumes_count+'</td><td>'+n.ec_shards_count+'</td>'+
      '<td><button class="del" data-id="'+esc(n.node_id)+'" title="Delete node permanently">✕</button></td></tr>';
  }).join('');
  document.querySelector('#nodes tbody').innerHTML = nt || '<tr><td colspan=8>— no nodes registered —</td></tr>';
  const vt=j.volumes.map(v=>'<tr><td>'+esc(v.collection)+'</td><td>'+v.volume_id+'</td>'+
    '<td class="'+v.state.toLowerCase()+'">'+v.state+'</td>'+
    '<td>'+v.available_shards+'/'+v.total_shards+'</td>'+
    '<td>'+esc((v.offline_nodes||[]).join(', '))+'</td>'+
    '<td>'+esc((v.nodes_to_start||[]).join(', '))+'</td></tr>').join('');
  document.querySelector('#volumes tbody').innerHTML = vt || '<tr><td colspan=6>— no EC volumes —</td></tr>';
}
document.querySelector('#nodes tbody').addEventListener('click', async (e)=>{
  const btn = e.target.closest('button.del');
  if(!btn) return;
  const id = btn.dataset.id;
  if(!confirm('Delete node "'+id+'" from the control plane?\nIt will disappear from the dashboard entirely, also when offline.')) return;
  const r = await fetch('/api/v1/nodes/'+encodeURIComponent(id), {method:'DELETE'});
  document.getElementById('del-result').textContent = r.ok ? 'deleted "'+id+'" ✓' : 'delete failed';
  load();
});
async function testNotify(){
  const r = await fetch('/api/v1/admin/notify/test', {method:'POST'});
  document.getElementById('notify-result').textContent = r.ok ? 'sent ✓' : 'failed';
}
setInterval(load, 5000); load();
</script>
</body>
</html>`
