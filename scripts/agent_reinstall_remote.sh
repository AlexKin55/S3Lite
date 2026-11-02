#!/bin/bash
#
# agent_reinstall_remote.sh
#
# Helper script that runs ON the remote node (copied there by
# scripts/reinstall_agents.sh). It removes the s3lite-agent package (after
# backing up its configuration), refreshes repository metadata, installs the
# package fresh from the repository, restores the saved configuration and
# restarts the service.
#
# An embedded copy of this script lives at
# src/agent/cmd/agent/reinstall_agent.sh (the agent self-update feature:
# there it runs inside the root s3lite-agent-update.service helper unit);
# keep both files in sync — only the header comment differs.
#
# Emits machine-readable KEY=VALUE lines that the caller parses:
#   HOSTNAME, PM, WAS_INSTALLED, OLD_VERSION, BACKUP_DIR,
#   REMOVE_RC / INSTALL_RC, NOW_INSTALLED, NEW_VERSION,
#   RESTORED, CONFIGURED, ACTIVE, SERVICE_ERR, RESULT, DETAIL
#
# It also writes a self-update result marker to
# /var/lib/s3lite/agent/update-result (STATUS/VERSION/TS/DETAIL); the agent
# consumes it at startup to report the update outcome to the control plane.
#
# Usage: agent_reinstall_remote.sh [package-name]   (default: s3lite-agent)

PKG="${1:-s3lite-agent}"

# Self-update result marker (consumed by the agent, owned by it after chown).
result_dir=/var/lib/s3lite/agent
result_file="$result_dir/update-result"

# write_update_marker STATUS [DETAIL]
write_update_marker() {
    mkdir -p "$result_dir" 2>/dev/null
    {
        echo "STATUS=$1"
        echo "VERSION=${new_version:--}"
        echo "TS=$(date +%s)"
        [ -n "$2" ] && echo "DETAIL=$2"
    } > "$result_file" 2>/dev/null
    chown s3lite:s3lite "$result_file" 2>/dev/null || true
}

echo "HOSTNAME=$(hostname 2>/dev/null || echo '?')"

# ---------------------------------------------------------------------------
# Detect the package manager
# ---------------------------------------------------------------------------
PM=""
if command -v dnf >/dev/null 2>&1; then
    PM=dnf
elif command -v zypper >/dev/null 2>&1; then
    PM=zypper
elif command -v apt-get >/dev/null 2>&1; then
    PM=apt
fi
echo "PM=$PM"
if [ -z "$PM" ]; then
    write_update_marker failed "no supported package manager (dnf/zypper/apt-get)"
    echo "RESULT=FAIL"
    echo "DETAIL=no supported package manager (dnf/zypper/apt-get)"
    exit 0
fi

# ---------------------------------------------------------------------------
# Check whether the agent is already installed
# ---------------------------------------------------------------------------
was_installed=0
old_version="-"
if rpm -q "$PKG" >/dev/null 2>&1; then
    was_installed=1
    old_version="$(rpm -q --qf '%{VERSION}-%{RELEASE}' "$PKG" 2>/dev/null || echo '?')"
elif dpkg -s "$PKG" >/dev/null 2>&1; then
    was_installed=1
    old_version="$(dpkg-query -W -f='${Version}' "$PKG" 2>/dev/null || echo '?')"
fi
echo "WAS_INSTALLED=$was_installed"
echo "OLD_VERSION=$old_version"

# ---------------------------------------------------------------------------
# Back up the configuration before touching the package
#   - /etc/s3lite/agent.env            (%config(noreplace), but backed up anyway)
#   - /var/lib/s3lite/agent/agent.json (in /var/lib, rpm never touches it,
#                                       backed up anyway)
# ---------------------------------------------------------------------------
backup_dir="/tmp/s3lite-agent-backup-$(date +%s)"
mkdir -p "$backup_dir"
[ -f /etc/s3lite/agent.env ] && cp -a /etc/s3lite/agent.env "$backup_dir/" 2>/dev/null
[ -f /var/lib/s3lite/agent/agent.json ] && cp -a /var/lib/s3lite/agent/agent.json "$backup_dir/" 2>/dev/null
echo "BACKUP_DIR=$backup_dir"

# ---------------------------------------------------------------------------
# 1) Remove the old package (config already backed up above).
# 2) Rescan repositories / refresh the package-manager cache: a stale cache
#    hides freshly published builds (dnf/zypper would otherwise install an
#    old version from cache).
# 3) Install the package from the repository — whatever it currently
#    publishes: a newer build when there is one, the same version otherwise
#    (unlike `dnf reinstall`, which pins the installed NEVRA and would
#    silently redeploy an outdated agent).
# 4) Restore the saved configuration (agent.env / agent.json) if the
#    package replaced or removed it, and restart s3lite-agent.
#
# NOTE: between the remove and the install steps the node temporarily has no
# agent package; if the install fails the node is left without an agent and
# the caller reports FAIL.
# ---------------------------------------------------------------------------
if [ "$was_installed" = 1 ]; then
    case "$PM" in
        dnf)    dnf -y -q remove "$PKG" ;;
        zypper) zypper --non-interactive --quiet remove "$PKG" ;;
        apt)    DEBIAN_FRONTEND=noninteractive apt-get remove -y "$PKG" ;;
    esac
    echo "REMOVE_RC=$?"
fi

case "$PM" in
    dnf)    dnf -y -q --refresh makecache >/dev/null 2>&1 || true ;;
    zypper) zypper --non-interactive --quiet refresh >/dev/null 2>&1 || true ;;
    apt)    apt-get update -qq >/dev/null 2>&1 || true ;;
esac

case "$PM" in
    dnf)    dnf -y -q install "$PKG" ;;
    zypper) zypper --non-interactive --quiet install --force "$PKG" ;;
    apt)    DEBIAN_FRONTEND=noninteractive apt-get install -y "$PKG" ;;
esac
echo "INSTALL_RC=$?"

# ---------------------------------------------------------------------------
# Verify the package is actually present now
# ---------------------------------------------------------------------------
now_installed=0
new_version="-"
if rpm -q "$PKG" >/dev/null 2>&1; then
    now_installed=1
    new_version="$(rpm -q --qf '%{VERSION}-%{RELEASE}' "$PKG" 2>/dev/null || echo '?')"
elif dpkg -s "$PKG" >/dev/null 2>&1; then
    now_installed=1
    new_version="$(dpkg-query -W -f='${Version}' "$PKG" 2>/dev/null || echo '?')"
fi
echo "NOW_INSTALLED=$now_installed"
echo "NEW_VERSION=$new_version"
if [ "$now_installed" != 1 ]; then
    write_update_marker failed "package $PKG not present after install"
    echo "RESULT=FAIL"
    echo "DETAIL=package $PKG not present after install"
    exit 0
fi

# ---------------------------------------------------------------------------
# 4a) Restore the configuration if the package replaced or removed it.
# agent.env needs restoring not only when it is missing: a fresh install
# ships the template with CHANGE_ME placeholders, which would otherwise
# mask the saved (configured) backup.
# ---------------------------------------------------------------------------
restore_env=0
if [ -f "$backup_dir/agent.env" ]; then
    if [ ! -f /etc/s3lite/agent.env ] || \
       grep -q 'CHANGE_ME' /etc/s3lite/agent.env 2>/dev/null; then
        restore_env=1
    fi
fi
if [ "$restore_env" = 1 ]; then
    install -m 0640 "$backup_dir/agent.env" /etc/s3lite/agent.env
    echo "RESTORED=agent.env"
fi
if [ ! -f /var/lib/s3lite/agent/agent.json ] && [ -f "$backup_dir/agent.json" ]; then
    cp -a "$backup_dir/agent.json" /var/lib/s3lite/agent/agent.json
    chown s3lite:s3lite /var/lib/s3lite/agent/agent.json 2>/dev/null
    echo "RESTORED=agent.json"
fi

# ---------------------------------------------------------------------------
# Check whether the agent is configured.
# agent.env is considered configured if it exists and does not contain the
# CHANGE_ME placeholder from the unit template.
# ---------------------------------------------------------------------------
configured=0
if [ -f /etc/s3lite/agent.env ] && ! grep -q 'CHANGE_ME' /etc/s3lite/agent.env 2>/dev/null; then
    configured=1
fi
echo "CONFIGURED=$configured"

echo "RESULT=OK"

systemctl daemon-reload >/dev/null 2>&1
systemctl enable s3lite-agent.service >/dev/null 2>&1 || true

# ---------------------------------------------------------------------------
# Restart the service — SKIPPED when S3LITE_SKIP_RESTART=1 (the caller
# restarts it itself after reading the result).
# ---------------------------------------------------------------------------
if [ "${S3LITE_SKIP_RESTART:-0}" = "1" ]; then
    exit 0
fi

# Restart the agent (only if configured; otherwise leave it stopped).
# In the self-update flow this runs in the helper unit's own cgroup: the
# agent process is replaced, the script keeps running.
if [ "$configured" = 1 ]; then
    systemctl restart s3lite-agent.service 2>&1
    sleep 3
    if systemctl is-active --quiet s3lite-agent.service; then
        echo "ACTIVE=yes"
        write_update_marker done
    else
        service_err="$(systemctl status s3lite-agent.service --no-pager -n 5 2>&1 | tr '\n' '|' | cut -c1-300)"
        echo "ACTIVE=no"
        echo "SERVICE_ERR=$service_err"
        echo "RESULT=FAIL"
        echo "DETAIL=s3lite-agent service not active after restart"
        write_update_marker failed "s3lite-agent service not active after restart: $service_err"
    fi
else
    echo "ACTIVE=not-configured"
fi
