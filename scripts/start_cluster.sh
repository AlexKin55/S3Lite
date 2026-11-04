#!/usr/bin/env bash
#
# Local SeaweedFS test cluster (single host, distinct ports):
#   master (9333) + volume servers x10 (8081..8090) + filer (8888) + S3 gateway (8333)
#   + admin server (12535) + maintenance worker (auto erasure coding 6+4)
#
# Each volume server emulates a separate PC node: its own data directory,
# its own port, its own rack. Future EC 6+4 needs >= 10 nodes.
#
# Usage:
#   ./start_cluster.sh start            start the cluster (10 nodes)
#   ./start_cluster.sh start -n 6       start the cluster with 6 nodes
#   ./start_cluster.sh stop             stop all processes
#   ./start_cluster.sh status           show process status
#   ./start_cluster.sh restart          restart
#
set -euo pipefail

PROJECT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WEED="${PROJECT_ROOT}/bin/weed"

CLUSTER_DIR="${PROJECT_ROOT}/.cluster"
DATA_DIR="${CLUSTER_DIR}/data"
LOG_DIR="${CLUSTER_DIR}/logs"
PID_DIR="${CLUSTER_DIR}/pids"

MASTER_PORT=9333
FILER_PORT=8888
S3_PORT=8333
S3_UPSTREAM_PORT=8332
VOLUME_BASE_PORT=8081
VOLUME_COUNT=10
ADMIN_PORT=12535
# Automatic erasure coding scheme (k+m): the maintenance worker encodes full,
# quiet volumes so data is spread over k+m shards across the nodes
# (6 data + 4 parity = survives the loss of 4 nodes).
EC_DATA_SHARDS=6
EC_PARITY_SHARDS=4
# Default replication for new volumes: 3 copies on 3 different racks, so
# every write is fault tolerant from the first second; the maintenance
# worker replaces the raw replicas with EC shards ~5 minutes after writes
# stop. Mirrors -defaultReplication=020 shipped in s3lite-master.service.
DEFAULT_REPLICATION=020
# Quiet period (seconds) before a volume is erasure-coded, mirroring
# /etc/s3lite/admin.toml of the packaged deployment.
EC_QUIET_SECONDS=300
# Volume fullness ratio below which quiet volumes are not encoded.
EC_FULLNESS_RATIO=0.04
EC_SCAN_INTERVAL_SECONDS=300

# Node address in the cluster. 127.0.0.1 is avoided: newer SeaweedFS versions
# reject inner RPCs to loopback addresses (SSRF protection).
NODE_IP="${NODE_IP:-$(hostname -I 2>/dev/null | awk '{print $1}')}"
NODE_IP="${NODE_IP:-127.0.0.1}"

# Volume size limit on master (MB). Node capacity = max * volumeSizeLimitMB.
# Single source of truth: deploy/obs/env/s3lite-server.env — the same variable
# the packaged s3lite-master.service consumes via /etc/s3lite/server.env.
VOLUME_SIZE_LIMIT_MB="$(sed -n 's/^VOLUME_SIZE_LIMIT_MB=//p' \
  "${PROJECT_ROOT}/deploy/obs/env/s3lite-server.env" | tail -n1)"
VOLUME_SIZE_LIMIT_MB="${VOLUME_SIZE_LIMIT_MB:-100}"

mkdir -p "${DATA_DIR}" "${LOG_DIR}" "${PID_DIR}"

if [[ ! -x "${WEED}" ]]; then
  echo "ERROR: build weed first: ./scripts/build_weed.sh" >&2
  exit 1
fi

run_bg() {
  local name="$1"; shift
  nohup "$@" >"${LOG_DIR}/${name}.log" 2>&1 &
  echo "$!" >"${PID_DIR}/${name}.pid"
  echo "  [${name}] PID=$(cat "${PID_DIR}/${name}.pid")"
}

start() {
  local count="${VOLUME_COUNT}"
  if [[ "${1:-}" == "-n" ]]; then
    count="$2"
  fi

  if [[ -f "${PID_DIR}/master.pid" ]] && kill -0 "$(cat "${PID_DIR}/master.pid")" 2>/dev/null; then
    echo "Cluster is already running. Use 'status' or 'stop'."
    return 0
  fi

  echo "==> Starting SeaweedFS cluster: master:${MASTER_PORT}, volume x${count} (${VOLUME_BASE_PORT}..$((VOLUME_BASE_PORT + count - 1))), filer:${FILER_PORT}, s3:${S3_PORT}, admin:${ADMIN_PORT} (EC ${EC_DATA_SHARDS}+${EC_PARITY_SHARDS})"

  # -preferredNodesFile is the preferred-replica list; in production the
  # control plane (s3lite-server dashboard ★ toggle) writes it and the
  # master re-reads it periodically. Locally it can be edited by hand.
  : >"${CLUSTER_DIR}/preferred-nodes.txt"
  run_bg master "${WEED}" master \
    -port="${MASTER_PORT}" \
    -mdir="${DATA_DIR}/master" \
    -volumeSizeLimitMB="${VOLUME_SIZE_LIMIT_MB}" \
    -defaultReplication="${DEFAULT_REPLICATION}" \
    -preferredNodesFile="${CLUSTER_DIR}/preferred-nodes.txt"

  sleep 1
  for i in $(seq 1 "${count}"); do
    local port=$((VOLUME_BASE_PORT + i - 1))
    mkdir -p "${DATA_DIR}/v${i}"
    run_bg "volume${i}" "${WEED}" volume \
      -ip="${NODE_IP}" \
      -port="${port}" \
      -dir="${DATA_DIR}/v${i}" \
      -master="127.0.0.1:${MASTER_PORT}" \
      -dataCenter=dc1 \
      -rack="rack${i}"
    # Brief pause so each node registers with the master before the next starts.
    sleep 0.3
  done

  sleep 1
  run_bg filer "${WEED}" filer \
    -port="${FILER_PORT}" \
    -master="127.0.0.1:${MASTER_PORT}"

  sleep 1
  run_bg s3 "${WEED}" s3 \
    -port="${S3_UPSTREAM_PORT}" \
    -filer="127.0.0.1:${FILER_PORT}" \
    -config="${PROJECT_ROOT}/src/thirdparty/seaweedfs/docker/compose/s3.json"

  sleep 1
  # Admin server: maintenance control plane the worker connects to. Its data
  # dir persists task configuration, incl. the EC data/parity shard counts
  # (defaults: 6+4, adjustable via the admin UI on :12535). The declarative
  # admin.toml (mirroring /etc/s3lite/admin.toml of the packaged deployment)
  # is applied at every startup and overrides UI edits, so the local stand
  # behaves like production: 3x replication is encoded into EC shards
  # ~5 minutes after writes stop.
  mkdir -p "${CLUSTER_DIR}/admin"
  cat >"${CLUSTER_DIR}/admin.toml" <<EOF
[maintenance]
enabled = true

[maintenance.erasure_coding]
quiet_for_seconds = ${EC_QUIET_SECONDS}
fullness_ratio = ${EC_FULLNESS_RATIO}
min_size_mb = 10
scan_interval_seconds = ${EC_SCAN_INTERVAL_SECONDS}
max_concurrent = 2
EOF
  run_bg admin "${WEED}" -config_dir="${CLUSTER_DIR}" admin \
    -port="${ADMIN_PORT}" \
    -master="127.0.0.1:${MASTER_PORT}" \
    -dataDir="${CLUSTER_DIR}/admin"
  wait_tcp "${ADMIN_PORT}"

  # Maintenance worker: runs automatic erasure coding. Full, quiet volumes are
  # encoded into EC 6+4 (10 shards over 10 nodes; any 6 shards restore the
  # data, so up to 4 nodes may fail) and the shards are spread across nodes.
  mkdir -p "${CLUSTER_DIR}/worker"
  run_bg ecworker "${WEED}" worker \
    -admin="127.0.0.1:${ADMIN_PORT}" \
    -jobType=erasure_coding \
    -workingDir="${CLUSTER_DIR}/worker"

  sleep 1
  # Thin S3 proxy in front of the gateway: turns 5xx downloads into an S3
  # error that names the offline PCs to power on (via the control plane).
  run_bg s3proxy "${PROJECT_ROOT}/bin/s3proxy" \
    -listen=":${S3_PORT}" \
    -upstream="http://127.0.0.1:${S3_UPSTREAM_PORT}" \
    -control="http://127.0.0.1:8099"

  echo "==> Waiting for the master to be ready..."
  for _ in $(seq 1 15); do
    if curl -sf "http://127.0.0.1:${MASTER_PORT}/cluster/status" >/dev/null 2>&1; then
      echo "==> Cluster is ready."
      status
      return 0
    fi
    sleep 1
  done
  echo "WARNING: master did not respond within 15s. See logs: ${LOG_DIR}/" >&2
  status
}

wait_tcp() {
  local port="$1"
  for _ in $(seq 1 20); do
    if (exec 3<>"/dev/tcp/127.0.0.1/${port}") 2>/dev/null; then
      exec 3>&- 3<&- 2>/dev/null || true
      return 0
    fi
    sleep 0.5
  done
  echo "WARNING: port ${port} did not open within 10s" >&2
  return 0
}

stop() {
  echo "==> Stopping the cluster..."
  # Stop in reverse order: ecworker, admin, s3proxy, s3, filer, volumeN..volume1, master
  local names=(ecworker admin s3proxy s3 filer)
  for i in $(seq "${VOLUME_COUNT}" -1 1); do
    names+=("volume${i}")
  done
  names+=(master)

  for name in "${names[@]}"; do
    local pidfile="${PID_DIR}/${name}.pid"
    if [[ -f "${pidfile}" ]]; then
      local pid
      pid="$(cat "${pidfile}")"
      if kill -0 "${pid}" 2>/dev/null; then
        kill "${pid}" 2>/dev/null || true
        for _ in $(seq 1 10); do
          kill -0 "${pid}" 2>/dev/null || break
          sleep 0.3
        done
        kill -0 "${pid}" 2>/dev/null && kill -9 "${pid}" 2>/dev/null || true
        echo "  [${name}] PID=${pid} stopped"
      fi
      rm -f "${pidfile}"
    fi
  done
  echo "==> Done."
}

status() {
  echo "==> Cluster status:"
  local any=0
  for pidfile in "${PID_DIR}"/*.pid; do
    [[ -e "${pidfile}" ]] || continue
    local name pid
    name="$(basename "${pidfile}" .pid)"
    pid="$(cat "${pidfile}")"
    if kill -0 "${pid}" 2>/dev/null; then
      echo "  [RUN ] ${name} (PID=${pid})"
      any=1
    else
      echo "  [DEAD] ${name} (PID=${pid})"
    fi
  done
  if [[ "${any}" -eq 0 ]]; then
    echo "  No processes."
  fi
  echo
  echo "  master: http://localhost:${MASTER_PORT}/ui/index.html"
  echo "  filer:  http://localhost:${FILER_PORT}/ui/index.html"
  echo "  s3:     http://localhost:${S3_PORT} (via s3proxy, upstream :${S3_UPSTREAM_PORT})"
  echo "  admin:  http://localhost:${ADMIN_PORT} (maintenance UI; EC worker auto-encodes 6+4)"
  if curl -sf "http://127.0.0.1:${MASTER_PORT}/cluster/status" 2>/dev/null | head -c 400 >/dev/null; then
    echo
    echo "  Master topology (live volume servers):"
    curl -sf "http://127.0.0.1:${MASTER_PORT}/cluster/status" 2>/dev/null | tr ',' '\n' | grep -o '"Volume Size":[0-9]*' || true
  fi
}

case "${1:-start}" in
  start)   shift || true; start "$@" ;;
  stop)    stop ;;
  restart) stop; sleep 1; start ;;
  status)  status ;;
  *) echo "Usage: $0 {start|stop|restart|status}" >&2; exit 1 ;;
esac
