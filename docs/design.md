# Fern: environments for AI coding agents (design doc, 2026-10-16)

> **Status:** design draft for Fern v2. Nothing here is built yet. Fern v1 (a control plane that runs coding-agent jobs in disposable containers and seals a verified Git artifact) is preserved at tag [`v1-final`](https://github.com/nebler/fern/tree/v1-final).
> This document covers the problem, the landscape, the architecture, the T3 Code integration, Kubernetes, setup, agent tooling, risks, and the designs we rejected.

---

## Contents
1. One-line summary
2. Why we are building this
3. Hard constraints
4. The problem, from first principles
5. What already exists (landscape)
6. What is left for us (the gap)
7. Design principles
8. The model: three nouns ("git for the whole environment")
9. Architecture: one job per part
10. Lifecycles, step by step
11. Setup: how environments get built (bash → Alchemy → Code Mode)
12. Storage and snapshot formats
13. Boxes: why they can be stopped at any time
14. The front door and wake layer
15. T3 Code integration
16. Kubernetes: mapping, scaling, setup
17. Agent superpowers (speed, not safety)
18. Secrets, logins and subscription rules
19. Decision record: actors, Durable Objects, Temporal, operators
20. Relationship to today's Fern repo
21. Roadmap
22. Naming
23. Risks, open questions, unverified claims
24. Appendix A: rejected designs and why
25. Appendix B: glossary
26. Appendix C: sources and files

---

## 1. One-line summary

**Every AI coding thread gets its own environment that behaves like git for everything — code, dependencies, the running database — starts instantly from a golden snapshot, sleeps when idle, wakes on the next message, and rewinds or forks together with the conversation. It runs on one Linux machine or on Kubernetes, uses your own Claude/Codex subscriptions, and plugs into T3 Code.**

Short version: *"Amp's Orbs, but open source and self-hosted, for T3 — with the database rewinding along with the code."*

---

## 2. Why we are building this

- **Agent threads are getting cheap, but their environments are not.** Every new thread repeats setup, parallel threads collide on ports and databases, and rewinding a turn only rewinds the code.
- **The interesting work is below the agent:** isolated environments, copy-on-write storage, wake-on-demand compute, crash-safe operations, and Kubernetes. These are well-understood primitives, combined for a new workload.
- **We want to use it ourselves,** every day, with T3 Code and parallel agents.
- **It doesn't need to win a market.** The bar is: finished, deep, measured, and honest about its limits.

---

## 3. Hard constraints

- **Subscriptions only.** Agents are the real `claude` / `codex` CLIs with your own login. No separately billed API keys. API access only through credits bundled with a plan, for own use only.
- **Secrets:**
  - the unmodified CLI with your own login
  - credentials are **never baked into snapshots**
  - no proxy that sits between you and your credentials (see §18)
- **Single user per login.** Every person on a team uses their own subscription. Never share one.
- **Remote-first.** Fern and the T3 server run on a **remote Linux machine** (a cloud VM, a home server, or a cluster). The Mac or phone is only a client (the T3 app). A local VM on the Mac is for development of Fern itself, not the intended workflow. See §14.1 for the connection layer.
  - Theo reportedly said on stream that most T3 users are on Linux. *Unverified.*

---

## 4. The problem, from first principles

### 4.1 What an agent thread needs
An agent works in **turns**: you send a message, then it runs commands, reads output and edits files until it stops. Before it can start, three things must exist:
1. **A folder:** code, installed packages, database data.
2. **A computer** to run the agent, the dev server and the database.
3. **A history** to go back to.

### 4.2 A project is more than files
| Part | Tracked by git? |
|---|---|
| Code | yes |
| Dependencies (`node_modules`, build caches) | no |
| Database data | no |
| Running programs (dev server, DB process) | no |

T3 already saves a **git checkpoint after every turn**. "Edit from here → Revert files too" restores it. T3's docs say this rewind "does not undo external actions". The database stays in the future, and code and data disagree (e.g. `SELECT email` after the agent dropped the `email` column).

### 4.3 The real everyday pain is slowness and drift, not disasters
- **Setup is slow, and every new thread repeats it:** clone, install, seed.
- **Parallel threads collide** on `:3000` and on one shared database.
- **Data drifts:** migrations, seeds and test runs leave the data out of sync with the code.
- **An agent can't cheaply "try three things and compare".**
- **Disasters still happen, but in production:**
  - PocketOS, April 2026: a Cursor agent on Opus 4.6 deleted a production DB and its backups in 9 s.
  - March 2026: Claude Code plus Terraform wiped 2.5 years of data.
  - Those are many model versions ago and about production. **Safety is a side effect, not the pitch.**

### 4.4 Demand signals
- Codex removed undo: issue #9203 (534 reactions) and #11626 (229), asking to rewind code plus chat.
- Claude's `/rewind` misses edits made through Bash (#87575, 44 reactions).
- T3's git-ref checkpoints time out on monorepos (#3646).

---

## 5. What already exists (landscape)

*Vendor claims, as of October 2026; we measured none of them.*

| Product | Its unit | Lifecycle | Fork / snapshot | Self-host | Notes |
|---|---|---|---|---|---|
| **Amp Orbs** (June 2026) | one sandbox per thread (bought from E2B) | sleeps after 5 min agent idle + 20 min human idle; wakes on a message, preview hit, webhook or schedule | golden "project snapshot" from `.agents/setup`, reused ≤72 h | no (runners = your machine, unmanaged) | closest product vision; supports Claude Pro/Max since 2026-10 |
| **Fly Sprites** | a named "computer" with a URL | warm → cold on idle; wake on request | disk checkpoints, ~300 ms (pages disagree) | no | best "feel" |
| **Vercel Sandbox** | a named sandbox containing sessions | `getOrCreate(name)`; any call wakes it; persistent by default | snapshots outlive the sandbox; rollback by moving a pointer | no | **cleanest model** (one noun, ephemeral opt-in) |
| **Google Agent Substrate** | "actor" = a DB record with a stable name, compute borrowed | router holds the request, restores the snapshot onto a warm pod | one snapshot per actor; revert to the latest only | yes (Apache-2.0) | gVisor checkpoint/restore; docs call parts "aspirational" |
| **kubernetes-sigs/agent-sandbox** | `Sandbox` (stateful pod) | `operatingMode: Running/Suspended` (pod deleted, volumes kept) | snapshots GKE-only | yes | `SandboxTemplate`, `WarmPool`, `Claim`, `sandboxd` (gRPC exec + PTY, REST files) |
| **E2B** | Firecracker VM | pause keeps memory forever | fork up to 100 copies; named snapshots | Embed (one node) | |
| **smolvm** | local microVM | | live CoW branches; checkpoints with memory, `--at ~N` | yes, macOS + Linux | doesn't capture host-mounted folders; no per-turn trigger |
| **forkd** | Firecracker VM | | 100 forks in ~101 ms; MCP server | yes, Linux/KVM | Postgres fork-per-test recipe |
| **Modal** | short-lived sandbox (5 min default, 24 h max) | idle = deleted | filesystem/directory snapshots; memory snapshots gated | no | |
| **Daytona** | sandbox | stop after 15 min, archive | memory fork on VMs | open-source repo frozen June 2026 | |
| **Cloudflare Sandbox SDK** | a Durable Object per ID; container per session | DO hibernates; container disk wiped on sleep | | no | "same ID → same object; same ID ≠ same container" |
| **OpenSandbox** | sandbox (Firecracker "Fast Sandbox") | pause/resume | content-addressed checkpoints; `SandboxSnapshot` to OCI | yes | fork PR open |
| **Coder / Ona** | workspace / environment per task | idle stop | | Coder yes | Coder Tasks deprecated in favour of Coder Agents |
| **GitHub Codespaces prebuilds** | container snapshot per branch/config/region | | the "golden snapshot" precedent | no | `onCreate`/`updateContent` baked, `postCreate` after |
| **devenv / Flox (Nix)** | reproducible toolchain | | | yes | rebuilds tools, not data; complements snapshots |
| **Grab Palana** | namespace per agent | idle reaper | none (EFS `/data`) | internal | Vault + proxy secrets, Envoy/OPA egress, Cilium |
| **KEP-5823** | Kubernetes pod checkpoint/restore | | | — | alpha targeted v1.37, same node, volumes out of scope |
| **boat.dev** | hosted sandbox ($0.036/h) | | disk fork from named snapshots | no | |

**Patterns the best ones share:**
1. **The name lasts; the machine is borrowed.**
2. **Always reachable; wakes on the first touch.** Requests are held, and duplicate wake-ups are merged.
3. **Idle means specific signals,** not CPU.
4. **Template → golden snapshot → warm pool → claim.**
5. **Snapshots never change,** outlive their source, and are moved by pointer.

---

## 6. What is left for us (the gap)

Nobody does all of these together:
1. **Snapshots keyed to agent turns and tied to the conversation:** rewind chat, code *and* data together.
2. **A consistent snapshot of code + dependencies + the running database.** Most products snapshot files only.
3. **Real subscription CLIs** in an open, self-hosted setup.
4. **One environment, same name, on your laptop VM or your server/cluster.**
5. **"What did this turn change outside git?"**: dependencies, DB, untracked files.
6. **Snapshot support for agent-sandbox outside GKE.** The proposal (#694, PR #762 "Rewind & Fork") went stale, and there is no `SnapshotProvider` API.
7. **Per-thread external resources:** a Neon branch or preview Worker created with the environment and destroyed with it (via Alchemy stages, §11).

**Honest framing:** the *primitives* exist (VM fork, checkpoints). Ours is the layer above them: **when** to snapshot (agent turns), **what** (everything, consistently), **how long** (history, golden), and **integration** (T3, subscriptions, local + Kubernetes).

---

## 7. Design principles

**From Amp's product decisions:**
1. **The thread is the unit.** Users never say "environment". One per thread, created automatically, named by the thread ID.
2. **Move the viewer, never the disk.** Open the thread anywhere; the environment stays where it is.
3. **The golden snapshot is the most important hidden feature.**
4. **The repo owns setup:** `setup` (baked into golden), `resume` (every wake), `services` (dev server and DB). Let the agent write them.
5. **Sleep, don't delete.** Wake on any touch. "Ephemeral" is opt-in.
6. **Isolation over permission; quick reversibility over careful review; parallelism over queues.**
7. **Few knobs:** one size class, automatic time limits, network denied by default.
8. **Rewind and fork are plumbing and agent tools,** not the headline. Amp removed both undo and fork as *user* features.
9. **Build for today's models,** and keep a "what we won't do" list.

**From sound primitives (actors, Durable Objects, Orleans, git):**

10. **Three lifetimes, kept apart:** snapshot (never changes), environment (lives as long as the thread), session/box (only while working).
11. **One writer per environment:** one actor, operations one at a time, no locks.
12. **Snapshot = a value that never changes; environment = a named pointer to one.**
13. **"Idle" = no incoming work** (turn, exec, preview connection), never CPU.
14. **The box holds no state.** Everything that matters is on the disk.
15. **Multi-step operations go through a durable log,** and every step is safe to repeat.
16. **A fork gets a fresh identity:** new IDs, secrets, ports, sessions.
17. **Clear split of truth:** Kubernetes custom resources hold the *desired* state (what environments exist, their class, their node); the node's daemon owns the disk and reports *observed* state. Per-turn snapshots never become Kubernetes objects.

---

## 8. The model: three nouns ("git for the whole environment")

### 8.1 Who sees what
| Who | Sees | Nothing else |
|---|---|---|
| **You, in T3** | the thread, its **preview URL**, a **sleep badge**, plus **Revert** and **Fork** (both already exist in T3) | no "environment", "snapshot" or "box" |
| **The repo** | one environment definition: `.agents/*` or `.fern/env.ts`, plus an optional size class | |
| **The agent** | 4 tools: `fork`, `snapshot`, `restore`, `exec` (plus Code Mode) | |
| **Fern internally** | everything else | |

### 8.2 The git analogy
| git | Fern | Meaning |
|---|---|---|
| commit | **snapshot** | frozen, never changes |
| tagged commit | **golden** | a snapshot built from the setup definition |
| branch + working tree | **environment** | a named pointer + a writable `live/` folder |
| `git reset --hard <c>` | **rewind** | point back, restore `live/` |
| `git switch -c new <c>` | **fork** | new environment from a snapshot |
| running the checkout | **session** (the box) | temporary compute on top of `live/` |

**Properties, not nouns:**
- golden = a tagged snapshot
- persistent / ephemeral = the environment's retention
- light / heavy / bench / untrusted / gpu = its size class

### 8.3 Relationships
- **Thread ↔ environment: one-to-one,** named by the thread ID. Technically it's bound to the thread's worktree, and T3 already refuses file restore when two threads share a directory.
- **Snapshots form a family tree:** golden → turn snapshots → forks. Retention: keep the last N, plus anything tagged, plus "expire when unused".
- **Sessions are never addressed directly.** You talk to the environment, and it decides whether a session must start.

### 8.4 The three lifetimes
```
time  ───────────────────────────────────────────────────────────▶
snapshot      ◆      ◆        ◆                 ◆        (never change)
environment   ████████████████████████████████████████   (as long as the thread)
session       ████████        ██████                ███  (only while working)
```

---

## 9. Architecture: one job per part

```
 T3 app (laptop / phone)      view only
        │
 T3 server                    threads · chat · git checkpoints
        │ unix socket / hook
 ┌──────▼──────────────────── fern daemon (Go) ───────────────────────┐
 │ front door      wakes environments, holds requests, routes previews │
 │ managers        one actor per environment, one op at a time         │
 │ operation log   SQLite; multi-step ops are durable and safe to rerun│
 │ box driver      start / stop sessions (agent-sandbox Sandbox)       │
 └──────┬──────────────────────────────────────┬──────────────────────┘
        │                                      │ mount
 disk (btrfs): golden/ · envs/<id>/live · envs/<id>/snaps     sessions (boxes)
        ▲
 repo: .agents/setup · .agents/resume · services  (or .fern/env.ts)
```

| Part | Its one job | Go package (sketch) |
|---|---|---|
| **Store** | btrfs subvolumes: golden, live, snaps; retention | `store` |
| **Manager (actor)** | owns one environment's name and current pointer; does one operation at a time; supervises its session | `actor` (goroutine + channel per environment) |
| **Operation log** | durable steps for rewind, fork, golden build; replays after a crash | `oplog` (SQLite) |
| **Front door / wake layer** | the entrances (socket, HTTP previews, webhooks); merges duplicate wake-ups and holds requests | `wake` |
| **Box driver** | starts and stops sessions by flipping an agent-sandbox `Sandbox` between `Running` and `Suspended` (Kubernetes only, §16.1, §19) | `driver` |
| **Thin operator** | reconciles this node's `Environment` resources (desired state) and writes status | `operator` |
| **Setup runner** | builds golden from the repo's definition | `setup` |
| **T3 hook** | maps T3 checkpoint capture/restore/delete to snapshot/restore/prune | small TS patch in T3 |

**Precedents:** Cloudflare Sandbox SDK ("same ID → same Durable Object, but not the same container"), Orleans virtual actors, Fly Sprites, Agent Substrate's actors.

**Placement rule:** *the disk decides where an environment lives.* T3 sits on the same machine and reads files directly. That honours T3's rule that the environment "owns providers, execution, files, and durable state".

---

## 10. Lifecycles, step by step

### 10.1 A new thread
1. In T3, "New thread" (optionally "New sandbox").
2. The T3 hook or `SandboxService` asks Fern for environment `t3-8f2c`. It's idempotent: get-or-create.
3. The manager looks up **golden**, keyed by repo + setup-definition hash + lockfiles + base image + size class:
   - **Hit:** `live/` = a writable copy of golden (**milliseconds**, copy-on-write).
   - **Miss:** the setup runner builds golden first (minutes, once), then copies it.
4. Nothing runs yet. **A session starts lazily on the first command.**

### 10.2 A turn
1. T3 sends `exec("claude …")` through the front door.
2. If the environment is asleep, the front door holds the request and the manager asks the box driver for a session (~1–2 s, *unmeasured*). `.agents/resume` runs (logins, ≤10 s), and services start.
3. The CLI runs inside the session with a PTY. Output streams back to T3.
4. The turn ends. T3 saves git checkpoint `7f3a` (exists today), and its hook sends `snapshot(env, "7f3a")`.
5. The manager runs `btrfs subvolume snapshot -r live snaps/7f3a` (ms) and replies ok. **T3 marks the turn saved only after that ok.**

### 10.3 Rewind ("Revert files too")
1. T3 restores code from git (exists today).
2. The hook calls `restore(env, "7f3a")`. The manager logs these steps to the op log:
   1. stop the session (graceful: services get a shutdown signal; Postgres shuts down cleanly)
   2. `live.new` = writable copy of `snaps/7f3a`
   3. swap `live.new` ↔ `live`
   4. delete the old live
3. Reply ok. The next command spawns a session on the restored disk.
4. **No new commands are accepted while a rewind is running** (the actor's mailbox makes this automatic).

### 10.4 Fork
1. T3's fork (it already has a `t3_thread_fork` MCP tool), or the agent's `env.fork`.
2. New environment `t3-91aa`, whose `live/` = a writable copy of the chosen snapshot.
3. **Post-fork identity hook:** new environment ID, fresh secrets, ports and session tokens. This is the lesson from Lambda SnapStart and from our spike, where copying T3's state duplicated identity and resurrected revoked logins.
4. **Caveat found in T3's code:** a T3 fork **shares the source thread's worktree** (`ThreadForkService.ts:84-110`), so today it would share the environment too. Either add a small "fork into a new worktree" patch to T3 (~50–150 lines), or state the rule that only agent-made forks (`env_fork`) get their own environment.

### 10.5 Sleep and wake
- **Idle** = no turn running, no terminal open, no preview connection, for N minutes (Amp uses 5 min agent idle + 20 min human idle). CPU doesn't count.
- **Sleep:** stop the session; `live/` stays; memory is freed.
- **Wake triggers:** a message, opening the thread (optional, like Amp's "Saver Mode"), a preview hit, a webhook, a schedule, a reply from another agent.
- **On wake:** start the session, run `.agents/resume`, start services. Processes restart; their data persists.

### 10.6 Ephemeral mode (opt-in)
- When idle, `live/` is **thrown away**; sealed snapshots are kept.
- The next wake starts fresh from golden.
- This is "serverless, only if you choose". Today's Fern "seal and tear down" flow is exactly this mode (§20).

### 10.7 Crash recovery
- The daemon restarts, reads the op log, and finishes unfinished steps (every step is safe to repeat).
- It reconciles sessions that are actually running with what the store expects.
- T3 then resumes the agent:
  - Claude: `resume: nativeThreadId`, auto "Continue where you left off" up to 3 times; it falls back to a fresh session with a summary handoff.
  - Use Claude Code ≥ 2.1.265, which marks a tool call interrupted by a crash as interrupted.
  - Before resuming after a mid-turn crash, roll back to the last finished turn.

---

## 11. Setup: how environments get built

**This is the hardest UX problem.** Amp calls its version "the most important hidden feature".

### 11.1 How Amp does it
- **`.agents/setup`:**
  - safe to rerun; killed after 20 minutes
  - **no long-running processes** (anything left running is killed before the snapshot)
  - **no personal credentials** (a project-scoped OIDC token is allowed, and removed before the snapshot)
- **The project snapshot (= golden):**
  - keyed by orb size + repo source + shared config
  - reused for up to 72 hours
  - setup is skipped when it matches
- **`.agents/resume`:** every wake, ≤10 s, for logins and tunnels. Machine identity is never baked in.
- **`.amp/services.yaml`** plus `amp orb services ensure|start`. Apps must respect `PORT` and `PUBLIC_URL`. A running service doesn't keep the orb awake.
- **A rich base image:** Node, Bun, pnpm, Python, uv, gh, agent-browser, ffmpeg, tmux. Setup scripts stay small.
- **Repo advice: "Don't make them guess."**
  - one safe-to-rerun dev-server script
  - ports written to a JSON file
  - logs in one place
  - dev-only endpoints like `/__dev/log-me-in` and `/__dev/preflight`

### 11.2 Fern's layered approach
1. **Amp-compatible bash, the floor:** read `.agents/setup`, `.agents/resume` and a services file. Any repo already set up for Amp works unchanged.
2. **Fallbacks:**
   - `devcontainer.json`, using Codespaces' prebuild split: `onCreate` / `updateContent` go into golden, `postCreate` runs after
   - T3's `t3.json` worktree scripts (`runOnWorktreeCreate`)
3. **Nothing there? The agent writes it.** `fern init` runs a first thread whose only job is to produce setup, resume and services, then prove them by building golden.
4. **Smarter golden keys:** hash(lockfiles + setup definition + base image + size class), not the commit. Most commits don't change dependencies, so golden is rebuilt rarely.
5. **Incremental rebuilds:** build the new golden on top of the old one, so it's mostly a warm `npm ci`.

### 11.3 The Alchemy layer (optional, typed)
Alchemy ("Infrastructure-as-Effects", the Effect-based version is vendored in T3's repo) has:
- `Command` with `Memo` (*caching semantics unverified*)
- Docker, Kubernetes, Neon and other providers
- **stages** (the same program deployed per stage; used for PR previews)
- `plan` / `diff` / `drift`

What it would give us:
- **Memoized setup steps:** change only the seed and only the seed reruns.
- **Stage = thread:** per-thread external resources (a Neon branch, a preview Worker, a test Stripe account) created with the environment and destroyed with it.
- **Plans the agent can read:** "this will add Redis and reseed the DB".
- **Typed TypeScript, Effect-based like T3,** which agents write well.

Sketch (a hypothetical API, not Alchemy's real one):
```ts
// .fern/env.ts
export default Env("myrepo", function* () {
  yield* Command.Memo("deps", { run: "pnpm install --frozen-lockfile", inputs: ["pnpm-lock.yaml"] }) // baked into golden
  const db = yield* Postgres("db", { version: 16, seed: "pnpm db:seed", inputs: ["prisma/**"] })     // baked into golden
  yield* Neon.Branch("cloud-db", { parent: "main" })              // optional, per thread (stage = thread)
  yield* Service("web", { run: "pnpm dev", port: 3000, env: { DATABASE_URL: db.url } })             // restarted on wake
})
```

Caveats:
- `alchemy` with `effect@rc` is pre-release.
- Per-thread cloud resources cost money or quota, so they're opt-in.
- Alchemy's state and secrets must stay out of snapshots.

### 11.4 Code Mode
- The agent edits the environment definition, and drives Fern's API from small scripts instead of many tool calls (§17.2).
- Code Mode is a fit for **operations**: fork, bench, bisect. Setup itself is just running a program.

---

## 12. Storage and snapshot formats

### 12.1 On one Linux machine (btrfs)
- Every environment's `live/` is a **subvolume**, which behaves like a folder.
- **Snapshot:** `btrfs subvolume snapshot -r live snaps/<id>`. It isn't a file: it's a second, read-only view of the same blocks. It's a metadata operation, taking milliseconds whatever the size.
- **Fork:** the same command without `-r` (writable).
- **Restore:** a writable copy of the snapshot, then swap.
- **Copy-on-write:** a snapshot costs ~0 bytes until the live copy changes; then only the changed blocks are new.
- **Why btrfs over ZFS for this model:** ZFS clones depend on their origin snapshot (`zfs promote` is needed to break that), while btrfs snapshots are independent subvolumes.
  - ZFS is still supported: `zfs snapshot` / `clone` / `rollback`.
  - XFS/ext4 per-file reflinks are slow for many files.
- **No btrfs disk? Make one:** a sparse image file (`/var/lib/fern/pool.img`), formatted as btrfs and loop-mounted. Zero repartitioning.

### 12.2 Directory layout
```
/var/lib/fern/
  golden/
    myrepo@<setup-hash>/        read-only: the starting point
  envs/
    t3-8f2c/
      live/                     writable; T3's worktree points here
        src/  node_modules/  .fern/pg/  (code, packages, DB data)
        .home/.claude  .home/.codex      (agent session files, so resume works)
        .home/.claude/.credentials.json  ← NOT here: login files are bind-mounted from outside live/
                                           (or kept in a nested subvolume, which btrfs snapshots skip)
      snaps/
        7f3a/  91bc/  …         read-only, one per turn (named by T3 checkpoint ID)
  oplog.sqlite                  durable operations
  index.sqlite                  snapshot ID ↔ checkpoint, parent, env, size, tags
```

### 12.3 Moving snapshots between machines
- `btrfs send` / `zfs send` produce a stream (a file), including incremental streams against a parent.
- Uses: backups, moving an environment to another machine, sharing a bug.
- On Kubernetes, `VolumeSnapshot` (CSI) is used only for durable or cross-machine copies. Its format depends on the driver (a ZFS snapshot on the node, an EBS snapshot on AWS).

### 12.4 Consistency
- **A btrfs snapshot is atomic** and crash-consistent. Postgres sees it like a power cut and recovers from its WAL.
  - Earlier spike: 41/41 exact Postgres rewinds, but on Docker Desktop on a Mac. **Re-measure on Linux.**
- For extra safety: `CHECKPOINT` + `fsfreeze` before the snapshot, run from the node and not from inside gVisor.
- **SQLite apps:** copy the WAL file together with the main file, or snapshot after a stop. (Our T3 spike showed that copying the main DB without its WAL silently gives an empty DB.)

### 12.5 Memory snapshots: not by default
- gVisor `runsc checkpoint` / CRIU: images are about as large as the RAM in use; TCP connections drop; restore creates a new container.
- Fly says resume is "not guaranteed", and Fly, Modal and E2B all show it's fragile and expensive.
- **The promise is "stop": disk kept, processes restarted.** Memory checkpoints are a stretch goal (e.g. resuming in the middle of a turn).

---

## 13. Boxes: why they can be stopped at any time

**How stopping works:**
- Locally: stop the container.
- On Kubernetes: delete the pod (agent-sandbox: `operatingMode: Suspended`).
- The disk survives because its lifetime isn't tied to the session.

**What makes it safe: the box holds no state** (12-factor "stateless processes"):
- Everything writable is mounted from `live/`: code, `node_modules`, DB data, **and `~/.claude` / `~/.codex`** (so `claude --resume` works). The image is read-only.
- **Polite stop:** a shutdown signal first (Postgres shuts down cleanly), then kill.
- **Never stop in the middle of a turn.** The idle rule guarantees this.
- **On wake,** the services file restarts the dev server and DB. Even after a hard kill, Postgres recovers.
- **Processes started by hand don't come back.** Only declared services do. Document this.

**Isolation inside the session:**
- runc for light boxes, gVisor (`runsc`) for untrusted ones, Kata/Firecracker micro-VMs when a separate kernel is needed
- CPU and memory limits (cgroups / ResourceQuota)
- network denied by default, with an allowlist (package registry, GitHub)

---

## 14. The front door and wake layer

### 14.1 The connection layer: Mac as client, Linux machine remote
The intended workflow is **remote**: your Mac (or phone) runs only the T3 app; the T3 server, Fern, the disks and the boxes run on a remote Linux machine. Most of the connection layer already exists in T3 (verified in `docs/user/remote-access.md`):

| What needs to reach the remote machine | How | Exists today? |
|---|---|---|
| **The T3 app → T3 server** | pairing over HTTPS, `t3 serve --tailscale-serve`, **desktop-managed SSH** (the app starts or reuses a server on `user@host` and opens the port forward), or T3 Connect | yes |
| **Previews (dev servers in boxes)** | T3's **browser tabs belong to the environment** and are streamed to every device; `localhost` on the host is reachable. So Fern only has to forward each box's ports to the host's loopback (as T3's sandbox PR does) | T3 side yes; Fern side to build |
| **Sharable preview URLs** (outside T3) | the front door's hostname routing behind Tailscale or a tunnel with a wildcard hostname | to build, optional |
| **The `fern` CLI from the Mac** | `ssh host fern …` in v1; later the daemon's API over Tailscale or HTTPS with a token | to build |
| **Agent logins** | done once on the remote machine (`claude setup-token`, Codex device login), stored outside environments | yes (CLI features) |

**Practical setup:** one cheap Linux VM (or a cluster) on Tailscale; `fern up` there; in the T3 desktop app, *Add environment → SSH* (or a pairing link). Nothing runs on the Mac except the app.

### 14.2 Where the front door runs

**Where it runs:** inside the Fern daemon, one per machine. It has three entrances:
1. **A local socket** (`/run/fern.sock`): T3, the CLI and hooks.
2. **An HTTP listener for previews:** `fix-login.myrepo.<host>` routed by hostname to the environment.
3. **Webhooks:** `/hooks/<env>`.

**What it does:**
- an environment is asleep → hold the request
- wake it **once** (duplicates are merged)
- forward everything when it's ready
- never return an error just because it was asleep

**On Kubernetes:** the environment lives on one machine, but a preview request can land on any machine.
- **Simplest:** every daemon forwards to the right one. Which machine an environment lives on is a field on its `Environment` resource.
- **Later:** a small "edge" Deployment behind the wildcard Ingress that does the lookup.

**Names instead of ports: portless** (Vercel Labs):
- It swaps `localhost:3000` for `myapp.localhost`, hands each app a free port via `PORT`, and gives each git worktree its own subdomain.
- Use it as is when there are no sandboxes.
- With sandboxes, *verify* whether portless can point a name at an already-forwarded port, or needs an adapter.
- Fern does not build its own URL product.

**Gotchas we found:**
- **The T3 app treats a 404 as permanent and a 503 as temporary.** A waking proxy must answer 503, never 404.
- **T3 client connection attempts time out after ~15 s,** and it retries forever with backoff up to 5 min. A cold start under ~15 s succeeds on the first try.

---

## 15. T3 Code integration

### 15.1 How T3 is built (verified in code, commit `ec1313c8f8`)
- **Environment** = one `t3 serve` process. It "owns providers, execution, files, and durable state" (`docs/internals/remote.md`). Projects and threads belong to exactly one environment.
- **The client catalog is local to each app:** IndexedDB in browsers, an encrypted `connection-catalog.json` on desktop.
  - Every *enabled* saved environment is connected at startup and retried forever (`registry.ts:381`, `supervisor.ts:40-50`).
  - There is a per-environment on/off switch in Settings → Connections.
- **Pairing:**
  - `t3 auth pairing create` / `session issue` work from the CLI
  - a curl `POST /oauth/token` redeems a code (verified in the spike)
  - there's no pairing deep link: `t3code://` only covers `new` and `threads/<env>/<thread>`
- **Placement:** a new thread goes to its project's environment. The load balancer is **off by default**, and when on it only picks *connected* environments that *already have the project*. Its score is weight × CPUs × free CPU × free memory, read from the whole node, not cgroups.
- **Checkpoints:**
  - `VcsCheckpointOps` in `apps/server/src/vcs/VcsDriver.ts` (`captureCheckpoint`, `restoreCheckpoint`, `deleteCheckpointRefs`)
  - called from `CheckpointService.ts:287,398,524`
  - "Edit from here → Revert files too" (`docs/user/composer.md:145-157`)
- **Crash recovery:** `RestartContinuation.ts` ("Continue where you left off", up to 3 times); `ProviderTurnStartService.ts` falls back to a summary handoff.
- **MCP tools:** `t3_project_clone`, `t3_project_create`, `t3_thread_launch` (`mcp/toolkits/project/tools.ts:68-104`), and `t3_thread_fork`.
- **State layout:**
  - `userdata/environment-id` is a plain file
  - **one SQLite file `statev2.sqlite`** holds both logins and all threads
  - `secrets/server-signing-key.bin`
  - the Claude history lives in `$HOME/.claude` (or `CLAUDE_CONFIG_DIR`), keyed by cwd

### 15.2 The seams Fern plugs into
| Seam | Exists? | What Fern does there |
|---|---|---|
| **Checkpoint capture / restore / delete** (`VcsCheckpointOps`) | **yes, on `main`** | a ~100-line hook: snapshot / restore / prune under the same ID → "Revert files too" also reverts the DB; works for every provider |
| **Thread fork** | yes | fork the environment under it |
| **`SandboxService`** (PR #15350 by t3dotgg: "threads can run in Docker sandboxes") | open; conflicts; Macroscope flagged 5 blocking issues | a Fern backend: "New sandbox" creates a Fern environment and session |
| **MCP** | yes | the agent's `env_*` tools |

**PR #15350 in detail:**
- 60 files, +3,586 lines.
- The CLI runs *inside* the container (Claude via `spawnClaudeCodeProcess`, Codex via a wrapped `app-server`, terminals via `docker exec -it`).
- **Same paths:** the worktree, git dir, `~/.claude`, `~/.codex` and `~/.gitconfig` are bind-mounted at host paths, so T3 keeps reading files and history.
- A Node stdio relay forwards ports.
- It fails closed, and stop/restart is lazy.
- The `SandboxService` interface (register / start / execTarget / stop / remove / list / forwardedPorts) is implemented with the `docker` CLI.

### 15.3 The four ways to map threads (D chosen; C re-checked and rejected)
| Model | Verdict |
|---|---|
| A. One T3 per user, threads as worktrees | works today; low Kubernetes value |
| B. One T3 per repo | the fallback if #15350 stalls |
| C. **One full `t3 serve` per thread pod** | **rejected after sizing the patch** (below) |
| D. **One T3 per person (a "cell"); each thread's session is a pod** behind `SandboxService` | **chosen** |
| D+. D, plus moving a whole cell to a standby node (fed by incremental `btrfs send`) on drain or node loss | **adopt later** |

**Why C is not "a little patch"** (sized against T3's code, 2026-10-17):

| C patch | Lines |
|---|---|
| Fleet sync (the app learns about new pods) | 250–300 |
| Lazy connect (registering an environment connects immediately today: `registry.ts:544-578`) | 150–250 |
| New thread creates a pod (`ChatView.tsx:3192-3235`, plus mobile) | 350–550 |
| Fork into a new pod (`Orchestrator.ts:3536` + Fern cleanup) | 100–200 |
| Hide per-thread rows in Settings → Connections | 30–60 |
| Checkpoint hook (same as D) | ~100 |
| **Total** | **~1,050–1,550 + tests** |

- **It's client code** (web, desktop, mobile). You would ship your own T3 apps forever; a patched phone app isn't realistic. D needs ~550–950 lines, all on the server, and works with the official apps.
- **C still can't put a thread on any node.** T3 reads the worktree, the project's `.git` and `~/.claude` as local files (`ProviderTurnStartService.ts:446-467`, `workflowFileRead.ts:11`, `CheckpointService.ts`), so each thread's disk pins its pod to a node anyway.
- **Other costs:** ~310 MiB of T3 per awake thread, 4–7 s wakes, a Codex login copy per pod (its token refresh breaks), and lazy connect makes background threads show stale status.
- **What helps C for free:** repository grouping (`projectGrouping.ts`, default) shows N per-thread environments as one project.

**The mapping for D (the chosen model):**

| T3 / Fern element | Kubernetes or Fern |
|---|---|
| T3 app (laptop, phone) | outside the cluster, the official unpatched app, via an Ingress/Service or SSH |
| Person | Namespace + ResourceQuota + NetworkPolicy |
| T3 server | **one pod per person** (1-replica StatefulSet + Service) = the **cell**; the scheduler picks the node, the disk keeps it there |
| Fern daemon | DaemonSet + CSI node plugin (+ the thin operator, §19) |
| Disk | the node's btrfs pool, one subvolume per environment, no PVCs |
| Project | a git checkout in the cell's pool + a row in T3's database; no Kubernetes object |
| Thread | a row in T3's database = one Fern `Environment` |
| Worktree | the environment's `live/` subvolume, at the path T3 uses |
| Turn / checkpoint | a T3 git checkpoint ref + a Fern read-only snapshot with the same ID; no Kubernetes object |
| Session | an agent-sandbox `Sandbox` owned by the `Environment`; its pod mounts the disk through the CSI driver and is deleted on sleep (`operatingMode: Suspended`) |
| Size class | `EnvironmentClass` CRD → RuntimeClass + resources + nodeSelector |

**One T3 server or many?** One per person. The chain is person → cell (one T3 server) → projects → threads; each thread has one worktree and one environment; an environment has many snapshots and at most one session pod at a time. A person who outgrows a node gets a second cell; the app shows two machines, but the sidebar merges them into one project.

### 15.4 Spike results (run in Docker and k3s)
- `t3 serve` in a container: **158 MB image**, **2.9 s cold start**, **~310 MiB idle**. No node-pty build was needed with the npm binary.
- Scripted pairing plus curl redeem: works.
- Restart: 2.9–4.3 s; a second server on the same volume refuses to start (exit code 78).
- In k3s, the pod is back in **3.7–6.9 s** with its identity intact.
- Live SQLite copies: 45/45 passed integrity checks, **but only with the WAL file.**
- Rewinding T3's DB brings back revoked sessions (confirmed). **So Fern never snapshots T3's own database,** only environment disks.

### 15.5 Patches and upstream plan
1. **Checkpoint hook** (~100 lines): ask the maintainers first; ship a patched build meanwhile.
2. **A Fern `SandboxService` backend:** after #15350 lands. Ask Theo early whether a pluggable backend is welcome.
3. **Later, optional:** a "fleet address" client feature (~250–300 lines in `packages/client-runtime/src/connection/fleet.ts`). Not needed in the chosen design.
4. **Contribution etiquette:** check `CONTRIBUTING.md`, talk to maintainers before large PRs.

---

## 16. Kubernetes: mapping, scaling, setup

### 16.1 Kubernetes only, even for one person
**Decision (2026-10-17): Fern runs on Kubernetes only.** One person on one machine runs a single-node k3s. There is one code path; a small fake driver exists only for fast unit tests.

**Why:**
- Even one person needs what Kubernetes gives for free: gVisor (RuntimeClass), deny-by-default networking, CPU/memory limits, restarts and supervision, Secrets, an ingress, metrics. A separate local driver would have to rebuild all of it.
- Two backends would spend half the effort on a mode the target use case doesn't need.
- The riskiest piece (a gVisor pod using a btrfs subvolume through our CSI driver) only exists on Kubernetes, so it gets tested from week 0.

**Costs, accepted:**
- k3s idles at ~1.2–1.45 GB RAM ([k3s profiling](https://docs.k3s.io/reference/resource-profiling)), so the first node needs 8 GB.
- A wake is ~1–2.5 s slower than a plain container (the spike measured 3.7–6.9 s pod restarts vs 2.9–4.3 s for Docker), still inside the 15 s budget.

### 16.2 The rule: keep the fast path off the Kubernetes API
- A `VolumeSnapshot` goes through controllers and the API server, typically **≥1 s** (*to be measured*). Restoring means a new PVC and a reschedule.
- **Per-turn snapshots therefore happen in the node-local daemon (btrfs, ms).** Kubernetes objects are only for slow things: scheduling, quotas, sleep, durable or cross-machine copies.

### 16.3 Mapping T3 and Fern onto Kubernetes
| Concept | Kubernetes |
|---|---|
| T3 server | a **pod** (1-replica StatefulSet), pinned to a machine, sharing that machine's `/var/lib/fern` |
| Fern daemon | a **DaemonSet** pod on every machine, owning the btrfs disk; also a **CSI node plugin** (below) |
| Project | a folder on that machine's disk, with no object |
| **Thread** | a Fern **environment**, named by the thread ID; optionally a read-only `Environment` resource for `kubectl` |
| Thread's disk | a **btrfs subvolume, not a PVC** (a PVC per thread is slow and heavy) |
| Thread's session | an **agent-sandbox `Sandbox` owned by the thread's `Environment`**. That node's Fern daemon (a kubelet-style node agent, §19) creates it and sets `operatingMode`; the agent-sandbox controller creates the pod, labelled `t3/thread=…` |
| Mounting the thread's disk | **a CSI inline (ephemeral) volume:** `csi: { driver: fern, volumeAttributes: { env: t3-8f2c } }`. Kubernetes calls Fern to bind-mount that subvolume. There's no raw hostPath in user pods. |
| Turn snapshot | a read-only subvolume (no object); `VolumeSnapshot` only for backups |
| Sleep / wake | `operatingMode: Suspended` / `Running` (the agent-sandbox controller deletes / creates the pod) |
| Size class (`EnvironmentClass`) | RuntimeClass (runc / gVisor / Kata), resources, node selector, warm pool |
| Per-person limits | a namespace per person + ResourceQuota + NetworkPolicy |

### 16.4 Types of machines and boxes (between "cluster" and "pod")
**The layers:**
- **node pools** (labels, taints)
- **namespaces**
- **RuntimeClass**
- resources and quotas
- templates and warm pools (agent-sandbox `SandboxTemplate` / `SandboxWarmPool`; not used in v1, §19)

**Different OSes:**
- other Linux distros: trivial
- other kernels: Kata/Firecracker
- Windows: Windows nodes
- full VMs: KubeVirt
- macOS: effectively no (Apple's license needs Mac hardware)

**Example classes:**
```yaml
kind: EnvironmentClass
metadata: { name: light }
spec: { runtime: gvisor, cpu: 1, memory: 2Gi, nodePool: small, warmPool: 10 }
---
kind: EnvironmentClass
metadata: { name: bench }
spec: { runtime: runc, cpu: 4, memory: 8Gi, nodePool: dedicated, pinnedCpus: true }
```
The classes: `light`, `heavy` (big monorepos, NVMe), `bench` (pinned CPUs, its own machine), `untrusted` (gVisor or micro-VM, no network), `gpu`.

### 16.5 Scaling: what scales and what doesn't
| Thing | How |
|---|---|
| Sessions (thread pods) | many per machine; they sleep when idle; **this is the main thing that scales** |
| Machines | `fern join` / the cluster autoscaler |
| T3 server | **doesn't scale out** (a single process; a second copy refuses to start); it's light, so scale it up if needed |
| One person beyond one machine | one T3 per machine for that person; the app shows "node-1", "node-2"; **T3's load balancer picks** |
| A team | one **cell** per person (their T3 + sessions + disk); Kubernetes spreads the cells |
| Agent benchmark forks | **anywhere**: they aren't T3 threads; the disk is sent with `btrfs send` (seconds for GBs) |

### 16.6 One-command setup
```
curl -fsSL get.fern.dev | sh      # or: fern up
```
1. A cluster exists? Use it (`helm install fern`). Otherwise install **k3s**, a single binary that starts in under a minute.
2. No btrfs/ZFS? Create a **loop-mounted btrfs image file**.
3. One Helm chart:
   - Fern DaemonSet + CSI driver
   - the agent-sandbox controller (a chart dependency; Fern uses only its `Sandbox` object)
   - gVisor RuntimeClass (optional)
   - default NetworkPolicy and quotas
4. Start the T3 server pod (patched build with the hook) on the machine holding the disk.
5. Print **one pairing link**; paste it into the T3 app once.
6. Add machines: `fern join <token>` (k3s supports joining natively).

**Prior art:** k3sup, OrbStack's "it just works", Coolify-style installers.

---

## 17. Agent superpowers (speed, not safety)

### 17.1 What agents and people can do
| Power | Value |
|---|---|
| **Golden → instant environments** | every new thread is ready in milliseconds instead of minutes; probably the biggest daily win |
| **Rewind with data** | undo a migration, undo test pollution, switch between threads with different schemas, bisect with data |
| **Fork with data** | best-of-N: three agents, three approaches, identical code + data; keep the winner |
| **Parallel benchmarks** | fork 4 identical environments, compare A vs B; spread them over machines so they don't skew each other |
| **Fair agent evals** | same task, same snapshot, different models or prompts, on *your* repo |
| **Automatic safety branches** | the agent forks before a risky step, keeps the better result |
| **Local preview environments** | every PR or branch forked from golden |
| **Parallel tests from a warm state** | split suites across forks instead of setting up N times |
| **Move a thread between machines** | `send` the disk from a laptop VM to a server |
| **Share a bug** | export the exact environment where it fails |
| **Finer undo** | snapshots per tool call, not just per turn |
| **Reversibility → autonomy** | if everything can be rewound, approval prompts for "drop table?" or "run migration?" can be auto-approved (only if T3 agrees to tie permissions to it) |

### 17.2 Code Mode API (sketch)
```ts
// one script, not 20 tool calls
const base  = await env.snapshot("pre-bench")
const forks = await env.fork(base, { count: 4 })
const runs  = await Promise.all(forks.map(f => f.exec("npm run bench")))
await env.delete(forks)
return best(runs)
```
Agent tools (MCP): `env_snapshot`, `env_restore`, `env_fork`, `env_exec`, `env_delete`. Keep it to about four or five; agents get worse with many tools.

### 17.3 Guardrails
- **A cap on forks** (e.g. 8): `fork({count: 200})` is clipped.
- **TTL:** forks made by agents delete themselves after 1 hour.
- **Limits:** e.g. 2 CPU / 4 GB per environment.
- **Network:** denied by default, with an allowlist.
- **Forks that run another agent spend your subscription quota.** Forks that only run code don't.
- **Rewind can't undo the outside world:** pushed commits, API calls, emails. Pair it with network rules.

---

## 18. Secrets, logins and subscription rules

**Claude:**
- `claude setup-token` gives a 1-year token, passed as `CLAUDE_CODE_OAUTH_TOKEN` from a Secret.
- Anthropic docs say it works with the CLI and "surfaces that wrap it, including the Agent SDK", and staff confirmed subscriptions plus the Agent SDK are fine.

**Codex:**
- `codex login --device-auth` (enable device code in ChatGPT settings), or copy `auth.json` into `CODEX_HOME`.
- **Never share one `auth.json` across concurrent sessions or machines;** its refresh token rotates (issue #10332).
- One login per session, or one refresher that hands out fresh copies.

**Where credentials live:**
- **injected by `.agents/resume`** on every wake, from a Secret or the host
- never in golden or snapshots
- forks get fresh identity

**Usage limits:**
- Claude Pro/Max: a 5-hour window plus a weekly cap shared across sessions; no documented concurrency cap.
- Codex: per-5-hour and weekly estimates.
- **Subscription limits, not Kubernetes, cap a solo user's fleet.**

**Terms:**
- Anthropic: limits "assume ordinary, individual usage"; signing in to the unmodified CLI with your own subscription is allowed; **a proxy that holds or "intermediates" credentials is not**; the consumer terms ban "automated or non-human means" unless permitted.
- OpenAI: API keys are "the right way to authenticate automation"; ChatGPT auth is for "trusted private automation".
- **Verdict: single user, interactive, your own login = a defensible grey area. Never share or resell.**
- **This replaces the earlier "placeholder token + egress proxy" (Palana) idea** for model logins. It's still fine for *other* secrets (GitHub, cloud), e.g. Fern's existing GitHub App broker (§20) and Amp-style per-thread OIDC tokens.

---

## 19. Decision record: actors, Durable Objects, Temporal, operators

*Revised 2026-10-17 after a dedicated review (notes in the private research folder).*

| Option | Decision | Why |
|---|---|---|
| **Erlang/OTP actors** | **adopt the pattern** | one goroutine + mailbox per environment; supervision; restart limits; let it crash |
| **Actor rules, made precise** | **adopt** | only **mutations** (snapshot, restore, fork, delete) queue one at a time; running execs and golden builds are tracked separately, and a restore cancels them using the generation number. Timers (idle sleep, fork TTL, retention) are stored in SQLite so they survive restarts, like Durable Objects' alarms. |
| **Virtual actors (Orleans, Dapr, Akka sharding)** | **adopt the pattern** | always addressable, activated on demand, deactivated when idle; the disk decides placement; idle = no incoming work |
| **Cloudflare Durable Objects** | **don't use; copy the ideas** | "don't ack a turn until the snapshot is durable", "no mutations during a rewind", alarms. Later: a hosted front door for webhooks while you're offline |
| **Temporal** | **don't use** | its history limit is solvable ("continue as new"); the real reasons are a second system to run, a central service while our state lives on one node, and added latency |
| **DBOS Go / Restate** | **don't use; same idea in miniature** | DBOS Go now runs on SQLite, but Fern v1's intent log already exists, is tested, and adds generation fencing |
| **Kubernetes: thin, kubelet-style operator** | **adopted (replaces "no operator")** | `Environment` and `EnvironmentClass` CRDs hold desired state (repo, class, retention, node); each node's daemon reconciles only its own node's environments and writes status; each `Environment` owns one agent-sandbox `Sandbox`, which owns the session pod (garbage collection). Snapshot / restore / exec / wake go straight to the daemon, like `kubectl exec` goes to the kubelet; **per-turn snapshots never become Kubernetes objects.** Built with controller-runtime. Removes the homemade "which node owns what" table. |
| **Exec plane** | **in-box agent (`fern-init`)** | `kubectl exec` now uses WebSockets (1.31), so "fragile" is dated; the real reasons are no reattach and all traffic through the API server. `fern-init` must keep agent terminals alive across daemon restarts and allow reattaching; speak agent-sandbox's `sandboxd` protocol where possible |
| **Security of mounts and pods** | **new decisions** | the CSI driver only mounts a pod's **own** environment (it checks the requesting pod's identity); an admission policy limits the daemon to creating `Sandbox` objects pinned to its own node with allowed runtimes; user namespaces on for boxes (GA in 1.36); **login files never in `live/`** (bind-mounted, or in a nested subvolume that snapshots skip) |
| **Docker inside the box** | **adopt as a class** | many repos start Postgres with `docker compose`: a `docker` class using Kata or gVisor (Sysbox where available) |
| **Warm pools vs inline volumes** | **no warm pools in v1** | a pod's volumes are fixed at creation, so a pre-started pod can't receive an environment's disk later |
| **Quotas** | **btrfs simple quotas** (Linux 6.7+) | avoids the cost of classic qgroups |
| **Snapshots as named, frozen copies** | **adopted** | snapshot = value, environment = pointer. (Not "content-addressed": btrfs snapshots are named; the golden key is a hash of inputs.) |
| **Memory snapshots by default** | **rejected** | fragile, large, connections drop |
| **Plan 9 namespaces / capabilities** | **adopt** | grant access by mounting; per-environment tokens |
| **FUSE on the hot path** | **rejected** | slow for `node_modules` |
| **agent-sandbox** | **build on its `Sandbox` object (revised 2026-10-18)** | It is the SIG Apps standard (v1.0.x, `v1beta1`). Its `podTemplate` is a plain PodSpec, so our CSI volume, gVisor RuntimeClass and node pinning fit directly; `operatingMode: Running/Suspended` is our wake/sleep. It replaces only the pod-creation code (~5–10% of Fern). Everything it lacks is Fern: snapshots outside GKE, rewind, fork with data, automatic sleep and wake (roadmap: *planned*), golden builds, T3. **Not used:** `SandboxWarmPool` / `SandboxClaim` (volumes are fixed at pod creation), its PVCs (our disk is btrfs), `shutdownTime` (the actor decides). `fern-init` speaks the `sandboxd` protocol so its SDKs work. Costs: one more controller, a beta API, possibly a slower wake (measure in week 0). Contribution plan in §21.4 |
| **Others** | | crun: adopt (faster starts). Kueue: later, for benchmark forks. Knative/KEDA: reject (prior art for the wake layer). Virtual Kubelet, Cluster API, NATS/Redis/Raft: reject. Kubernetes Lease: only for leader election. OpenTelemetry: adopt (one trace per wake). TopoLVM / OpenEBS / Ceph / overlayfs: not needed with node-local btrfs |

## 20. Relationship to today's Fern repo

Fern v1 (this repository at tag `v1-final`, 393 commits, Go) is "a self-hosted Go control plane for engineers who want to run coding-agent jobs in disposable containers, steer them live, and retain a verified Git artifact after the workspace is removed." It already has strong building blocks that map onto this design:

| Existing Fern | Role in the new design |
|---|---|
| **"Intent before effect"**: SQLite records admission and lifecycle transitions before external mutations; recovery reconciles observed effects with durable intent | **= the operation log** (§9, §10.7) |
| **Exact writer, fencing, sealing** (stop and fence the writer, verify the Git snapshot and bundle, then tear down) | **= ephemeral mode** (§10.6), and "one writer per environment" |
| **Disposable resource identity** | session identity; fresh identity on fork |
| **Host-side GitHub App broker** (short-lived, repo-scoped tokens, refreshed in private storage) | the non-model secrets path (§18) |
| **`fern attach` through an expiring capability** | the capability model for viewers and agents |
| Docker driver, XFS project quotas | the local box driver; quotas move to btrfs qgroups or cgroups |
| OpenCode runtime, "credential-free model provider" | **changes:** real `claude` / `codex` CLIs on subscriptions; T3 as the front end |
| "No honest one-command quickstart yet" (XFS project storage, `CAP_SYS_ADMIN`, a manual GitHub App) | **fixed by** `fern up` (§16.6) and the loop-mounted btrfs pool |

**What changes in the product framing:** from "run a job → seal an artifact → tear down" to **"every thread has a lasting environment that sleeps, wakes, rewinds and forks; sealing is one way to end it."**

**Why evolve rather than start over:** the hard parts (durable intent, recovery, fencing, credential delivery) already exist and are tested.

---

## 21. Roadmap (about 8 weeks, Kubernetes first)

*Revised 2026-10-17: "engine first, Kubernetes later" built throwaway local code and left the riskiest piece for last.*

**The week-by-week plan now lives in [roadmap.md](roadmap.md)** (2026-10-18): an MVP (`v0.1`) in about 3 weeks, where "Revert files too" also restores the database, then v1 by week 8. The table below is the v1 summary; the demo, cut list, machines and contribution plan follow.

| Week | Goal |
|---|---|
| **0 (2 days, local)** | local lab (k3s in a privileged Docker container, loop-mounted btrfs), the agent-sandbox controller, a tiny CSI plugin. **Prove a `Sandbox` can use a btrfs subvolume through our CSI inline volume;** try gVisor; time `Suspended` → `Running` |
| **1 (local)** | the engine from the CLI: store, daemon API, `Sandbox` driver, CSI, `fern-init`: snapshot → break → restore → data is back |
| **2 (local)** | actor, op log, generation numbers, idle sleep and wake on exec, Postgres consistency, small crash test |
| **3 (local)** | T3 on top (checkpoint hook, `fern-claude` wrapper, two T3 settings, a real Claude turn): the MVP works locally |
| **4 (remote from here)** | go remote: VPS, gVisor, real timings, Mac/phone over SSH → **MVP `v0.1`**; then golden snapshots, agent fork, previews through the front door; **first agent-sandbox PR:** suspend/resume benchmark on k3s + gVisor (§21.4) |
| **5** | `Environment` / `EnvironmentClass` CRDs (thin operator); security (CSI authorization, admission policy, user namespaces); Helm / `fern up` |
| **6** | crash harness at scale, terminal reattach, second node, Grafana |
| **7** | measurements, failure-analysis doc, record the demo, agent-sandbox bug-fix PRs |
| **8** | buffer, blog post, upstream proposals (T3 checkpoint hook; agent-sandbox auto-suspend #968 and snapshot provider #694, with Fern as working code) → **`v1.0`** |

### 21.1 The demo (about 2.5 minutes, 2-node k3s)
| Time | Action | What they see |
|---|---|---|
| 0:00 | `kubectl get ds,csidriver,runtimeclass,environments,sandboxes` | the Fern DaemonSet on both nodes, the CSI driver, gVisor, the CRDs. "Sessions are standard agent-sandbox `Sandbox` objects; their disks are btrfs subvolumes, not PVCs." |
| 0:15 | new thread | the environment comes from golden in milliseconds; the first command starts a gVisor pod with an inline CSI volume |
| 0:40 | a turn: a migration drops `users.email` | the snapshot takes milliseconds; "this path makes zero Kubernetes API calls" |
| 1:00 | "Revert files too" | code and Postgres both come back |
| 1:20 | fork ×3 | three pods, each with its own Postgres on :5432, ~0 extra bytes |
| 1:45 | **live failure:** force-delete the Fern daemon pod in the middle of a rewind | the DaemonSet restarts it, the op log replays, the environment ends consistent; "CI runs thousands of random crash points" |
| 2:10 | idle sleep, then 20 requests at once | the pod is gone; requests get 503 then 200; exactly one pod is created |
| 2:30 | numbers slide + "why snapshots aren't CRDs" | |

- **Must be real:** k3s, DaemonSet, CSI, btrfs, gVisor, Postgres in the box, crash recovery, wake-on-request, metrics.
- **Can be stubbed:** the live agent turn (a script; show one real `claude` turn in the recording), golden pre-built (show cold vs golden as a number), T3 as a short clip.
- **Numbers:** snapshot time at 0 / 500 MB / 5 GB (flat), restore, fork time and bytes, wake time by stage, golden vs cold, RAM while asleep (0), Postgres recoveries N/N, crash tests N/N, Kubernetes API calls per turn (0).
- **Don't kill a node live:** environments live on one node; put that in the written failure analysis (and D+ cell moves later).

### 21.2 Cut from v1
A local (non-Kubernetes) driver (the local lab runs k3s in Docker instead), ZFS, Alchemy, Code Mode, portless, the `SandboxService` backend, Firecracker/Kata (cheap VMs lack nested virtualization), devcontainer and `t3.json` fallbacks, webhooks, Codex (Claude first), most MCP tools, warm pools (and agent-sandbox claims), multi-person cells.

### 21.3 Machines
An **8 GB first node** (e.g. Netcup VPS 1000 G12 ~€10/month, or Hetzner CX/CAX if available; check consoles), plus a **4 GB second node** from week 6. About €15–20/month in total. Your Mac only runs the T3 app (§14.1).

### 21.4 Contributing to agent-sandbox
Fern uses agent-sandbox, so stress-testing it is part of the work, and what we find goes upstream. Its roadmap lists exactly Fern's pieces as *planned*: Auto Suspend/Resume, Scale to Zero, Extended Benchmarking, Publish Benchmarking Methodology, Measure & Improve TFFI. Snapshots exist only on GKE.

| Order | Contribution | Related |
|---|---|---|
| 1 | Benchmark suspend/resume on k3s + gVisor (not GKE); publish the method and results as a docs PR | roadmap: benchmarking, TFFI |
| 2 | Fix the bugs the stress test finds, each with a reproduction | e.g. #837 (status stale at zero), #952 (when a resumed pod is ready) |
| 3 | Suspend/resume metrics | #1155 |
| 4 | Join the design discussions with working code: Fern's wake-on-touch and btrfs + CSI snapshots | #968, #849 (auto-suspend, idle policy); #694, #949 (snapshot provider); #1237 (guest agent ≈ `fern-init`) |

**Rules:** read `CONTRIBUTING.md`; sign the CNCF CLA before the first PR; small PRs first; large features need a KEP and maintainer agreement, which can take months. Only merged PRs count.

## 22. Naming

**"Fern" as the name: good, with one caveat.**
- **Pros:**
  - It continues an existing project with real history (§20).
  - Short and memorable.
  - The metaphor fits: fern fronds **branch** and **unfurl**, like forks and snapshots growing from a golden root.
- **Caveat: Fern (buildwithfern.com),** a known YC company making API docs and SDK generators, is in the same developer-tools space. Searching "fern cli" will be noisy, and the npm, Homebrew and domain names may be taken.
- **Mitigations:**
  - a distinct CLI and repo name (`fernd`, `fern-env`, or a GitHub org)
  - or "Fern" as the project name with a clear tagline
- **Avoid "orbd/orb":** Amp's **Orbs** (June 2026) are the same concept, and OrbStack owns "orb".

---

## 23. Risks, open questions, unverified claims

**Big risks:**
1. **T3 accepting the checkpoint hook.** Fallback: a patched build.
2. **agent-sandbox API changes** (`v1beta1`). Pin a version; only the `driver` package touches it.
3. **PR #15350** may change, stall or never merge.
4. **Setup friction** for real repos: golden builds failing, services not respecting `PORT`.
5. **Subscription terms.** Stay single-user, interactive, own login.
6. **Scope creep:** Alchemy, memory snapshots, ACP and Code Mode beyond the basics are stretch goals.

**Open technical questions:**
- **Wake time:** session start + resume + services (target ≤15 s, so T3's first connection attempt succeeds).
- **Postgres consistency** on Linux btrfs under load (re-run the 41/41 test).
- **portless with forwarded sandbox ports.**
- **Rewind coordination:** is the hook point the right place for revert-with-data in all of T3's flows (edit from here, revert files only, thread fork)?
- **Kubernetes CSI inline volume + btrfs subvolume:** mount semantics under gVisor.
- **Node loss:** environments are tied to their machine. Recovery = restore from shipped snapshots. Write this up as a failure analysis.
- **btrfs quotas:** simple quotas (Linux 6.7+) vs classic qgroups.
- **CSI driver authorization:** confirm the driver can reliably check the requesting pod's identity; confirm the admission policy can read the node name from node-bound tokens.
- **Late mounting under gVisor** and whether `sandboxd` supports reattach.
- **agent-sandbox fit:** our CSI inline volume in a `Sandbox` `podTemplate` under gVisor; `Suspended` → `Running` time; whether a force-deleted pod is recreated; the pod → `Sandbox` → `Environment` ownership chain for CSI mount checks ("Strict Sandbox-to-Pod Mapping" is still in progress upstream).
- **Where Claude's transcripts live:** inside `live/` (rewound with the disk) or outside (T3 handles conversation rollback itself).

**Unverified or conflicting:**
- Sprites' checkpoint timing (300 ms, ~1 s, or 10–30 s across pages).
- Theo's "most users on Linux".
- Alchemy `Memo` semantics.
- Amp's linked Anthropic support article (not opened).
- E2B Embed availability (page versions conflict).
- All timing numbers so far come from Docker Desktop on a Mac.

---

## 24. Appendix A: rejected designs and why

| Design | Why rejected |
|---|---|
| **"orbd as a T3 backend" (v1 framing)** | weak: T3 and T3 Connect already cover remote machines |
| **One full `t3 serve` per thread pod + a fleet broker** | fights T3's client: the app never lets pods sleep, nothing outside can add machines, "New thread" can't make a pod, identity is duplicated on fork, revoked logins come back on rewind (§15.3) |
| **Splitting the agent CLI into another pod via `binaryPath`** | T3 owns files, history and checkpoints locally, so the files would be split |
| **A heavy operator with per-turn objects (`AgentSession`, `VolumeSnapshot` per turn)** | slow and noisy; replaced by a thin, kubelet-style operator where CRDs hold only desired state (§19) |
| **`kubectl exec` relay** | fragile data plane |
| **Per-thread PVCs through CSI snapshots for every turn** | seconds, not milliseconds |
| **Placeholder-token egress proxy for Claude/Codex logins** | the terms forbid intermediating credentials; OAuth tokens refresh themselves |
| **Durable Objects / Temporal as the core** | §19 |
| **Memory snapshots by default** | fragile and large |
| **Our own URL proxy** | portless exists |
| **"Safety" as the headline** | models are good now; speed and parallelism are the pitch |
| **Exposing "environment" and "snapshot" to users** | Amp's lesson: the thread is the only unit |
| **The name "orbd"** | Amp's Orbs, OrbStack |


---

## 25. Appendix B: glossary

| Term | Meaning |
|---|---|
| **Turn** | one agent loop, from your message to the agent stopping |
| **Environment** | a thread's named, lasting workspace: a pointer + `live/` |
| **Snapshot** | a frozen, read-only copy of `live/` at a moment |
| **Golden** | a snapshot built from the repo's setup definition; new environments start from it |
| **Session / box** | temporary compute (a pod) running on top of `live/` |
| **Sandbox** | agent-sandbox's object for one stateful pod; Fern's session, switched between `Running` and `Suspended` |
| **Front door / wake layer** | the daemon's entrances; wakes sleeping environments and holds requests |
| **Manager / actor** | the one goroutine that owns an environment and processes its messages in order |
| **Operation log** | SQLite record of multi-step operations, for crash recovery |
| **Cell** | one person's T3 server + sessions + disk on one machine |
| **EnvironmentClass** | a size/isolation profile (runtime, CPU, memory, node pool) |
| **CoW (copy-on-write)** | copies share blocks until one side changes |
| **CSI inline volume** | a standard Kubernetes way for a pod to ask a driver (Fern) to mount something |
| **T3 hook** | the small patch that calls Fern on checkpoint capture/restore/delete |

---

## 26. Appendix C: sources

**T3 Code source**, commit `ec1313c8f8`:
- `apps/server/src/vcs/VcsDriver.ts`, `orchestration-v2/CheckpointService.ts`, `RestartContinuation.ts`
- `packages/client-runtime/src/connection/{registry,supervisor}.ts`, `load-balancing.ts`
- `docs/internals/{remote,environment-auth,t3-connect}.md`, `docs/user/{composer,remote-access}.md`

**Key external links:**
- T3 PRs/issues: #15350 (sandboxes), #15904, #15975, #19615, #7545 (closed), #14115
- agent-sandbox: `api/v1beta1/sandbox_types.go` (`operatingMode`), `packages/sandboxd/USER_GUIDE.md`; snapshot proposal #694, PR #762
- Amp: ampcode.com/news (Orbs, Free Agent, Use Your Claude Plan, Neo, Stick a Fork in It), ampcode.com/notes/putting-an-agent-in-an-orb
- portless: vercel-labs/portless
- Anthropic: code.claude.com/docs/en/authentication, code.claude.com/docs/en/legal-and-compliance
- Incidents: oecd.ai/en/incidents/2026-04-27-6153 (PocketOS), oecd.ai/en/incidents/2026-03-07-79ec
