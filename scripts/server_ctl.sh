#!/usr/bin/env bash
#
# server_ctl.sh — вспомогательный скрипт для запуска, выгрузки и перезапуска
# всех служб s3lite-server в корректном порядке.
#
# Службы связаны зависимостями (см. deploy/obs/server/*.service):
#   s3lite-master  -> s3lite-filer -> s3lite-s3 -> s3lite-s3proxy
#   s3lite-master  -> s3lite-admin -> s3lite-ecworker
#   s3lite-server  (control plane, зависит от master и filer)
#
# Порядок запуска:   master -> filer -> s3 -> s3proxy -> admin -> ecworker -> server
# Порядок выгрузки:  s3proxy -> ecworker -> server -> admin -> s3 -> filer -> master
#
# Поведение при сбоях:
#   - уже остановленные/упавшие службы пропускаются (stop по ним — no-op);
#   - ошибка на одной службе не прерывает обработку остальных: все проблемы
#     собираются и выводятся в конце с ненулевым кодом выхода;
#   - при запуске каждая служба ждёт готовности своего порта (60 с), чтобы
#     зависимые не стартовали вхолостую; недождавшийся порт фиксируется
#     как предупреждение, но остальные службы продолжают запускаться.
#
# Использование:
#   sudo ./server_ctl.sh start           # запуск всех служб по порядку
#   sudo ./server_ctl.sh stop            # выгрузка всех служб в обратном порядке
#   sudo ./server_ctl.sh restart         # stop + start
#   sudo ./server_ctl.sh status          # текущее состояние всех служб
#   Флаги: --skip-wait — не ждать готовности портов при запуске
#
set -euo pipefail

# Порядок запуска (в порядке зависимостей). Выгрузка — в обратном порядке.
SERVICES=(
    s3lite-master
    s3lite-filer
    s3lite-s3
    s3lite-s3proxy
    s3lite-admin
    s3lite-ecworker
    s3lite-server
)

# Порт для проверки готовности (пусто — не проверять). Индексы совпадают с SERVICES.
READY_PORTS=(
    9333   # master
    8888   # filer
    8332   # s3 gateway
    8333   # s3proxy (внешняя точка входа)
    12535  # admin
    ""     # ecworker — входящих портов не слушает
    8099   # s3lite-server (control plane, LISTEN=:8099 по умолчанию)
)

SKIP_WAIT=0
CMD=""
for arg in "$@"; do
    case "$arg" in
        start|stop|restart|status) CMD="$arg" ;;
        --skip-wait) SKIP_WAIT=1 ;;
        -h|--help)
            sed -n '2,32p' "$0" | sed 's/^# \{0,1\}//'
            exit 0
            ;;
        *)
            echo "Неизвестный аргумент: $arg (см. --help)" >&2
            exit 2
            ;;
    esac
done
if [[ -z "$CMD" ]]; then
    echo "Использование: $0 {start|stop|restart|status} [--skip-wait]" >&2
    exit 2
fi

if [[ "$CMD" != "status" && $EUID -ne 0 ]]; then
    echo "Ошибка: запустите скрипт от root (sudo)." >&2
    exit 1
fi
if ! command -v systemctl >/dev/null 2>&1; then
    echo "Ошибка: systemctl не найден — требуется systemd." >&2
    exit 1
fi

log() { printf '[s3lite] %s\n' "$*"; }

# Проверка существования unit-файлов: пропускаем неустановленные службы,
# чтобы скрипт работал и на неполных инсталляциях.
installed_units=()
installed_ports=()
for i in "${!SERVICES[@]}"; do
    svc="${SERVICES[$i]}"
    if systemctl cat "${svc}.service" >/dev/null 2>&1; then
        installed_units+=("$svc")
        installed_ports+=("${READY_PORTS[$i]}")
    else
        log "Пропуск ${svc}.service: unit не установлен"
    fi
done

wait_port() {
    # wait_port <имя> <порт> — ждёт, пока порт начнёт принимать соединения.
    local name="$1" port="$2"
    local timeout=60 waited=0
    if [[ -z "$port" ]]; then
        return 0
    fi
    log "Ожидание готовности ${name} (порт ${port})..."
    while (( waited < timeout )); do
        if (exec 3<>"/dev/tcp/127.0.0.1/${port}") 2>/dev/null; then
            log "${name} готов"
            return 0
        fi
        sleep 1
        waited=$((waited + 1))
    done
    echo "Предупреждение: ${name} не открыл порт ${port} за ${timeout}s" >&2
    return 1
}

do_start() {
    log "=== Запуск служб (в порядке зависимостей) ==="
    local errors=0
    for i in "${!installed_units[@]}"; do
        svc="${installed_units[$i]}"
        if systemctl is-active --quiet "${svc}.service"; then
            log "${svc}.service уже запущен"
            continue
        fi
        log "Запуск ${svc}.service"
        if ! systemctl start "${svc}.service"; then
            echo "Ошибка: не удалось запустить ${svc}.service (см. journalctl -u ${svc})" >&2
            errors=$((errors + 1))
            continue   # ошибка одной службы не останавливает запуск остальных
        fi
        if (( ! SKIP_WAIT )); then
            wait_port "$svc" "${installed_ports[$i]}" || errors=$((errors + 1))
        fi
    done
    return "$errors"
}

do_stop() {
    log "=== Выгрузка служб (обратный порядок зависимостей) ==="
    local errors=0
    for (( i=${#installed_units[@]}-1; i>=0; i-- )); do
        svc="${installed_units[$i]}"
        if ! systemctl is-active --quiet "${svc}.service" \
           && ! systemctl is-failed --quiet "${svc}.service"; then
            log "${svc}.service уже остановлен"
            continue
        fi
        log "Остановка ${svc}.service"
        if ! systemctl stop "${svc}.service"; then
            echo "Ошибка: не удалось остановить ${svc}.service" >&2
            errors=$((errors + 1))
            continue   # выгрузка остальных служб продолжается
        fi
    done
    return "$errors"
}

do_status() {
    log "=== Статус служб ==="
    local overall=0 state
    for svc in "${installed_units[@]}"; do
        state="$(systemctl is-active "${svc}.service" 2>/dev/null || true)"
        log "  ${svc}.service: ${state}"
        [[ "$state" == "active" ]] || overall=1
    done
    return "$overall"
}

case "$CMD" in
    start)   do_start ;;
    stop)    do_stop ;;
    restart) do_stop && log "---" && do_start ;;
    status)  do_status ;;
esac
rc=$?

log "=== Итоговый статус ==="
for svc in "${installed_units[@]}"; do
    log "  ${svc}.service: $(systemctl is-active "${svc}.service" 2>/dev/null || echo unknown)"
done

if (( rc != 0 )); then
    echo "Завершено с проблемами (код ${rc}) — проверьте journalctl -u <служба>." >&2
else
    log "Операция '${CMD}' выполнена успешно"
fi
exit "$rc"
