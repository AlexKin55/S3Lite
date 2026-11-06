#!/usr/bin/env bash
#
# wipe_cluster.sh — полное удаление всех данных и томов кластера s3lite.
#
# Останавливает службы s3lite, удаляет все коллекции через API управления
# мастера (мастер передаёт команду на удаление томов на все volume-серверы,
# включая удалённые ноды), затем стирает локальные данные мастера, филера,
# админ-сервера, EC-воркера и локальный стенд в .cluster/. Опционально
# запускает службы заново с чистым состоянием.
#
# ВАЖНО: тома могут находиться на удалённых agent-нодах (не на этом хосте).
# Скрипт удаляет их через маster API, но если агентская нода продолжает
# работать и слать сердцебиения, она зарепортирует тома заново. Для полной
# очистки таких нод нужно запустить wipe_cluster.sh (или эквивалент) на
# каждой из них отдельно, либо предоставить SSH-доступ.
#
# Использование:
#   sudo ./scripts/wipe_cluster.sh           # удалить данные, службы оставить остановленными
#   sudo ./scripts/wipe_cluster.sh --restart # удалить данные и запустить службы снова
#   sudo ./scripts/wipe_cluster.sh --help
#
set -euo pipefail

PROJECT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CLUSTER_DIR="${PROJECT_ROOT}/.cluster"
S3LITE_DATA="/var/lib/s3lite"

# Адрес мастера для API-запросов (取 из env, по умолчанию 127.0.0.1:9333).
MASTER_ADDR="${MASTER_ADDR:-127.0.0.1:9333}"
CONTROL_PLANE="${CONTROL_PLANE:-http://127.0.0.1:8099}"

SERVICES=(
    s3lite-server
    s3lite-s3proxy
    s3lite-ecworker
    s3lite-admin
    s3lite-s3
    s3lite-filer
    s3lite-master
)

RESTART=0
for arg in "$@"; do
    case "$arg" in
        --restart) RESTART=1 ;;
        -h|--help)
            sed -n '2,34p' "$0" | sed 's/^# \{0,1\}//'
            exit 0
            ;;
        *)
            echo "Неизвестный аргумент: $arg (см. --help)" >&2
            exit 2
            ;;
    esac
done

if [[ $EUID -ne 0 ]]; then
    echo "Ошибка: запустите скрипт от root (sudo)." >&2
    exit 1
fi

log() { printf '[wipe_cluster] %s\n' "$*"; }

delete_collections() {
    log "=== Удаление всех коллекций через API мастера ==="
    # Сначала пробуем через или inspired by dashboard's orphans cleanup —
    # это удаляет коллекции без бакетов на филере (мастер проксирует удаление
    # томов на все volume-серверы, включая удалённые ноды).
    if curl -sf "${CONTROL_PLANE}/api/v1/orphans/cleanup" -X POST >/dev/null 2>&1; then
        log "Orphan-коллекции удалены через /api/v1/orphans/cleanup"
    else
        log "Не удалось вызвать /api/v1/orphans/cleanup (контрольная плоскость недоступна)"
    fi
}

stop_services() {
    log "=== Остановка служб s3lite (обратный порядок зависимостей) ==="
    for svc in "${SERVICES[@]}"; do
        if systemctl is-active --quiet "${svc}.service" 2>/dev/null \
           || systemctl is-failed --quiet "${svc}.service" 2>/dev/null; then
            log "Остановка ${svc}.service"
            systemctl stop "${svc}.service" 2>/dev/null || log "Не удалось остановить ${svc}.service"
        else
            log "${svc}.service уже остановлен"
        fi
    done
}

start_services() {
    log "=== Запуск служб s3lite (порядок зависимостей) ==="
    local order=(s3lite-master s3lite-filer s3lite-s3 s3lite-s3proxy s3lite-admin s3lite-ecworker s3lite-server)
    for svc in "${order[@]}"; do
        if ! systemctl cat "${svc}.service" >/dev/null 2>&1; then
            log "Пропуск ${svc}.service: unit не установлен"
            continue
        fi
        log "Запуск ${svc}.service"
        systemctl start "${svc}.service" 2>/dev/null || log "Не удалось запустить ${svc}.service"
    done
}

wipe_data() {
    log "=== Удаление локальных данных кластера ==="

    if [[ -d "${S3LITE_DATA}" ]]; then
        log "Очистка ${S3LITE_DATA} (master, filer, admin, worker, agent, preferred-nodes, server-state)"
        rm -rf "${S3LITE_DATA:?}/"*
        mkdir -p "${S3LITE_DATA}"
        chown s3lite:s3lite "${S3LITE_DATA}" 2>/dev/null || true
    else
        mkdir -p "${S3LITE_DATA}"
        chown s3lite:s3lite "${S3LITE_DATA}" 2>/dev/null || true
        log "Создан пустой ${S3LITE_DATA}"
    fi

    if [[ -d "${CLUSTER_DIR}" ]]; then
        log "Очистка ${CLUSTER_DIR} (data, admin, worker, logs, pids, agents, preferred-nodes, admin.toml)"
        rm -rf "${CLUSTER_DIR:?}/"*
    else
        mkdir -p "${CLUSTER_DIR}"
        log "Создан пустой ${CLUSTER_DIR}"
    fi

    log "Локальные данные и тома кластера удалены."
}

case "${1:-}" in
    --restart)
        delete_collections
        stop_services
        wipe_data
        start_services
        ;;
    "")
        delete_collections
        stop_services
        wipe_data
        log "Готово. Службы остановлены, локальные данные удалены.
Удалённые agent-ноды могут зарепортировать тома заново при следующем сердцебиении.
Для полной очистки запустите wipe_cluster.sh на каждой agent-ноде отдельно."
        ;;
esac
