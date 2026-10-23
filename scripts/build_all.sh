#!/usr/bin/env bash
#
# Builds all S3Lite binaries into <ROOT>/bin:
#   weed (SeaweedFS + our EC k+m patch), agent, server, mc (test client).
#
# Usage:
#   ./build_all.sh             build everything into bin/
#   ./build_all.sh -v          verbose go builds
#   GO_BIN=/path/to/go ./build_all.sh
#
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="${ROOT}/bin"
mkdir -p "${BIN}"

VERBOSE=0
[[ "${1:-}" == "-v" ]] && VERBOSE=1

GO_BIN="${GO_BIN:-}"
if [[ -z "${GO_BIN}" ]]; then
  GO_BIN="$(command -v go || true)"
fi
if [[ -z "${GO_BIN}" || ! -x "${GO_BIN}" ]] && [[ -x /usr/local/go/bin/go ]]; then
  GO_BIN="/usr/local/go/bin/go"
fi
if [[ -z "${GO_BIN}" || ! -x "${GO_BIN}" ]]; then
  echo "ERROR: Go not found. Set GO_BIN." >&2
  exit 1
fi
export PATH="$(dirname "${GO_BIN}"):${PATH}"

build() { # build <dir> <pkg> <out>
  local dir="$1" pkg="$2" out="$3"
  echo "==> ${out}"
  if [[ "${VERBOSE}" -eq 1 ]]; then
    (cd "${ROOT}/${dir}" && "${GO_BIN}" build -o "${BIN}/${out}" "${pkg}")
  else
    (cd "${ROOT}/${dir}" && "${GO_BIN}" build -o "${BIN}/${out}" "${pkg}" 2>&1 | grep -v '^go: downloading' || true)
  fi
}

# 1) SeaweedFS weed (with our EC k+m patch)
echo "==> weed"
"${ROOT}/scripts/build_weed.sh"

# 2) Mock S3 client used by the integration tests
echo "==> mc"
if [[ ! -x "${BIN}/mc" ]]; then
  "${GO_BIN}" install github.com/minio/mc@latest 2>/dev/null || true
  if [[ -f "${HOME}/go/bin/mc" ]]; then cp "${HOME}/go/bin/mc" "${BIN}/mc"; fi
fi

# 3) Agent and control plane
build "src/agent" "./cmd/agent" "agent"
build "src/server" "./cmd/server" "server"

# 4) S3 recovery-hint proxy (fronts the SeaweedFS S3 gateway)
echo "==> s3proxy"
build "src/s3proxy" "." "s3proxy"

echo
echo "==> Built binaries in ${BIN}:"
ls -lh "${BIN}" | awk '{print $5, $9}'
