#!/bin/sh
# Post-remove hook shared by the s3lite packages
# (nfpm runs it as deb postrm / rpm %postun).
# deb passes $1=purge on full removal; rpm passes $1=0 on final erase.
set -e

purge=0
[ "$1" = "purge" ] && purge=1
[ "$1" = "0" ] && purge=1

if [ "${purge}" = "1" ]; then
  case "${NFPM_PACKAGE_NAME:-}" in
    s3lite-agent)
      rm -rf /var/lib/s3lite/agent
      rmdir /var/lib/s3lite 2>/dev/null || true
      rm -f /etc/s3lite/agent.env
      rmdir /etc/s3lite 2>/dev/null || true
      ;;
    s3lite-server)
      rm -rf /etc/s3lite
      ;;
  esac
  # Drop the shared user only when neither s3lite package remains installed.
  if ! { command -v s3lite-server >/dev/null 2>&1 || command -v s3lite-agent >/dev/null 2>&1; }; then
    if command -v userdel >/dev/null 2>&1; then
      userdel --system s3lite >/dev/null 2>&1 || true
    fi
  fi
fi

if command -v systemctl >/dev/null 2>&1; then
  systemctl daemon-reload >/dev/null 2>&1 || true
fi

exit 0