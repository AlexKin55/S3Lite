#!/bin/sh
# Post-install hook shared by the s3lite-agent and s3lite-server packages
# (nfpm runs it as deb postinst / rpm %post).
# nfpm exports NFPM_PACKAGE_NAME; deb passes $1 (configure|...), rpm passes
# the number of installed instances.
set -e

# Create the shared service account (idempotent).
if ! getent passwd s3lite >/dev/null 2>&1; then
  if command -v useradd >/dev/null 2>&1; then
    useradd --system --no-create-home --home-dir /var/lib/s3lite/agent \
      --shell /usr/sbin/nologin s3lite
  else
    adduser --system --no-create-home --home /var/lib/s3lite/agent \
      --shell /usr/sbin/nologin s3lite
  fi
fi

case "${NFPM_PACKAGE_NAME:-}" in
  s3lite-agent)
    install -d -o s3lite -g s3lite -m 0750 /var/lib/s3lite/agent
    ;;
  s3lite-server)
    install -d -o s3lite -g s3lite -m 0750 /var/lib/s3lite
    install -d -m 0750 /etc/s3lite/tls
    ;;
esac

if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
  systemctl daemon-reload >/dev/null 2>&1 || true
  # Enabled but NOT started: the control plane address must be configured
  # first in the /etc/s3lite/*.env file.
  systemctl enable "${NFPM_PACKAGE_NAME}.service" >/dev/null 2>&1 || true
fi

exit 0