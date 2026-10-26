# S3Lite in Open Build Service (openSUSE 16)

OBS builds the `s3lite-server` and `s3lite-agent` RPM packages for openSUSE 16
(Leap 16) directly from the git **`release`** branch — no tags involved.

## Files in this directory

| File                             | Purpose                                                        |
|----------------------------------|----------------------------------------------------------------|
| [`_service`](_service)           | `tar_scm`: downloads the S3Lite repo as `s3lite.tar.gz` (same file for both packages) |
| `server/s3lite-server.spec`      | Control plane package                                          |
| `agent/s3lite-agent.spec`        | Node agent package (bundles the `weed` binary)                 |

The version source of truth is `src/server/VERSION` / `src/agent/VERSION`
(today on `main`, later on `release`). Every spec has `Version:` and verifies
in `%prep` that it matches its `VERSION` file — a mismatch fails the build.

## Current phase: building from main (debug)

`_service` points `revision` at **`main`** while the OBS packages are being
debugged. Once the build is green:

1. slice `main` -> `release` (`git checkout release && git merge main`,
   push),
2. change `revision` in `_service` to `release`,
3. re-run services on both OBS packages.

## Repository layout in OBS

- Project, e.g. `home:<user>:S3Lite`, with a repository for
  `openSUSE_Leap_16.0` (`Path: openSUSE:Leap:16.0 / standard`).
- Two packages: `s3lite-server` and `s3lite-agent`. Both build from the same
  `s3lite.tar.gz` snapshot; each has its own spec and its own version.

## Uploading a package (osc)

```bash
# go to your OBS working copy, create the package dirs
osc mkpac s3lite-server s3lite-agent
cd s3lite-server
cp ../../deploy/obs/_service .
cp ../../deploy/obs/server/s3lite-server.spec .
osc service run          # runs tar_scm locally (needs obs-service-tar-scm)
                         # or trigger services from the web UI instead
osc commit
cd ../s3lite-agent
cp ../../deploy/obs/_service .
cp ../../deploy/obs/agent/s3lite-agent.spec .
osc service run
osc commit
```

After that, re-running the services (web UI "Trigger services" / `osc service
run` + commit) picks up the newest state of the `release` branch.

## Build requirements

- **Go >= 1.27**: the modules in `src/server`, `src/agent` declare
  `go 1.27.0`. If the Leap 16 base repo does not ship it yet, add a path to a
  project with a newer `golang` (e.g. `devel:languages:go`) in the repository
  definition, or enable network (below) and let `GOTOOLCHAIN=auto`
  (Go default) fetch the right toolchain.
- **Network for Go modules**: `go build` downloads module dependencies
  (server/weed) at build time. Allowsnet the build network access in the
  repository meta, for example:

  ```xml
  <repository name="openSUSE_Leap_16.0">
    <path project="openSUSE:Leap:16.0" repository="standard"/>
    <allownetwork>true</allownetwork>
  </repository>
  ```

  (The exact knob depends on the OBS instance. Alternatives: vendor the
  modules into the repo, or build locally with a populated Go module cache
  and push the binaries.)

## Cutting a release (slice main -> release)

1. Bump the version(s) in `src/server/VERSION` and/or `src/agent/VERSION`
   on `main`.
2. Keep `Version:` in the matching spec in sync.
3. Slice into `release` (fast-forward / merge from `main`) and push.
4. Trigger services on both OBS packages — the next build uses the new slice.
