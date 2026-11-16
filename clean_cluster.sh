#!/usr/bin/env bash
# Clean all cluster nodes: stop agents, wipe volume data, remove node_id, reboot.
# SSH uses keys (no password for ssh). Sudo password: 123456 on all nodes.
# Usage: ./clean_cluster.sh

set -euo pipefail

SUDO_PASS="123456"

fedora_nodes=(Home:Fedora Home:Fedora2 Home:Fedora3 Note:Fedora Note:Fedora3 Note:Fedora2)
suse_nodes=(Note:Suse Note:Suse2 Home:Suse2 Home:Suse)

run_remote() {
    local alias="$1"
    local cmd="$2"
    ssh -o StrictHostKeyChecking=no "$alias" "echo '${SUDO_PASS}' | sudo -S ${cmd}" 2>&1
}

run_on_node() {
    local alias="$1"
    echo "=== Cleaning $alias ==="
    run_remote "$alias" "systemctl stop s3lite-agent 2>/dev/null || true"
    run_remote "$alias" "pkill -f 'weed volume' 2>/dev/null || true"
    run_remote "$alias" "rm -rf /var/lib/s3lite/data/* 2>/dev/null || true"
    run_remote "$alias" "rm -f /var/lib/s3lite/agent/agent.json 2>/dev/null || true"
    run_remote "$alias" "reboot" 2>&1 || echo "WARNING: $alias may not have responded (already rebooting?)"
}

run_control_plane() {
    local alias="$1"
    echo "=== Cleaning control plane $alias ==="
    run_remote "$alias" "systemctl stop s3lite-server s3lite-master s3lite-filer s3lite-admin s3lite-ecworker s3lite-agent 2>/dev/null || true"
    run_remote "$alias" "pkill -f 'weed master' 2>/dev/null || true"
    run_remote "$alias" "pkill -f 'weed filer' 2>/dev/null || true"
    run_remote "$alias" "pkill -f 'weed admin' 2>/dev/null || true"
    run_remote "$alias" "pkill -f 'weed worker' 2>/dev/null || true"
    run_remote "$alias" "pkill -f 'weed volume' 2>/dev/null || true"
    run_remote "$alias" "rm -rf /var/lib/s3lite/master/* 2>/dev/null || true"
    run_remote "$alias" "rm -rf /var/lib/s3lite/filer/* 2>/dev/null || true"
    run_remote "$alias" "rm -rf /var/lib/s3lite/data/* 2>/dev/null || true"
    run_remote "$alias" "rm -f /var/lib/s3lite/preferred-nodes.txt 2>/dev/null || true"
    run_remote "$alias" "rm -f /var/lib/s3lite/server-state.json 2>/dev/null || true"
    run_remote "$alias" "rm -f /var/lib/s3lite/agent/agent.json 2>/dev/null || true"
    run_remote "$alias" "reboot" 2>&1 || echo "WARNING: $alias may not have responded (already rebooting?)"
}

echo "=== Starting cluster cleanup ==="
echo "Control plane node: Home:Fedora"
run_control_plane "Home:Fedora"

echo ""
echo "=== Cleaning remaining agent nodes ==="
for n in "${fedora_nodes[@]}"; do
    if [ "$n" = "Home:Fedora" ]; then continue; fi
    run_on_node "$n"
done

for n in "${suse_nodes[@]}"; do
    run_on_node "$n"
done

echo ""
echo "=== All nodes scheduled for cleanup and reboot ==="
echo "Wait 1-2 minutes, then start services on each node."
echo "Start control plane first: systemctl start s3lite-server s3lite-master s3lite-filer s3lite-admin s3lite-ecworker"
echo "Then start agents: systemctl start s3lite-agent"
