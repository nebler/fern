# Report: iteration 1, the first working slice

**Branch:** `iter-1` (from `v2`), not pushed. **Written:** 2026-10-18, plan for iteration 2 added 2026-10-20.

## Summary

- **The core risk is retired.** An agent-sandbox `Sandbox` runs on a btrfs subvolume served by our own CSI driver. The whole disk, Postgres included, is snapshotted in about 10 ms and restored in about 1.2 s. This works under runc **and under gVisor**, inside Docker Desktop.
- **Every "Done when" item holds, with two caveats.** The Go code is 2,560 lines, against a target of 2,000–2,500 (§2). Some `go-code-review` findings are still open (§7).
- **Blocker checks (2026-10-21) found and fixed two bugs:** exec broke on chunks over 32 KiB, and the first exec after a node restart used a stale pod IP. Claude Code runs in a box up to the login, under runc and gVisor (`lab/RESULTS.md`).
- **The structure is the slice's shortcut, not the final shape.** `api` holds a lock per environment and decides when to wake a box. Iteration 2 moves that into an actor per environment with an op log, the pattern used by kubelet and Fly.io's `flyd` (§9).

## 1. How to run it

```sh
lab/up.sh && lab/demo-slice.sh && lab/down.sh   # the slice, from a clean start (~50 s)
lab/go.sh test -race ./... && lab/lint.sh        # unit tests and lint, in containers
lab/e0.sh; lab/e1.sh; lab/e4.sh; lab/gvisor.sh   # experiments (need lab/up.sh)
```

- **Store tests on real btrfs:** `lab/test-store.sh`.
- **After a code change, in a running lab:** `lab/reload.sh`.
- **By hand:** after `lab/up.sh`, use `lab/fern create|ls|snapshot|restore|exec -it …`.

The demo slice creates an environment from `lab/sample-repo` (Postgres with 3 users), takes a snapshot, drops the table, restores, and shows the 3 users are back.

## 2. What was built

Three programs:

| Program | What it is |
|---|---|
| **The daemon** (`fern daemon`) | One per node, privileged: the node agent. It owns the btrfs pool, serves the API on a unix socket, serves CSI to kubelet, and relays exec to the boxes. |
| **`fern-init`** | PID 1 in every box. It runs the repo's services and serves exec. |
| **The CLI** (`fern …`) | Talks to the daemon's socket. |

The packages behind them:

| Package / dir | Code | Tests | What it does |
|---|---|---|---|
| `internal/store` | 412 | 348 | The btrfs pool plus a SQLite index. **Create:** an empty subvolume, optionally seeded from a tar extracted as uid 1000. **Snapshot:** `syncfs`, then `btrfs subvolume snapshot -r`. **Restore:** `live.new` from the snapshot, `renameat2(RENAME_EXCHANGE)`, delete the old one. Also delete and list. Every step can be repeated: leftovers (`live.new`, a snapshot without a row, a half-made environment) are cleaned up on the next call. |
| `internal/driver` | 365 | 0 | One `Sandbox` per environment, created `Suspended`. **Wake:** patch `Running`, poll until `Ready` with a current `observedGeneration`, return `podIP:7070`. **Sleep:** patch `Suspended`, poll until the pod is gone. **Delete:** waits for the Sandbox and the pod. The pod template has the CSI inline volume, the labels `fern.dev/env` and `part-of=fern`, node pinning, uid 1000, a read-only root, an `emptyDir` `/tmp`, and a readiness probe on fern-init. `driver.Fake` (87 of the 365 lines) is used by the API tests. |
| `internal/csi` | 175 | 0 | CSI Identity and Node services. `NodePublishVolume` bind-mounts `envs/<env>/live` for ephemeral volumes only, idempotently (it checks `/proc/self/mountinfo`), and logs the pod's namespace/name, UID and service account. `NodeUnpublishVolume` unmounts. There is no authorization yet. |
| `internal/boxexec` | 368 | 200 | The exec protocol, used three ways: fern-init serves it, the CLI speaks it, and the daemon relays it (`Proxy`). Each command gets one WebSocket: a JSON request, then binary frames (stdin, stdout, stderr, stdin EOF, resize), then a JSON exit code. PTYs come from `creack/pty`. |
| `internal/api` | 574 | 366 | The unix-socket HTTP API and a typed client. One mutex per environment serializes mutations and wakes. The box address is cached in memory, so an exec on an awake environment makes no Kubernetes call; if dialing it fails, the cache entry is dropped and the box is woken once more. |
| `cmd/fern` | 429 | 123 | `fern daemon` (API and CSI gRPC under one errgroup, graceful shutdown) and the CLI: `create`, `ls`, `rm`, `snapshot`, `snapshots`, `restore`, `exec [-i] [-t] [-it]`, `sleep`, `wake`. `-it` puts the terminal in raw mode and forwards SIGWINCH. |
| `cmd/fern-init` | 237 | 88 | PID 1. It reads `services.yaml` (run/stop/ready/port/env) and supervises each service in its own process group, restarting it with backoff. `/healthz` runs the ready checks. It serves exec on :7070, guarded by the per-environment token, and runs the `stop` commands on SIGTERM. |
| **Go total** | **2,560** | **1,125** | |
| `lab/` | 581 lines of shell and YAML (incl. `deploy/`, `images/`) | | `up.sh`, `down.sh`, `build.sh`, `reload.sh`, `go.sh`, `lint.sh`, `test-store.sh`, `fern` (the CLI wrapper), `demo-slice.sh`, `e0.sh`, `e1.sh`, `e4.sh`, `gvisor.sh`, `lib.sh`, `sample-repo/`, `RESULTS.md` |
| `deploy/fern.yaml` | | | Namespaces `fern-system` and `fern-boxes`, `CSIDriver fern.dev`, RBAC (a Role in `fern-boxes`), and the DaemonSet (the daemon plus node-driver-registrar) |
| `images/` | | | `fern.Dockerfile` (Debian trixie with btrfs-progs) and `fern-box.Dockerfile` (Debian trixie with Postgres 17 and fern-init, uid 1000) |

On disk, an environment looks like this:

```
/var/lib/fern/                  btrfs (in the lab: a 30 GB loop-mounted image)
  state/fern.sqlite             the index
  envs/<id>/live/               mounted at /workspace in the box
      <repo files>  .fern/pg/   code and the database, side by side
  envs/<id>/snaps/<snapshot>/   read-only
```

**Why 2,560 lines and not 2,500 or fewer:** the exec protocol plays three roles (server, client and relay), and the API ships its own client (192 lines) so that the CLI and the tests share it. Nothing is built "for later".

## 3. Results

These are Docker Desktop numbers, so they show correctness, not speed; real timings come from the VPS in week 4. Details are in `lab/RESULTS.md`.

| | Result | Key numbers |
|---|---|---|
| Demo slice | PASS; `up && demo && down` twice in a row from a clean start | snapshot 12.5 ms; restore, including stopping the box, 1.2 s |
| E0 | PASS | wake (`Suspended` → `Ready`) p50 808 ms, p95 811 ms; sleep p50 2.8 s; a force-deleted pod is back in 0.7 s and Postgres recovers |
| E1 | PASS | snapshot p50 8.9 / 8.4 / 9.2 ms at 0 MB / 500 MB / 5 GB (one earlier run: 43 ms p50 at 5 GB); restore equals the snapshot by tree hash; no leaked subvolumes |
| E4 | PASS | read-only root; `/tmp` is gone after sleep; `live/` and the database survive; stray processes don't come back; Postgres recovers from a force delete |
| gVisor (bonus) | **works** | box, CSI bind mount and Postgres under `runsc`; 20 of 20 snapshot/drop/restore/wake cycles; wake p50 811 ms |

## 4. Deviations from the brief and the docs

1. **Exec is `GET /envs/{id}/exec`, not `POST`,** because a WebSocket handshake is a GET (RFC 6455).
2. **Create is `PUT /envs/{id}?repo=<name>` with an optional tar body,** not the JSON body `{repo, class, retention, from?}` of architecture §5. The CLI runs inside the daemon pod, so `--repo DIR` streams the directory as a tar (`lab/fern` streams it from the Mac). This is a stand-in until golden builds clone the repo. Copying a whole directory is too slow for big repos; golden snapshots (week 4) fix that, since a new environment is then a ~10 ms snapshot of golden.
3. **`POST /envs/{id}/sleep` and `/wake` were added.** They are in architecture §5 but not in the brief's API list, and the CLI needs them.
4. **The CSI driver is named `fern.dev`, not `fern`** (architecture §14.2), to match the `fern.dev/env` label domain. The label is `fern.dev/env` as in the brief; the architecture says `fern/env`.
5. **Boxes live in their own namespace, `fern-boxes`,** apart from the privileged daemon in `fern-system`.
6. **The workspace is mounted at `/workspace`,** not at "the same path T3 uses" (roadmap §2). Using T3's path is week-3 work.
7. **The box token is an env var in the `Sandbox`'s pod template.** Anyone who can read Sandboxes in `fern-boxes` can see it, and processes in the box can read it from `/proc/1/environ` (fern-init only keeps it out of what its children inherit). It only grants exec in that same box. It should move to a Secret (question 1 in §8).
8. **The exec protocol is WebSocket (as in the brief), not agent-sandbox's `sandboxd` gRPC (architecture §3).**
9. **Extra lab scripts:** `go.sh`, `lint.sh`, `reload.sh`, `lib.sh` and `gvisor.sh`. `down.sh` also removes the Go and lint cache volumes and the `fern:dev` / `fern-box:dev` images, so "removes it all" is literal. BuildKit's cache isn't removed.
10. **The data model is a subset** of architecture §4: `environments(id, repo, head, token, created_at)` and `snapshots(env, id, created_at)`, with no generation, sessions, ops or timers. There is no startup reconcile ("the disk is the truth"). Instead, `Create` wipes anything a previous environment with the same ID left on disk without a row.
11. **`.golangci.yml`:** gofumpt's deprecated `extra-rules: true` was replaced by `extra: {group-params: true, clothe-returns: true}`, which behaves the same. All findings were fixed in code; there is no `//nolint` anywhere.
12. **E1 runs 20 snapshots per size (as in the brief), not 100 (as in engine-local).** Fork isn't covered; it's out of scope.

## 5. Docs vs reality

- **agent-sandbox v1.0.7 carries a CSI inline volume in `podTemplate`, and suspends and resumes cleanly.** Suspend deletes the pod (condition `Suspended=True`, reason `PodTerminated`). The pod has the Sandbox's name and a controller reference to it, and `status.podIPs` gives its address. A Sandbox created `Suspended` has no conditions until the controller first reconciles it. A force-deleted pod is recreated in under 1 s.
- **CSI works under k3s in Docker.** It needs `mount --make-rshared /` in the k3s container, `Bidirectional` propagation on `/var/lib/kubelet/pods` in the daemon pod, and the node-driver-registrar sidecar, which the brief didn't list but kubelet needs. Kubelet unmounts (`NodeUnpublishVolume`) before the pod object disappears, so "the pod is gone" is a safe point to swap `live/`.
- **gVisor works in Docker Desktop,** although roadmap and engine-local expected it might not. Two practical notes: the `releases/release/latest/<arch>/runsc` URLs return 404, so use a dated release's `gvisor.tar.bz2`; and k3s auto-detects crun, nvidia and wasm runtimes but not `runsc`, which needs a containerd drop-in.
- **Snapshots capture lock files.** A snapshot taken while Postgres runs contains `postmaster.pid`. After a restore under gVisor, which hands out PIDs deterministically, another process could hold that PID, and Postgres refused to start (about 1 in 7 restores). The sample service now deletes the stale pid file. This belongs in the setup contract (architecture §8): services must treat pid and lock files in `live/` as stale on start.
- **Wake time is set by the readiness probe** (`periodSeconds: 1`): about 0.8 s, or 1.8 s when the first probe misses. That is well inside the 15 s budget (architecture §20), and can be cut further (question 4 in §8).
- **Sleep takes about 2.8 s** under runc (Postgres's `stop`, then kubelet's teardown), and about 0.8 s on some gVisor runs. Not investigated.
- **Docker Desktop's containerd image store** needs `docker save --platform linux/<arch>` for `ctr images import` to work.
- **Claude in a box wasn't tried.** Roadmap week 1 says `fern exec -it <env> -- claude` should work by hand, but the brief puts Claude out of scope and forbids logging in to anything. Claude isn't in the box image yet. Exec runs any command, so it should work once it is.

## 6. Interfaces introduced, and their second implementations

| Interface | Where (the consumer) | Implementation 1 | Implementation 2 |
|---|---|---|---|
| `api.Store` (7 methods) | `internal/api` | `*store.Store` | `fakeStore` in `api_test.go` |
| `api.Driver` (5 methods) | `internal/api` | `*driver.Driver` | `*driver.Fake` |
| `interface{ Scan(...any) error }` (inline) | `store.scanEnv` | `*sql.Row` | `*sql.Rows` |

`csi.New` takes a `livePath` function rather than an interface (one caller, one method).

**Against AGENTS.md:** `api.Store` and `api.Driver` are bigger than 1–3 methods. Splitting them now would be artificial, because a single lock-and-call handler uses them all. The actor in iteration 2 is the natural place to narrow them.

## 7. `go-code-review` findings

**Fixed:**
- all golangci-lint findings (errcheck, nilerr, staticcheck SA4023, contextcheck, gofumpt);
- restore with an invalid snapshot ID now returns `invalid` instead of `not_found`;
- `create --repo` checks that the directory exists before streaming it;
- driver timeouts now say "not done after 2m0s" instead of showing the client rate limiter's error.

**Not fixed (should-fix, planned in §9):**
1. **fern-init doesn't reap zombies.** As PID 1, it leaves orphans of exec'd commands (for example a daemonized `pg_ctl start`) as zombies.
2. **TTY exec waits for the terminal to close.** A background job that keeps the terminal open keeps `fern exec -t` from returning, as with ssh.
3. **`boxexec.Run` doesn't wait for its stdin goroutine,** because a blocked `Read` on `os.Stdin` can't be interrupted. This is documented in the code; it's an exception to "every goroutine is waited for".
4. **The box token** (deviation 7).
5. **`api.Server.envs` never shrinks:** it keeps one small struct for every environment ID it has seen.
6. **`csi` ignores the `readonly` flag.** It should refuse it or honour it.
7. **There are no unit tests for `driver` or `csi`.** E0 and E4 cover them. The controller-runtime fake client would make driver tests cheap.
8. **`store` stamps times with `time.Now()`** rather than an injected clock. These are only timestamps; there are no timers.

## 8. Open questions for you

1. **Token transport:** is it OK to move the box token to a per-environment Secret? That's one more API object per environment, created once, not per turn.
2. **CSI driver name:** keep `fern.dev`, or change it to `fern` as in architecture §14.2?
3. **Interface size:** accept the 5–7-method interfaces in `api` until the actor lands, or split them now?
4. **Wake path:** the daemon could poll fern-init's `/healthz` directly, instead of waiting for kubelet's probe and the controller's `Ready` condition. That would cut a wake to roughly pod start plus services. Worth doing in week 2?
5. **gVisor:** it works locally. Should it be the default class in the lab from week 2, so the MVP demo's gVisor box is tested every day?

## 9. Plan for iteration 2

### 9.1 Structure: split `api`

Today `api` decodes requests, holds a lock per environment and decides when to wake a box. Three jobs in one package make the code hard to reason about, and every new operation would add branches to it. Iteration 2 splits it the way architecture §3 already describes, so that dependencies only point one way:

```
edges (translate)         core (decides)            backends (do)
api   (HTTP, CLI/T3) ─┐
csi   (gRPC, kubelet) ├─▶ actor, one per env ─────▶ store  (btrfs)
boxexec (WebSocket)  ─┘   steps written to oplog ─▶ driver (Kubernetes)
```

- **`api`** only decodes a request, calls the actor and encodes the answer. It has no locks and no wake logic.
- **`actor`** is the only place where an environment changes state. Each operation is a fixed list of idempotent steps, so a new operation adds a new list rather than `if` branches in old code. It also narrows `api.Store` and `api.Driver` (§6).
- **`oplog`** records each step before it runs, so a half-done operation finishes after a crash.
- **`wake`** (the front door) turns a burst of requests into one wake.
- **The daemon stays one process.** It is the node agent, like kubelet; `cmd/fern` only wires the pieces together.

In pattern names: ports and adapters on the outside, an actor per environment in the core, and a write-ahead log of idempotent steps for operations. Reconcile loops join in weeks 5–8, for the `Environment` CRDs.

### 9.2 Prior art

Mature Go projects with the same problem are built this way:

| Project | What it does | What we copy |
|---|---|---|
| Fly.io [`flyd`](https://fly.io/blog/carving-the-scheduler-out-of-our-orchestrator/) | One daemon per server, the source of truth for its own VMs. Operations are state machines whose steps are logged in BoltDB, so a restarted `flyd` "picks up right where it left off". Its author would use SQLite today ([interview](https://fly.io/blog/the-exit-interview-jp/)). | The op log: an operation is a list of steps, logged in SQLite and resumed after a crash. Every log line is tagged with the operation, the step and the environment ID. |
| [E2B](https://github.com/e2b-dev/infra/blob/main/docs/ARCHITECTURE.md) | AI sandboxes. One root Go orchestrator per node, and `envd` in every VM (processes with streamed I/O, files, health, token auth). | Confirms the daemon and `fern-init` split. |
| [kubelet](https://github.com/kubernetes/kubernetes/blob/v1.2.2/docs/proposals/pod-lifecycle-event-generator.md) | One goroutine per pod with a one-slot mailbox; while a pod is busy, only the newest update is kept. Polling every pod scaled badly and was replaced by events. | The actor: one goroutine per environment, newest update wins. Watch instead of poll. |
| [Incus](https://github.com/lxc/incus/tree/main/internal/server/storage) | Storage logic in `storage/backend.go`, plus one file per filesystem under `drivers/` (`driver_btrfs.go`, `driver_zfs.go`, `driver_mock.go`) behind one interface. | Split `store` this way when ZFS arrives, not before. |
| [Go module layout](https://go.dev/doc/modules/layout) | Binaries in `cmd/`, server code in `internal/`, and `main` does the wiring. | Already the layout. |

**Not copied:** containerd's plugin system, and E2B's central control plane (Redis, Postgres, ClickHouse). Both serve a multi-tenant cloud.

### 9.3 Box image

Make the base image the repo's choice (for example its devcontainer image), and inject `fern-init`, a static binary, into any image through an init container or an image volume.

- **Default: Debian slim.** Agent tools and prebuilt binaries expect glibc, which rules out Alpine.
- **Hardened option: Wolfi,** which is glibc-based, minimal and patched daily.

This changes architecture §10, which plans one shared base image.

### 9.4 What the blocker checks changed

`lab/blockers.sh` (results in `lab/RESULTS.md`) adds four items:

- **Claude's login must survive sleep.** Claude keeps its state in `$HOME/.claude*`, and `$HOME` is `/tmp`, an `emptyDir`, so a login is lost on every sleep. It needs its own volume outside `live/` (architecture §12 already says credentials never enter `live/`). This blocks real turns.
- **T3's MCP tools are still untested.** T3's MCP server listens on the host's loopback, which a box can't reach. The fallback is to have `fern-init` forward one port back (roadmap risk table). Testing it needs T3.
- **Waking many environments at once is slow:** 10 at once took 6.5 s, against 0.8 s for one. The driver polls each Sandbox every 200 ms, through a client with client-go's default rate limit. Watching instead of polling (item 3 below) should fix it; measure again afterwards.
- **A cancelled exec prints an error** ("failed to get reader: context canceled") instead of exiting quietly.

### 9.5 Work list

In order:

1. **The actor** replaces the per-environment mutex, and adds a generation number so that a restore cancels open execs instead of killing their pod underneath them. This also lets `api.Server.envs` shrink (finding 5).
2. **The op log** for create, restore and delete, plus a startup reconcile of `envs/` against the index (deviation 10).
3. **Faster, cheaper waits:** watch the Sandbox, or poll fern-init directly (question 4), instead of polling the API every 200 ms.
4. **fern-init:** reap zombies (finding 1), and remove known stale pid and lock files (`postmaster.pid`) before starting services. Document "pid and lock files are stale on start" in the setup contract.
5. **The box token** moves to a Secret, if question 1 is a yes (finding 4).
6. **Tests:** driver tests with the controller-runtime fake client, and CSI tests (finding 7).
7. **Claude's credentials** on a volume outside `live/` that survives sleep (§9.4).
8. **Disk-space guard and pruning:** keep the last N snapshots per environment, and refuse a new snapshot with a clear error when the pool is nearly full.
9. **CI** runs unit tests, the btrfs store tests and lint (`.github/workflows/ci.yml`, added 2026-10-21; it runs once the branch is pushed).

By hand, needs your login: `lab/fern exec -it <env> -- claude`, log in, and ask it to query the sample database. That is the last open check of the "agent inside the box" risk.
