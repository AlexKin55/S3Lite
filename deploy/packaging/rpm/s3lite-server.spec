# s3lite-server RPM spec.
# The build script (build_packages.sh) stages the filesystem tree and passes
# the version via --define (pkgversion, pkgrelease).
%{!?pkgversion:%global pkgversion 0.1.0}
%{!?pkgrelease:%global pkgrelease 1}
%global _unitdir /usr/lib/systemd/system

Name:           s3lite-server
Version:        %{pkgversion}
Release:        %{pkgrelease}
Summary:        S3Lite control plane server
License:        Apache-2.0
URL:            https://example.invalid/s3lite
Requires(post):   systemd
Requires(preun):  systemd
Requires(postun): systemd

%description
Central control plane of the S3Lite distributed S3 storage: node registry,
health monitoring, volume classification (OK/DEGRADED/AT_RISK), redundancy
evaluation and notifications, plus a browser dashboard and Prometheus
/metrics endpoint.

%install
rm -rf %{buildroot}
mkdir -p %{buildroot}
cp -a %{_sourcedir}/server/. %{buildroot}/

%files
%defattr(-,root,root,-)
%config(noreplace) %{_sysconfdir}/s3lite/server.env
%{_bindir}/s3lite-server
%{_unitdir}/s3lite-server.service
%{_defaultdocdir}/s3lite-server/README.md

%post
getent passwd s3lite >/dev/null 2>&1 || useradd --system --no-create-home \
  --home-dir /var/lib/s3lite/agent --shell /usr/sbin/nologin s3lite \
  >/dev/null 2>&1 || :
install -d -o s3lite -g s3lite -m 0750 /var/lib/s3lite
install -d -m 0750 /etc/s3lite/tls
systemctl daemon-reload >/dev/null 2>&1 || :
# Enabled but NOT started: the control plane must be configured first in
# /etc/s3lite/server.env.
systemctl enable s3lite-server.service >/dev/null 2>&1 || :

%preun
if [ "$1" -eq 0 ]; then
  systemctl disable --now s3lite-server.service >/dev/null 2>&1 || :
  systemctl daemon-reload >/dev/null 2>&1 || :
fi

%postun
if [ "$1" -ge 1 ]; then
  systemctl daemon-reload >/dev/null 2>&1 || :
fi