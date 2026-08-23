# fern Code Review — Pass 0 + Pass 1

**Date:** 2026-08-23 · **Commit:** `f8fc396` · **Scope:** orientation map + architecture/boundaries
judgment. **No code was modified.** Passes 2–4 (idiom/tooling, performance, deslop) not yet run,
pending go/no-go. Module: `github.com/nebler/fern`, Go 1.24. The vendored `opencode/` checkout
(upstream source, own `.git`) is excluded — reference material, not fern code.

One calibration note before anything: the brief describes fern as wake-proxy + idle-pause +
watcher + registry. The repo today also contains a durable task subsystem (SQLite journal, five
coordinators, verification runner, GitHub App publication) roughly 3× the size of the wake/pause
spine. This pass reviews the spine in depth (read line-by-line: `workspace`, `runtime`, `watch`,
`proxy`, `registry`, `cmd/fern/up.go`) and the task stack at architecture level (structure, seams,
dependency directions) — line-level deslop there belongs to Pass 4.

---

## Pass 0 — Orient

### 0.1 Package layout and dependency graph

```
cmd/fern ──────────────────────────────────────────────┐ (imports ALL 21 internal pkgs; composition root)
   │                                                   │
   ├── internal/config          (leaf)                 │
   ├── internal/control         (leaf)                 │
   ├── internal/task            (leaf)                 │
   ├── internal/runtime         (leaf: types + Docker) │
   ├── internal/registry        → runtime              │
   ├── internal/watch           → runtime              │
   ├── internal/workspace       → runtime              │
   ├── internal/opencodeapi     (leaf)                 │
   ├── internal/githubapp       (leaf)                 │
   ├── internal/publication     → control              │
   ├── internal/verification    → task                 │
   ├── internal/taskstore       → task                 │
   ├── internal/taskresult      → task, taskstore      │
   ├── internal/proxy           → control, publication,│
   │                              runtime, task,       │
   │                              taskapi, workspace   │
   ├── internal/taskapi         → githubapp, task,     │
   │                              taskresultcoord,     │
   │                              taskstore            │
   ├── internal/workspacegithub → runtime, workspace   │
   ├── internal/taskdelivery    → runtime, task,       │
   │                              taskstore, workspace │
   ├── internal/taskexecution   → (same four)          │
   ├── internal/taskresultcoord → task, taskresult,    │
   │                              taskstore, workspace │
   ├── internal/taskverification→ task, taskstore,     │
   │                              verification         │
   └── internal/taskpublicationcoord → githubapp, task,│
                                   taskpublication,    │
                                   taskstore           │
```

- **No cycles.** No near-cycles either: dependencies form strict layers (leaves → stores →
  coordinators → api/proxy → cmd). The one hub is `internal/task` (pure domain types) — ten
  packages depend on it, it depends on nothing. That is the right kind of hub.
- `cmd/fern` importing everything is correct composition-root behavior, not a smell.
- Cross-cutting observation: three packages (`taskdelivery`, `taskexecution`,
  `taskresultcoord`) each depend on `workspace` *and* `runtime` to acquire fences. The fence
  concept lives in `workspace.Manager`; `runtime` supplies the vocabulary. Direction is
  consistent; nothing reaches back up.

### 0.2 Lifecycle of one workspace (concrete trace)

| Phase | Path through code |
| --- | --- |
| **Ingress classify** | `internal/proxy/proxy.go:259` `requestIntent` → Observe (`GET /api/event`), Read (`/api/health`, `/api/session/active`), Work (default incl. upgrades) |
| **Admit** | `proxy.newUpstreamHandler` (`proxy.go:212`) → `workspace.Manager.AcquireRequest` (`workspace/manager.go:98`) → `admitRequest` (`:524`) takes admission lease; Observe skips admission |
| **Wake/create** | `ensureTarget` (`manager.go:173`): cached endpoint ⇒ return; else coalesce into one `runWake` goroutine (`:199`) → `ensureRunning` (`:236`) takes lifecycle token → `runtime.Docker.EnsureRunningObserved` (`runtime/docker.go:385`) |
| **Runtime mutate** | `inspectByReference` (`docker.go:565`) classifies via Docker inspect + intent store → `StateAbsent`⇒`create` (`:188`: volumes, intent clear, `ContainerCreate` loopback-only, `ContainerStart`, retain-volume barrier at `:263`), `StatePaused`⇒`resumeObserved` (`:437`: fingerprint+`verifyActualSpec` drift gates, unpause/start, re-inspect) → `WaitHealthy` (`runtime/health.go`, 60 s budget, negative auth probes) |
| **Attest & publish** | back in `observeAndPublish` (`manager.go:251`): requires running+endpoint+valid image ID → `m.observe` callback (`up.go:134`) → `watch.StreamController.ConnectEndpoint` (`watch/controller.go:70`) → `replace` bumps **epoch**, spawns generation goroutine → SSE connected → `publishEndpoint` (`manager.go:275`) increments **endpoint generation** |
| **Proxy** | `httputil.ReverseProxy{FlushInterval:-1}` (`proxy.go:178–193`) with `Rewrite` stripping `Forwarded`/`X-Forwarded-*`, trusted-origin Host; release func runs after response completes |
| **Idle detect** | SSE frames → `watch.Stream` parser (`watch/event.go:38`) → `session.status` → `Observation` → `Supervisor.Run` actor loop (`watch/supervisor.go:40`) → `activityModel.apply` (`:144`) arms timer only on busy→idle drain with current epoch → timer fire → drain queued observations → `OnPause` |
| **Pause intent** | `Manager.Pause`→`AcquirePaused` (`manager.go:310`): `beginPause` (`:551`) closes admission → `acquireLifecycle` → `pauseWhileHeld` (`:401`): refuse provisioning, two passes of `watch.AllSessionsIdle` (`watch/status.go`, six surfaces) → `pauseRuntime` (`:437`) → `Docker.Pause` (`docker.go:281`) → `pauseObserved` (`:306`): `BeginPause` intent → `ContainerStop(10 s)` → `CommitPause` |
| **Crash classification** | `inspectByReference:608–649`: exited+`PauseIntentCommitted/Shutdown`(in window)⇒paused; exited+none⇒failed; OOM/dead⇒failed; frozen⇒paused |

### 0.3 Concurrency inventory

Goroutine spawn sites (non-test, total 8):

| Site | Lifetime owner | Bounded how |
| --- | --- | --- |
| `up.go` errgroup ×~9 (servers, supervisor, 5 coordinators, shutdown watcher) | `serviceCtx` | `group.Wait`; signal-cancel |
| `workspace/manager.go:188` `runWake` | Manager | coalesced singleton; `Close` awaits `wake.done` (`:486`); 90 s timeout |
| `watch/controller.go:113` generation loop | StreamController | `operations` token serializes replace/stop; `done` awaited |
| `watch/controller.go:160` per-attempt `Stream` | generation loop | `attemptCtx`; drained via `streamDone` |
| `publication/coordinator.go:226` worker | Coordinator (legacy lane — currently unwired in prod, `up.go:347–358` returns nil) | Close(ctx) |
| `verification/verification.go:743,751` process-group reaper | runner invocation | bounded teardown |

Mutexes: 16 sites, each guarding one small struct (per-coordinator `runMu`, per-store `mu`,
pairing limiter, tracker map). Channels: 21 `make(chan)` — token channels (`lifecycle`,
`operations`), broadcast-by-close (`requestsDone`, `pauseDone`, `ready`/`done`), data planes
(`observations` buf-64, per-attempt `events`). Context: `serviceCtx` threaded everywhere;
detached `context.Background()` appears 37× — sampled: all are **cleanup/shutdown paths with
explicit timeouts** (rollback, observer-failure pause, server Shutdown), several pinned by
`TestObserverRollbackUsesIndependentContext`. Timers: supervisor idle timer uses
correct stop-and-drain discipline (`supervisor.go:223`).

No goroutine can outlive its workspace: the only long-lived ones hang off `serviceCtx`, and
`Manager.Close` drains admission → pause → lifecycle → wake before Docker client close
(`up.go:114–122` even documents why the Docker client is leaked rather than raced on timeout).

### 0.4 Where state lives

| Kind | Location |
| --- | --- |
| Memory | Manager: endpoint+generation+closing+admission counters. StreamController: one `streamState`. Supervisor: `activityModel` (actor-owned, zero locks). Pairing limiter, control store cache |
| Disk | `$HOME/.fern/locks` (host lease), `control/<hash>.json` (devices/workflows/legacy pubs; atomic-replace, fsync), pause-intent JSON (hashed filename, `O_NOFOLLOW`, Nlink==1, 0600, 4 KiB cap), `$HOME/.fern/tasks/<ws>.db` (SQLite WAL, trigger-enforced transitions), `$HOME/.fern/github-app/` |
| Derived from Docker, never persisted | container state, dynamic loopback port, immutable image ID, spec-fingerprint label |
| Duplicated by design | cached endpoint vs Docker truth (invalidated by generation on transport failure); spec fingerprint label vs recomputed spec (drift detector); two persistence systems (legacy JSON control store vs SQLite task journal — documented split, ARCHITECTURE §10) |

### 0.5 Test coverage shape

Behavior-heavy, implementation-light — genuinely. Unit tests assert *policy and concurrency
outcomes*, not call shapes: `TestCanceledCallerDoesNotCancelSharedWake`,
`TestRequestCannotCrossAuthoritativePauseCheck`,
`TestStaleFailureDoesNotInvalidateNewGeneration`,
`TestConcurrentEnsureRunningCreatesOnce`,
`TestPairingAttemptsAreConcurrentOracleFreeAndDoNotPoisonOtherCodes`. The watch tests exercise
the epoch reducer directly against crafted observation sequences. Above that: a real-Docker
lifecycle harness (14 scenarios incl. 10 measured wakes), a black-box contract harness pinning
the exact opencode image digest (13 scenarios printing proven-vs-blocked properties), a
reproducible-release harness, tamper/symlink/path-escape rejection fixtures. Coverage gaps that
matter: no `-race` evidence cited here (CI claims it), no goleak, and the task-store trigger
matrix is tested but the five coordinators have thinner test files (1–2 each) relative to their
size.

### 0.6 Intended architecture, and whether the code implements it

The intended architecture is a **single-workspace lifecycle kernel behind a wake-aware ingress**:
`runtime` is the port to Docker, `watch`+`opencodeapi` the port to opencode, `workspace.Manager`
the policy kernel that serializes mutation and admits requests, `proxy` the boundary translating
HTTP into intents, `registry` the durable intent/lease substrate, `cmd/fern/up.go` an explicit
composition root, and the task stack a set of journaled coordinators over a transactional store —
everything fail-closed, nothing inferring state it cannot attest. **The code implements that
architecture, faithfully, with essentially no drift.** What drifted is *scope*, not shape: the
task/GitHub subsystem grew to dominate the codebase while the brief still describes fern as the
thin wake-proxy. The one place semantics softened under that growth is the failed-vs-paused
boundary (Finding H2). My honest headline: **your worry about hidden thoughtlessness is, at the
spine, misplaced** — this is unusually deliberate code. The findings below are real but none are
the rot you were bracing for.

---

## Pass 1 — Findings

### Critical

None found in the reviewed spine. I looked specifically for: cross-mutex ordering violations,
generation-check-then-use races, broadcast-channel reuse races, timer misuse, shutdown paths
that drop owned goroutines. The admission/lifecycle/wake interlock is correct as far as I can
trace it, and the adversarial tests cover the cases I tried to construct by hand. Saying
"nothing critical" is a finding, not filler.

### High

```markdown
[HIGH] internal/proxy/proxy.go:83-88,130-153 — Configuration validated by panic, duplicating an existing error-returning validator
What: parseTrustedOrigin/NewHandlers panic("invalid ... origin") on malformed input, while
      internal/config.ParseRemoteOrigin (config.go:897) validates the same grammar and returns errors.
Why: two validators for one concept will diverge (they already differ: proxy demands canonical raw ==
     scheme://host, config checks DNS-shape); a library-shaped package panicking on caller input is
     un-Go-like — Google Go Style Guide reserves panic for programmer error; bad config is user error.
Fix: NewHandlers returns (Handlers, error); delete parseTrustedOrigin's panic paths and have up.go feed
     the already-validated origin through. Callers: up.go:217, tests.
Source: Google Go Style Guide (panic-on-input), Go Code Review Comments (errors, not panics, for expected failure)
```

```markdown
[HIGH] internal/runtime/docker.go:266-276 + 666-674 — Failed health check manufactures "pause" intent, collapsing failed into paused
What: when create/resume transitions succeed but WaitHealthy fails, rollbackStarted calls pauseObserved,
      which Begins AND Commits a pause intent on a container that never became healthy. Subsequent
      inspection therefore classifies it StatePaused — indistinguishable from a deliberate idle pause.
Why: the state machine's whole point (runtime.go:113 comment: "distinguish an intentional pause from a
      crash") is defeated for the unhealthy-start case; a persistently broken workspace presents as
      dormancy, and every later wake repeats start→fail→stop silently. If deliberate anti-crash-loop
      behavior, it deserves its own observable state or intent flavor.
Fix: question first — is this intended? If yes: commit a distinct intent kind (e.g. FailedStart) or log/
     surface a degraded condition; if no: roll back to stopped-without-commit so classification yields failed.
```

```markdown
[HIGH] internal/workspace/manager.go (whole file) — Cross-lock ordering invariant exists only in the author's head
What: admitRequest (:524-548) nests admissionMu → isClosing() → wakeMu. No path nests wakeMu → admissionMu
      (Close takes them sequentially, :451-464). The system is deadlock-free *today* by discipline, not by
      construction or documentation.
Why: this is the definition of information hiding failure — the invariant a maintainer must know is
      nowhere stated, and Manager is precisely the file future changes (warm pool, pool-aware pause)
      will edit.
Fix: a doc comment block on Manager stating the three mechanisms (admissionMu, wakeMu, lifecycle token),
     the nesting rule, and who owns which state. Zero behavior change.
```

### Medium

```markdown
[MEDIUM] internal/runtime/docker.go:124-129,144-146 — ExecWorkspaceGH discards root causes behind a bare sentinel
What: exec create/attach/inspect failures all return bare ErrCommandFailed; the wrapped Docker error is dropped.
Why: gh-inside-workspace failures become undebuggable ("command failed" with no reason); everywhere else in
     this file errors are meticulously wrapped with %w — this function is stylistically from another codebase.
Fix: wrap causes (fmt.Errorf("exec create: %w", err)) and keep ErrCommandFailed for the caller-facing
     classification via errors.Is.
```

```markdown
[MEDIUM] internal/runtime/docker.go:744 — Spec fingerprint hashes env KEYS but not VALUES
What: specFingerprint includes sortedEnvKeys(specEnvironment(spec)) — value changes leave the label unchanged;
      drift is caught later only by verifyActualSpec comparing live env at resume (docker.go:780-785).
Why: if intentional (keeping secrets out of `docker inspect`-visible labels — likely, and clever), it is an
     undocumented security invariant that looks like an accident; if accidental, create-time drift on value-
     only changes is silently accepted until next resume.
Fix: one comment stating the keys-only choice and why, or include a salted hash of values if labels are
     deemed safe. Question: which is it?
```

```markdown
[MEDIUM] internal/proxy/proxy.go:63-73 + 169-174 — Dead production constructors and an unreachable nil-waker branch
What: New and NewWithControls have zero references outside this package's tests; newUpstreamHandler carries a
      nil-waker 503 fallback reachable only through them.
Why: test-only code compiled into the production binary, plus a defensive branch for a state NewHandlers
     cannot produce — the exact "defensive programming overkill" pattern.
Fix: move both constructors into a _test.go file (Go allows same-package test helpers there); delete the nil
     branch or reduce it to a comment.
```

```markdown
[MEDIUM] internal/watch — Backoff logic duplicated; helper reimplements a builtin
What: nextBackoff (event.go:143-151) and its inline twin in controller.runGeneration (controller.go:184-194);
      separately minDuration (supervisor.go:232-237) reimplements Go 1.21's builtin min for time.Duration.
Why: same package, two copies of retry-backoff policy that will drift (one already differs subtly in reset
     conditions); minDuration is pure noise against `min(a, b)`.
Fix: one nextBackoff used by both; delete minDuration.
```

```markdown
[MEDIUM] internal/workspace/manager.go:103,177,215,242,354,530 (+control paths) — Eight copies of one literal error
What: errors.New("workspace manager is shutting down") repeated inline; callers cannot errors.Is it.
Why: inconsistent with the package's own sentinel style (ErrRequestsActive etc. at :14-16); any future
     caller that must treat shutdown distinctly (e.g., proxy returning 503 Retry-After) will string-match.
Fix: var ErrManagerClosed, wrap where contextual detail helps.
```

```markdown
[MEDIUM] cmd/fern/up.go:106-109 + internal/publication — Wired-out subsystem kept on the hot startup path
What: newWorkspacePublisher returns (nil,nil) for every valid mode; publication.Coordinator, its controls
      plumbing, and control.Workflows UI survive as unreachable-from-config code paths.
Why: ~900 lines (publication/github.go 929 + coordinator) of maintained, tested, dead-in-production surface
     that every reader must mentally simulate. This is the task-system transition's leftover.
Fix: decide its fate — if the Amp-style workspace-gh direction is final, move publication/ + workflow routes
     behind an explicit build tag or extract to a branch/fossil doc; keep the GitHub App broker path (that
     one is live).
```

### Low

```markdown
[LOW] internal/workspace/manager.go:85 — wakeOperationTimeout hardcoded 90s as an unexported field
What: field exists but nothing can set it; not derived from config or health budget.
Fix: const, or wire to config alongside healthTimeout. (Perf pass should sanity-check 90s vs healthTimeout=60s.)
```

```markdown
[LOW] internal/workspace/manager.go:343-345,379,419-420 — Function-field nil checks at call time instead of constructor validation
What: observe/allIdle/nil-observe callbacks checked at each use site.
Fix: validate the required trio once in NewManager (it already takes them); delete per-call branches.
```

```markdown
[LOW] internal/proxy/gateway.go:63-69 — Manual prefix slice instead of strings.HasPrefix
What: len(path)>len("/fern/") && path[:len("/fern/")]=="/fern/".
Fix: strings.HasPrefix(request.URL.Path, "/fern/"); keep the exact "/fern" case. Clearer, same behavior.
```

```markdown
[LOW] internal/runtime/docker.go:81,99 — Nil-receiver paranoia on internal paths
What: d == nil || d.cli == nil guards in ResolveImageID/ExecWorkspaceGH; NewDocker cannot produce that.
Fix: delete; a nil Docker is a programmer error and should panic loudly, not return ErrSpecDrift.
```

```markdown
[LOW] internal/runtime/runtime.go:65-67 — Spec.ServerAuth implicitly reads a magic env key
What: password source is Env["OPENCODE_PASSWORD"] by string convention.
Fix: a named const shared with config (config already enforces the key elsewhere), or an explicit field.
```

```markdown
[LOW] internal/config/config.go:185-399 — Three near-twin loader families
What: Load/LoadAttach/LoadEvents × WithEnvironment variants share most of load(); attach/events loaders
      differ only in accepted sections.
Fix: acceptable as-is for a CLI; if a fourth variant appears, collapse onto loadSections + per-command decoders.
```

---

## Coupling and change amplification (requested scenarios)

**1. Warm pool of freezer-paused containers.** Cheapest of the four, because the classifier
already models frozen state (`Observation.Frozen`, defensive unpause at `docker.go:337,460,529`)
and Manager's states are intent-based, not start/stop-based. Files: `runtime/docker.go`
(add freeze path beside stop in `pauseObserved`; teach `resumeObserved` that thaw skips
WaitHealthy-or-shortens-it), `workspace/manager.go` (nothing structural — maybe a fast-path
flag), `config` (idle.mode knob), lifecycle harness expectations. **Verdict: the architecture is
ready**; the state machine does *not* assume stop/start semantics — intents are about
explanations, not mechanics. The main work is policy (RAM accounting) and measurement, not
refactoring.

**2. Multi-workspace.** The real one. Everything in the spine is singular by declaration:
`Manager` owns exactly one spec; `StreamController` one baseURL; `Supervisor` one observation
channel/model; `registry.Acquire` one lease per workspace dir; `proxy.Controls.Tasks` a
workspace-scoped store; `config.Config.Workspace` singular; `gatewayHandler` has no dispatch
dimension. Files touched: `up.go` (become per-workspace supervisor set), `workspace` (Manager →
set + routing key), `watch` (N controllers/models or a multiplexer), `proxy`/`gateway` (host/path
→ workspace resolution before intent classification), `taskapi`/`taskstore` (already
workspace-keyed — survives), `config` (list-of-workspaces schema), `registry` (fine). This is a
deliberate single-tenant posture, not an accident — but it is the change that will hurt, and the
current code buys its simplicity honestly.

**3. `/remote` session migration.** Blocked upstream regardless (import bugs staled unfixed).
If built fern-side via volume+db copy: new package (quiesce via `AcquireQuiesced` → tar volume +
git bundle → restore elsewhere), plus a CLI verb; spine untouched except exporting quiesce.
Small, additive.

**4. Runtime swap (Docker → other).** The seam is genuinely narrow: `lifecycleRuntime`
(manager.go:37, four methods) + `IntentStore` + `Spec`/`Observation`. Leaks to plug:
`PrepareShutdown` capability discovered by interface assertion (manager.go:494),
`ExecWorkspaceGH` living on concrete `*Docker` (consumed via `workspacegithub`'s own small
interfaces — survivable), and the fingerprint/labels being Docker-native. Days, not weeks.

---

## Concurrency architecture verdict

Clear ownership everywhere I traced: the supervisor is a textbook single-goroutine actor
(documented at `supervisor.go:142`); Manager splits state across exactly two locks plus a
token channel with one nesting rule (undocumented — H3); StreamController serializes
operations with a token and guards a single state cell. Context propagation is real, not
decoration — cancellation reaches SSE bodies, exec streams, and health polls. The epoch
scheme is **genuinely necessary**, not a workaround: opencode's SSE is volatile by contract
(no replay), backend restarts change the port (new generation), and a stale disconnect from a
dying connection must not veto a legitimate pause. The two generation counters (endpoint
generation in Manager, connection epoch in watch) guard different layers and compose
correctly — `TestStaleFailureDoesNotInvalidateNewGeneration` and the epoch tests pin both.

Error design: consistently `fmt.Errorf(...: %w)` + sentinels + `errors.Is` + `errors.Join`
for aggregate cleanup — above typical Go codebase standard — with two lapses (ExecWorkspaceGH
swallowing; inline shutdown literals). Logging happens once at coordination boundaries, not at
every layer.

---

## The three things I'd fix first, and in what order

1. **H1 — de-panic the proxy origin validation and deduplicate the validator.** Smallest
   diff, removes the only un-Go-like library boundary, touches two files plus tests.
2. **H2 — resolve failed-vs-paused for unhealthy starts.** First answer the intent question;
   the fix is then either a new intent flavor or a doc comment plus harness expectation. This
   is the only place the state machine lies.
3. **H3 — write the Manager concurrency-invariant comment block.** Ten minutes, protects the
   file every future feature edits.

## Things I initially flagged and then decided were correct

- **The SSE epoch/generation machinery** — my prior was workaround-for-missing-ownership; it
  is the opposite: single-owner actor, stale-event rejection is required by opencode's
  volatile-stream contract, and the tests attack exactly the subtle cases.
- **Two-pass all-idle before pause, gated by closed admission** — looks redundant until you
  see opencode exposes six unrelated activity surfaces with no atomic snapshot; the second
  pass catches activity beginning mid-check, and `TestRequestCannotCrossAuthoritativePauseCheck`
  proves the gate.
- **Detached `context.Background()` cleanup contexts (37 sites)** — sampled across rollback,
  observer-failure pause, and shutdown: all are "finish the side effect even though the
  caller left," each with a timeout, one explicitly pinned by a test. Correct pattern, would
  benefit from the odd comment.
- **~40 small consumer-defined interfaces** — the inverse of Java-in-Go; interfaces live with
  their consumers (`Store`, `Fencer`, `TargetAcquirer`, `Waker`), almost all ≤4 methods,
  several with exactly one production implementation plus a test fake. This is the Go
  convention executed correctly (Code Review Comments; Pike's "bigger the interface, the
  weaker the abstraction").
- **Pause-intent store paranoia** (Nlink, NOFOLLOW, mode bits, size caps, hashed names) —
  matches the documented tamper-evidence threat model; not slop.
- **Hand-rolled connectionTracker in up.go** — stdlib http.Server exposes no connection
  enumeration; the tracked-listener wrapper is the established pattern for force-close
  fallback after graceful shutdown.
- **Domain "pause" meaning docker stop** — jarring next to `ContainerUnpause`/Frozen handling,
  but internally consistent and partly documented; renaming would cost more than a glossary
  comment.

## Open questions (asked, not assumed)

1. H2: is unhealthy-start → committed-pause-intent deliberate anti-crash-loop behavior?
2. M-fingerprint: are env *values* deliberately excluded from the spec label (secret leakage
   via `docker inspect`)?
3. M-publication: is the legacy host-credential publisher officially retired (extract/archive)
   or awaiting re-wiring?
