# AGENTS.md

Rules for anyone (human or agent) writing code in this repository. Read this before writing Go.

## The project
Fern gives every T3 Code thread an environment that behaves like git for everything: code, dependencies and the running database. On one server it runs as two systemd services, the daemon and T3's server ([briefs/fern-without-kubernetes.md](docs/briefs/fern-without-kubernetes.md)); the daemon runs the boxes itself with gVisor's runsc ([briefs/boxes-without-kubernetes.md](docs/briefs/boxes-without-kubernetes.md)). The Kubernetes install (agent-sandbox pods and a CSI driver) was removed on 2026-10-29: it cost too much disk and memory, and nothing tested it. It is in git history before that date.
- What we build: [docs/architecture.md](docs/architecture.md)
- Order of work: [docs/roadmap.md](docs/roadmap.md)
- Experiments: [docs/engine-local.md](docs/engine-local.md)
- Why: [docs/design.md](docs/design.md)

If the code and the docs disagree, stop and say so. Don't silently pick one.

## Words
The nouns are in [architecture §2](docs/architecture.md#2-concepts). In the code:
- **Pool:** the btrfs filesystem at `/var/lib/fern`, which holds every environment's folders (`envs/<id>/`) and snapshots.
- **Workspace:** an environment's writable folder, a btrfs subvolume: `envs/<id>/live`, or a T3 worktree at its own path.
- **Box:** an environment's sandbox, a gVisor sandbox the daemon runs (`internal/runsc`). An asleep environment has none. Its **agent box**, `<env>.agent`, runs Claude; Claude's Bash runs in the box ([briefs/claude-outside-the-box.md](docs/briefs/claude-outside-the-box.md)).
- **Daemon:** `fern-daemon` on every node (`fern-daemon.service`): the API on a Unix socket, the store, the actors and the box driver.
- **fern-init:** a box's main process. It runs the services in `services.yaml` and serves boxexec.
- **boxexec:** Fern's WebSocket protocol for running a command in a box.

## Commands
- Setup, once, and again to update: `lab/setup.sh` (the local lab; `FERN_HOST=…` for the VPS). It ends with a pairing link for the T3 app.
- After a change: `lab/dev.sh` (about a minute): unit tests and the build, then `check.sh`'s quick sections in the lab
- Unit tests: `go test -race ./...` (fake driver, runs anywhere Go runs)
- Lint: `lab/lint.sh`: golangci-lint (config in `.golangci.yml`), shellcheck on the scripts and hadolint on the Dockerfile (`.hadolint.yaml`). Fix the code, don't silence the linter.
- Fuzz: `lab/fuzz.sh` runs every `Fuzz` test for 20 s (`FUZZTIME=5m` for longer). A failure leaves its input in the package's `testdata/fuzz/`; commit it with the fix.
- The index's SQL: tables in `internal/store/index/schema.sql`, queries in `queries.sql`; `lab/sqlc.sh generate` writes the Go for them (`sqlc.yaml`). Never edit the generated files; CI fails if they are out of date (`lab/sqlc.sh diff`).
- Integration tests: `lab/check.sh` (the quick sections, under a minute in the lab) after a change, `lab/check.sh all` before a merge or a tag; in the local lab (`lab/up.sh`, a systemd node in Docker) first, then on the VPS with `FERN_HOST` (see engine-local.md)
- Perf gate: `lab/perf.sh --fast` (about 1.5 minutes, big regressions only) while working on speed or memory, and `lab/perf.sh` (about 13 minutes in the lab, 26 on the VPS) before merging anything that could change them; it fails when a metric is worse than `lab/perf/<lab|vps>.tsv`. A change that is meant to move a number updates the baseline with `lab/perf.sh --accept` in the same commit. The VPS's baseline is the one that counts.
- Did a change move a number: `lab/ab.sh -s "box t3"` builds HEAD (A) and the working tree (B), runs the sections on each in turn on the same node (A B, then B A, 6 rounds) and gives a verdict per metric (medians, change, 95% interval, p). Trust it over a single gate run: both versions see the same machine. About 25 minutes for `box` on the VPS.
- Where an operation's time and CPU go: `lab/profile.sh on` (Jaeger on the node, traces from the daemon), `lab/profile.sh run wake 10` (also `sleep`, `restore`, `snapshot`, `exec`), then `lab/profile.sh off`. Each run writes `lab/profiles/<run>/`: `summary.md` (CPU by process, hottest functions, every step's p50/p95 and the median operation's timeline), whole-node on- and off-CPU flamegraphs (VPS: perf), the daemon's Go profile and the traces. `lab/profile.sh diff A B` draws what changed; `lab/profile.sh ui` opens Jaeger. The gate refuses to run while tracing is on.
- The T3 demo, end to end: `lab/t3-run.sh toy 5` (Haiku, your subscription); `PARALLEL=5` runs five threads at once
- Everything, the same way each time: `lab/suite.sh`: every check (scale included: 1 to 20 awake boxes), the perf gate and T3 end to end, one thread at a time and five at once (about 45 minutes on the VPS). Run it before a tag, on the VPS; its summary goes into `lab/RESULTS.md`.
- The lab is shared by every worktree: its scripts wait for each other (`lab_lock`). Don't run two at once by other means.

## Style sources
Follow, in this order:
1. this file
2. the [Google Go Style Guide](https://google.github.io/styleguide/go/) (decisions + best practices)
3. [Go Code Review Comments](https://go.dev/wiki/CodeReviewComments) and [Effective Go](https://go.dev/doc/effective_go)

The same sources are available as on-demand skills in [.claude/skills/](.claude/skills/) (`go-interfaces`, `go-error-handling`, `go-concurrency`, `go-context`, `go-testing`, `go-code-review` and others). Use `go-code-review` before handing work back. Where a skill disagrees with this file, this file wins. Never regenerate `.golangci.yml` from a skill; edit it.

## Go rules

**Packages and types**
- One job per package, named after the job (`store`, `actor`, `runsc`). No `util`, `common`, `helpers`, `types` or `models` packages.
- No stutter: `store.Store`, not `store.StoreService`.
- Plain structs and constructors (`New(...)`). No builders, no getters/setters. Functional options only when a constructor really has many optional settings.
- No globals, no `init()`, no package-level mutable state. Wiring happens by hand in `cmd/`.

**Interfaces**
- Define an interface in the package that **uses** it, not the one that implements it.
- Keep interfaces small (1–3 methods). Return concrete types.
- Only add an interface when there are two implementations. A test fake counts (for example `runsc.Fake`). Never `type X interface` + `xImpl`.

**Errors**
- Handle an error, or wrap it and return it: `fmt.Errorf("restore %s: %w", env, err)`. Never both log and return it; log once, at the top.
- Compare errors with `errors.Is` / `errors.As`. Use sentinel errors (`ErrNotFound`) or error types for errors callers act on.
- No `panic` outside `main` and truly impossible states.

**Concurrency**
- Every goroutine has an owner who cancels it (a `context.Context`) and waits for it (`errgroup` or a `done` channel). Tests check for leaks with `goleak`. The one exception is a goroutine blocked reading the CLI's stdin, which nothing can interrupt: it ends at its next read, or with the process.
- An actor's state is touched only by its own goroutine. Talk to it through its mailbox. No mutex next to a mailbox.
- `context.Context` is the first parameter of anything that blocks, or does I/O. Never store it in a struct. Never `context.Background()` below `main` or tests.

**Time, processes, I/O**
- Use the `time` package directly. Tests of timers and actors run in a `testing/synctest` bubble, whose clock is fake. No `time.Sleep` in tests outside a bubble.
- Run programs with `exec.CommandContext(ctx, "btrfs", "subvolume", "snapshot", ...)`: an argument list, never `sh -c` with a built string.
- Logging with `log/slog`, structured keys (`slog.String("env", id)`). No `fmt.Print*` for logs.

**Comments and docs**
- Comment *why*, not *what*. Doc comments on exported names, as full sentences.
- No commented-out code, no TODOs without an owner and reason.

## Fern rules
- **Every multi-step change is crash-safe:** after a crash at any step, the disk shows what was under way (a marker file, a leftover copy beside the workspace, a snapshot without its row), and `Reconcile` finishes it or clears it on start. Every step is safe to repeat (idempotent).
- **The disk is the truth; SQLite is the index.** On start, reconcile what exists with what the tables say.
- **Per-turn work stays on the node.** Snapshots and execs in an awake box go to the node-local daemon only. A box's wake reads Claude's login from `/etc/fern/claude.env`.
- **Credentials never enter `live/`, golden or snapshots.**
- **No API keys.** Agents are the real `claude` / `codex` CLIs with the user's own subscription.

## Tests
- Table-driven tests with `t.Run`. Diffs with `github.com/google/go-cmp/cmp`.
- Standard `testing` only: no testify, no gomock. Hand-written fakes.
- Test behaviour through the public API of the package. Crash tests stop every multi-step change at each of its steps (`store.SetFailAt`).

## Dependencies
Allowed without asking: the standard library, `golang.org/x/...`, `sigs.k8s.io/yaml`, `modernc.org/sqlite`, `creack/pty`, `coder/websocket`, the MCP Go SDK, Prometheus and OpenTelemetry clients, `go-cmp`, `goleak`. Anything else: ask first and say why the standard library isn't enough.

## Don't
- Don't add files or abstractions "for later". Build what the current roadmap step needs.
- Don't reformat or rename code you aren't changing.
- Don't commit secrets, `.env` files, or anything from `fern.env`.
