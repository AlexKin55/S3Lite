# s3lite-agent RPM spec.
# The build script (build_packages.sh) stages the filesystem tree and passes
# the version via --define (pkgversion, pkgrelease).
%{!?pkgversion:%global pkgversion 0.1.0}
%{!?pkgrelease:%global pkgrelease 1}
%global _unitdir /usr/lib/systemd/system

Name:           s3lite-agent
Version:        %{pkgversion}
Release:        %{pkgrelease}
Summary:        S3Lite storage node agent
License:        Apache-2.0
URL:            https://example.invalid/s3lite
Requires(post):   systemd
Requires(preun):  systemd
Requires(postun): systemd

%description
The S3Lite node agent runs on every PC that contributes storage to the
S3Lite cluster. It registers the node with the control plane, starts and
supervises a local SeaweedFS volume server (the weed binary is bundled)
and reports health/disk metrics to the control plane.

%install
rm -rf %{buildroot}
mkdir -p %{buildroot}
cp -a %{_sourcedir}/agent/. %{buildroot}/

%files
%defattr(-,root,root,-)
%config(noreplace) %{_sysconfdir}/s3lite/agent.env
%{_bindir}/s3lite-agent
%{_bindir}/weed
%{_unitdir}/s3lite-agent.service
%{_defaultdocdir}/s3lite-agent/README.md

%post
getent passwd s3lite >/dev/null 2>&1 || useradd --system --no-create-home \
  --home-dir /var/lib/s3lite/agent --shell /usr/sbin/nologin s3lite \
  >/dev/null 2>&1 || :
install -d -o s3lite -g s3lite -m 0750 /var/lib/s3lite/agent
systemctl daemon-reload >/dev/null 2>&1 || :
# Enabled but NOT started: the control plane address must be configured first
# in /etc/s3lite/agent.env.
systemctl enable s3lite-agent.service >/dev/null 2>&1 || :

%preun
if [ "$1" -eq 0 ]; then
  systemctl disable --now s3lite-agent.service >/dev/null 2>&1 || :
  systemctl daemon-reload >/dev/null 2>&1 || :
fi

%postun
if [ "$1" -ge 1 ]; then
  systemctl daemon-reload >/dev/null 2>&1 || :
fi