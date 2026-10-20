# Report: iteration 1, the first working slice

**Branch:** `iter-1` (from `v2`), not pushed. **Date:** 2026-10-18.

**Result:** every "Done when" item holds, with two caveats: Go code is 2,560 lines (the target was 2,000–2,500), and `go-code-review` findings that weren't fixed are listed in §7. The core risk is retired: an agent-sandbox `Sandbox` runs on a btrfs subvolume served by our CSI driver, and the whole disk, Postgres included, is snapshotted in ~10 ms and restored. It works under runc **and under gVisor** in Docker Desktop.

## 1. How to run it

```sh
lab/up.sh && lab/demo-slice.sh && lab/down.sh   # the slice, from a clean start (~50 s)
lab/go.sh test -race ./... && lab/lint.sh        # unit tests and lint, in containers
lab/e0.sh; lab/e1.sh; lab/e4.sh; lab/gvisor.sh   # experiments (need lab/up.sh)
```
Store tests on real btrfs: `lab/test-store.sh`. After a code change in a running lab: `lab/reload.sh`.

## 2. What was built

| Package / dir | Code | Tests | What it does |
|---|---|---|---|
| `internal/store` | 412 | 348 | btrfs pool + SQLite index. Create (empty subvolume, optional tar seed extracted as uid 1000), snapshot (`syncfs`, `btrfs subvolume snapshot -r`), restore (`live.new` from the snapshot, `renameat2(RENAME_EXCHANGE)`, delete the old one), delete, list. Every step can be repeated: leftovers (`live.new`, a snapshot without a row, a half-made env) are handled on the next call. |
| `internal/driver` | 365 | 0 | One `Sandbox` per environment, created `Suspended`. Wake = patch `Running`, poll until `Ready` (with `observedGeneration` current), return `podIP:7070`. Sleep = patch `Suspended`, poll until the pod is gone. Delete waits for the Sandbox and pod. Pod template: CSI inline volume, labels `fern.dev/env` + `part-of=fern`, node pinning, uid 1000, read-only root, `emptyDir` `/tmp`, readiness probe on fern-init. `driver.Fake` (87 of the 365 lines) for API tests. |
| `internal/csi` | 175 | 0 | Identity + Node services. `NodePublishVolume` bind-mounts `envs/<env>/live` (ephemeral volumes only, idempotent via `/proc/self/mountinfo`); logs the pod's namespace/name, UID and service account. `NodeUnpublishVolume` unmounts. No authorization. |
| `internal/boxexec` | 368 | 200 | The exec protocol, used three ways: fern-init serves it, the CLI speaks it, and the daemon relays it (`Proxy`). One WebSocket per command: JSON request, binary frames (stdin, stdout, stderr, stdin EOF, resize), JSON exit code. PTY via `creack/pty`. |
| `internal/api` | 574 | 366 | Unix-socket HTTP API + typed client. One mutex per environment serializes mutations and wakes. The box address is cached in memory, so an exec on an awake environment makes no Kubernetes call; a failed dial forgets it and wakes once more. |
| `cmd/fern` | 429 | 123 | `fern daemon` (API + CSI gRPC, errgroup, graceful shutdown) and the CLI (`create`, `ls`, `rm`, `snapshot`, `snapshots`, `restore`, `exec [-i] [-t] [-it]`, `sleep`, `wake`); raw terminal + SIGWINCH for `-it`. |
| `cmd/fern-init` | 237 | 88 | PID 1: reads `services.yaml` (run/stop/ready/port/env), supervises each service in its own process group with backoff, `/healthz` runs the ready checks, serves exec on :7070 with the per-environment token, runs `stop` commands on SIGTERM. |
| **Go total** | **2,560** | **1,125** | |
| `lab/` | 581 lines of shell + YAML (incl. `deploy/`, `images/`) | | `up.sh`, `down.sh`, `build.sh`, `reload.sh`, `go.sh`, `lint.sh`, `test-store.sh`, `fern` (CLI wrapper), `demo-slice.sh`, `e0.sh`, `e1.sh`, `e4.sh`, `gvisor.sh`, `lib.sh`, `sample-repo/`, `RESULTS.md` |
| `deploy/fern.yaml` | | | namespaces `fern-system` + `fern-boxes`, `CSIDriver fern.dev`, RBAC (Role in `fern-boxes`), DaemonSet (daemon + node-driver-registrar) |
| `images/` | | | `fern.Dockerfile` (Debian trixie + btrfs-progs), `fern-box.Dockerfile` (Debian trixie + Postgres 17 + fern-init, uid 1000) |

**Why 2,560 and not ≤2,500:** the exec protocol serves three roles (server, client, relay) and the API ships its own client (192 lines) so the CLI and the tests share it. Nothing is built "for later".

## 3. Results (from `lab/RESULTS.md`)

| | Result | Key numbers (Docker Desktop, correctness only) |
|---|---|---|
| demo slice | PASS; `up && demo && down` twice in a row from clean | snapshot 12.5 ms; restore incl. stopping the box 1.2 s |
| E0 | PASS | wake (`Suspended` → `Ready`) p50 808 / p95 811 ms; sleep p50 2.8 s; force-deleted pod back in 0.7 s, Postgres recovers |
| E1 | PASS | snapshot p50 8.9 / 8.4 / 9.2 ms at 0 MB / 500 MB / 5 GB (one earlier run: 43 ms p50 at 5 GB); restore = snapshot by tree hash; no leaked subvolumes |
| E4 | PASS | read-only root; `/tmp` gone after sleep; `live/` and DB survive; stray processes don't return; Postgres recovers from force delete |
| gVisor (bonus) | **works** | box + CSI bind mount + Postgres under `runsc`; 20 of 20 snapshot/drop/restore/wake cycles; wake p50 811 ms |

## 4. Deviations from the brief and the docs

1. **Exec is `GET /envs/{id}/exec`, not `POST`.** A WebSocket handshake is a GET (RFC 6455).
2. **Create takes `PUT /envs/{id}?repo=<name>` with an optional tar body**, not the JSON body `{repo, class, retention, from?}` of architecture §5. The CLI runs inside the daemon pod, so `--repo DIR` streams the directory as a tar (`lab/fern` streams it from the Mac). This is a stand-in until golden builds clone the repo.
3. **`POST /envs/{id}/sleep` and `/wake`** were added (they are in architecture §5, not in the brief's API list) because the CLI needs them.
4. **CSI driver name `fern.dev`**, not `fern` (architecture §14.2), to match the `fern.dev/env` label domain. The label is `fern.dev/env` as in the brief (architecture says `fern/env`).
5. **Boxes live in their own namespace `fern-boxes`**, apart from the privileged daemon in `fern-system`.
6. **The workspace is mounted at `/workspace`**, not "the same path T3 uses" (roadmap §2). That shortcut is week-3 work.
7. **The box token is an env var in the `Sandbox`'s pod template.** Anyone who can read Sandboxes in `fern-boxes` can see it, and processes in the box can read it from `/proc/1/environ` (fern-init only removes it from what children inherit). It only grants exec in that same box. It should move to a Secret.
8. **The protocol is WebSocket (brief), not agent-sandbox's `sandboxd` gRPC (architecture §3).**
9. **Extra lab scripts:** `go.sh`, `lint.sh`, `reload.sh`, `lib.sh`, `gvisor.sh`. `down.sh` also removes the Go/lint cache volumes and the `fern:dev` / `fern-box:dev` images, so "removes it all" is literal. BuildKit's cache isn't removed.
10. **The data model is a subset** of architecture §4 (`environments(id, repo, head, token, created_at)`, `snapshots(env, id, created_at)`); no generation, sessions, ops or timers. There is no startup reconcile ("disk is the truth"); instead `Create` wipes anything an env ID left on disk without a row.
11. **`.golangci.yml`:** gofumpt's deprecated `extra-rules: true` was replaced by `extra: {group-params: true, clothe-returns: true}`, which is the same behaviour. All findings were fixed in code; there is no `//nolint` anywhere.
12. **E1 runs 20 snapshots per size (brief), not 100 (engine-local).** Fork isn't covered (out of scope).

## 5. Docs vs reality

- **agent-sandbox v1.0.7 carries a CSI inline volume in `podTemplate` and suspends and resumes cleanly.** Suspend deletes the pod (condition `Suspended=True`, reason `PodTerminated`); the pod has the Sandbox's name and a controller reference to it; `status.podIPs` gives the address. A Sandbox created `Suspended` has no conditions until the controller first reconciles it. A force-deleted pod is recreated in under 1 s.
- **CSI under k3s-in-Docker works** with `mount --make-rshared /` in the k3s container, `Bidirectional` propagation on `/var/lib/kubelet/pods` in the daemon pod, and the node-driver-registrar sidecar (the brief didn't list it; kubelet needs it). Kubelet unmounts (`NodeUnpublishVolume`) before the pod object disappears, so "pod gone" is a safe point to swap `live/`.
- **gVisor works in Docker Desktop** (roadmap/engine-local expected it might not). Two practical notes: the `releases/release/latest/<arch>/runsc` URLs return 404, so use a dated release's `gvisor.tar.bz2`. k3s auto-detects crun, nvidia and wasm runtimes but not `runsc`, which needs a containerd drop-in.
- **Snapshots capture lock files.** A snapshot taken while Postgres runs contains `postmaster.pid`. After a restore, under gVisor's deterministic PIDs, another process could hold that PID and Postgres refused to start (about 1 in 7 restores). The sample service now deletes the stale pid file. This belongs in the setup contract (architecture §8): services must treat pid/lock files in `live/` as stale on start.
- **Wake time is set by the readiness probe** (`periodSeconds: 1`): about 0.8 s, or 1.8 s when the first probe misses. That is well inside the 15 s budget (architecture §20).
- **Sleep takes about 2.8 s** under runc (Postgres `stop`, then kubelet teardown), and about 0.8 s on some gVisor runs. Not investigated.
- **Docker Desktop's containerd image store** needs `docker save --platform linux/<arch>` for `ctr images import` to work.

## 6. Interfaces introduced, and their second implementations

| Interface | Where (the consumer) | Implementation 1 | Implementation 2 |
|---|---|---|---|
| `api.Store` (7 methods) | `internal/api` | `*store.Store` | `fakeStore` in `api_test.go` |
| `api.Driver` (5 methods) | `internal/api` | `*driver.Driver` | `*driver.Fake` |
| `interface{ Scan(...any) error }` (inline) | `store.scanEnv` | `*sql.Row` | `*sql.Rows` |

`csi.New` takes a `livePath` function rather than an interface (one caller, one method). **Against AGENTS.md:** `api.Store` and `api.Driver` are bigger than 1–3 methods. Splitting them would be artificial while one lock-and-call handler uses them all; the actor in iteration 2 is the natural place to narrow them.

## 7. `go-code-review` findings

**Fixed:** all golangci-lint findings (errcheck, nilerr, staticcheck SA4023, contextcheck, gofumpt); restore with an invalid snapshot ID now returns `invalid` instead of `not_found`; `create --repo` checks that the directory exists before streaming; driver timeouts now say "not done after 2m0s" instead of the client rate limiter's error.

**Not fixed (should-fix, for iteration 2):**
1. **fern-init doesn't reap zombies.** As PID 1, orphans of exec'd commands (for example a daemonized `pg_ctl start`) will stay as zombies.
2. **TTY exec waits for the terminal to close**, so a background job that keeps the terminal open keeps `fern exec -t` from returning (as with ssh).
3. **`boxexec.Run` doesn't wait for its stdin goroutine**: a blocked `Read` on `os.Stdin` can't be interrupted. This is documented in the code; it's an exception to "every goroutine is waited for".
4. **The box token** (deviation 7).
5. **`api.Server.envs` never shrinks** (one small struct per environment ID ever seen).
6. **`csi` ignores the `readonly` flag**; it should refuse it or honour it.
7. **No unit tests for `driver` or `csi`.** They are covered by E0/E4 only. The controller-runtime fake client would make driver tests cheap.
8. **`store` stamps times with `time.Now()`** rather than an injected clock (only timestamps; no timers).

## 8. Open questions for you

1. **Token transport:** OK to move the box token to a per-environment Secret in iteration 2 (one more API object per environment, created once, not per turn)?
2. **CSI driver name:** keep `fern.dev`, or change it to `fern` as in architecture §14.2?
3. **Interface size rule:** accept 5–7-method consumer interfaces in `api` until the actor lands, or split now?
4. **Wake path:** the daemon could poll fern-init's `/healthz` directly instead of waiting for kubelet's probe and the controller's `Ready` condition. That would cut wake to roughly pod start plus services. Worth doing in week 2?
5. **gVisor works locally.** Make it the default class in the lab from week 2, so the MVP demo's gVisor box is tested every day?

## 9. What I'd change in iteration 2

**First: split `api`.** Today it decodes requests, holds a lock per environment and decides when to wake a box. Iteration 2 splits it the way architecture §3 already describes, so that dependencies only point one way:

```
edges (translate)         core (decides)            backends (do)
api   (HTTP, CLI/T3) ─┐
csi   (gRPC, kubelet) ├─▶ actor, one per env ─────▶ store  (btrfs)
boxexec (WebSocket)  ─┘   steps written to oplog ─▶ driver (Kubernetes)
```

- `api` only decodes, calls the actor and encodes. No locks and no wake logic.
- `actor` is the only place where an environment changes state. Each operation is a fixed list of idempotent steps, so new operations add lists, not `if` branches in old code.
- `wake` (the front door) turns a burst of requests into one wake. `oplog` lets a half-done operation finish after a crash.
- The daemon stays one process: it is the node agent, like kubelet. `cmd/fern` only wires the pieces together.

**Prior art.** This is how mature Go projects with the same problem are built:

| Project | What it does | What we copy |
|---|---|---|
| Fly.io [`flyd`](https://fly.io/blog/carving-the-scheduler-out-of-our-orchestrator/) | one daemon per server, the source of truth for its own VMs; operations are state machines whose steps are logged in BoltDB, so a restarted `flyd` "picks up right where it left off". Its author would use SQLite today ([interview](https://fly.io/blog/the-exit-interview-jp/)) | the op log: an operation is a list of steps logged in SQLite and resumed after a crash; tag every log line with operation, step and environment ID |
| [E2B](https://github.com/e2b-dev/infra/blob/main/docs/ARCHITECTURE.md) | AI sandboxes: one root Go orchestrator per node, and `envd` in every VM (processes with streamed I/O, files, health, token auth) | confirms the daemon + `fern-init` split |
| [kubelet](https://github.com/kubernetes/kubernetes/blob/v1.2.2/docs/proposals/pod-lifecycle-event-generator.md) | one goroutine per pod with a one-slot mailbox; while a pod is busy only the newest update is kept; per-pod polling scaled badly and was replaced by events | the actor: one goroutine per environment, newest update wins; watch instead of poll |
| [Incus](https://github.com/lxc/incus/tree/main/internal/server/storage) | storage logic in `storage/backend.go`, one file per filesystem under `drivers/` (`driver_btrfs.go`, `driver_zfs.go`, `driver_mock.go`) behind one interface | split `store` this way when ZFS arrives, not before |
| [Go module layout](https://go.dev/doc/modules/layout) | binaries in `cmd/`, server code in `internal/`, `main` wires | already the layout |

Not copied: containerd's plugin system and E2B's central control plane (Redis, Postgres, ClickHouse). Both serve a multi-tenant cloud.

**Box image.** Make the base image the repo's choice (for example its devcontainer image) and inject `fern-init`, a static binary, into any image, through an init container or an image volume. Keep Debian slim as the default: agent tools and prebuilt binaries expect glibc, which rules out Alpine. Offer Wolfi (glibc, minimal, patched daily) as a hardened option. This changes architecture §10, which plans one shared base image.

Then:

- Replace the per-environment mutex with the actor; add the generation number, so a restore cancels open execs instead of killing their pod underneath them.
- Op log for restore/delete/create, plus a startup reconcile of `envs/` against the index.
- Watch the Sandbox (or poll fern-init directly) instead of polling the API every 200 ms.
- Zombie reaping in fern-init.
- Put "pid/lock files are stale on start" into the setup contract docs, and have fern-init remove known ones (`postmaster.pid`) before starting services.
- Driver tests with the controller-runtime fake client.
