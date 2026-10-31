# Fern v2 architecture

**Status:** what Fern is and how it works, as built on 2026-10-29, with what is planned marked so.
- Why, and the research behind it: [design.md](design.md).
- How each decision was made: [briefs/](briefs/). The first, Kubernetes-first design and its experiments are in [history/](history/).

---

## 1. What Fern is

Fern gives every AI coding thread its own **environment**: code, installed dependencies and database data on a copy-on-write disk.
- It starts from a **golden snapshot**.
- It runs in a **box**, a gVisor sandbox, only while something is happening.
- It **sleeps** when idle and **wakes** on the next touch.
- It takes a **snapshot at every agent turn**, so reverting a thread brings the data back along with the code.

Fern runs on **one remote Linux machine as two systemd services**: the Go daemon (`fern-daemon.service`), which owns the environments and runs their boxes itself with gVisor's `runsc`, and T3 Code's server (`fern-t3.service`). Claude runs in a box of its own per thread; its shell commands run in the box with the services ([briefs/claude-outside-the-box.md](briefs/claude-outside-the-box.md)). Your Mac or phone only runs the T3 app. Agents are the real `claude` / `codex` CLIs on your own subscription.

How it got here: the first design ran boxes as agent-sandbox `Sandbox` pods on Kubernetes with a CSI driver. The daemon took over running the boxes on 2026-10-27 ([briefs/boxes-without-kubernetes.md](briefs/boxes-without-kubernetes.md)), Kubernetes left the single-machine install on 2026-10-28 ([briefs/fern-without-kubernetes.md](briefs/fern-without-kubernetes.md)), and the Kubernetes mode was removed from the code on 2026-10-29.

### 1.1 In scope
- environments, snapshots, golden snapshots, boxes, sleep and wake, revert; fork (planned)
- the setup contract (`services.yaml`, `.agents/setup`, `.agents/prepare`)
- a local socket API, a CLI, Bash for an agent box over MCP, and a small hook in T3

### 1.2 Out of scope (non-goals)
- **Owning threads or chat.** T3 owns conversations and git; Fern owns disks and boxes.
- **Memory snapshots** (`runsc checkpoint`, CRIU). The promise is "stop and restart". Tried: restoring a checkpoint cost more CPU than a cold start and corrupted Postgres on a swapped workspace (`lab/spike/runsc/RESULTS.md`).
- **API-key model access,** and any proxy that intermediates subscription credentials.
- **A hosted multi-tenant service.** Fern is self-hosted, one login per person.
- **Kubernetes.** Removed; see §1.
- **Our own URL product.** Previews (planned) use portless or simple hostname routing.
- **Running on the Mac.** Fern runs on a remote Linux machine; the Mac only runs the T3 app.

## 2. Concepts

Three nouns. Everything else is a property of one of them.

| Noun | What it is | Lifetime | git analogy |
|---|---|---|---|
| **Snapshot** | a read-only, frozen copy of an environment's disk | never changes; deleted only by retention | commit |
| **Environment** | a name + a pointer to its latest state + a writable `live/` folder | as long as the thread | branch + working tree |
| **Session** (the "box") | a gVisor sandbox running on top of the workspace | only while working | running the checkout |

**Properties:**
- **Golden** = a snapshot tagged as the starting point for a repo.
- **Retention** = `persistent` (planned: `ephemeral`).
- **Class** (planned) = size and isolation (`light`, `heavy`, `bench`, `untrusted`, `gpu`). Today every box gets the same limits (§10).

**Who sees what:**
- **The user in T3:** threads, a preview URL, a sleep badge, Revert, Fork.
- **The repo:** the setup contract.
- **The agent:** a handful of tools.
- **Fern itself:** everything.

### 2.1 Identifiers
| Thing | ID | Example |
|---|---|---|
| Environment | the T3 thread ID (or any caller-chosen name) | `t3-8f2c` |
| Snapshot | the T3 checkpoint ref, or a generated ID | `7f3a…`, `golden/myrepo@<key>` |
| Box | the environment's ID, or `<env>.agent` for its agent box | `t3-8f2c.agent` |
| Golden builder | `golden-<repo>-<hash>`, from the repository's name | `golden-app-1a2b3c4d` |

---

## 3. Components (one job each)

```
                ┌──────────────────────────── fern-daemon (one per machine) ─────────────────────────┐
 T3 hook ──┐    │                                                                                     │
 CLI ──────┴──▶ │ api (unix socket) ──▶ actor (one per box) ──┬──▶ store (btrfs + SQLite index) │──▶ btrfs pool
 agent box ───▶ │ MCP Bash (port 7071)  mailbox · one change    └──▶ runsc driver                    │──▶ boxes
                │                       at a time; idle sleep                                          │
                │                       Reconcile ◀── finishes or clears a change a crash stopped      │
                └─────────────────────────────────────────────────────────────────────────────────────┘
 inside every box:  tini → fern-init → runs the services, serves exec (boxexec) and readiness
```

| Component | Job | Code |
|---|---|---|
| **api** | the daemon's request surface: JSON over HTTP on a unix socket; exec as a WebSocket relayed to the box; for agent boxes, an MCP server whose one tool, Bash, runs a command in the environment's box | `internal/api`, `internal/api/server` |
| **actor** | one goroutine per box while it may run: makes its environment's changes **one at a time**, counts the commands running in it, cancels them on restore, sleep and delete, and puts it to sleep when idle (§6.3). Pre-wakes (§6.4) and golden builds (§7.5) go through it too | `internal/actor` |
| **box policy** | decides what each box runs and gets: services or Claude, Claude's folder and login or not (`Group.boxOf`) | `internal/actor` |
| **store** | the btrfs pool: workspaces, snapshots, restore by swap, adoption of T3's worktrees, retention, and the SQLite index. `Reconcile` gives crash safety without a log: every multi-step change leaves the disk showing how far it got, and Reconcile finishes or clears it at start. Crash tests stop each change at every step | `internal/store` |
| **runsc driver** | wakes and stops boxes: a network namespace and veth per wake, an overlay root on the box image, an OCI bundle, `runsc create`/`start`; the nftables firewall; adopts boxes that outlived a daemon restart | `internal/runsc` |
| **setup** | reads the setup contract and computes golden keys | `internal/setup` |
| **image** | unpacks `docker save` streams into the pool, as box and T3 roots (`fern image load`) | `internal/image` |
| **watch** | while an agent box is awake, watches its workspace from the host with fanotify and keeps the writes of the agent box's processes (their cgroup names the box); the actor passes them to the environment's box as touches (§10) | `internal/watch` |
| **fern-init** | a box's main process: starts and supervises the services (on demand, §8), serves exec with PTYs (`internal/boxexec`), readiness, a graceful stop and touches | `cmd/fern-init` |
| **hibernate** | in the CLI that T3 runs as `claude`: stops an idle Claude so its box can sleep, and resumes it at the next message (§6.3) | `internal/hibernate` |
| **preview** | the front door for previews: serves boxes' ports over HTTP and WebSocket, routed by host, waking a box that is asleep and counting each request as a use of it (§5, §11) | `internal/preview` |
| identity, more MCP tools for agents | planned (§5, §11) | |

**Why an in-box agent (`fern-init`)?** Exec, services and readiness work the same however the box is run, without `docker exec`-style side channels, which are fragile for long streams. Its exec protocol is Fern's own: one WebSocket per command, with frames for stdin, stdout, stderr, terminal sizes and signals (`internal/boxexec`).

## 4. Data model (the index, SQLite)

The tables are in `internal/store/index/schema.sql`; [sqlc](https://sqlc.dev) generates the Go for the queries in `queries.sql`.

```sql
environments(id PRIMARY KEY, workspace UNIQUE, git, created_at, box_token)
snapshots(env REFERENCES environments ON DELETE CASCADE, id, created_at, PRIMARY KEY (env, id))
```

- **The disk is the truth; SQLite is the index.** At start the daemon **reconciles** what exists on disk with what the tables say (§12).
- `git` is the main repository's git folder that the box gets read-only (§13); `box_token` authenticates the daemon to the environment's boxes, and is why only root may read the index.
- **A restore fences stale work** by stopping the box and cancelling every command the actor counts, including one still connecting, so nothing that started before it runs after it.
- **Golden snapshots are rows too:** a golden is a snapshot of its builder environment named `golden-<key>`, and a build waiting at a restart is a snapshot named `golden-source` (§7.5).
- **Planned:** tags, a snapshot's parent and size, and timers as rows so idle clocks survive a restart (today a box still running at a restart gets an actor whose clocks start over).

## 5. API

Unix socket `/run/fern/fern.sock`, for root and the box user's group (T3's server runs as the box user), JSON over HTTP; exec is a WebSocket. Every call is safe to repeat; planned: an `Idempotency-Key` on every mutating call.

| Call | Meaning | Notes |
|---|---|---|
| `PUT /envs/{id}?path=P` | get-or-create an environment; a tar body fills its workspace | built; its box comes with the first wake. Planned: from golden, `{repo, class, retention, from?}` |
| `GET /envs` / `GET /envs?path=P` | list with box states / the environment whose workspace holds P | built; a lookup by path answers from the index alone (fern-claude does one per Claude start) |
| `GET /envs/{id}/exec` | run a command (optional PTY) over a WebSocket | built; wakes the environment if asleep; with `?then=sleep` the box sleeps as soon as the command ends, unless another runs (a hibernating Claude, §6.3) |
| `POST /envs/{id}/snapshots` | take a snapshot `{id, replace?}` | built; the T3 hook uses the checkpoint ref as the ID, and `replace` after a revert. Planned: `tags` |
| `GET /envs/{id}/snapshots` | list snapshots | built |
| `POST /envs/{id}/restore` | restore `{snapshot}` | built; stops the box, swaps the workspace; the box restarts lazily |
| `POST /envs/{id}/sleep` / `wake` | explicit lifecycle | built; planned: usually automatic |
| `POST /envs/{id}/prewake` | wake the box ahead of a likely command, in the background | built; answers at once (§6.4); the T3 hook calls it when a thread is opened or reverted |
| `DELETE /envs/{id}` | delete the environment, its box and snapshots | built |
| `DELETE /envs/{id}/snapshots/{s}` | prune | built; the T3 hook calls this when checkpoints are deleted |
| `POST /envs/{id}/fork` | new environment from `{snapshot}` `{newId, count?}` | planned; runs the identity hook; fork caps apply |
| `GET /events` | event stream (state changes, snapshots, wakes) | planned; for UIs and tests |
| `PUT /goldens/{repo}` | build repo's golden from a checkout, a tar body, unless it has one for it | built; T3 sends it when a project is added (`fern prebuild REPO`). Answers before the build |

**Other entrances (planned):**
- **Previews (built):** `http://<port>-<env>.<host>/…` is port `<port>` of the environment's box, with nothing to declare, and `http://<env>.<host>/…` its default preview, the port of the first service in `services.yaml` (by name) with a `port` (copied from Delta). `<host>` is `--preview-host`, `localhost` by default: a browser sends every name under `.localhost` to the machine itself, so no DNS is needed. The front door listens on `--preview-addr` (`:30780`); like T3's own port it is never public (the VPS's firewall lets in only SSH; reach it with `ssh -L 30780:127.0.0.1:30780`, as T3's), and boxes can't reach it. The repository isn't in the name, as planned: an environment's ID is unique on its node. A box learns its URL as `FERN_PREVIEW` (with `{port}`), which Claude's hint passes on, so Claude can hand out the link. Answers: 404 for a name that isn't a preview, an unknown environment or one without a default; 503 with `Retry-After` if the box doesn't wake; 502 saying that nothing listens on the port, after trying for 5 s (a dev server a wake starts listens a moment later).
- **Webhooks:** `POST /hooks/{env}/{token}` → wakes the environment and delivers the request.

**Errors:** an HTTP status (404 not found, 400 invalid, 500 failed) with `{"error": message}`. Planned with the actor and quotas: busy, conflict and quota exceeded; normal calls will queue in the actor's mailbox.

**For agent boxes (built):** an MCP server on port 7071, which only agent boxes can reach (§13), at `/mcp/{env}`, authenticated with the environment's box token. Its tool Bash runs a command in the environment's box (§10); with `run_in_background` the command goes on after the call, and BashOutput and KillShell read and stop it (§10).

**Agent tools (planned):** `env_snapshot`, `env_restore`, `env_fork`, `env_exec`, `env_delete` over MCP.

---

## 6. State machines

### 6.1 Environment
```
          create
            │
            ▼
   ┌──── asleep ◀──────────── stopping ◀─── idle timeout / sleep()
   │        │  touch (exec, preview, hook)        ▲
   │        ▼                                     │
   │     starting ──── box ready ────▶ running ───┘
   │        │                            │
   │        └──── start failed ──▶ failed (backoff, retry on next touch)
   │
   ├── restore / fork-source / golden-build steps run as ops (state: busy:<op>)
   └── delete ──▶ deleting ──▶ (gone)
```
- **Asleep** = disk only. **Running** = disk + session.
- **Only mutations queue** (snapshot, restore, fork, delete): they run one at a time. Running execs and golden builds are tracked separately; a restore **cancels** running execs (built), so a long agent turn never blocks the mailbox and a restore never interleaves with an exec.
- **Ephemeral environments:** on idle, `stopping` → discard `live/` (keep tagged snapshots) → `asleep (empty)`. The next touch forks from golden again.

### 6.2 Session
`waking (network, runsc create and start, fern-init up) → ready (fern-init's /readyz) → stopping (POST /stop: the services' stop commands, SIGTERM, then kill) → gone`

A box that is up but doesn't answer (a crash, one stopped by hand) is found by the next command, whose dial fails: the actor wakes it once more (built). Planned: restarts with backoff, and giving up after N failures in a window (OTP-style supervision).

### 6.3 Idle rule
An environment goes to sleep when all of these hold (two clocks, copied from Amp):
- **no exec is open** (a thread's Claude is one exec while T3 keeps its session), **no terminal is attached** and **no preview connection is active**
- **agent clock:** `idle.agent` (default 5 min) since the last exec closed
- **human clock:** `idle.human` (default 20 min) since the last human touch: an exec or terminal opened, or a preview request

CPU usage and running services are ignored. An explicit `hold` keeps it awake.

**Background commands (built)** are the exception to the first rule: one Claude starts with Bash's `run_in_background`, such as a dev server, is an exec the daemon holds open (`Group.Job`), but it doesn't hold the box awake as other execs do. With one running, the box sleeps on the two clocks, ignoring a Bash call's 5 s linger and a pre-wake's lease, and Claude's edits passed on to it (§10) count as touches of the human clock. That sleep ends the command: a dev server Claude forgot costs at most 20 minutes of an awake box.

Built: the actor's timer, with both clocks touched by every exec (`--idle-agent`, `--idle-human`; 0 turns it off), and previews: every request through the front door is an exec for as long as it lasts (`Group.Preview`), so a page whose dev server's live reload is connected keeps the box awake, and once it is closed the clocks run. Terminals and `hold` are planned.

**T3 keeps a thread's Claude running between turns,** in one exec, and ends it once the session has been idle (no turn, tool call, approval or background task). Unpatched T3 waits 30 minutes, so a box would stay awake about 35 minutes after a short turn, holding Claude's memory (200 MB and up). Fern's T3 image waits 5 minutes (`images/t3/fern-sessions.patch`; `T3CODE_PROVIDER_IDLE_TIMEOUT_MS` changes it): Claude exits 5 minutes after the last turn, and the box sleeps 5 minutes later, or 20 minutes after Claude started if that is later.

**Hibernation: Fern stops an idle Claude itself (built).** T3's wait is still 20 minutes of an awake box per short turn, and Claude waiting costs more than the box: in the lab, a box with Claude waiting uses about 1.8 s of CPU a minute and 146 MiB, against 40 MiB without it. So `fern exec --hibernate D` (fern-claude passes `$FERN_HIBERNATE`, default 30 s; 0 turns it off) stands in for Claude in T3's server (`internal/hibernate`). It reads the stream-json between Claude and its SDK; once Claude has had nothing to do for D (its turn ended with a `result`, no control request is unanswered either way, no background task runs), it sends Claude SIGTERM, which saves its session, and ends its exec with `?then=sleep`: the box sleeps at once unless another command runs, and T3 keeps its session as before. The SDK's next line starts Claude again with `--resume=<session>` (the flags that picked or forked a session are dropped), sends it first the setup the SDK gave the stopped one (the newest `initialize`, `mcp_set_servers`, `set_model`, `set_permission_mode` and the like), keeps their answers from the SDK, then the line. Only a Claude driven by its SDK (stream-json both ways, no terminal) is stopped; anything else runs as it is. In T3 (`lab/t3-run.sh`, the idle step) the box sleeps 32.7 s after the turn, the same `fern exec` keeps T3's session, and the next turn, which wakes the box and resumes Claude, answers about the turn before it (lab).

### 6.4 Pre-wake
A wake takes about 250 ms on the VPS (it took about a second as a pod), and a turn that finds its box asleep waits for it. When a turn is likely soon, T3's hook asks for the wake ahead (`POST /envs/{id}/prewake`), and the user's think time hides it:
- **a thread is opened:** T3's server streams an open thread to the app (`orchestration.subscribeThread`); one still open after 2 s is woken ahead. A thread only clicked past isn't: the stream's end cancels the wait. The app's sidebar opens the same stream for the first three threads it shows, which can't be told apart; those cost a wake and two minutes of an idle box each.
- **a thread is reverted:** after T3's rollback has finished (Fern's restore, git's restore and the deleted checkpoints' snapshots), so the wake holds up none of them and starts no services on half-restored files.

The daemon answers at once and wakes in the background, two pre-wakes at a time; up to 8 more wait, and further ones are dropped. A box that is awake is left as it is, its idle clocks included. A box woken ahead sleeps after `--idle-prewake` (default 2 min) unless a command comes first; the first command gives it the usual clocks (§6.3). A pre-wake holds its environment's actor during the wake, like any wake: a command that comes meanwhile waits for the same wake instead of starting another. `fern_prewakes_total` counts them by result (`woken`, then `used` or `wasted`; `awake`, `unknown`, `dropped`, `error`).

Measured when boxes were pods (lab, `lab/check.sh box`): a command 3 s after a pre-wake took 65 ms (p50), one to an asleep box 733 ms. In T3 (`lab/t3-run.sh toy 3`), the turn after a revert no longer waits for a wake, and the revert itself is unchanged (2.2 s p50).

---

## 7. Main flows

### 7.1 New thread
1. T3 (hook or `SandboxService`) → `PUT /envs/t3-8f2c {repo, class}`.
2. The actor computes the **golden key**: hash(setup definition + lockfiles + base image + class).
3. **Golden hit:** `live/` = a writable snapshot of golden (ms). **Miss:** build golden (§8), then copy.
4. Environment `asleep`. No box yet.

**Built:** at its first checkpoint, T3's hook creates the environment for the worktree T3 made (`PUT /envs/{id}?path=P`). The key (`internal/setup`) is a hash of `services.yaml`, `.agents/`/`.fern/` `setup` and `prepare`, the lockfiles at the root, the files `services.yaml` lists under `golden.inputs` (the sample lists its seed and start script) and the box image; a repository with none of the contract files gets no golden. **Hit:** the worktree is adopted onto a writable copy of the golden instead of an empty subvolume, so it has its own checkout over the golden's build output (the sample's seeded database): 0.21 s to create, a first wake of 0.78 s instead of 2.0 s with `initdb` (lab). **Miss:** the worktree is adopted as before and the thread doesn't wait; its first workspace is kept as snapshot `golden-source` and built in the background (§7.5), so the next thread hits. **Ahead:** when a project is added, T3 sends its committed files (`git archive HEAD`) to `PUT /goldens/{repo}`; the daemon unpacks them into a source environment (`setup.SourceID`) and, unless the golden exists or there is no contract, builds from it as from a worktree, then deletes it. So a project's first thread hits too; only the first after `services.yaml` or a lockfile changes misses. T3's turn-0 checkpoint comes right after the create, so a revert to it keeps the database too.

### 7.2 A turn
1. T3 → `POST /envs/t3-8f2c/exec {cmd: "claude …", pty: true}`.
2. Asleep → the actor wakes the box (requests that come meanwhile wait for the same wake) → `fern-init` is ready; a service with `listen` starts at its first connection (§8).
3. The exec streams across turns: T3 keeps Claude running and sends it each turn, and ends it when the session is idle (§6.3).
4. T3 saves git checkpoint `7f3a` → the hook → `POST /snapshots {id: "7f3a"}` → `btrfs subvolume snapshot -r` → ok. **A failed snapshot doesn't fail the turn** (copied from Delta, whose hooks never break the agent): the hook logs it and T3 keeps its git checkpoint. That turn then has code but no disk, and §7.3 refuses to revert to it.

### 7.3 Revert ("Edit from here → Revert files too")
1. The hook → `POST /restore {snapshot: "7f3a"}`. The op is logged in four steps:
   1. stop the session (gracefully)
   2. `live.new` = writable snapshot of `7f3a`
   3. keep the current live as a read-only snapshot `pre-restore-<time>` for a day, so a revert can itself be undone (copied from Delta, which keeps work it couldn't sync instead of dropping it); taken before the swap, so a crash can't lose it
   4. atomic swap `live.new` ↔ `live`; cancel the execs the actor counts

   Built. A crash at any step leaves the old or the new workspace, and `Reconcile` clears `live.new`.
2. ok. Then T3 restores code from git, which also fixes the index in the main repository.
3. The next exec restarts the box on the restored disk.

**A turn without a snapshot** (§7.2) can't be reverted with its data: the restore fails with that reason, and T3's git restore doesn't run. Restoring the code alone would leave the database newer than the code.

### 7.4 Fork
1. `POST /envs/t3-8f2c/fork {snapshot, newId: "t3-91aa"}`. The op is logged: create `t3-91aa/live` from the snapshot, then run the identity hook.
2. The new environment is `asleep`. It wakes on its first touch.

### 7.5 Golden build
1. A builder session on a fresh subvolume: clone at the commit → `.agents/setup` (time limit 20 min; must be safe to rerun).
2. **Kill leftover processes; strip credentials** (anything injected for setup); verify nothing secret remains (a scan).
3. Read-only snapshot → `golden/<repo>@<key>`; record it in `goldens`.
4. **Incremental:** when the key changes, start from the previous golden, so `install` is warm.

**Built,** one build at a time, in the daemon (`internal/actor/golden.go`): the repository's builder environment (`golden-<repo>-<hash>`, listed by `fern ls`) gets a fresh workspace with a copy of the `golden-source` checkout, wakes, so it installs the tools the repository pins (§8) and its services make their first start (`initdb`, seed), sleeps, which stops them cleanly, and loses every file the copy brought in; what is left is snapshotted as `golden-<key>`, and the builder keeps the newest 50. A build that stops leaves no snapshot, and the next starts afresh. A golden holds what the repository's code made, so it is the agent's: a new worktree goes onto it through `os.Root`s on both sides (no symlink in the golden or the worktree leads a file, as root, out of them; files are opened without following symlinks or blocking), and a build starts from an empty workspace rather than the last one's. The builder's box gets no Claude login and no Claude folder, so no credential can reach a golden. The key is computed again from the builder's copy of the source snapshot, which can't change, so a golden is published under the key of what it was built from; a source still waiting at a restart is built then; a build that stopped half-way leaves its builder a workspace (`Reconcile`); a new environment whose golden can't be used (pruned since it was looked up) starts without one. **Not built yet:** running `.agents/setup` (only services' first start is), step 2's scan, the `goldens` table (the builder's snapshots are the index), incremental builds. Build output that names its absolute path (a Python venv's scripts) points at the builder's workspace, not the thread's, and caches keyed on it are rebuilt in each thread (Vite's dependency optimizer). mise's tools don't name it.

### 7.6 Delete
Stop the session → delete `live/` → apply retention to snapshots (keep tagged) → remove rows.

---

## 8. The setup contract

Read from the repo, in order of preference:
1. **`.fern/env.ts`** (later): a typed definition (Alchemy-style) with cached steps, services and optional per-thread external resources (stage = thread).
2. **`.agents/setup` + `.agents/prepare` + `.agents/resume` + `services.yaml`**: compatible with Amp and Delta, which both read `.agents/setup`. `.agents/linked` lists ignored local files, such as `.env`, to bring in. A file in `.fern/` replaces the `.agents/` file of the same name; the two are never merged (Delta does the same with `.delta/`).
3. **`devcontainer.json`**: `onCreateCommand` / `updateContentCommand` go into golden, `postCreateCommand` runs after.
4. **T3's `t3.json`** worktree scripts.
5. **Nothing:** `fern init` asks an agent to write (2) and proves it by building golden.

**Rules** (adopted from Amp and Delta):
- `setup` must be **safe to rerun**, finish in ≤20 min, **leave no processes running**, and use **no personal credentials**.
- `setup` starts **no services**: golden is taken before the environment belongs to a thread. Services start from `services.yaml` or `resume`.
- `prepare` runs in **every new workspace**: before `setup` in a golden build, and once in each new environment (from golden or a fork) before the first `resume`. It must be fast: downloads and builds belong in `setup` (Delta's rule).
- `linked` paths come from the project's main repository (`repos/<repo>/`) and are mounted read-only into the box, **outside `live/`**, so they never enter golden or snapshots. They are not mounted while `setup` runs. (Delta symlinks them to one shared copy instead.)
- `resume` runs on **every wake** and when `fern-init` reconnects without a sleep, so it must be **safe to run twice**. A turn waits for it at most 10 s; after that it keeps running in the background. It's for logins, tunnels and machine identity.
- Services must respect **`PORT`** and **`PUBLIC_URL`**. **A running service does not keep the environment awake.**
- **Tools (built):** a repository pins its tools (Node, pnpm, bun, Python, …) with mise's files at its root: `mise.toml`, `.mise.toml`, `.tool-versions`, `.config/mise.toml` or `.config/mise/config.toml`. The box image has mise and none of the tools. The environment's box (fern-init) installs what they pin into the workspace's `.fern/tools/` (mise's data folder; a `.gitignore` of `*` keeps it out of git) before it starts the services, unless `.fern/tools/.installed` holds the hash of the same files; mise's download cache stays in `/tmp`, out of snapshots. Every command and service runs with mise's shims first on `PATH`, so each runs the version its folder pins. The tool files are part of the golden key, and a repository with only them gets a golden: its build installs the tools once, and every thread forks them with it (`lab/check.sh tools`: Node 22 and pnpm 10 for TanStack Start, golden build 41 s with the dependencies, VPS). A thread whose golden missed installs them at its first wake (Node 22 and pnpm 10: 12 s in a box, against 2.7 s under runc, VPS). A program a command names is found on the PATH it runs with (boxexec), so `node` is the pinned one without a shell. The agent box doesn't install; it shares the workspace. **Later:** the repository's Dockerfile or `devcontainer.json` for system packages.
- **On demand (built):** a service with `listen: unix:PATH` or `listen: tcp:PORT` starts at the first connection there, not with the box: fern-init holds the socket, the box is ready without the service, and the first connections wait and are relayed once its `ready` passes. A unix socket stays fern-init's until then (the service replaces the socket file, as Postgres does, and later connections reach it directly); a TCP port is let go at the first connection, and one in between is refused. A golden builder's box starts every service at once (`FERN_ON_DEMAND=off`), as their first start is what it builds.

`services.yaml`:
```yaml
services:
  postgres:
    run: pg_ctl start -w -D .fern/pg -o "-p $PORT"
    stop: pg_ctl stop -D .fern/pg -m fast
    ready: pg_isready -p $PORT
    listen: tcp:5432    # built: start at the first connection (on demand)
  web:
    run: pnpm dev
    port: 3000          # the default preview, <env>.<repo>.<host>
    env: { DATABASE_URL: "postgres://localhost:${services.postgres.port}/app" }
golden:
  inputs: [db/seed.sql]  # built: files beyond the contract and lockfiles that the golden key covers
```

---

## 9. Storage

```
/var/lib/fern/                   btrfs (a real partition, or a loop-mounted image file)
  golden/<repo>@<key>/           read-only (built as envs/golden-<repo>-<hash>/snaps/golden-<key>/)
  envs/<id>/live/                writable workspace of an environment without a path
      <repo files> node_modules/ .fern/pg/
  envs/<id>/snaps/<snapshot>/    read-only
  envs/<id>/claude/              Claude's state (CLAUDE_CONFIG_DIR): never snapshotted, so a restore keeps the transcripts
  worktrees/<repo>/<name>/       writable workspace of an environment at a path: T3's worktrees
  state/fern.sqlite              the index (on the same disk, not inside any snapshot; root only)
  boxes/<box>/                   a box's /tmp and log while it may run (its bundle is on the node's tmpfs)
  images/<id>/  images/<tag>     the box and T3 images, unpacked (fern image load)
  userdata/ caches/ tools/       T3's own state: T3's home is the pool, so its worktrees land in worktrees/
  repos/<repo>/                  the projects' main repositories, which T3's worktrees point into
```

Only `live/` and `worktrees/` are workspaces; Fern never snapshots T3's state or the main repositories.

- **Snapshot:** `btrfs subvolume snapshot -r`. **Fork:** the same without `-r`. **Restore:** snapshot to `live.new`, then swap. All are metadata operations, whose time doesn't grow with the workspace. A snapshot first flushes what was written since the last one: about 1 ms with nothing written, 24 ms after 8 MB (measured), and more on a busy disk.
- **Consistency:** a snapshot is atomic (crash-consistent). Postgres recovers through its WAL; SQLite needs its WAL file kept beside it.
- **Retention:**
  - keep the last N turn snapshots per environment, plus tagged snapshots, plus golden (built: the newest 50, pruned after every snapshot; no tags yet)
  - keep `pre-restore` copies (§7.3) for a day (built)
  - expire unused goldens after M days
  - **space pressure → refuse new snapshots with a clear error** rather than fill the disk (built: below 5% of the pool and 2 GiB free, new snapshots and environments get `ErrPoolFull`, HTTP 507; restores still run)
  - **an environment whose worktree T3 removed** (its thread was deleted) is deleted when it is still gone an hour later (built); a worktree T3 makes again at the same path, for a thread that resumes, is adopted back with its snapshots
- **Quotas:** btrfs qgroups per environment (cost to be measured), or cgroup-level limits on the box.
- **Moving between machines:** `btrfs send | receive`, incremental against a shared parent. Used for backups, migration and forks on remote machines.
- **ZFS** is an optional alternative backend (`zfs snapshot/clone/rollback`). Clones depend on their origin, so the swap logic differs.

---

## 10. Boxes (sessions)

**Rule: a box holds no state.** Everything writable comes from the workspace; a box's own `/tmp` is emptied at every wake.

The daemon runs each box itself as a gVisor sandbox (`internal/runsc`; reasons in [briefs/boxes-without-kubernetes.md](briefs/boxes-without-kubernetes.md)). A box is the node's process, so it outlives a daemon restart, and the restarted daemon adopts it.

| Aspect | Spec |
|---|---|
| Image | read-only Debian base with Postgres, git, curl, Node and Claude Code (pinned, checksum-verified), unpacked into the pool (`fern image load`); each box gets an overlay on it for its mount points |
| Main process | `tini` as PID 1 (reaps orphans) → `fern-init`: services, exec/PTY server, readiness (`/readyz`), graceful stop (`POST /stop`, which names the wake it stops, so a late request can't stop the next wake at a reused address) |
| Mounts | the workspace at **the same path T3 uses**; Claude's folder `envs/<id>/claude/` (`CLAUDE_CONFIG_DIR`) at its own path, **outside the workspace**, so a restore doesn't rewind its transcripts; the main repository's git folder and the worktree's `.git` file **read-only** (§13) |
| User | non-root, the same uid as T3 (1000), no capabilities, `no_new_privs`, a read-only root |
| Runtime | gVisor (`runsc`) for every box; planned: classes (§2) |
| Limits | 2 CPUs, 2 GiB of memory, 4,096 processes, a cgroup per box under `/fern` |
| Network | a network namespace and veth per wake, a /30 from `10.89.0.0/16`; the internet only (§13) |
| Stop | `POST /stop` to fern-init (the services' `stop:` commands, then SIGTERM and kill), `runsc kill` after 15 s; then the network, overlay and cgroup go |

**Two boxes per environment** (the default, `--agent-boxes`; [briefs/claude-outside-the-box.md](briefs/claude-outside-the-box.md)). The **environment's box** runs the services and gets no Claude folder or login. The **agent box**, `<env>.agent`, runs Claude (`fern exec --agent`, which fern-claude passes) with its file tools and no services; its shell tools are replaced by the daemon's MCP Bash, which runs each command in the environment's box, waking it, and lets it sleep 5 s after the last. A restore, sleep or delete stops both; a pre-wake wakes the agent box. A golden builder's box runs the services, started at once, and gets no Claude at all. The actor decides this in one place (`Group.boxOf`).

**Claude's background commands (built).** A Bash call's command, its process group included, ends with the call, and the box sleeps 5 s after the last one: `npm run dev &` would die at once. So Bash takes `run_in_background`, as Claude's own Bash does: the daemon runs the command as an exec in the environment's box that it holds open itself, keeps its output (the newest 1 MiB not yet read) and answers with an ID (`bash_1`); `BashOutput` returns what it printed since the last read and whether it runs, `KillShell` ends it with its process group. Up to 16 per environment, finished ones included. A job outlives the Bash call and Claude's hibernation (Claude stops 30 s after its turn and its box sleeps; the dev server's box stays up), and ends with KillShell, the box's sleep on its idle clocks (§6.3), a restore, a delete, or the daemon. Claude's hint says to start dev servers this way. Checked by `lab/check.sh devserver`, which starts Vite through MCP Bash, as Claude does: a minute later its box is awake and Vite serves; after KillShell the box sleeps 5 s after the last call.

**Claude's edits reach the dev servers (built).** gVisor tells a box only of its own changes: a dev server (Vite, Next.js) in the environment's box never hears of the files Claude writes from the agent box, and keeps serving the old ones. One sandbox with both as containers doesn't help: events cross only within one container's mount, and gVisor shares bind mounts between containers only for tmpfs ([lab/spike/watch/RESULTS.md](../lab/spike/watch/RESULTS.md)). So while the environment's box is awake the daemon watches its workspace from the host with fanotify (`internal/watch`), whose events name the writing process; it keeps those in the agent box's cgroup, outside `.git`, `node_modules` and `.fern`, gathers them for 20 ms, and asks the box's fern-init (`POST /touch`) to set each file's times to what they are. That is an attribute change inside the box, which its watchers hear: Vite reports `change`, and `add` for new files and folders. The box's own touches are its writes, not the agent box's, so nothing loops. The watch is the environment's box's, not the agent box's: the agent box often sleeps right after a write, and a watch that ended with it would drop the edits it hadn't passed on yet (it did, in the lab). Not passed on: deletes and renames away (Claude's file tools don't make them), and files changed while the environment's box was asleep, which its next wake reads as they are. Checked by `lab/check.sh devserver`.

**Measured on the VPS** (2026-10-28, median of 3): a wake 253 ms and a sleep 90 ms at p50, against 993 and 1,221 ms as pods; 45 MiB per box with 20 awake.

## 11. Front door (wake layer)

Built for previews (`internal/preview`, §5); webhooks are planned. The actor gives every request that comes during a wake the same wake.

- **Entrances:** the unix socket (T3, CLI, MCP), HTTP previews (Host-header routing), webhooks.
- **Wake coalescing:** the first touch on an asleep environment starts **one** wake; later touches wait on it.
- **Holding (built):** a preview request waits for the wake (about 250 ms), then up to 5 s for the port to listen; a wake that fails is **503 + Retry-After**. Planned: health and probe requests get the 503 at once while waking.
- **Activity (built for previews):** a preview request is an exec of the box's actor while it lasts (§6.3). Planned: attached terminals.

---

## 12. Failure handling

| Failure | What happens |
|---|---|
| **Daemon crash** | on restart, `Reconcile` reads what each half-done change left on disk (markers, `live.new`, snapshots without rows) and finishes or clears it; boxes left running get an actor, so they still sleep when idle (built) |
| **Box crash** | the next command's dial fails and the actor wakes the box once more (built); planned: restarts with backoff, and `failed` after N failures in a window |
| **Crash mid-turn** | the agent CLI resumes from its session files; Fern can restore to the last turn snapshot before resuming if the transcript is broken |
| **Restore during an exec** | the box stops and the actor cancels the exec, so no late write from the old session reaches the restored disk (built) |
| **Disk full** | snapshots and new environments refused with "the pool is almost full" (HTTP 507) before the disk is; retention runs at every snapshot; the box keeps working (built) |
| **Snapshot fails at a turn's end** | the turn still counts: T3 keeps its git checkpoint and the hook logs the error; reverting to that turn is refused with the reason (§7.3) |
| **Machine loss** | environments are unavailable until it returns; planned: restoring them elsewhere from shipped snapshots (`btrfs send` to object storage) |
| **Golden build fails** | the environment is created from the previous golden, with a visible warning; the agent is asked to fix the setup |

---

## 13. Security

The threat model is in [briefs/claude-outside-the-box.md](briefs/claude-outside-the-box.md); reporting is in [SECURITY.md](../SECURITY.md).

- **Trust boundaries:**
  - the daemon is privileged (it manages btrfs, mounts and the boxes)
  - **boxes are untrusted:** they run what an agent writes
  - T3 and the CLI are trusted local callers, through the socket's group
  - an agent box's MCP Bash is authenticated with its environment's box token and reaches only that environment's box
- **A box sees only its own folders:** its workspace, its Claude folder (the agent box only) and its repository's git folder read-only, as bind mounts in its OCI spec. Nothing else of the host.
- **A box can't make T3 run its code.** T3's server runs git in the worktrees, as the box's user. The box gets the main repository's git folder read-only, and the worktree's `.git` file read-only too, so it can't point git at a folder whose config runs commands (such as `core.fsmonitor`). `fern-claude` never runs Claude in T3's process for a folder in the pool's worktrees. Checked by `lab/check.sh t3`.
- **Isolation:** gVisor, a non-root user, no capabilities, `no_new_privs`, a read-only root, cgroup limits (§10).
- **Credentials:**
  - Claude's login (`CLAUDE_CODE_OAUTH_TOKEN`) is in `/etc/fern/claude.env`, which only root reads; the daemon reads it at every wake and gives it, as an environment variable, only to the box that runs Claude (§13.1). It is never in the index or a workspace
  - **never in a workspace, golden or snapshot.** Claude's state lives in `envs/<id>/claude/`, outside the workspace; with the login in its environment Claude writes no credentials file. A golden builder's box gets no login (§7.5)
  - planned: fresh credentials for forks; a host-side broker for short-lived, scoped tokens (Fern v1's GitHub App broker)
- **The boxes' network:** an nftables table of their own (`inet fern`, replaced whole in one transaction at every daemon start) drops what boxes send to private, shared, loopback, link-local and multicast ranges and everything they send to the node except answers; only the daemon, on the node, connects to a box. Agent boxes alone may reach the daemon's MCP port. Boxes resolve names with public resolvers (`--box-dns`). Checked by `lab/check.sh`: another box, the node's ports (T3, the daemon's metrics) and the metadata address time out; the internet answers.
- **The machine's network:** T3's port must never be public, as anyone who reaches it uses the owner's subscription (§13.1). On a VPS, `lab/vps.sh` installs a firewall (`inet fern_host`, a table of its own) that lets in only SSH; reach T3 through an SSH tunnel or a tailnet. The daemon's metrics and profiles listen on localhost.
- **Known gaps:** boxes of the same repository share its read-only git folder, so one can read another's checkpoints (the full fix is a repository per thread); the box token is readable in `/proc` by the box's own processes (harmless while only the daemon can reach fern-init); user namespaces inside gVisor aren't set up.
- **Isolation levels by class (planned):** gVisor → micro-VM.

### 13.1 Subscription terms
Checked 2026-10-22 against Anthropic's, OpenAI's and the other providers' published terms. These pages change (Anthropic's twice in 2026): check again before a public release.

**Allowed:** a person runs the vendor's real, unmodified CLI with their own subscription on their own machines, in containers, headless. Anthropic says so for Claude Code ([legal and compliance](https://code.claude.com/docs/en/legal-and-compliance)), and suggests keeping a `setup-token` token as a secret. OpenAI documents Codex on headless machines ([auth](https://learn.chatgpt.com/docs/auth)). Using it through T3 Code is tolerated: since 2026-10-07 Anthropic still lets third-party apps use plan limits through the Agent SDK.

**Fern never:**
- has a login screen, or stores, proxies or relays a subscription token. The person runs `claude setup-token` (or the Codex login) and stores the token on their machine (`lab/claude-login.sh`); Fern only passes it to the boxes.
- changes how a CLI authenticates or what it sends (no `ANTHROPIC_BASE_URL` relay, no client spoofing).
- serves one login to several people. Multi-person cells: each person brings their own.
- is reachable without auth. Anyone who reaches T3 uses the owner's account, which is account sharing. On a VPS the T3 port is behind a firewall or a tailnet, never public.
- shares one Codex `auth.json` between running boxes. OpenAI forbids it ([CI/CD auth](https://learn.chatgpt.com/docs/auth/ci-cd-auth)) and concurrent refreshes break it. The daemon owns the login and refreshes it; boxes get short-lived access tokens.

**Careful with:** parallel forks and automated turns (webhooks, schedules). Plans assume "ordinary, individual usage", so they are capped per subscription and opt-in. API keys stay out (no API keys).

**Providers:** Claude Code and Codex through their own CLIs are supported. Blocked: Claude logins through other agents (pi, OpenCode), which Anthropic prohibits, and Google Antigravity / Gemini consumer logins, which Google forbids wrapping. Fine through T3 if wanted later: pi and OpenCode with ChatGPT, Copilot or xAI logins, Cursor, Muse. Grok's login in a container is undocumented.

---

## 14. Running modes

One remote Linux machine running two systemd services. `lab/setup.sh` (with `FERN_HOST` for a VPS, through `lab/vps.sh`):
- creates the btrfs pool (a loop-mounted image file), installs gVisor and a firewall that lets in only SSH
- installs `fern` and `fern-daemon` in `/usr/local/bin`, loads the box and T3 images into the pool (`fern image load`) and installs `deploy/systemd/` (`fern-daemon.service`, `fern-t3.service`)
- stores Claude's login in `/etc/fern/claude.env`, runs the quick checks and prints the pairing link

The local lab is the same machine as a systemd container in Docker (`lab/up.sh`). Your Mac or phone only runs the T3 app. Against the earlier k3s install, this saves what k3s and containerd cost while idle: about 780 MiB and 9% of a core.

**Scaling (planned):** boxes scale within a machine, since they sleep when idle; more machines, each with its own pool and T3, form cells, and a thread's disk moves with `btrfs send`.

## 15. T3 integration

| T3 seam | Fern call | Status in T3 |
|---|---|---|
| `VcsCheckpointOps.captureCheckpoint(cwd, ref)` | `POST /envs/{env}/snapshots {id: ref}` | exists; needs a ~100-line hook |
| `restoreCheckpoint(cwd, ref)` ("Revert files too") | `POST /envs/{env}/restore {snapshot: ref}` | exists; same hook |
| `deleteCheckpointRefs(cwd, refs)` | `DELETE /envs/{env}/snapshots/{ref}` | exists; same hook |
| Thread fork | `POST /envs/{env}/fork` | exists |
| `SandboxService` (register / start / exec / stop / remove / ports) | env create / wake / exec / sleep / delete / previews | open upstream PR (#15350); Fern would be a backend |
| MCP | `env_*` tools | exists (T3 exposes MCP to agents) |

**The environment ID is derived from T3's thread ID.** T3's worktree path *is* the environment's `live/`. Fern **never snapshots T3's own database.** Rewinding it would resurrect revoked logins; T3 keeps its own history.

**A new thread is a Fern environment, with no setting to change (built).** Only a *New worktree* thread gets an environment: a thread in *Local* works in the project's main folder, which has no box or database, and there a Revert brings nothing back (seen in the lab and on the first VPS). So:
1. **T3's defaults on Fern's server** (`images/t3-settings.js`, which `fern-t3.service` runs before T3 starts, only where the person hasn't chosen): new threads are *New worktree* (the server-wide `defaultThreadEnvMode`, so nothing happens per project), and deleting a thread removes its worktree (`storageCleanup.worktreeOnDelete`), which takes its environment with it (§9). The app reads the setting from the server, so the composer shows *New worktree* from the start.
2. **`fern-claude` refuses a thread's session in a project's main folder** (`repos/`), with a message naming *New worktree*. T3's probes and the commit messages it writes there still run: T3 starts sessions with `--session-id` or `--resume`, and those without. It also keeps Claude out of T3's own process, with its login and the whole pool. Still open: whether T3 shows the message or only "exited with code 1".
3. **Claude is told where it is:** fern-init writes a `CLAUDE.md` in Claude's folder (outside the workspace, so in no snapshot or diff): its own services and data, that a revert brings data back, that it doesn't commit.

Not done: making the main folder an environment. One environment for many threads breaks per-thread revert, gives the box the main repository's `.git` to write (and so code that T3 runs, §13), and puts whatever the folder holds into snapshots. Next, already planned: create the environment when T3 creates the worktree, not at its first checkpoint, which is where goldens start (§7.1).

### 15.1 How the hook reaches a user
Nobody applies a patch by hand. T3's server has to run next to the pool anyway (it runs git and the agent on the worktree paths), so Fern runs it as a systemd service from an image Fern builds with its patches (`images/t3/`) and unpacks into the pool (§14). The user installs Fern, opens the T3 app on their Mac and pairs it with the printed link (Settings → Connections). From then on checkpoints and reverts also cover dependencies and the database.

The cost moves to Fern: the image follows T3's releases, and the hook can break when T3 changes. Three ways to carry it:

| Option | What it is | Verdict |
|---|---|---|
| **A. Patched image** | ~100 lines: a layer that wraps `CheckpointStore` (`apps/server/src/checkpointing/CheckpointStore.ts`), swapped in at one line in `server.ts` | The spike and the MVP; only Fern's image carries it |
| **B. Upstream hook** | A general T3 setting (~50 lines): a command T3 runs when it takes, restores or deletes a checkpoint, with the cwd and the ref | The goal: stock T3, Fern only sets the setting. Needs T3's maintainers |
| **C. No T3 change** | git's `reference-transaction` hook sees saves and deletes (`git update-ref` on `refs/t3/…/checkpoints`) | Fallback only: a restore (`git restore --source <commit>`) moves no ref and fires no hook, so it would need a `git` shim in the image, which breaks when T3's git calls change |

A first, then propose B with what the spike learned (roadmap week 8). C only if B is refused.

---

## 16. Observability

- **Metrics (Prometheus)** at `/metrics` on `127.0.0.1:9090` (`--metrics-addr`):
  - histograms `fern_wake_seconds`, `fern_stop_seconds`, `fern_restore_seconds`, `fern_snapshot_seconds` and `fern_golden_build_seconds` (by `result`: `ok` or `error`); `fern_queue_seconds` (how long a change waited for its actor, by `change`); `fern_wake_step_seconds` (by `step`: `net`, `start`, `ready`)
  - counters `fern_idle_sleeps_total`, `fern_prewakes_total` (by `result`, §6.4), `fern_golden_total` (new worktrees by golden `hit`, `miss` or `none`); Go's runtime and process metrics
  - Go's profiles at `/debug/pprof/` on the same address
  - planned: active boxes, pool usage, box restarts
- **Traces:** with `--trace-endpoint` (OTLP/HTTP), the daemon sends a span per API request, per change (with the wait for its actor as `queue`), per store call, and per step of a box's wake (`net`, `start`, `ready`) and stop. `lab/profile.sh` turns them on with Jaeger on the node, puts them beside whole-node flamegraphs, and turns them off again: they cost a little, so the perf gate refuses to run with them.
- **Logs:** the daemon's with `journalctl -u fern-daemon`; a box's output in `boxes/<box>/log` while it runs. Planned: `fern logs <env>`.
- **Turn timing:** `FERN_TIMING=1` on T3 records provider launch, the first decoded Claude event, Claude's cumulative API counter and each checkpoint span. `fern-claude` passes the flag to boxexec, which logs the process start and first stdout without arguments, environment values or stream contents. `lab/t3-run.sh` adds a `timing` object to each turn.
- **The lab measures everything else:** `lab/perf.sh` gates 288 metrics against a baseline, `lab/ab.sh` compares two versions on the same machine, `lab/profile.sh` shows where an operation's time and CPU go.
- **CLI:** `fern ls`, `fern snapshots <env>`; planned: `fern inspect`, `fern top`.

## 17. Configuration

The daemon's flags (`deploy/systemd/fern-daemon.service` reads them from `/etc/fern/daemon.env` as `FERN_DAEMON_ARGS`):

| Flag | Default | Meaning |
|---|---|---|
| `--idle-agent` | 5m | sleep a box this long after its last command ended (0: never) |
| `--idle-human` | 20m | … and this long after its last command started (0: never) |
| `--idle-prewake` | 2m | sleep a box woken ahead this long after its wake, unless a command comes (0: no pre-wakes) |
| `--agent-boxes` | true | run Claude in an agent box per environment (§10) |
| `--preview-addr` | `:30780` | serve previews on this address; `""` turns them off (§5) |
| `--preview-host` | `localhost` | previews are `http://<port>-<env>.<host>/` and `http://<env>.<host>/` (§5) |
| `--box-dns` | 1.1.1.1,8.8.8.8 | the resolvers boxes use |
| `--metrics-addr` | 127.0.0.1:9090 | metrics and Go's profiles |
| `--trace-endpoint` | none | send traces by OTLP/HTTP |

Fixed in the code for now: the pool (`/var/lib/fern`), retention (50 turn snapshots, pre-restore copies for a day, §9) and the box limits (§10). Planned: a `fern.yaml` with classes, fork caps and retention.

## 18. Code layout

```
cmd/fern/            small CLI
cmd/fern-daemon/     node daemon: wiring only
cmd/fern-init/       the main process inside boxes
internal/api/        socket protocol and client
internal/api/server/ socket API server, the agent box's MCP Bash
internal/actor/      per-environment actors, idle rule, the box policy, golden builds
internal/store/      btrfs pool, snapshots, retention, the SQLite index, Reconcile
internal/runsc/      boxes as gVisor sandboxes: bundle, network, firewall (+ a fake for unit tests)
internal/boxexec/    the exec protocol: fern-init's server, the client, the daemon's relay; touches
internal/watch/      the files an agent box writes, as the host sees them (fanotify)
internal/preview/    the front door for previews
internal/hibernate/  stops an idle Claude and resumes it at the next message
internal/image/      unpacks docker save streams into box roots
internal/setup/      setup contract, golden keys
internal/span/       trace spans
lab/                 the lab: setup, integration checks, perf gate, A/B, profiler
deploy/systemd/      the daemon's and T3's units
```

---

## 19. Milestones

The order of work is [roadmap.md](roadmap.md). Measured results, phase by phase, are in [lab/RESULTS.md](../lab/RESULTS.md).

## 20. Open questions

- **Wake time:** a warm pool of boxes could take a box's start (about 250 ms) out of a turn; a runsc subcontainer pool measured 395 ms against a cold 494 ms (`lab/spike/runsc/RESULTS.md`).
- **Quotas:** btrfs qgroups vs cgroup-level limits, given qgroups' performance cost.
- **Revert edge cases:** T3's "revert files only" vs "revert files too"; threads in the project directory rather than a worktree (§15).
- **Previews:** whether portless can name an already-forwarded port, or needs an adapter.
- **Is the hook enough** to cover every T3 flow that should snapshot or restore?
