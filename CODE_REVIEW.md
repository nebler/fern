# Fern Foundation Review

Date: 2026-08-12

> **Document status:** This is a historical remediation record, not a complete
> current security or architecture audit. Its resolved findings still explain
> why the foundation has its present shape, but some assurance language is
> broader than the checked-in automated tests. Later implementation work added
> pre-wake authentication, explicit local-Docker enforcement, and endpoint
> invalidation after failed pause attempts. Read
> [ARCHITECTURE_DEEP_DIVE.md](./ARCHITECTURE_DEEP_DIVE.md) for the current
> source-verified system model and limitations.

This document records the Go and Rich Hickey-style foundation review and the remediation completed immediately afterward. The upstream `opencode/` checkout was excluded from implementation review.

## Result

The original implementation passed tests but had two critical safety flaws: Docker ownership was not verified, and an SSE observation gap could leave an old idle timer eligible to stop new work. The foundation has been refactored so lifecycle decisions now depend on explicit current values rather than names, historical readiness, or elapsed-time heuristics.

## Resolved Findings

| Finding | Resolution |
|---|---|
| Foreign containers could be stopped by name | Every mutation verifies Fern ownership and workspace labels; adapter tests cover all lifecycle entry points and real-Docker integration passes |
| Watcher gaps retained pause eligibility | Connected/disconnected/status observations carry epochs; disconnect clears eligibility |
| Health ignored Basic auth | One `ServerAuth` value is used by health, SSE, and `/session/status` |
| Wake used `context.Background()` | Wake derives from service lifetime and is joined before dependency cleanup |
| Stream readiness meant connected once | Controller tracks current connected generation and clears failed generations |
| Five-second request grace approximated state | Full request leases plus explicit request observations replace the grace period |
| HTTP shutdown was not awaited | Server and supervisor run under `errgroup`; shutdown and lifecycle completion are awaited |
| Runtime collapsed crash/OOM into pause | Rich `runtime.Observation` and explicit `failed` state preserve failure facts |
| Config errors occurred after Docker effects | Strict normalization/validation and TCP bind happen before Docker mutation |
| `resume` bypassed one-writer lock | Every mutating CLI command acquires the workspace lease |
| Existing container ignored changed config | Deterministic desired-spec fingerprint detects drift |
| Event queue carried token firehose | Lifecycle controller emits only typed connection and session-status observations |
| Memory units were reinterpreted | Memory is parsed once to checked bytes with decimal/binary semantics preserved |
| Missing env references became empty | Required expansion errors on missing variables and explicit empty server passwords |
| SSE parser handled only one data line | Parser accumulates complete SSE frames and supports multiline data |
| Lock errors all looked like contention | Only `EWOULDBLOCK`/`EAGAIN` mean contention; metadata errors propagate |
| Image inputs drifted | Node base, OpenCode version, and OpenCode checksums are pinned |
| Long-lived clients blocked shutdown | Request contexts inherit service cancellation; remaining and hijacked connections are tracked and force-closed after the drain deadline |
| Partial create/resume could leak running compute | Runtime rolls back post-start failures with an independent bounded cleanup context |
| Name could be replaced after ownership inspection | Every mutation uses the verified immutable container ID |
| Canceled stream generation could publish stale connected state | Generations are joined, stale callbacks cannot update state, and older connected epochs are rejected |
| Manager close omitted pause/admission work | Close shuts admission, crosses the lifecycle barrier, and waits for wake operations |
| Admission mutex ignored request cancellation | Admission is a context-aware channel gate |
| Fingerprint label could lie about actual Docker settings | Resume inspects actual image, memory, init, environment, mounts, and ports |
| `null` status authorized stop | Authoritative status requires a JSON object |
| Queued disconnect could lose to timer select | Timer path drains queued observations before pause consideration |
| External clean exit looked intentionally paused | Atomic pause intent is persisted against immutable container ID |
| Broken config could block emergency down/status/logs | Explicit workspace name uses a narrow config-independent command path |
| Environment values could not contain `$` | `$$` escapes a literal dollar and required references remain strict |
| Trailing YAML documents were ignored | Loader requires EOF after the first strict document |

## Go Design After Refactor

### Goroutine ownership

- The root signal context owns one `errgroup`.
- Server and supervisor are group members.
- Manager wake operations derive from the service context.
- `Manager.Close` prevents new operations and awaits the current one.
- Stream generations have explicit cancel and done channels.

### Concurrency ownership

- `wakeMu` owns exactly one shared wake call.
- `lifecycleMu` serializes runtime transitions.
- the context-aware admission gate owns held request count and closes the pre-pause race.
- `operationMu` serializes stream generation replacement.
- The supervisor goroutine exclusively owns and transitions the activity model.

### Error policy

- Unknown ownership is an error, never adoption.
- Unknown activity is not idle.
- Failed/OOM compute is not automatically resumed.
- Spec drift is explicit and actionable.
- Auth/status failures leave compute running.
- Listener failure happens before compute creation.

## Rich Hickey Reading

The refactor favors values over places:

- desired workspace is `runtime.Spec`;
- observed compute is `runtime.Observation`;
- current activity is epoch-tagged `watch.Observation`;
- request intent distinguishes holding compute from potentially starting work;
- activity transition is an explicit single-owner `activityModel.apply` method;
- timer, Docker, HTTP, and SSE remain effects around that core.

Important distinctions are no longer braided:

| Concepts | Kept separate as |
|---|---|
| readiness vs idleness | health result vs session-status observation |
| lookup vs ownership | Docker name vs verified labels |
| desired vs observed state | spec/fingerprint vs runtime observation |
| connection history vs current connection | epoch and connected state |
| request lifetime vs provider lifetime | request lease vs OpenCode status |
| policy vs mechanism | reducer/manager vs timer/Docker/SSE |
| failure vs intentional stop | `failed` vs `paused` |

The result is not a full actor framework. It uses plain values at boundaries and explicit single-owner mutation inside an idiomatic imperative Go shell.

## Remaining Constraints

- All agent writes must pass through the Fern proxy. Direct backend-port writes bypass request admission.
- SSE has no replay cursor. Fern handles this safely by invalidating eligibility, but a missed idle event can leave compute running longer.
- A request canceled by every caller can still complete a service-owned wake. This may waste compute but does not authorize an unsafe pause.
- Permission-wait behavior still needs provider-backed empirical testing before it is described as a safe stop boundary.
- Apt repository packages are not pinned to a Debian snapshot, although the base and OpenCode binary are pinned.
- Host-local `flock` is intentionally not distributed coordination.

## Verification

Automated:

```bash
GOTOOLCHAIN=local go test ./...
GOTOOLCHAIN=local go test -race ./...
GOTOOLCHAIN=local go vet ./...
docker build -t fern/opencode:dev images/opencode
```

The suite covers strict config, environment failures and escaping, checked memory units, authenticated health/status/SSE, multiline SSE, epochs, disconnect invalidation, request invalidation, request leases, observer rollback, shared wake, spec and pause-intent identity, foreign container and volume refusal (including the inspect/create race), streaming flush, and cross-process locking.

Real Docker verification covered:

- foreign container refusal for `status` and `down`;
- Fern-owned container and volume labels;
- spec-drift refusal;
- authenticated create, health, SSE, status, stop, and wake;
- dynamic endpoint change with new watcher epoch;
- concurrent request wake coalescing;
- session persistence across stop/wake and container recreation;
- occupied proxy address failing before container creation;
- actual out-of-band memory drift refusal;
- external SIGTERM classified failed and refused wake;
- long-lived proxied SSE closed during shutdown;
- clean process shutdown with no leaked Fern test resources.
