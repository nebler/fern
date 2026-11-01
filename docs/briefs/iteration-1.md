# Brief: iteration 1, the first working slice (roadmap weeks 0–1)

**For:** a coding agent working alone. **Stop** when the "Done when" list is complete, or at a stop condition (§9), and write the report (§8). Don't start iteration 2.

## 1. Goal
In a local lab on the Mac (k3s inside Docker), this script passes:

```sh
lab/demo-slice.sh
# fern create demo --repo lab/sample-repo        → environment exists, asleep
# fern exec demo -- psql -c "select count(*) from users"   → wakes the box, prints 3
# fern snapshot demo s1                          → milliseconds
# fern exec demo -- psql -c "drop table users"
# fern restore demo s1                           → box stops, disk swapped
# fern exec demo -- psql -c "select count(*) from users"   → wakes again, prints 3
```

This proves the core: a `Sandbox` pod runs on a btrfs subvolume served by our CSI driver, and the whole disk, database included, can be snapshotted and restored.

## 2. Read first
1. [AGENTS.md](../../AGENTS.md): the rules. Use the `go-*` skills in `.claude/skills/`, and run `go-code-review` before you finish.
2. [roadmap.md](../history/roadmap-weeks.md) §2 (shortcuts) and §3 weeks 0–1
3. [architecture.md](../architecture.md) §3 (components), §5 (API), §9 (storage), §10 (boxes), §14.2 (Kubernetes mapping), §18 (code layout)
4. [engine-local.md](../history/engine-local.md) §1.0 (local lab) and experiments E0, E1, E4

If this brief and those docs disagree, this brief wins for this iteration. List every disagreement in the report.

## 3. Ground rules
- **Branch:** create `iter-1` from `v2`. Small commits with clear messages. **Never push.**
- **The Mac:** install nothing on the Mac itself. Everything runs in Docker containers (Go builds and tests in a `golang:1.27` container; lint in the `golangci/golangci-lint` v2 image).
- **Docker Desktop:** the user starts it. If `docker info` fails, stop and ask.
- **Cleanup:** only remove what you created. Name or label everything `fern-lab` / `app.kubernetes.io/part-of=fern`. `lab/down.sh` removes it all.
- **Untracked files in the repo** (`integration/`, `plugins/`, `fern.env`, …): don't touch, read or commit them.
- **No credentials** are needed in this iteration. Don't log in to anything.
- **Module path:** `github.com/nebler/fern`, Go 1.27.

## 4. Scope

### In
| Package / dir | What to build | Notes |
|---|---|---|
| `lab/` | `up.sh`, `down.sh`, `build.sh`, `demo-slice.sh`, `e0.sh`, `e1.sh`, `e4.sh`, `sample-repo/` | see §5 |
| `internal/store` | btrfs pool: create environment (empty subvolume), snapshot (`syncfs`, then `btrfs subvolume snapshot -r`), restore, delete, list; SQLite index of environments and snapshots | restore = make `live.new` from the snapshot, **atomically exchange** it with `live` (`unix.Renameat2` with `RENAME_EXCHANGE`), then delete the old one |
| `internal/driver` | one agent-sandbox `Sandbox` per environment: create, set `operatingMode` `Running` / `Suspended`, wait for ready / pod gone, delete; plus `driver.Fake` | the `podTemplate` carries the CSI inline volume, labels `fern.dev/env`, the box image, `restartPolicy` and resources; runc only in this iteration |
| `internal/csi` | CSI node plugin: `NodeGetInfo`, `NodeGetCapabilities`, `NodePublishVolume` (bind-mount `envs/<env>/live` to the target path), `NodeUnpublishVolume`; Identity service | ephemeral volumes only; log the pod identity from the volume context; **no authorization yet** (week 5, E14) |
| `internal/api` | unix-socket HTTP API: `PUT/GET/DELETE /envs/{id}`, `GET /envs`, `POST /envs/{id}/snapshots`, `GET /envs/{id}/snapshots`, `POST /envs/{id}/restore`, `POST /envs/{id}/exec` (WebSocket) | exec on a sleeping environment wakes it first; mutations are serialized by **one lock per environment** (iteration 2 replaces it with the actor); operations are idempotent by design (PUT, snapshot with a given ID); no `Idempotency-Key` store yet |
| `cmd/fern` | daemon (`fern daemon`, also runs the CSI plugin) and CLI (`create`, `ls`, `rm`, `snapshot`, `snapshots`, `restore`, `exec [-it]`, `sleep`, `wake`) | hand-wired in `main`; no DI, no global state |
| `cmd/fern-init` | PID 1 in the box: reads `services.yaml`, starts and supervises services, forwards signals, serves exec (stdio, optional PTY, exit code) over WebSocket on a pod port with a per-environment token | no reattach, no activity reporting yet |
| `deploy/` | plain YAML for the lab (namespace, DaemonSet, CSIDriver, RBAC, box image reference) | Helm comes in week 5 |
| `images/` | Dockerfiles: `fern` (daemon), `fern-box` (Debian slim + Postgres + `fern-init`) | |

**`sleep`** = set `Suspended` and wait until the pod is gone. **`restore`** = sleep, swap, stay asleep (the next exec wakes it).

### Out (don't build)
Actor/mailbox, op log, idle timers, golden snapshots, `.agents/setup`, fork, identity hook, the front door and previews, CRDs/operator, Helm, MCP, T3 anything, gVisor *requirement*, CSI authorization, admission policy, metrics, reattach, `adopt`, Codex/Claude.

## 5. The lab
- **`lab/up.sh`:** one privileged `rancher/k3s` container named `fern-lab` (pin a version), with:
  - a btrfs image file loop-mounted at `/var/lib/fern` inside it
  - `/` made `rshared`, so CSI mount propagation works (`mount --make-rshared /`)
  - the agent-sandbox **v1.0.7** controller (`sandbox.yaml` from that release)
  - our images imported (`docker save … | docker exec -i fern-lab ctr images import -`)
  - `deploy/` applied
- **`lab/fern`:** a wrapper so the CLI works from the Mac: `docker exec -i[t] fern-lab kubectl -n fern-system exec [-it] ds/fern -c fern -- fern "$@"`.
- **`lab/sample-repo/`:** `services.yaml` with one `postgres` service whose script runs `initdb` into `.fern/pg` if empty, then starts Postgres as a non-root user; a `seed.sql` creating `users` with 3 rows, applied once on first start.
- **Bonus (don't block on it):** try gVisor (`runsc`) in the lab and record whether it works.

## 6. Tests
- **Unit tests** (`go test -race ./...` in the Go container) for everything that can run without btrfs or Kubernetes: API handlers on `driver.Fake` with a fake store, CLI argument parsing, the `services.yaml` parser, fern-init's exec protocol (both sides, in-process).
- **Store tests** against real btrfs: run in a privileged container with a loop-mounted image (`lab/test-store.sh`); skip cleanly (`t.Skip`) when `FERN_TEST_BTRFS` is unset.
- **Scripted experiments**, each printing pass/fail and p50/p95 timings:
  - `lab/e0.sh`: E0 (`Sandbox` + CSI + btrfs; snapshot while Postgres runs; restore; `Suspended` → `Running` timing; a force-deleted pod is recreated)
  - `lab/e1.sh`: E1 (snapshot cost at 0 MB / 500 MB / 5 GB, ×20 each; restore equals snapshot by tree hash; no leaked subvolumes)
  - `lab/e4.sh`: E4 (box holds no state: a file written outside `live/` is gone after sleep/wake; data in `live/` survives; force delete → Postgres recovers)
- **Lint:** `golangci-lint run` clean, with no new `//nolint` unless it explains why.

## 7. Done when
- [ ] `lab/up.sh && lab/demo-slice.sh && lab/down.sh` passes from a clean start, twice in a row
- [ ] `lab/e0.sh`, `lab/e1.sh`, `lab/e4.sh` pass; numbers are in `lab/RESULTS.md` with the date, Docker Desktop version, kernel and btrfs-progs version
- [ ] `go test -race ./...` and `golangci-lint run` are clean
- [ ] `go-code-review` was run, and its findings are fixed or listed in the report
- [ ] about **2,000–2,500 lines of Go** outside tests (measure with `find . -name '*.go' ! -name '*_test.go' | xargs wc -l`)
- [ ] the report (§8) is written

## 8. The report: `docs/briefs/iteration-1-report.md`
- what was built, per package, with line counts (code and tests)
- how to run it (three commands)
- results of E0, E1, E4 (copied from `lab/RESULTS.md`), and whether gVisor worked
- every place you deviated from this brief or the docs, and why
- disagreements found between docs and reality (for example agent-sandbox behaviour, CSI under k3s-in-Docker)
- open questions for the user, and anything you'd change in iteration 2
- the interfaces you introduced, and for each one the second implementation that justifies it

## 9. Stop conditions (stop, write the report, don't work around)
- **CSI bind mounts don't work in the lab** after a real attempt at mount propagation. Don't fall back to `hostPath` in box pods; that would hide the core risk.
- **agent-sandbox can't carry a CSI inline volume** in its `podTemplate`, or won't suspend/resume. Record exactly what fails; don't switch to plain pods without asking.
- **Go code passes ~3,000 lines** outside tests: stop and report what made it bigger.
- **Anything needs an install on the Mac,** credentials, or a push.
