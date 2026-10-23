#!/usr/bin/env bash
#
# Generates a self-signed CA and certificates for the S3Lite control plane.
#
# Output (into deploy/tls/out/):
#   ca.crt            CA certificate (distribute to agents/servers)
#   server.crt/key    control plane TLS certificate
#   agent.crt/key     example agent certificate (optional, mTLS)
#
# Usage:
#   ./generate.sh [IP,...] [DNS,...]
#   ./generate.sh 192.168.1.9 localhost
#
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OUT="${HERE}/out"
mkdir -p "${OUT}"

IPS="${1:-127.0.0.1,192.168.1.9}"
DNS="${2:-localhost}"

echo "==> Generating CA"
openssl genrsa -out "${OUT}/ca.key" 2048 2>/dev/null
openssl req -x509 -new -nodes -key "${OUT}/ca.key" -sha256 -days 3650 \
  -out "${OUT}/ca.crt" -subj "/CN=S3Lite CA" 2>/dev/null

echo "==> Generating server certificate (IPs: ${IPS}, DNS: ${DNS})"
SAN=""
IFS=',' read -r -a ips <<< "${IPS}"
for ip in "${ips[@]}"; do SAN="${SAN}IP:${ip},"; done
IFS=' ' read -r -a dns <<< "${DNS}"
for d in "${dns[@]}"; do SAN="${SAN}DNS:${d},"; done

openssl genrsa -out "${OUT}/server.key" 2048 2>/dev/null
openssl req -new -key "${OUT}/server.key" -out "${OUT}/server.csr" \
  -subj "/CN=s3lite-server" 2>/dev/null
printf "subjectAltName=%s" "${SAN%,}" > "${OUT}/server.ext"
openssl x509 -req -in "${OUT}/server.csr" -CA "${OUT}/ca.crt" -CAkey "${OUT}/ca.key" \
  -CAcreateserial -out "${OUT}/server.crt" -days 825 -sha256 \
  -extfile "${OUT}/server.ext" 2>/dev/null

echo "==> Generating agent certificate"
openssl genrsa -out "${OUT}/agent.key" 2048 2>/dev/null
openssl req -new -key "${OUT}/agent.key" -out "${OUT}/agent.csr" \
  -subj "/CN=s3lite-agent" 2>/dev/null
openssl x509 -req -in "${OUT}/agent.csr" -CA "${OUT}/ca.crt" -CAkey "${OUT}/ca.key" \
  -CAcreateserial -out "${OUT}/agent.crt" -days 825 -sha256 2>/dev/null

rm -f "${OUT}"/*.csr "${OUT}"/*.ext
echo "==> Done:"
ls -l "${OUT}"
echo
echo "Start the control plane with:"
echo "  bin/server -listen=:8099 -tlsCert=${OUT}/server.crt -tlsKey=${OUT}/server.key ..."
echo
echo "Run the agent with:"
echo "  bin/agent -server=https://<control-ip>:8099 -insecure ..."
