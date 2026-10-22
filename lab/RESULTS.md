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

## Blocker checks of 2026-10-21

Command: `lab/up.sh && lab/blockers.sh`, same lab as above, box image with Claude Code 2.1.304. Nothing was logged in to, so Claude was checked up to the login.

**Two bugs found and fixed:**
- **Exec broke on chunks over 32 KiB.** The daemon's relay and the API client kept the WebSocket library's default read limit, so `cat` of 50 MB (and any large prompt on stdin) failed with "message too big". Fixed in `boxexec.Accept` / `MaxMessage`, with tests.
- **The first exec after a node restart failed** ("no route to host"): the Sandbox's status still named the old pod IP. The driver now takes the address and readiness from the pod. Two reruns pass.

| Check | Result |
|---|---|
| `claude --version` in a box (read-only root, uid 1000), runc and gVisor | PASS |
| the box reaches `api.anthropic.com`, runc and gVisor | PASS |
| `claude -p --input-format stream-json --output-format stream-json` through `fern exec -i` | PASS: whole JSON lines (`system/init`, `assistant`, `result`); without a login the result is "Not logged in · Please run /login" |
| interactive `claude` through `fern exec -it` | PASS: the first-run screen draws; two Ctrl-Cs quit |
| 10,000 JSON lines and 50 MB of random bytes through `cat`, unchanged | PASS (50 MB round trip Mac → box → Mac: 472 ms) |
| 200 MB of stdout, every byte | PASS (846 ms) |
| stdout/stderr apart, exit code, stdin EOF, Ctrl-C → 130, a client that goes away stops its command | PASS |
| a command idle for 180 s still answers | PASS |
| 10 parallel execs, awake / asleep (one wake) | PASS (asleep: 1.0 s for all 10) |
| restore while an exec is open | the exec ends with an error after 3 s (no hang); the environment works after it |
| daemon pod deleted while a box is awake | PASS: same box pod, Postgres keeps its data, snapshot and restore work |
| whole node restarted (`docker restart`) | PASS after the fix: data and a snapshot from before the restart are back; first exec 3.0 s |
| 101,003 files, 1.4 GB in `live/` | snapshot p50 6.5 ms (n=10); restore while asleep p50 12.5 ms (n=5); `rm` with 10 snapshots 382 ms |
| `create --repo` of 20,000 files (20 MB) from the Mac | 728 ms |
| `git clone --depth 1` of t3code inside a box | 18 s, 32,040 files, 483 MB; the snapshot right after took 148 ms (flushing what the clone just wrote) |
| 10 sleeping environments woken at once | PASS, 6.4–6.6 s until all answered; 10 awake boxes use about 1 GB |

**Not checked here:** a real Claude turn (needs a login, by hand), and T3's MCP tools from inside a box (needs T3).

## After the review fixes, 2026-10-21

Command: `lab/build.sh && lab/up.sh && lab/demo-slice.sh && lab/blockers.sh claude stream concurrency restore daemon bigrepo many review`, same lab. Every check passes (`demo-slice: PASS`, and every blocker check, after two fixes to the `review` section noted below).

| Check | Result |
|---|---|
| demo slice (create, exec, snapshot, drop table, restore, rm) | PASS; restore 1.4 s |
| Claude Code in a box, stream-json, terminal | PASS (unchanged) |
| exec stream: 10k JSON lines, 50 MB, 200 MB stdout, exit codes, stdin EOF, Ctrl-C, disconnect, 180 s idle | PASS; 50 MB round trip 1.0 s, 200 MB stdout 1.5 s |
| 10 parallel execs on a sleeping box | PASS, one wake, 0.9 s |
| restore with an open exec | PASS; the exec ends after 2.8 s with an error (iteration 2: the actor cancels it cleanly) |
| daemon restart | PASS; back in 1.7 s, the box keeps running |
| 101k files / 1.4 GB: snapshot, restore, rm | snapshot p50 7.2 ms, restore p50 17.3 ms, rm with 10 snapshots 434 ms |
| t3code clone in a box, then a snapshot | 19.2 s clone; snapshot 224 ms (btrfs writes the clone's data first) |
| 10 fresh environments woken at once | PASS, 6.2 s. All pods start within 1 s; the rest is 10 `initdb`s at once. Waking the same 10 again: 1.9 s; one alone: 1.0 s |
| `fern exec` without `-i` sees stdin EOF | PASS |
| commands in the box run with `no_new_privs` | PASS |
| box CPU limit (2 cores), pod PID limit (4096) | PASS |
| a box can't reach another box on :7070; the daemon still can | PASS |
| restoring a missing snapshot is refused and leaves the box running | PASS |
| `fern rm` interrupted by the client still finishes, nothing left on disk | PASS |
| sleep with Postgres (stop command, then SIGTERM) | 1.0 s |

**Two checks were wrong at first, not the code:** the PID limit is on the pod's cgroup, which the container can't see (now read from the node), and `nosuid,nodev` on the workspace was dropped: the container runtime makes its own bind mount for the box without them, whatever the CSI plugin sets. `no_new_privs` is checked instead.

## Iteration 2, part A: workspaces at a path and the fast wake, 2026-10-21

Command: `lab/demo-slice.sh && lab/blockers.sh paths review stream concurrency restore daemon many claude`, then the timing sections again after each wake change. Every check passes (53 PASS).

| Check | Result |
|---|---|
| a workspace at `worktrees/<repo>/<name>`: the box runs there, Postgres works, `which` finds it from a folder inside | PASS |
| `exec --here`, `exec -e`, SIGTERM forwarded (the command's trap exits 7) | PASS |
| `fern-claude` runs claude in the box inside a workspace, the local one outside | PASS |
| adopt the sample repo / 20,000 files (a plain directory T3 made first) | 302 ms / 810 ms through the CLI, a subvolume afterwards, Postgres works |
| a replaced snapshot holds the new state; `rmsnap` removes it; `rm` removes the workspaces | PASS |
| warm wake with Postgres, 10 runs | p50 622 ms, p95 711 ms (was about 0.8–1.0 s) |
| 10 fresh environments woken at once | 2.4–2.7 s (was 6.2 s) |
| 10 parallel execs on one sleeping box | 0.95–1.43 s over three runs (0.94 s in one earlier run; the 10 `docker exec` + `kubectl exec` hops make it noisy) |

**Three things the faster wake exposed:**
- **The sample repo's ready check passed before its seed loaded,** so the first exec found no table. The old wake was slow enough to hide it. `ready` now also waits for `.fern/seeded`.
- **Connections to a new pod hung.** For a moment after a pod starts, the network policy drops packets instead of refusing them, and each attempt waited the client's full timeout (10 parallel execs: 4.3 s). Connecting now times out after 250 ms.
- **Ten fresh boxes took 20 s once and 7.5 s another time.** The boxes' logs show 5 s between `initdb` finishing and Postgres starting: `initdb` flushing its files, ten at once on Docker Desktop's disk. The sample repo now runs `initdb --no-sync` (the next snapshot flushes anyway), and the daemon asks `/healthz?wait=5s` once instead of every 50 ms, while fern-init runs its checks at most every 100 ms.

## Iteration 2, part B: T3's server on the node, 2026-10-22

Command: `lab/up.sh && lab/build-t3.sh && lab/blockers.sh t3`. T3 Code at `9c161462` with `images/t3/fern-checkpoints.patch`; the image build runs the hook's tests and T3's own checkpoint tests first (19 pass). A cold build takes 3 min 42 s; the image is 2.8 GB. Every check passes (11 PASS).

| Check | Result |
|---|---|
| the Mac reaches T3's API through the NodePort (`/.well-known/t3/environment`) | PASS |
| T3 runs as uid 1000 and uses the daemon's socket (group 1000, mode 0660) | PASS |
| `claude` on T3's PATH is `fern-claude`: the local Claude outside a workspace, the box's inside one | PASS |
| a worktree T3 made is adopted at its first checkpoint | 172 ms through `kubectl exec` (31 ms inside the pod) |
| checkpoint snapshot / the same checkpoint replaced | 5.1 ms / 7.7 ms |
| restore brings back the replaced snapshot and a file git doesn't track; git works afterwards | PASS |
| `which` finds the worktree's environment; `rmsnap` removes the checkpoint's snapshot | PASS |

**Found on the way:**
- **T3 keeps its worktrees in `<home>/worktrees`,** so with its home at `/var/lib/fern/t3` the hook would never have fired. T3's home is now the pool itself: its worktrees land in `worktrees/`, and its own state in `userdata/`, `caches/` and `tools/` (architecture §9).
- **Restoring an awake box takes 2.2–3.0 s (4 runs); asleep, 8–9 ms.** The time is stopping the box, with or without Postgres: sleep took 1.4–3.0 s over 10 runs today (1.0 s in one run on 2026-10-21). In a traced run the pod was gone 1.85 s after the sleep began, so most of it is Kubernetes stopping and removing the pod. A T3 revert usually comes right after a turn, while the box is awake, so this is the restore time users will see.
- **The image has no web UI** (`/` answers 503, "No static directory configured"): the Mac app brings its own.

Not checked yet: a real turn and a revert through T3 itself (they need the Claude login), and T3's MCP tools from a box.
