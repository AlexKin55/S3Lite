#
# spec file for package s3lite-agent
#
# S3Lite node agent, built by OBS from the git "release" branch.
#
# Source handling:
#   _service (tar_scm) downloads the release branch and produces s3lite.tar.gz
#   -> Source0 below. The archive unpacks into the s3lite/ directory.
#
# Versioning:
#   The single source of truth for the package version is
#   src/agent/VERSION committed on the release branch. Version: in this
#   spec must match it (the check in %prep fails the build otherwise).
#
Name:           s3lite-agent
Version:        0.0.1
Release:        0
Summary:        S3Lite distributed S3 storage - node agent
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
The S3Lite node agent runs on every PC that contributes storage to the
S3Lite cluster. It registers the node with the control plane, starts and
supervises a local SeaweedFS volume server (the weed binary is bundled)
and reports health/disk metrics to the control plane.

Set the control plane address in /etc/s3lite/agent.env, then start the
service:

    systemctl start s3lite-agent

%prep
%setup -q -n s3lite
# The VERSION file on the release branch is the version source of truth.
test "$(cat src/agent/VERSION)" = "%{version}" || {
  echo "ERROR: src/agent/VERSION ($(cat src/agent/VERSION)) does not match spec Version (%{version})" >&2
  exit 1
}

%build
export GO111MODULE=on
export GOPATH="%{_builddir}/gopath"
export GOCACHE="%{_builddir}/gocache"
(cd src/agent && go build -trimpath -ldflags "-s -w" -o s3lite-agent ./cmd/agent)
(cd src/thirdparty/seaweedfs/weed && go build -trimpath -ldflags "-s -w" -o weed .)

%install
mkdir -p %{buildroot}%{_bindir}
mkdir -p %{buildroot}%{_unitdir}
mkdir -p %{buildroot}%{_sysconfdir}/s3lite
mkdir -p %{buildroot}%{_localstatedir}/lib/s3lite/agent
install -m 0755 src/agent/s3lite-agent %{buildroot}%{_bindir}/s3lite-agent
install -m 0755 src/thirdparty/seaweedfs/weed/weed %{buildroot}%{_bindir}/weed
install -m 0644 deploy/packaging/systemd/s3lite-agent.service %{buildroot}%{_unitdir}/s3lite-agent.service
install -m 0640 deploy/packaging/env/s3lite-agent.env %{buildroot}%{_sysconfdir}/s3lite/agent.env
install -m 0644 README.md %{buildroot}%{_defaultdocdir}/s3lite-agent/README.md

%pre
# Shared service account (also used by s3lite-server).
getent passwd s3lite >/dev/null 2>&1 || \
  useradd --system --no-create-home --home-dir /var/lib/s3lite/agent \
          --shell /usr/sbin/nologin s3lite 2>/dev/null || :
exit 0

%post
install -d -m 0750 -o s3lite -g s3lite %{_localstatedir}/lib/s3lite/agent 2>/dev/null || :
if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
  systemctl daemon-reload >/dev/null 2>&1 || :
  # Enabled but NOT started: /etc/s3lite/agent.env must be configured first.
  systemctl enable s3lite-agent.service >/dev/null 2>&1 || :
fi
exit 0

%preun
if [ "$1" = 0 ]; then
  systemctl disable --now s3lite-agent.service >/dev/null 2>&1 || :
  systemctl daemon-reload >/dev/null 2>&1 || :
fi
exit 0

%postun
# Data under /var/lib/s3lite is intentionally kept on package removal.
exit 0

%files
%defattr(-,root,root)
%{_bindir}/s3lite-agent
%{_bindir}/weed
%{_unitdir}/s3lite-agent.service
%config(noreplace) %attr(0640,root,root) %{_sysconfdir}/s3lite/agent.env
%dir %attr(0750,s3lite,s3lite) %{_localstatedir}/lib/s3lite
%dir %attr(0750,s3lite,s3lite) %{_localstatedir}/lib/s3lite/agent
%doc README.md

%changelog
* Mon Oct 26 2026 S3Lite <dev@s3lite.dev> - 0.0.1-0
- Initial OBS package: sources from the release git branch (VERSION file driven)
