# S3Lite OBS packaging

Sources for building the `s3lite-server` and `s3lite-agent` packages on
[Open Build Service](https://build.opensuse.org).

## Layout

| Path | Purpose |
|------|---------|
| `_service` | OBS source service: `tar_scm` downloads the S3Lite repo from git and packs it into `s3lite-<revision>.tar.gz` |
| `agent/s3lite-agent.spec` | Spec for the node agent package (bundles the `weed` binary) |
| `agent/s3lite-agent.service` | systemd unit shipped in the agent package |
| `server/s3lite-server.spec` | Spec for the control plane server package |
| `server/s3lite-server.service` | systemd unit shipped in the server package |
| `env/` | conffile templates installed to `/etc/s3lite/*.env` (shared by all distros) |
| `vendor/` | vendored Go dependency bundles (`server.tar.gz`, `seaweedfs.tar.gz`) unpacked in `%prep` — must be present in the OBS sources, they are consumed with `GOFLAGS=-mod=vendor` and no network access |

## One project, multiple repositories (rpm)

In OBS one package holds **one spec**; distro targets are *repositories* of
the project, not specs. Create a single OBS project (e.g. `s3lite`) with the
packages `s3lite-agent` and `s3lite-server`, and add one repository per
target:

* `openSUSE_16` — rpm
* `Fedora_44` — rpm

(Debian/Ubuntu .deb builds are on hold — the spec-based deb path needs extra
packaging work and is not enabled.)

The same spec is built against every repository. Per-distro differences live
in `%if` blocks driven by macros OBS injects per repository:

| Macro | Target | Effect |
|-------|--------|--------|
| `0%{?suse_version}` | openSUSE | `golang`, `Requires(pre): shadow`, `Group:`/`BuildRoot:` |
| `0%{?fedora}` | Fedora | `golang`, `Requires(pre): shadow-utils` (no `Group:`/`BuildRoot:`) |

All modules floor their `go` directive at 1.26 (`src/agent`, `src/server`,
`src/server/gen/weed/pb/*`, `src/thirdparty/seaweedfs`) so the packages build
on Fedora 44, which ships Go 1.26.1. Distros with a newer Go satisfy it too.

Adding another distro of the same family (e.g. `openSUSE_Tumbleweed`,
`Fedora_45`) needs no spec change — just add the repository.

## Versioning

- The single source of truth is the `VERSION` file committed per component:
  `src/server/VERSION` for `s3lite-server`, `src/agent/VERSION` for
  `s3lite-agent`. The spec `Version:` must match it — `%prep` aborts the build
  otherwise.
- `_service` checks out the `main` branch directly (there is no `release`
  branch). Bumping a version means committing the new value on `main` and
  pushing; OBS picks up the new state on its next build.

## Local build check

Optionally verify a spec locally (needs `rpmbuild` + a Go toolchain and the
vendor bundles in place):

```bash
osc build --clean  # or: rpmbuild -ba deploy/obs/agent/s3lite-agent.spec
```
