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

**Status (2026-10-22):** T3's server runs in the lab (`lab/build-t3.sh`, `deploy/t3.yaml`) and every command the hook makes works from its pod on a worktree T3 made (`lab/blockers.sh t3`, 11 PASS, `lab/RESULTS.md`). Left before the real turn: Claude's folder per environment outside `live/` and the token in a Secret (code), then the token itself (the user). A revert of an awake box takes 2.2–3.0 s, almost all of it stopping the pod.

### What the blocker research settled (8 agents, 2026-10-21)

| Question | Answer | What it means for Part B |
|---|---|---|
| T3's server in a pod | Feasible: Node 24 (`node dist/bin.mjs serve`), `node:sqlite`, prebuilt `node-pty`; pair the Mac app with the printed link (Settings → Connections) | Build the image from T3's monorepo; run as uid 1000 (Claude refuses full access as root); use a NodePort, not 3773 on the Mac |
| Claude dies when the box stops | T3 resumes it with `--resume` on the next message, no T3 change; mid-turn it fails the turn | `fern-claude` must exit when the box goes; the transcript must survive outside `live/`; same cwd every time; idle sleep only between turns |
| Login | `claude setup-token`: a one-year, inference-only token for subscriptions, as `CLAUDE_CODE_OAUTH_TOKEN` | Token in a Secret, injected by fern-init, never through T3; a per-environment `CLAUDE_CONFIG_DIR` outside `live/` at the same path in T3 and the box (transcripts are keyed by cwd) |
| MCP tools | Streamable HTTP on T3's port at `/mcp`, URL always `127.0.0.1`, per-session bearer token sent over Claude's stdin | fern-init forwards `127.0.0.1:3773/mcp` to the T3 Service; egress NetworkPolicy allows boxes only DNS, the internet and T3:3773 |
| git in the box | A worktree's `.git` file points into the main repo; the box can rewrite it and get code run by T3's next host-side `git` (tested: `core.fsmonitor`) | Mount the main repo's git dir read-only at the same path; the T3 hook pins `GIT_DIR`/`GIT_WORK_TREE` and commits with hooks off; T3's own `git restore` runs after Fern's swap |
| Postgres snapshots | Crash-consistent by design (PGDATA and WAL in one subvolume) | Write `lab/e2.sh` (pgbench under load, 60 snapshots, amcheck); run under gVisor too; refuse `fsync=off` |
| agent-sandbox | Keep, pinned to v1.0.7; pod name = Sandbox name is intended; suspend honours the grace period | Failed pods aren't replaced upstream (#729): the driver now deletes them |
| VPS | gVisor needs `--systemd-cgroup` on systemd hosts; lab scripts assume `docker exec`; amd64 images never built | Write `lab/vps-smoke.sh`; build amd64 images; one k3s config for lab and VPS |

## Part C: safe and sleepy (roadmap week 2)

The actor per environment with a generation number (restore cancels open execs), the op log for create, adopt, restore and delete with a startup reconcile, failpoint crash tests (E6-lite), E2 and E5, the idle timer, and `/metrics`.

## Stop conditions

- Part A adds at most ~900 lines of Go (code, not tests). If it needs more, stop and say why.
- No new dependencies beyond AGENTS.md's list.
- Nothing from roadmap weeks 4+ (golden, forks, CRDs, quotas, user namespaces).
