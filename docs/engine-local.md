# The engine: experiments and dev loop

**Status:** plan, revised 2026-10-17 to match the Kubernetes-first decision.
- [architecture.md](architecture.md) says what we build.
- [roadmap.md](roadmap.md) gives the week-by-week plan; [design.md §21](design.md) gives the demo.

The engine is everything Fern runs on a node:
- the **store** (btrfs snapshots)
- the **actors** (one per environment)
- the **operation log**
- the **thin operator**
- the **driver** (agent-sandbox `Sandbox` per environment)
- the **CSI driver**
- **`fern-init`** (inside boxes)
- the **setup runner** (golden snapshots)
- the **front door** (wake on touch)
- the **T3 hook**

**Where it runs:** **locally first, then remote** ([roadmap.md](roadmap.md)). Weeks 0–3 use a local lab: k3s inside a privileged Docker container on the Mac. From week 4, a remote Linux machine with k3s. Both are Kubernetes, so there is one code path. Nothing gets installed on the Mac itself.

---

## 1. The dev machines

### 1.0 The local lab (weeks 0–3)
- **What:** Docker Desktop (you start it), one privileged `rancher/k3s` container, a btrfs image file loop-mounted inside it at `/var/lib/fern`. A second k3s container joins as node 2 when needed.
- **Go:** builds and unit tests run in a `golang` container with the repo mounted.
- **Known to work** (earlier spikes on this Mac): btrfs snapshots and swaps, Postgres recovery, git worktrees in subvolumes, k3s pods restarting with their data (kernel 6.10 linuxkit, btrfs-progs 6.6).
- **gVisor** (`runsc`) works inside Docker Desktop's k3s: `lab/up.sh` installs it, and every box runs under it, as on the VPS.
- **Use it for correctness, not speed:** the virtual disk makes some snapshots slow (spike 01: 1 in 10 took 200 ms–1 s). Only VPS numbers go into the demo.
- **Cleanup:** delete the lab containers and the image file; nothing else is touched.

```sh
# on the Mac (sketch)
docker run -d --name fern-lab --privileged -v fern-lab:/var/lib/fern-img rancher/k3s server
docker exec fern-lab sh -c 'truncate -s 40G /var/lib/fern-img/pool.img && mkfs.btrfs -q /var/lib/fern-img/pool.img \
  && mkdir -p /var/lib/fern && mount -o loop /var/lib/fern-img/pool.img /var/lib/fern'
docker exec fern-lab kubectl get nodes
```

### 1.1 The remote machine (from week 4)
- **Node 1:** 8 GB RAM, 2–4 vCPU, NVMe, a recent kernel (6.7+ for btrfs simple quotas; 1.36+ Kubernetes for user namespaces). For example, Netcup VPS 1000 G12, or a Hetzner CX/CAX if available. Check prices in the console first.
- **Node 2** (from week 6): 4 GB, joined with `fern join` / `k3s agent`.
- **Access:** Tailscale (or plain SSH) from the Mac. The T3 app pairs via *Add environment → SSH*.

### 1.2 Bootstrap on the VPS (week 4)
`lab/vps.sh HOST`, from the Mac, as root over SSH. Every step skips what is done, so running it again updates Fern:
- **The pool:** a sparse 50 GB image file on the server's own disk, btrfs, loop-mounted `noatime` at `/var/lib/fern` from fstab; k3s waits for the mount. A network volume would be slower.
- **gVisor:** a pinned release, checked against its SHA-512, registered with k3s's containerd before k3s first starts; RuntimeClass `gvisor`.
- **k3s:** a pinned version with `deploy/k3s-config.yaml`; then a pinned agent-sandbox release.
- **Fern and T3:** `lab/build.sh t3` builds the images on the Mac and streams them to the node's containerd, so the VPS needs the Mac's CPU architecture (ARM for an M-series Mac).

With `FERN_HOST=HOST`, the lab's scripts (`check.sh`, `fern`, `claude-login.sh`, `t3-pair.sh`, `build.sh`) work on the VPS. Only SSH may be reachable from outside: a firewall in front of the server must close the Kubernetes API and T3's NodePort.

### 1.3 Fast inner loop
- **Unit tests** run anywhere (`go test -race ./...`), using the fake driver. That works on the Mac inside a Go container, or on the VM.
- **Integration tests** run against k3s, in the local lab first and on the VM before each tag: `lab/build.sh` builds the images and restarts the pods, then `lab/check.sh` runs the checks by section (E0 is `box`, E1 is `disk`, E4 is `box`): by default the quick ones (slice, box, stream, restore, paths, golden, t3; 36 s in the lab, 103 s on the VPS from the Mac), with `all` every one. Sections run four at a time, each with an environment of its own, except those that restart the daemon or the node or load it (idle, daemon, node, many), which run alone after; `JOBS=1` runs them one by one, for timings undisturbed by the others. The scripts run the fern CLI on the node, against the daemon's socket, rather than through `kubectl exec` (70 ms a command instead of 200 in the lab).
- **Rules:**
  - Everything Fern creates is labelled `app.kubernetes.io/part-of=fern`, so cleanup is `helm uninstall fern` plus deleting the pool.
  - No credentials in images or snapshots. Agent logins are done by you on the VM and mounted from outside `live/`.

---

## 2. What we build, as Go packages

| Package | Job | Tested by |
|---|---|---|
| `store` | btrfs pool, subvolumes, snapshot / fork / restore / delete, retention, index (SQLite); crash safety: every multi-step change leaves the disk in a state `Reconcile` finishes or clears (no separate op log) | E1, E2, E6 |
| `actor` | one goroutine per environment; mutations one at a time; execs tracked; idle sleep | E5, E8 |
| `operator` | kubelet-style reconcile of this node's `Environment` resources; status; owner references | E6, E13 |
| `driver` | one agent-sandbox `Sandbox` per environment (`podTemplate` with RuntimeClass, limits, CSI volume; `operatingMode` for sleep/wake); a fake for unit tests | E0, E4 |
| `csi` | CSI node plugin: mount an environment's `live/`, **only for its own pod** | E0, E14 |
| `fern-init` | the main process of boxes: services, exec/PTY with reattach, activity | E4, E8 |
| `setup` | golden from `.agents/setup`; golden keys | E3 |
| `wake` | front door: socket, previews, one wake per burst, holding requests | E7, E8 |
| `identity` | post-fork hook: new ID, secrets, ports | E9 |
| `cmd/fern` | daemon + CLI | all |

---

## 3. A sample repo to test against

- **Node app** (an Express or Next.js API) with a few hundred MB of `node_modules`.
- **Postgres** with a migration and a seed.
- **Repo files:**
  - `.agents/setup`: `pnpm install --frozen-lockfile && createdb app && pnpm db:migrate && pnpm db:seed`
  - `.agents/resume`: a no-op, or exporting test credentials
  - `services.yaml`: `postgres` (`pg_ctl start -D .fern/pg`), `web` (`pnpm dev`, respects `PORT`)
- **A second, heavier repo** (~1–2 GB `node_modules`) for timing.

---

## 4. Experiments

Each one: **shows**, **how**, **pass**. Numbers are reported as p50/p95, with node specs.

### E0. Week-0 spike: gVisor + btrfs + CSI inline volume in a `Sandbox` (the riskiest piece)
- **Shows:** a gVisor agent-sandbox `Sandbox` can use a btrfs subvolume served by our CSI driver.
- **How:**
  1. A minimal CSI node plugin that bind-mounts `/var/lib/fern/envs/x/live` on `NodePublishVolume`.
  2. A `Sandbox` whose `podTemplate` has `runtimeClassName: gvisor` and `csi: {driver: fern, volumeAttributes: {env: x}}`.
  3. Write files and run Postgres inside.
  4. Take a snapshot from the host while it runs; restore; `Suspended` → `Running`.
  5. Time `Suspended` → `Running`; force-delete the pod and check the controller recreates it.
- **Pass:** works under gVisor and runc; data is visible both ways; unmount is clean on suspend; the CSI driver sees the pod's owner chain (pod → `Sandbox` → `Environment`).

### E1. Store basics and snapshot cost
- **How:**
  1. Create a subvolume and fill it with 0 MB, 500 MB and 5 GB.
  2. `btrfs subvolume snapshot -r` ×100 each.
  3. Fork; restore by swap; check `btrfs filesystem du`.
- **Pass:**
  - snapshot p50 flat across sizes
  - restore = exact snapshot content (tree hashes)
  - forks don't leak
  - space ≈ changed blocks

### E2. Postgres consistency under load
- **How:**
  1. Postgres runs in a session pod with its data in `live/.fern/pg`.
  2. A writer records the last acknowledged ID.
  3. Take 50+ snapshots during the writes.
  4. Recover each snapshot on a writable copy.
- **Pass:** every copy recovers; `max(id)` ≥ the last ID acknowledged before the snapshot; `amcheck` is clean. Bonus: SQLite in WAL mode.
- **Status (2026-10-24):** `lab/check.sh pgload`: 60 snapshots, one a second, under `pgbench -c 4`; each restored into the environment, then pgbench's account, teller and branch balances agree (every transaction whole) and `pg_amcheck --heapallindexed` is clean: 60 of 60. The acknowledged-ID part isn't checked. Every box runs under gVisor now; since retention keeps an environment's newest 50 snapshots, the newest 50 of the 60 are restored, and the oldest are checked to be refused.

### E3. Golden snapshots
- **How:**
  1. Time a cold setup vs 10 environments from golden.
  2. Check the rebuild rules (lockfile → rebuild, source file → no rebuild).
  3. Check setup leftovers: no processes, no secrets in golden.
- **Pass:** an environment from golden in well under 1 s (excluding pod start); rebuild rules hold.

### E4. Session pods hold no state
- **How:**
  1. Start a session (`Sandbox` → `Running`) (read-only image, `live/` via CSI, services from `services.yaml`).
  2. Measure start-to-ready.
  3. `Suspended` → `Running` → data intact.
  4. `kubectl delete pod --force` → recreate → Postgres recovers.
  5. A write outside the mounts is gone afterwards.
- **Pass:** all recover; only declared services return.

### E5. One writer per environment (actors)
- **How:** 50 goroutines firing mixed exec/snapshot/restore/fork at one environment; check invariants after each step; run under `-race`.
- **Pass:** zero violations over 10,000 randomized runs; long execs don't block mutations; a restore cancels execs.
- **Status (2026-10-24):** `TestConcurrentChanges`: 50 goroutines, 2,000 mixed changes and execs over three environments, with a store and driver that fail on any overlap; passes under `-race`, repeated. `TestStopCancelsExecs` covers the cancel. Fork isn't built.

### E6. Crash safety (Reconcile + operator)
- **How:**
  1. Failpoints between every step of rewind, fork and golden build.
  2. Kill the daemon there (`kubectl delete pod --force` on the DaemonSet pod), let it restart and reconcile.
  3. Repeat thousands of times in CI (fake driver) and hundreds of times on k3s.
- **Pass:** always the "before" or the "after" state; no orphaned subvolumes or pods; `Environment` status correct.
- **Status (2026-10-24), the store part:** `TestCrash` stops create, adopt, delete, snapshot, delete snapshot and restore at each of their 15 steps, runs `Reconcile`, and checks before-or-after, that a restore never loses the replaced workspace, and that the change can be made again: all pass on btrfs. Killing the real daemon at those points, fork, golden and the operator are still to do.

### E7. Front door: waking on touch
- **How:** put an environment to sleep, then send 20 concurrent requests (preview, exec, socket). Count pod creations and time-to-first-byte.
- **Pass:** exactly **one** pod created; all 20 succeed; 503 (never 404) while waking.

### E8. Idle, sleep, reattach
- **How:**
  1. Use a short idle timeout.
  2. Each signal (open exec, attached terminal, preview connection) keeps it awake; CPU doesn't.
  3. Restart the daemon during a long exec: the agent's terminal survives and can be reattached through `fern-init`.
- **Pass:** sleep happens only when all signals are off; reattach works.

### E9. Fork identity and isolation
- **How:** fork twice, run both on `:3000` with their own Postgres; check the identity hook; check for data leaks.
- **Pass:** no clashes, no shared identity, no leaks.

### E10. The T3 hook
- **How:** first a fake T3 script (snapshot per "turn", restore on revert, a migration that drops a column); then the real T3 (patched build) with one real `claude` turn. You log in on the VM yourself.
- **Pass:** after a revert to turn N, both code and DB match turn N.

### E13. CRDs and garbage collection
- **How:** `kubectl apply` an `Environment`; check the daemon on that node adopts it and writes status. Delete it: session pod, `live/` and snapshots are cleaned up (finalizer). Change its `EnvironmentClass` → the next session uses the new runtime and limits.
- **Pass:** status always matches reality; nothing leaks.

### E14. Security checks
- **How:**
  - a pod in another namespace names someone else's environment in its CSI volume → **mount refused**
  - the daemon tries to create a pod on another node → **admission policy rejects it**
  - a box with user namespaces: root inside is not root on the host
  - login files are absent from every snapshot (scan)
- **Pass:** every check refuses or holds.

### E15. Two nodes (week 6)
- **How:** join node 2; forks for benchmarks land on node 2 (disk shipped with `btrfs send`); previews to node-2 environments arrive via node 1's daemon and are forwarded.
- **Pass:** works; benchmark numbers on separate nodes are stable.

### E16. agent-sandbox stress test (week 3, upstream)
- **How:** 100s of `Sandbox` objects cycling `Running` ↔ `Suspended` on k3s + gVisor; random pod and controller kills; record p50/p95 for each phase and the controller's API calls.
- **Pass:** a reproducible benchmark script plus results, published as a docs PR to agent-sandbox; every bug found filed with a reproduction (design §21.4).

---

## 5. What this plan no longer includes
- **A local (non-Kubernetes) driver:** dropped. The local lab runs k3s in Docker instead, so it uses the same code path.
- **Firecracker / Kata:** cheap VMs lack nested virtualization.
- **ZFS, Alchemy, Code Mode, portless, warm pools:** later.

---

## 6. Order (matches [roadmap.md](roadmap.md))
| Week | Experiments |
|---|---|
| 0 (local) | E0 (runc; gVisor if possible) |
| 1 (local) | E1, E4 |
| 2 (local) | E2, E5, E6 (lite, ~100 runs), E8 (sleep part) |
| 3 (local) | E10 → MVP works locally |
| 4 (remote) | rerun everything on the VPS with gVisor → `v0.1`; then E3, E7, E9, E16 |
| 5 | E13, E14 |
| 6 | E6 (thousands of runs), E8 (reattach), E15 |
| 7 | measure everything, failure analysis, record the demo |

**Done when:** E0–E16 pass as scripted tests, crash recovery holds under thousands of injected crashes, and the 2.5-minute demo (design §21.1) runs end to end.
