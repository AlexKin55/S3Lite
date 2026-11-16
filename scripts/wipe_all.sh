#!/usr/bin/env bash
#
# wipe_all.sh — full cleanup of the entire s3lite cluster (including control plane).
#
# Stops services, kills all weed processes, wipes metadata and data,
# removes agent.json (node_id), reboots each node.
#
# SSH connects via keys (ssh <alias> without password).
# Sudo password: 123456 on all nodes.
#
# Nodes from ~/.ssh/config:
#   Home:Fedora, Home:Fedora2, Home:Fedora3, Note:Fedora, Note:Fedora3, Note:Fedora2 (fedy)
#   Note:Suse, Note:Suse2, Home:Suse2, Home:Suse                  (suse)
#
# Usage:
#   ./scripts/wipe_all.sh          # clean all nodes and reboot
#   ./scripts/wipe_all.sh --help
#
set -euo pipefail

SUDO_PASS="123456"

CONTROL_PLANE="Home:Fedora"

FEDORA_NODES=(
    Home:Fedora
    Home:Fedora2
    Home:Fedora3
    Note:Fedora
    Note:Fedora3
    Note:Fedora2
)

SUSE_NODES=(
    Note:Suse
    Note:Suse2
    Home:Suse2
    Home:Suse
)

log() { printf '[wipe_all] %s\n' "$*"; }

# Runs the cleanup script on the remote node via a single sudo -S call.
# The script is written to a temp file, executed, then deleted.
run_cleanup() {
    local alias="$1"
    local user="$2"
    local cleanup_script="$3"

    ssh -o StrictHostKeyChecking=no -o ConnectTimeout=10 "$alias" "cat > /tmp/clean_node.sh" <<'CLEANUP_SCRIPT'
set -e
systemctl stop s3lite-server s3lite-master s3lite-filer s3lite-admin s3lite-ecworker s3lite-agent 2>/dev/null || true
pkill -f 'weed master' 2>/dev/null || true
pkill -f 'weed filer' 2>/dev/null || true
pkill -f 'weed admin' 2>/dev/null || true
pkill -f 'weed worker' 2>/dev/null || true
pkill -f 'weed volume' 2>/dev/null || true
rm -rf /var/lib/s3lite/master /var/lib/s3lite/filer /var/lib/s3lite/data /var/lib/s3lite/admin /var/lib/s3lite/worker 2>/dev/null || true
rm -f /var/lib/s3lite/preferred-nodes.txt /var/lib/s3lite/server-state.json /var/lib/s3lite/agent/agent.json 2>/dev/null || true
mkdir -p /var/lib/s3lite/master /var/lib/s3lite/filer /var/lib/s3lite/data /var/lib/s3lite/admin /var/lib/s3lite/worker /var/lib/s3lite/agent 2>/dev/null || true
chown -R s3lite:s3lite /var/lib/s3lite 2>/dev/null || true
echo CONTROL_PLANE_CLEANED
CLEANUP_SCRIPT

    echo "${SUDO_PASS}" | ssh -o StrictHostKeyChecking=no -o ConnectTimeout=10 "$alias" "sudo -S bash /tmp/clean_node.sh; rm -f /tmp/clean_node.sh" 2>&1
}

run_agent_cleanup() {
    local alias="$1"

    ssh -o StrictHostKeyChecking=no -o ConnectTimeout=10 "$alias" "cat > /tmp/clean_node.sh" <<'CLEANUP_SCRIPT'
set -e
systemctl stop s3lite-agent 2>/dev/null || true
pkill -f 'weed volume' 2>/dev/null || true
rm -rf /var/lib/s3lite/data 2>/dev/null || true
rm -f /var/lib/s3lite/agent/agent.json 2>/dev/null || true
mkdir -p /var/lib/s3lite/data /var/lib/s3lite/agent 2>/dev/null || true
chown -R s3lite:s3lite /var/lib/s3lite 2>/dev/null || true
echo AGENT_CLEANED
CLEANUP_SCRIPT

    echo "${SUDO_PASS}" | ssh -o StrictHostKeyChecking=no -o ConnectTimeout=10 "$alias" "sudo -S bash /tmp/clean_node.sh; rm -f /tmp/clean_node.sh" 2>&1
}

if [[ "${1:-}" == "-h" || "${1:-}" == "--help" ]]; then
    sed -n '2,35p' "$0" | sed 's/^# \{0,1\}//'
    exit 0
fi

#echo "=== Starting cluster cleanup ==="
#echo "Control plane: ${CONTROL_PLANE}"
#run_cleanup "$CONTROL_PLANE" "fedy"

echo ""
echo "=== Cleaning agent nodes ==="
for n in "${FEDORA_NODES[@]}"; do
    run_agent_cleanup "$n"
done

for n in "${SUSE_NODES[@]}"; do
    run_agent_cleanup "$n"
done

echo ""
echo "=== All nodes cleaned ==="
echo "Wait 1-2 minutes, then start services:"
echo "  Control plane: systemctl start s3lite-server s3lite-master s3lite-filer s3lite-admin s3lite-ecworker"
echo "  Agents:        systemctl start s3lite-agent"
