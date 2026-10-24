# Brief: iteration 2, ready for T3, then safe

**Branch:** `iter-1` (continues it), small commits, never push. **Written:** 2026-10-21, after the 20-agent review (iteration-1 report §7.2 and the review summary).

**Order (the user's decision, 2026-10-21):** close the gaps that block T3 first and spike the integration, then build the actor and the op log around the operations that turn out to exist. Build everything with performance designed in, and measure it.

## Part A: the gaps that block week 3

1. **Fix the review's bugs:** a repeat `create --repo` reported a failure; an interrupted `rm` leaked its disk; a TTY exec hung on a leftover in another process group; Create's lost-workspace guard had holes; the probe timed out (1 s) before the ready checks (2 s each); fern-init's SIGKILL reached only the leader; the kill test passed without `ps`; shutdown closed the index under running changes.
2. **Environments keyed by worktree path.** An environment has a `path`. Its live subvolume sits at that path, so T3, the daemon and the box all see the same path. By default the path stays `envs/<id>/live`; with `--path` it must be `<pool>/worktrees/<repo>/<name>`.
   - **Subvolume first** (fast path, ~10 ms): if the path doesn't exist, create an empty subvolume there; `git worktree add` can fill it.
   - **Adopt** (fallback, O(files)): if the path is a plain directory, reflink-copy it into a new subvolume and swap it in with `RENAME_EXCHANGE`.
   - Look an environment up by path: `GET /envs?path=…`.
3. **Snapshots for checkpoints:** delete a snapshot (`DELETE /envs/{id}/snapshots/{snap}`), and replace one that exists (`replace: true`), because T3 reuses checkpoint numbers after a revert.
4. **Exec for `fern-claude`:** the request carries a working directory (inside the workspace) and allowlisted environment variables; the client forwards SIGINT, SIGTERM and SIGHUP as signal frames instead of dropping the connection; `fern exec --here` finds the environment from the current directory. A `fern-claude` script runs `claude` in the box for paths in the pool and the local `claude` otherwise.
5. **Speed now:** wake polls fern-init's `/healthz` on the new pod directly (instead of waiting for kubelet's probe and the controller's Ready); `fern ls` makes one Kubernetes List; the probe's timeout fits the ready checks; `/healthz` caches its answer once ready; boxes share their process namespace, so the pause container reaps zombies.

**Done when:** unit tests, store tests and lint pass; the lab runs the demo and `blockers.sh` plus new checks for adopt, path lookup, snapshot delete/replace, exec with a directory, signals, and wake time (target ≤ 0.6 s warm on Docker Desktop, measured).

**Status (2026-10-21): done.** All of it is built and passes in the lab (`lab/RESULTS.md`). Warm wake p50 0.62 s, p95 0.71 s: just over the 0.6 s target. Go code grew by about 1,000 lines net, over the ~900 budget: the store's path, adopt and reconcile work, and the two builders' changes (fern-init readiness, the exec stream's pty and buffer work).

## Part B: the T3 spike (needs the user's Claude login)

T3's server in a pod with the pool and the daemon socket, the checkpoint hook in a patched build, one real turn through `fern-claude`, a revert that brings back Postgres. Rough is fine; the goal is to learn the real interface. Blocker research (8 agents, 2026-10-21) feeds this part.

**The patch (2026-10-22):** one new file wrapping T3's `CheckpointStore` (capture → `fern snapshot --replace`, restore → `fern restore` before T3's git restore, delete → `fern rmsnap`), plus one line in `server.ts`. The Claude program is a T3 setting and MCP goes through fern-init, so neither needs a patch. Users never apply it: it ships inside Fern's T3 image, and the Mac app pairs with that server. Keep notes on what the hook needed to carry; they shape the upstream proposal (architecture §15.1).

**Status (2026-10-22):** T3's server runs in the lab (`lab/build.sh t3`, `deploy/t3.yaml`). Every command the hook makes works from its pod on a worktree T3 made, and that worktree's box has git (the main repository's git folder, read-only), Claude's own folder outside `live/` (`envs/<id>/claude`, kept by sleep and restore) and the login from the `claude-login` Secret: with a fake token, Claude in the box reaches Anthropic and gets a 401 (`lab/check.sh t3`, `lab/RESULTS.md`). A failed Fern snapshot now leaves T3 its git checkpoint; reverting to it is refused with Fern's reason. A revert of an awake box takes 1.8–3.0 s, almost all of it stopping the pod.

**Status (2026-10-22, evening):** two review rounds for simplicity and Go practice are done. The store, driver, API, CLI, boxexec, fern-init and the hook got simpler (about 890 lines less Go), and the lab went from 18 scripts to 11 with one Dockerfile. From a fresh lab every check passes (`lab/check.sh`, then `lab/check.sh t3`), after one check that tested claude rather than Fern was replaced (`lab/RESULTS.md`).

**Status (2026-10-24): done.** In the T3 app on the Mac, with Claude on the user's subscription, "Revert files too" on a turn that deleted a user from Postgres brought the user back (`lab/RESULTS.md`). The steps that got there:
1. `claude setup-token` on the Mac, then `lab/claude-login.sh` and paste the token (hidden; it goes straight into the Secret).
2. `lab/t3-pair.sh`: it adds the sample project (`repos/sample`, Postgres with three users) and prints a pairing link; in the T3 app, Settings → Connections, add it.
3. In that connection's `sample` project, start a thread **in a new worktree**, ask Claude to count the users, then to delete one; revert that turn and check the user is back.

### What the blocker research settled (8 agents, 2026-10-21)

| Question | Answer | What it means for Part B |
|---|---|---|
| T3's server in a pod | Feasible: Node 24 (`node dist/bin.mjs serve`), `node:sqlite`, prebuilt `node-pty`; pair the Mac app with the printed link (Settings → Connections) | Build the image from T3's monorepo; run as uid 1000 (Claude refuses full access as root); use a NodePort, not 3773 on the Mac |
| Claude dies when the box stops | T3 resumes it with `--resume` on the next message, no T3 change; mid-turn it fails the turn | `fern-claude` must exit when the box goes; the transcript must survive outside `live/`; same cwd every time; idle sleep only between turns |
| Login | `claude setup-token`: a one-year, inference-only token for subscriptions, as `CLAUDE_CODE_OAUTH_TOKEN` | Token in a Secret, injected by fern-init, never through T3; a per-environment `CLAUDE_CONFIG_DIR` outside `live/` at the same path in T3 and the box (transcripts are keyed by cwd) |
| MCP tools | Streamable HTTP on T3's port at `/mcp`, URL always `127.0.0.1`, per-session bearer token sent over Claude's stdin | fern-init forwards `127.0.0.1:3773/mcp` to the T3 Service; egress NetworkPolicy allows boxes only DNS, the internet and T3:3773 |
| git in the box | A worktree's `.git` file points into the main repo; the box can rewrite it and get code run by T3's next host-side `git` (tested: `core.fsmonitor`) | Mount the main repo's git dir read-only at the same path; the T3 hook pins `GIT_DIR`/`GIT_WORK_TREE` and commits with hooks off; T3's own `git restore` runs after Fern's swap |
| Postgres snapshots | Crash-consistent by design (PGDATA and WAL in one subvolume) | Add a `lab/check.sh` section (pgbench under load, 60 snapshots, amcheck); run under gVisor too; refuse `fsync=off` |
| agent-sandbox | Keep, pinned to v1.0.7; pod name = Sandbox name is intended; suspend honours the grace period | Failed pods aren't replaced upstream (#729): the driver now deletes them |
| VPS | gVisor needs `--systemd-cgroup` on systemd hosts; lab scripts assume `docker exec`; amd64 images never built | Write `lab/vps-smoke.sh`; build amd64 images; one k3s config for lab and VPS |

## Part C: safe and sleepy (roadmap week 2)

The actor per environment with a generation number (restore cancels open execs), the op log for create, adopt, restore and delete with a startup reconcile, failpoint crash tests (E6-lite), E2 and E5, the idle timer, and `/metrics`.

**Status (2026-10-24): done, with one change of plan.**
- **Actor** (`internal/actor`): one goroutine per environment while its box may be running; changes one at a time; a restore, sleep or delete cancels the execs it counts, which fences stale work without a generation number.
- **Idle sleep:** 5 min after the last command ended and 20 min after it started (`--idle-agent`, `--idle-human`); boxes left awake by an earlier daemon get an actor at start.
- **No op log.** The store already left the disk showing how far each change got (markers, `live.new`, `adopt.new`), and `Reconcile` finishes or clears it; a log in SQLite would have been a second source of truth. Added instead: a restore keeps the replaced workspace as `pre-restore-<time>` for a day (taken before the swap), `Reconcile` matches snapshot rows to the disk both ways, and `TestCrash` stops every change at each of its 15 steps (E6, store part).
- **E2:** Postgres whole (pgbench's balances agree) and clean (`pg_amcheck`) after each of 60 restores of snapshots taken under write load (`lab/check.sh pgload`). **E5:** 2,000 changes from 50 goroutines, no overlap (`TestConcurrentChanges`).
- **`/metrics`** on the daemon's port 9090: wake, restore and snapshot latency, idle sleeps.
- **Review round** (architecture, Go, security, dead code, performance): fixed a box escape into T3's pod through the worktree's `.git` file, CSI mounts for any pod, Claude falling back to T3's pod, missing egress rules, and two actor bugs when a caller gives up (`lab/RESULTS.md`).

**From Amp's self-hosted orbs (research 2026-10-22):**
- **Idle timer with two clocks** (architecture §6.3): sleep only when no exec is open, 5 min have passed since the last exec closed, and 20 min since the last human touch. Test it with `testing/synctest`.
- **Keep the lifecycle safe to repeat** after the actor refactor: `TestLifecycleRepeats` (create, sleep and wake twice each, delete) must still pass, and the lab runs the same sequence against k3s.
- **A small manifest next to each snapshot** (env, snapshot ID, created, format version), so the startup reconcile can rebuild the index from disk and refuse a snapshot from an unknown format.
- **For Part B's `fern-claude`, check:** Claude's onboarding and trust prompts are already accepted in `CLAUDE_CONFIG_DIR`, its autoupdater is off (the image pins the version), and `ANTHROPIC_API_KEY` is unset whenever `CLAUDE_CODE_OAUTH_TOKEN` is set.

**From Zed's Delta (research 2026-10-22):**
- **Restore keeps the old workspace** as a read-only `pre-restore` snapshot for a day instead of deleting it (architecture §7.3), so a revert can be undone. The crash tests check it is never lost.
- **For Part B's hook:** a failed snapshot is logged and the turn keeps its git checkpoint; a restore without a snapshot is refused with the reason, before T3's git restore runs (architecture §7.2–7.3).
- **The hook's tests replay real `fern` output:** Go tests write each command's output and exit code to `testdata/`; the hook's fake `fern` answers from them and fails the test on a broken rule, such as a snapshot before `create`.

## Stop conditions

- Part A adds at most ~900 lines of Go (code, not tests). If it needs more, stop and say why.
- No new dependencies beyond AGENTS.md's list.
- Nothing from roadmap weeks 4+ (golden, forks, CRDs, quotas, user namespaces).
