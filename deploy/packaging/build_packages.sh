#!/usr/bin/env bash
#
# Builds installable .deb and .rpm packages for the S3Lite agent and server.
#
# Output goes to deploy/packaging/dist/packages/.
#
# Usage:
#   ./build_packages.sh                        # agent + server, deb + rpm
#   ./build_packages.sh -p agent               # only agent (deb + rpm)
#   ./build_packages.sh -f rpm                 # only rpm (agent + server)
#   ./build_packages.sh -p server -f deb       # one package, one format
#   PKG_VERSION=0.2.0 ./build_packages.sh      # explicit version (default: git tag / 0.1.0)
#   PKG_RELEASE=2 ./build_packages.sh          # explicit package release number (default: 1)
#   SKIP_BUILD=1 ./build_packages.sh           # reuse existing bin/ binaries
#   GO_BIN=/usr/local/go/bin/go ./build_packages.sh
#
# Requirements:
#   - Go toolchain for the build (SKIP_BUILD=1 to reuse bin/)
#   - deb: dpkg-deb (Debian/Ubuntu) or GNU ar+tar fallback (any Linux)
#   - rpm: rpmbuild  (RPM distros, e.g. `dnf install rpm-build`)
#
set -euo pipefail

PACKAGING="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "${PACKAGING}/../.." && pwd)"
BIN="${ROOT}/bin"

DIST="${PACKAGING}/dist"
OUT="${DIST}/packages"
STAGE_DEB="${DIST}/stage/deb"
STAGE_RPM="${DIST}/stage/rpm"
RPMTOPDIR="${DIST}/rpmbuild"

PACKAGES="agent server"
FORMATS="deb rpm"

usage() {
  sed -n '2,20p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
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

# RPM versions must not contain '-' and must start with a digit.
RPM_VERSION="${VERSION//-/_}"
[[ "${RPM_VERSION}" =~ ^[0-9] ]] || RPM_VERSION="0.${RPM_VERSION}"

case "$(uname -m)" in
  x86_64 | amd64)      DEB_ARCH="amd64";  RPM_ARCH="x86_64"  ;;
  aarch64 | arm64)     DEB_ARCH="arm64";  RPM_ARCH="aarch64" ;;
  *) die "unsupported architecture: $(uname -m)" ;;
esac

echo "==> S3Lite packaging"
echo "    version:    ${VERSION} (rpm: ${RPM_VERSION}-${RELEASE})"
echo "    arch:       ${DEB_ARCH} / ${RPM_ARCH}"
echo "    packages:   ${PACKAGES}"
echo "    formats:    ${FORMATS}"
echo

# ---------------------------------------------------------------- tool checks

if [[ "${FORMATS}" == *deb* ]] && ! have dpkg-deb \
   && ! { have ar && have tar && have md5sum; }; then
  die "deb needs dpkg-deb or the ar+tar fallback (install binutils and GNU tar)"
fi
if [[ "${FORMATS}" == *rpm* ]] && ! have rpmbuild; then
  die "rpmbuild not found (install rpm-build, e.g. 'dnf install rpm-build')"
fi

find_go() {
  local g
  g="$(command -v go || true)"
  if [[ -z "${g}" || ! -x "${g}" ]] && [[ -x /usr/local/go/bin/go ]]; then
    g="/usr/local/go/bin/go"
  fi
  [[ -n "${g}" && -x "${g}" ]] || die "go not found. Install Go, set GO_BIN, or use SKIP_BUILD=1"
  echo "${g}"
}

# ---------------------------------------------------------------- binaries

if [[ "${SKIP_BUILD:-0}" == "1" ]]; then
  for b in weed agent server; do
    [[ -x "${BIN}/${b}" ]] || die "SKIP_BUILD=1 but ${BIN}/${b} is missing"
  done
  echo "==> Reusing binaries from ${BIN}"
else
  GO_BIN="${GO_BIN:-$(find_go)}"
  export PATH="$(dirname "${GO_BIN}"):${PATH}"
  echo "==> Building binaries (GO_BIN=${GO_BIN})"
  "${ROOT}/scripts/build_weed.sh"
  (cd "${ROOT}/src/agent"  && "${GO_BIN}" build -trimpath -o "${BIN}/agent"  ./cmd/agent)
  (cd "${ROOT}/src/server" && "${GO_BIN}" build -trimpath -o "${BIN}/server" ./cmd/server)
  echo
fi

mkdir -p "${OUT}"

# ---------------------------------------------------------------- staging

stage_agent_root() { # $1: root dir
  local root="$1"
  mkdir -p "${root}/usr/bin" \
           "${root}/usr/lib/systemd/system" \
           "${root}/etc/s3lite" \
           "${root}/usr/share/doc/s3lite-agent"
  cp "${BIN}/agent" "${root}/usr/bin/s3lite-agent"
  cp "${BIN}/weed"  "${root}/usr/bin/weed"
  cp "${PACKAGING}/systemd/s3lite-agent.service" "${root}/usr/lib/systemd/system/"
  cp "${PACKAGING}/env/s3lite-agent.env"         "${root}/etc/s3lite/agent.env"
  cp "${ROOT}/README.md"                         "${root}/usr/share/doc/s3lite-agent/README.md"
}

stage_server_root() { # $1: root dir
  local root="$1"
  mkdir -p "${root}/usr/bin" \
           "${root}/usr/lib/systemd/system" \
           "${root}/etc/s3lite" \
           "${root}/usr/share/doc/s3lite-server"
  cp "${BIN}/server" "${root}/usr/bin/s3lite-server"
  cp "${PACKAGING}/systemd/s3lite-server.service" "${root}/usr/lib/systemd/system/"
  cp "${PACKAGING}/env/s3lite-server.env"         "${root}/etc/s3lite/server.env"
  cp "${ROOT}/README.md"                          "${root}/usr/share/doc/s3lite-server/README.md"
}

mk_deb_dir() { # $1: pkg (agent|server)
  local pkg="$1" root="${STAGE_DEB}/${pkg}"
  rm -rf "${root}"
  mkdir -p "${root}/DEBIAN"
  "stage_${pkg}_root" "${root}"
  sed -e "s|@@VERSION@@|${VERSION}|g" \
      -e "s|@@ARCH@@|${DEB_ARCH}|g" \
      "${PACKAGING}/deb/${pkg}-control" > "${root}/DEBIAN/control"
  cp "${PACKAGING}/deb/${pkg}-postinst" "${root}/DEBIAN/postinst"
  cp "${PACKAGING}/deb/${pkg}-prerm"    "${root}/DEBIAN/prerm"
  cp "${PACKAGING}/deb/${pkg}-postrm"   "${root}/DEBIAN/postrm"
  printf '/etc/s3lite/%s.env\n' "${pkg}" > "${root}/DEBIAN/conffiles"
  chmod 0755 "${root}/DEBIAN/postinst" "${root}/DEBIAN/prerm" "${root}/DEBIAN/postrm"
}

mk_rpm_dir() { # $1: pkg (agent|server)
  local pkg="$1" root="${STAGE_RPM}/${pkg}" docdir
  rm -rf "${root}"
  mkdir -p "${STAGE_RPM}"
  cp -a "${STAGE_DEB}/${pkg}" "${root}"
  rm -rf "${root}/DEBIAN" "${root}/usr/share/doc"
  # Store README under the distro-specific rpm docdir (%{_defaultdocdir},
  # e.g. /usr/share/doc/packages on openSUSE) so %files and the staged tree
  # always match.
  docdir="$(rpm --eval '%{_defaultdocdir}')"
  mkdir -p "${root}${docdir}/s3lite-${pkg}"
  cp "${ROOT}/README.md" "${root}${docdir}/s3lite-${pkg}/README.md"
}

# ---------------------------------------------------------------- deb

make_deb_pkg() { # $1: pkg (agent|server)
  local pkg="$1"
  echo "==> deb: s3lite-${pkg}_${VERSION}_${DEB_ARCH}.deb"
  dpkg-deb --build --root-owner-group \
    "${STAGE_DEB}/${pkg}" \
    "${OUT}/s3lite-${pkg}_${VERSION}_${DEB_ARCH}.deb" \
    | sed 's/^/    /'
}

# Fallback deb builder: a .deb is just an ar archive with control.tar.gz,
# data.tar.gz and a "2.0" version marker. Works without dpkg-deb.
make_deb_native() { # $1: pkg (agent|server)
  local pkg="$1" root="${STAGE_DEB}/${pkg}" \
        out="${OUT}/s3lite-${pkg}_${VERSION}_${DEB_ARCH}.deb" \
        tmp control_tar data_tar
  echo "==> deb (ar+tar fallback): s3lite-${pkg}_${VERSION}_${DEB_ARCH}.deb"
  tmp="$(mktemp -d)"
  trap 'rm -rf "${tmp}"' RETURN
  mkdir -p "${tmp}/control" "${tmp}/data"

  cp "${root}/DEBIAN/control"    "${tmp}/control/control"
  cp "${root}/DEBIAN/conffiles"  "${tmp}/control/conffiles"
  cp "${root}/DEBIAN/postinst"   "${tmp}/control/postinst"
  cp "${root}/DEBIAN/prerm"      "${tmp}/control/prerm"
  cp "${root}/DEBIAN/postrm"     "${tmp}/control/postrm"
  chmod 0755 "${tmp}/control/postinst" "${tmp}/control/prerm" "${tmp}/control/postrm"

  # md5sums of every data file (the DEBIAN/ dir is excluded).
  ( cd "${root}" && find . -path './DEBIAN' -prune -o -type f -print \
      | sed 's|^\./||' | sort | xargs -r md5sum ) > "${tmp}/control/md5sums"

  control_tar="${tmp}/control.tar.gz"
  data_tar="${tmp}/data.tar.gz"
  tar -C "${tmp}/control" -czf "${control_tar}" .
  tar -C "${root}" --owner=0 --group=0 --exclude='./DEBIAN' -czf "${data_tar}" .
  printf '2.0\n' > "${tmp}/debian-binary"

  ar rcs "${out}" "${tmp}/debian-binary" "${control_tar}" "${data_tar}"
}

# ---------------------------------------------------------------- rpm

make_rpm_pkg() { # $1: pkg (agent|server)
  local pkg="$1" rpmfile
  echo "==> rpm: s3lite-${pkg}-${RPM_VERSION}-${RELEASE}.${RPM_ARCH}.rpm"
  "${RPMBUILD:-rpmbuild}" -bb \
    --define "_topdir ${RPMTOPDIR}" \
    --define "_sourcedir ${STAGE_RPM}" \
    --define "pkgversion ${RPM_VERSION}" \
    --define "pkgrelease ${RELEASE}" \
    "${PACKAGING}/rpm/s3lite-${pkg}.spec" \
    | sed 's/^/    /'
  rpmfile="$(find "${RPMTOPDIR}/RPMS" -name "s3lite-${pkg}-${RPM_VERSION}-${RELEASE}.*.rpm" -print -quit)"
  [[ -n "${rpmfile}" ]] || die "rpmbuild produced no package for ${pkg}"
  cp "${rpmfile}" "${OUT}/$(basename "${rpmfile}")"
}

# ---------------------------------------------------------------- run

for pkg in ${PACKAGES}; do
  case "${pkg}" in agent|server) ;; *) die "unknown package: ${pkg}" ;; esac
  mk_deb_dir "${pkg}"
done

if [[ "${FORMATS}" == *deb* ]]; then
  for pkg in ${PACKAGES}; do
    if have dpkg-deb; then
      make_deb_pkg "${pkg}"
    else
      have ar && have tar && have md5sum || die "deb fallback needs ar (binutils), tar and md5sum"
      make_deb_native "${pkg}"
    fi
  done
fi

if [[ "${FORMATS}" == *rpm* ]]; then
  for pkg in ${PACKAGES}; do mk_rpm_dir "${pkg}"; done
  for pkg in ${PACKAGES}; do make_rpm_pkg "${pkg}"; done
fi

echo
echo "==> Packages in ${OUT}:"
ls -lh "${OUT}" | awk 'NR==1 || $9 != "" {print "    " $5 "  " $9}'