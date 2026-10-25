# S3Lite packaging (deb / rpm)

Builds installable packages for the **control plane server** (`s3lite-server`)
and the **node agent** (`s3lite-agent`, includes the SeaweedFS `weed` binary
it depends on). Built with the platform-native tools, no Ruby/fpm needed:

| Format   | Tool        | Packages produced                      |
|----------|-------------|----------------------------------------|
| `.deb`   | `dpkg-deb`  | `s3lite-agent_<ver>_<arch>.deb`, `s3lite-server_<ver>_<arch>.deb` |
| `.rpm`   | `rpmbuild`  | `s3lite-agent-<ver>-1.<arch>.rpm`, `s3lite-server-<ver>-1.<arch>.rpm` |

## Requirements

- Go toolchain (unless `SKIP_BUILD=1`; powers `scripts/build_weed.sh` internally)
- deb: `dpkg-deb` (Debian/Ubuntu) or the fallback GNU `ar` + `tar` + `md5sum`
  (any Linux with binutils)
- rpm: `rpmbuild` (`dnf install rpm-build` on Fedora/RHEL, `zypper install rpm-build` on SUSE)

> Both formats can be built on one machine with a single command; when
> `dpkg-deb` is absent the deb is produced with the native ar+tar fallback.

## Usage

```bash
cd deploy/packaging

./build_packages.sh                 # agent + server, deb + rpm
./build_packages.sh -f deb          # only deb
./build_packages.sh -p agent        # only the agent package

PKG_VERSION=0.2.0 ./build_packages.sh   # explicit version (default: git tag / 0.1.0)
SKIP_BUILD=1 ./build_packages.sh        # reuse existing bin/ binaries
```

Artifacts land in `deploy/packaging/dist/packages/`.

## What is inside

**`s3lite-agent`** (per storage node):

- `/usr/bin/s3lite-agent`, `/usr/bin/weed`
- `/usr/lib/systemd/system/s3lite-agent.service`
- `/etc/s3lite/agent.env` (conffile: control plane address)
- data dir `/var/lib/s3lite/agent` (owned by the `s3lite` system user)

**`s3lite-server`** (control plane):

- `/usr/bin/s3lite-server`
- `/usr/lib/systemd/system/s3lite-server.service`
- `/etc/s3lite/server.env` (conffile: listen address, master, TLS, webhook)

Both packages create the shared `s3lite` system user, enable (but **do not
start**) the service — the address must be configured first.

## Install & configure

Debian/Ubuntu:

```bash
sudo apt install -y ./dist/packages/s3lite-server_*.deb
# or agent on each storage node:
sudo apt install -y ./dist/packages/s3lite-agent_*.deb

# configure and start:
sudo systemctl edit --full s3lite-agent   # or edit /etc/s3lite/agent.env
sudo systemctl start s3lite-agent
sudo systemctl status s3lite-agent
```

RHEL/Fedora:

```bash
sudo dnf install -y ./dist/packages/s3lite-server-*.rpm
sudo dnf install -y ./dist/packages/s3lite-agent-*.rpm
sudo systemctl start s3lite-agent
```

### Agent quick start

```ini
# /etc/s3lite/agent.env
CONTROL_PLANE=https://192.168.1.9:8099
WEED_BIN=/usr/bin/weed
```

Agent config, local data and `volume.log` live in `/var/lib/s3lite/agent`
(`agent.json` is generated on first start with a fresh node id).

### Server quick start

```ini
# /etc/s3lite/server.env
LISTEN=:8099
MASTER=192.168.1.9:9333
#TLS_CERT=/etc/s3lite/tls/server.crt
#TLS_KEY=/etc/s3lite/tls/server.key
#WEBHOOK=https://alerts.example.com/hook
```

TLS certificates go to `/etc/s3lite/tls/` (created by the package post-install).

## Uninstall

```bash
sudo apt remove s3lite-agent; sudo apt purge s3lite-agent   # purge deletes /var/lib/s3lite/agent
sudo dnf remove s3lite-agent
```

## Notes

- The agent package bundles `weed` (same build that ships test clusters via
  `scripts/build_weed.sh`) so a node can be provisioned from a single package.
- Packages are versioned from the nearest `git describe` tag (stripped of the
  leading `v`); fallback `0.1.0`, override with `PKG_VERSION`.
- `s3proxy` is not packaged yet — it fronts the S3 gateway and usually runs
  next to the control plane/filer.