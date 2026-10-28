#
# spec file for package s3lite-weed
#
# Bundled SeaweedFS binaries, built by OBS from the git "main" branch.
# Shared by s3lite-server (runs the master via s3lite-master.service) and
# s3lite-agent (runs a local volume server), so the control plane does not
# have to pull the node agent just to obtain the weed binary.
#
# Source handling:
#   _service (tar_scm) downloads the S3Lite repo and produces
#   s3lite-<revision>.tar.gz (no versionformat by design — the archive name
#   reflects the git revision). The spec has no Source0; %prep unpacks the
#   matching archive via a glob and the VERSION file pins the real version.
#
# Versioning:
#   The single source of truth for the package version is
#   src/thirdparty/seaweedfs/VERSION committed in the repo. Version: in this
#   spec must match it (the check in %prep fails the build otherwise).
#
Name:           s3lite-weed
Version:        0.0.3
Release:        0
Summary:        S3Lite distributed S3 storage - bundled SeaweedFS binaries
License:        Apache-2.0
URL:            https://github.com/AlexKin55/S3Lite

# --- per-distro build/runtime prerequisites -------------------------------
%if 0%{?suse_version}
Group:          System/Base
BuildRoot:      %{_tmppath}/%{name}-%{version}-build
BuildRequires:  golang >= 1.26
%endif
%if 0%{?fedora}
BuildRequires:  golang >= 1.26
%endif
%if 0%{?ubuntu} || 0%{?debian}
BuildRequires:  golang-go >= 1.26
%endif

%description
The SeaweedFS weed binary bundled with S3Lite. This package is a shared
dependency of s3lite-server (which runs a master through
s3lite-master.service) and s3lite-agent (which runs a local volume server),
so installing the control plane does not require the node agent itself.

%prep
# The OBS source services may deliver the archive as s3lite-<version>.tar.gz
# or keep the "_service:<service>:" prefix (server-side service run), so
# unpack the matching archive instead of relying on an exact Source0 name.
rm -rf s3lite-src
mkdir s3lite-src
tar --strip-components=1 -xzf %{_sourcedir}/*s3lite*.tar.gz -C s3lite-src
cd s3lite-src
# Vendored Go deps ship as a compressed bundle (the artifact the official
# obs-service-go_modules produces); extract it back into the module.
tar xzf deploy/obs/vendor/seaweedfs.tar.gz -C src/thirdparty/seaweedfs
# Diagnostics: confirm the bundle carried real content, not an LFS pointer.
ls -lh deploy/obs/vendor/
du -sh src/thirdparty/seaweedfs/vendor
ls src/thirdparty/seaweedfs/vendor | head -5
# The VERSION file is the version source of truth.
test "$(cat src/thirdparty/seaweedfs/VERSION)" = "%{version}" || {
  echo "ERROR: src/thirdparty/seaweedfs/VERSION ($(cat src/thirdparty/seaweedfs/VERSION)) does not match spec Version (%{version})" >&2
  exit 1
}

%build
# rpmbuild re-establishes the build dir before each scriptlet,
# so re-enter the unpacked archive explicitly.
cd s3lite-src
export GO111MODULE=on
export GOPATH="%{_builddir}/gopath"
export GOCACHE="%{_builddir}/gocache"
# Dependencies are vendored; never touch the network — fail loudly instead.
export GOFLAGS=-mod=vendor
export GOPROXY=off
(cd src/thirdparty/seaweedfs/weed && go build -trimpath -ldflags "-s -w" -o weed .)

%install
cd s3lite-src
mkdir -p %{buildroot}%{_bindir}
install -m 0755 src/thirdparty/seaweedfs/weed/weed %{buildroot}%{_bindir}/weed
install -D -m 0644 README.md %{buildroot}%{_defaultdocdir}/s3lite-weed/README.md

%files
%defattr(-,root,root)
%{_bindir}/weed
%doc README.md

%changelog
* Tue Oct 27 2026 S3Lite <dev@s3lite.dev> - 0.0.2-0
- Split the bundled SeaweedFS binaries into their own package so s3lite-server
  does not have to depend on the node agent