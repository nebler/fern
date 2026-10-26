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

## Part B: Claude's login, its folder and git in the box, 2026-10-22

Command: `lab/reload.sh && lab/build-t3.sh && lab/blockers.sh t3 paths review stream restore && lab/demo-slice.sh`. Every check passes (56 PASS; the T3 section 15). No real login was used: the check stores a fake token in the `claude-login` Secret and removes it afterwards.

| Check | Result |
|---|---|
| the box of a T3 worktree has `CLAUDE_CONFIG_DIR` at `envs/<id>/claude`, outside the workspace | PASS |
| what Claude writes there outlives a sleep and a restore | PASS |
| git works in the box (`git log`, `git status`): the main repository's git folder is mounted read-only | PASS |
| the box can't write to that folder (`touch`, `git commit` → "Read-only file system") | PASS |
| `claude -p` through T3's `fern-claude` with the fake token: Claude in the box reaches Anthropic and gets "401 Invalid bearer token" | PASS |
| `ANTHROPIC_API_KEY` is unset in the box | checked by hand |
| the hook's 9 tests (2 new: a failed snapshot keeps T3's git checkpoint; a restore Fern can't do is refused before git touches the files), `vp fmt`, `vp lint` | PASS, 0 warnings |
| warm wake (Postgres), 10 runs | p50 566 ms, p95 619 ms |
| restore of an awake T3 worktree | 1.8–2.0 s (2.2–3.0 s earlier the same day) |

**Found on the way:**
- Claude writes `.claude.json`, `projects/` (the transcripts) and `sessions/` into `CLAUDE_CONFIG_DIR`, so one folder per environment keeps all of it; `/tmp` gets only a scratch folder.
- **T3's Claude SDK (0.3.276) was built for Claude Code 2.1.276; the box has 2.1.304.** Newer CLIs talk to older SDKs, but if the first real turn fails on the protocol, pin the box to 2.1.276.
- **The sample repo had no `.gitignore`,** so T3's git checkpoints would have copied Postgres's data folder (`.fern/`) into git at every turn. Any repo whose services keep state in the workspace needs it ignored.
- Claude can't commit from the box (the git folder is read-only); T3 commits on its side.

## One check script, 2026-10-22

The lab's checks are now one script, `lab/check.sh`, in sections named for what they check. The commands above name the scripts it replaced: `demo-slice.sh` is its `slice` section; `e0.sh` and `e4.sh` are `box`; `e1.sh` and `blockers.sh bigrepo` are `disk`; `gvisor.sh` is `gvisor`; `blockers.sh review` is split into `limits`, `restore` and `daemon`. `lab/build.sh` replaces `reload.sh` and `build-t3.sh`.

Command: `lab/down.sh && lab/up.sh && lab/build.sh t3 && lab/check.sh && lab/check.sh t3`, from a clean start: setup 176 s, checks about 25 minutes, same host and versions as on 2026-10-18. 86 PASS, 1 FAIL: the new check that two Ctrl-Cs quit interactive claude (below). With that check replaced, `lab/check.sh stream claude` passes.

| Check | Result |
|---|---|
| warm wake with Postgres, 10 runs | p50 622 ms, p95 670 ms |
| sleep, until the pod is gone, 10 runs | p50 1,010 ms, p95 1,014 ms |
| the workspace is unmounted at every sleep; Postgres answers again after its pod is force-deleted | PASS; 748 ms |
| 100,000 files, 1.4 GB: snapshot (10 runs) / restore while asleep (5 runs) | p50 7.2 ms / 7.8 ms; the restore equals its snapshot (tree hash) |
| `rm` with 10 snapshots | 432 ms; nothing left |
| `git clone --depth 1` of t3code in a box (32,203 files, 484 MB), then a snapshot | 19.5 s, then 242 ms |
| 10 sleeping environments woken at once / 10 parallel execs on one sleeping box | 2.2 s / 1.0 s |
| restore of an awake T3 worktree (no services) | 816 ms |
| gVisor: wake after a restore, 5 runs | p50 808 ms |
| `fern-t3:dev` | 1.45 GB (was 2.82 GB): only the server's production packages |

**Found on the way:**
- **The old check that two Ctrl-Cs quit interactive claude could never fail, and claude doesn't quit that way,** so the rows above that say it does are wrong. At its first-run screen Claude Code 2.1.304 shows "Press Ctrl-C again to exit" and keeps running after the second, in plain Docker without Fern too. Fern's part is now checked directly: both Ctrl-C bytes reach a program that reads the terminal raw, and the session ends when that program exits.
- The check that a command without `-i` gets no stdin passed whenever the script's own stdin was empty; it now offers endless input.
- A process a command leaves in the background ends with the exec (its process group is killed), so the check that nothing survives a sleep starts its stray process with `setsid`.

## First real turn in T3, 2026-10-24

The T3 app on the Mac, paired with the lab's T3 server (`lab/t3-pair.sh`), Claude on the user's own subscription (`lab/claude-login.sh`). In the `sample` project, a thread in a new worktree:
1. "How many users?" Claude ran psql in the box: 3.
2. "Delete Ada from the database with psql. Don't edit any files." Claude deleted her; T3's diff showed nothing, since Postgres's files are in `.fern/`, which git ignores.
3. "Edit from here" on that message, then "Revert files too".
4. "How many users?" 3. Checked outside the chat, in the box: Ada, Grace, Linus.

**PASS.** The worktree became environment `t3-16439bae89448185` at its first checkpoint, with one Fern snapshot per checkpoint. Git's checkpoints alone would have left Ada deleted.

**Found on the way:**
- A thread started in **Local** (the project's main folder) isn't a Fern environment: Claude runs in T3's pod without a database, and edited `seed.sql` instead. Only **New worktree** threads get a box.
- "Revert and keep changes" rewinds only the chat; "Revert files too" is the one that restores.
- A stopped Docker Desktop stops the lab; `docker start fern-lab` brings it back with everything in it.

## Part C: actor, crash safety, idle sleep, 2026-10-24

Same lab. Commands: `lab/test-store.sh`, `lab/go.sh test -race ./...`, `lab/check.sh slice concurrency restore pgload idle daemon paths limits claude t3`.

| Check | Result |
|---|---|
| crash at each of 15 steps of create, adopt, delete, snapshot, delete snapshot, restore, then `Reconcile` (`TestCrash`) | before or after every time; a restore's replaced workspace never lost |
| 50 goroutines, 2,000 mixed changes and execs on 3 environments, `-race` (`TestConcurrentChanges`) | no overlapping change |
| Postgres: 60 snapshots under `pgbench -c 4`, each restored | 60 of 60 whole and `pg_amcheck` clean; snapshot under load p50 19 ms, p95 166 ms |
| idle sleep, with 5 s / 10 s idle times | asleep 10 s after a command started; a running command keeps it awake; also across a daemon restart |
| the same under gVisor, 10 snapshots | 10 of 10; snapshot p50 17 ms; wake after a restore p50 1,170 ms (808 ms on 2026-10-22; not yet explained) |
| a restore keeps the replaced workspace (`pre-restore-<time>`) | restoring it undoes the restore |
| a box rewrites the worktree's `.git` | refused (read-only) |
| `claude` in a pool worktree without an environment | refuses to run |
| a pod outside `fern-boxes` mounts an environment | refused: "may not mount" |
| a box reaches the daemon's metrics / the Kubernetes API / the internet | no / no / yes |

**Found on the way:**
- **Security review: a box could run code in T3's pod.** T3 runs git in each worktree, and git follows the worktree's `.git` file. A box could point it at a folder of its own whose config runs a command (`core.fsmonitor`). The `.git` file is now read-only in the box. Also fixed: the CSI plugin mounted folders for any pod, `fern-claude` ran Claude in T3's pod when the daemon didn't answer, and boxes could reach anything private.
- **Go review: two bugs when a caller gives up.** An exec cancelled during a wake stayed counted, so that box never slept; a cancelled seeded create left the actor reading a finished request's body. A caller now waits for a change the actor has started.
- **k3s in the lab exits when its datastore is slow** (under an image build or pgbench): "Transaction commit failed". The lab container now restarts it. One snapshot under load took 44 s around such a stall.

## Speed and memory pass, 2026-10-24

Same lab, with `deploy/k3s-config.yaml` applied (k3s restarted). Commands: `lab/go.sh test -race ./...`, `lab/check.sh box stream concurrency restore idle many limits t3`. All pass.

| Measurement | Result |
|---|---|
| wake, 30 times (nearest-rank) | p50 620–670 ms, p95 717–722 ms |
| a wake's steps (`fern_wake_step_seconds`, mean) | Sandbox call 6 ms, pod until it has an address 590–623 ms, Postgres ready 21–38 ms |
| sleep, 30 times | p50 1,007 ms; the times fall 200 ms apart (607, 807, 1,007 ms): the driver looks for the pod every 200 ms |
| `sh -c 'sleep 600 & echo ready'` | at once; was 2.0 s (the leftover held the output pipes) |
| SIGTERM to a command that isn't reading 256 KiB of stdin | at once; was never, until the command exited |
| a failing ready check printing 64 MiB | fern-init stays small; was 459 MiB |
| idle box, sample Postgres (cgroup) | runc 27 MB, gVisor 72 MB; `claude` started in it adds about 110 MB |
| ten boxes awake | the lab container uses 1.25 GiB |
| allocatable memory | 6.2 of 7.65 GiB: 1 GiB reserved for k3s and the system, eviction at 500 MiB |

**Found on the way:**
- **The 25–41 s wakes in the metrics were `check.sh many` during a disk stall:** Docker Desktop's disk held up k3s's datastore and containerd (10 s to create a pod sandbox, 10 s more for its container). Healthy wakes under the same load test take 0.6–1.5 s. Fern has no timer of that length.
- **T3 kept Claude running for 30 idle minutes,** so a short turn kept its box (and Claude's 200 MB and more) awake about 35 minutes. Fern's T3 image now releases idle sessions after 5 minutes.

## First VPS, 2026-10-24

A Hetzner CAX21 (4 shared Ampere ARM cores, 7.5 GiB, local NVMe, Nuremberg), Ubuntu 26.04, set up by `lab/vps.sh`: k3s 1.36.5 with `deploy/k3s-config.yaml`, gVisor 20261012, the pool a 50 GB image file on the server's disk. Every box runs under gVisor (in the lab too). Command: `FERN_HOST=… lab/check.sh` and `lab/check.sh`; both run all 81 checks.

| Measurement | VPS, gVisor | VPS, runc (earlier the same day) | lab, gVisor |
|---|---|---|---|
| wake, 30 times, p50 / p95 | 1,142 / 1,225 ms | 696 / 885 ms | 708 / 909 ms |
| a wake's steps (mean): Sandbox call, pod until it has an address, Postgres ready | 13, 944, 191 ms | 12, 686, 49 ms | 6, 679, 44 ms |
| sleep, p50 | 1,019 ms | 1,415 ms | 809 ms |
| snapshot of a 100,000-file workspace, p50 | 18 ms | 17 ms | 10 ms |
| snapshot under pgbench's writes, p50 / p95 | 26 / 39 ms | 25 / 32 ms | 17 / 23 ms |
| ten existing environments woken at once | 5.4 s until all answered (3.3–4.4 s each) | | |
| ten new environments woken at once (first wake: `initdb`) | 34 s | 6.6 s | 10 s |
| node memory used with ten boxes awake | 2.9 GiB | 2.0 GiB | 1.9 GiB (lab container) |

**Found on the way:**
- **gVisor costs about 0.45 s a wake on this CPU:** containerd's sandbox start goes from 150 to 250 ms and the container's start from 100 to 200 ms; Postgres takes 140 ms longer to be ready. Turning off gVisor's root overlay (`overlay2=none`; the root is read-only) changed nothing (1,152 ms either way), so it isn't set.
- **A new environment's first wake runs `initdb`,** about 6 CPU-seconds under gVisor (2.2 s alone, 15 s for ten at once with the four cores busy, 42% of it system time). Golden snapshots, with the database already made, remove that step; nothing is done about it before then.
- **The node's public address let boxes reach the Kubernetes API:** inside the cluster the API's address leads there, which the egress policy allowed. `lab/build.sh` adds the address to the policy's exceptions on a VPS.
- **`kubectl exec` cuts large output short** over the slow link from the VPS to the Mac (one run lost 200 KB of 200 MB, without an error). Fern's stream is whole in the node, even to a reader that stalls 30 s; check.sh counts the 200 MB there.
- **From the Mac, over home internet to Germany:** a 50 MB round trip through a box takes 42 s, and a create seeded with 20 MB from the Mac 26 s. T3 runs on the node, so daily use doesn't take this path.

## The demo on the VPS, 2026-10-25

The T3 app on the Mac, paired with the VPS's T3 server through an SSH tunnel (`T3_PORT=30774 lab/t3-pair.sh`), Claude on the user's subscription (`lab/claude-login.sh`). In the `sample` project, a thread in a new worktree:
1. "How many users?" 3; then Claude dropped the table. The first wake took 3.4 s.
2. "Revert files too" to the checkpoint before it (restore 1.9 s, the next wake 3.3 s), then "how many users?": 3.
3. Left alone, the box went to sleep by itself (06:36 UTC).
4. "What did I ask you first?" The box woke in 1.18 s and Claude answered from the conversation before the sleep. "Felt instant."

**PASS.** The box ran under gVisor; the environment had a snapshot per checkpoint and the `pre-restore-` one.

**Found on the way:**
- **A thread in Local gets no box:** Claude ran in T3's pod and found no database. Planned in architecture §15.
- **The wakes in a turn took 3.3–3.4 s,** not the 1.1 s of the benchmark's. Not yet looked into.
- **The box slept 4 minutes later than its idle times say,** after a `fern exec` at 06:12 that reset them: something touched it about 06:16. Not yet looked into.

## Wake and sleep profile, 2026-10-25

Same CAX21 and gVisor. Each single-box comparison has 25 cycles, timed at
the node's Unix socket. SSH and kubectl run outside the timed operations.
Percentiles use nearest rank. Reproduce with
`FERN_HOST=HOST lab/bench-wake.sh pf1-wake 25`. Pass a snapshot ID as the
third argument to measure restore/wake cycles. For cold wakes, take that
snapshot before the first wake.

Before: wake p50/p95 **1,023/1,143 ms**; sleep **817/1,816 ms**. The earlier
1,142 ms wake median was a different run on shared cores. Compare changes
with this run, rather than claiming that difference as an improvement.

| Rank | Wake component | Before, mean ms | Evidence |
|---|---|---:|---|
| 1 | Kubelet and volume preparation before RunPodSandbox | 327 | Scheduled event to CRI call, 25 pods |
| 2 | Services and readiness after StartContainer returns | 267 | CRI completion to wake response, 25 pods |
| 3 | containerd/gVisor RunPodSandbox, including CNI | 224 | containerd log pairs, 25 pods |
| 4 | containerd/gVisor StartContainer | 178 | containerd log pairs, 25 pods |
| 5 | CreateContainer | 16 | containerd log pairs, 25 pods |
| 6 | Fern's Sandbox API call | 12 | histogram delta, 25 wakes |
| 7 | agent-sandbox controller and scheduler | 12 | request to Scheduled was 24 ms, including the API call |

The first component includes CSI and kubelet's waits; 90 CSI publishes
averaged **0.50 ms** (maximum 2.24 ms). One warm box took 215 ms from fern-init's startup log to
Postgres accepting connections, then another 132 ms until wake answered.
Fern-init started 25 ms before StartContainer returned, so these finer
measurements overlap the table's components. The old `pod` histogram
combined most of this startup with up to 200 ms of address polling.

Sleep's means: StopContainer **252 ms**, StopPodSandbox/CNI **154 ms**, then
**468 ms** for kubelet cleanup, pod removal and Fern noticing it. Postgres
gets its graceful stop; returning before the pod and mounts disappear is
not safe for restore.

**The slow demo wakes:** T3's initial checkpoint was captured at
06:05:51.246, before the first box wake. The rollback explicitly selected
checkpoint 0. Thus both wakes ran `initdb`; the restored checkpoint did not
contain an initialized cluster. Twenty-five restores of an empty snapshot
gave wake p50/p95 **3,123/3,226 ms**; box logs put about 2 s between fern-init
starting and Postgres starting. The demo's CRI container startup finished
at 06:05:53.043 and 06:08:59.970, well before Fern answered at 06:05:55.581
and 06:09:02.469. Golden snapshots address this planned cost.

**The late sleep:** T3's event database records its Claude session stopping
at 06:14:13.893. The supervising Iteration 1 thread, run 215, then ran
`fern exec ... /proc` at 06:16:26 to check the process list. Its tool result
was recorded at 06:16:27.109. That probe reset the human clock to 06:36:26;
the daemon logged sleep complete at 06:36:28.118. The previous answer had
mistakenly called the probe time 06:12. No hidden T3 keepalive is needed to
explain the four minutes. Use `fern ls` and node/pod logs to observe idle
boxes without resetting their clocks.

**Kept:** `7156cb02` checks the sample repo's seed marker before spawning
`pg_isready`. Readiness still requires both conditions. It avoids probes
while initdb runs. Cold wake p50/p95 became **2,971/3,080 ms**, down
**152/146 ms**, about 5%, in 25 more empty-snapshot restores. Existing
worktrees benefit when their `services.yaml` is updated; this is a sample
service optimization. Golden snapshots still remove roughly 2 s more.

The final 25 initialized wakes were **1,020/1,128 ms**; sleep was
**817/2,616 ms**. The sleep median is unchanged. In both 2.6 s outliers,
the controller logged deletion in the request's second; there was about
1.46 s before StopContainer and 0.75 s after StopPodSandbox. The CRI stops
themselves took about 250/156 ms. The tail sits around kubelet's lifecycle
handling. No sleep change was retained.

`a7eed297` logs exec activity, idle deadlines and completed CSI timings.
`372993c4` records pod-address detection after the API call, correcting
the step boundary. `1d49dee1` adds restore cycles to the benchmark.

**Rejected:** 50 ms pod polling. An original-code crossover repeated the
baseline: wake **1,014/1,104 ms**, sleep **816/1,815 ms**. At 50 ms, wake
was **1,072/1,115 ms**, sleep **814/914 ms**. Thirty concurrent initialized
wakes per variant gave p50/p95 **3,546/4,374 ms** at 200 ms polling and
**3,775/4,227 ms** at 50 ms. The better single-box sleep tail did not justify
four times as many queries and a worse wake median. Polling remains 200 ms.

Non-waiting `pg_ctl stop -W` was considered, not shipped: fern-init would
start its 3 s kill deadline immediately instead of allowing up to 8 s for
the stop command. PostgreSQL documents its waiting behavior in
[pg_ctl](https://www.postgresql.org/docs/17/app-pg-ctl.html).

Remaining gains are estimates: golden snapshots **about 2 s** per cold
wake; upstream kubelet/volume scheduling **50–200 ms** of the measured
327 ms; service startup and readiness commands **30–100 ms**. The planned
watches should reduce API work; allow **0–100 ms** for faster detection,
not a guaranteed latency win. CSI's bind mounts are already negligible.

Validation: Docker `lab/go.sh test -race ./...`, `lab/lint.sh` (0 issues),
`go vet` and gofmt. An existing supervisor timing assertion flaked once
under build load; the rerun passed. The sample still had 3 users after the
final 25 initialized restarts. `FERN_HOST=HOST lab/build.sh` redeployed the
final code. The daemon and T3 CLI hashes both matched the local image,
`cad3acd333fac00e…`.

## Memory, exec, store and T3 passes, 2026-10-25

Four more agents (GPT-6.1-Sol), each in its own worktree. Their full notes stay on their branches (`t3/measure-memory-footprint`, `t3/reduce-exec-overhead`, `t3/store-performance`, `t3/t3-turn-performance`); what was kept is here.

| Measurement | Result |
|---|---|
| empty box under gVisor, lab (fern-init, pause, runtime) | 25.6 MiB |
| sample Postgres in it | +44.6 MiB; 128 MiB shared_buffers only partly resident |
| a Postgres backend with a psql client | +3.2 MiB a pair |
| Claude started and waiting, no prompt | +83 MiB, most of it file-backed |
| box image unpacked | 756 MiB: Claude 275 MiB, an LLVM library 118 MiB |
| `fern` CLI start, in the daemon pod | 160–240 ms: the CLI is the daemon's binary and loads Kubernetes, CSI and gRPC |
| a path lookup over the socket / a checkpoint, fake store | 0.29 / 0.60 ms |
| Go heap held per idle exec, through the relay | 279 KiB → 72 KiB: stream buffers start at 4 KiB, grow to 64 KiB |
| re-indexing 100 snapshots on start | 3.40 s → 27 ms: one transaction, not 100 commits |
| `fern-claude` per Claude start | two lookups → one |

**Rejected:**
- **A smaller Postgres** (32 MiB buffers, 20 connections): 9–20 MiB a box saved, writes looked slower.
- **SQLite `synchronous=NORMAL`:** the index holds box tokens.
- **Flushing before adopt's reflink copy, prepared statements for single rows:** no gain.

**For an 8 GiB node:** plan on 10–12 boxes with Claude active at once. The suggested box settings (request 384 MiB, a 64 MiB RuntimeClass overhead, limit 2 GiB) are a recommendation, not measured; not applied.

**Not merged yet:** btrfs ioctls instead of the `btrfs` command (`t3/store-performance`, `e70e95bf`). Far fewer allocations, but in the lab a snapshot after 8 MiB of writes went 67 → 84 ms while a clean one went 51 → 28 ms; it is measured on the VPS first.

**Biggest remaining per-call cost:** the CLI's start, paid at every checkpoint and twice per Claude start. A small CLI binary, or the checkpoint hook speaking to the socket, would remove most of it.

## The whole suite on the VPS, 2026-10-25

The same CAX21. Command: `FERN_HOST=… lab/perf.sh --accept` (the baseline, `lab/perf/vps.tsv`, 5 runs, 42 min), then `FERN_HOST=… lab/suite.sh` (45 min), at commit `2deaff9d`.

**PASS:** all checks (13 min), the perf gate against the new baseline (`golden.build_ms` 21% better), and every T3 run: 5/5 one at a time, 5/5 at once, 2/2 on `sample`. The suite first reported the T3 parts as failed: `t3-run.sh`'s tunnel cleanup returned 1 under `set -e` after the tunnel had already ended. Fixed.

**Scale** (`check.sh scale`: 1, 5, 10, 20 new boxes woken at once):

| Awake boxes | until all are ready | node memory per box | daemon memory | exec p50 | 20 execs at once | snapshot p50 |
|---|---|---|---|---|---|---|
| 1 | 3.1 s | 94 MiB | 15.1 MiB | 102 ms | 103 ms | 17 ms |
| 5 | 9.5 s | 88 MiB | 15.1 MiB | 119 ms | 168 ms | 16 ms |
| 10 | 12.5 s | 93 MiB | 15.2 MiB | 114 ms | 262 ms | 18 ms |
| 20 | 26.7 s | 88 MiB | 15.8 MiB | 115 ms | 1,430 ms | 18 ms |

The daemon, exec and snapshots don't grow with the number of boxes. Waking many at once does, at about 1.3 s a box: the four cores are the limit (gVisor and Postgres starting). A box's own cgroup holds 47 MiB; on the node it costs about 90 MiB.

**T3 end to end** (`toy`, Haiku, through T3's server as the app would):

| | one thread at a time | five threads at once |
|---|---|---|
| turn 1 (new worktree, first wake), p50 / max | 8.3 / 9.3 s | 13.9 / 16.2 s |
| turn 2 (edit, run the tests), p50 | 14.8 s | 17.0 s |
| revert files to before turn 2, p50 / max | 1.8 / 2.4 s | 2.6 / 4.7 s |
| turn 3 (after the revert), p50 | 11.6 s | 13.8 s |
| a wake inside a turn, p50 | 1.3 s | 1.9 s |
| node memory, before → peak | 1.66 → 2.03 GiB | 1.89 → 3.07 GiB |

Five threads at once cost about 240 MiB each at the peak (box plus Claude), so the 8 GiB node holds 20 or more active threads by memory; CPU limits it first.

**Not Fern:** `disk.create_seeded_ms` (33 s) and `stream.roundtrip_50mb_ms` (67 s) in the VPS baseline carry 20–50 MB from the Mac to Germany; they measure the home link.

## Wake follow-up after the kernel lab lock, 2026-10-25

Local Docker Desktop lab, ARM64, gVisor; base `508befc5`, which includes
`944075b5`'s flock fix. Each build and its measurement held one lab lock.
No VPS runs or baseline changes. Reproduce each comparison with:

```sh
bash -c 'source lab/lib.sh; lab_lock; lab/build.sh && WAKES=10 REPEAT=3 lab/perf.sh box golden'
```

**No optimization retained.** The saved watch prototype was adapted to
`cmd/fern-daemon`, measured, then reverted. A crossover redeployed the
unchanged code. Values below are medians of three runs; each box run has
ten sleep/wake cycles.

| Metric, ms | Unchanged A | Watch B | Unchanged crossover A |
|---|---:|---:|---:|
| wake p50 | 703.9 | 706.6 | 706.8 |
| wake p95 | 847.3 | 861.1 | 951.3 |
| golden first wake | 747.1 | 741.9 | 865.9 |
| cold first wake | 1,739.2 | 2,000.6 | 1,926.2 |
| sleep p50 | 1,011.0 | 1,010.3 | 1,011.8 |
| sleep p95 | 1,213.3 | 1,818.6 | 1,657.2 |

All three gates passed. Watching did not beat the baseline's 25% wake
margin (34% for cold first wake). Across the three runs, the step means
shifted from approximately **668 ms pod / 64 ms ready** to **555 / 172**:
earlier address detection mostly moved time between the two buckets.

**Where the time goes:** a separate ten-wake trace correlated daemon, CSI,
kubelet and containerd timestamps by pod UID. Extra kubelet logging was
enabled only for this diagnostic run and restored afterward. Its wake
p50 was 725.5 ms, mean 807.6 ms; one Postgres startup took 745 ms.

| Component | Mean ms |
|---|---:|
| Fern's Sandbox API call | 6.7 |
| Controller, scheduling and kubelet before the volume wait | 13.1 |
| Kubelet WaitForAttachAndMount | 302.1 |
| RunPodSandbox, including CNI and gVisor | 117.7 |
| CreateContainer | 9.3 |
| StartContainer | 87.5 |
| Services and detecting readiness after StartContainer | 263.1 |
| Other gaps between calls | 8.1 |

The initial volume check ran before mounts were ready. Every wake then
waited for its first 300 ms recheck: **300.2–306.5 ms** in the trace.
CSI publishes averaged **0.51 ms**, maximum **1.00 ms** (22 publishes,
including warmup). The last publish finished **29–181 ms**, mean **104 ms**,
before kubelet noticed the mounts. This is not CSI just missing a second
poll tick: mounts completed before the first recheck, after the initial
check. The trace also shows a separate reconciliation pass to mark these
non-attachable volumes attached, then another pass about 100 ms later to
mount them. Kubernetes' 100 ms discovery/reconciliation loops explain
most of the earlier part of the wait.

Four disposable gVisor pods per volume type confirmed the boundary:
Scheduled → RunPodSandbox averaged **9.2 ms without volumes**, **309.0 ms
with hostPath**, and **308.5 ms with emptyDir**. These were diagnostic
controls, not proposed replacements for Fern's CSI mounts. Fern already
sets `attachRequired: false` and `fsGroupPolicy: None`; switching volume
types does not remove this wait. The retry and loop periods are constants
in [Kubernetes' volume manager](https://github.com/kubernetes/kubernetes/blob/v1.36.5/pkg/kubelet/volumemanager/volume_manager.go),
not a kubelet configuration setting.

**Inside gVisor:** an additional warm wake, with info-level runtime logs,
spent 122 ms in RunPodSandbox and 78 ms in StartContainer. Its built-in
boot timer covered 53 ms: 16 ms waiting for the parent's start signal,
9 ms compiling seccomp, 5 ms trimming capabilities, 3 ms initializing
systrap, and 7 ms creating/starting the root container, plus other setup.
The workload's gofer took about 18 ms to prepare mounts and filters;
filter compilation accounted for 4 ms. Both sentry and gofer reported
missing precompiled seccomp programs for this ARM64 configuration. These
timings overlap the CRI intervals.
The temporary runtime wrapper was removed before further measurements.

The next substantial target is upstream kubelet volume scheduling and
notification: notifying a waiter when mounts complete could recover the
measured 104 ms average recheck gap; faster discovery/reconciliation is a
separate opportunity. Runtime startup and service startup follow it.

Validation on the restored code: `lab/go.sh test -race ./...`,
`lab/go.sh vet ./...`, `lab/lint.sh` (0 issues), gofmt, and the quick
`WAKES=3 lab/check.sh` sections all passed. The final deployed binaries
use the unchanged wake path.

## Five agents, one layer each, 2026-10-25

Five agents (GPT-6.1) each took one layer (wake, box memory, daemon and CLI, stop and revert, disk and golden) in its own worktree and measured only its own sections, A against B. Merged into iter-1; then `lab/perf.sh` on the merge passed in the lab and on the VPS, and both baselines were recorded again (5 runs each).

| Kept | measured by its agent (lab) | VPS gate, before → after |
|---|---|---|
| `fern` CLI apart from the daemon (`cmd/fern-daemon`): no Kubernetes, SQLite, gRPC in the client | exec in the node 15.4 → 6.3 ms, start 10.2 → 1.3 ms, binary 61 → 9.7 MB | checkpoint 16.4 → 10.3 ms, adopt 155 → 100 ms, exec with 20 boxes 103 → 87 ms |
| btrfs ioctls instead of the `btrfs` command | restore p50 19.2 → 11.4 ms, 128 → 11 KB allocated | snapshot p50 18.9 → 15.4 ms |
| golden sources cloned, not copied | 20,000 files 751 → 16 ms | golden build 5,696 → 4,479 ms |
| 250 ms connect timeout to a stale pod | force-delete recovery 1,924 → 1,141 ms | 2,114 → 1,303 ms |
| box memory counted with gVisor's shared memory | (accounting only: 44.6 → 70.5 MiB a box) | |

Wake on the VPS went 1,144 → 1,035 ms (baseline medians); box and daemon memory didn't change (47, 16 MiB). Sleep stays 2.6 s.

**Where the rest goes:**
- **A box, about 72 MiB warm in the lab:** 24 the application, 23 gVisor's sentry and gofers, 15 shims and helpers, 9 kernel and node. Details in `lab/BOX-MEMORY.md`.
- **A wake, lab:** kubelet's volume wait 302 ms (CSI publishes in 0.5 ms; the mount is done 104 ms before kubelet notices), RunPodSandbox 118, StartContainer 87, services and readiness 263.
- **A stop, lab:** containers 183 ms, sandbox and network 113, kubelet's cleanup until Fern sees it 750.

**Rejected,** with numbers in the commits and reports: a watch instead of polling for wakes (704 → 707 ms), 50 ms polling for stops, deleting the pod in `rm`, grouping shims (4 MiB a box), `GOGC=20` for helpers, prepared SQLite statements, stripping the CLI.

**Found on the way:** `lab_lock`'s mkdir lock let two scripts in when both took over a dead owner's lock; it is a kernel flock now. The first wake agent's measurements were void for it.

## T3 turn timing, 2026-10-25

Local lab only, from `fc7dd98b` with timing enabled; build and measurement
held the same `lab_lock`. Two `toy` runs on `claude-haiku-5-5` passed,
including their reverts. `lab/check.sh t3 claude` and `lab/perf.sh --fast`
also passed. No perf baseline was changed.

The driver retains `wallMs`, `runMs`, answers and daemon histogram deltas.
Its new `timing` object joins T3's launch/checkpoint logs with fern-init's
successful exec start and first stdout, and records completion delivered by
T3's WebSocket stream. Log collection follows the measured interval. The
Mac and node clocks must agree; intervals within the startup benchmark use
one process's monotonic clock. The exec protocol and its output are unchanged.

Baseline ranges across the two runs (seconds after sending the message):

| Boundary | New worktree | After revert |
|---|---:|---:|
| T3 launches the wrapper | 0.364–0.523 | 0.111–0.327 |
| Claude's process has started | 1.320–1.625 | 1.002–1.412 |
| First stdout from Claude | 2.404–2.866 | 1.940–2.371 |
| First decoded SDK event | 2.486–2.939 | 1.995–2.432 |
| Completion reaches the driver | 5.146–6.239 | 6.563–8.304 |

The warm edit/test turn reused the process and completed in 9.171–13.633 s.
There was **no per-tool Fern checkpoint**: its three command tools and two
file changes produced one end-of-turn snapshot. New worktrees and turns
after a revert captured a baseline as well as the end checkpoint.

- Waking took 873–1,086 ms in these turns. Wrapper launch to process start
  exceeded the daemon's wake by only 16–18 ms, including the lookup,
  transport and 1–2 ms process spawn.
- Native Claude startup to first stdout took 1,084–1,241 ms on a new
  environment and 938–959 ms after a revert. Claude is the pinned native/Bun
  executable in the image; Node runs the toy service, not Claude.
- The hook's actual adoption took 22–26 ms, versus `check.sh`'s 212 ms
  `t3.adopt_ms`, which includes the kubectl measurement transport.
- Ten Fern snapshot hook spans took 11–37 ms, median 18 ms. The associated
  Git captures took 49–220 ms, median 69.5 ms. Claude result to streamed
  completion took 147–253 ms, including the final Git/Fern capture.

Two traps in the old timings: T3 assigns `completedAt` before the checkpoint
finishes, and its worktree-preparation command is not Claude output. The
first actual provider command on a new thread started at 4.066–4.867 s
(from retained baseline event IDs); new records exclude preparation using
`providerTurnId`. Claude's `duration_api_ms` is cumulative across turns and
resumes and can exceed its reported turn duration. The driver records the
raw total and its delta rather than subtracting it as pure model wall time.

The cold box build initially remained in HTTP package downloads after
450 s and was stopped to release the lab. Bootstrapping apt's CA path and
using HTTPS completed the box build in 77 s (105 MB fetched in 39 s).

Reproduce startup and transport comparisons without a model prompt:

```bash
bash -c 'source lab/lib.sh; lab_lock; lab/build.sh t3 && lab/t3-start.sh'
```

### Snapshot transport comparison

The kept candidate sends the awaited `POST /envs/<id>/snapshots` directly
from the checkpoint hook over `/run/fern/fern.sock`, instead of spawning
`fern snapshot`. Git capture still succeeds first; adoption, restore and
pruning still use the CLI. Snapshot replacement, the 120 s deadline,
cancellation and the best-effort failure rule stay in place. The added
socket tests cover 200/204 success and 503 failure while retaining Git refs.

Twenty alternating CLI/socket samples in one T3 Node process, against the
same daemon and workspace, each with a fresh snapshot ID:

| Snapshot request | CLI | Socket |
|---|---:|---:|
| p50, ms | 15.49 | 11.85 |
| p95, ms | 41.67 | 24.19 |

That is a **3.64 ms (24%) p50 saving**, with one process removed per capture.
Across two more passing Haiku toy runs, the ten hook spans had median
**18 → 13 ms** (ranges 11–37 → 7–39 ms). Summed hook time above the daemon's
snapshot time fell from approximately **10 → 4 ms per capture**. These
millisecond savings are attributable to the transport; overall turns had
larger model/wake variation. The warm turn still has one Fern capture,
and individual tool calls have none.

### Native startup experiments

Ten samples per case, without a model prompt. The same native Claude
2.1.304 binary, worktree and Claude config directory are used on both
sides. Inside is the normal two-CPU gVisor box; outside is T3's pod. The
initialize probe waits for a successful SDK control response, then stops
its own process. Values are warm p50 milliseconds:

| Probe | Outside | Inside |
|---|---:|---:|
| `claude --version` | 11.2 | 31.3 |
| SDK initialize, default | 542.3 | 694.1 |
| JIT disabled | 549.8 | 704.3 |
| One GC marker | 554.9 | 703.2 |
| Telemetry/error reporting disabled | 237.8 | 376.6 |

`fern which` took 5.2 ms, a complete `fern exec --here -- true` 15.7 ms,
and `fern-claude --version` 42.8 ms (all measured from T3, without kubectl
per sample). Rejected JIT/GC changes: neither improved the median.
Telemetry opt-out saved about 318 ms inside, but also disables feature-flag
fetching and associated CLI features, so it is not enabled in the image
([Claude's variable reference](https://code.claude.com/docs/en/env-vars#features-that-need-feature-flag-fetching)).
Node's compile cache is inapplicable to the native/Bun agent executable.
No runtime tuning flags were kept.

The socket cohort also passed `lab/check.sh t3 claude`. Its first fast gate
flagged `paths.adopt_ms` at 139 ms versus the 75 ms baseline; wake, streaming
and Git captures were also slower in that run. The adoption implementation
is unchanged. The baseline cohort's fast gate passed at 85 ms. No margin or
baseline was changed for the repeat.
The second single-sample repeat also flagged adoption (170 ms), wake p95
(1,466.8 ms) and sleep p95 (2,815.5 ms). Race tests, vet and lint passed on
the final Go source. The final control against the committed CLI baseline
and three-sample candidate gate never acquired the shared lab before the
time box ended. The queued control was canceled before it ran. The full
fast gate remains unresolved; its failures are not reported as a pass.

## WAKE beyond the volume wait: floor, 2026-10-25

Base `fc7dd98b`, ARM64 Docker Desktop lab, gVisor 20261012. Builds and
measurements held the same shared `lab_lock`. No VPS or perf-baseline edits.
The unchanged three-run gate (`WAKES=10 REPEAT=3 lab/perf.sh box golden`)
measured wake p50/p95 **742.4/930.7 ms**, golden first wake **745.1 ms**, cold
first wake **1,900.1 ms**, and box anon + shmem **47.6 MiB**. All correctness
checks passed; the unchanged p95 exceeded the existing gate's 25% margin.

### The zero-volume floor

Thirty disposable pods, ten per case, alternating 0/1/3 then 3/1/0 volumes.
Each used the real fern-init entrypoint without services, `fern-box:dev`,
gVisor, a read-only root, uid/gid 1000, the shared PID namespace, runtime
seccomp, CPU limit 2 and memory limit 2 GiB. Each explicitly set
`automountServiceAccountToken: false` and `enableServiceLinks: false`.
The returned pod specs confirmed exactly 0, 1 or 3 volumes; the nonzero
controls used emptyDir volumes mounted at distinct paths.

| Median, ms | 0 volumes | 1 volume | 3 volumes |
|---|---:|---:|---:|
| Scheduled → RunPodSandbox begins | 9.5 | 310.1 | 309.7 |
| WaitForAttachAndMount | 0 | 301.0 | 300.7 |
| Host request → fern-init startup log | 382.7 | 675.7 | 667.2 |

The last row includes the host's Docker/kubectl request overhead. The first
row correlates scheduler and CRI timestamps. With zero volumes the volume
manager returns before logging a wait; with either one or three, the first
wait consumes one 300 ms retry. More volumes do not add another cycle.

Today's regular box has **three volumes**: emptyDir `/tmp`, CSI workspace,
and CSI Claude state. A T3 worktree has **four**, adding the read-only git
folder; its read-only `.git` subPath is another mount of the workspace
volume, not another volume. A golden builder has **two**, tmp and workspace.
There is no projected service-account token, ConfigMap or Secret volume;
Claude's login comes through an environment variable backed by a Secret.

**No volume merger retained.** Going from three volumes to one did not
reduce the wait. A production box still needs its CSI workspace and the
separate, unsnapshotted Claude state; bypassing CSI or copying state into the
container would break architecture §§10/13/14. The zero-volume pod is a
control, not a replacement box design.

### Ten initialized Postgres wakes

A separate warm trace matched pod UIDs across daemon, CSI, kubelet and CRI
logs. Kubelet logging verbosity was temporarily raised for this diagnostic
and restored before releasing the lock. Wake mean/p50: **713.4/690.3 ms**.

| Component | Mean ms |
|---|---:|
| Before the volume wait, including Sandbox API and scheduling | 17.3 |
| WaitForAttachAndMount | 301.0 |
| RunPodSandbox | 103.3 |
| CreateContainer | 7.7 |
| StartContainer | 79.1 |
| After StartContainer until Fern answers ready | 198.0 |
| Other gaps between calls | 7.1 |

The last CSI publish completed **123.2 ms** before kubelet noticed it
(median **122.4 ms**). The separate attach-verification → mount
reconciliation pass remains visible about 100 ms apart.

Within service startup, fern-init's startup log → Postgres's first log took
**82.1 ms**, Postgres's first log → accepting connections **62.1 ms**, and
accepting connections → Fern's wake response **62.1 ms**, all means. These
span a few milliseconds before StartContainer returns, so they do not sum
exactly to the after-StartContainer row. Readiness is already a direct long
HTTP request to fern-init; it executes service checks every 100 ms. Fern
polls for a new pod address every 200 ms and does not wait for Kubernetes'
Ready condition. The remaining service-side candidates are the sample's
process wrappers and the initial readiness retry cadence.

### Runtime flag trials

Each candidate used the unchanged Go code and sample, a build and
`WAKES=10 REPEAT=3 lab/perf.sh box golden` in one lock. The installed runsc
was temporarily invoked through an exec wrapper with the selected flag;
the original executable was restored before releasing the lock. Values are
medians across the three runs.

| Metric | Unchanged A | `directfs=false` | `host-settings=ignore` | `cpu-num-from-quota=false` | Unchanged crossover |
|---|---:|---:|---:|---:|---:|
| Warm wake p50, ms | 742.4 | 761.7 | 746.9 | 698.6 | 761.4 |
| Warm wake p95, ms | 930.7 | 889.1 | 927.0 | 849.1 | 983.3 |
| Box anon + shmem, MiB | 47.6 | 49.6 | 47.4 | 49.4 | 47.0 |
| Golden first wake, ms | 745.1 | 875.2 | 1,364.4 | 709.1 | 895.5 |
| Cold first wake, ms | 1,900.1 | 3,568.9 | 4,776.0 | 2,033.2 | 2,014.5 |

**None retained.** DirectFS-off and ignoring host settings did not improve
warm wakes and failed the cold-start gate. The CPU-count flag passed the
gate but increased box memory by 1.8 MiB against A and 2.4 MiB against the
unchanged crossover. Over thirty warm pods per flag, median
RunPodSandbox/StartContainer were **102.9/104.0**, **119.9/82.0**, and
**109.7/76.3 ms**, respectively, versus **101.4/73.6 ms** in the unchanged
ten-wake trace. The unchanged crossover through the same wrapper measured
**122.7/83.0 ms**; the CPU-count candidate lies between these unchanged
batches. The crossover also failed the wake and sleep p95 gates. These
sequential batches show why a small end-to-end median change alone is
insufficient evidence for a runtime setting.

### Services and readiness: individual trials

Each standard trial again used a build and `WAKES=10 REPEAT=3 lab/perf.sh
box golden` under one lock. Candidate source files were restored before
releasing the lock, keeping the trials independent. The daemon retry trial
changed connection retries from 50 to 10 ms while leaving pod-address
lookup at 200 ms; it was not the earlier rejected 50 ms pod-lookup trial.

| Metric | Unchanged A | Unchanged crossover | Postgres script | Early service checks | Daemon connection retry |
|---|---:|---:|---:|---:|---:|
| Wake p50, ms | 742.4 | 761.4 | 723.0 | 744.3 | 798.6 |
| Wake p95, ms | 930.7 | 983.3 | 895.8 | 1,081.8 | 916.4 |
| Box anon + shmem, MiB | 47.6 | 47.0 | 46.8 | 47.4 | 47.5 |
| Golden first wake, ms | 745.1 | 895.5 | 846.6 | 782.5 | 867.1 |
| Cold first wake, ms | 1,900.1 | 2,014.5 | 1,941.4 | 1,865.5 | 2,318.8 |
| Sleep p95, ms | 1,218.7 | 1,617.0 | 2,615.5 | 2,611.2 | 1,214.7 |

**Neither retry change retained on its own.** Service checks used 25 ms
retries for the first 200 ms, then the existing 100 ms pace. This did not
improve warm wake against A, and its wake tail worsened. The 10 ms daemon
connection retry passed the gate but was slower than both unchanged runs.
Passing the gate alone is not an improvement.

The Postgres script trial skipped `rm` when there was no stale pid file,
used `exec` for an already seeded Postgres, and used an explicit `exec` in
the service's run command. Cold initialization and seeding stayed intact.
Its apparent unpaired saving needed confirmation, especially given the
sleep-tail change.

A same-lock confirmation alternated A/B then B/A for ten pairs, on the
same initialized environment. Only the script and services.yaml changed,
while the box was asleep. Each wake was followed by a timed sleep. The
installed runtime was release-20261012.0 with the default two-line runsc
runtime configuration.

| Paired-run medians | Original A | Script B |
|---|---:|---:|
| Wake, ms | 717.7 | 745.1 |
| Sleep, ms | 811.3 | 812.7 |
| fern-init → first Postgres log, ms | 78.4 | 86.9 |
| fern-init → Postgres accepting connections, ms | 139.7 | 158.0 |

**Script change rejected on its own.** Median within-pair B-minus-A wake
was **+30.7 ms**; B was slower in seven of ten pairs. It did not shorten
service startup in this confirmation. The sleep medians were unchanged,
so the earlier unpaired sleep-tail increase is not established as a script
regression either. The unpaired 761.4 → 723.0 ms result is not retained as
a performance claim.

### Combined candidate and unchanged validation

Combining the warm-script change with the bounded 25 ms service retries
passed the three-run gate: wake p50/p95 **714.7/787.0 ms**, box anon + shmem
**46.9 MiB**, golden first wake **749.5 ms**, cold first wake **2,147.7 ms**,
and sleep p95 **1,417.4 ms**. This is a 27.7 ms p50 change against A and
46.7 ms against the unchanged crossover. It is not independently a claim
that either of the rejected standalone changes helps.

The unchanged code then passed `lab/go.sh test -race ./...`, `lab/lint.sh`
(0 issues), `lab/go.sh vet ./...`, a build, and
`WAKES=10 JOBS=1 lab/check.sh box golden restore`, under one lock. A fresh
ten-wake trace on this unchanged code illustrates the between-batch spread:

| Mean ms | Early unchanged trace | Later unchanged trace |
|---|---:|---:|
| Whole wake | 713.4 | 813.2 |
| Before the volume wait | 17.3 | 22.9 |
| WaitForAttachAndMount | 301.0 | 301.3 |
| RunPodSandbox | 103.3 | 153.1 |
| CreateContainer | 7.7 | 11.3 |
| StartContainer | 79.1 | 89.3 |
| After StartContainer until ready | 198.0 | 226.7 |
| Other gaps | 7.1 | 8.6 |

The later trace's p50 was **833.4 ms**. Its service subinterval means were
**97.3 ms** to the first Postgres log, **70.2 ms** until Postgres accepted
connections, and **64.1 ms** until Fern answered ready. The volume wait is
stable; runtime and process startup varied substantially on unchanged code.

**Final disposition: no production optimization retained.** The combined
candidate's 27.7–46.7 ms median shift is smaller than the observed
unchanged-code spread, and the script's apparent standalone saving failed
the alternating confirmation. The combined candidate's additional full
validation and phase-trace run never acquired the shared lab lock before
the timebox ended. Its own waiting lock helper was cancelled before any
source or deployment change. Do not treat 714.7 ms as a confirmed gain.

Only these measurement notes remain on the branch. Production Go, the
sample scripts, node settings and `lab/perf/*.tsv` are unchanged. The
unchanged code passed the required box/golden/restore correctness checks,
race tests, lint and vet; its timing-gate tail failures are recorded above.
No VPS access, lab restart, full suite, push or merge was performed.

The substantial remaining target is kubelet's mount scheduling and waiter
notification: one volume already incurs the same approximately 301 ms
wait as three, and publishes finish approximately 123 ms before the waiter
notices them. There is no measured reason here to add a Fern volume merger
or retain more aggressive polling.

## VPS sleep: PLEG cache waits, 2026-10-25

CAX21, the pinned k3s `v1.36.5+k3s1` and gVisor `20261012`. Unchanged
`fc7dd98b`, deployed before measuring: `WAKES=10 REPEAT=3 lab/perf.sh box t3`
gave sleep p50/p95 **2,615.2/2,819.5 ms**, wake **1,064.9/1,171.5 ms**,
and T3 restore **837.0 ms**. All metrics passed the existing VPS gate.
After reverting both configuration experiments below, a further ten cycles
gave sleep **2,614.7/2,815.8 ms**, wake **1,079.4/1,131.9 ms**.

**Cause:** kubelet's pod worker waits for PLEG's status cache to be newer
than its last sync, even before termination. A pending ordinary sync can
wait first, followed by another wait before termination, followed by one
before cleanup. The [pinned pod worker](https://github.com/kubernetes/kubernetes/blob/v1.36.5/pkg/kubelet/pod_workers.go#L1268-L1288)
calls `GetNewerThan`; the [generic PLEG period](https://github.com/kubernetes/kubernetes/blob/v1.36.5/pkg/kubelet/kubelet.go#L207-L216)
is one second. This is not a two-second container grace period.

One traced VPS sleep took **2,614.2 ms**. Pod
`7c4e8707-eff4-4aea-8fd7-181ec6b0bee4`, UTC:

- **20:00:51.394009:** kubelet receives graceful deletion while an ordinary
  sync is already waiting for fresh status.
- **20:00:51.948529:** that sync has finished; the terminating event starts
  processing (**554.5 ms** after deletion).
- **20:00:52.950373:** `SyncTerminatingPod` finally enters (**1,001.8 ms**
  more in `GetNewerThan`). StopContainer follows immediately.
- **20:00:53.479724:** container and sandbox stop complete (**529.4 ms**).
- **20:00:53.955824:** `SyncTerminatedPod` enters (**476.1 ms** more waiting
  for fresh status). Volumes are already unmounted.
- **20:00:53.964566:** cleanup finishes (**8.7 ms**); pod deletion and
  Fern's existing 200 ms observation interval account for the remainder.

These timestamps come from kubelet verbosity 4, enabled temporarily through
its authenticated `/debug/flags/v` endpoint, plus containerd's CRI logs.
The unchanged gates ran with verbosity restored to zero. A separate 20-cycle
node-local diagnostic produced sleep **1,015/2,415 ms**: the slow path
depends on where wake and the following request land in the PLEG cycle.
The earlier lab trace (183 ms containers + 113 ms sandbox + 750 ms cleanup)
does not have the extra pre-stop waits. Faster startup and different
request timing can avoid them; the fixed timer belongs to kubelet, not
Fern's service stop commands.

The live lab's k3s config is byte-identical to `deploy/k3s-config.yaml`.
Both nodes' `configz` show no feature-gate overrides, the same 1 minute
ordinary sync frequency, 2 minute runtime timeout, 100 ms CPU quota period
and API QPS of 50. The cgroup drivers differ (`cgroupfs` in the lab,
`systemd` on the VPS), but the traced VPS cleanup above took only 8.7 ms
after the cache wait. The excess time precedes CRI calls and is not cgroup
destruction or a different configured grace period.

**Rejected host changes, same Fern images:**

- `EventedPLEG=true`: ten cycles, sleep **13,615.5/13,816.0 ms**, wake
  **1,074.0/1,116.6 ms**, T3 restore **6,836.0 ms**. The event stream
  connected with zero errors, but the evented cache has a five-second
  fallback refresh. A brief diagnostic log change overlapped its first
  cycle; the nine subsequent samples at verbosity zero were 13.4–13.8 s.
  The gate failed; the flag was removed.
- `PLEGOnDemandRelist=false`: ten cycles, sleep **2,815.2/3,017.2 ms**,
  wake **1,824.6/1,966.8 ms**, T3 restore **1,433.5 ms**. Wake regressed;
  the flag was removed. Both experiments applied `deploy/k3s-config.yaml`
  and restarted k3s; the original configuration was restored afterward.

An initial measurement overlapping the first image transfer was discarded:
the saturated SSH connection delayed requests enough to change which PLEG
waits they hit. Both host comparisons and the unchanged gates ran after
the transfer finished; the traced node-local run only attributes the waits.

A later local validation wrapper was also discarded: its nested lock
handoff reopened fd 9 for the VPS lock, releasing the inherited local lock.
Its baseline build overlapped another lab build around 21:45–21:47 UTC.
The rerun holds one local lock for the whole paired comparison and checks
its owner and fd before building. None of the overlapping lab results count.

**Kept: start graceful shutdown after durable stop intent.** The driver
first suspends or deletes the Sandbox, then sends an authenticated,
pod-UID-bound request to fern-init. Services can finish their normal
shutdown while kubelet waits for status. The existing pod/mount fence
still determines completion. Old or unreachable init falls back to
kubelet; no kubelet or gVisor configuration change is retained.

Final consecutive A/B comparisons used unchanged `fc7dd98b` and this
change, each with `WAKES=10 REPEAT=3 lab/perf.sh box t3`. One local
lock covered the entire window; both nodes used the same settings
throughout their pair. Timing excludes image transfers.

| Metric (ms, except memory in MiB) | VPS A | VPS B | Lab A | Lab B |
|---|---:|---:|---:|---:|
| `box.box_memory_mib` | 47.2 | 47.2 | 47.3 | 46.9 |
| `box.daemon_memory_mib` | 13.8 | 14.6 | 14.5 | 14.3 |
| `box.force_delete_ms` | 1323.0 | 1443.0 | 1188.0 | 1145.0 |
| `box.sleep_ms.p50` | 2614.0 | 1622.6 | 1014.5 | 1017.7 |
| `box.sleep_ms.p95` | 2815.9 | 1824.0 | 1612.3 | 1814.3 |
| `box.wake_ms.p50` | 1018.5 | 1090.0 | 743.1 | 766.6 |
| `box.wake_ms.p95` | 1131.2 | 1140.4 | 921.1 | 1066.6 |
| `t3.adopt_ms` | 309.0 | 294.0 | 208.0 | 210.0 |
| `t3.checkpoint_ms` | 10.6 | 10.3 | 20.0 | 20.3 |
| `t3.checkpoint_replaced_ms` | 14.7 | 15.9 | 13.4 | 9.9 |
| `t3.restore_ms` | 1029.5 | 1037.1 | 1026.2 | 1025.1 |

VPS sleep p50 improved **38%**;
the VPS gate passed. Other paired changes stayed within the existing
target margins and absolute floors (25 ms / 8 MiB). No baseline file
was edited. The saved lab gate flagged sleep p95 in unchanged A and
sleep/wake p95 in B; their paired changes stayed within its margins.
Lab sleep median was unchanged within noise.

**Correctness:** race tests for all Go packages, vet and lint passed,
including replay after interruptions at both new stop boundaries,
request authorization, stale pod UIDs, retry and old-init fallback.
Lab `box restore golden t3` and VPS `box restore t3` passed: mounts
are gone after sleep, Postgres data survives, and restores fence execs.
The committed code is redeployed to the VPS after these checks.

## Second round, merged, 2026-10-26

Five more agents (sleep on the VPS, restore without a stop, wake past the volume wait, box memory, a T3 turn's Fern share), merged into iter-1 at `546eb7d4`; `lab/perf.sh` passed in the lab and on the VPS, and both baselines were recorded again (5 runs).

| VPS, baseline medians | before | after |
|---|---|---|
| sleep p50 / p95 | 2,615 / 2,817 ms | 1,621 / 1,822 ms |
| node memory per box, 20 awake | 89.8 MiB | 80.0 MiB |
| a box's memory (pod cgroup, sample) | 46.7 MiB | 39.5 MiB |
| wake | 1,035 ms | 1,018 ms |
| `t3.restore_ms` | 832 ms | 1,039 ms (worse; being looked into) |

- **Sleep:** kubelet waited for its one-second PLEG cache before it stopped the container; the driver now asks fern-init for its graceful shutdown right after the Sandbox's suspension is stored (`POST /stop`, 250 ms budget, the pod's UID in `If-Match`), and still waits for the pod and its mounts to go. Architecture §10 says how.
- **Memory:** the sample's Postgres has 20 connection slots, 512 KiB WAL buffers and no logical replication launcher; its 128 MiB cache stays. See `lab/BOX-MEMORY.md`.
- **A turn:** the checkpoint hook talks to the daemon's socket itself, 18 → 13 ms. Claude starts in 694 ms in a box (542 outside); Fern takes no checkpoint per tool call.
- **Not built:** a restore under a running box (the CSI bind keeps the old subvolume; with the parent mounted, open files stay old; `lab/LIVE-RESTORE.md`), and a wake without volumes (the volume wait is 300 ms of a wake, 10 ms with none; it needs another way to bring the workspace in).

## VPS restore: reserve the head start for services, 2026-10-26

Base `59dfdff2`, CAX21, the existing k3s/gVisor configuration. The T3
worktree has no services (`fern-init started services=0`), so the sample
Postgres settings in `2cb57c62` do not run in it. Between `fc7dd98b` and
`546eb7d4`, the actor/store restore path is unchanged; `dc912be3` adds the
stop request to the driver.

A fresh unchanged `REPEAT=3 WAKES=10 lab/perf.sh t3 box` passed with restore
**633.9 ms** and sleep **1,621.2 ms**. Its daemon restore durations were
1,233.4 / 632.7 / 436.2 ms; this small cohort alone does not reproduce
the saved 1,039 ms median. Do not interpret a single three-run median as a
repair: kubelet's periodic cleanup and Fern's 200 ms observation interval
make this metric strongly phase-dependent.

Two alternating cohorts then isolated `stopInit`, using one diagnostic
binary and a temporary node-local switch that skipped the pod lookup and
HTTP request. Each cohort ran six pairs of the complete T3 check, reversing
the order every pair and adding a random 0–999 ms pause before each check.
No image transfer or other check overlapped these measurements. All checks
passed. Across twelve restores per side, nearest-rank medians were:

| `stopInit` | Restore p50, ms | Mean, ms |
|---|---:|---:|
| early stop enabled | 1,033.9 | 1,053.1 |
| early stop disabled | 833.2 | 966.5 |

The request is authorized and UID-matched: traced replies were HTTP 204 in
roughly 3–9 ms, not a lost 250 ms timeout. One slow restore, UTC, pod
`6b0455e4-364a-431a-839b-00b34eef8cdc`:

- **00:44:53.650:** suspension persisted, 12.3 ms after entering Sleep.
- **53.654:** pod lookup and `/stop` complete, 4.5 ms total; init starts
  stopping. There are no services to wait for.
- **53.659–53.976:** kubelet's `SyncTerminatingPod`; CRI StopContainer takes
  134 ms and StopPodSandbox 168 ms.
- **54.154:** the workspace, git and Claude CSI volumes are unmounted.
- **54.712:** PLEG reports the container finished; `SyncTerminatedPod`
  enters after another 736 ms waiting for fresh cache status.
- **54.721:** cleanup finishes; **54.857:** Fern observes the pod gone;
  **54.873:** restore finishes. Driver stop is 1,220 ms, restore 1,236 ms;
  only about 16 ms is outside the stop. The restore histogram records
  1.235567 seconds. The whole check records three stops, 4.663880 seconds.

The same trace also caught a restart in the final delete: pod
`05e6dcb4-d0da-410e-b855-9039ba3da465` exits at **00:45:01.635826**, kubelet
starts a second box container at **01.943649**, then enters
`SyncTerminatingPod` at **01.943745**. Its final status has restartCount 1.
Persisting Sandbox intent does not mean kubelet has observed pod deletion;
exiting init in that gap races ordinary pod reconciliation. Not every
slow restore restarts: the trace above also shows the independent PLEG
cleanup wait.

A broader candidate kept init alive after draining in every box. Its local
checks and perf gate passed (restore 834.5 ms, sleep 1,015.3 ms), but it was
rejected on the VPS: restore 636.5 ms, sleep **2,618.7 ms**, sleep p95
2,820.9 ms. Keeping a service box's init alive lost the full sleep win.

A narrower candidate limited the head start to boxes with services. Without services,
init still authenticates the token and pod UID and acknowledges `/stop`, but
does not cancel its context; kubelet's SIGTERM performs the normal shutdown.
There is no service work to overlap in that case. Boxes with services keep
the original early shutdown, including process exit. The durable Sandbox
mutation, kubelet fallback and pod/mount fence are unchanged. No new disk
state needs reconciliation.

Handler tests cover repeated acknowledgement without cancellation for an
empty box, graceful cancellation for a service box, and token/UID rejection
in both cases. The tests wait for the handler to return, after the flushed
acknowledgement and deferred cancellation, before checking its effect.
Removing only the service guard makes the empty-box case fail.

The scoped candidate passed `lab/go.sh test -race ./...`, `lab/go.sh vet ./...`
and `lab/lint.sh` (zero issues), and the Go code review. Local
`lab/build.sh t3`, `lab/check.sh box restore t3` and
`REPEAT=3 lab/perf.sh t3 box` pass. Fresh before/candidate lab medians:
restore **836.7 → 643.5 ms**, sleep **1,013.6 → 1,014.7 ms**, wake
**723.3 → 714.2 ms**. The before run used ten box cycles per repeat and
the candidate run the default twenty. The restore comparison remains subject
to the phase variation described above.

The scoped candidate also passed the requested VPS three-repeat gate:
restore **837.6 ms**, sleep **1,621.4 ms** (p95 1,824.1 ms), wake
1,007.6 ms. VPS `box restore t3` correctness passed, and a follow-up trace
showed restartCount 0. These results alone would have looked like a fix.

However, a final image-only A/B did **not** validate it. Two balanced cohorts
of six pairs alternated the original and scoped init images under the same
unmodified daemon, with a random 0–999 ms pause before each T3 check. Each
sample checked the CRI image ID after retagging; all checks passed. Twelve
restores per image gave:

| Init image | Restore p50, ms | Mean, ms |
|---|---:|---:|
| original | 837.8 | 854.6 |
| scoped candidate | 1,032.1 | 1,021.5 |

The scoped candidate is therefore **rejected**, despite its passing gates.
Both candidates' Go changes and the architecture edit are reverted. The
original init image is restored on the VPS; no performance baseline is
edited. Only this investigation is committed.

**Conclusion:** there is a verified premature-exit/restart race, observed
in delete, but the evidence does not establish that it caused the whole
reported restore median shift. PLEG cache waits and the 200 ms observer
make small cohorts misleading. No verified restore latency fix was found
within the 90-minute time box. Preserve the sleep win and investigate the
cleanup/cache phase with longer paired measurements before changing stop
semantics again.

After reverting, race tests and lint pass again, and the restored VPS
passes `lab/check.sh box restore t3`. The local lab is also rebuilt from
the unchanged Go source. Temporary image aliases and the diagnostic marker
are removed, and kubelet verbosity is returned to zero.

## CPU, disk and platform perf gate, 2026-10-26

Final sampler `6f51f56e`, timing isolation `05f84f96`, based on `a23cefdd`.
The lab figures replace the provisional ENOSPC cohort. Both targets completed
three full default-gate repeats, a separate 50-cycle stability check and two
toy runs (six Haiku turns). A subsequent three-repeat `box hardware` gate
verified the timing-isolation fixes. Box CPU, idle and platform values use
that final cohort; unchanged golden/T3/scale costs use the earlier full gate. Fresh/golden
and T3 operations have three observations; warm CPU costs have nine, and
legacy box timings 60. Claude figures summarize six turns per target. Lab
builds and measurement commands used `lab_lock`; observed host free space stayed at least 87 GiB.

Only lab scripts and reports changed: no Go, image inputs, deployment or
runtime settings. The local build/deploy succeeded. The VPS image import
stalled with no live remote import process; only our own transfer processes
were stopped. The VPS kept iter-1's running deployment. No runtime image
change is required for these collectors.

| Operation: CPU-ms, node / box / daemon pod | VPS | Lab |
|---|---:|---:|
| Fresh wake, including initdb | 5,570 / 4,000 / 17.7 | 3,220 / 1,966 / 20.8 |
| Golden wake | 1,620 / 700 / 19.6 | 1,270 / 412 / 21.6 |
| Warm wake | 1,630 / 671 / 18.8 | 870 / 351 / 17.0 |
| Sleep | 1,280 / — / 23.0 | 720 / — / 13.3 |
| Restore, awake T3 workspace | 980 / — / 23.0 | 530 / — / 16.8 |
| T3 checkpoint | 30 / — / 5.6 | 20 / — / 2.6 |
| Postgres checkpoint, 1 MiB changed | 30 / — / 6.2 | 10 / — / 3.1 |

Cgroups use `usage_usec`, including Sentry/gofers for boxes. Box wake CPU
is its pod's lifetime counter read after readiness. `/proc/stat` has 10 ms
resolution; node totals include platform/observer work and cover the whole
Docker VM in the lab. Dying box cgroups have no final CPU sample.

Idle box CPU, VPS / lab: **518 / 622
CPU-ms/min with Postgres**, **108 / 94
without services**, from 15-second windows. Daemon CPU during the 20-box
exec/checkpoint workload was **2,423 / 3,548
CPU-ms/min**.

| Quiet platform: MiB anon(+shmem for pods) / CPU-ms per minute | VPS | Lab |
|---|---:|---:|
| k3s | 450.7 / 4,373 | 489.8 / 4,683 |
| containerd | 55.6 / 1,654 | 61.5 / 1,190 |
| agent-sandbox controller pod | 19.3 / 76 | 27.6 / 124 |
| T3 pod | 284.2 / 437 | 368.5 / 1,014 |
| Daemon pod, registrar included | 20.4 / 3 | 29.3 / 11 |

Full k3s/containerd RSS was **590.7/167.1 MiB** VPS,
**594.1/116.7 MiB** lab. These short quiet windows follow the box check's cleanup;
normal platform background work remains included. Pod kernel charges are
separate metrics.

**Claude:** service-only toy boxes used **38.2 / 37.9 MiB**.
Active pod peaks were **178–222 / 176–208 MiB**;
median increments **155 / 159 MiB** include Claude, tools and
runtime growth. Median turn CPU, box/T3/daemon, was
**7,096/2,246/118 ms** versus
**3,205/1,089/50 ms**; sampled Claude-tree CPU lower bounds were
**3,815 / 1,765 ms**. Guest residency estimates and limits are in
[BOX-MEMORY.md](BOX-MEMORY.md#claude-process-accounting-2026-10-26).

**Disk:** both targets measured live data **38.4 MiB**, golden
**38.5 MiB**, latest snapshot **39.4 MiB referenced, zero exclusive**
(shared with live). Live plus five checkpoints used **43.4 MiB**, versus
**236.5 MiB** summed references. Replacing 1 MiB/checkpoint grew allocated
data **1.000 / 1.000 MiB**. Accounting now flushes the filesystem before
both endpoints: unflushed initdb allocations previously inflated apparent
checkpoint growth. `btrfs filesystem du` excludes metadata. Charged
fresh wake writes were **16.77 / 16.73 MiB**, golden wake
**0.38 / 0.37 MiB** (pairs VPS/lab); checkpoint daemon writes **0.49 / 0.41 MiB**.
Request-window `io.stat` includes delayed-writeback/stacked-device effects;
these are not physical device-write totals.

| Retained delta: daemon RSS / node used memory, MiB | VPS | Lab |
|---|---:|---:|
| 1 → 20 → 0 boxes, median of three ramps | +0.6 / -103.9 | +0.2 / +39.6 |
| 50 wake/sleep cycles | -1.5 / +27.4 | -0.3 / +18.1 |

After 50 cycles, descriptors were **14 → 14** on both targets;
goroutines **22 → 22 VPS / 20 → 20 lab**.
No pods/mounts remained. Short-term node-memory changes include background
platform activity; these windows do not establish long-duration retention.

**The 20-box flag: no collector penalty observed.** Three standalone VPS
pairs used the same Mac-side harness/SSH path, node code, `JOBS=1`,
`WAKES=10`, scale 1/5/10/20 and normal temporary filesystem. Only
`SCALE_HARDWARE=0/1` changed; order was off/on, on/off, off/on. The earlier
provisional gate had moved the harness/TMP onto the node, changing the
comparison with the saved baseline.

| Original sampler, 20-box metric (ms) | Collectors off: runs 1/2/3 | Collectors on: runs 1/2/3 | Median off / on |
|---|---:|---:|---:|
| All awake | 27,367 / 28,942 / 30,342 | 27,396 / 27,765 / 27,740 | 28,942 / 27,740 |
| Wake p95 | 27,234.8 / 28,747.6 / 30,192.1 | 27,261.2 / 27,649.1 / 27,614.9 | 28,747.6 / 27,614.9 |

One collector-off run already exceeded the old 15% limit. The prior
25,258 → 29,173 ms flag is consistent with drift/noise, not added sampling
load during wakes. Sampling is synchronous and outside timed operations.
The full default gate's completion/wake-p95 medians were
**27,727 / 27,627.0 ms VPS** and
**10,197 / 10,087.5 ms lab**, both passing.

The process census now uses shell reads instead of a `cat` process for every
`/proc` entry. Five idle VPS samples had median wall cost **1,342 → 220 ms**;
cgroup-only workload/T3 polls cost **166 ms**, including SSH. Census samples
remain at platform/stability endpoints. Per-command contention latencies use
a separate batch, so timestamps, result files and percentile formatting are
outside the legacy parallel-completion timer. Guest polling still adds
observer CPU, included in the reported totals.

**Gate coverage and verification:** final default sections are `box disk stream
paths golden t3 many scale hardware`. `hardware` took median **48.2 s lab /
58.3 s VPS**, including three CPU-only lifecycle cycles. Pod lookups and
counter reads stay outside the original wake/sleep timing loop. Quick/`--fast`
omit its idle windows. Contention p50/p95 uses a separate batch.
Large-workspace snapshot/restore CPU/I/O profiling is **all-only** or explicit
`DISK_HARDWARE=1`; the default `disk` uses its original direct commands.
Postgres checkpoint CPU/I/O stays in default `hardware`. `stability` took
**110 / 156 s**; its 50 cycles belong in `check.sh all` or an explicit perf
section, never the default gate. T3 toy profiling is explicit (`t3-run.sh`,
or perf's opt-in `E2E`), also outside the default gate.

Before the final timing-isolation fixes, the full lab gate **passed in
14m52s**. VPS functional checks passed, but its **26m59s full gate flagged
five legacy metrics** below. Matched original
`a23cefdd`/collector controls ran each complete section once, serially on the
same VPS before timing isolation; box used ten wakes in both. These single
pairs are controls, not replacement baselines.

| Legacy VPS metric, ms | Saved baseline | Gate median | Original-script control | Collector control |
|---|---:|---:|---:|---:|
| Sleep p50 | 1,620.9 | 2,018.5 | 1,623.1 | 2,020.4 |
| Sleep p95 | 1,822.5 | 2,220.5 | 1,821.3 | 2,226.2 |
| Seeded create | 25,560.0 | 32,842.0 | 29,954.0 | 38,010.0 |
| Golden build | 4,485.0 | 5,547.0 | 5,616.0 | 5,580.0 |
| 200 MiB stdout | 3,096.0 | 3,734.0 | 3,380.0 | 3,570.0 |

The matched sleep controls exposed a real observer bias: 1,623.1/1,821.3 ms
p50/p95 in the original loop versus 2,020.4/2,226.2 ms inline-instrumented.
CPU sampling now runs as three separate cycles in `hardware`, after its
idle/disk measurements. **Final three-repeat box/hardware verdicts: PASS lab / PASS VPS**.
Final sleep p50/p95: **1,621.5/1,823.8 ms VPS / 1,017.2/1,219.0 ms lab**.

Seeded-create controls also varied with observer cadence: original/on
29,954/38,010 ms, then reverse-order on/original 29,957/25,981 ms. Profiling
was moved out of the default disk sequence. Its final default-path functional
run passed at **28,988 ms**. Golden build controls both remained around
5.6 s; stdout controls were 3,380/3,570 ms. These controls do not clear the
historical seeded-create/golden/stdout baseline flags. A complete default gate
was not rerun after the isolation fixes within this 90-minute task; the
post-fix partial gates and disk check are the final validation. The 20-box
flag is resolved. CPU/disk/platform metrics report `new` until coordinator
baselines are accepted. Neither TSV nor any tolerance changed.
Race tests, vet, gofmt, Bash/POSIX syntax and
`koalaman/shellcheck:stable -x -P SCRIPTDIR` passed; functional, lifecycle and
both toy runs passed per target.

Still unmeasured: exact guest-process host residency; final dying-box CPU;
filesystem metadata/physical write amplification; statistically useful
contention p99; sustained network load; and retention over hours/days.
