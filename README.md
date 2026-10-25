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

## Production deployment

### TLS (control plane)

Generate a self-signed CA and certificates, then run the control plane over HTTPS:

```bash
./deploy/tls/generate.sh 192.168.1.9 localhost
bin/server -listen=:8099 \
  -tlsCert=deploy/tls/out/server.crt -tlsKey=deploy/tls/out/server.key \
  -master=192.168.1.9:9333 -webhook=https://alerts.example.com/hook
```

Agents connect with `-server=https://<control-ip>:8099 -insecure`
(remove `-insecure` once the CA is trusted on the nodes).

### Installing from deb/rpm packages

Build native installable packages for the agent and the control plane
(no fpm — plain `dpkg-deb`/`rpmbuild`, details in
[`deploy/packaging/README.md`](deploy/packaging/README.md)):

```bash
./deploy/packaging/build_packages.sh        # deb + rpm for agent and server
# artifacts: deploy/packaging/dist/packages/
```

- **`s3lite-agent`** bundles the `weed` binary; set the control plane address
  in `/etc/s3lite/agent.env`, then `systemctl start s3lite-agent`.
- **`s3lite-server`**: configure `/etc/s3lite/server.env` and start the service.
- Both packages create the `s3lite` system user, install a systemd unit and
  enable (but do not auto-start) the service.

### Installing the agent as a service

- Linux (systemd): `sudo ./deploy/systemd/install_agent.sh <CONTROL_PLANE_ADDR> [PORT]`
- Windows (NSSM): `powershell -File deploy/windows/s3lite-agent.ps1 -ControlPlane 192.168.1.9`

### High availability

- **Control plane**: run two+ instances behind a load balancer with a shared
  PostgreSQL registry ([`deploy/postgres/schema.sql`](deploy/postgres/schema.sql));
  the node registry and placement history move to PostgreSQL (in-memory is the
  development default).
- **SeaweedFS**: run the master as a Raft trio (`weed master -peers=...`) and
  multiple filers/S3 gateways behind a proxy.

### Monitoring & operator UI

- **Browser dashboard**: open the control plane root (`http://<cp>:8099/`) — a
  self-contained page shows cluster stats, EC volumes with their
  `Power on to restore` column, registered nodes and a notification test button
  (auto-refresh every 5 s). Its JSON is at `/api/v1/dashboard`.
- Control plane exposes `/metrics` (Prometheus text format): registered nodes
  and EC volumes per state (ok/degraded/at_risk).
- Scrape config: [`deploy/prometheus/prometheus.yml`](deploy/prometheus/prometheus.yml)
  (control plane + weed master + volume servers).

### S3 recovery-hint proxy

`s3proxy` sits in front of the S3 gateway ([`scripts/start_cluster.sh`](scripts/start_cluster.sh)
runs it on :8333 with the gateway on :8332). When a download fails with 5xx,
it asks the control plane which PCs are offline and returns an S3 error like:

```
Storage nodes are offline. Please power on the following nodes to recover
the data: 192.168.1.9:8085
```

### CA storage and recovery

- EC 6+4 with one shard per node means data survives 4 nodes offline.
- If a node is lost permanently, use `ec.rebuild` (weed shell) to reconstruct
  missing shards on live nodes; AT_RISK notifications name the exact PCs to
  power back on.
- Store the EC shard layout (`ec_placement` table) so recommendations survive
  control-plane restarts.

## Testing

```bash
./scripts/build_all.sh                    # build weed, agent, server (+mc)
python3 src/tests/integration.py          # all cases A–E
python3 src/tests/integration.py -c A,B  # selected cases
python3 src/tests/integration_webhook.py # webhook delivery check (TODO-3)
```

## Status

- [x] Stage 1: test cluster (master + volume x10 + filer + s3), stock EC 10+4 verified
- [x] Stage 2: custom EC k+m patch, EC 6+4 verified (4 nodes offline → reads still work)
- [x] Stage 3: Go node agent (registration, volume supervision, heartbeat, auto-restart on crash)
- [x] Stage 4: control plane core (registry, health, redundancy OK/DEGRADED/AT_RISK, notifications, metrics)
- [x] Stage 5: integration tests (12/12 PASS: DEGRADED, AT_RISK, return, rebuild scenarios)
- [ ] Stage 6: production readiness

Details: [plans/action-plan.md](plans/action-plan.md), [plans/distributed-storage-plan.md](plans/distributed-storage-plan.md).
