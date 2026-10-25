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
