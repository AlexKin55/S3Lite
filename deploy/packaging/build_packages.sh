#!/usr/bin/env bash
#
# Builds .deb and .rpm packages for the S3Lite agent and server using nfpm
# (https://nfpm.goreleaser.com).
#
# Output goes to deploy/packaging/dist/packages/.
#
# Usage:
#   ./build_packages.sh                        # agent + server, deb + rpm
#   ./build_packages.sh -p agent               # only agent (deb + rpm)
#   ./build_packages.sh -f rpm                 # only rpm (agent + server)
#   ./build_packages.sh -p server -f deb       # one package, one format
#   PKG_VERSION=0.2.0 ./build_packages.sh      # explicit version (default: git tag / 0.1.0)
#   PKG_RELEASE=2 ./build_packages.sh          # explicit rpm release number (default: 1)
#   SKIP_BUILD=1 ./build_packages.sh           # reuse existing bin/ binaries
#   GO_BIN=/usr/local/go/bin/go ./build_packages.sh
#
# Requirements:
#   - nfpm v2: `go install github.com/goreleaser/nfpm/v2/cmd/nfpm@latest`
#     or https://github.com/goreleaser/nfpm/releases
#   - Go toolchain for the build (SKIP_BUILD=1 to reuse bin/)
#
set -euo pipefail

PACKAGING="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "${PACKAGING}/../.." && pwd)"
BIN="${ROOT}/bin"
OUT="${PACKAGING}/dist/packages"

PACKAGES="agent server"
FORMATS="deb rpm"

usage() {
  sed -n '3,16p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
  echo
  echo "Options:"
  echo "  -p <list>   packages to build: agent, server or both (default: '${PACKAGES}')"
  echo "  -f <list>   formats to build: deb, rpm or both (default: '${FORMATS}')"
  echo "  -h          show this help"
}

while getopts ":p:f:h" opt; do
  case "${opt}" in
    p) PACKAGES="${OPTARG}" ;;
    f) FORMATS="${OPTARG}" ;;
    h) usage; exit 0 ;;
    *) usage; exit 1 ;;
  esac
done
shift $((OPTIND - 1))

die() { echo "ERROR: $*" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

# ---------------------------------------------------------------- nfpm

NFPM="$(command -v nfpm || true)"
if [[ -z "${NFPM}" && -x "${GOPATH:-$HOME/go}/bin/nfpm" ]]; then
  NFPM="${GOPATH:-$HOME/go}/bin/nfpm"
fi
if [[ -z "${NFPM}" || ! -x "${NFPM}" ]]; then
  die "nfpm not found. Install it: go install github.com/goreleaser/nfpm/v2/cmd/nfpm@latest"
fi

# ---------------------------------------------------------------- version/arch

VERSION="${PKG_VERSION:-}"
if [[ -z "${VERSION}" ]]; then
  if VERSION="$(git -C "${ROOT}" describe --tags --always --dirty 2>/dev/null)"; then
    VERSION="${VERSION#v}"
  else
    VERSION="0.1.0"
  fi
fi
RELEASE="${PKG_RELEASE:-1}"

# Package versions must not contain '-' (deb splits upstream/revision on the
# last dash, rpm forbids it entirely) and must start with a digit, so both
# deb and rpm get the same sanitized version.
SAFE_VERSION="${VERSION//-/_}"
[[ "${SAFE_VERSION}" =~ ^[0-9] ]] || SAFE_VERSION="0.${SAFE_VERSION}"

case "$(uname -m)" in
  x86_64 | amd64)  DEB_ARCH="amd64"; RPM_ARCH="x86_64" ;;
  aarch64 | arm64) DEB_ARCH="arm64"; RPM_ARCH="aarch64" ;;
  *) die "unsupported architecture: $(uname -m)" ;;
esac

echo "==> S3Lite packaging (nfpm)"
echo "    nfpm:       ${NFPM}"
echo "    version:    ${VERSION} (packages: ${SAFE_VERSION}-${RELEASE})"
echo "    arch:       ${DEB_ARCH} / ${RPM_ARCH}"
echo "    packages:   ${PACKAGES}"
echo "    formats:    ${FORMATS}"
echo

# ---------------------------------------------------------------- binaries

if [[ "${SKIP_BUILD:-0}" == "1" ]]; then
  for b in weed agent server; do
    [[ -x "${BIN}/${b}" ]] || die "SKIP_BUILD=1 but ${BIN}/${b} is missing"
  done
  echo "==> Reusing binaries from ${BIN}"
else
  GO_BIN="${GO_BIN:-}"
  if [[ -z "${GO_BIN}" ]]; then
    GO_BIN="$(command -v go || true)"
  fi
  if [[ -z "${GO_BIN}" || ! -x "${GO_BIN}" ]] && [[ -x /usr/local/go/bin/go ]]; then
    GO_BIN="/usr/local/go/bin/go"
  fi
  [[ -n "${GO_BIN}" && -x "${GO_BIN}" ]] || die "go not found. Install Go, set GO_BIN, or use SKIP_BUILD=1"
  export PATH="$(dirname "${GO_BIN}"):${PATH}"
  echo "==> Building binaries (GO_BIN=${GO_BIN})"
  "${ROOT}/scripts/build_weed.sh"
  (cd "${ROOT}/src/agent"  && "${GO_BIN}" build -trimpath -o "${BIN}/agent"  ./cmd/agent)
  (cd "${ROOT}/src/server" && "${GO_BIN}" build -trimpath -o "${BIN}/server" ./cmd/server)
  echo
fi

mkdir -p "${OUT}"

# ---------------------------------------------------------------- nfpm build

build_one() { # $1: pkg (agent|server), $2: fmt (deb|rpm)
  local pkg="$1" fmt="$2" file cfg pkg_ver
  pkg_ver="${SAFE_VERSION}"
  if [[ "${fmt}" == "deb" ]]; then
    file="${OUT}/s3lite-${pkg}_${SAFE_VERSION}_${DEB_ARCH}.deb"
  else
    file="${OUT}/s3lite-${pkg}-${SAFE_VERSION}-${RELEASE}.${RPM_ARCH}.rpm"
  fi
  # nfpm has no CLI overrides for metadata, so substitute the placeholders.
  cfg="${OUT}/.nfpm-${pkg}-${fmt}.yaml"
  sed -e "s|@@PKG_VERSION@@|${pkg_ver}|g" \
      -e "s|@@PKG_RELEASE@@|${RELEASE}|g" \
      -e "s|@@PKG_ARCH@@|${DEB_ARCH}|g" \
      "${PACKAGING}/nfpm-${pkg}.yaml" > "${cfg}"
  echo "==> nfpm (${fmt}): $(basename "${file}")"
  # relative src paths in the yaml are resolved from the packaging dir
  (cd "${PACKAGING}" && "${NFPM}" package \
      --config "${cfg}" \
      --target "${file}" \
      --packager "${fmt}" \
      | sed 's/^/    /')
  rm -f "${cfg}"
}

for pkg in ${PACKAGES}; do
  case "${pkg}" in agent|server) ;; *) die "unknown package: ${pkg}" ;; esac
  for fmt in ${FORMATS}; do
    case "${fmt}" in deb|rpm) ;; *) die "unknown format: ${fmt}" ;; esac
    build_one "${pkg}" "${fmt}"
  done
done

echo
echo "==> Packages in ${OUT}:"
ls -lh "${OUT}" | awk 'NR==1 || $9 != "" {print "    " $5 "  " $9}'