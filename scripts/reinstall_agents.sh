#!/usr/bin/env bash
#
# scripts/reinstall_agents.sh
#
# Triggers the agent SELF-UPDATE on cluster nodes via the control-plane API
# "handle":
#
#     POST /api/v1/nodes/{node_id}/agent/update
#
# The control plane delivers the "update_agent" command to the agent with its
# next heartbeat response; the agent then updates ITSELF:
#   - backs up its configuration (agent.env / agent.json),
#   - removes the s3lite-agent package, refreshes repository metadata and
#     installs the package fresh from the repository,
#   - restores the configuration and restarts s3lite-agent.
# All privileged work is performed by the root s3lite-agent-update.path
# helper unit on the node itself — the script needs NO SSH and NO sudo on
# the nodes. The self-update flow lives in src/agent/cmd/agent/selfupdate.go
# (the embedded update script is src/agent/cmd/agent/reinstall_agent.sh).
#
# Nodes are identified by IP address (the NODES list below). A token is
# matched against the ip field of the nodes known to the control plane
# (node_id also works). Default: all nodes registered on the control plane.
#
# The script polls the control plane for each node's update_status
# ("requested" -> "dispatched" -> "running" -> "done"/"failed") and prints
# a per-node report plus a summary. A full report is saved to a file.
#
# Requirements: curl, and jq or python3 for JSON parsing.
#
# Usage:
#   ./scripts/reinstall_agents.sh                                # all nodes
#   NODES_OVERRIDE="192.168.1.17 192.168.1.21" ./scripts/reinstall_agents.sh
#   CONTROL_PLANE=http://192.168.1.9:8099 ./scripts/reinstall_agents.sh
#
# Environment overrides:
#   CONTROL_PLANE    control plane base URL (default http://127.0.0.1:8099)
#   CURL_OPTS        extra curl options (e.g. "-k" for self-signed TLS)
#   POLL_INTERVAL    status poll interval, seconds (default 5)
#   UPDATE_TIMEOUT   max wait per node, seconds (default 900; the agent
#                    bounds the self-update itself at 15 minutes)
#   STALE_SEC        a node whose last heartbeat is older than this many
#                    seconds is skipped (default 120)

set -u

CONTROL_PLANE="${CONTROL_PLANE:-http://127.0.0.1:8099}"
API_BASE="${CONTROL_PLANE%/}/api/v1"
CURL_OPTS="${CURL_OPTS:-}"
POLL_INTERVAL="${POLL_INTERVAL:-5}"
UPDATE_TIMEOUT="${UPDATE_TIMEOUT:-900}"
STALE_SEC="${STALE_SEC:-120}"

# ---------------------------------------------------------------------------
# Node list (IP addresses). Override externally:
#   NODES_OVERRIDE="192.168.1.17 192.168.1.21" ./scripts/reinstall_agents.sh
# Empty NODES + empty NODES_OVERRIDE = all nodes known to the control plane.
# ---------------------------------------------------------------------------
NODES=(
    "192.168.1.17"   # Home:Fedora
    "192.168.1.12"   # Home:Fedora2
    "192.168.1.18"   # Home:Fedora3
    "192.168.1.19"   # Note:Fedora
    "192.168.1.7"    # Note:Fedora3
    "192.168.1.20"   # Note:Fedora2
    "192.168.1.21"   # Note:Suse
    "192.168.1.22"   # Note:Suse2
    "192.168.1.8"    # Home:Suse2
    "192.168.1.13"   # Home:Suse
)

NODES_OVERRIDE="${NODES_OVERRIDE:-}"

REPORT_DIR="${REPORT_DIR:-$PWD}"
TIMESTAMP="$(date +%Y%m%d_%H%M%S)"
REPORT_FILE="${REPORT_DIR}/s3lite_agent_reinstall_${TIMESTAMP}.txt"

# ---------------------------------------------------------------------------
# Counters
# ---------------------------------------------------------------------------

ok_updated=0       # update finished with status "done"
failed=0           # update finished with status "failed"
timed_out=0        # no terminal status within UPDATE_TIMEOUT
skipped=0          # node stale/offline / not found / API errors

# Append one line to the report file.
report_add() {
    echo "$1" >> "$REPORT_FILE"
}

# ---------------------------------------------------------------------------
# JSON helpers (jq preferred, python3 fallback)
# ---------------------------------------------------------------------------

HAVE_JQ=0
HAVE_PY=0
command -v jq >/dev/null 2>&1 && HAVE_JQ=1
command -v python3 >/dev/null 2>&1 && HAVE_PY=1

if [ "$HAVE_JQ" = 0 ] && [ "$HAVE_PY" = 0 ]; then
    echo "ERROR: need jq or python3 to parse the control-plane API responses" >&2
    exit 2
fi

# api <METHOD> <path>; prints body, exit code 0 on HTTP 2xx.
api() {
    curl -fsS $CURL_OPTS -X "$1" "$API_BASE$2" 2>/dev/null
}

# TSV columns (shared by all parsers below):
#   1 node_id | 2 ip | 3 agent_version | 4 update_status |
#   5 update_detail | 6 last_heartbeat

# nodes_tsv: GET /api/v1/nodes -> one TSV line per node.
nodes_tsv() {
    local body
    body=$(api GET /nodes) || return 1
    if [ "$HAVE_JQ" = 1 ]; then
        jq -r '.nodes[] | [.node_id, .ip, (.agent_version // ""),
                           (.update_status // ""), (.update_detail // ""),
                           (.last_heartbeat // "")] | @tsv' <<<"$body"
    else
        NODES_JSON="$body" python3 -c '
import json, os
for n in json.loads(os.environ["NODES_JSON"]).get("nodes", []):
    print("\t".join([
        n.get("node_id", ""),
        n.get("ip", ""),
        n.get("agent_version") or "",
        n.get("update_status") or "",
        n.get("update_detail") or "",
        n.get("last_heartbeat") or "",
    ]))
'
    fi
}

# node_tsv <node_id>: GET /api/v1/nodes/{id} -> one TSV line.
node_tsv() {
    local body
    body=$(api GET "/nodes/$1") || return 1
    if [ "$HAVE_JQ" = 1 ]; then
        jq -r '[.node_id, .ip, (.agent_version // ""),
               (.update_status // ""), (.update_detail // ""),
               (.last_heartbeat // "")] | @tsv' <<<"$body"
    else
        NODE_JSON="$body" python3 -c '
import json, os
n = json.loads(os.environ["NODE_JSON"])
print("\t".join([
    n.get("node_id", ""),
    n.get("ip", ""),
    n.get("agent_version") or "",
    n.get("update_status") or "",
    n.get("update_detail") or "",
    n.get("last_heartbeat") or "",
]))
'
    fi
}

# ---------------------------------------------------------------------------
# Resolve the node list: NODES / NODES_OVERRIDE entries (IP addresses) are
# matched against the ip field of the nodes known to the control plane;
# node_id also works. Empty both = all nodes.
# Returns TSV lines via stdout, "NOT_FOUND:<token>" lines for misses.
# ---------------------------------------------------------------------------

if [ -n "${NODES_OVERRIDE:-}" ]; then
    read -r -a NODES <<< "$NODES_OVERRIDE"
fi

select_nodes() {
    local all matched token nid ip ver st det hb
    all=$(nodes_tsv) || {
        echo "API_ERROR" >&2
        return 1
    }

    if [ "${#NODES[@]}" -eq 0 ]; then
        printf '%s\n' "$all"
        return 0
    fi

    for token in "${NODES[@]}"; do
        matched=""
        while IFS=$'\t' read -r nid ip ver st det hb; do
            [ -z "$nid" ] && continue
            if [ "$token" = "$ip" ] || [ "$token" = "$nid" ]; then
                printf '%s\t%s\t%s\t%s\t%s\t%s\n' "$nid" "$ip" "$ver" "$st" "$det" "$hb"
                matched=1
            fi
        done <<<"$all"
        [ -z "$matched" ] && printf 'NOT_FOUND:%s\n' "$token"
    done
}

# heartbeat_age <iso8601>: seconds since last_heartbeat; -1 when unknown.
heartbeat_age() {
    local epoch now
    epoch=$(date -d "${1%%.*}" +%s 2>/dev/null) || { echo -1; return 0; }
    [ -z "$epoch" ] && { echo -1; return 0; }
    now=$(date +%s)
    echo $((now - epoch))
}

# ---------------------------------------------------------------------------
# Process one node (TSV columns via positional params)
# ---------------------------------------------------------------------------

process_node() {
    local nid="$1" ip="$2" ver_before="$3" st_before="$4" hb="$5"

    echo ""
    echo "=== [$ip] ==="
    echo "  node_id: $nid"
    echo "  agent before: ${ver_before:--}"

    # Skip nodes whose agent stopped heartbeating: the update command is
    # delivered with the next heartbeat and would just sit unserved.
    local hb_age=-1
    [ -n "$hb" ] && hb_age=$(heartbeat_age "$hb")
    if [ "$hb_age" -lt 0 ] || [ "$hb_age" -gt "$STALE_SEC" ]; then
        echo "  SKIP: agent heartbeat is stale (${hb:-none}, age ${hb_age}s > ${STALE_SEC}s);"
        echo "        the update command would not be delivered"
        report_add "${ip}|SKIPPED|stale heartbeat (age ${hb_age}s)"
        skipped=$((skipped + 1))
        return
    fi

    # --- trigger the self-update via the control-plane handle ---
    if ! api POST "/nodes/$nid/agent/update" >/dev/null; then
        echo "  ERROR: control plane rejected the update request (is the node registered? is CONTROL_PLANE correct?)"
        report_add "${ip}|FAIL|update request rejected"
        failed=$((failed + 1))
        return
    fi
    echo "  update requested: POST /api/v1/nodes/$nid/agent/update"

    # --- poll update_status: requested -> dispatched -> running -> done/failed ---
    local waited=0 st detail row ver_after
    while :; do
        sleep "$POLL_INTERVAL"
        waited=$((waited + POLL_INTERVAL))

        row=$(node_tsv "$nid") || row=""
        st=$(cut -f4 <<<"$row")
        detail=$(cut -f5 <<<"$row")
        # an empty status right after the restart means the fresh agent is
        # reporting again and the control plane has just marked it "done";
        # the next poll picks it up, so persistent emptiness is handled
        # by the timeout below.

        case "$st" in
            done)
                ver_after=$(cut -f3 <<<"$row")
                echo "  update: OK (self-update finished)"
                echo "  agent version: ${ver_before:--} -> ${ver_after:--}"
                report_add "${ip}|OK|self-update done, agent ${ver_after:--} (was ${ver_before:--})"
                ok_updated=$((ok_updated + 1))
                return
                ;;
            failed)
                echo "  ERROR: self-update failed on the node${detail:+: $detail}"
                report_add "${ip}|FAIL|self-update failed${detail:+: $detail}"
                failed=$((failed + 1))
                return
                ;;
        esac

        if [ "$waited" -ge "$UPDATE_TIMEOUT" ]; then
            echo "  ERROR: timed out after ${UPDATE_TIMEOUT}s waiting for the update (last status: '${st:-<none>}')${detail:+: $detail}"
            report_add "${ip}|TIMEOUT|last status '${st:-none}'${detail:+: $detail}"
            timed_out=$((timed_out + 1))
            return
        fi
    done
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

if ! command -v curl >/dev/null 2>&1; then
    echo "ERROR: curl is required" >&2
    exit 2
fi

echo "Triggering s3lite-agent self-update via the control plane: $CONTROL_PLANE"
echo "Report: $REPORT_FILE"
echo "Started: $(date '+%Y-%m-%d %H:%M:%S')"

{
    echo "S3Lite agent self-update report"
    echo "Date: $(date '+%Y-%m-%d %H:%M:%S')"
    echo "Control plane: $CONTROL_PLANE"
    echo "Nodes filter: ${NODES_OVERRIDE:-${NODES[*]:-<all>}}"
    echo ""
    echo "node|result|details"
} > "$REPORT_FILE"

# Probe the control plane once so a wrong URL fails fast and clearly.
if ! api GET /nodes >/dev/null; then
    echo ""
    echo "ERROR: cannot reach the control plane at $CONTROL_PLANE"
    echo "       (start the server or set CONTROL_PLANE=<url>, e.g."
    echo "        CONTROL_PLANE=http://192.168.1.9:8099 ./scripts/reinstall_agents.sh)"
    report_add "control-plane|FAIL|unreachable at $CONTROL_PLANE"
    exit 2
fi

selection=$(select_nodes) || {
    echo "ERROR: failed to fetch the node list from the control plane" >&2
    exit 2
}

if [ -z "$selection" ]; then
    echo "No nodes are registered on the control plane."
    exit 0
fi

while IFS=$'\t' read -r nid ip ver st det hb; do
    case "$nid" in
        NOT_FOUND:*)
            token="${nid#NOT_FOUND:}"
            echo ""
            echo "=== [$token] ==="
            echo "  ERROR: no node matching '$token' on the control plane"
            echo "         (known nodes:)"
            nodes_tsv | while IFS=$'\t' read -r n2 i2 _v2 _s2 _d2 _h2; do
                echo "           $i2 (node_id=$n2)"
            done
            report_add "${token}|NOT_FOUND|no matching node"
            skipped=$((skipped + 1))
            continue
            ;;
    esac
    [ -z "$nid" ] && continue
    process_node "$nid" "$ip" "$ver" "$st" "$hb"
done <<<"$selection"

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------

echo ""
echo "================ SUMMARY ================"
echo "Updated successfully:         $ok_updated"
echo "Failed:                       $failed"
echo "Timed out:                    $timed_out"
echo "Skipped (stale/not found):    $skipped"
echo "========================================="

{
    echo ""
    echo "================ SUMMARY ================"
    echo "Updated successfully:         $ok_updated"
    echo "Failed:                       $failed"
    echo "Timed out:                    $timed_out"
    echo "Skipped (stale/not found):    $skipped"
    echo "========================================="
} >> "$REPORT_FILE"

echo ""
echo "Full report saved to: $REPORT_FILE"

if [ "$failed" -gt 0 ] || [ "$timed_out" -gt 0 ]; then exit 1; else exit 0; fi
