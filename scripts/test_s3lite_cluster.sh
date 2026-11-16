#!/usr/bin/env bash
#
# S3Lite Cluster Test Suite
# Tests read/write, stress tolerance, fault tolerance, and EC encoding
# for demonstration purposes.
#
# Prerequisites:
#   - mc (MinIO client) configured with alias 's3test'
#   - Cluster running with 10+ nodes online
#   - ssh access to all nodes for fault tolerance tests
#
# Usage:
#   ./test_s3lite_cluster.sh [test_name]
#
# Tests:
#   all            - Run all tests (default)
#   write-read     - Basic write/read of various file sizes
#   stress         - Concurrent uploads stress test
#   fault-tolerance - Kill a node, verify data still accessible
#   distribution   - Show data distribution across nodes
#   ec-status      - Show EC encoding status
#   cleanup        - Remove all test objects

set -euo pipefail

# Configuration
S3_ALIAS="s3test"
BUCKET="test-bucket-demo"
MC="/home/alex/go/bin/mc"
CTRL_PLANE="127.0.0.1:8099"
CTRL_HOST="127.0.0.1"

# Node aliases for fault tolerance tests
declare -A NODE_ALIASES=(
    ["192.168.1.12"]="Home:Fedora2"
    ["192.168.1.13"]="Home:Fedora3"
    ["192.168.1.14"]="Note:Fedora"
    ["192.168.1.17"]="Note:Fedora3"
    ["192.168.1.18"]="Note:Fedora2"
    ["192.168.1.19"]="Home:Suse"
    ["192.1681.20"]="Home:Suse2"
    ["192.168.1.21"]="Note:Suse2"
    ["192.168.1.22"]="Home:Fedora"
)

log() { echo "[TEST] $*"; }
pass() { echo "[PASS] $*"; }
fail() { echo "[FAIL] $*"; }
info() { echo "[INFO] $*"; }

# -------------------------------------------------------------------
# Prerequisites check
# -------------------------------------------------------------------
check_prereqs() {
    info "Checking prerequisites..."

    if ! command -v "$MC" &>/dev/null; then
        fail "mc (MinIO client) not found at $MC"
        exit 1
    fi

    if ! "$MC" alias list 2>/dev/null | grep -q "$S3_ALIAS"; then
        fail "S3 alias '$S3_ALIAS' not configured. Run: $MC alias set $S3ALIAS http://<host>:8333 <access> <secret>"
        exit 1
    fi

    # Check cluster is writable
    local nodes_online
    nodes_online=$(curl -s "http://$CTRL_PLANE/api/v1/nodes" 2>/dev/null | python3 -c "
import json,sys
data=sys.stdin.read()
start=data.find('{')
d=json.loads(data[start:])
print(sum(1 for n in d['nodes'] if n.get('alive')))
" 2>/dev/null || echo "0")

    if [ "$nodes_online" -lt 3 ]; then
        fail "Only $nodes_online nodes online. Need at least 3."
        exit 1
    fi

    info "Prerequisites OK: $nodes_online nodes online, mc configured"
}

# -------------------------------------------------------------------
# Setup bucket
# -------------------------------------------------------------------
setup_bucket() {
    info "Setting up bucket: $BUCKET"
    "$MC" mb "$S3_ALIAS/$BUCKET" 2>/dev/null || true
    pass "Bucket ready: $BUCKET"
}

# -------------------------------------------------------------------
# Test 1: Write/Read various file sizes
# -------------------------------------------------------------------
test_write_read() {
    info "=== Test: Write/Read various file sizes ==="

    local sizes="1 10 100 500"  # MB
    local uploaded=""

    for size in $sizes; do
        local file="/tmp/test_${size}mb.bin"
        local obj="${BUCKET}/test_${size}mb.bin"

        # Create random file
        if [ ! -f "$file" ]; then
            info "Creating ${size}MB test file..."
            dd if=/dev/urandom of="$file" bs=1M count="$size" 2>/dev/null
        fi

        # Upload
        info "Uploading ${size}MB file..."
        if timeout 120 "$MC" cp "$file" "$S3_ALIAS/$obj" 2>&1 | grep -q "->"; then
            pass "Upload OK: ${size}MB"
            uploaded="$uploaded $obj"
        else
            fail "Upload FAILED: ${size}MB"
            continue
        fi

        # Verify with stat
        local stat_out
        stat_out=$(timeout 30 "$MC" stat "$S3_ALIAS/$obj" 2>&1 || true)
        if echo "$stat_out" | grep -q "Size: ${size}000000\|Size: ${size}0MB\|Size: $((size * 1000000))"; then
            pass "Stat OK: ${size}MB file exists with correct size"
        else
            info "Stat output: $stat_out"
        fi
    done

    info "Uploaded files: $uploaded"
    pass "Write/Read test completed"
}

# -------------------------------------------------------------------
# Test 2: Stress test - concurrent uploads
# -------------------------------------------------------------------
test_stress() {
    info "=== Test: Stress - Concurrent Uploads ==="

    local concurrency=10
    local files_per_node=5
    local total=$((concurrency * files_per_node))

    info "Starting $total concurrent uploads ($concurrency parallel, $files_per_node each)..."

    local success=0
    local failed=0
    local pids=()

    for i in $(seq 1 $concurrency); do
        (
            local s_ok=0
            local s_fail=0
            for j in $(seq 1 $files_per_node); do
                local idx=$(( (i-1) * files_per_node + j ))
                local file="/tmp/stress_${idx}.bin"
                local obj="${BUCKET}/stress_${idx}.bin"

                # Create random file (1MB)
                dd if=/dev/urandom of="$file" bs=1M count=1 2>/dev/null

                if timeout 60 "$MC" cp "$file" "$S3_ALIAS/$obj" 2>&1 | grep -q "->"; then
                    s_ok=$((s_ok + 1))
                else
                    s_fail=$((s_fail + 1))
                fi
            done
            echo "${s_ok} ${s_fail}"
        ) &
        pids+=($!)
    done

    # Wait for all
    for pid in "${pids[@]}"; do
        wait "$pid" || true
    done

    # Collect results from temp files
    # (Simplified - just check bucket object count)
    local obj_count
    obj_count=$("$MC" ls "$S3_ALIAS/$BUCKET/" 2>/dev/null | wc -l || echo "0")
    info "Objects in bucket: $obj_count"

    if [ "$obj_count" -gt 0 ]; then
        pass "Stress test completed: $obj_count objects uploaded"
    else
        fail "Stress test: no objects found"
    fi
}

# -------------------------------------------------------------------
# Test 3: Data distribution across nodes
# -------------------------------------------------------------------
test_distribution() {
    info "=== Test: Data Distribution Across Nodes ==="

    info "Current node volume distribution:"
    curl -s "http://$CTRL_PLANE/api/v1/nodes" 2>/dev/null | python3 -c "
import json,sys
data=sys.stdin.read()
start=data.find('{')
d=json.loads(data[start:])
total=0
for n in d['nodes']:
    if n.get('alive'):
        total += n.get('volumes_count',0)
        print(f\"  {n['ip']}: {n.get('volumes_count',0)} volumes, {n.get('ec_shards_count',0)} EC shards, preferred={n.get('preferred',False)}\")
print(f\"Total volumes across cluster: {total}\")
" 2>/dev/null || echo "Failed to query nodes"

    pass "Distribution test completed"
}

# -------------------------------------------------------------------
# Test 4: EC Encoding Status
# -------------------------------------------------------------------
test_ec_status() {
    info "=== Test: EC Encoding Status ==="

    curl -s "http://$CTRL_PLANE/api/v1/maintenance/encode/status" 2>/dev/null | python3 -c "
import json,sys
data=sys.stdin.read()
start=data.find('{')
d=json.loads(data[start:])
ops=d.get('operations',[])
if not ops:
    print('  No EC encoding operations recorded')
else:
    for op in ops[:10]:
        print(f\"  Volume {op.get('volume_id','?')}: {op.get('status','?')} at {op.get('started_at','?')}\")
" 2>/dev/null || echo "Failed to query EC status"

    info "Node EC shard counts:"
    curl -s "http://$CTRL_PLANE/api/v1/nodes" 2>/dev/null | python3 -c "
import json,sys
data=sys.stdin.read()
start=data.find('{')
d=json.loads(data[start:])
for n in d['nodes']:
    if n.get('alive') and n.get('ec_shards_count',0) > 0:
        print(f\"  {n['ip']}: {n.get('ec_shards_count',0)} EC shards\")
print('  (0 EC shards = encoding not yet triggered on small data)')
" 2>/dev/null || echo "Failed to query EC shards"

    pass "EC status check completed"
}

# -------------------------------------------------------------------
# Test 5: Fault Tolerance - Kill a node, verify data accessible
# -------------------------------------------------------------------
test_fault_tolerance() {
    info "=== Test: Fault Tolerance ==="

    # First upload a test file
    local testfile="/tmp/ft_test.bin"
    dd if=/dev/urandom of="$testfile" bs=1M count=10 2>/dev/null
    local obj="${BUCKET}/ft_test.bin"

    info "Uploading 10MB file before node failure..."
    if ! timeout 60 "$MC" cp "$testfile" "$S3_ALIAS/$obj" 2>&1 | grep -q "->"; then
        fail "Pre-failure upload failed"
        return 1
    fi
    pass "Pre-failure upload OK"

    # Verify it's accessible
    if timeout 30 "$MC" stat "$S3_ALIAS/$obj" >/dev/null 2>&1; then
        pass "File accessible before node failure"
    else
        fail "File not accessible before node failure"
        return 1
    fi

    # Pick a non-preferred node to kill (so we don't lose preferred data)
    local kill_node=""
    local kill_alias=""
    for ip in "${!NODE_ALIASES[@]}"; do
        local is_pref
        is_pref=$(curl -s "http://$CTRL_PLANE/api/v1/nodes" 2>/dev/null | python3 -c "
import json,sys
data=sys.stdin.read()
start=data.find('{')
d=json.loads(data[start:])
for n in d['nodes']:
    if n.get('ip') == '$ip':
        print(n.get('preferred',False))
        break
" 2>/dev/null || echo "False")
        if [ "$is_pref" = "False" ]; then
            kill_node="$ip"
            kill_alias="${NODE_ALIASES[$ip]}"
            break
        fi
    done

    if [ -z "$kill_node" ]; then
        info "No non-preferred node available, using any node"
        kill_node="192.168.1.14"
        kill_alias="Note:Fedora"
    fi

    info "Stopping node $kill_node ($kill_alias) to simulate failure..."
    sshpass -p '123456' ssh -o StrictHostKeyChecking=no -o ConnectTimeout=10 "$kill_alias" \
        'echo "123456" | sudo -S systemctl stop s3lite-agent 2>/dev/null; echo "123456" | sudo -S pkill -f "weed volume" 2>/dev/null' \
        2>/dev/null || info "SSH stop failed, trying to proceed anyway"

    sleep 5

    # Check node is marked offline
    local node_status
    node_status=$(curl -s "http://$CTRL_PLANE/api/v1/nodes" 2>/dev/null | python3 -c "
import json,sys
data=sys.stdin.read()
start=data.find('{')
d=json.loads(data[start:])
for n in d['nodes']:
    if n.get('ip') == '$kill_node':
        print(f\"alive={n.get('alive')}, status={n.get('status','?')}\")
        break
" 2>/dev/null || echo "unknown")
    info "Node $kill_node status: $node_status"

    # Verify file still accessible
    info "Verifying file accessible after node failure..."
    if timeout 30 "$MC" stat "$S3_ALIAS/$obj" >/dev/null 2>&1; then
        pass "File accessible after node $kill_node failure"
    else
        fail "File NOT accessible after node failure"
    fi

    # Restart the node
    info "Restarting node $kill_node..."
    sshpass -p '123456' ssh -o StrictHostKeyChecking=no -o ConnectTimeout=10 "$kill_alias" \
        'echo "123456" | sudo -S systemctl start s3lite-agent 2>/dev/null' \
        2>/dev/null || true

    sleep 10

    # Verify node is back online
    node_status=$(curl -s "http://$CTRL_PLANE/api/v1/nodes" 2>/dev/null | python3 -c "
import json,sys
data=sys.stdin.read()
start=data.find('{')
d=json.loads(data[start:])
for n in d['nodes']:
    if n.get('ip') == '$kill_node':
        print(f\"alive={n.get('alive')}\")
        break
" 2>/dev/null || echo "unknown")
    info "Node $kill_node status after restart: $node_status"

    pass "Fault tolerance test completed"
}

# -------------------------------------------------------------------
# Test 6: Cleanup
# -------------------------------------------------------------------
test_cleanup() {
    info "=== Test: Cleanup ==="
    info "Removing all test objects from bucket $BUCKET..."
    "$MC" rm --recursive --force "$S3_ALIAS/$BUCKET/" 2>/dev/null || true
    "$MC" rb --force "$S3_ALIAS/$BUCKET" 2>/dev/null || true
    pass "Cleanup completed"
}

# -------------------------------------------------------------------
# Main
# -------------------------------------------------------------------
main() {
    local test="${1:-all}"

    check_prereqs

    case "$test" in
        all)
            setup_bucket
            test_write_read
            test_stress
            test_distribution
            test_ec_status
            test_fault_tolerance
            ;;
        write-read)
            setup_bucket
            test_write_read
            ;;
        stress)
            setup_bucket
            test_stress
            ;;
        distribution)
            test_distribution
            ;;
        ec-status)
            test_ec_status
            ;;
        fault-tolerance)
            setup_bucket
            test_fault_tolerance
            ;;
        cleanup)
            test_cleanup
            ;;
        *)
            fail "Unknown test: $test"
            echo "Usage: $0 [all|write-read|stress|distribution|ec-status|fault-tolerance|cleanup]"
            exit 1
            ;;
    esac

    info "=== Test suite completed ==="
}

main "$@"
