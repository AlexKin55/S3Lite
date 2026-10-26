# S3Lite packaging (deb / rpm) with nfpm

Builds installable packages for the **control plane server** (`s3lite-server`)
and the **node agent** (`s3lite-agent`, includes the SeaweedFS `weed` binary
it depends on) using [nfpm](https://nfpm.goreleaser.com) — one YAML
description, both formats:

| Format | Tool  | Package produced                          |
|--------|-------|-------------------------------------------|
| `.deb` | nfpm  | `s3lite-agent_<ver>_<arch>.deb`, `s3lite-server_<ver>_<arch>.deb` |
| `.rpm` | nfpm  | `s3lite-agent-<ver>-<rel>.<arch>.rpm`, `s3lite-server-<ver>-<rel>.<arch>.rpm` |

## Requirements

- [nfpm v2](https://nfpm.goreleaser.com/install):
  `go install github.com/goreleaser/nfpm/v2/cmd/nfpm@latest`
  (or download the binary from the [releases](https://github.com/goreleaser/nfpm/releases) page)
- Go toolchain to build the binaries (skip with `SKIP_BUILD=1` if `bin/` is fresh)

No dpkg/rpm tooling is needed for the packaging itself.

## Usage

```bash
cd deploy/packaging

./build_packages.sh                  # agent + server, deb + rpm
./build_packages.sh -f deb           # only deb
./build_packages.sh -p agent         # only the agent package
./build_packages.sh -p server -f rpm # one package, one format

PKG_VERSION=0.2.0 ./build_packages.sh    # explicit version (default: git tag / 0.1.0)
PKG_RELEASE=2 ./build_packages.sh        # rpm release number (default: 1)
SKIP_BUILD=1 ./build_packages.sh         # reuse existing bin/ binaries
```

Artifacts land in `deploy/packaging/dist/packages/`.

## How it is wired

- [`nfpm-agent.yaml`](nfpm-agent.yaml) / [`nfpm-server.yaml`](nfpm-server.yaml) —
  package metadata, file lists, conffile `/etc/s3lite/*.env`, systemd units.
  `version`/`release`/`arch` come from the environment (`PKG_VERSION`,
  `PKG_RELEASE`, `PKG_ARCH`).
- [`scripts/`](scripts/) — one shared postinstall/preremove/postremove trio
  used for both deb and rpm (nfpm exports `NFPM_PACKAGE_NAME`; deb and rpm
  pass different `$1` values, both handled).
- [`systemd/`](systemd/) & [`env/`](env/) — service units and configuration
  templates shipped with the packages.

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
sudo apt install -y ./dist/packages/s3lite-agent_*.deb   # on each storage node
sudo systemctl start s3lite-agent
```

RHEL/Fedora/openSUSE:

```bash
sudo dnf install -y ./dist/packages/s3lite-server-*.rpm
sudo rpm -ivh ./dist/packages/s3lite-agent-*.rpm
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

TLS certificates go to `/etc/s3lite/tls/` (directory created by postinstall).

## Uninstall

```bash
sudo apt remove s3lite-agent; sudo apt purge s3lite-agent   # purge deletes /var/lib/s3lite/agent
sudo dnf remove s3lite-agent
```

## Release workflow (OBS)

S3Lite uses a **release branch** model — OBS
([build.opensuse.org](https://build.opensuse.org)) pulls the sources from the
`release` branch instead of tags:

1. Development happens on `main`. Versions of the components live in
   `src/server/VERSION` and `src/agent/VERSION` — the single source of truth,
   so the server and the agent can version independently.
2. To cut a release, make a **slice** of `main` into `release`:

   ```bash
   git checkout release
   git merge main
   git push origin release
   ```

3. OBS `tar_scm` service checks out `revision=release`; each package reads its
   version from the `VERSION` file inside the archive:
   - `s3lite-server` → [`src/server/VERSION`](../../src/server/VERSION)
   - `s3lite-agent` → [`src/agent/VERSION`](../../src/agent/VERSION)

Bumping a component version means committing the new value on `main` and
slicing it into `release` — OBS picks up the new state on its next build.

## Notes

- The agent package bundles `weed` (the same build that ships test clusters
  via `scripts/build_weed.sh`) so a node can be provisioned from one package.
- Packages are versioned from the nearest `git describe` tag (stripped of the
  leading `v`); fallback `0.1.0`, override with `PKG_VERSION`. The package
  version is sanitized for both formats (no `-`, leading digit) so the same
  value works in deb and rpm.
- `s3proxy` is not packaged yet — it fronts the S3 gateway and usually runs
  next to the control plane/filer.