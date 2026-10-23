#!/usr/bin/env bash
#
# Installs the S3Lite agent as a systemd service on a PC node (Linux).
#
# Usage:
#   sudo ./install_agent.sh <CONTROL_PLANE_ADDR> [PORT]
#   sudo ./install_agent.sh 192.168.1.9 8080
#
set -euo pipefail

CONTROL_ADDR="${1:?control plane address required, e.g. 192.168.1.9}"
PORT="${2:-8080}"

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
INSTALL_DIR="/usr/local/bin"
DATA_DIR="/var/lib/s3lite/agent"
SERVICE="s3lite-agent"

echo "==> Installing binary"
cp "${ROOT}/bin/agent" "${INSTALL_DIR}/s3lite-agent"
cp "${ROOT}/bin/weed" "${INSTALL_DIR}/weed"
chmod +x "${INSTALL_DIR}/s3lite-agent" "${INSTALL_DIR}/weed"

echo "==> Creating data dir"
mkdir -p "${DATA_DIR}"

echo "==> Rendering systemd unit"
sed -e "s|<RUNAS_USER>|${SUDO_USER:-root}|g" \
    -e "s|<CONTROL_PLANE_ADDR>|${CONTROL_ADDR}|g" \
    -e "s|-dir=/var/lib/s3lite/agent.*|-dir=${DATA_DIR} -server=https://${CONTROL_ADDR}:8099 -port=${PORT} -weed=${INSTALL_DIR}/weed -insecure|" \
    "${ROOT}/deploy/systemd/${SERVICE}.service" > "/etc/systemd/system/${SERVICE}.service"

echo "==> Enabling and starting the service"
systemctl daemon-reload
systemctl enable --now "${SERVICE}"
systemctl status --no-pager "${SERVICE}" | head -12

echo
echo "Done. Logs: journalctl -u ${SERVICE} -f"
