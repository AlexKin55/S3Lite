#!/usr/bin/env bash
#
# scripts/reinstall_agents.sh
#
# Reinstalls the s3lite-agent package on a list of cluster nodes, one by one,
# over SSH, preserving each node's configuration:
#   - /etc/s3lite/agent.env            (%config(noreplace), backed up anyway)
#   - /var/lib/s3lite/agent/agent.json (in /var/lib, rpm never touches it,
#                                       backed up anyway)
#
# Behavior per node:
#   - agent installed    -> backup config, remove the package, refresh repo
#                           metadata, install the package from the repo,
#                           restore config if needed, daemon-reload + restart;
#   - agent not installed -> install; if /etc/s3lite/agent.env is not
#                           configured (contains CHANGE_ME or is missing),
#                           the service is left stopped and the node is
#                           flagged NEEDS_CONFIG in the report;
#   - SSH connection fails -> node is SKIPPED and reported as unreachable.
#
# The remote helper script (scripts/agent_reinstall_remote.sh) is copied to
# each node via scp, executed there, and removed afterwards.
#
# The report is printed to stdout and saved to a file (see REPORT_DIR).
#
# Usage:
#   ./scripts/reinstall_agents.sh                 # all nodes from NODES array
#   NODES_OVERRIDE="Home:Fedora Note:Suse" ./scripts/reinstall_agents.sh
#   SSH_USER=root ./scripts/reinstall_agents.sh
#
# Node names ("Home:Fedora", ...) are expected to be Host aliases from
# ~/.ssh/config with key-based auth (BatchMode). Otherwise set SSH_USER
# and/or SSH_KEY below.

set -u

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REMOTE_SCRIPT_SRC="${SCRIPT_DIR}/agent_reinstall_remote.sh"

# ---------------------------------------------------------------------------
# Settings
# ---------------------------------------------------------------------------

# Node list (Host aliases from ~/.ssh/config). Order = traversal order.
# Override externally: NODES_OVERRIDE="Home:Fedora Note:Suse" ./scripts/reinstall_agents.sh
NODES=(
    "Home:Fedora"
    "Home:Fedora2"
    "Home:Fedora3"
    "Note:Fedora"
    "Note:Fedora3"
    "Note:Fedora2"
    "Note:Suse"
    "Note:Suse2"
    "Home:Suse2"
    "Home:Suse"
)

SSH_USER=""                  # empty = user from ~/.ssh/config
SSH_PORT=""                  # empty = port from ~/.ssh/config
SSH_KEY=""                   # empty = key from ~/.ssh/config
SSH_TIMEOUT=15               # ConnectTimeout, seconds
SSH_OPTS="-o BatchMode=yes -o StrictHostKeyChecking=accept-new"

PKG="s3lite-agent"
REMOTE_PATH="/tmp/agent_reinstall_remote.sh"   # where the helper is copied

REPORT_DIR="${REPORT_DIR:-$PWD}"
TIMESTAMP="$(date +%Y%m%d_%H%M%S)"
REPORT_FILE="${REPORT_DIR}/s3lite_agent_reinstall_${TIMESTAMP}.txt"

# ---------------------------------------------------------------------------
# Counters
# ---------------------------------------------------------------------------

ok_updated=0            # reinstalled, service active
ok_needs_config=0       # installed/reinstalled, but configuration is required
failed=0                # install/start errors
skipped=0               # nodes unreachable over SSH

# Append one line to the report file.
report_add() {
    echo "$1" >> "$REPORT_FILE"
}

# ---------------------------------------------------------------------------
# Process one node
# ---------------------------------------------------------------------------
process_node() {
    local node="$1"
    local target="$node"
    [ -n "$SSH_USER" ] && target="${SSH_USER}@${node}"

    local port_opts=()
    [ -n "$SSH_PORT" ] && port_opts+=(-p "$SSH_PORT")
    local key_opts=()
    [ -n "$SSH_KEY" ]  && key_opts+=(-i "$SSH_KEY")

    echo ""
    echo "=== [$node] ==="

    # --- connectivity check: on failure, skip the node ---
    if ! ssh $SSH_OPTS "${port_opts[@]}" "${key_opts[@]}" \
         -o ConnectTimeout="$SSH_TIMEOUT" "$target" true >/dev/null 2>&1; then
        echo "  SKIP: could not connect to the node over SSH"
        report_add "$node|SKIPPED|could not connect over SSH"
        skipped=$((skipped + 1))
        return
    fi

    # --- copy the helper script to the node ---
    # NOTE: scp is not used deliberately: node aliases ("Home:Fedora") contain a
    # colon, which scp treats as a host:path separator (and the SFTP subsystem
    # may be disabled on the nodes). Copying via `ssh cat` avoids both problems.
    if ! ssh $SSH_OPTS "${port_opts[@]}" "${key_opts[@]}" \
         -o ConnectTimeout="$SSH_TIMEOUT" "$target" \
         "cat > $REMOTE_PATH && chmod 755 $REMOTE_PATH" \
         < "$REMOTE_SCRIPT_SRC" >/dev/null 2>&1; then
        echo "  ERROR: failed to copy the helper script to the node"
        report_add "$node|FAIL|failed to copy helper script"
        failed=$((failed + 1))
        return
    fi

    # --- run the helper script on the node ---
    local out rc
    out=$(ssh $SSH_OPTS "${port_opts[@]}" "${key_opts[@]}" \
          "$target" "bash $REMOTE_PATH $PKG" 2>&1)
    rc=$?

    # --- remove the helper script from the node ---
    ssh $SSH_OPTS "${port_opts[@]}" "${key_opts[@]}" \
        "$target" "rm -f $REMOTE_PATH" >/dev/null 2>&1 || true

    # --- parse the result ---
    local result pm was_inst configured active old_ver new_ver detail restored
    result=$(awk -F= '$1=="RESULT"{print $2}'          <<<"$out")
    pm=$(awk -F= '$1=="PM"{print $2}'                  <<<"$out")
    was_inst=$(awk -F= '$1=="WAS_INSTALLED"{print $2}' <<<"$out")
    configured=$(awk -F= '$1=="CONFIGURED"{print $2}'  <<<"$out")
    active=$(awk -F= '$1=="ACTIVE"{print $2}'          <<<"$out")
    old_ver=$(awk -F= '$1=="OLD_VERSION"{print $2}'    <<<"$out")
    new_ver=$(awk -F= '$1=="NEW_VERSION"{print $2}'    <<<"$out")
    detail=$(awk -F= '$1=="DETAIL"{print $2}'          <<<"$out")
    restored=$(awk -F= '$1=="RESTORED"{printf "%s ", $2}' <<<"$out")

    echo "  hostname: $(awk -F= '$1=="HOSTNAME"{print $2}' <<<"$out")"
    echo "  package manager: ${pm:--}"
    echo "  agent before: $([ "${was_inst:-0}" = 1 ] && echo "installed ($old_ver)" || echo "NOT installed")"

    if [ "$rc" -ne 0 ] || [ "$result" != "OK" ]; then
        echo "  ERROR: ${detail:-remote command exited with code $rc}"
        echo "  --- node output ---"
        sed 's/^/    /' <<<"$out"
        report_add "$node|FAIL|${detail:-rc=$rc}"
        failed=$((failed + 1))
        return
    fi

    if [ "${was_inst:-0}" = 1 ]; then
        echo "  reinstall: OK ($old_ver -> $new_ver)"
    else
        echo "  install: OK (version $new_ver)"
    fi
    [ -n "$restored" ] && echo "  restored configs: $restored"

    if [ "${configured:-0}" = 1 ]; then
        if [ "$active" = "yes" ]; then
            echo "  s3lite-agent service: active (restart OK)"
            report_add "$node|OK|$([ "${was_inst:-0}" = 1 ] && echo "reinstalled $new_ver, service active" || echo "installed $new_ver, service active")"
            if [ "${was_inst:-0}" = 1 ]; then
                ok_updated=$((ok_updated + 1))
            else
                ok_needs_config=$((ok_needs_config + 1))
            fi
        else
            echo "  ERROR: s3lite-agent service is NOT active after restart"
            report_add "$node|FAIL|agent $new_ver installed but service not active"
            failed=$((failed + 1))
        fi
    else
        echo "  ATTENTION: agent installed but NOT CONFIGURED"
        echo "             -> edit /etc/s3lite/agent.env (CONTROL_PLANE, STORAGE, STORAGE_SIZE, ...)"
        echo "             -> then: systemctl restart s3lite-agent"
        report_add "$node|NEEDS_CONFIG|installed/reinstalled $new_ver, CONFIGURATION REQUIRED: edit /etc/s3lite/agent.env, service not started"
        ok_needs_config=$((ok_needs_config + 1))
    fi
}

# ---------------------------------------------------------------------------
# Main loop
# ---------------------------------------------------------------------------

# Support overriding the node list from the environment (see header).
if [ -n "${NODES_OVERRIDE:-}" ]; then
    read -r -a NODES <<< "$NODES_OVERRIDE"
fi

if [ ! -f "$REMOTE_SCRIPT_SRC" ]; then
    echo "ERROR: helper script not found: $REMOTE_SCRIPT_SRC" >&2
    exit 2
fi

echo "Reinstalling ${PKG} on ${#NODES[@]} nodes: ${NODES[*]}"
echo "Report: $REPORT_FILE"
echo "Started: $(date '+%Y-%m-%d %H:%M:%S')"

{
    echo "S3Lite agent reinstall report"
    echo "Date: $(date '+%Y-%m-%d %H:%M:%S')"
    echo "Package: $PKG"
    echo "Nodes: ${NODES[*]}"
    echo ""
    echo "node|result|details"
} > "$REPORT_FILE"

for node in "${NODES[@]}"; do
    process_node "$node"
done

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------

echo ""
echo "================ SUMMARY ================"
echo "Total nodes:                  ${#NODES[@]}"
echo "Reinstalled and running:      $ok_updated"
echo "Agent configuration required: $ok_needs_config"
echo "Errors:                       $failed"
echo "Skipped (no SSH):             $skipped"
echo "========================================="

{
    echo ""
    echo "================ SUMMARY ================"
    echo "Total nodes:                  ${#NODES[@]}"
    echo "Reinstalled and running:      $ok_updated"
    echo "Agent configuration required: $ok_needs_config"
    echo "Errors:                       $failed"
    echo "Skipped (no SSH):             $skipped"
    echo "========================================="
} >> "$REPORT_FILE"

echo ""
echo "Full report saved to: $REPORT_FILE"

if [ "$failed" -gt 0 ]; then exit 1; else exit 0; fi
