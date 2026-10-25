#!/bin/sh
# Pre-remove hook shared by the s3lite packages
# (nfpm runs it as deb prerm / rpm %preun).
# deb passes $1=remove|purge|upgrade; rpm passes $1=0 on final erase.
set -e

if [ "$1" = "remove" ] || [ "$1" = "purge" ] || [ "$1" = "0" ]; then
  if command -v systemctl >/dev/null 2>&1; then
    systemctl disable --now "${NFPM_PACKAGE_NAME}.service" >/dev/null 2>&1 || true
    systemctl daemon-reload >/dev/null 2>&1 || true
  fi
fi

exit 0