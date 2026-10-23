#!/usr/bin/env bash
#
# Локальный прототип кластера SeaweedFS (всё на одном хосте, разные порты):
#   master (9333) + volume-серверы x10 (8081..8090) + filer (8888) + S3 gateway (8333)
#
# Каждый volume-сервер имитирует отдельный ПК-узел: свой каталог данных,
# свой порт, свой rack. Для будущего EC 6+4 нужно >=10 узлов.
#
# Использование:
#   ./start_cluster.sh start            — поднять кластер (10 узлов)
#   ./start_cluster.sh start -n 6       — поднять кластер с 6 узлами
#   ./start_cluster.sh stop             — остановить все процессы
#   ./start_cluster.sh status           — статус процессов
#   ./start_cluster.sh restart          — перезапуск
#
set -euo pipefail

PROJECT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
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

# Адрес узлов в кластере. Не используем 127.0.0.1: новые версии SeaweedFS
# запрещают внутренние RPC на loopback (SSRF-защита).
NODE_IP="${NODE_IP:-$(hostname -I 2>/dev/null | awk '{print $1}')}"
NODE_IP="${NODE_IP:-127.0.0.1}"

# Лимит размера тома на master (МБ). Ёмкость узла = max * volumeSizeLimitMB.
VOLUME_SIZE_LIMIT_MB=100

mkdir -p "${DATA_DIR}" "${LOG_DIR}" "${PID_DIR}"

if [[ ! -x "${WEED}" ]]; then
  echo "ОШИБКА: соберите weed сперва: ./src/scripts/build_weed.sh" >&2
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
    echo "Кластер уже запущен. Используйте 'status' или 'stop'."
    return 0
  fi

  echo "==> Запуск кластера SeaweedFS: master:${MASTER_PORT}, volume x${count} (${VOLUME_BASE_PORT}..$((VOLUME_BASE_PORT + count - 1))), filer:${FILER_PORT}, s3:${S3_PORT}"

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
    # Небольшая пауза, чтобы каждый узел успел зарегистрироваться у master
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

  echo "==> Ожидание готовности master..."
  for _ in $(seq 1 15); do
    if curl -sf "http://127.0.0.1:${MASTER_PORT}/cluster/status" >/dev/null 2>&1; then
      echo "==> Кластер готов."
      status
      return 0
    fi
    sleep 1
  done
  echo "ВНИМАНИЕ: master не ответил за 15 c. Смотрите логи: ${LOG_DIR}/" >&2
  status
}

stop() {
  echo "==> Остановка кластера..."
  # останавливаем в обратном порядке: s3, filer, volumeN..volume1, master
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
        echo "  [${name}] PID=${pid} остановлен"
      fi
      rm -f "${pidfile}"
    fi
  done
  echo "==> Готово."
}

status() {
  echo "==> Статус кластера:"
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
    echo "  Процессов нет."
  fi
  echo
  echo "  master: http://localhost:${MASTER_PORT}/ui/index.html"
  echo "  filer:  http://localhost:${FILER_PORT}/ui/index.html"
  echo "  s3:     http://localhost:${S3_PORT}"
  if curl -sf "http://127.0.0.1:${MASTER_PORT}/cluster/status" 2>/dev/null | head -c 400 >/dev/null; then
    echo
    echo "  Топология master (число живых volume-серверов):"
    curl -sf "http://127.0.0.1:${MASTER_PORT}/cluster/status" 2>/dev/null | tr ',' '\n' | grep -o '"Volume Size":[0-9]*' || true
  fi
}

case "${1:-start}" in
  start)   shift || true; start "$@" ;;
  stop)    stop ;;
  restart) stop; sleep 1; start ;;
  status)  status ;;
  *) echo "Использование: $0 {start|stop|restart|status}" >&2; exit 1 ;;
esac
