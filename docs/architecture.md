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
- environments, snapshots, golden snapshots, boxes, sleep and wake, revert, fork
- the setup contract (`services.yaml`, `.agents/setup`, `.agents/prepare`), or a guess from `package.json` without one
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
- **Class** (planned) = size and isolation (`light`, `heavy`, `bench`, `untrusted`, `gpu`). Today a services' box gets the limits the daemon's flags set, or the ones its repository asks for (§10, §17).

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
 CLI ──────┴──▶ │ api (unix socket) ──▶ actor (one per box) ──┬──▶ store (btrfs)                │──▶ btrfs pool
 agent box ───▶ │ MCP Bash (port 7071)  mailbox · one change    └──▶ runsc driver                    │──▶ boxes
                │                       at a time; idle sleep                                          │
                │                       Reconcile ◀── finishes or clears a change a crash stopped      │
                └─────────────────────────────────────────────────────────────────────────────────────┘
 inside every box:  tini → fern-init → runs the services, serves exec (boxexec) and readiness
```

| Component | Job | Code |
|---|---|---|
| **api** | the daemon's request surface: JSON over HTTP on a unix socket; exec as a WebSocket relayed to the box (§5) | `internal/api`, `internal/api/server` |
| **tools** | for agent boxes, an MCP server with Claude's tools: Bash and its background commands in the environment's box, `commit`, the forks (`env_fork`, `env_exec`, `env_delete`) and `cloudflare_connect` (§5); and the settings that point an agent at them in place of its own shell as it starts | `internal/tools` |
| **actor** | one goroutine per box while it may run: makes its environment's changes **one at a time**, counts the commands running in it, cancels them on restore, sleep and delete, and puts it to sleep when idle (§6.3). Pre-wakes (§6.4) and golden builds (§7.5) go through it too | `internal/actor` |
| **box policy** | decides what each box runs and gets: services or Claude, Claude's folder and login or not (`Group.boxOf`) | `internal/actor` |
| **store** | the btrfs pool: workspaces, snapshots, restore by swap, adoption of T3's worktrees, retention, and each environment's record. `Reconcile` gives crash safety without a log: every multi-step change leaves the disk showing how far it got, and Reconcile finishes or clears it at start. Crash tests stop each change at every step | `internal/store` |
| **runsc driver** | wakes and stops boxes: a network namespace and veth per wake, an overlay root on the box image, an OCI bundle, `runsc create`/`start`; the nftables firewall; adopts boxes that outlived a daemon restart | `internal/runsc` |
| **setup** | reads the setup contract and computes golden keys | `internal/setup` |
| **image** | unpacks `docker save` streams into the pool, as box and T3 roots (`fern image load`) | `internal/image` |
| **commit** | makes a thread's git commits on the node, for Claude, whose box has the git folder read-only (§5, §13) | `internal/commit` |
| **disk** | says what uses the node's disk (`fern df`) and removes what nothing needs (`fern gc`), by itself when the pool or the disk under it is low (§9) | `internal/disk` |
| **backup** | backs the threads up hourly to an S3 bucket, encrypted on the node, and restores them on a new one (§14) | `internal/backup` |
| **github** | mints short-lived tokens from the person's GitHub App for T3's git and `gh` (§13) | `internal/github` |
| **health** | says what is wrong on the node now that would otherwise be a line in the daemon's log (`GET /problems`, `fern doctor`, the `fern_problems` metric, §16) | `internal/health` |
| **watch** | while an agent box is awake, watches its workspace from the host with fanotify and keeps the writes of the agent box's processes (their cgroup names the box); the actor passes them to the environment's box as touches (§10) | `internal/watch` |
| **fern-init** | a box's main process: starts and supervises the services (on demand, §8), serves exec with PTYs (`internal/boxexec`), the box's background commands (`internal/background`), readiness, a graceful stop and touches; in an agent box, the relay to the daemon's MCP server (`internal/mcprelay`) | `cmd/fern-init` |
| **hibernate** | in the CLI that T3 runs as `claude` and `opencode`: stops an idle Claude, or OpenCode server, so its box can sleep, and starts it again at the next message (§6.3) | `internal/hibernate` |
| **preview** | the front door for previews: serves boxes' ports over HTTP and WebSocket, routed by host, waking a box that is asleep and counting each request as a use of it (§5, §11) | `internal/preview` |
| identity | planned (§11) | |

**Why an in-box agent (`fern-init`)?** Exec, services and readiness work the same however the box is run, without `docker exec`-style side channels, which are fragile for long streams. Its exec protocol is Fern's own: one WebSocket per command, with frames for stdin, stdout, stderr, terminal sizes and signals (`internal/boxexec`). A command run as a **session** outlives its connection: both sides number their frames and keep them until acknowledged, so a new connection carries on with nothing lost or repeated: it takes over only once the connection before it has let go, and is then told where stdin got to. Each side keeps at most 1 MiB the other hasn't acknowledged, and the box acknowledges stdin when the command has taken it, not when it arrived; stdin that waits never holds up the reading of the client's acknowledgements, which come on the same connection (a command that echoed more than a few MiB of stdin hung before that). A close message means the client is done, and kills the command; a connection that just breaks means the daemon went away, and the command waits for one for 2 minutes. That is how running commands and turns get through a restart of the daemon, which is how Fern upgrades. In an agent box, Claude reaches the daemon's MCP server through a relay in fern-init, which sends a call again under the same key until a daemon answers, so a Bash call running then finds its command instead of failing or running it twice. The relay also answers a tool call at once, with an event stream whose one message is the daemon's answer when the tool is done: Claude Code gives the answer to a POST 60 s to begin, whatever the tool's own limit, so until 2026-11-12 every Bash command longer than a minute failed with "The operation timed out." (a thread on the VPS took it for a slow clock, and polled background commands with `sleep 57` for an hour). A call Claude says it has given up on (`notifications/cancelled`) the relay ends, and its command with it; the stream alone would stay open.

## 4. Data model (the disk)

There is no database. An environment is its directory, `envs/<id>/`, with a record file in it (`internal/store/record.go`):

```json
envs/<id>/env.json   {"version": 1, "workspace": "…", "git": "…", "created": "…"}
envs/<id>/snaps/<snapshot>/   a snapshot: a read-only subvolume, taken when btrfs says it was
```

- **The disk is the only record.** An environment exists when its directory has its record: a create writes it last, in one flushed rename, and a delete removes it first. A snapshot exists when its subvolume does, and btrfs keeps the time it was taken. At start the daemon finishes or clears what a change left half-way (§12).
- **Until 2026-11-12 there was a SQLite index beside the disk** (`state/fern.sqlite`), and the two had to be reconciled. A pool or a backup from before then still has one: the store reads its environments into records at its next start and moves it aside (`internal/store/import.go`).
- `git` is the main repository's git folder that the box gets read-only (§13).
- **A record holds no secret.** A box's token, which authenticates the daemon to its fern-init and an agent box to the daemon, is made when the box starts and kept with its bundle on the node's tmpfs (`runsc.Driver.Token`): every box has its own, it ends with the box, and it is in no record, snapshot or backup. Until 2026-11-12 one token per environment was kept in the record (before that in the index) and shared by its two boxes; a box running since then answers to the one in its spec.
- **No two environments share a workspace:** a create claims its path in the daemon's memory until its record is written.
- **A restore fences stale work** by stopping the box and cancelling every command the actor counts, including one still connecting, so nothing that started before it runs after it.
- **What went wrong is on the disk too, with what it is about:** a builder whose last golden build failed has `envs/<id>/build-failed.json` (deleted by a build that works, and with the builder), and the last backup's result is `backup.json` in the pool (§16).
- **Golden snapshots are snapshots too:** a golden is a snapshot of its builder environment named `golden-<key>`, and a build waiting at a restart is a snapshot named `golden-source` (§7.5).
- **Planned:** tags, a snapshot's parent and size, and timers as rows. Idle clocks survive a restart in the box instead: fern-init keeps when it was last used (`GET /activity`), and the next daemon reads it.

## 5. API

Unix socket `/run/fern/fern.sock`, for root and the box user's group (T3's server runs as the box user), JSON over HTTP; exec is a WebSocket. Every call is safe to repeat; planned: an `Idempotency-Key` on every mutating call.

| Call | Meaning | Notes |
|---|---|---|
| `PUT /envs/{id}?path=P` | get-or-create an environment; a tar body fills its workspace | built; its box comes with the first wake. Planned: from golden, `{repo, class, retention, from?}` |
| `GET /envs` / `GET /envs?path=P` | list with box states / the environment whose workspace holds P | built; a lookup by path reads the records alone (fern-claude does one per Claude start) |
| `GET /envs/{id}/exec` | run a command (optional PTY) over a WebSocket | built; wakes the environment if asleep; with `?then=sleep` the box sleeps as soon as the command ends, unless another runs (a hibernating Claude, §6.3) |
| `POST /envs/{id}/snapshots` | take a snapshot `{id, replace?}` | built; the T3 hook uses the checkpoint ref as the ID, and `replace` after a revert. Planned: `tags` |
| `GET /envs/{id}/snapshots` | list snapshots | built |
| `POST /envs/{id}/restore` | restore `{snapshot}` | built; stops the box, swaps the workspace; the box restarts lazily |
| `POST /envs/{id}/sleep` / `wake` | explicit lifecycle | built; planned: usually automatic |
| `POST /envs/{id}/prewake` | wake the box ahead of a likely command, in the background | built; answers at once (§6.4); the T3 hook calls it when a thread is opened or reverted |
| `GET /envs/{id}/holds` | what holds the environment's box awake, and its agent box: what runs in each, when it sleeps and by which rule | built (`fern why <env>`); asks the actors and wakes nothing (§6.3) |
| `DELETE /envs/{id}` | delete the environment, its box and snapshots | built |
| `DELETE /envs/{id}/snapshots/{s}` | prune | built; the T3 hook calls this when checkpoints are deleted |
| `POST /envs/{id}/fork` | `{count, snapshot?}` new environments, forks, from a snapshot or the workspace as it is; answers `{forks: [ids]}` | built (§7.4); at most 8 forks an environment (409 past that). Planned: the identity hook |
| `GET /disk` | what uses the node's disk: the pool's room and the room of the disk under it, then each environment, golden, image and Podman's images, with what `gc` would remove | built (`fern df`); asks btrfs for each environment's size, seconds on a pool with large workspaces |
| `POST /gc` | remove what nothing needs, and answer what was removed | built (`fern gc`, §9) |
| `POST /backup` | a backup now, as the hourly ones make it; 404 when none are set up, 409 while one runs | built (`fern backup`, §14) |
| `POST /github/token` | a short-lived token from the person's GitHub App, for a repository or for the account it is installed on | built (`fern github-token`, `fern git-credential`, §13) |
| `GET /problems` | what is wrong on the node now that the daemon knows of: a pool that takes no more snapshots, backups that fail or have stopped, golden builds that failed; each with what to do | built (`fern doctor`, `internal/health`, §16) |
| `GET /events` | event stream (state changes, snapshots, wakes) | planned; for UIs and tests |
| `PUT /goldens/{repo}` | build repo's golden from a checkout, a tar body, unless it has one for it | built; T3 sends it when a project is added (`fern prebuild REPO`). Answers before the build |

**Other entrances (planned):**
- **Previews (built):** `http://<port>-<env>.<host>/…` is port `<port>` of the environment's box, with nothing to declare, and `http://<env>.<host>/…` its default preview, the port of the first service in `services.yaml` (by name) with a `port` (copied from Delta). `<host>` is `--preview-host`, `localhost` by default: a browser sends every name under `.localhost` to the machine itself, so no DNS is needed. The front door listens on `--preview-addr` (`:30780`); like T3's own port it is never public (the VPS's firewall lets in only SSH; reach it with `ssh -L 30780:127.0.0.1:30780`, as T3's), and boxes can't reach it. The repository isn't in the name, as planned: an environment's ID is unique on its node. A box learns its URL as `FERN_PREVIEW` (with `{port}`), which Claude's hint passes on, so Claude can hand out the link. Answers: 404 for a name that isn't a preview, an unknown environment or one without a default; 503 with `Retry-After` if the box doesn't wake; 502 saying that nothing listens on the port, after trying for 5 s (a dev server a wake starts listens a moment later).
- **Webhooks:** `POST /hooks/{env}/{token}` → wakes the environment and delivers the request.

**Errors:** an HTTP status (404 not found, 400 invalid, 500 failed) with `{"error": message}`. Planned with the actor and quotas: busy, conflict and quota exceeded; normal calls will queue in the actor's mailbox.

**For agent boxes (built):** an MCP server on port 7071, which only agent boxes can reach (§13), at `/mcp/{env}`, authenticated with the token of the environment's running agent box, which fern-init's relay in that box adds to each call (§4): the agent itself is given none, and the box that runs the repository's code has another, which opens nothing here. Its tool Bash runs a command in the environment's box (§10); with `run_in_background` the command goes on after the call, and BashOutput and KillShell read and stop it (§10). These three are marked to come with Claude's prompt (`_meta` `anthropic/alwaysLoad`): Claude Code otherwise lists an MCP server's tools by name and fetches a definition at its first use, so every session began with Claude saying it had no shell and loading one (seen on the VPS, 2026-11-12). The fork and Cloudflare tools are still fetched when used.

**Cloudflare's, for Cloudflare repositories (built, opt-in):** with a token in `/etc/fern/cloudflare.env` (root only; `install.sh cloudflare`), the daemon adds Cloudflare's MCP server (`https://mcp.cloudflare.com/mcp`, the token as its bearer token) to the agent's MCP config as it starts Claude or OpenCode's server in the agent box, when the workspace deploys to Cloudflare (§8). It reads the file at each start, so adding or removing the token needs no restart. The token is in the agent's arguments or settings in the agent box only, beside Claude's login; the box that runs the repository's code, the workspace and its snapshots never see it (`lab/check.sh cloudflare`).

**`cloudflare_connect` (built, with a Cloudflare token and a GitHub App):** a tool on the environment's MCP server that sets the repository's `CLOUDFLARE_API_TOKEN` Actions secret to the node's token, so a push deploys from GitHub Actions: the daemon reads `origin` with git in the environment's box and seals the value with the repository's key, through the App (*Secrets: write*). Claude's skills for it (`fern-deploy`, and Cloudflare's own) are links in its folder into the box image (fern-init). Brief: [briefs/cloudflare-projects.md](briefs/cloudflare-projects.md).

**T3's own MCP server (built):** T3 gives its Claude an MCP server at `http://127.0.0.1:30773/mcp`, on the node's loopback, in the session's stream, not its arguments. An agent box's fern-init answers at that address in the box (`FERN_T3_ADDR`) for the one path `/mcp`, adds the box's token and passes the call to the daemon's `/t3/{id}`, which checks the token, takes it off and passes the call to T3. A call needs T3's token for the thread too, which only Claude's session holds: the box that runs the repository's code has the first token and not the second. `--t3 ""` on the daemon turns it off.

**Skill packs (built):** Claude's skills come in packs baked into the box image (`/usr/local/share/fern/skills`); a pack is on when the node turns it on (`/etc/fern/skills`, `safety` by default, passed to Claude's box as `FERN_SKILLS` at every wake) or when Fern detects it (`cloudflare`). fern-init links the active packs' skills into Claude's folder, outside the workspace, and adds their notes to the hint. An Astro site's agent also gets Astro's docs MCP server. Brief: [briefs/cloudflare-projects.md](briefs/cloudflare-projects.md).

**`commit` (built):** a tool on the environment's MCP server with which Claude commits its thread's changes: a message, and optionally the paths. The box can't (§13), so the daemon runs the node's git in the worktree (`internal/commit`), as T3's user and with T3's home, whose git config names the author, and with everything that would run a program off: hooks (also a hooks folder the repository's config names inside the worktree), the file monitor, signing, background maintenance. It commits only in a worktree whose git folder is the one in the environment's record, the repository's own, which no box can write: a repository the box made in its workspace has a config of its own, whose filters git would run. A folder of the worktree with a repository of its own is left out of the commit for the same reason, and the commit is made with the commands that never look into one (§13); the tool tells Claude what it left out. It doesn't push; T3 does. The install copies the person's git author from their computer (`install.sh git-author`), and `fern doctor` says when the node has no git or no author. `lab/check.sh commit`. Until 2026-11-12 Claude's hint said "the person commits from T3", which on a server without a git author nobody could.

**Agent tools (built: `env_fork`, `env_exec`, `env_delete`):** on the same MCP server, Claude forks its own environment, runs commands in its forks (calls for different forks run at once) and deletes them; it reaches no other environment (§7.4). Planned: `env_snapshot`, `env_restore`.

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

A wake whose fern-init exits as it starts (a `services.yaml` it can't read, a socket it can't bind) fails within seconds with the end of the box's log: while it waits for `/readyz` the driver looks for the box once a second (built; `lab/check.sh limits`).

**A box found running (built).** What a wake finds of a box is one of three things (`runsc.find`): nothing (its last stop removed its directory: start it), a leftover (a directory and no running container, what a wake or a stop cut short leaves: clear it, then start) or a running box, which outlived a daemon restart or a wake that was cut short. A running box is kept, with the commands in it, once fern-init says its services are ready. One that answers "not ready" is waited for, up to 45 s, longer than fern-init's longest pause before it starts a service again; one whose fern-init is silent for 1 s is stopped and started anew.

A box that is up but doesn't answer (a crash, one stopped by hand) is found by the next command, whose dial fails: it tells the actor the box is dead (`Exec.Dead`), and tries again, which wakes it once more (built). A death is counted once, however many commands find it: each names the wake it ran in. **Giving up (built):** a box that dies under its commands or fails to wake 3 times in 5 minutes isn't woken again until the first of those is 5 minutes old; until then every command fails at once with `ErrCrashing` (503), saying why it last failed and naming its log, rather than each paying for a wake that ends the same way. `fern wake` (a person) tries again at once. The count lives in `Run` between actors, as a box's actor ends when it sleeps; it starts over with the daemon.

### 6.3 Idle rule
An environment goes to sleep when all of these hold (two clocks, copied from Amp):
- **no exec is open** (a thread's Claude is one exec while T3 keeps its session), **no terminal is attached** and **no preview connection is active**
- **agent clock:** `idle.agent` (default 5 min) since the last exec closed
- **human clock:** `idle.human` (default 20 min) since the last human touch: an exec or terminal opened, or a preview request

CPU usage and running services are ignored. An explicit `hold` keeps it awake.

**Why a box is awake (built):** the rule is one function of what the actor counts (`sleepAt` in `internal/actor/idle.go`), which names the rule that decided as well as the time. Everything counted as running in a box is one list in its actor, each entry a command, a preview request or a job. `fern why <env>` prints that list for the environment's box and its agent box, with when each sleeps and by which rule (`GET /envs/{id}/holds`).

**Background commands (built)** are the exception to the first rule: one Claude starts with Bash's `run_in_background`, such as a dev server, is counted as a job of the box (`Group.Job`), but it doesn't hold the box awake as other execs do. With one running, the box sleeps on the two clocks, ignoring a Bash call's 5 s linger and a pre-wake's lease, and Claude's edits passed on to it (§10) count as touches of the human clock. That sleep ends the command: a dev server Claude forgot costs at most 20 minutes of an awake box.

Built: the actor's timer, with both clocks touched by every exec (`--idle-agent`, `--idle-human`; 0 turns it off), and previews: every request through the front door is an exec for as long as it lasts (`Group.Preview`), so a page whose dev server's live reload is connected keeps the box awake, and once it is closed the clocks run. Terminals and `hold` are planned.

**T3 keeps a thread's Claude running between turns,** in one exec, and ends it once the session has been idle (no turn, tool call, approval or background task). Unpatched T3 waits 30 minutes, so a box would stay awake about 35 minutes after a short turn, holding Claude's memory (200 MB and up). Fern's T3 image waits 5 minutes (`images/t3/fern-sessions.patch`; `T3CODE_PROVIDER_IDLE_TIMEOUT_MS` changes it): Claude exits 5 minutes after the last turn, and the box sleeps 5 minutes later, or 20 minutes after Claude started if that is later.

**Hibernation: Fern stops an idle Claude itself (built).** T3's wait is still 20 minutes of an awake box per short turn, and Claude waiting costs more than the box: in the lab, a box with Claude waiting uses about 1.8 s of CPU a minute and 146 MiB, against 40 MiB without it. So `fern exec --hibernate D` (fern-claude passes `$FERN_HIBERNATE`, default 30 s; 0 turns it off) stands in for Claude in T3's server (`internal/hibernate`). It reads the stream-json between Claude and its SDK; once Claude has had nothing to do for D (its turn ended with a `result`, no control request is unanswered either way, no background task runs), it sends Claude SIGTERM, which saves its session, and ends its exec with `?then=sleep`: the box sleeps at once unless another command runs, and T3 keeps its session as before. The SDK's next line starts Claude again with `--resume=<session>` (the flags that picked or forked a session are dropped), sends it first the setup the SDK gave the stopped one (the newest `initialize`, `mcp_set_servers`, `set_model`, `set_permission_mode` and the like), keeps their answers from the SDK, then the line. Only a Claude driven by its SDK (stream-json both ways, no terminal) is stopped; anything else runs as it is. A turn is under way from a user message to its result, counted: a message sent while a turn runs gets a result of its own, for the part it cut short, and the turn goes on (until 2026-11-13 that result made Claude look idle, and 30 s without a line in the rest of the turn stopped it). In T3 (`lab/t3-run.sh`, the idle step) the box sleeps 32.7 s after the turn, the same `fern exec` keeps T3's session, and the next turn, which wakes the box and resumes Claude, answers about the turn before it (lab).

**OpenCode's hibernation (built).** T3 calls an OpenCode server over HTTP and reads its URL from what `opencode serve` prints; it keeps a long-lived event stream (`GET /api/event`) and, if that drops, reconnects at once, which would wake the box again. So for `opencode serve` `fern exec --hibernate D` (fern-opencode passes `$FERN_HIBERNATE`, default 30 s) runs a proxy instead (`hibernate.Serve`): it listens at the `--hostname` and `--port` T3 gave, prints its own URL, and passes T3's requests on to the server in the agent box. It follows the server's events itself; once no request and no execution (`session.execution.started` without its end) has been under way for D, it sends the server SIGTERM and ends its exec with `?then=sleep`. T3's event stream stays open meanwhile, with the proxy's own heartbeat every 15 s (T3 gives up after 45 s of silence). The next request, with T3's password (others are refused without waking the box), starts the server again, adds back the MCP servers T3 added at run time (`PUT /api/experimental/mcp/…`, which a restarted server forgets; session instructions are in OpenCode's database), subscribes to its events and then goes in; each of T3's streams begins with the first server's `server.connected` only. OpenCode keeps its sessions on disk in the agent box's Claude folder, and its Claude Agent provider resumes Claude's session from them. A server that ends by itself takes T3's streams along, so T3 reconnects and asks what became of the turns it lost.

### 6.4 Pre-wake
A wake takes about 155 ms on the VPS (it took about a second as a pod, and 220 ms until 2026-11-11, when a wake stopped asking runsc for its list of boxes and making the network one command at a time), and a turn that finds its box asleep waits for it. When a turn is likely soon, T3's hook asks for the wake ahead (`POST /envs/{id}/prewake`), and the user's think time hides it:
- **a thread is opened:** T3's server streams an open thread to the app (`orchestration.subscribeThread`); one still open after 2 s is woken ahead. A thread only clicked past isn't: the stream's end cancels the wait. The app's sidebar opens the same stream for the first three threads it shows, which can't be told apart; those cost a wake and two minutes of an idle box each.
- **a thread is reverted:** after T3's rollback has finished (Fern's restore, git's restore and the deleted checkpoints' snapshots), so the wake holds up none of them and starts no services on half-restored files.

The daemon answers at once and wakes in the background, two pre-wakes at a time; up to 8 more wait, and further ones are dropped. A box that is awake is left as it is, its idle clocks included. A box woken ahead sleeps after `--idle-prewake` (default 2 min) unless a command comes first; the first command gives it the usual clocks (§6.3). A pre-wake holds its environment's actor during the wake, like any wake: a command that comes meanwhile waits for the same wake instead of starting another. `fern_prewakes_total` counts them by result (`woken`, then `used` or `wasted`; `awake`, `unknown`, `dropped`, `error`).

Measured when boxes were pods (lab, `lab/check.sh box`): a command 3 s after a pre-wake took 65 ms (p50), one to an asleep box 733 ms. In T3 (`lab/t3-run.sh toy 3`), the turn after a revert no longer waits for a wake, and the revert itself is unchanged (2.2 s p50).

---

### 6.5 The states in the code
The diagrams above are how an environment looks from outside. In the code, each thing that changes over time has one named state, written by one owner. An actor (a goroutine with a mailbox) owns it where several goroutines would change it at once; otherwise it is an enum under the lock its struct already has. What can be worked out from the state is a function, not a second field: when a box sleeps is `sleepAt`, a restart's pause is `backoffAfter`.

| What | States | Owner | Where |
|---|---|---|---|
| A box, to its actor | asleep, maybe up, up; and a lease: none, linger, prewaking, prewoken, retry | the environment's actor | `internal/actor` |
| A box, on the node | nothing, leftover, running | the driver, from the disk and runsc at each wake | `internal/runsc` |
| A box's answer | no answer, not ready, ready | fern-init | `internal/runsc` |
| A hibernated Claude | running, stopping, asleep, ending | `hibernate.Run`'s loop | `internal/hibernate` |
| The OpenCode proxy's server | stopped, starting, up, stopping | the proxy's actor | `internal/hibernate` |
| A background command, to the daemon | watching, exited, box stopped, lost | its watch, once | `internal/tools` |
| A command session, for its client | open, delivered, abandoned; and at most one connection | the session's lock | `internal/boxexec` |
| A service | running, waiting to start again, stopped | its supervisor | `cmd/fern-init` |
| A service started on demand | listening, starting, ready, failed (and back to starting) | its lock | `cmd/fern-init` |

Each type's comment lists its transitions and the function that makes each. The store's states stay on disk (a marker file, a leftover copy, a directory without its record, §9): there the disk is the record, and `Reconcile` reads it.

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
1. `POST /envs/t3-8f2c/fork {count: 3}` (or `fern fork -n 3 t3-8f2c`, or Claude's `env_fork`). The environment's actor picks the lowest free numbers, so two forks never take the same, and makes `t3-8f2c-fork-1` … `-3`: each an ordinary create (crash-safe the same way: the marker first) whose workspace, `envs/<fork>/live`, is a writable btrfs snapshot of the thread's workspace as it is, or of `{snapshot}`. Taking it flushes the workspace's writes first, so a running Postgres's data is as a crash would leave it, which it recovers from as from a revert.
2. A fork is `asleep` and wakes on its first command into a box of its own: its own services, its own Postgres on :5432, a token of its own like every box and an empty Claude folder. Its `.git` file still names the thread's worktree, whose git folder the box gets read-only, as the thread's does.
3. A fork's ID says whose it is (`store.ForkOf`): Claude's tools reach only its own environment's forks, and deleting an environment deletes its forks first. Forks sleep when idle like any environment; `env_exec` lets a fork's box sleep 5 s after its last command.

**Built** (`lab/check.sh fork`, VPS): three forks of a thread with Postgres running take 77–94 ms; woken and each changing and counting its own database at the same time, 2.1–2.7 s. **Not built:** the identity hook (fresh secrets and ports; a fork's services get the same configuration as the thread's), T3's own Fork, which shares the thread's worktree (design §10.4), and forks on another machine.

### 7.5 Golden build
1. A builder session on a fresh subvolume: clone at the commit → `.agents/setup` (time limit 20 min; must be safe to rerun).
2. **Kill leftover processes; strip credentials** (anything injected for setup); verify nothing secret remains (a scan).
3. Read-only snapshot → `golden/<repo>@<key>`; record it in `goldens`.
4. **Incremental:** when the key changes, start from the previous golden, so `install` is warm.

**Built,** one build at a time, in the daemon (`internal/actor/golden.go`): the repository's builder environment (`golden-<repo>-<hash>`, listed by `fern ls`) gets a fresh workspace with a copy of the `golden-source` checkout. If the repository has a setup script (`.fern/setup`, else `.agents/setup`; `setup.Script`), the builder first wakes as a box with the tools the repository pins (§8) and no services (`runsc.Setup`), and the daemon runs the script there as a command of the box, as a program if it may and with `/bin/sh` if not, for at most `--setup-timeout` (an hour); the box is stopped again. Then it wakes as a builder, so its services make their first start (`initdb`, seed), sleeps, which stops them cleanly, and loses every file the copy brought in; what is left is snapshotted as `golden-<key>`, and the builder keeps the newest 3 (a golden with a toolchain in it is gigabytes; a thread started from one that goes keeps its files, as its workspace is a copy). A script that fails or runs past the limit fails the build, with the start and end of what it printed in the daemon's log, and leaves no golden. A build that failed isn't made again from the same key for 15 minutes (`buildRetry`): every new worktree without a golden asks for one, and a script that fails would otherwise run again for each. A builder's box has 8 minutes to wake, not 2, and 6 of them to install the tools (a thread's box has 90 s); if it can't, it doesn't start and the build fails, rather than leave a golden without them, whose threads would fail at their first command with nothing to say why. The build is three changes of the builder, not one: a change holds its actor's mailbox and has ten minutes, and the script runs between them. Its key is the checkout's as it came, before the script ran: a script may rewrite a lockfile. A builder's boxes get the limits the repository asks for its own box (§10): a script sized for that box doesn't fit a smaller one. Until 2026-11-12 no script ran: a compiler's repository got a golden with its tools alone, and its thread spent its first 39 minutes building a toolchain that the next thread would have built again (VPS; `lab/check.sh golden`). A build that stops leaves no snapshot, and the next starts afresh. A golden holds what the repository's code made, so it is the agent's: a new worktree goes onto it through `os.Root`s on both sides (no symlink in the golden or the worktree leads a file, as root, out of them; files are opened without following symlinks or blocking), and a build starts from an empty workspace rather than the last one's. The builder's box gets no Claude login and no Claude folder, so no credential can reach a golden. The key is computed again from the builder's copy of the source snapshot, which can't change, so a golden is published under the key of what it was built from; a source still waiting at a restart is built then; a build that stopped half-way leaves its builder a workspace (`Reconcile`); a new environment whose golden can't be used (pruned since it was looked up) starts without one. **Not built yet:** `prepare`, step 2's scan, the `goldens` table (the builder's snapshots are the index), a build that survives a daemon restart (it starts again from the checkout), and incremental builds. An incremental build would start from the last golden, which holds the services' first start too: a seed that changed (`golden.inputs`) would then not be loaded again, as the database is there already. It needs the repository to say its services' data doesn't depend on the build's inputs, or a build that keeps the script's output and makes the services' again. Build output that names its absolute path (a Python venv's scripts) points at the builder's workspace, not the thread's, and caches keyed on it are rebuilt in each thread (Vite's dependency optimizer). mise's tools don't name it.

### 7.6 Delete
Stop the session → delete `live/` → apply retention to snapshots (keep tagged) → remove the record.

---

## 8. The setup contract

Read from the repo, in order of preference:
1. **`.fern/env.ts`** (later): a typed definition (Alchemy-style) with cached steps, services and optional per-thread external resources (stage = thread).
2. **`.agents/setup` + `.agents/prepare` + `.agents/resume` + `services.yaml`**: compatible with Amp and Delta, which both read `.agents/setup`. `.agents/linked` lists ignored local files, such as `.env`, to bring in. A file in `.fern/` replaces the `.agents/` file of the same name; the two are never merged (Delta does the same with `.delta/`).
3. **`devcontainer.json`**: `onCreateCommand` / `updateContentCommand` go into golden, `postCreateCommand` runs after.
4. **T3's `t3.json`** worktree scripts.
5. **Nothing:** `fern init` asks an agent to write (2) and proves it by building golden.

**Rules** (adopted from Amp and Delta):
- `setup` must be **safe to rerun**, finish in ≤20 min (the daemon stops one after an hour, `--setup-timeout`), **leave no processes running**, and use **no personal credentials**. It runs in the golden build (built, §7.5), in the workspace, with the repository's tools on its `PATH`, and what it leaves in the workspace is in every new thread. A thread whose golden wasn't built yet doesn't have it: Claude's hint names the script and says to run it then.
- `setup` starts **no services**, and none runs yet when it does: golden is taken before the environment belongs to a thread. Services start from `services.yaml` or `resume`.
- `prepare` runs in **every new workspace**: before `setup` in a golden build, and once in each new environment (from golden or a fork) before the first `resume`. It must be fast: downloads and builds belong in `setup` (Delta's rule).
- `linked` paths come from the project's main repository (`repos/<repo>/`) and are mounted read-only into the box, **outside `live/`**, so they never enter golden or snapshots. They are not mounted while `setup` runs. (Delta symlinks them to one shared copy instead.)
- `resume` runs on **every wake** and when `fern-init` reconnects without a sleep, so it must be **safe to run twice**. A turn waits for it at most 10 s; after that it keeps running in the background. It's for logins, tunnels and machine identity.
- Services must respect **`PORT`** and **`PUBLIC_URL`**. **A running service does not keep the environment awake.**
- **Tools (built):** a repository pins its tools (Node, pnpm, bun, Python, …) with mise's files at its root: `mise.toml`, `.mise.toml`, `.tool-versions`, `.config/mise.toml` or `.config/mise/config.toml`. The box image has mise and none of the tools. The environment's box (fern-init) installs what they pin into the workspace's `.fern/tools/` (mise's data folder; a `.gitignore` of `*` keeps it out of git) before it starts the services, unless `.fern/tools/.installed` holds the hash of the same files; mise's download cache stays in `/tmp`, out of snapshots. Every command and service runs with mise's shims first on `PATH`, so each runs the version its folder pins. The tool files are part of the golden key, and a repository with only them gets a golden: its build installs the tools once, and every thread forks them with it (`lab/check.sh tools`: Node 22 and pnpm 10 for TanStack Start, golden build 41 s with the dependencies, VPS). A thread whose golden missed installs them at its first wake (Node 22 and pnpm 10: 12 s in a box, against 2.7 s under runc, VPS), within 90 s; an install that fails or runs over is logged, the box starts without them, and Claude's hint says what a missing tool means and to run `mise install` (in a loaded lab the same install took 91 s once, and the next command was `pnpm: not found`). A program a command names is found on the PATH it runs with (boxexec), so `node` is the pinned one without a shell. The agent box doesn't install; it shares the workspace. **Later:** the repository's Dockerfile or `devcontainer.json` for system packages.
- **Guessed services (built):** a repository with no `services.yaml` but a `package.json` with a `dev` script gets one guessed (`setup.MakeGuess`), so a project T3 cloned from a URL runs without Fern's files. The package manager comes from `packageManager` or the lockfile (pnpm, bun, yarn through corepack, else npm); the dev server's flags and port from the framework it depends on (Next.js, Nuxt, Astro, Vite and those that run it), else `PORT`; Postgres from a `docker-compose.yml` that runs one, with its role and database and `DATABASE_URL` for the dev server; its data folder, `.fern/pg`, ignores itself for git, as such a repository has no `.gitignore` for it and T3's checkpoints and the commit tool take every untracked file. Without tool files of its own, it also gets Node (`.nvmrc`, `.node-version`, `engines.node`, else LTS) and the package manager through mise, from `.fern/tools/guess.toml` as mise's global config, which a repository's own files override. The daemon (golden key, default preview) and fern-init (services, tools) read the same guess; the files it read are in the golden key. Claude's hint says the services were guessed and how to replace them. `lab/check.sh guess`: TanStack Start with its Fern files removed and Postgres in `docker-compose.yml`, golden build 26 s, page through the preview 29 s from asleep (VPS).
- **Cloudflare repositories (built):** `setup.IsCloudflare` says a repository deploys to Cloudflare: wrangler's config at its root, or a dependency on `wrangler`, `@astrojs/cloudflare`, `@cloudflare/vite-plugin` or `@opennextjs/cloudflare`. Its dev server (wrangler's, or the framework's adapter, which runs workerd) emulates D1, R2 and KV in `.wrangler/state` in the workspace, so a turn's snapshot holds their data and a revert brings it back (`lab/check.sh cloudflare`); the guess gives a plain Worker wrangler's flags and port 8787. Claude's hint says so, and that deploys happen by pushing (Cloudflare's Workers Builds), so no Cloudflare credential is needed in a box. With a token on the node (§13), the agent box's Claude also gets Cloudflare's MCP server. Brief: [briefs/cloudflare-projects.md](briefs/cloudflare-projects.md).
- **On demand (built):** a service with `listen: unix:PATH` or `listen: tcp:PORT` starts at the first connection there, not with the box: fern-init holds the socket, the box is ready without the service, and the first connections wait and are relayed once its `ready` passes. If it isn't ready within a minute they are dropped, with what its supervisor says of it (how often it exited, and how); the supervisor goes on starting it, and on a unix socket the next connection waits for it again. A unix socket stays fern-init's until then (the service replaces the socket file, as Postgres does, and later connections reach it directly); a TCP port is let go at the first connection, and one in between is refused. The port a service is previewed on (`port`) is held on every address, as the preview comes to the box's address; any other on localhost. The guess's dev server starts this way: a wake doesn't wait for it. A golden builder's box starts every service at once (`FERN_ON_DEMAND=off`), as their first start is what it builds.

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
limits:                  # built: the box's size, each up to the node's most (§17); left out, the node's default
  cpus: 4
  memory: 4096           # MiB
  pids: 8192
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
  envs/<id>/env.json             the environment's record (outside the workspace, so in no snapshot; root only)
  worktrees/<repo>/<name>/       writable workspace of an environment at a path: T3's worktrees
  boxes/<box>/                   a box's /tmp and log while it may run (its bundle is on the node's tmpfs)
  images/<id>/  images/<tag>     the box and T3 images, unpacked (fern image load)
  userdata/ caches/ tools/       T3's own state: T3's home is the pool, so its worktrees land in worktrees/
  repos/<repo>/                  the projects' main repositories, which T3's worktrees point into
  projects/<repo>/               the same, for the projects T3 cloned from a URL
```

Only `live/` and `worktrees/` are workspaces; Fern never snapshots T3's state or the main repositories.

- **Snapshot:** `btrfs subvolume snapshot -r`. **Fork:** the same without `-r`. **Restore:** snapshot to `live.new`, then swap. All are metadata operations, whose time doesn't grow with the workspace. A snapshot first flushes what was written since the last one: about 1 ms with nothing written, 24 ms after 8 MB (measured), and more on a busy disk.
- **Consistency:** a snapshot is atomic (crash-consistent). Postgres recovers through its WAL; SQLite needs its WAL file kept beside it.
- **Retention:**
  - keep the last N turn snapshots per environment, plus tagged snapshots, plus golden (built: the newest 50, pruned after every snapshot; a builder's newest 3 goldens, pruned after every build; no tags yet)
  - keep `pre-restore` copies (§7.3) for a day (built)
  - expire unused goldens after M days
  - **space pressure → refuse new snapshots with a clear error** rather than fill the disk (built: below 5% of the pool and 2 GiB free, new snapshots and environments get `ErrPoolFull`, HTTP 507; restores still run). **The disk under the pool counts too (built):** the installer's pool is a sparse file on the server's disk (`/var/lib/fern-img/pool.img`), so it can only grow into what that disk has free. The store finds the file from the pool's loop device (`diskUnder`) and applies the same limit to its disk, with an error that names it and `fern gc`. Until 2026-11-12 only the pool was looked at: on the VPS it had 27 GiB free on a disk with none, Podman's build leftovers had taken 35 GB of that disk, and the first write that failed was T3's, which lost a running turn.
  - **what nothing needs is removed (built, `internal/disk`):** `fern gc` removes the unpacked images no tag points to and nothing runs from (`fern image load` keeps a tag's previous image for the boxes still running from it; a mount table says whether one does), Podman's images that no container uses (the pool has its own copy; the next build or install fetches again what it needs) and every environment's snapshots past their retention, which otherwise go only at that environment's next snapshot. The daemon does the same by itself when, at one of its looks each minute, the pool has less than twice the limit free or the disk under it less than that or a tenth of itself, and then not again for an hour; Podman's images go only when it is the server's disk that is low, as they are not in the pool (until 2026-11-13 a pool that was low by itself had them pruned every hour, for nothing, and the log said "freed" with nothing freed). With nothing left to free it says so, and what to do. A builder's goldens beyond its newest 3 go with the snapshots past retention. **Not seen by the guard:** btrfs's room for its own records. A pool whose device is all allocated can't add to it, and then fails writes with free space showing (2026-11-13: 3.4 GiB free, 1 MiB unallocated, metadata three quarters used; the pool was grown from 50 to 60 GB by hand). `fern doctor` reads it (`pool records`); the guard doesn't. `fern df` shows the two disks and what uses them, with what `gc` would remove. At the source: `install.sh` removes other releases' images from Podman after it has loaded one, and `lab/build.sh` on a VPS the untagged ones older than a day (`lab/check.sh space`).
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
| Image | read-only Debian base with Postgres, git, curl, xz, unzip, Node and Claude Code (pinned, checksum-verified; no compiler, `make` or Python: a repository brings those with mise or its setup script), unpacked into the pool (`fern image load`); each box gets an overlay on it for its mount points |
| Main process | `tini` as PID 1 (reaps orphans) → `fern-init`: services, exec/PTY server, readiness (`/readyz`), graceful stop (`POST /stop`, which names the wake it stops, so a late request can't stop the next wake at a reused address) |
| Mounts | the workspace at **the same path T3 uses**; Claude's folder `envs/<id>/claude/` (`CLAUDE_CONFIG_DIR`) at its own path, **outside the workspace**, so a restore doesn't rewind its transcripts; the main repository's git folder and the worktree's `.git` file **read-only** (§13) |
| User | non-root, the same uid as T3 (1000), no capabilities, `no_new_privs`, a read-only root |
| Runtime | gVisor (`runsc`) for every box; planned: classes (§2) |
| Limits | 2 CPUs, 2 GiB of memory, 4,096 processes, a cgroup per box under `/fern`. The daemon's `--box-cpus`, `--box-memory` and `--box-pids` change them for services' boxes and golden builders', from a box's next wake on; an agent box keeps these. A repository asks for its own under `limits:` in `services.yaml` (§8), up to `--box-max-…`. One that asks for more gets the most and nothing said by the daemon, so Claude's hint says it: an agent box learns what its environment's box gets (`FERN_BOX_LIMITS`), and fern-init names what falls short and the flags the person raises on the server (until 2026-11-12 a repository that asked for 4 CPUs and 5 GiB ran with 2 and 2 with no word of it, and someone had to read the daemon's flags to find out) |
| Network | a network namespace and veth per wake, a /30 from `10.89.0.0/16`; the internet only (§13) |
| Stop | `POST /stop` to fern-init (the services' `stop:` commands, then SIGTERM and kill), `runsc kill` after 15 s; then the network, overlay and cgroup go |

**Two boxes per environment** (the default, `--agent-boxes`; [briefs/claude-outside-the-box.md](briefs/claude-outside-the-box.md)). The **environment's box** runs the services and gets no Claude folder or login. The **agent box**, `<env>.agent`, runs Claude (`fern exec --agent`, which fern-claude passes) with its file tools and no services; its shell tools are replaced by the daemon's MCP Bash, which runs each command in the environment's box, waking it, and lets it sleep 5 s after the last. A restore, sleep or delete stops both; a pre-wake wakes the agent box. A golden builder's box runs the services, started at once, and gets no Claude at all. The actor decides this in one place (`Group.boxOf`).

**Bash runs bash (since 2026-11-13).** The tool's commands ran with `/bin/sh`, which in the box is dash: `make &> log` put make in the background and made an empty log with exit code 0, under a tool that says to use it as one would Bash. A command whose box goes away under it (a restore, a sleep, or the box stopped for its memory) answers with what it had printed and a sentence saying so, where it answered with a WebSocket error alone; one that never ran because the box was still starting when its timeout passed says that.

**Claude's background commands (built).** A Bash call's command, its process group included, ends with the call, and the box sleeps 5 s after the last one: `npm run dev &` would die at once. So Bash takes `run_in_background`, as Claude's own Bash does: the environment's fern-init runs the command (`background.Jobs`), in its own process group and tied to no connection, keeps its output (the newest 1 MiB not yet read), and the daemon answers with an ID (`bash_k3x9…`); `BashOutput` returns what it printed since the last read and whether it runs, at once or, with `wait_seconds`, when it has ended or that long has passed (at most ten minutes); `KillShell` ends it with its process group. A wait counts as a command of the box, so the box stays up for it: a job alone doesn't hold a box past its idle times. Without it Claude waits by running `sleep` in Bash: on the VPS (2026-11-12) 208 of a thread's 324 Bash calls were such looks, and its context ran out twice. Up to 16 per box, finished ones included. When one ends, the daemon takes what it had left to say and how it ended from the box and keeps them for BashOutput: the box sleeps 5 s after Claude's next Bash call and its next fern-init knows no jobs, so until 2026-11-12 a command that had finished was often gone by the time Claude asked. While it runs, the daemon counts it as a job of the box's actor (`Group.Job`), watching it with a long-poll (`GET /jobs/{id}/wait`). A job outlives the Bash call, Claude's hibernation (Claude stops 30 s after its turn and its box sleeps; the dev server's box stays up) and the daemon: a restarted daemon asks every awake box for its jobs (`AdoptJobs`) and counts them again; their boxes' idle clocks start over then. A job ends with KillShell, the box's sleep on its idle clocks (§6.3), a restore or a delete. Claude's hint says to start dev servers this way. Checked by `lab/check.sh devserver`, which starts Vite through MCP Bash, as Claude does: a minute later its box is awake and Vite serves; after `systemctl restart fern-daemon` Vite still serves and BashOutput reaches it; after KillShell the box sleeps 5 s after the last call.

**Claude's edits reach the dev servers (built).** gVisor tells a box only of its own changes: a dev server (Vite, Next.js) in the environment's box never hears of the files Claude writes from the agent box, and keeps serving the old ones. One sandbox with both as containers doesn't help: events cross only within one container's mount, and gVisor shares bind mounts between containers only for tmpfs ([lab/spike/watch/RESULTS.md](../lab/spike/watch/RESULTS.md)). So while the environment's box is awake the daemon watches its workspace from the host with fanotify (`internal/watch`), whose events name the writing process; it keeps those in the agent box's cgroup, outside `.git`, `node_modules` and `.fern`, gathers them for 20 ms, and asks the box's fern-init (`POST /touch`) to set each file's times to what they are. That is an attribute change inside the box, which its watchers hear: Vite reports `change`, and `add` for new files and folders. The box's own touches are its writes, not the agent box's, so nothing loops. The watch is the environment's box's, not the agent box's: the agent box often sleeps right after a write, and a watch that ended with it would drop the edits it hadn't passed on yet (it did, in the lab). Not passed on: deletes and renames away (Claude's file tools don't make them), and files changed while the environment's box was asleep, which its next wake reads as they are. Checked by `lab/check.sh devserver`.

**Measured on the VPS** (2026-10-28, median of 3): a wake 253 ms and a sleep 90 ms at p50, against 993 and 1,221 ms as pods; 45 MiB per box with 20 awake.

## 11. Front door (wake layer)

Built for previews (`internal/preview`, §5); webhooks are planned. The actor gives every request that comes during a wake the same wake.

- **Entrances:** the unix socket (T3, CLI, MCP), HTTP previews (Host-header routing), webhooks.
- **Wake coalescing:** the first touch on an asleep environment starts **one** wake; later touches wait on it.
- **Rendering ahead (built):** when an environment's own box wakes (not its agent box or a golden builder's), the actor names it on a channel (`Config.Woken`), and the front door asks its default preview for `/` once, by the preview's name, as a browser's first request would, retrying while the dev server starts (`Server.Warm`, 2 min at most). A dev server compiles a page at its first request: TanStack Start's takes 10 s in a box, so the page a person opens after Claude's turn woke the box comes in 0.11 s instead of 16.6 s (`lab/check.sh tools`, VPS). A wake a browser's request made isn't rendered twice: that request is the first.
- **Holding (built):** a preview request waits for the wake (about 155 ms), then up to 5 s for the port to listen; a wake that fails is **503 + Retry-After**. Planned: health and probe requests get the 503 at once while waking.
- **Activity (built for previews):** a preview request is an exec of the box's actor while it lasts (§6.3). Planned: attached terminals.

---

## 12. Failure handling

| Failure | What happens |
|---|---|
| **Daemon crash** | on restart, `Reconcile` reads what each half-done change left on disk (markers, `live.new`, a directory without its record) and finishes or clears it; boxes left running get an actor, so they still sleep when idle (built) |
| **Box crash** | the next command's dial fails and the actor wakes the box once more (built); after 3 crashes or failed wakes in 5 minutes, commands fail at once with `ErrCrashing` until the window has passed or a person wakes it (built) |
| **Crash mid-turn** | the agent CLI resumes from its session files; Fern can restore to the last turn snapshot before resuming if the transcript is broken |
| **Restore during an exec** | the box stops and the actor cancels the exec, so no late write from the old session reaches the restored disk (built) |
| **Disk full** | snapshots and new environments refused with "the pool is almost full" (HTTP 507) before the pool is, or the disk it is a file on; the daemon removes what nothing needs when either is low (§9); retention runs at every snapshot; the box keeps working (built). Not built: a limit on what one box may write, so a thread can still fill the disk faster than the daemon looks |
| **Snapshot fails at a turn's end** | the turn still counts: T3 keeps its git checkpoint and the hook logs the error; reverting to that turn is refused with the reason (§7.3) |
| **Machine loss** | restored on a new machine from the hourly backups (built): `install.sh` with `FERN_RESTORE_FROM` brings back each thread's workspace as of its last backup, its record, T3's data and the repositories; turns older than that backup can't be reverted to |
| **Golden build fails** | no golden is made, and the failure is kept with the builder (`envs/<builder>/build-failed.json`: the key, when, and why, with what a setup script printed), so a restart of the daemon keeps it. `fern doctor` lists it, and a thread whose checkout has that key has its Claude told at its box's next start, to tell the person (§16, built 2026-11-13: until then it was a line in the daemon's log). New threads start without a golden, as before the first build; the same key isn't built again for 15 minutes; a build that works removes the record (built). Planned: the environment created from the previous golden |

---

## 13. Security

The threat model is in [briefs/claude-outside-the-box.md](briefs/claude-outside-the-box.md); reporting is in [SECURITY.md](../SECURITY.md).

- **Trust boundaries:**
  - the daemon is privileged (it manages btrfs, mounts and the boxes)
  - **boxes are untrusted:** they run what an agent writes
  - T3 and the CLI are trusted local callers, through the socket's group
  - an agent box's MCP Bash is authenticated with that agent box's own token, made at its start, and reaches only its environment's box; the box that runs the repository's code has a different token, so the firewall is no longer the only thing between it and the agent's tools and exec port
- **A box sees only its own folders:** its workspace, its Claude folder (the agent box only) and its repository's git folder read-only, as bind mounts in its OCI spec. Nothing else of the host.
- **A box can't make T3 run its code.** T3's server runs git in the worktrees, as the box's user. The box gets the main repository's git folder read-only, and the worktree's `.git` file read-only too, so it can't point git at a folder whose config runs commands (such as `core.fsmonitor`). The daemon runs git there too, for Claude's commits (§5), under the same protection and with hooks off. `fern-claude` never runs Claude in T3's process for a folder in the pool's worktrees. Checked by `lab/check.sh t3`.
  - **That protection has a hole, found by a review on 2026-11-13: a repository in a folder of the worktree.** The box can make one (`git init vendor/x`), with a config of its own. While it is only an untracked folder, git leaves it alone. Once the index holds it as a submodule, `git add`, `git status`, `git diff` against the worktree and `git commit` with paths each start a git in that folder, and that git runs the filters its config names: the box's program, on the node, as the user in the daemon's socket's group. Reproduced with plain git 2.50: `git add --all` twice was enough, with hooks, the file monitor and signing all off. No setting stops it: `diff.ignoreSubmodules=all` is undone by a `.gitmodules` in the worktree, which the box writes; only `--ignore-submodules=all` on the command line holds.
  - **Closed for Claude's commits (built):** `internal/commit` no longer uses `git add` or `git commit`. It stages with `diff-files --ignore-submodules=all`, `ls-files --others` and `update-index`, builds the commit's tree in an index of its own in the worktree's git folder, and commits with `write-tree`, `commit-tree` and `update-ref`, none of which looks into a folder's own repository. A folder with one is left out, and the tool tells Claude; a submodule that reaches the index all the same is put back to what the last commit has. So the tool also never arms the index for T3. Tests plant the repository three ways (a new folder, a submodule the repository has, a tracked file replaced by one), with the `.gitmodules`, and commit several times each; `lab/check.sh commit` does it through the tool, from a box.
  - **Open for T3's own git (not built):** T3 runs `git status` and `git diff` in every worktree, and stages with git itself when the person commits from the app. In a repository that has a submodule, the box can fill the submodule's folder with a repository of its own, and T3's next `git status` runs its filter; in any repository, the same follows a commit made from T3's app while such a folder exists. It needs code in the box that means harm (a dependency's install script, a model that was talked into it), and gives it T3's service: every worktree, the daemon's socket and the logins in T3's environment. What would close it, none built: T3's git as a user that doesn't own what the box writes, so that git's own ownership check refuses the folder's repository; or git run where it can start nothing; or a `git` on T3's path that adds `--ignore-submodules=all`. Until then: don't work on a repository with submodules in a thread whose code you don't trust, and commit through Claude, not T3's app.
- **Isolation:** gVisor, a non-root user, no capabilities, `no_new_privs`, a read-only root, cgroup limits (§10).
- **Credentials:**
  - Claude's login (`CLAUDE_CODE_OAUTH_TOKEN`) is in `/etc/fern/claude.env`, which only root reads; the daemon reads it at every wake and gives it, as an environment variable, only to the box that runs Claude (§13.1). It is never in a record or a workspace Claude's box takes nothing from the workspace that it would run: no project settings, hooks or MCP servers (`--setting-sources=user`), and, since 2026-11-13, not the workspace's tools on its PATH. Until then fern-init put `<workspace>/.fern/tools/shims` first there in every box, the box with the services writes that folder, and a file named `claude` in it ran in Claude's box at its next start, with the login in its environment (found by the same review; `lab/check.sh t3` plants one). Not checked: what OpenCode in an agent box starts from the workspace (formatters, language servers).
  - **never in a workspace, golden or snapshot.** Claude's state lives in `envs/<id>/claude/`, outside the workspace; with the login in its environment Claude writes no credentials file. A golden builder's box gets no login (§7.5)
  - GitHub (built): T3's server pushes and opens pull requests, never a box (the repository's git folder is read-only there). Its git asks `fern git-credential` (set in `fern-t3.service`) and its `gh` asks `fern github-token` (`images/fern-gh`) for a token; the daemon mints it from the person's GitHub App (`internal/github`; its ID and key in `/etc/fern/github-app/`, root only): for the one repository, with contents and pull requests only, for an hour, renewed five minutes before. T3 itself asks differently: once for the host, in its own folder and no repository (`gh auth token --hostname github.com`), and uses that credential for every thread's pull requests. There the wrapper gives it a token for the account the App is installed on (`fern github-token --account`, `App.AccountToken`): every repository of that installation, with the same permissions and hour. That is no more than T3 can get already, a repository at a time. Until 2026-11-12 the call got nothing: T3 had no GitHub credential on a Fern server, and its pull request lookups failed once a minute for every thread (`lab/check.sh github` asked for a token by repository, never as T3 does). With an App's token GitHub answers T3's GraphQL (`viewer` is the App's bot) and refuses `GET /user`; what T3 does with pull requests through it has not been tried in the app. The request goes over the API's socket, which boxes can't reach, so no token is in a box, a workspace or a snapshot. Without an App, or one installed on the repository, git and gh go on without a token. `lab/check.sh github` checks it against a fake GitHub.
  - forks get no credentials (built: an empty Claude folder, and a token of their own as every box has); planned: fresh secrets in the identity hook
- **The boxes' network:** an nftables table of their own (`inet fern`, replaced whole in one transaction at every daemon start) drops what boxes send to private, shared, loopback, link-local and multicast ranges and everything they send to the node except answers; only the daemon, on the node, connects to a box. Agent boxes alone may reach the daemon's MCP port. Boxes resolve names with public resolvers (`--box-dns`). Checked by `lab/check.sh`: another box, the node's ports (T3, the daemon's metrics) and the metadata address time out; the internet answers.
- **The machine's network:** T3's port must never be public, as anyone who reaches it uses the owner's subscription (§13.1). On a VPS, `lab/vps.sh` installs a firewall (`inet fern_host`, a table of its own) that lets in only SSH; reach T3 through an SSH tunnel or a tailnet. The daemon's metrics and profiles listen on localhost.
- **Known gaps:** boxes of the same repository share its read-only git folder, so one can read another's checkpoints (the full fix is a repository per thread); a box's own token is readable in `/proc` by that box's processes, in fern-init's first environment (it opens only that box's fern-init, which they are already inside of, and for an agent box that environment's tools, which its agent has anyway; it is no longer in Claude's arguments); the Cloudflare token, where one is set up, is still in the agent's arguments; user namespaces inside gVisor aren't set up.
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

**Providers:** Claude Code and Codex through their own CLIs are supported. Blocked: Claude logins through other agents (pi, OpenCode's plugins), which Anthropic prohibits; OpenCode's Claude Agent provider (2.2.3, 2026-10-30) is not one, as it runs the unmodified `claude` with its own login through the Agent SDK, and Google Antigravity / Gemini consumer logins, which Google forbids wrapping. Fine through T3 if wanted later: pi and OpenCode with ChatGPT, Copilot or xAI logins, Cursor, Muse. Grok's login in a container is undocumented.

---

## 14. Running modes

One remote Linux machine running two systemd services. `lab/setup.sh` (with `FERN_HOST` for a VPS, through `lab/vps.sh`):
- creates the btrfs pool (a loop-mounted image file), installs gVisor and a firewall that lets in only SSH
- installs `fern` and `fern-daemon` in `/usr/local/bin`, loads the box and T3 images into the pool (`fern image load`) and installs `deploy/systemd/` (`fern-daemon.service`, `fern-t3.service`)
- stores Claude's login in `/etc/fern/claude.env`, runs the quick checks and prints the pairing link

An update restarts the daemon, which running commands and turns carry on through (§3), and T3's server, which holds the turn and ends it. So `install.sh` waits up to ten minutes for the running turns before it restarts T3 (`FERN_DRAIN`), and then restarts it; `lab/build.sh` on a VPS waits as long and then leaves T3 running its image from before, saying so, until it is run again with the thread idle (on 2026-11-12 it restarted T3 at once, and a day's fixes waited eight hours on one working thread because of it).

The local lab is the same machine as a systemd container in Docker (`lab/up.sh`). Your Mac or phone only runs the T3 app. Against the earlier k3s install, this saves what k3s and containerd cost while idle: about 780 MiB and 9% of a core.

**Backups (built):** with `/etc/fern/backup.env` (root only; `install.sh` writes it), the daemon backs up hourly (`--backup-every`), and `fern backup` now (`internal/backup`). Each environment whose workspace's btrfs generation moved since its last backup gets a snapshot through its actor (`fern-backup-<time>`), sent with `btrfs send`, incremental from the last one (which is then deleted; a full send every 168, or when the parent was pruned). Around them goes an archive of the state: each environment's record, T3's data with its databases copied by SQLite (its `server-owner.sqlite` is only a lock), the projects' repositories and each environment's Claude folder. Everything is gzipped and encrypted on the node (AES-256-GCM in 64 KiB chunks, the STREAM construction) with a key the person's Mac keeps, and uploaded to an S3 bucket in parts by a small SigV4 client (no SDK). A manifest, written last, names what makes up the backup; what a backup that stopped left (objects, snapshots) the next one deletes. `fern-daemon restore NODE` receives each workspace's chain into a new pool, then the state. `lab/check.sh backup` runs it against versitygw on the node: a backup, an incremental one, none for an unchanged thread, and a restore into another pool that matches the snapshot file for file.

**Scaling (planned):** boxes scale within a machine, since they sleep when idle; more machines, each with its own pool and T3, form cells, and a thread's disk moves with `btrfs send`.

## 15. T3 integration

| T3 seam | Fern call | Status in T3 |
|---|---|---|
| `VcsCheckpointOps.captureCheckpoint(cwd, ref)` | `POST /envs/{env}/snapshots {id: ref}` | exists; needs a ~100-line hook |
| `restoreCheckpoint(cwd, ref)` ("Revert files too") | `POST /envs/{env}/restore {snapshot: ref}` | exists; same hook |
| `deleteCheckpointRefs(cwd, refs)` | `DELETE /envs/{env}/snapshots/{ref}` | exists; same hook |
| Thread fork | `POST /envs/{env}/fork` | built |
| `SandboxService` (register / start / exec / stop / remove / ports) | env create / wake / exec / sleep / delete / previews | open upstream PR (#15350); Fern would be a backend |
| MCP | `env_*` tools | exists (T3 exposes MCP to agents) |

**The environment ID is derived from T3's thread ID.** T3's worktree path *is* the environment's `live/`. Fern **never snapshots T3's own database.** Rewinding it would resurrect revoked logins; T3 keeps its own history.

**A new thread is a Fern environment, with no setting to change (built).** Only a *New worktree* thread gets an environment: a thread in *Local* works in the project's main folder, which has no box or database, and there a Revert brings nothing back (seen in the lab and on the first VPS). So:
1. **T3's defaults on Fern's server** (`images/t3-settings.js`, which `fern-t3.service` runs before T3 starts, only where the person hasn't chosen): new threads are *New worktree* (the server-wide `defaultThreadEnvMode`, so nothing happens per project), and deleting a thread removes its worktree (`storageCleanup.worktreeOnDelete`), which takes its environment with it (§9). T3 keeps a deleted thread's worktree when git shows changes in it, and by default untracked files count: with `worktreeKeepWhen: tracked-changes` it cleans those away and keeps only a worktree with edits to tracked files that were never committed, whose environment then stays, with no word of it (not built: removing those too, or saying so). Codex is off, as the image has none and T3 writes pull request text and commit messages with it unless it is (roadmap step 8), and so are T3's own browser tools, which the server can't run. The app reads the setting from the server, so the composer shows *New worktree* from the start. T3 overrules that default for the first thread of a project whose folder is directly in its own projects folder (`projects/`, where it also puts the repositories it clones): it takes such a project for one it just made from a name, with one commit, and starts the thread as *Local*, which step 2 then refuses (seen on the VPS, 2026-11-12, with a repository added by its URL). So T3's server also gives every project it adds *New worktree* as the project's own setting (`images/t3/fern-newthread.patch`), which T3 doesn't overrule. A project added before that keeps the fault for its first thread only.
2. **`fern-claude` refuses a thread's session anywhere that isn't a Fern workspace**: a project's main folder in `repos/` or `projects/`, or wherever T3 cloned it (its home, which is `projects/`: until 2026-11-10 it was `/home/node`, outside the pool, and a Local thread in a clone there ran Claude unboxed), with a message naming *New worktree*. T3's probes and the commit messages it writes there still run: T3 starts sessions with `--session-id` or `--resume`, and those without. It also keeps Claude out of T3's own process, with its login and the whole pool. T3 doesn't show the message: on the VPS (2026-11-12) the thread only failed, and T3's log had `claude-query-stream-failed` without it. Not done: passing the message on to the thread.
3. **Claude is told where it is:** fern-init writes a `CLAUDE.md` in Claude's folder (outside the workspace, so in no snapshot or diff): its own services and data (which the hint named only without agent boxes until 2026-11-13: an agent box runs none, and was told of none), a `services.yaml` that can't be read and that Bash doesn't work until it can (Claude's box starts all the same, to mend it), that a revert brings data back, that only the worktree lasts (the box stops seconds after a command and starts again with an empty `/tmp`: a thread on the VPS kept a built program there, and still had it only because a background command happened to hold the box up), that it commits with the `commit` tool, not git, and doesn't push, and, for a repository with a setup script, that the script's output is normally there and when to run it itself.
4. **A terminal in T3 is a shell in the thread's box (`images/t3/fern-terminals.patch`, `images/fern-shell`; built 2026-11-13).** T3 starts a terminal's shell itself: on Fern that was a shell in T3's service, outside every box, as the user that owns every worktree and with the server's logins in its environment, for anyone paired with the server. Now a terminal on Fern runs `fern-shell` and nothing else, with no other shell tried after it: in a thread's worktree it is `fern exec -it --here -- bash`, a shell in the box with the services, where Claude's Bash runs (the thread's files, tools and database, and no login; only `TERM` and `COLORTERM` are passed on). A worktree whose thread has not had its first checkpoint yet is made an environment first, as `fern-opencode` does. Anywhere else (a *Local* thread, a project's main folder) the pane says that the folder has no box, and no shell starts. An open terminal is an open command, which keeps its box awake (§6.3), so the shell ends itself after 20 minutes at a prompt with nothing typed (`TMOUT`, the idle rule's human clock; `FERN_TERMINAL_IDLE` on T3's service changes it), and a command that runs keeps it. T3's project scripts (its own setup and settle scripts) are typed into a terminal before the shell in the box is ready for them, and T3 can't see the commands they start (it asks `ps` on the server), so they are left out, with a line in T3's log; what a repository needs set up goes in `.fern/setup` (§8). For a morning the patch turned terminals off instead.

Not done: making the main folder an environment. One environment for many threads breaks per-thread revert, gives the box the main repository's `.git` to write (and so code that T3 runs, §13), and puts whatever the folder holds into snapshots. Next, already planned: create the environment when T3 creates the worktree, not at its first checkpoint, which is where goldens start (§7.1).

### 15.1 How the hook reaches a user
Nobody applies a patch by hand. T3's server has to run next to the pool anyway (it runs git and the agent on the worktree paths), so Fern runs it as a systemd service from an image Fern builds with its patches (`images/t3/`) and unpacks into the pool (§14). The user installs Fern, opens the T3 app on their Mac and pairs it with the printed link (Settings → Connections). From then on checkpoints and reverts also cover dependencies and the database.

The cost moves to Fern: the image follows T3's releases, and the hook can break when T3 changes. Three ways to carry it:

| Option | What it is | Verdict |
|---|---|---|
| **A. Patched image** | ~100 lines: a layer that wraps `CheckpointStore` (`apps/server/src/checkpointing/CheckpointStore.ts`), swapped in at one line in `server.ts` | The spike and the MVP; only Fern's image carries it |
| **B. Upstream hook** | A general T3 setting (~50 lines): a command T3 runs when it takes, restores or deletes a checkpoint, with the cwd and the ref | The goal: stock T3, Fern only sets the setting. Needs T3's maintainers |
| **C. No T3 change** | git's `reference-transaction` hook sees saves and deletes (`git update-ref` on `refs/t3/…/checkpoints`) | Fallback only: a restore (`git restore --source <commit>`) moves no ref and fires no hook, so it would need a `git` shim in the image, which breaks when T3's git calls change |

A first, then propose B with what the spike learned ([history/roadmap-weeks.md](history/roadmap-weeks.md) week 8). C only if B is refused.

---

## 16. Observability

**A failure the daemon knows of reaches the place a person looks (built, 2026-11-13).** A thread on the VPS ran for a day beside faults that were each one line in a log: a golden build that never happened, a disk with nothing free. So:
- **In the thread:** at the start of a box with Claude, the daemon passes what is wrong for that thread (`FERN_NOTICES`, `internal/actor/notices.go`), and fern-init puts it at the top of Claude's hint with the word to tell the person, once: the pool or the disk under it takes no more snapshots, so a revert has no new points; the golden build of the checkout this thread has failed, with the end of what the setup script printed. A failed build of other files (an older lockfile) is not that thread's, so the key is compared, and only then worked out: a wake with nothing wrong reads one file name that isn't there.
- **On the server:** `fern doctor` lists the same and more (`GET /problems`, `internal/health`): a full pool, backups that failed (with when the last one worked) or that have not run for three of their intervals, each builder whose last build failed. Each has what to do. The last backup's result is a file in the pool (`backup.json`): until then doctor said "set up" for backups that failed.
- **Not built:** a failure of T3's own (a turn it marks failed, a refused *Local* thread) reaching the thread; anything that tells a person who isn't in a thread, such as a mail.

- **Metrics (Prometheus)** at `/metrics` on `127.0.0.1:9090` (`--metrics-addr`):
  - histograms `fern_wake_seconds`, `fern_stop_seconds`, `fern_restore_seconds`, `fern_snapshot_seconds` and `fern_golden_build_seconds` (by `result`: `ok` or `error`); `fern_queue_seconds` (how long a change waited for its actor, by `change`); `fern_wake_step_seconds` (by `step`: `net`, `start`, `ready`)
  - counters `fern_idle_sleeps_total`, `fern_prewakes_total` (by `result`, §6.4), `fern_golden_total` (new worktrees by golden `hit`, `miss` or `none`), `fern_box_failures_total` (wakes that failed, and boxes that stopped answering under a command); Go's runtime and process metrics
  - gauges, read when the metrics are: `fern_problems` (what `fern doctor` would list), `fern_environments`, `fern_boxes_up` (it runs `runsc list`), `fern_pool_bytes`, `fern_pool_free_bytes`, `fern_pool_least_free_bytes` (below it snapshots are refused) and `fern_disk_free_bytes` (the disk under a pool that is a file)
  - Go's profiles at `/debug/pprof/` on the same address
  - nothing on the server reads them yet: no Prometheus runs there. Planned: one, with an alert on `fern_problems`.
- **Traces:** with `--trace-endpoint` (OTLP/HTTP), the daemon sends a span per API request, per change (with the wait for its actor as `queue`), per store call, and per step of a box's wake (`net`, `start`, `ready`) and stop. `lab/profile.sh` turns them on with Jaeger on the node, puts them beside whole-node flamegraphs, and turns them off again: they cost a little, so the perf gate refuses to run with them.
- **Logs:** `fern logs <env>` (built), as root on the node: the daemon's lines that name the environment or its agent box (`journalctl -u fern-daemon`, a day back unless `--since` says otherwise), then what each of its boxes that runs has printed (`boxes/<box>/log` in the pool: its services' output and fern-init's). It reads the journal and the files itself, so it works with the daemon down. A box's own log goes when the box stops; not built: keeping the last one.
- **Turn timing:** `FERN_TIMING=1` on T3 records provider launch, the first decoded Claude event, Claude's cumulative API counter and each checkpoint span. `fern-claude` passes the flag to boxexec, which logs the process start and first stdout without arguments, environment values or stream contents. `lab/t3-run.sh` adds a `timing` object to each turn.
- **The lab measures everything else:** `lab/perf.sh` gates 288 metrics against a baseline, `lab/ab.sh` compares two versions on the same machine, `lab/profile.sh` shows where an operation's time and CPU go.
- **CLI:** `fern ls`, `fern snapshots <env>`, `fern why <env>` (what holds its boxes awake), `fern logs <env>`, `fern df` (what uses the disk) and `fern gc` (§9), `fern doctor` (the node, part by part, btrfs's room for its records among them, and what the daemon knows has gone wrong); planned: `fern inspect`, `fern top`.

## 17. Configuration

The daemon's flags (`deploy/systemd/fern-daemon.service` reads them from `/etc/fern/daemon.env` as `FERN_DAEMON_ARGS`):

| Flag | Default | Meaning |
|---|---|---|
| `--idle-agent` | 5m | sleep a box this long after its last command ended (0: never) |
| `--idle-human` | 20m | … and this long after its last command started (0: never) |
| `--idle-prewake` | 2m | sleep a box woken ahead this long after its wake, unless a command comes (0: no pre-wakes) |
| `--setup-timeout` | 1h | stop a repository's setup script that runs longer than this in a golden build (§7.5) |
| `--agent-boxes` | true | run Claude in an agent box per environment (§10) |
| `--preview-addr` | `:30780` | serve previews on this address; `""` turns them off (§5) |
| `--preview-host` | `localhost` | previews are `http://<port>-<env>.<host>/` and `http://<env>.<host>/` (§5) |
| `--box-dns` | 1.1.1.1,8.8.8.8 | the resolvers boxes use |
| `--box-cpus` | 2 | the CPUs a services' box may use, from its next wake on (§10) |
| `--box-memory` | 2048 | … and its memory, in MiB |
| `--box-pids` | 4096 | … and its processes and threads |
| `--box-max-cpus`, `--box-max-memory`, `--box-max-pids` | the three above | the most a repository's `services.yaml` may ask for its box. The agent can write that file, so by default it can only ask for less |
| `--t3` | `http://127.0.0.1:30773` | T3's server on this node, whose own MCP server Claude in an agent box reaches through the daemon; `""`: not |
| `--github-app` | `/etc/fern/github-app` | the GitHub App T3's git and `gh` get tokens from: its id and key (missing: none, §13) |
| `--github-api` | GitHub's | GitHub's API; another for tests |
| `--backup-every` | 1h | back the threads up this often, if `/etc/fern/backup.env` says where (§14) |
| `--metrics-addr` | 127.0.0.1:9090 | metrics and Go's profiles |
| `--trace-endpoint` | none | send traces by OTLP/HTTP |

Fixed in the code for now: the pool (`/var/lib/fern`), retention (50 turn snapshots, pre-restore copies for a day, §9) and an agent box's limits (§10). Planned: a `fern.yaml` with classes, fork caps and retention.

## 18. Code layout

```
cmd/fern/            small CLI
cmd/fern-daemon/     node daemon: wiring only
cmd/fern-init/       the main process inside boxes
internal/api/        socket protocol and client
internal/api/server/ socket API server
internal/tools/      the agent box's MCP tools, and what points an agent at them
internal/actor/      per-environment actors, idle rule, the box policy, golden builds
internal/store/      btrfs pool, snapshots, retention, the environments' records, Reconcile
internal/runsc/      boxes as gVisor sandboxes: bundle, network, firewall (+ a fake for unit tests)
internal/boxexec/    the exec protocol: fern-init's server, the client, the daemon's relay; touches
internal/background/ a box's background commands: fern-init's side and the daemon's client
internal/mcprelay/   the relay in an agent box between Claude and the daemon's MCP server
internal/watch/      the files an agent box writes, as the host sees them (fanotify)
internal/preview/    the front door for previews
internal/hibernate/  stops an idle Claude or OpenCode server and starts it again at the next message
internal/image/      unpacks docker save streams into box roots
internal/disk/       what uses the node's disk, and removing what nothing needs
internal/commit/     git commits in a thread's worktree, made on the node for a box that can't
internal/health/     what is wrong on the node now: failed builds, failing backups, a full pool
internal/backup/     hourly backups to an S3 bucket, encrypted on the node, and the restore
internal/github/     tokens from the person's GitHub App, for T3's git and gh
internal/setup/      setup contract, golden keys
internal/span/       trace spans
lab/                 the lab: setup, integration checks, perf gate, A/B, profiler
deploy/systemd/      the daemon's and T3's units
```

---

## 19. Milestones

The order of work is [roadmap.md](roadmap.md). Measured results, phase by phase, are in [lab/RESULTS.md](../lab/RESULTS.md).

## 20. Open questions

- **Wake time:** a warm pool of boxes could take a box's start (about 155 ms) out of a turn; a runsc subcontainer pool measured 395 ms against a cold 494 ms (`lab/spike/runsc/RESULTS.md`).
- **Quotas:** btrfs qgroups vs cgroup-level limits, given qgroups' performance cost.
- **Revert edge cases:** T3's "revert files only" vs "revert files too"; threads in the project directory rather than a worktree (§15).
- **Previews:** whether portless can name an already-forwarded port, or needs an adapter.
- **Is the hook enough** to cover every T3 flow that should snapshot or restore?
