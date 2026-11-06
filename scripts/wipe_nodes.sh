#!/usr/bin/env bash
#
# wipe_nodes.sh — удаление данных и томов на agent-нодах кластера s3lite по SSH.
#
# Список нод берётся из ~/.ssh/config (алиасы вида Home:Fedora, Note:Suse и т.д.).
# На каждой ноде скрипт:
#   1. Останавливает s3lite-agent.service
#   2. Удаляет всё содержимое /var/lib/s3lite (тома агента, agent-state)
#   3. Пересоздаёт пустую директорию с владельцем s3lite:s3lite
#   4. Запускает s3lite-agent.service снова
#
# Использование:
#   sudo ./scripts/wipe_nodes.sh           # очистить все ноды из списка (параллельно)
#   sudo ./scripts/wipe_nodes.sh --serial  # очистить последовательно
#   sudo ./scripts/wipe_nodes.sh --help
#
set -euo pipefail

SUDO_PASS="${SUDO_PASS:-123456}"

# Ноды из ~/.ssh/config (алиасы, по которым ssh <alias> работает без ключей).
NODES=(
    Home:Fedora
    Home:Fedora2
    Home:Fedora3
    Note:Fedora
    Note:Fedora3
    Note:Fedora2
    Note:Suse
    Note:Suse2
    Home:Suse2
    Home:Suse
)

SERIAL=0
for arg in "$@"; do
    case "$arg" in
        --serial) SERIAL=1 ;;
        -h|--help)
            sed -n '2,18p' "$0" | sed 's/^# \{0,1\}//'
            exit 0
            ;;
        *)
            echo "Неизвестный аргумент: $arg (см. --help)" >&2
            exit 2
            ;;
    esac
done

log() { printf '[wipe_nodes] %s\n' "$*"; }

wipe_node() {
    local node="$1"
    log "=== $node: очистка ==="

    local script
    script=$(cat <<REMOTE
set -e
echo '${SUDO_PASS}' | sudo -S systemctl stop s3lite-agent.service 2>&1
echo '${SUDO_PASS}' | sudo -S rm -rf /var/lib/s3lite/* 2>&1
echo '${SUDO_PASS}' | sudo -S mkdir -p /var/lib/s3lite 2>&1
echo '${SUDO_PASS}' | sudo -S chown s3lite:s3lite /var/lib/s3lite 2>&1
echo '${SUDO_PASS}' | sudo -S systemctl start s3lite-agent.service 2>&1
echo '${SUDO_PASS}' | sudo -S ls -la /var/lib/s3lite/ 2>&1
REMOTE
    )

    ssh -o ConnectTimeout=10 -o BatchMode=no "$node" bash -c "$script" 2>&1
    local rc=$?
    if [[ $rc -ne 0 ]]; then
        log "=== $node: ОШИБКА (код $rc) ==="
        return $rc
    fi
    log "=== $node: готово ==="
}

if [[ "${SERIAL}" -eq 1 ]]; then
    for node in "${NODES[@]}"; do
        wipe_node "$node" || log "Внимание: не удалось очистить $node"
    done
else
    pids=()
    for node in "${NODES[@]}"; do
        ( wipe_node "$node" ) &
        pids+=("$!")
    done
    failed=0
    for pid in "${pids[@]}"; do
        wait "${pid}" || failed=$((failed + 1))
    done
    if [[ "${failed}" -gt 0 ]]; then
        log "Внимание: ${failed} нод(ы) завершились с ошибкой."
        exit 1
    fi
fi

log "Все ноды очищены."
