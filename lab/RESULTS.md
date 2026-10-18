# Lab results

Numbers from the local lab. They show correctness; timings come from Docker
Desktop's virtual disk and VM and are **not** demo numbers (those come from the
VPS in week 4). All timings are wall clock as seen by the CLI, inside the
daemon pod, so they include the HTTP round trip over the unix socket.

## Run of 2026-10-18 (iteration 1)

| | |
|---|---|
| Host | MacBook, Apple M4 Pro, macOS 26.6.1 |
| Docker | Docker Desktop 4.39.0 (184744), engine 28.0.1, 12 CPUs, 7.65 GiB |
| Kernel | 6.10.14-linuxkit (aarch64) |
| btrfs-progs | v6.14 (Debian trixie, in `fern:dev`) |
| k3s | v1.36.5+k3s1 (`rancher/k3s:v1.36.5-k3s1`) |
| agent-sandbox | v1.0.7 (`sandbox.yaml` from the release) |
| CSI registrar | `csi-node-driver-registrar:v2.18.0` |
| Postgres | 17.11 (Debian) |
| gVisor | release 20261012, aarch64 |

Command: `lab/down.sh && lab/up.sh && lab/demo-slice.sh && lab/e0.sh && lab/e1.sh && lab/e4.sh && lab/gvisor.sh`, from a clean start. `lab/up.sh` takes about 30 s with warm build caches.

### Demo slice: PASS
`lab/up.sh && lab/demo-slice.sh && lab/down.sh` passed twice in a row from a clean start (runs 2 and 3; run 2 also ran `lab/gvisor.sh` between the demo and `down`, which installs gVisor from scratch). In run 1 the demo and E0/E1/E4 passed, but the chain stopped at `lab/gvisor.sh` (a `kubectl exec` right after the k3s restart got a 502, since fixed), so `down` ran separately.

| Step | Time |
|---|---|
| `fern snapshot demo s1` | 12.5 ms |
| `fern restore demo s1` (stop box, swap, stay asleep) | 1,221 ms |
| `lab/up.sh && lab/demo-slice.sh && lab/down.sh` | 49 s |

### E0: Sandbox + CSI inline volume + btrfs (runc): PASS

| Check | Result |
|---|---|
| Postgres runs on `live/` through the CSI inline volume | PASS |
| pod is owned by its `Sandbox` (`ownerReferences[0].kind`) | PASS |
| CSI plugin logs the pod's identity (namespace/name, UID, service account) | PASS |
| writes from the box appear in the pool, and the other way round | PASS |
| snapshot while Postgres writes; restore; Postgres recovers through its WAL | PASS (4 of 4 rows) |
| `live/` is unmounted after every sleep, mounted once while awake | PASS (10 of 10) |
| force-deleted pod is recreated by the controller; Postgres recovers | PASS |
| no subvolumes left after `fern rm` | PASS |

| Timing (ms) | n | p50 | p95 | max |
|---|---|---|---|---|
| first wake + `psql` query | 1 | 3,020 | | |
| sleep: `Running` → pod gone | 10 | 2,808 | 2,809 | 3,009 |
| wake: `Suspended` → `Ready` | 10 | 808 | 811 | 820 |
| force delete → new pod `Ready` | 1 | 703 | | |

### E1: snapshot cost, restore = snapshot, no leaks: PASS

| Data in `live/` | snapshot n | p50 | p95 | max | restore = snapshot (tree hash) |
|---|---|---|---|---|---|
| 0 MB | 20 | 8.9 ms | 9.7 ms | 10.1 ms | PASS |
| 500 MB | 20 | 8.4 ms | 9.7 ms | 11.1 ms | PASS |
| 5 GB | 20 | 9.2 ms | 10.6 ms | 13.7 ms | PASS |

- Data is 100 MB files of random bytes plus 200 small files, written straight into `live/` from the daemon container and `sync`ed first.
- `btrfs filesystem du` shows 0 bytes exclusive for both `live/` and the snapshot: all extents are shared.
- No subvolumes or files are left after `fern rm`.
- **Variance:** in an earlier run the same day (same lab, not clean), 5 GB snapshots took p50 42.8 ms / p95 83.7 ms. That matches spike 01's warning about Docker Desktop's virtual disk. Trust only VPS numbers.

### E4: the box holds no state: PASS

| Check | Result |
|---|---|
| root filesystem is read-only | PASS |
| a file in `/tmp` (an `emptyDir`) is gone after sleep and wake | PASS |
| a file in `live/` survives | PASS |
| a database row survives | PASS |
| a stray background process (`sleep 1000`) doesn't come back; only declared services do | PASS (after wake: `fern-init`, `postgres`, `postgres.sh`, `sh`) |
| force delete during writes → Postgres recovers; the seed ran once | PASS |

| Timing (ms) | n | p50 | p95 | max |
|---|---|---|---|---|
| wake: `Suspended` → services ready | 10 | 809 | 818 | 3,209 |

Wake latency is set by the readiness probe (`periodSeconds: 1`): about 0.8 s when the first probe after start passes, about 1.8 s when it misses.

### Bonus: gVisor: works

`lab/gvisor.sh` installs gVisor into the lab node, adds the `runsc` runtime through a containerd drop-in (`config-v3.toml.d/runsc.toml`), creates RuntimeClass `gvisor`, and patches one environment's `Sandbox` to use it.

| Check | Result |
|---|---|
| the box runs under gVisor (`dmesg`: "Starting gVisor...") | PASS |
| Postgres runs on the CSI-mounted btrfs subvolume | PASS |
| snapshot → drop table → restore → wake → table is back | PASS, 20 of 20 (also 5 of 5 in clean run 2) |

| Timing (ms) | n | p50 | p95 | max |
|---|---|---|---|---|
| gVisor wake after restore | 20 | 811 | 1,810 | 1,811 |

**Found on the way:** before a fix, about 1 in 7 restores under gVisor left Postgres unable to start. The cause was that the snapshot contained `postmaster.pid`. In the new box, another process (a readiness check) got the same PID, because gVisor hands out PIDs deterministically, so Postgres refused to start. The sample service now deletes the stale pid file on start. Any service that keeps a pid or lock file in `live/` has the same problem after a restore.
