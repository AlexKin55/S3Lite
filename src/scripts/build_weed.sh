#!/usr/bin/env bash
#
# Сборка бинаря weed (SeaweedFS) из src/thirdparty/seaweedfs
# Результат: <PROJECT_ROOT>/bin/weed
#
# Использование:
#   ./build_weed.sh            — обычная сборка
#   ./build_weed.sh -v         — подробный вывод go build
#   ./build_weed.sh -tags '...'— сборка с дополнительными тегами (например sqlite)
#
set -euo pipefail

# Корень проекта: содержит src/ и bin/ (скрипт лежит в src/scripts/)
PROJECT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
WEED_SRC="${PROJECT_ROOT}/src/thirdparty/seaweedfs"
OUT_DIR="${PROJECT_ROOT}/bin"
OUT_BIN="${OUT_DIR}/weed"

GO_BIN="${GO_BIN:-}"
if [[ -z "${GO_BIN}" ]]; then
  GO_BIN="$(command -v go || true)"
fi
# fallback на системную установку из /usr/local/go
if [[ -z "${GO_BIN}" || ! -x "${GO_BIN}" ]] && [[ -x /usr/local/go/bin/go ]]; then
  GO_BIN="/usr/local/go/bin/go"
fi

if [[ -z "${GO_BIN}" || ! -x "${GO_BIN}" ]]; then
  echo "ОШИБКА: Go не найден. Установите Go (например, /usr/local/go) или задайте GO_BIN." >&2
  exit 1
fi

VERBOSE=0
TAGS=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    -v|--verbose) VERBOSE=1; shift ;;
    -tags) TAGS="$2"; shift 2 ;;
    *) echo "Неизвестный аргумент: $1" >&2; exit 1 ;;
  esac
done

if [[ ! -d "${WEED_SRC}" ]]; then
  echo "ОШИБКА: исходники SeaweedFS не найдены: ${WEED_SRC}" >&2
  exit 1
fi

echo "==> Go:          ${GO_BIN}"
echo "==> Исходники:   ${WEED_SRC}"
echo "==> Бинарь:      ${OUT_BIN}"
echo "==> Версия Go:   $("${GO_BIN}" version)"

mkdir -p "${OUT_DIR}"

BUILD_ARGS=(build -o "${OUT_BIN}")
if [[ -n "${TAGS}" ]]; then
  BUILD_ARGS+=(-tags "${TAGS}")
fi
if [[ "${VERBOSE}" -eq 0 ]]; then
  BUILD_ARGS+=(-trimpath)
fi

echo "==> Сборка..."
if [[ "${VERBOSE}" -eq 1 ]]; then
  (cd "${WEED_SRC}/weed" && "${GO_BIN}" "${BUILD_ARGS[@]}" .)
else
  (cd "${WEED_SRC}/weed" && "${GO_BIN}" "${BUILD_ARGS[@]}" . 2>&1 | grep -v "^go: downloading" || true)
fi

echo "==> Готово: $(ls -lh "${OUT_BIN}" | awk '{print $5, $9}')"
"${OUT_BIN}" version
