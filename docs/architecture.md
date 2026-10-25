# Fern v2 architecture

**Status:** what we intend to build. What exists is marked built; the rest is planned.
- Why, and the research behind it: [design.md](design.md).
- Experiments and dev loop: [engine-local.md](engine-local.md).

---

## 1. What Fern is

Fern gives every AI coding thread its own **environment**: code, installed dependencies and database data on a copy-on-write disk.
- It starts instantly from a **golden snapshot**.
- It runs in a **box** (container or pod) only while something is happening.
- It **sleeps** when idle and **wakes** on the next touch.
- It takes a **snapshot at every agent turn**, so rewinding or forking a thread brings the data along with the code.

Fern runs on **Kubernetes only**: one Go daemon per node (a DaemonSet), a thin operator, and a CSI driver. Sessions are standard [agent-sandbox](https://github.com/kubernetes-sigs/agent-sandbox) `Sandbox` objects. One person on one machine runs a single-node k3s. Everything runs on a **remote Linux machine**; your Mac or phone only runs the T3 app. Fern integrates with **T3 Code** and runs the real `claude` / `codex` CLIs with your own subscriptions.

### 1.1 In scope
- environments, snapshots, golden snapshots, boxes, sleep and wake, rewind, fork
- the setup contract (`.agents/setup`, `.agents/resume`, `services.yaml`)
- a local socket API, a CLI, MCP tools for agents, and a small hook for T3
- Kubernetes only: DaemonSet + CSI driver + thin operator (`Environment`, `EnvironmentClass` CRDs) + agent-sandbox `Sandbox` for sessions; single-node k3s for one machine

### 1.2 Out of scope (non-goals)
- **Owning threads or chat.** T3 owns conversations and git; Fern owns disks and boxes.
- **Memory snapshots** (`runsc checkpoint`, CRIU). The promise is "stop and restart"; memory snapshots are a later stretch goal.
- **API-key model access,** and any proxy that intermediates subscription credentials.
- **A hosted multi-tenant service.** Fern is self-hosted, one login per person.
- **Per-turn Kubernetes objects.** Snapshots and execs in an awake box never go through the API server; CRDs hold only desired state. Only a box's wake and stop (a sleep, or the stop in a restore) change its `Sandbox`, which happens when a thread wakes or reverts, not every turn.
- **A non-Kubernetes local backend.** A fake driver exists only for unit tests.
- **Our own URL product.** We use portless or the front door's simple hostname routing.
- **Running on the Mac.** Fern runs on a remote Linux machine; the Mac only runs the T3 app.

---

## 2. Concepts

Three nouns. Everything else is a property of one of them.

| Noun | What it is | Lifetime | git analogy |
|---|---|---|---|
| **Snapshot** | a read-only, frozen copy of an environment's disk | never changes; deleted only by retention | commit |
| **Environment** | a name + a pointer to its latest state + a writable `live/` folder | as long as the thread | branch + working tree |
| **Session** (the "box") | a container or pod running on top of `live/` | only while working | running the checkout |

**Properties:**
- **Golden** = a snapshot tagged as the starting point for a repo.
- **Retention** = `persistent` (default) or `ephemeral`.
- **Class** = size and isolation (`light`, `heavy`, `bench`, `untrusted`, `gpu`).

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
| Session | a generated ID, never addressed by callers | `s-01HZ…` |
| Operation | a generated ID + a caller **idempotency key** | `op-…`, key `t3:checkpoint:7f3a` |

---

## 3. Components (one job each)

```
                ┌──────────────────────── fern daemon (one per machine) ────────────────────────┐
 T3 hook ──┐    │                                                                                │
 CLI ──────┼──▶ │ api      ──▶ wake (front door) ──▶ actor (one per environment) ──┬──▶ store    │──▶ btrfs pool
 MCP ──────┘    │ (socket)     (previews, hooks)       mailbox · one op at a time  ├──▶ driver   │──▶ boxes
 browser ──────▶│ preview http                                                   ├──▶ setup    │
 webhook ──────▶│ /hooks                                                          └──▶ identity │
                │                       Reconcile ◀── finishes or clears a change a crash stopped │
                └────────────────────────────────────────────────────────────────────────────────┘
 inside every box:  fern-init (main process) → runs services, serves exec/PTY and readiness, reports activity
```

| Component | Job | Talks to |
|---|---|---|
| **api** | the daemon's request surface: unix socket (JSON over HTTP), auth, idempotency keys | wake, actor |
| **wake** (front door) | finds or creates the environment's actor; wakes sleeping environments **once**, holds requests while waking, routes previews and webhooks | actor |
| **actor** | owns one environment while its box may be running: applies changes **one at a time**, counts running execs, cancels them on restore, sleep and delete, and puts an idle box to sleep (built: `internal/actor`) | store, driver, setup, identity |
| **Reconcile** (in store) | crash safety without a separate log: every multi-step change leaves the disk in a state that shows how far it got, and `Reconcile` finishes or clears it on start (built). Crash tests stop each change at every step | btrfs, SQLite |
| **store** | btrfs pool: subvolumes, snapshots, forks, restore by swap, retention, size accounting | btrfs |
| **operator** (in the daemon) | kubelet-style: watches `Environment` resources assigned to **its own node**, reconciles them, writes status; each `Environment` owns one `Sandbox`, which owns the session pod | Kubernetes API (controller-runtime) |
| **driver** | makes one agent-sandbox `Sandbox` per environment at its first wake and brings its `podTemplate` up to date at every wake, then flips `operatingMode` (`Running` = awake, `Suspended` = asleep); the template carries the CSI volumes, RuntimeClass, limits and node pinning. Kubernetes only, plus a fake for unit tests | Kubernetes API (agent-sandbox `v1beta1`) |
| **setup** | builds golden snapshots from the repo's setup contract; computes golden keys | driver, store |
| **identity** | post-fork / post-restore hook: new environment ID, fresh secrets, new preview names and ports | actor |
| **fern-init** | the main process of every box (the pod's pause container is PID 1): starts and supervises services, serves exec with PTYs and readiness, reports activity (open execs, preview connections; planned) | api (via the driver) |
| **mcp** | exposes agent tools (`env_snapshot`, `env_restore`, `env_fork`, `env_exec`, `env_delete`) | api |
| **csi** | CSI node plugin: mounts an environment's folders (workspace, Claude's folder, git folder) into its box, **only for that environment's own pod**: named after it, in `fern-boxes` (built); planned: the pod's UID checked against its `Sandbox` | store |

**Why an in-box agent (`fern-init`)?** Exec, services and activity reporting then work the same way locally and on Kubernetes. That avoids `docker exec` / `kubectl exec` as a data plane (those are fragile for long streams). Its exec protocol is Fern's own: one WebSocket per command, with frames for stdin, stdout, stderr, terminal sizes and signals (`internal/boxexec`). Speaking agent-sandbox's `sandboxd` (gRPC with PTY) instead, so its SDKs would work, is still open (§20).

---

## 4. Data model (daemon state, SQLite)

```sql
environments(
  id TEXT PRIMARY KEY,            -- t3-8f2c
  repo TEXT, class TEXT, retention TEXT,          -- persistent | ephemeral
  state TEXT,                     -- see §6
  live_subvol TEXT,               -- /var/lib/fern/envs/t3-8f2c/live
  head_snapshot TEXT,             -- last snapshot taken or restored
  golden TEXT,                    -- golden it was created from
  created_at, last_activity_at, generation INTEGER  -- generation bumps on every restore/fork (fencing)
)
snapshots(
  id TEXT PRIMARY KEY, env TEXT, parent TEXT, kind TEXT,  -- turn | manual | golden
  path TEXT, bytes_exclusive INTEGER, tags TEXT, created_at
)
sessions(
  id TEXT PRIMARY KEY, env TEXT, driver_ref TEXT,  -- container id / pod name
  state TEXT, started_at, generation INTEGER
)
goldens(key TEXT PRIMARY KEY, repo TEXT, snapshot TEXT, built_at, last_used_at)
```

- The disk is the data; SQLite is the index and the log. On startup the daemon **reconciles**: the subvolumes and sessions that actually exist versus what the tables say (§12).
- **A restore fences stale work** by stopping the box and cancelling every exec the actor counts, including one still connecting, so nothing that started before it runs after it (built). The planned `generation` column records the count of restores for status.
- **Timers** (idle sleep, fork TTL, retention) are rows too (`timers(env, kind, due_at)`), so they survive restarts, like Durable Objects' alarms.
- **Kubernetes holds desired state, the daemon holds observed state:**
```yaml
apiVersion: fern.dev/v1alpha1
kind: Environment
metadata: { name: t3-8f2c, namespace: noah }
spec: { repo: github.com/acme/app, class: light, retention: persistent, node: node-1 }
status: { phase: Asleep, head: 7f3a, session: null, generation: 12, bytes: 1.2Gi }
```
  Snapshots are listed in status (counts, head), never as objects.

---

## 5. API

Unix socket `/run/fern/fern.sock` (a host directory, so T3's server pod can share it), JSON over HTTP; exec is a WebSocket. Every call is safe to repeat; planned: an `Idempotency-Key` on every mutating call.

| Call | Meaning | Notes |
|---|---|---|
| `PUT /envs/{id}?path=P` | get-or-create an environment; a tar body fills its workspace | built; its box comes with the first wake. Planned: from golden, `{repo, class, retention, from?}` |
| `GET /envs` / `GET /envs?path=P` | list with box states / the environment whose workspace holds P | built; a lookup by path answers from the index alone (fern-claude does one per Claude start) |
| `GET /envs/{id}/exec` | run a command (optional PTY) over a WebSocket | built; wakes the environment if asleep |
| `POST /envs/{id}/snapshots` | take a snapshot `{id, replace?}` | built; the T3 hook uses the checkpoint ref as the ID, and `replace` after a revert. Planned: `tags` |
| `GET /envs/{id}/snapshots` | list snapshots | built |
| `POST /envs/{id}/restore` | restore `{snapshot}` | built; stops the box, swaps the workspace; the box restarts lazily |
| `POST /envs/{id}/sleep` / `wake` | explicit lifecycle | built; planned: usually automatic |
| `DELETE /envs/{id}` | delete the environment, its box and snapshots | built |
| `DELETE /envs/{id}/snapshots/{s}` | prune | built; the T3 hook calls this when checkpoints are deleted |
| `POST /envs/{id}/fork` | new environment from `{snapshot}` `{newId, count?}` | planned; runs the identity hook; fork caps apply |
| `GET /events` | event stream (state changes, snapshots, wakes) | planned; for UIs and tests |
| `POST /goldens/{repo}/build` | (re)build golden | planned; normally automatic |

**Other entrances (planned):**
- **Previews:** `http(s)://<env>.<repo>.<host>/…` → the front door → the service with a `port` in `services.yaml`. Any other port the box listens on is at `<port>-<env>.<repo>.<host>`, with nothing to declare, and `fern-init url <port>` prints that URL inside the box so agents can hand out links (both copied from Delta). Previews answer **503** (never 404) while waking. Like T3's own port, they are never public (firewall or tailnet).
- **Webhooks:** `POST /hooks/{env}/{token}` → wakes the environment and delivers the request.

**Errors:** an HTTP status (404 not found, 400 invalid, 500 failed) with `{"error": message}`. Planned with the actor and quotas: busy, conflict and quota exceeded; normal calls will queue in the actor's mailbox.

**Agent tools (MCP):** `env_snapshot`, `env_restore`, `env_fork`, `env_exec`, `env_delete`. A **Code Mode** client wraps the same API as a small TypeScript library.

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
`creating → starting (fern-init up, resume script, services) → ready → stopping (SIGTERM services, timeout, kill) → gone`

The driver reports unexpected exits. The actor restarts with backoff, and gives up after N failures in a window (OTP-style supervision).

### 6.3 Idle rule
An environment goes to sleep when all of these hold (two clocks, copied from Amp):
- **no exec is open** (a thread's Claude is one exec while T3 keeps its session), **no terminal is attached** and **no preview connection is active**
- **agent clock:** `idle.agent` (default 5 min) since the last exec closed
- **human clock:** `idle.human` (default 20 min) since the last human touch: an exec or terminal opened, or a preview request

CPU usage and running services are ignored. An explicit `hold` keeps it awake.

Built: the actor's timer, with both clocks touched by every exec (`--idle-agent`, `--idle-human`; 0 turns it off). Terminals, previews and `hold` are planned.

**T3 keeps a thread's Claude running between turns,** in one exec, and ends it once the session has been idle (no turn, tool call, approval or background task). Unpatched T3 waits 30 minutes, so a box would stay awake about 35 minutes after a short turn, holding Claude's memory (200 MB and up). Fern's T3 image waits 5 minutes (`images/t3/fern-sessions.patch`; `T3CODE_PROVIDER_IDLE_TIMEOUT_MS` changes it): Claude exits 5 minutes after the last turn, and the box sleeps 5 minutes later, or 20 minutes after Claude started if that is later. The next turn wakes the box and T3 resumes the same conversation.

---

## 7. Main flows

### 7.1 New thread
1. T3 (hook or `SandboxService`) → `PUT /envs/t3-8f2c {repo, class}`.
2. The actor computes the **golden key**: hash(setup definition + lockfiles + base image + class).
3. **Golden hit:** `live/` = a writable snapshot of golden (ms). **Miss:** build golden (§8), then copy.
4. Environment `asleep`. No box yet.

**Built:** at its first checkpoint, T3's hook creates the environment for the worktree T3 made (`PUT /envs/{id}?path=P`). The key (`internal/setup`) is a hash of `services.yaml`, `.agents/`/`.fern/` `setup` and `prepare`, the lockfiles at the root, the files `services.yaml` lists under `golden.inputs` (the sample lists its seed and start script) and the box image; a repository with none of the contract files gets no golden. **Hit:** the worktree is adopted onto a writable copy of the golden instead of an empty subvolume, so it has its own checkout over the golden's build output (the sample's seeded database): 0.21 s to create, a first wake of 0.78 s instead of 2.0 s with `initdb` (lab). **Miss:** the worktree is adopted as before and the thread doesn't wait; its first workspace is kept as snapshot `golden-source` and built in the background (§7.5), so the next thread hits. T3's turn-0 checkpoint comes right after the create, so a revert to it keeps the database too.

### 7.2 A turn
1. T3 → `POST /envs/t3-8f2c/exec {cmd: "claude …", pty: true}`.
2. Asleep → the front door holds the request → the actor → the driver starts a session → `fern-init` runs `.agents/resume` (planned), starts services → ready.
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

**Built,** one build at a time, in the daemon (`internal/actor/golden.go`): the repository's builder environment (`golden-<repo>-<hash>`, listed by `fern ls`) gets a fresh workspace with a copy of the `golden-source` checkout, wakes, so its services make their first start (`initdb`, seed), sleeps, which stops them cleanly, and loses every file the copy brought in; what is left is snapshotted as `golden-<key>`, and the builder keeps the newest 50. A build that stops leaves no snapshot, and the next starts afresh. A golden holds what the repository's code made, so it is the agent's: a new worktree goes onto it through `os.Root`s on both sides (no symlink in the golden or the worktree leads a file, as root, out of them; files are opened without following symlinks or blocking), and a build starts from an empty workspace rather than the last one's. The builder's box gets no Claude login and no Claude folder, so no credential can reach a golden. The key is computed again from the builder's copy of the source snapshot, which can't change, so a golden is published under the key of what it was built from; a source still waiting at a restart is built then; a build that stopped half-way leaves its builder a workspace (`Reconcile`); a new environment whose golden can't be used (pruned since it was looked up) starts without one. **Not built yet:** running `.agents/setup` (only services' first start is), step 2's scan, the `goldens` table (the builder's snapshots are the index), incremental builds. Build output that names its absolute path (a Python venv's scripts) points at the builder's workspace, not the thread's.

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

`services.yaml`:
```yaml
services:
  postgres:
    run: pg_ctl start -w -D .fern/pg -o "-p $PORT"
    stop: pg_ctl stop -D .fern/pg -m fast
    ready: pg_isready -p $PORT
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
  state/fern.sqlite              the index (on the same disk, not inside any snapshot)
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

**Rule: a box holds no state.** Everything writable comes from `live/`.

| Aspect | Spec |
|---|---|
| Image | read-only base image with common toolchains (Node, Bun, pnpm, Python, uv, git, gh, Postgres binaries, agent CLIs pinned to tested versions) |
| Main process | `fern-init` (the pause container is PID 1 and reaps orphans): services, exec/PTY server, readiness for the daemon's wake (`/readyz`), graceful shutdown; planned: **reattachable sessions** (agent terminals survive daemon restarts and upgrades) and activity reporting |
| Mounts | `live/` → the workspace at **the same path T3 uses**; Claude's folder `envs/<id>/claude/` → `CLAUDE_CONFIG_DIR` at the same path, **outside `live/`**, so a restore doesn't rewind its transcripts; the main repository's git folder **read-only**, so git works and can't change the repository's config or hooks; agent logins as environment variables from a Secret |
| User | non-root, the same uid as T3 so file ownership matches; **user namespaces on** (GA in Kubernetes 1.36), so a box escape isn't that user on the host |
| Runtime | from the class: `crun`/`runc` (light), `runsc` / gVisor (untrusted), a `docker` class for repos that need `docker compose` inside (gVisor or Kata). Built: every box runs under gVisor, from the daemon's `-runtime-class`, until classes exist |
| Limits | CPU and memory from the class (cgroups / pod resources); fork count and TTL caps |
| Network | deny by default; allowlist (package registries, GitHub, the model provider); previews only through the front door |
| Stop | SIGTERM to services (`stop:` commands), timeout, then kill; never during an open exec unless forced |

---

## 11. Front door (wake layer)

- **Entrances:** the unix socket (T3, CLI, MCP), HTTP previews (Host-header routing), webhooks.
- **Wake coalescing:** the first touch on an asleep environment starts **one** wake; later touches wait on it.
- **Holding:** requests are held up to `wake_timeout` (default 30 s). Health and probe requests get **503 + Retry-After** while waking.
- **Activity:** the front door and `fern-init` report open execs, attached terminals and preview connections to the actor for the idle rule.
- **On Kubernetes:** a preview can arrive at any machine's daemon. It forwards the request to the daemon that owns the environment (ownership is the `node` field of its `Environment` resource). An optional edge Deployment can do the lookup behind a wildcard Ingress.

---

## 12. Failure handling

| Failure | What happens |
|---|---|
| **Daemon crash** | on restart, `Reconcile` reads what each half-done change left on disk (markers, `live.new`, snapshots without rows) and finishes or clears it; boxes left running get an actor, so they still sleep when idle (built) |
| **Box crash** | the actor restarts it with backoff; after N failures in a window → `failed`, the next touch retries |
| **Crash mid-turn** | the agent CLI resumes from its session files; Fern can restore to the last turn snapshot before resuming if the transcript is broken |
| **Restore during an exec** | the box stops and the actor cancels the exec, so no late write from the old session reaches the restored disk (built) |
| **Disk full** | snapshots and new environments refused with "the pool is almost full" (HTTP 507) before the disk is; retention runs at every snapshot; the box keeps working (built) |
| **Snapshot fails at a turn's end** | the turn still counts: T3 keeps its git checkpoint and the hook logs the error; reverting to that turn is refused with the reason (§7.3) |
| **Node loss (Kubernetes)** | environments on that node are unavailable until it returns, or until they're restored elsewhere from shipped snapshots (`btrfs send` to object storage, optional) |
| **Golden build fails** | the environment is created from the previous golden, with a visible warning; the agent is asked to fix the setup |

---

## 13. Security

- **Trust boundaries:**
  - the daemon is privileged (it manages btrfs and the runtime)
  - **boxes are untrusted**
  - T3 and the CLI are trusted local callers
  - agents via MCP get **a token scoped to their own environment**
- **Mounting is the permission.** A box sees only its own workspace and Claude folder, and its repository's git folder read-only, only through the CSI driver, never raw host paths.
- **A box can't make T3 run its code.** T3's server runs git in the worktrees, as the box's user. The box gets the main repository's git folder read-only, and the worktree's `.git` file read-only too, so it can't point git at a folder whose config runs commands (such as `core.fsmonitor`). `fern-claude` never runs Claude in T3's pod for a folder in the pool's worktrees. Built and checked (`lab/check.sh t3`).
- **The CSI driver authorizes every mount:** it only mounts an environment for the pod named after it in `fern-boxes`, where only the daemon makes pods (built). Without this, any pod could name another environment and get its disk. Planned: the pod's UID checked against its `Sandbox`, and the `Environment` owner once there are CRDs.
- **An admission policy limits the daemon:** it may only create `Sandbox` objects pinned to its own node (node-bound service-account tokens) and only with allowed runtimes.
- **User namespaces** for every box.
- **Credentials:**
  - agent logins (`CLAUDE_CODE_OAUTH_TOKEN`, a short-lived Codex access token from the login the daemon refreshes) come from a Kubernetes Secret as environment variables when the box starts (§13.1), so they are never in the `Sandbox`, the index or a file
  - **never in `live/`, golden or snapshots** (golden builds are scanned). Claude's state lives in `envs/<id>/claude/`, outside `live/`; with the login in its environment it writes no credentials file. Codex keeps `auth.json` in its home, which is mounted from outside `live/` the same way
  - forks get fresh credentials
- **Non-model secrets:** a host-side broker for short-lived, scoped tokens (Fern v1's GitHub App broker); per-environment OIDC tokens later.
- **Network:** a box takes connections only from the daemon, and reaches only DNS and the public internet: not T3, the daemon, the Kubernetes API, other boxes, private addresses or a cloud's metadata endpoint (built, `deploy/fern.yaml`). Inside the cluster the Kubernetes API's address leads to the node's public address, as do NodePorts, so the policy also excepts the node's public address (on the VPS, `lab/build.sh` adds it; measured: the API and T3 refused, the internet reached). Some network plugins always allow traffic to the node; there it takes the node's firewall. Planned: per-class allowlists.
- **Known gaps:** boxes of the same repository share its read-only git folder, so one can read another's checkpoints (the full fix is a repository per thread); the box token is readable in `/proc` by the box's own processes (harmless while only the daemon can reach fern-init); user namespaces and a Pod Security level for `fern-boxes` aren't set yet.
- **Isolation levels by class:** runc → gVisor → micro-VM.

### 13.1 Subscription terms
Checked 2026-10-22 against Anthropic's, OpenAI's and the other providers' published terms. These pages change (Anthropic's twice in 2026): check again before a public release.

**Allowed:** a person runs the vendor's real, unmodified CLI with their own subscription on their own machines, in containers, headless. Anthropic says so for Claude Code ([legal and compliance](https://code.claude.com/docs/en/legal-and-compliance)), and suggests keeping a `setup-token` token as a secret. OpenAI documents Codex on headless machines ([auth](https://learn.chatgpt.com/docs/auth)). Using it through T3 Code is tolerated: since 2026-10-07 Anthropic still lets third-party apps use plan limits through the Agent SDK.

**Fern never:**
- has a login screen, or stores, proxies or relays a subscription token. The person runs `claude setup-token` (or the Codex login) and creates the Secret; Fern only mounts it.
- changes how a CLI authenticates or what it sends (no `ANTHROPIC_BASE_URL` relay, no client spoofing).
- serves one login to several people. Multi-person cells: each person brings their own.
- is reachable without auth. Anyone who reaches T3 uses the owner's account, which is account sharing. On a VPS the T3 port is behind a firewall or a tailnet, never public.
- shares one Codex `auth.json` between running boxes. OpenAI forbids it ([CI/CD auth](https://learn.chatgpt.com/docs/auth/ci-cd-auth)) and concurrent refreshes break it. The daemon owns the login and refreshes it; boxes get short-lived access tokens.

**Careful with:** parallel forks and automated turns (webhooks, schedules). Plans assume "ordinary, individual usage", so they are capped per subscription and opt-in. API keys stay out (no API keys).

**Providers:** Claude Code and Codex through their own CLIs are supported. Blocked: Claude logins through other agents (pi, OpenCode), which Anthropic prohibits, and Google Antigravity / Gemini consumer logins, which Google forbids wrapping. Fine through T3 if wanted later: pi and OpenCode with ChatGPT, Copilot or xAI logins, Cursor, Muse. Grok's login in a container is undocumented.

---

## 14. Running modes

### 14.1 One remote Linux machine = single-node k3s
`fern up` on the remote machine:
- installs k3s (or uses an existing cluster)
- creates the btrfs pool (a partition, or a loop-mounted image)
- installs the Helm chart (DaemonSet, CSI driver, CRDs, the agent-sandbox controller, gVisor RuntimeClass, NetworkPolicy defaults)
- starts the person's T3 server pod and prints the pairing step (or use T3's *Add environment → SSH*)

Your Mac or phone only runs the T3 app. More nodes: `fern join <token>`.

### 14.2 Kubernetes
| Concept | Kubernetes |
|---|---|
| Fern daemon | **DaemonSet**, privileged, owns `/var/lib/fern` on each node |
| Mounting `live/` into a pod | **CSI inline (ephemeral) volume** served by the daemon's CSI node plugin: `csi: {driver: fern, volumeAttributes: {env: t3-8f2c}}` |
| Session | an **agent-sandbox `Sandbox` owned by its `Environment`**, created by that node's daemon (kubelet-style), pinned to the node, labelled `fern/env`, `t3/thread`; sleep/wake = `operatingMode: Suspended/Running` |
| T3 server | one pod per person per node (1-replica StatefulSet), sharing the node's Fern pool |
| Class | RuntimeClass + resources + node selector (no warm pools in v1) |
| Per-person limits | a namespace per person + ResourceQuota + NetworkPolicy |
| API objects | `Environment` (desired state + status) and `EnvironmentClass` CRDs; nothing per turn |
| Durable/cross-node copies | `VolumeSnapshot` or `btrfs send`; **never on the per-turn hot path** |
| Install | `fern up --k8s`: k3s if there's no cluster, the Helm chart, a loop-mounted btrfs pool if needed, `fern join <token>` for more nodes |

**Scaling:**
- sessions scale within a node (they sleep when idle)
- nodes scale out
- each person's T3 + environments form a **cell** pinned to a node; one person can have cells on several nodes, and T3's load balancer picks
- agent benchmark forks can run on any node (the disk is shipped with `send`)

---

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
1. **T3's defaults on Fern's server** (an init container in `deploy/t3.yaml`, only where the person hasn't chosen): new threads are *New worktree* (the server-wide `defaultThreadEnvMode`, so nothing happens per project), and deleting a thread removes its worktree (`storageCleanup.worktreeOnDelete`), which takes its environment with it (§9). The app reads the setting from the server, so the composer shows *New worktree* from the start.
2. **`fern-claude` refuses a thread's session in a project's main folder** (`repos/`), with a message naming *New worktree*. T3's probes and the commit messages it writes there still run: T3 starts sessions with `--session-id` or `--resume`, and those without. It also keeps Claude out of T3's pod, with its login and the whole pool. Still open: whether T3 shows the message or only "exited with code 1".
3. **Claude is told where it is:** fern-init writes a `CLAUDE.md` in Claude's folder (outside the workspace, so in no snapshot or diff): its own services and data, that a revert brings data back, that it doesn't commit.

Not done: making the main folder an environment. One environment for many threads breaks per-thread revert, gives the box the main repository's `.git` to write (and so code that T3 runs, §13), and puts whatever the folder holds into snapshots. Next, already planned: create the environment when T3 creates the worktree, not at its first checkpoint, which is where goldens start (§7.1).

### 15.1 How the hook reaches a user
Nobody applies a patch by hand. T3's server has to run next to the pool anyway (it runs git and the agent on the worktree paths), so Fern runs it as a pod from an image Fern builds (§14.1). The user installs Fern, opens the T3 app on their Mac and pairs it with the printed link (Settings → Connections). From then on checkpoints and reverts also cover dependencies and the database.

The cost moves to Fern: the image follows T3's releases, and the hook can break when T3 changes. Three ways to carry it:

| Option | What it is | Verdict |
|---|---|---|
| **A. Patched image** | ~100 lines: a layer that wraps `CheckpointStore` (`apps/server/src/checkpointing/CheckpointStore.ts`), swapped in at one line in `server.ts` | The spike and the MVP; only Fern's image carries it |
| **B. Upstream hook** | A general T3 setting (~50 lines): a command T3 runs when it takes, restores or deletes a checkpoint, with the cwd and the ref | The goal: stock T3, Fern only sets the setting. Needs T3's maintainers |
| **C. No T3 change** | git's `reference-transaction` hook sees saves and deletes (`git update-ref` on `refs/t3/…/checkpoints`) | Fallback only: a restore (`git restore --source <commit>`) moves no ref and fires no hook, so it would need a `git` shim in the image, which breaks when T3's git calls change |

A first, then propose B with what the spike learned (roadmap week 8). C only if B is refused.

---

## 16. Observability

- **Events:** `GET /events` (created, woke, slept, snapshot, restored, forked, failed).
- **Metrics (Prometheus),** at `/metrics` on the daemon's port 9090 (`--metrics-addr`):
  - built: histograms `fern_wake_seconds`, `fern_stop_seconds`, `fern_restore_seconds` and `fern_snapshot_seconds` (by `result`: `ok` or `error`), `fern_queue_seconds` (how long a change waited for its actor, by `change`), `fern_wake_step_seconds` (by `step`: `sandbox`, `pod`, `ready`); `fern_idle_sleeps_total`; Go's process metrics. A wake and a restore are also logged with their time
  - planned: golden hit rate and build time, active sessions, pool usage, box restarts
- **Logs:** per environment (services, `fern-init`, resume/setup output), viewable with `fern logs <env>`.
- **Turn timing (built):** `FERN_TIMING=1` on T3 records provider launch, the first decoded Claude event, Claude's cumulative API counter and each Git/Fern checkpoint span. `fern-claude` passes the flag to boxexec, which logs the successful process start and first stdout without arguments, environment values or stream contents. `lab/t3-run.sh` adds a `timing` object to each turn; it reads logs after the wall timer stops and uses T3's event stream for completion, keeping the existing `wallMs`, `runMs` and daemon metric fields. Driver and node clocks must be synchronized. `lab/t3-start.sh` compares the pinned native Claude binary inside and outside gVisor, including SDK initialization without a model prompt.
- **CLI:** `fern ls`, `fern inspect <env>`, `fern snapshots <env>`, `fern top`.

---

## 17. Configuration (`fern.yaml`, minimal)

```yaml
pool: /var/lib/fern            # btrfs mount (created if missing, as an image file)
driver: local                  # local | k8s
idle: { agent: 5m, human: 20m }
wake_timeout: 30s
retention: { turns: 50, golden_unused: 7d, pre_restore: 1d }
fork: { max: 8, agent_ttl: 1h }
classes:
  light:     { runtime: runc,  cpu: 2, memory: 4Gi }
  untrusted: { runtime: runsc, cpu: 2, memory: 4Gi, network: none }
  bench:     { runtime: runc,  cpu: 4, memory: 8Gi, pinned_cpus: true }
```

---

## 18. Code layout

```
cmd/fern/            small CLI (fern daemon execs its sibling fern-daemon)
cmd/fern-daemon/     node daemon + CSI plugin
cmd/fern-init/       the main process inside boxes
internal/api/        socket protocol and client
internal/api/server/ socket API server, auth, idempotency
internal/wake/       front door, preview routing, webhooks, coalescing
internal/actor/      per-environment actors, idle rule, supervision
internal/store/      btrfs (and later zfs) backends, retention, index
internal/operator/   kubelet-style reconciler for this node's Environments (controller-runtime)
internal/driver/     agent-sandbox Sandbox per environment (+ a fake for unit tests)
internal/setup/      setup contract, golden keys and builds
internal/identity/   post-fork/restore identity hook
internal/mcp/        agent tools
internal/csi/        CSI node plugin with per-pod mount authorization
api/v1alpha1/        Environment, EnvironmentClass CRD types
lab/                 experiments and crash-test harness (see engine-local.md)
deploy/              systemd unit, Helm chart
```

---

## 19. Milestones

Kubernetes first, with a vertical slice in week 1. The week-by-week plan (MVP in about 3 weeks, v1 by week 8) is in [roadmap.md](roadmap.md); the demo script is in [design.md §21](design.md#21-roadmap-about-8-weeks-kubernetes-first). Week 0 proves the riskiest piece: a gVisor `Sandbox` using a btrfs subvolume through the CSI inline volume.

---

## 20. Open questions

- **Wake time budget:** box start + `resume` + services. Target ≤15 s, so T3's first connection attempt succeeds. To be measured.
- **agent-sandbox:** `Suspended` → `Running` latency; recreation of a force-deleted pod; pod → `Sandbox` → `Environment` ownership for CSI checks.
- **Quotas:** btrfs qgroups vs cgroup/box-level limits, given qgroups' performance cost.
- **Revert edge cases:** T3's "revert files only" vs "revert files too". Threads in the project directory rather than a worktree: see §15, planned.
- **CSI inline volumes:** btrfs subvolume bind mounts under gVisor (week 0 spike); reliable pod identity for mount authorization.
- **Admission policy:** can it read the node name from node-bound tokens?
- **Reattach:** does `sandboxd` support it, or does `fern-init` need its own protocol?
- **portless:** whether it can name an already-forwarded port, or needs an adapter.
- **Is the hook enough** to cover every T3 flow that should snapshot or restore?
