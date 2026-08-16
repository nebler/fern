# Fern Two-Week Roadmap

Date range: 2026-08-15 through 2026-08-29

> **Document status (2026-08-16):** This body is the unrevised original plan.
> Its sequencing and present-tense feature claims are historical. Use
> [`todo/pre-phone`](./todo/pre-phone/README.md) for current delivery/evidence
> status and [docs/OPENCODE_V2.md](./docs/OPENCODE_V2.md) for the V2 work that
> landed after this plan. Setup/resume hooks and broad `doctor --json` remain
> deferred.

## Outcome

At the end of two weeks, fern should be a credible, reproducible demonstration
of a self-hosted OpenCode V1 workspace that:

- passes automated CI checks;
- installs and runs from documented commands;
- is reachable privately from a phone through Tailscale;
- attaches through the existing `fern attach` command;
- records measured wake and lifecycle behavior;
- runs bounded `.fern/setup` and `.fern/resume` hooks;
- reports machine-readable readiness through `fern doctor --json`;
- has a short recorded demonstration and an honest list of limitations.

This is a part-time plan. Each numbered day is one focused work session, not an
assumption of a full uninterrupted engineering day.

## Product Boundary

Fern remains one controller for one durable Docker workspace. OpenCode owns the
conversation, TUI, model calls, and session history. Fern owns lifecycle,
stable ingress, workspace preparation, and diagnostics.

The supported integration remains:

```text
fern attach
  -> opencode attach <fern proxy>
  -> wake if necessary
  -> opencode serve in Docker
```

Fern currently targets the pinned OpenCode V1 `1.18.16` image. OpenCode V2 is a
separate beta compatibility track, not part of this two-week implementation.

## Priorities

| Priority | Deliverable | Why now |
|---:|---|---|
| P0 | CI and reproducible build | Makes every later claim independently checkable |
| P0 | Tailnet deployment and phone dogfooding | Validates that fern improves on plain remote OpenCode |
| P1 | Setup and resume hooks | Makes wake produce a usable environment, not only a process |
| P1 | `fern doctor --json` | Makes failures legible to humans, scripts, and agents |
| P1 | Demo and measured results | Converts engineering work into product and interview evidence |
| P2 | Minimal install/release path | Reduces setup friction after the workflow is proven |

## Definition Of Done

The two-week slice is complete only when all of these are true:

- GitHub Actions runs formatting, unit tests, race tests, vet, build, and Docker
  image build from a clean checkout.
- The documented quick start works without undocumented local state.
- A phone on cellular can connect through tailnet-only access, attach, wake a
  paused workspace, complete a real OpenCode turn, disconnect, and observe the
  workspace stop after idle.
- Hook absence is a no-op; hook failure and timeout block readiness and produce
  useful diagnostics.
- `.fern/setup` runs on fresh container creation only.
- `.fern/resume` runs after a stopped-to-running transition only, not on every
  proxied request.
- `fern doctor --json` has a versioned schema and returns non-zero when required
  readiness checks fail.
- `go test ./...`, `go test -race ./...`, `go vet ./...`, and
  `go build ./cmd/fern` pass locally.
- README claims match observed behavior and do not claim VM scale-to-zero,
  zero infrastructure cost, mid-turn recovery, or production operation.

## Week One: Reproducibility And Real Use

### Day 1: Establish The Release Baseline

What:

- run the complete verification suite from a clean checkout;
- capture current binary, Go, Docker, image, and OpenCode versions;
- reconcile README, architecture, and implementation records;
- create one tracked checklist for failures discovered during the baseline.

How:

```bash
test -z "$(gofmt -l .)"
GOTOOLCHAIN=local go test ./...
GOTOOLCHAIN=local go test -race ./...
GOTOOLCHAIN=local go vet ./...
GOTOOLCHAIN=local go build ./cmd/fern
docker build -t fern/opencode:dev images/opencode
```

Exit gate: every failure is fixed or explicitly recorded as a blocker. No new
feature work starts from an unknown baseline.

### Day 2: Add CI

What:

- add `.github/workflows/ci.yml`;
- check formatting, tests, race tests, vet, binary build, and Docker image
  build;
- use pinned major versions of GitHub actions and Go 1.24;
- keep the workflow readable rather than introducing a build framework.

How:

- run fast Go checks in one job;
- run the Docker build in a separate Linux job so failures are attributable;
- use normal Go module caching;
- do not add release publishing, coverage services, or a large OS matrix yet.

Exit gate: a clean GitHub run is linked from the README or repository status.

### Day 3: Write The Tested Deployment Runbook

What:

- document one actual always-on host deployment;
- keep access private with Tailscale Serve or direct tailnet routing;
- document process supervision, Docker permissions, configuration location,
  secret injection, startup, shutdown, and log access.

How:

- test each command on the target host while writing it;
- bind fern only to the intended interface;
- keep Tailscale as external deployment infrastructure, not a fern feature;
- do not use public Funnel for the first deployment.

Exit gate: the host can reboot and restore fern through the documented process
without an interactive shell session remaining open.

### Day 4: Validate `fern attach`

What:

- test attachment locally and over the tailnet;
- test configured username/password propagation;
- verify that secrets do not appear in process arguments or normal logs;
- record any OpenCode V1 password-attach limitation rather than hiding it;
- distinguish the local proxy listen address from the externally reachable
  tailnet URL.

How:

- preserve the current wrapper over `opencode attach`;
- do not generate or overwrite `opencode.json`;
- add an explicit `-url` override only if dogfooding proves that Tailscale Serve
  cannot be represented safely by the current `-listen` override;
- add focused regression tests only for failures reproduced during dogfooding.

Exit gate: one documented command reaches the stable proxy and reconnects to a
persisted OpenCode session.

### Day 5: Phone Dogfood And Measurements

What:

- run the same real task through plain remote OpenCode and through fern;
- test from a phone on cellular, not only local Wi-Fi;
- capture wake latency, memory released while stopped, reconnect behavior,
  unexpected failures, and idle-stop timing.

How:

- use at least ten wake samples rather than quoting one favorable run;
- preserve raw timestamps and summarize median and range;
- include one disconnect/reconnect and one container recreation test;
- distinguish host cost from container resource reclamation.

Exit gate: there is evidence that fern removes a real operational burden. If it
does not, update positioning before adding features.

### Day 6: Specify The Hook Contract

What:

- define `.fern/setup` and `.fern/resume` behavior before coding;
- define timeout, cancellation, output, exit-code, and rollback semantics;
- add fixture scripts for success, failure, timeout, and missing-hook cases.

How:

- `.fern/setup` executes once for a newly created container;
- `.fern/resume` executes only after an actual stopped-to-running transition;
- both run as the unprivileged workspace user from `/home/user/workspace`;
- missing files are successful no-ops;
- executable files run directly so their shebang selects the interpreter;
- fern captures bounded stdout/stderr and the exact exit status;
- readiness is not published after failure or timeout.

Initial limits:

| Hook | Timeout | Intended work |
|---|---:|---|
| `.fern/setup` | 10 minutes | Dependency installation and generated setup |
| `.fern/resume` | 30 seconds | Fast idempotent repair and service restart |

Exit gate: the contract is documented and test cases describe all failure
semantics before the Docker interface changes.

### Day 7: Add A Narrow Docker Exec Capability

What:

- add one runtime operation for executing a declared hook in the verified fern
  container;
- preserve stdout and stderr separation;
- inspect the Docker exec result for the real process exit code.

How:

- verify fern ownership before exec;
- target the immutable container ID obtained from that inspection;
- set the working directory and user explicitly;
- use caller cancellation and a bounded timeout;
- cap retained output while allowing logs to stream to a controlled sink;
- keep hook execution separate from the minimal lifecycle interface unless it
  is required compositionally.

Exit gate: unit and Docker integration tests cover success, non-zero exit,
timeout, cancellation, missing script, and container replacement races.

## Week Two: Wake Ready And Publishable

### Day 8: Integrate `.fern/setup`

What:

- run setup during fresh container creation;
- wait for OpenCode health, execute setup, then publish the endpoint;
- roll back the newly started container if setup fails.

How:

- bind execution to the new container ID;
- replace the ambiguous transition boolean with a small typed result such as
  `none`, `created`, or `resumed`, so setup and resume cannot both run;
- never mark setup complete independently of the container generation;
- avoid a new database or cache in this slice;
- rerun setup after `fern down` creates a new container around the retained
  OpenCode volume.

Exit gate: setup runs exactly once per created container and no failed setup can
receive proxied traffic.

### Day 9: Integrate `.fern/resume`

What:

- run resume after a real stopped-to-running transition;
- do not run it when `EnsureRunning` observes an already-running workspace;
- roll back a transition if resume fails.

How:

- use the typed transition result returned by `EnsureRunning`;
- execute before watcher attachment and endpoint publication;
- on hook failure, use the existing bounded rollback path to stop the container
  and restore an intentional paused classification;
- make concurrent wake callers share the same hook execution through the
  existing manager-owned wake call.

Exit gate: five concurrent wake requests cause one container start and one
resume-hook execution.

### Day 10: Persist Bounded Hook Diagnostics

What:

- record hook name, container ID, start/end time, duration, exit code, timeout,
  and bounded output;
- make the latest result available without reading Docker internals.

How:

- store diagnostics under fern's existing host state directory;
- write atomically and bind records to workspace plus container identity;
- treat malformed or stale records as unknown rather than successful;
- never persist expanded secret values in metadata.

Exit gate: a failed wake explains which hook failed and where its bounded log
can be read.

### Day 11: Implement `fern doctor --json`

What:

- add machine-readable checks for configuration, repository, Docker access,
  image availability, runtime ownership, desired-spec drift, OpenCode health,
  watcher state, and latest hook results;
- provide concise text output when `--json` is absent.

How:

- define a versioned top-level schema;
- reuse `runtime.Observation` and existing config validation;
- report each check as `PASS`, `FAIL`, or `UNKNOWN` with a stable code and
  remediation message;
- keep doctor observational: it must not create, resume, stop, or repair the
  workspace.

Example shape:

```json
{
  "version": 1,
  "ready": false,
  "checks": [
    {
      "code": "runtime.health",
      "result": "UNKNOWN",
      "message": "workspace is paused",
      "remediation": "run fern resume"
    }
  ]
}
```

Exit gate: scripts can distinguish ready, failed, and unknown states without
parsing logs or human prose.

### Day 12: End-To-End Failure Verification

What:

- run the complete unit, race, vet, build, and Docker suites;
- add integration coverage for hook rollback and diagnostics;
- repeat lifecycle tests through the stable proxy.

Failure cases:

- setup exits non-zero;
- setup exceeds its timeout;
- resume exits non-zero;
- fern shuts down while a hook runs;
- container identity changes before exec;
- watcher cannot attach after a successful hook;
- a new request arrives while wake is already in progress.

Exit gate: all failures leave the workspace in a classified state, do not leak
hook processes, and do not publish an unobserved endpoint.

### Day 13: Installation And Demo

What:

- document the smallest supported installation path;
- record a 60-90 second demonstration;
- include measured results and explicit limitations.

Demo sequence:

```text
phone opens the tailnet-only OpenCode web surface
the ordinary request wakes the paused workspace
fern reports the transition and persisted session
terminal fern attach reaches the same stable workspace
OpenCode resumes a persisted session
setup/resume diagnostics are visible
a real task completes
the client disconnects
fern stops the idle workspace
```

Exit gate: another engineer can understand the value and reproduce the basic
workflow without reading implementation code.

### Day 14: Buffer, Cut, And Stop

What:

- fix only blockers found by CI, deployment, or the demo;
- remove stale claims and duplicate planning notes;
- record deferred work rather than starting it;
- tag a release only if all definition-of-done gates pass.

Exit gate: the worktree is clean, verification results are recorded, and the
next slice is selected from evidence rather than enthusiasm.

## Deferred Until After 2026-08-29

- wake-aware application preview routing;
- artifact browsing;
- `fern prove` and state-bound verification receipts;
- signed GitHub webhooks;
- OpenCode V2 migration;
- a public workspace-provider adaptor;
- multiple workspaces;
- Redis, PostgreSQL, OIDC issuance, or an LLM credential proxy;
- Kubernetes, BYOC, managed cloud, billing, SSO, or multi-tenancy;
- MCP lifecycle tools, a desktop GUI, or a Puck-style meta-agent.

## Decision Gates

After the two weeks, choose exactly one next slice:

| Evidence | Next decision |
|---|---|
| Phone dogfooding shows preview friction | Build one wake-aware private HTTP service |
| Users distrust completion claims | Build manual `fern prove` with freshness |
| Setup/resume dominates wake time | Investigate a content-addressed setup cache |
| OpenCode V2 publishes a supported workspace API | Evaluate an upstream adaptor against the public contract |
| OpenCode ships equivalent self-hosted lifecycle behavior | Narrow fern to diagnostics or stop investing |
| No repeated personal or external use | Freeze features and keep fern as a finished portfolio project |

Do not begin a hosted control plane because the demo is interesting. Reconsider
that only after repeated external usage and an explicit willingness to operate
a company-sized service.
