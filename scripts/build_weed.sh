#!/usr/bin/env bash
#
# Builds the weed (SeaweedFS) binary from src/thirdparty/seaweedfs
# Output: <PROJECT_ROOT>/bin/weed
#
# Usage:
#   ./build_weed.sh             plain build
#   ./build_weed.sh -v          verbose go build output
#   ./build_weed.sh -tags '...' build with extra build tags (e.g. sqlite)
#
set -euo pipefail

# Project root: contains src/ and bin/ (this script lives in scripts/).
PROJECT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WEED_SRC="${PROJECT_ROOT}/src/thirdparty/seaweedfs"
OUT_DIR="${PROJECT_ROOT}/bin"
OUT_BIN="${OUT_DIR}/weed"

GO_BIN="${GO_BIN:-}"
if [[ -z "${GO_BIN}" ]]; then
  GO_BIN="$(command -v go || true)"
fi
# Fall back to a system install under /usr/local/go
if [[ -z "${GO_BIN}" || ! -x "${GO_BIN}" ]] && [[ -x /usr/local/go/bin/go ]]; then
  GO_BIN="/usr/local/go/bin/go"
fi

if [[ -z "${GO_BIN}" || ! -x "${GO_BIN}" ]]; then
  echo "ERROR: Go not found. Install Go (e.g. /usr/local/go) or set GO_BIN." >&2
  exit 1
fi

VERBOSE=0
TAGS=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    -v|--verbose) VERBOSE=1; shift ;;
    -tags) TAGS="$2"; shift 2 ;;
    *) echo "Unknown argument: $1" >&2; exit 1 ;;
  esac
done

if [[ ! -d "${WEED_SRC}" ]]; then
  echo "ERROR: SeaweedFS sources not found: ${WEED_SRC}" >&2
  exit 1
fi

echo "==> Go:          ${GO_BIN}"
echo "==> Sources:     ${WEED_SRC}"
echo "==> Binary:      ${OUT_BIN}"
echo "==> Go version:  $("${GO_BIN}" version)"

mkdir -p "${OUT_DIR}"

BUILD_ARGS=(build -o "${OUT_BIN}")
if [[ -n "${TAGS}" ]]; then
  BUILD_ARGS+=(-tags "${TAGS}")
fi
if [[ "${VERBOSE}" -eq 0 ]]; then
  BUILD_ARGS+=(-trimpath)
fi

echo "==> Building..."
if [[ "${VERBOSE}" -eq 1 ]]; then
  (cd "${WEED_SRC}/weed" && "${GO_BIN}" "${BUILD_ARGS[@]}" .)
else
  (cd "${WEED_SRC}/weed" && "${GO_BIN}" "${BUILD_ARGS[@]}" . 2>&1 | grep -v "^go: downloading" || true)
fi

echo "==> Done: $(ls -lh "${OUT_BIN}" | awk '{print $5, $9}')"
"${OUT_BIN}" version
