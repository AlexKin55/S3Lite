#!/usr/bin/env python3
"""Trigger the agent SELF-UPDATE on cluster nodes via the control-plane API.

Instead of running package-manager commands over SSH (which silently no-op
for unprivileged ssh users), the script hits the control-plane "handle":

    POST /api/v1/nodes/{node_id}/agent/update

The control plane delivers the "update_agent" command to the agent with its
next heartbeat response; the agent then updates ITSELF:
  - backs up its configuration (agent.env / agent.json),
  - removes the s3lite-agent package, refreshes repository metadata and
    installs the package fresh from the repository,
  - restores the configuration and restarts s3lite-agent.

All privileged work is performed by the root s3lite-agent-update.path helper
unit on the node itself — no SSH, no sudo. The self-update flow lives in
src/agent/cmd/agent/selfupdate.go (the embedded update script is
src/agent/cmd/agent/reinstall_agent.sh).

Nodes are identified by IP address (NODES below); a token may also be a
node_id. Default: all nodes registered on the control plane.

The script polls each node's update_status
("requested" -> "dispatched" -> "running" -> "done"/"failed") and prints a
per-node report plus a summary; a full report is saved to a file.

Requires: python3 (stdlib only).

Usage:
    ./scripts/reinstall_agents.py                          # all nodes
    NODES_OVERRIDE="192.168.1.17 192.168.1.21" ./scripts/reinstall_agents.py
    CONTROL_PLANE=http://192.168.1.9:8099 ./scripts/reinstall_agents.py

Environment overrides:
    CONTROL_PLANE    control plane base URL (default http://127.0.0.1:8099)
    POLL_INTERVAL    status poll interval, seconds (default 5)
    UPDATE_TIMEOUT   max wait per node, seconds (default 900; the agent
                     bounds the self-update itself at 15 minutes)
    STALE_SEC        a node whose last heartbeat is older than this many
                     seconds is skipped (default 120)
    DISPATCH_TIMEOUT max seconds to wait between the command being
                     "dispatched" and the agent reacting ("running"/result);
                     exceeding it usually means the installed agent is too
                     old to know the update_agent command (default 180)
"""

import json
import os
import sys
import time
import urllib.error
import urllib.request
from datetime import datetime, timezone

# ---------------------------------------------------------------------------
# Settings
# ---------------------------------------------------------------------------

CONTROL_PLANE = os.environ.get("CONTROL_PLANE", "http://127.0.0.1:8099").rstrip("/")
API_BASE = CONTROL_PLANE + "/api/v1"
POLL_INTERVAL = int(os.environ.get("POLL_INTERVAL", "5"))
UPDATE_TIMEOUT = int(os.environ.get("UPDATE_TIMEOUT", "900"))
STALE_SEC = int(os.environ.get("STALE_SEC", "120"))
DISPATCH_TIMEOUT = int(os.environ.get("DISPATCH_TIMEOUT", "180"))

# Node list (IP addresses). Override externally:
#   NODES_OVERRIDE="192.168.1.17 192.168.1.21" ./scripts/reinstall_agents.py
# Empty NODES and empty NODES_OVERRIDE = all nodes known to the control plane.
NODES = [
    "192.168.1.17",  # Home:Fedora
    "192.168.1.12",  # Home:Fedora2
    "192.168.1.18",  # Home:Fedora3
    "192.168.1.19",  # Note:Fedora
    "192.168.1.7",   # Note:Fedora3
    "192.168.1.20",  # Note:Fedora2
    "192.168.1.21",  # Note:Suse
    "192.168.1.22",  # Note:Suse2
    "192.168.1.8",   # Home:Suse2
    "192.168.1.13",  # Home:Suse
]

NODES_OVERRIDE = os.environ.get("NODES_OVERRIDE", "").split()
if NODES_OVERRIDE:
    NODES = NODES_OVERRIDE

REPORT_DIR = os.environ.get("REPORT_DIR", os.getcwd())
TIMESTAMP = datetime.now().strftime("%Y%m%d_%H%M%S")
REPORT_FILE = os.path.join(REPORT_DIR, f"s3lite_agent_reinstall_{TIMESTAMP}.txt")

report_lines: list[str] = []


def report_add(line: str) -> None:
    report_lines.append(line)


def out(msg: str = "") -> None:
    """print() with flush: without it output looks frozen when piped."""
    print(msg, flush=True)


# ---------------------------------------------------------------------------
# Control-plane API helpers (stdlib only)
# ---------------------------------------------------------------------------


def api(method: str, path: str) -> tuple[int, dict | list | None]:
    """Perform an API request; returns (http_status, decoded JSON or None)."""
    req = urllib.request.Request(
        API_BASE + path,
        method=method,
        headers={"Accept": "application/json"},
    )
    try:
        with urllib.request.urlopen(req, timeout=15) as resp:
            data = resp.read().decode("utf-8", errors="replace")
            status = resp.status
    except urllib.error.HTTPError as e:
        return e.code, None
    except (urllib.error.URLError, TimeoutError, OSError):
        return 0, None
    try:
        return status, json.loads(data)
    except json.JSONDecodeError:
        return status, None


def nodes_tsv() -> list[dict] | None:
    """GET /api/v1/nodes -> list of node dicts (or None on API failure)."""
    status, body = api("GET", "/nodes")
    if status != 200 or not isinstance(body, dict):
        return None
    return body.get("nodes", [])


def node_get(node_id: str) -> dict | None:
    status, body = api("GET", f"/nodes/{node_id}")
    return body if status == 200 and isinstance(body, dict) else None


def heartbeat_age(node: dict) -> float:
    """Seconds since the node's last heartbeat; -1 when unknown."""
    raw = node.get("last_heartbeat") or ""
    if not raw:
        return -1.0
    try:
        ts = datetime.fromisoformat(raw)
    except ValueError:
        return -1.0
    if ts.tzinfo is None:
        ts = ts.replace(tzinfo=timezone.utc)
    return (datetime.now(ts.tzinfo) - ts).total_seconds()


# ---------------------------------------------------------------------------
# Per-node flow
# ---------------------------------------------------------------------------


def process_node(node: dict) -> bool:
    """Trigger and await the self-update. Returns True on success."""
    nid = node["node_id"]
    ip = node.get("ip", "?")
    ver_before = node.get("agent_version") or "-"
    st_before = node.get("update_status") or "-"
    hb = node.get("last_heartbeat") or "none"

    out()
    out(f"=== [{ip}] ===")
    out(f"  node_id: {nid}")
    out(f"  agent before: {ver_before}")

    # Skip nodes whose agent stopped heartbeating: the update command is
    # delivered with the next heartbeat and would just sit unserved.
    hb_age = heartbeat_age(node)
    if hb_age < 0 or hb_age > STALE_SEC:
        out(
            f"  SKIP: agent heartbeat is stale ({hb}, age {hb_age:.0f}s > {STALE_SEC}s);\n"
            "        the update command would not be delivered"
        )
        report_add(f"{ip}|SKIPPED|stale heartbeat (age {hb_age:.0f}s)")
        return False

    # --- trigger the self-update via the control-plane handle ---
    status, _ = api("POST", f"/nodes/{nid}/agent/update")
    if status != 200:
        out(
            "  ERROR: control plane rejected the update request "
            f"(HTTP {status or 'unreachable'}; is the node registered? is CONTROL_PLANE correct?)"
        )
        report_add(f"{ip}|FAIL|update request rejected (HTTP {status or 'unreachable'})")
        return False
    out(f"  update requested: POST /api/v1/nodes/{nid}/agent/update")

    # --- poll update_status: requested -> dispatched -> running -> done/failed ---
    waited = 0
    while True:
        time.sleep(POLL_INTERVAL)
        waited += POLL_INTERVAL

        cur = node_get(nid) or {}
        st = cur.get("update_status") or ""
        detail = cur.get("update_detail") or ""
        # An empty status right after the restart means the fresh agent is
        # reporting again and the control plane has just marked it "done";
        # the next poll picks it up, so persistent emptiness is handled by
        # the timeout below.

        if st == "done":
            ver_after = cur.get("agent_version") or "-"
            out("  update: OK (self-update finished)")
            out(f"  agent version: {ver_before} -> {ver_after}")
            report_add(f"{ip}|OK|self-update done, agent {ver_after} (was {ver_before})")
            return True
        if st == "failed":
            out(f"  ERROR: self-update failed on the node{': ' + detail if detail else ''}")
            report_add(f"{ip}|FAIL|self-update failed{': ' + detail if detail else ''}")
            return False

        # The command was delivered but the agent never reacted: installed
        # agent builds without self-update support simply ignore the
        # update_agent command, so the status stays "dispatched" forever.
        # Fail fast instead of silently waiting out the full timeout.
        if st in ("dispatched", "requested", "") and waited >= DISPATCH_TIMEOUT and st != "running":
            out(
                f"  ERROR: the agent did not react to the update command within {waited}s "
                f"(status stuck at '{st or '<none>'}')."
            )
            out(
                "         Most likely the installed agent build predates the self-update "
                "feature and ignores the update_agent command;"
            )
            out(
                "         bootstrap such a node once via SSH (sudo dnf/zypper update s3lite-agent), "
                "after that the self-update handle works."
            )
            report_add(
                f"{ip}|FAIL|agent ignored the update command (no reaction in {waited}s, status '{st or 'none'}'); "
                "agent build predates self-update support"
            )
            return False

        out(f"  waiting: {waited}s elapsed, status='{st or '-'}'")
        if waited >= UPDATE_TIMEOUT:
            out(
                f"  ERROR: timed out after {UPDATE_TIMEOUT}s waiting for the update "
                f"(last status: '{st or '<none>'}'){': ' + detail if detail else ''}"
            )
            report_add(f"{ip}|TIMEOUT|last status '{st or 'none'}'{': ' + detail if detail else ''}")
            return False


# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------


def main() -> int:
    print(f"Triggering s3lite-agent self-update via the control plane: {CONTROL_PLANE}")
    print(f"Report: {REPORT_FILE}")
    print(f"Started: {datetime.now().strftime('%Y-%m-%d %H:%M:%S')}")

    report_add("S3Lite agent self-update report")
    report_add(f"Date: {datetime.now().strftime('%Y-%m-%d %H:%M:%S')}")
    report_add(f"Control plane: {CONTROL_PLANE}")
    report_add(f"Nodes filter: {' '.join(NODES) if NODES else '<all>'}")
    report_add("")
    report_add("node|result|details")

    # Probe the control plane once so a wrong URL fails fast and clearly.
    all_nodes = nodes_tsv()
    if all_nodes is None:
        print()
        print(f"ERROR: cannot reach the control plane at {CONTROL_PLANE}")
        print("       (start the server or set CONTROL_PLANE=<url>, e.g.")
        print("        CONTROL_PLANE=http://192.168.1.9:8099 ./scripts/reinstall_agents.py)")
        report_add(f"control-plane|FAIL|unreachable at {CONTROL_PLANE}")
        return 2

    # --- resolve the node list (match by ip, node_id also works) ---
    selected: list[dict] = []
    if not NODES:
        selected = list(all_nodes)
    else:
        for token in NODES:
            matches = [
                n
                for n in all_nodes
                if n.get("ip") == token or n.get("node_id") == token
            ]
            if matches:
                selected.extend(matches)
            else:
                print()
                print(f"=== [{token}] ===")
                print(f"  ERROR: no node matching '{token}' on the control plane")
                print("         (known nodes:)")
                for n in all_nodes:
                    print(f"           {n.get('ip', '?')} (node_id={n.get('node_id', '?')})")
                report_add(f"{token}|NOT_FOUND|no matching node")

    if not selected:
        print("No nodes are registered on the control plane.")
        return 0

    # --- run the flow sequentially ---
    ok_updated = failed = 0
    for node in selected:
        if process_node(node):
            ok_updated += 1
        else:
            failed += 1

    # --- summary ---
    print()
    print("================ SUMMARY ================")
    print(f"Updated successfully:         {ok_updated}")
    print(f"Failed/Timed out/Skipped:     {failed}")
    print("=========================================")

    report_add("")
    report_add("================ SUMMARY ================")
    report_add(f"Updated successfully:         {ok_updated}")
    report_add(f"Failed/Timed out/Skipped:     {failed}")
    report_add("=========================================")

    with open(REPORT_FILE, "w", encoding="utf-8") as f:
        f.write("\n".join(report_lines) + "\n")

    print()
    print(f"Full report saved to: {REPORT_FILE}")

    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
