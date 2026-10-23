# S3Lite

A distributed S3-compatible storage built on top of [SeaweedFS](https://github.com/seaweedfs/seaweedfs):
ordinary PCs act as storage nodes (Go agent + `weed volume`), while a central control plane
manages the nodes, redundancy (erasure coding) and notifies the administrator about the risk of data loss.

## Key idea

- **A minimal SeaweedFS fork**: added arbitrary erasure-coding schemes `k+m`
  (flags `-dataShards` / `-parityShards` in `ec.encode`), e.g. the target **6+4** scheme
  (10 shards, survives the loss of 4 out of 10 nodes).
- **Custom Go code**: node agent (`src/agent`) + control plane (`src/server`):
  node registry, health monitoring, volume classification OK/DEGRADED/AT_RISK,
  repair scheduler, notifications.

## Layout

```
plans/                        # plans and presentations
src/
  agent/                      # PC node agent (in progress)
  server/                     # control plane (in progress)
  scripts/                    # weed build (build_weed.sh) and test-cluster startup (start_cluster.sh)
  thirdparty/seaweedfs/       # SeaweedFS fork (30GB 4.48) with the EC k+m patch
```

## Quick start (development cluster)

```bash
./src/scripts/build_weed.sh     # builds weed into bin/
./src/scripts/start_cluster.sh start   # master + 10 volume nodes + filer + s3
# EC 6+4:
printf 'lock\nec.encode -volumeId=1 -dataShards=6 -parityShards=4\nunlock\n' \
  | ./bin/weed shell -master=127.0.0.1:9333
```

## Status

- [x] Stage 1: test cluster (master + volume x10 + filer + s3), stock EC 10+4 verified
- [x] Stage 2: custom EC k+m patch, EC 6+4 verified (4 nodes offline → reads still work)
- [x] Stage 3: Go node agent (registration, volume supervision, heartbeat, auto-restart on crash)
- [x] Stage 4: control plane core (registry, health, redundancy OK/DEGRADED/AT_RISK, notifications, metrics)
- [x] Stage 5: integration tests (12/12 PASS: DEGRADED, AT_RISK, return, rebuild scenarios)
- [ ] Stage 6: production readiness

Details: [plans/action-plan.md](plans/action-plan.md), [plans/distributed-storage-plan.md](plans/distributed-storage-plan.md).
