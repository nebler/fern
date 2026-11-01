# Roadmap

**Status:** 2026-11-01. This is the source of truth for the order of work. The first plan, week by week and Kubernetes first, is [history/roadmap-weeks.md](history/roadmap-weeks.md); Fern left Kubernetes on 2026-10-29 ([briefs/fern-without-kubernetes.md](briefs/fern-without-kubernetes.md)).
- [architecture.md](architecture.md): what we build, with what is planned marked so
- [../lab/RESULTS.md](../lab/RESULTS.md): what was measured

## Built
- **The promise:** a snapshot per agent turn, and a revert in T3 brings back the files and the database (`v0.1`).
- **The engine:** environments on a btrfs pool, gVisor boxes the daemon runs itself, an actor per environment, crash-safe changes with `Reconcile`, sleep when idle and wake on use, pre-wake.
- **Agents:** Claude in an agent box of its own; its Bash runs in the environment's box, which never sees the login.
- **Golden snapshots:** a repository's setup (tools, dependencies, an initialised database) built once and shared by every thread.
- **Web development** (2026-10-31): dev servers hear Claude's edits, keep running, and are reachable as previews that wake the box; tools a repository pins (mise).
- **Install** (`v0.2.0`, 2026-11-01): one command from a Mac sets up a server from released images, Claude's login, a tunnel and the T3 pairing.

## Next, in order

| # | Build | Done when |
|---|---|---|
| 1 (built 2026-11-01) | **Your own repository from a git URL:** add a project by its URL; without a `services.yaml`, Fern guesses one from the repository (the `dev` script and its port, the Node version, the package manager, Postgres in `docker-compose.yml`) | a Vite repo and a Next.js repo with Postgres, neither with Fern files, start and serve a preview from their URLs |
| 2 | **GitHub access for private repositories and `git push`**, without a token in any workspace, golden or snapshot | a thread clones a private repo and pushes a branch; a scan finds no token in the pool |
| 3 | **`fern doctor`:** the services, the tunnel, Claude's login, the disk and gVisor, each with what to do when it fails | each failure the installer knows of is named by it |
| 4 | **Supervision:** a box that keeps crashing backs off, then its environment is `failed`, with the reason; background jobs and idle clocks survive a daemon restart | crash tests: a box that dies at once ends `failed` after N tries; a dev server started through Bash runs on after `systemctl restart fern-daemon` |
| 5 | **Backups:** snapshots shipped with `btrfs send` to object storage, and an environment restored from them on a new server | a server deleted and set up again gets its threads back, the database included |
| 6 | **A faster first page:** the default preview rendered in the background after a wake; gVisor with KVM measured | TanStack Start's first page from an asleep box well under 16.6 s (lab/RESULTS.md, 2026-10-31) |
| 7 | **Forks:** an environment copied from a snapshot, several at once, each with its own Postgres on :5432 (`env_fork`) | three forks of a thread run their tests at once |
| 8 | **Codex** next to Claude, with its own login | a Codex thread in T3 snapshots and reverts like a Claude one |

## Not now
Several nodes and moving environments between them, quotas and several people per server, micro-VM boxes, warm pools of boxes, memory snapshots (rejected: lab/spike/runsc), EKS.

## Rules for staying fast
- **Every step ends with something you can run and show.** If a step slips, cut scope, not the demo.
- **Measure before and after:** a change that could move a number runs `lab/perf.sh`, and `lab/ab.sh` when it is close.
- **Tests as you go:** unit tests with the fake driver on every change, `lab/check.sh` in the lab and on the VPS before a merge.
- **Don't build what T3 already does.** Patch it, and propose the patch upstream.

## Cost
One VPS for development and the demo: a Hetzner CAX21 (4 ARM cores, 8 GB).
