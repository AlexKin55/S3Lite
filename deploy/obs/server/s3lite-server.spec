#
# spec file for package s3lite-server
#
# S3Lite control plane server, built by OBS from the git "release" branch.
#
# Source handling:
#   _service (tar_scm) downloads the S3Lite repo and produces
#   s3lite-<revision>.tar.gz (no versionformat by design — the archive name
#   reflects the git revision). The spec has no Source0; %prep unpacks the
#   matching archive via a glob and the VERSION file pins the real version.
#   The systemd unit (deploy/obs/server/s3lite-server.service) and the env
#   template (deploy/obs/env/s3lite-server.env) are installed from the
#   unpacked archive by their repo-relative paths.
#
# Versioning:
#   The single source of truth for the package version is
#   src/server/VERSION committed on the release branch. Version: in this
#   spec must match it (the check in %prep fails the build otherwise).
#
Name:           s3lite-server
Version:        0.0.3
Release:        0
Summary:        S3Lite distributed S3 storage - control plane server
License:        Apache-2.0
URL:            https://github.com/AlexKin55/S3Lite

# --- per-distro build/runtime prerequisites -------------------------------
%if 0%{?suse_version}
Group:          System/Daemons
BuildRoot:      %{_tmppath}/%{name}-%{version}-build
BuildRequires:  golang >= 1.26
Requires(pre):  shadow
%endif
%if 0%{?fedora}
BuildRequires:  golang >= 1.26
Requires(pre):  shadow-utils
%endif
%if 0%{?ubuntu} || 0%{?debian}
BuildRequires:  golang-go >= 1.26
Requires(pre):  passwd
%endif
Requires(post): systemd
Requires(preun): systemd
Requires(postun): systemd
# The SeaweedFS weed binary (needed to run the master) is always required at
# runtime; it comes from the shared s3lite-weed package, so installing
# s3lite-server does not pull in the node agent itself.
Requires:       s3lite-weed

%description
Central control plane of the S3Lite distributed S3 storage: node registry,
health monitoring, volume classification (OK/DEGRADED/AT_RISK), redundancy
evaluation and notifications, plus a browser dashboard and Prometheus
/metrics endpoint.

The s3lite-weed package (bundled SeaweedFS binaries) is pulled in
automatically as a dependency, and a SeaweedFS master (s3lite-master.service)
is installed and started right away.

Configure /etc/s3lite/server.env, then start the service:

    systemctl start s3lite-server

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
tar xzf deploy/obs/vendor/server.tar.gz -C src/server
# Diagnostics: confirm the bundle carried real content, not an LFS pointer.
ls -lh deploy/obs/vendor/
du -sh src/server/vendor
ls src/server/vendor | head -5
# The VERSION file on the release branch is the version source of truth.
test "$(cat src/server/VERSION)" = "%{version}" || {
  echo "ERROR: src/server/VERSION ($(cat src/server/VERSION)) does not match spec Version (%{version})" >&2
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
(cd src/server && go build -trimpath -ldflags "-s -w" -o s3lite-server ./cmd/server)

%install
cd s3lite-src
mkdir -p %{buildroot}%{_bindir}
mkdir -p %{buildroot}%{_unitdir}
mkdir -p %{buildroot}%{_sysconfdir}/s3lite
mkdir -p %{buildroot}%{_sysconfdir}/s3lite/tls
mkdir -p %{buildroot}%{_localstatedir}/lib/s3lite
mkdir -p %{buildroot}%{_localstatedir}/lib/s3lite/master
install -m 0755 src/server/s3lite-server %{buildroot}%{_bindir}/s3lite-server
install -m 0644 deploy/obs/server/s3lite-server.service %{buildroot}%{_unitdir}/s3lite-server.service
install -m 0644 deploy/obs/server/s3lite-master.service %{buildroot}%{_unitdir}/s3lite-master.service
install -m 0640 deploy/obs/env/s3lite-server.env %{buildroot}%{_sysconfdir}/s3lite/server.env
install -D -m 0644 README.md %{buildroot}%{_defaultdocdir}/s3lite-server/README.md

%pre
# Shared service account (also used by s3lite-agent).
# Create the group first, then the user as its member.
getent group s3lite >/dev/null 2>&1 || \
  groupadd --system s3lite 2>/dev/null || :
getent passwd s3lite >/dev/null 2>&1 || \
  useradd --system --gid s3lite --no-create-home --home-dir /var/lib/s3lite/agent \
          --shell /usr/sbin/nologin s3lite 2>/dev/null || :
exit 0

%post
install -d -m 0750 -o s3lite -g s3lite %{_localstatedir}/lib/s3lite
install -d -m 0750 -o s3lite -g s3lite %{_localstatedir}/lib/s3lite/master
install -d -m 0750 %{_sysconfdir}/s3lite/tls
if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
  systemctl daemon-reload >/dev/null 2>&1 || :
  # The master needs no configuration (auto-detects the LAN IP), so it is
  # enabled AND started right away: the control plane monitor requires it.
  systemctl enable --now s3lite-master.service >/dev/null 2>&1 || :
  # Enabled but NOT started: /etc/s3lite/server.env must be configured first.
  systemctl enable s3lite-server.service >/dev/null 2>&1 || :
fi
exit 0

%preun
if [ "$1" = 0 ]; then
  systemctl disable --now s3lite-master.service >/dev/null 2>&1 || :
  systemctl disable --now s3lite-server.service >/dev/null 2>&1 || :
  systemctl daemon-reload >/dev/null 2>&1 || :
fi
exit 0

%postun
# Data under /var/lib/s3lite is intentionally kept on package removal.
if [ "$1" = 0 ]; then
  rmdir %{_sysconfdir}/s3lite/tls 2>/dev/null || :
  # Remove the shared s3lite-weed package if no node agent is installed on
  # this host (i.e. this host was the last consumer of weed).
  if [ ! -x /usr/bin/s3lite-agent ]; then
    timeout 20 rpm -e s3lite-weed >/dev/null 2>&1 || :
  fi
fi
exit 0

%files
%defattr(-,root,root)
%{_bindir}/s3lite-server
%{_unitdir}/s3lite-server.service
%{_unitdir}/s3lite-master.service
%dir %attr(0755,root,root) %{_sysconfdir}/s3lite
%config(noreplace) %attr(0640,root,root) %{_sysconfdir}/s3lite/server.env
# Ownership is fixed to s3lite:s3lite by the post scriptlet.
# Keep root:root here so rpm does not auto-generate the
# user(s3lite)/group(s3lite) dependencies that block first install.
%dir %attr(0750,root,root) %{_localstatedir}/lib/s3lite
%dir %attr(0750,root,root) %{_localstatedir}/lib/s3lite/master
%dir %attr(0750,root,root) %{_sysconfdir}/s3lite/tls
%doc README.md

%changelog
* Tue Oct 27 2026 S3Lite <dev@s3lite.dev> - 0.0.2-0
- Remove the shared s3lite-weed package on uninstall when no node agent is
  installed on the same host
- Switch the hard dependency from s3lite-agent to s3lite-weed: the control
  plane no longer pulls the node agent, only the shared SeaweedFS binaries
- Ship and auto-start s3lite-master.service: the SeaweedFS master is now
  installed and started with the control plane (data in /var/lib/s3lite/master)
- Pull in s3lite-weed as a hard dependency (formerly s3lite-agent): provides
  the SeaweedFS weed binary needed to run the master
* Mon Oct 26 2026 S3Lite <dev@s3lite.dev> - 0.0.1-0
- Unified spec: one package built for openSUSE 16 / Fedora 44 / Ubuntu 25 (OBS repositories)
- Go floor lowered to 1.26 (Fedora 44 ships Go 1.26.1)
