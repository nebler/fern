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
- **Linux at the core.** On a Mac, use a Linux VM (OrbStack, Lima) or a remote box.
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
| **KEP-5823** | Kubernetes pod checkpoint/restore | | | — | alpha targeted v1.38, same node, volumes out of scope |
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
17. **One source of truth:** the daemon's store, not Kubernetes custom resources plus a disk.

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
 │ box driver      start / stop sessions (container or pod)            │
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
| **Box driver** | start/stop sessions: local container, or a Kubernetes pod | `driver/local`, `driver/k8s` |
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
- **Simplest:** every daemon forwards to the right one. Which machine an environment lives on is a label, or a row in a small shared table.
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

### 15.3 The four ways to map threads (we chose D, then refined it)
| Model | Verdict |
|---|---|
| A. One T3 per user, threads as worktrees | works today; low Kubernetes value |
| B. One T3 per repo | the fallback if #15350 stalls |
| C. **One full `t3 serve` per thread** | **rejected:** fights T3 everywhere (see below) |
| D. One T3 per person per machine; **each thread's session is a pod/container** behind `SandboxService` | **chosen**, refined into the cell + Fern-daemon design |

**Why C fails** (found in the spike plus code research):
- the app connects to every machine forever, so pods never sleep
- nothing outside the app can add machines; the upstream attempt, PR #7545 `t3 env add`, was closed by its own author after review
- "New thread" can't create a pod
- a copied state duplicates identity, and rewinding resurrects revoked logins (verified)
- ~310 MiB per pod

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

### 16.1 Do we need Kubernetes?
**No for the core:** everything works on one Linux machine.

**Yes for:**
- teams (one cell per person)
- benchmark forks spread over machines (forks on one machine skew each other's numbers)

**Order:** the engine and daemon first (weeks 1–2, identical either way), then the Kubernetes packaging.

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
| Thread's session | a **pod created by the machine's Fern daemon itself** (no operator), labelled `t3/thread=…` |
| Mounting the thread's disk | **a CSI inline (ephemeral) volume:** `csi: { driver: fern, volumeAttributes: { env: t3-8f2c } }`. Kubernetes calls Fern to bind-mount that subvolume. There's no raw hostPath in user pods. |
| Turn snapshot | a read-only subvolume (no object); `VolumeSnapshot` only for backups |
| Sleep / wake | pod deleted / created |
| Size class (`EnvironmentClass`) | RuntimeClass (runc / gVisor / Kata), resources, node selector, warm pool |
| Per-person limits | a namespace per person + ResourceQuota + NetworkPolicy |

### 16.4 Types of machines and boxes (between "cluster" and "pod")
**The layers:**
- **node pools** (labels, taints)
- **namespaces**
- **RuntimeClass**
- resources and quotas
- templates and warm pools (agent-sandbox `SandboxTemplate` / `SandboxWarmPool`)

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
   - agent-sandbox resources (optional)
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

| Option | Decision | Why |
|---|---|---|
| **Erlang/OTP actors** | **adopt the pattern** | one goroutine + mailbox per environment; supervision; restart limits; let it crash; boxes are watched by Fern, not tied to T3 |
| **Virtual actors (Orleans, Dapr, Akka sharding)** | **adopt the pattern** | always addressable, activated on demand, deactivated when idle. Their hardest problem (where does an actor live?) disappears: the disk decides. Idle = no incoming work. |
| **Cloudflare Durable Objects** | **don't use; copy the ideas** | DO = actor + storage + alarms + hibernation; copy "don't ack a turn until the snapshot is durable" and "no commands during a rewind". Running on Cloudflare would put the coordinator far from the disks. **Later use:** a hosted front door that holds webhooks while you're offline (T3's relay already does this with a DO). |
| **Temporal** | **don't use** | needs a cluster plus a DB; our steps take milliseconds to seconds; history limits (~51,200 events) fight environments that live for months |
| **DBOS / Restate ideas** | **adopt in miniature** | the SQLite operation log: durable, safe-to-repeat steps, replay after a crash |
| **Kubernetes operator + CRDs as truth** | **rejected** | two sources of truth (resource vs disk); the daemon creates pods directly; resources only as read-only status |
| **`kubectl exec` as the data plane** | **rejected** | fragile (kubelet stream timeouts, API-server load); use `sandboxd` (gRPC + PTY) or the node daemon |
| **Memory snapshots by default** | **rejected** | fragile, large, connections drop |
| **Content-addressed snapshots** | **adopted** | snapshot = value, environment = pointer (git, OCI, Nix, Neon, Dolt, Modal, Sprites) |
| **Plan 9 namespaces / capabilities** | **adopt** | grant access by mounting, not by permission checks; per-environment tokens; avoid shared `~/.claude` mounts across environments |
| **FUSE on the hot path** | **rejected** | `node_modules` is slow over FUSE; btrfs already exposes snapshots as folders |

---

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

## 21. Roadmap (about 8 weeks)

1. **Weeks 1–2: the engine on one Linux machine**
   - store (btrfs, loop-mounted pool), actor per environment, op log (reuse Fern's intent log), local box driver
   - golden from `.agents/setup` / `resume` / services
   - **T3 checkpoint hook:** "Revert files too" also reverts the DB
   - **re-measure everything on Linux** (snapshot time, Postgres consistency, wake time)
2. **Weeks 3–4: fork, agent tools, previews**
   - fork with fresh identity
   - MCP `env_*` tools + Code Mode API
   - portless integration
   - sleep and wake with the front door
3. **Weeks 5–6: Kubernetes packaging**
   - DaemonSet + CSI inline volumes + pods created by the daemon
   - `EnvironmentClass`, quotas, NetworkPolicy, gVisor
   - `fern up` on k3s
   - (if #15350 has landed) the Fern `SandboxService` backend
4. **Weeks 7–8: proof and polish**
   - benchmark forks across machines
   - failure analysis (node loss, mid-turn crash)
   - design doc, demo video, blog post
5. **Alongside:**
   - propose the checkpoint hook to T3
   - one agent-sandbox PR (snapshot API #694 or fsfreeze #1237); sign the CNCF CLA early
6. **Stretch:**
   - Alchemy layer (`.fern/env.ts`, stages = threads)
   - memory checkpoints
   - ACP
   - moving environments between machines via `send`


---

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
2. **PR #15350** may change, stall or never merge.
3. **Setup friction** for real repos: golden builds failing, services not respecting `PORT`.
4. **Subscription terms.** Stay single-user, interactive, own login.
5. **Scope creep:** Alchemy, memory snapshots, ACP and Code Mode beyond the basics are stretch goals.

**Open technical questions:**
- **Wake time:** session start + resume + services (target ≤15 s, so T3's first connection attempt succeeds).
- **Postgres consistency** on Linux btrfs under load (re-run the 41/41 test).
- **portless with forwarded sandbox ports.**
- **Rewind coordination:** is the hook point the right place for revert-with-data in all of T3's flows (edit from here, revert files only, thread fork)?
- **Kubernetes CSI inline volume + btrfs subvolume:** mount semantics under gVisor.
- **Node loss:** environments are tied to their machine. Recovery = restore from shipped snapshots. Write this up as a failure analysis.
- **btrfs quotas** (qgroups have a performance cost) vs XFS project quotas (today's Fern).

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
| **A Kubernetes operator with an `AgentSession` CRD as truth** | two sources of truth; the daemon is enough |
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
| **Session / box** | temporary compute (container or pod) running on top of `live/` |
| **Front door / wake layer** | the daemon's entrances; wakes sleeping environments and holds requests |
| **Manager / actor** | the one goroutine that owns an environment and processes its messages in order |
| **Operation log** | SQLite record of multi-step operations, for crash recovery |
| **Cell** | one person's T3 server + sessions + disk on one machine |
| **EnvironmentClass** | a size/isolation profile (runtime, CPU, memory, node pool, warm pool) |
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
