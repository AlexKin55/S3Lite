#!/bin/bash
#
# reinstall_agent.sh
#
# Embedded copy of scripts/agent_reinstall_remote.sh used by the agent's
# SELF-UPDATE feature (see selfupdate.go). Keep both files in sync.
#
# It reinstalls the s3lite-agent package, preserving the node's
# configuration, and restarts the service. The restart is safe here even for
# the running agent itself: in the packaged flow the script runs inside the
# root s3lite-agent-update.service unit (its own cgroup), so restarting
# s3lite-agent.service does not tear this script down.
#
# At the end it writes a machine-readable result marker to
# /var/lib/s3lite/agent/update-result (STATUS/VERSION/TS/DETAIL). The fresh
# agent process consumes it at startup and reports the update outcome to the
# control plane (the requesting agent is usually replaced by the restart).
#
# Emits machine-readable KEY=VALUE lines on stdout:
#   HOSTNAME, PM, WAS_INSTALLED, OLD_VERSION, BACKUP_DIR,
#   REINSTALL_RC / INSTALL_RC, NOW_INSTALLED, NEW_VERSION,
#   RESTORED, CONFIGURED, ACTIVE, SERVICE_ERR, RESULT, DETAIL
#
# Usage: reinstall_agent.sh [package-name]   (default: s3lite-agent)

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
# Reinstall (keeping config) or install fresh. NOTE: this is always a
# reinstall of the CURRENT package from the repo — the binaries are
# re-deployed even when the version is unchanged (same version, new build).
# ---------------------------------------------------------------------------
# Refresh repo metadata first: a stale package-manager cache hides freshly
# published builds (dnf/zypper happily reinstall an old version from cache),
# which is how nodes could end up "reinstalled" with an outdated agent.
case "$PM" in
    dnf)    dnf -y -q --refresh makecache >/dev/null 2>&1 || true ;;
    zypper) zypper --non-interactive --quiet refresh >/dev/null 2>&1 || true ;;
    apt)    apt-get update -qq >/dev/null 2>&1 || true ;;
esac

if [ "$was_installed" = 1 ]; then
    case "$PM" in
        dnf)    dnf -y -q --refresh reinstall "$PKG" ;;
        zypper) zypper --non-interactive --quiet install --force "$PKG" ;;
        apt)    DEBIAN_FRONTEND=noninteractive apt-get install --reinstall -y "$PKG" ;;
    esac
    echo "REINSTALL_RC=$?"
else
    case "$PM" in
        dnf)    dnf -y -q --refresh install "$PKG" ;;
        zypper) zypper --non-interactive --quiet install "$PKG" ;;
        apt)    DEBIAN_FRONTEND=noninteractive apt-get install -y "$PKG" ;;
    esac
    echo "INSTALL_RC=$?"
fi

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
    write_update_marker failed "package $PKG not present after install/reinstall"
    echo "RESULT=FAIL"
    echo "DETAIL=package $PKG not present after install/reinstall"
    exit 0
fi

# ---------------------------------------------------------------------------
# Restore the configuration if the package replaced it
# ---------------------------------------------------------------------------
if [ ! -f /etc/s3lite/agent.env ] && [ -f "$backup_dir/agent.env" ]; then
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
