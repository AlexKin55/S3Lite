#!/usr/bin/env bash
#
# Local SeaweedFS test cluster (single host, distinct ports):
#   master (9333) + volume servers x10 (8081..8090) + filer (8888) + S3 gateway (8333)
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
VOLUME_BASE_PORT=8081
VOLUME_COUNT=10

# Node address in the cluster. 127.0.0.1 is avoided: newer SeaweedFS versions
# reject inner RPCs to loopback addresses (SSRF protection).
NODE_IP="${NODE_IP:-$(hostname -I 2>/dev/null | awk '{print $1}')}"
NODE_IP="${NODE_IP:-127.0.0.1}"

# Volume size limit on master (MB). Node capacity = max * volumeSizeLimitMB.
VOLUME_SIZE_LIMIT_MB=100

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

  echo "==> Starting SeaweedFS cluster: master:${MASTER_PORT}, volume x${count} (${VOLUME_BASE_PORT}..$((VOLUME_BASE_PORT + count - 1))), filer:${FILER_PORT}, s3:${S3_PORT}"

  run_bg master "${WEED}" master \
    -port="${MASTER_PORT}" \
    -mdir="${DATA_DIR}/master" \
    -volumeSizeLimitMB="${VOLUME_SIZE_LIMIT_MB}"

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
    -port="${S3_PORT}" \
    -filer="127.0.0.1:${FILER_PORT}" \
    -config="${PROJECT_ROOT}/src/thirdparty/seaweedfs/docker/compose/s3.json"

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

stop() {
  echo "==> Stopping the cluster..."
  # Stop in reverse order: s3, filer, volumeN..volume1, master
  local names=(s3 filer)
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
  echo "  s3:     http://localhost:${S3_PORT}"
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
