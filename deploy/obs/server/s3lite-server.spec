#
# spec file for package s3lite-server
#
# S3Lite control plane server, built by OBS from the git "release" branch.
#
# Source handling:
#   _service (tar_scm) downloads the release branch and produces s3lite.tar.gz
#   -> Source0 below. The archive unpacks into the s3lite/ directory.
#
# Versioning:
#   The single source of truth for the package version is
#   src/server/VERSION committed on the release branch. Version: in this
#   spec must match it (the check in %prep fails the build otherwise).
#
Name:           s3lite-server
Version:        0.0.1
Release:        0
Summary:        S3Lite distributed S3 storage - control plane server
License:        Apache-2.0
Group:          System/Daemons
URL:            https://github.com/AlexKin55/S3Lite
Source0:        s3lite.tar.gz
BuildRequires:  golang >= 1.27
Requires(pre):  shadow
Requires(post): systemd
Requires(preun): systemd
Requires(postun): systemd
BuildRoot:      %{_tmppath}/%{name}-%{version}-build

%description
Central control plane of the S3Lite distributed S3 storage: node registry,
health monitoring, volume classification (OK/DEGRADED/AT_RISK), redundancy
evaluation and notifications, plus a browser dashboard and Prometheus
/metrics endpoint.

Configure /etc/s3lite/server.env, then start the service:

    systemctl start s3lite-server

%prep
%setup -q -n s3lite
# The VERSION file on the release branch is the version source of truth.
test "$(cat src/server/VERSION)" = "%{version}" || {
  echo "ERROR: src/server/VERSION ($(cat src/server/VERSION)) does not match spec Version (%{version})" >&2
  exit 1
}

%build
export GO111MODULE=on
export GOPATH="%{_builddir}/gopath"
export GOCACHE="%{_builddir}/gocache"
(cd src/server && go build -trimpath -ldflags "-s -w" -o s3lite-server ./cmd/server)

%install
mkdir -p %{buildroot}%{_bindir}
mkdir -p %{buildroot}%{_unitdir}
mkdir -p %{buildroot}%{_sysconfdir}/s3lite
mkdir -p %{buildroot}%{_sysconfdir}/s3lite/tls
mkdir -p %{buildroot}%{_localstatedir}/lib/s3lite
install -m 0755 src/server/s3lite-server %{buildroot}%{_bindir}/s3lite-server
install -m 0644 deploy/packaging/systemd/s3lite-server.service %{buildroot}%{_unitdir}/s3lite-server.service
install -m 0640 deploy/packaging/env/s3lite-server.env %{buildroot}%{_sysconfdir}/s3lite/server.env
install -m 0644 README.md %{buildroot}%{_defaultdocdir}/s3lite-server/README.md

%pre
# Shared service account (also used by s3lite-agent).
getent passwd s3lite >/dev/null 2>&1 || \
  useradd --system --no-create-home --home-dir /var/lib/s3lite/agent \
          --shell /usr/sbin/nologin s3lite 2>/dev/null || :
exit 0

%post
install -d -m 0750 -o s3lite -g s3lite %{_localstatedir}/lib/s3lite
install -d -m 0750 %{_sysconfdir}/s3lite/tls
if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
  systemctl daemon-reload >/dev/null 2>&1 || :
  # Enabled but NOT started: /etc/s3lite/server.env must be configured first.
  systemctl enable s3lite-server.service >/dev/null 2>&1 || :
fi
exit 0

%preun
if [ "$1" = 0 ]; then
  systemctl disable --now s3lite-server.service >/dev/null 2>&1 || :
  systemctl daemon-reload >/dev/null 2>&1 || :
fi
exit 0

%postun
# Data under /var/lib/s3lite is intentionally kept on package removal.
if [ "$1" = 0 ]; then
  rmdir %{_sysconfdir}/s3lite/tls 2>/dev/null || :
fi
exit 0

%files
%defattr(-,root,root)
%{_bindir}/s3lite-server
%{_unitdir}/s3lite-server.service
%config(noreplace) %attr(0640,root,root) %{_sysconfdir}/s3lite/server.env
%dir %attr(0750,s3lite,s3lite) %{_localstatedir}/lib/s3lite
%dir %attr(0750,root,root) %{_sysconfdir}/s3lite/tls
%doc README.md

%changelog
* Mon Oct 26 2026 S3Lite <dev@s3lite.dev> - 0.0.1-0
- Initial OBS package: sources from the release git branch (VERSION file driven)
