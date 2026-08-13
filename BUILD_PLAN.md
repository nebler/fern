# Fern Build Plan (Historical)

> This is the original planning document and contains pre-refactor sketches. It is retained as project history, not as the current API or architecture. Use [CODEBASE_GUIDE.md](./CODEBASE_GUIDE.md), [ARCHITECTURE.md](./ARCHITECTURE.md), and [IMPLEMENTATION.md](./IMPLEMENTATION.md) for the implemented foundation.

**Self-hosted agent workspaces that sleep when idle and wake on request. Kubernetes-native, single-tenant, your cluster.**

---

## 0. Before you write any code

### 0.1 The one sentence

Write this on a sticky note on your monitor:

> I close my laptop, the workspace sleeps, I send a message from my phone, it wakes, and the session is intact.

Every decision gets measured against that sentence. If a task doesn't move that sentence closer to true, it's not week-1 work.

### 0.2 The failure mode you are defending against

You have designed Hibernal, Terus, Reflow, and treeko without shipping them. The mechanism is always the same: the design is more interesting than the build, and the design has no failure condition, so it expands forever.

Three hard rules for this project:

1. **Week 2 Friday is a gate.** If Docker pause/wake doesn't work end-to-end by then, you stop adding features and fix that. No Kubernetes, no proxy polish, nothing.
2. **No second CRD.** The moment you're designing a second custom resource, you've drifted into architecture astronomy.
3. **Timebox reading.** Day 1 has a reading task with a three-sentence deliverable. If you're six hours in with a design document, you've relapsed. Write the three sentences and move on.

### 0.3 What you are NOT building

Put this list in the README on day one. It's evidence of deliberate scoping, which is a senior signal.

- OIDC workload identity issuer
- Multiplayer / shared sessions
- Webhook event queue
- Multi-tenant authorization
- A custom UI (opencode ships one)
- Multi-cloud provider support
- Firecracker orchestration from scratch
- Billing / metering beyond a display counter

### 0.4 Prerequisites to install before day 1

```bash
# Go 1.23+
go version

# Docker
docker version

# kind (Kubernetes in Docker) — needed week 3
go install sigs.k8s.io/kind@latest

# kubectl
# kubebuilder — needed week 3
curl -L -o kubebuilder "https://go.dev/dl/..."  # see kubebuilder book

# opencode itself
curl -fsSL https://opencode.ai/install | bash
```

Have an `ANTHROPIC_API_KEY` in your environment. Have a throwaway git repo to point the agent at — something with a real `package.json` or `go.mod`, not an empty directory, because dependency install time is part of what you're measuring.

---

## Phase 1 — Docker (Weeks 1–2)

**Goal:** prove the lifecycle works, on your laptop, with the fastest possible loop. No cloud. No Kubernetes. No YAML.

**Why Docker first:** the Docker daemon is the simplest possible orchestrator. It lets you find out whether idle-detect → pause → wake actually works before you take on a second unfamiliar thing. Roughly 60% of this code survives into Phase 2 (the SSE reader, the proxy, the activity tracking). The lifecycle orchestration gets rewritten as a reconcile loop, and that rewrite is how you learn Kubernetes properly.

---

### Day 1 (Mon) — The gate question

**Deliverable: three sentences in `NOTES.md`. Nothing else.**

The question: **when opencode's coordinator drains a session, does the runner reconstruct everything it needs from SQLite, or does it carry in-memory state between steps?**

This matters because it determines whether a hard stop mid-turn loses only the current turn or corrupts the session.

Read, in this order:

```
packages/core/src/session/run-coordinator.ts    # the actor pattern, ~150 lines
packages/core/src/session/execution.ts          # the interface you'll eventually implement
packages/core/src/session/execution/local.ts    # the only current implementation
packages/core/src/session/runner/index.ts       # what drain actually does
packages/core/src/session/history.ts            # how messages are read back
packages/core/src/session/store.ts
```

Things to specifically look for and note:

- `SessionMessageTable` has a `seq` column. Is it monotonic per session? Is it assigned at write time or read time?
- Does `drain(sessionID, force)` load history from the DB at the start of every call, or is there a cached in-memory representation?
- What does `wake` do differently from `run`? (The doc comment says "registers one coalesced follow-up" — what does coalescing mean concretely?)
- Where does a partially-streamed assistant message live before it's complete? `updatePart` publishes an event — who persists it?

**Clone it locally if you haven't:**

```bash
git clone --depth 1 https://github.com/anomalyco/opencode.git
cd opencode
```

**Second task, 30 minutes:** empirically verify the persistence claim.

```bash
# terminal 1
cd ~/some-test-repo
opencode serve --port 4096 --hostname 127.0.0.1

# terminal 2 — start a turn, then hard-kill mid-generation
# send a prompt via the API, wait 3 seconds, then:
pkill -9 -f "opencode serve"

# restart, list sessions, see what survived
opencode serve --port 4096 --hostname 127.0.0.1
```

Record in `NOTES.md`: did the partial turn survive? Did the session survive? Was the DB corrupted?

**Known facts to save you time:**

- Session state is SQLite (`packages/core/src/session/sql.ts`), not the JSON files in `storage/storage.ts` — that's legacy with migration code.
- Pragmas are `journal_mode = WAL` and `synchronous = NORMAL` (`packages/core/src/database/database.ts`).
- **What that combination means:** committed transactions survive process death (`kill -9`). They may NOT survive machine-level abrupt power loss, because WAL isn't fsynced on every commit. A hard VM/container stop is power-loss-shaped. If you need airtight durability before a pause, issue `PRAGMA synchronous = FULL` or do a clean shutdown and wait for it.

**Stop when you have three sentences.**

---

### Day 2 (Tue) — Dumbest possible harness

**Deliverable: `fern up` prints a URL, and you have a conversation through it.**

#### 2.1 Repo skeleton

```
fern/
├── go.mod
├── NOTES.md
├── README.md
├── cmd/
│   └── fern/
│       └── main.go
└── internal/
    ├── runtime/          # the start/stop interface + docker impl
    │   ├── runtime.go
    │   └── docker.go
    ├── watch/            # SSE reader, idle detection      (Day 3)
    ├── proxy/            # wake-on-request proxy           (Day 5)
    └── registry/         # session -> workspace state      (Week 2)
```

Note the name is `runtime`, not `provider`. You are abstracting "the thing that starts and stops workloads," and in Phase 2 that becomes Kubernetes. Naming it `provider` biases you toward thinking of it as a cloud.

#### 2.2 The interface — write this first, and keep it small

```go
package runtime

import "context"

type State string

const (
    StateAbsent       State = "absent"
    StateProvisioning State = "provisioning"
    StateRunning      State = "running"
    StatePaused       State = "paused"
)

type Spec struct {
    Name     string
    Image    string
    RepoURL  string
    MemoryMB int
    CPUs     float64
    Env      map[string]string
}

// Endpoint is returned by Resume, never cached by the caller.
// Rationale: on some backends the address changes across a pause
// (GCE releases ephemeral IPs on suspend; a rescheduled pod gets a new IP).
// Encoding that in the type prevents a whole bug class.
type Endpoint struct {
    Host string
    Port int
}

type Runtime interface {
    Create(ctx context.Context, spec Spec) (Endpoint, error)
    Pause(ctx context.Context, name string) error
    Resume(ctx context.Context, name string) (Endpoint, error)
    Destroy(ctx context.Context, name string) error
    Status(ctx context.Context, name string) (State, error)
}
```

Five methods. Resist adding more. If you find yourself wanting `Exec` or `CopyFile`, you're building a cloud SDK instead of a lifecycle manager.

#### 2.3 The image

`images/opencode/Dockerfile`:

```dockerfile
FROM debian:12-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates curl git openssh-client ripgrep unzip jq \
    && rm -rf /var/lib/apt/lists/*

# Node via nodesource or a tarball — pick one, pin it
RUN curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
    && apt-get install -y nodejs && rm -rf /var/lib/apt/lists/*

RUN useradd -m -s /bin/bash user
USER user
WORKDIR /home/user

RUN curl -fsSL https://opencode.ai/install | bash
ENV PATH="/home/user/.opencode/bin:${PATH}"

RUN mkdir -p /home/user/workspace
WORKDIR /home/user/workspace

EXPOSE 4096
CMD ["opencode", "serve", "--hostname", "0.0.0.0", "--port", "4096"]
```

**Why Debian 12 and not Alpine:** glibc, not musl — prebuilt binaries (Node native modules, Playwright/Chromium, anything shipping a `.node` prebuild) target glibc and a meaningful fraction break on Alpine. Also `apt-get install <anything>` has enormous coverage, which matters because the agent will reach for arbitrary tools. Also: frontier models have seen far more Debian/Ubuntu shell than anything exotic, so you're not fighting the agent's defaults on every command. This is the same reasoning Amp used.

**Memory:** codecloud found opencode using 3–4 GB on larger repos, having started at the 512MB default and had processes randomly die. Set 8GB from the start. Do not debug OOM in week 1.

#### 2.4 Docker implementation

```go
package runtime

// docker.go — sketch, not complete

type dockerRuntime struct {
    cli *client.Client
}

func (d *dockerRuntime) Create(ctx context.Context, spec Spec) (Endpoint, error) {
    // 1. container config: image, env (ANTHROPIC_API_KEY), exposed port 4096
    // 2. host config: memory limit, published port (let Docker pick host port),
    //    a named volume mounted at /home/user for persistence across destroy
    // 3. ContainerCreate + ContainerStart
    // 4. inspect to read back the mapped host port
    // 5. waitHealthy(ctx, endpoint) — poll until it answers
    return ep, nil
}

func (d *dockerRuntime) Pause(ctx context.Context, name string) error {
    return d.cli.ContainerPause(ctx, name)  // freezer cgroup, memory preserved
}

func (d *dockerRuntime) Resume(ctx context.Context, name string) (Endpoint, error) {
    if err := d.cli.ContainerUnpause(ctx, name); err != nil { return Endpoint{}, err }
    // re-inspect for the port — don't assume it's unchanged
    // waitHealthy before returning
}
```

**Health check helper — you'll use this constantly:**

```go
func waitHealthy(ctx context.Context, ep Endpoint, timeout time.Duration) error {
    ctx, cancel := context.WithTimeout(ctx, timeout)
    defer cancel()
    url := fmt.Sprintf("http://%s:%d/global/health", ep.Host, ep.Port)
    ticker := time.NewTicker(200 * time.Millisecond)
    defer ticker.Stop()
    for {
        select {
        case <-ctx.Done():
            return fmt.Errorf("health check timed out: %w", ctx.Err())
        case <-ticker.C:
            req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
            resp, err := http.DefaultClient.Do(req)
            if err != nil { continue }
            resp.Body.Close()
            if resp.StatusCode == 200 { return nil }
        }
    }
}
```

Verify the health path against the SDK docs — `/global/health` is what E2B's opencode example polls, but confirm it against your installed version.

#### 2.5 Structured logging — do this on day 2, not later

You will be debugging timing. Every state transition needs a timestamp.

```go
logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
    Level: slog.LevelDebug,
}))
logger.Info("state transition",
    "workspace", name,
    "from", old, "to", new,
    "elapsed_ms", time.Since(start).Milliseconds())
```

#### 2.6 Definition of done for Day 2

```bash
$ go run ./cmd/fern up
workspace: demo
url:       http://127.0.0.1:49213
ready in:  8.2s
```

Point your local opencode client at that URL, have a real conversation, verify tool calls execute inside the container.

**Do not build a CLI framework.** Hardcode the workspace name. Hardcode the image. Config comes when you have three things worth configuring.

---

### Day 3 (Wed) — See the boundary

**Deliverable: a stream of timestamped busy/idle transitions on stderr, and an hour of watching it.**

You are not building the idle logic today. You are finding out whether "idle" means what you think it means.

#### 3.1 The event stream

opencode exposes SSE at the event endpoint. Events you care about:

```
session.status   { sessionID, status: { type: "idle" | "busy" | "retry", ... } }
session.idle     { sessionID }                    # deprecated, but still emitted
```

From `packages/opencode/src/session/status.ts`: `set()` publishes `Status` always, and additionally publishes `Idle` and deletes the map entry when the status is idle. So idle is both a status value and a separate event.

```go
package watch

type Event struct {
    ID         string          `json:"id"`
    Type       string          `json:"type"`
    Properties json.RawMessage `json:"properties"`
}

func Stream(ctx context.Context, base string, out chan<- Event) error {
    req, _ := http.NewRequestWithContext(ctx, "GET", base+"/event", nil)
    req.Header.Set("Accept", "text/event-stream")
    resp, err := http.DefaultClient.Do(req)
    if err != nil { return err }
    defer resp.Body.Close()

    sc := bufio.NewScanner(resp.Body)
    sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)  // events can be large
    for sc.Scan() {
        line := sc.Text()
        if !strings.HasPrefix(line, "data: ") { continue }
        var ev Event
        if err := json.Unmarshal([]byte(line[6:]), &ev); err != nil { continue }
        select {
        case out <- ev:
        case <-ctx.Done():
            return ctx.Err()
        }
    }
    return sc.Err()
}
```

**Add a `fern debug events` subcommand** that just dumps this. You will use it constantly for the rest of the project.

#### 3.2 What to actually watch for

Spend an hour using the agent while watching the stream. Answer these in `NOTES.md`:

1. **Does idle fire between steps or only at turn end?** A turn is user-message → many model calls and tool executions → final assistant message with no tool calls. A step is one model call plus its tools. You want turn boundaries, not step boundaries.
2. **What happens on a permission prompt?** The agent stops and waits for a human. Does it report idle? This is critical: it's arguably a *better* pause point (a human might take 20 minutes) but you must not destroy the pending question when you pause. Check what `permission.ts` emits.
3. **What does `retry` mean and how long does it last?** The status union includes `retry` with an `attempt` count and a `next` timestamp. A session in retry is not idle.
4. **Does the SSE connection stay open indefinitely, or does it drop?** If it drops, you need reconnect logic with backoff.
5. **Does `session.idle` fire on session creation, before any work?**

#### 3.3 The reconnect problem — note it now, solve it in Phase 2

codecloud hit exactly this: they kept a long-lived connection pulling events, and there was a non-zero chance of missing events during reconnects — including `session.idle` for run completions. Worse, if the consumer crashed, they could miss the rest of a run entirely.

Their fix was to invert the flow: a relay inside the sandbox subscribes to the local event stream and pushes to a webhook, so the backend only handles short requests.

For Phase 1, a single long-lived connection with reconnect-and-backoff is fine — you're on localhost. **Write the inversion down as a known Phase 2 decision.** It's a good thing to have thought about before an interviewer asks how you'd make it reliable.

---

### Day 4 (Thu) — Pause and resume

**Deliverable: idle for 60s → paused. Manual resume works. Session intact.**

#### 4.1 The two pause modes, empirically

Run both experiments and record results. This is your Mode A vs Mode B data, and it's free on your laptop.

**Mode A — `docker stop` (disk only).** Container halts, RAM discarded. On restart, fresh `opencode serve` reads sessions from SQLite.

**Mode B — `docker pause` (freezer cgroup, memory preserved).** Processes frozen, RAM intact. On unpause, `opencode serve` has no idea time passed.

For each, test three scenarios:

| Scenario | Mode A (stop) | Mode B (pause) |
|---|---|---|
| Pause between turns, resume | ? | ? |
| Pause mid-generation, resume | ? | ? |
| Pause mid-tool-execution, resume | ? | ? |

**What you should expect, and why:**

Mode B preserves computation exactly and connections not at all. The TCP socket to the model provider is faithfully restored — same sequence numbers, ESTABLISHED — but the peer tore down its half while you were frozen. Its FIN arrived and landed nowhere. On resume you send into a connection that doesn't exist and get an RST. The clock also jumped: every pending timer fires at once, every token with an `exp` is expired.

This is exactly why Amp needs `.agents/resume`, and why theirs is a single script call to re-establish networking connections.

**The conclusion you should reach and write down:** *neither mode saves a mid-turn request.* Mode B keeps the accumulated context (so the app could retry), Mode A loses the turn. Since opencode won't retry a socket death it doesn't know about, **pause only at turn boundaries** and the difference stops mattering. That decision makes you backend-agnostic, which is the whole reason Phase 2 works.

Docker pause is short-lived (seconds to minutes) — you're not testing multi-hour freezes here. Note that the mid-turn failure gets *worse* with longer pauses, not better.

#### 4.2 The idle supervisor

```go
package watch

type Supervisor struct {
    idleAfter time.Duration
    onPause   func(ctx context.Context) error
    log       *slog.Logger
}

func (s *Supervisor) Run(ctx context.Context, events <-chan Event) error {
    timer := time.NewTimer(s.idleAfter)
    timer.Stop()  // don't arm until we've seen idle
    defer timer.Stop()

    busy := false
    for {
        select {
        case <-ctx.Done():
            return ctx.Err()

        case ev, ok := <-events:
            if !ok { return nil }
            status, isStatus := parseStatus(ev)
            if !isStatus { continue }

            switch status {
            case "busy", "retry":
                if !busy { s.log.Info("busy") }
                busy = true
                if !timer.Stop() {
                    select { case <-timer.C: default: }   // drain
                }
            case "idle":
                busy = false
                s.log.Info("idle, arming pause timer", "after", s.idleAfter)
                timer.Reset(s.idleAfter)
            }

        case <-timer.C:
            if busy { continue }   // belt and braces
            s.log.Info("idle threshold reached, pausing")
            if err := s.onPause(ctx); err != nil {
                s.log.Error("pause failed", "err", err)
            }
            return nil
        }
    }
}
```

**Go details that matter here:** `timer.Stop()` returning false means the timer already fired and the value is sitting in the channel — you must drain it before `Reset`, or the next `Reset` misbehaves. This is a classic `time.Timer` footgun and worth understanding rather than copying.

Start with 60 seconds for development. Real value is 5–15 minutes. Amp uses 5 minutes (dropped from 15).

#### 4.3 Definition of done

```
$ go run ./cmd/fern up
url: http://127.0.0.1:49213
[you have a conversation, then stop typing]
[60s later]
INFO idle threshold reached, pausing workspace=demo
INFO paused workspace=demo elapsed_ms=340

$ go run ./cmd/fern resume
INFO resumed workspace=demo endpoint=127.0.0.1:49213 elapsed_ms=120
[reconnect client, full history present]
```

---

### Day 5 (Fri) — Wake on request

**Deliverable: a request to a paused workspace wakes it and succeeds. Client sees one slow request.**

This is the thesis. Everything before was setup.

#### 5.1 The proxy

```go
package proxy

type Waker interface {
    // EnsureRunning blocks until the workspace can serve traffic,
    // returning the endpoint. Must be safe for concurrent calls —
    // ten simultaneous requests must produce one wake, not ten.
    EnsureRunning(ctx context.Context, name string) (runtime.Endpoint, error)
}

func New(w Waker, name string, log *slog.Logger) http.Handler {
    rp := &httputil.ReverseProxy{
        Rewrite: func(pr *httputil.ProxyRequest) {
            // target is set per-request in the handler below via context
        },
        FlushInterval: -1,  // CRITICAL: see 5.2
        ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
            log.Error("proxy error", "err", err, "path", r.URL.Path)
            http.Error(w, "upstream unavailable", http.StatusBadGateway)
        },
    }
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        start := time.Now()
        ep, err := w2.EnsureRunning(r.Context(), name)
        if err != nil {
            http.Error(w, "failed to wake workspace", http.StatusServiceUnavailable)
            return
        }
        log.Debug("routing", "wake_ms", time.Since(start).Milliseconds())
        // set target on request context, then rp.ServeHTTP(w, r)
    })
}
```

#### 5.2 `FlushInterval: -1` — set this on day 5 and never touch it

`httputil.ReverseProxy` buffers response bodies by default. **This silently breaks SSE.** The agent works fine; the client shows nothing. You would spend an afternoon on this in week 6 if you didn't set it now.

`-1` means flush immediately after every write. There is no downside for this use case.

**Verify it today**, while you have only one moving part: with the proxy in front, run `fern debug events` pointed at the *proxy* URL rather than the backend. Events should appear in real time. If they arrive in a burst when the connection closes, buffering is on.

Same issue applies to nginx (`proxy_buffering off`), Traefik, and any ingress you add in Phase 2. Note it in `NOTES.md` as a recurring hazard.

#### 5.3 Single-flight wake

Ten concurrent requests to a paused workspace must produce one wake:

```go
import "golang.org/x/sync/singleflight"

type waker struct {
    rt  runtime.Runtime
    sf  singleflight.Group
}

func (w *waker) EnsureRunning(ctx context.Context, name string) (runtime.Endpoint, error) {
    v, err, _ := w.sf.Do(name, func() (interface{}, error) {
        st, err := w.rt.Status(ctx, name)
        if err != nil { return nil, err }
        switch st {
        case runtime.StateRunning:
            return w.rt.Resume(ctx, name)  // cheap: inspect + health
        case runtime.StatePaused:
            return w.rt.Resume(ctx, name)
        case runtime.StateAbsent:
            return w.rt.Create(ctx, spec)
        }
        return nil, fmt.Errorf("unexpected state %q", st)
    })
    if err != nil { return runtime.Endpoint{}, err }
    return v.(runtime.Endpoint), nil
}
```

**Go learning here:** `singleflight` is exactly the right primitive and it's worth reading its source — it's short and it's a clean example of the mutex-plus-map-of-waiters pattern.

#### 5.4 Definition of done

```
[workspace is paused]
$ curl http://localhost:8080/global/health
INFO waking workspace=demo
INFO resumed elapsed_ms=180
{"status":"ok"}     # one slow request, then normal
```

---

### Weekend — Dogfood

Use it for real work. Not a demo. Actual work.

Keep a running list of annoyances in `NOTES.md`. Do not fix them yet. The list is more valuable than the fixes right now, because it tells you what's actually broken versus what you imagined would be.

---

### Week 2 — Make it real

The goal of week 2 is that you'd be mildly annoyed to lose it.

**Day 6 (Mon) — Registry and single-writer.**

The one distributed-systems property that matters: **for a given workspace, only one process is managing it.** The failure this prevents: two `fern` processes both think workspace `demo` is asleep, both wake it, and now two agents commit to the same branch.

Phase 1 can be a file with an flock, or a SQLite table with a unique constraint. Design the interface as if it were Postgres with a lease:

```go
type Registry interface {
    Acquire(ctx context.Context, workspace string, ttl time.Duration) (Lease, error)
    Get(ctx context.Context, workspace string) (Record, error)
    Put(ctx context.Context, r Record) error
}

type Record struct {
    Workspace    string
    State        runtime.State
    Endpoint     runtime.Endpoint
    LastActivity time.Time
    HeldBy       string   // process identity
    LeaseExpiry  time.Time
}
```

**Clock warning:** lease TTLs interact badly with machines whose clocks jump on resume. Use a monotonic source for durations locally, and treat any lease you find with a future-dated expiry as suspect. This is the DDIA chapter 8 material and it's worth a paragraph in the README.

**Day 7 (Tue) — Config and CLI.** Now you have three things worth configuring:

```yaml
# fern.yaml
workspace:
  image: fern/opencode:latest
  memory: 8Gi
  cpus: 2
idle:
  after: 10m
proxy:
  listen: :8080
```

Commands: `up`, `down`, `status`, `logs`, `debug events`. Nothing else.

**Day 8 (Wed) — Warm start cache.**

This is the highest value-to-effort item in the whole project and it's where a demo number comes from.

The idea, stolen from Amp: after running setup, snapshot the result and reuse it. Amp snapshots the sandbox after `.agents/setup` and reuses that snapshot for up to 24 hours for new orbs. Their own setup script is 428 lines of bash — Postgres tuned for ephemerality (`fsync = off`, `synchronous_commit = off`), seeded test users, `pnpm install --frozen-lockfile`. Running that on every creation would be unusable; snapshotting it once a day is the difference.

In Docker: run the setup, `docker commit` to a cache-tagged image, store a manifest with a timestamp and the hash of the setup script + lockfiles. On create, if a cache entry exists and is fresh, use that image.

Split the hooks like Amp does:

- `.fern/setup` — runs once when preparing workspace state, gets baked into the snapshot
- `.fern/resume` — runs on every wake, repairs what decayed (expired tokens, dead connections)

Amp blocks at most 10 seconds on resume before letting the agent continue in the background. Copy that: a resume hook that hangs must not hang the workspace.

**Day 9 (Thu) — Tests.**

The JD explicitly asks for automated testing (unit, integration). This is cheap signal.

Write a `fake` runtime implementing the interface with configurable delays and failures. Then test:

- Supervisor: busy → idle → timer fires → pause called once
- Supervisor: busy → idle → busy before timeout → no pause
- Waker: 10 concurrent EnsureRunning → exactly 1 Create call
- Waker: Create fails → error propagates, no partial state
- Registry: two processes race for a lease → exactly one wins
- Proxy: SSE passthrough is unbuffered (write, read, assert timing)

Table-driven, `t.Parallel()` where safe. This is also where you learn Go's testing idioms properly.

**Day 10 (Fri) — GATE. Write the README.**

The gate: does the one sentence work? Close laptop, sleep, wake, session intact.

If yes, write the README now while it's fresh:

- The one-sentence pitch
- The three numbers (cold, warm, wake)
- The explicit non-goals list from §0.3
- The architecture decision on turn-boundary pausing, with its cost stated
- The Amp/Palana comparison

**If no: stop. Week 3 is a fix week, not a Kubernetes week.**

---

## Phase 2 — Kubernetes (Weeks 3–5)

**Why:** it's your largest CV gap, it's Grab's substrate (Palana is intentionally Kubernetes-native; agents are custom resources and an operator reconciles namespaces, RBAC, storage, services, ingress, network policies), and — the part that surprises people — **it makes pause/wake simpler.** `replicas: 0` with a surviving PVC has fewer failure modes than VM suspend.

### Week 3 — Controller basics

**Day 11 (Mon) — Reading + scaffold.** First four chapters of the kubebuilder book (~2 hours). Then:

```bash
kubebuilder init --domain fern.dev --repo github.com/you/fern
kubebuilder create api --group fern --version v1alpha1 --kind Workspace
kind create cluster --name fern
```

Get a controller that does nothing but log reconciles. Understand: **level-triggered, not edge-triggered.** You are not handling events; you are repeatedly asked "make reality match spec" and must be idempotent.

**Note the connection to everything you've read:** a reconcile loop is keyed by resource, serialized per key, and idempotent. That's the actor model in Kubernetes clothing — same property as opencode's run-coordinator ("serializes execution for each key while allowing different keys to run concurrently") and Amp's thread actors. You've been studying this pattern for weeks; now you implement it.

**Day 12 (Tue) — The CRD.**

```go
type WorkspaceSpec struct {
    RepoURL   string                      `json:"repoURL"`
    Image     string                      `json:"image"`
    Resources corev1.ResourceRequirements `json:"resources,omitempty"`
    IdleAfter metav1.Duration             `json:"idleAfter,omitempty"`
    Storage   resource.Quantity           `json:"storage,omitempty"`
}

type WorkspacePhase string
const (
    PhaseProvisioning WorkspacePhase = "Provisioning"
    PhaseRunning      WorkspacePhase = "Running"
    PhaseIdle         WorkspacePhase = "Idle"
    PhaseStopped      WorkspacePhase = "Stopped"
)

type WorkspaceStatus struct {
    Phase        WorkspacePhase     `json:"phase,omitempty"`
    Endpoint     string             `json:"endpoint,omitempty"`
    LastActivity *metav1.Time       `json:"lastActivity,omitempty"`
    Conditions   []metav1.Condition `json:"conditions,omitempty"`
}
```

**One CRD. Do not add a second.**

**Day 13 (Wed) — Reconcile to real objects.** PVC, Deployment, Service. Set owner references so deleting the Workspace garbage-collects the rest. Verify: `kubectl apply` a Workspace, get a running opencode you can port-forward to.

**Day 14 (Thu) — Activity tracking.** Port the SSE reader. Write `lastActivity` into status. **Note:** don't write status on every event — you'll hammer the API server. Debounce to at most once per 30s, or only on busy→idle transitions.

**Day 15 (Fri) — Scale to zero.** `replicas: 0` when idle threshold passes. PVC survives. Verify session history intact after scale-up.

This is Palana's reaper: after a configurable idle threshold, warn and stop the workload while preserving state. Their philosophy, verbatim: *stop the compute, keep the state, and make resumption easy.*

### Week 4 — Wake, ingress, credentials

**Day 16–17 — Wake-on-request in-cluster.** Port the proxy. It needs cluster access to patch replicas, then wait for pod ready. **This is your differentiator** — Palana's published architecture stops workloads and a human restarts them via `pcli run`. There's no wake-on-request. Say so in the README, carefully and without overclaiming.

Cold start is seconds and a browser request just hangs. Decide deliberately: holding page that polls, or accept the hang with a progress indicator. This is the difference between "magic" and "broken" in the demo.

**Day 18 — Ingress.** Traefik or nginx. **`proxy_buffering off` / equivalent — the day-5 hazard recurs here.**

**Day 19–20 — The credential proxy.** The one Palana feature worth copying.

Palana splits secrets: agent-readable ones live under the agent's Vault path; **proxy-only secrets** are read by the proxy layer, not the agent. The agent sees a placeholder like `TOKEN_GRABGPT_API_KEY`, and the proxy swaps it for the real credential on outbound requests. Their LiteLLM wrapper derives agent identity from Kubernetes context rather than trusting client-provided headers.

Your version: the pod gets `ANTHROPIC_API_KEY=placeholder` and `ANTHROPIC_BASE_URL=http://fern-llm-proxy`. Your proxy injects the real key and forwards. The agent never holds it.

This is cheap (a day) and hits three things: the LLM-infrastructure gap on your CV, direct relevance to the GrabGPT Gateway team you're targeting, and a security argument you can defend on its merits.

Add per-workspace request logging while you're there — you get attributable LLM traffic for free, which is one of the three properties Palana names.

### Week 5 — Remote access + demo

**Day 21–22 — Auth. Not optional.** You are putting a machine with a shell and API access on a network. Palana uses OAuth2-Proxy through Traefik forward auth. For your scope: **Tailscale** — no public exposure, works from your phone, ~20 minutes. Cloudflare Tunnel + Access if you want a real hostname on video.

Do not hand-roll a bearer token.

**Day 23 — Phone test.** opencode ships a web UI (`packages/web`, `packages/session-ui`). Expose it through your proxy. Verify the full path from a phone on cellular data.

**Day 24–25 — Record the demo.** Three acts, under three minutes:

1. **The work outlives the client.** Long task, close laptop mid-turn, come back done.
2. **Idle costs nothing.** `kubectl get pods` shows nothing, cost meter flat.
3. **The wake.** Phone request → proxy holds → scale up → history renders → type a follow-up.

Two counters on screen throughout: elapsed cost, time-to-ready. Those numbers are the thesis.

---

## Phase 3 — Optional (Week 6+)

Only if weeks 1–5 shipped. In priority order:

1. **`RuntimeClass` for Kata/Firecracker.** One line in the pod spec, plus a writeup of the tradeoff: containers share the host kernel so a kernel exploit crosses the boundary; microVMs get their own kernel and the boundary is hardware virtualization. Grab chose namespaces; E2B chose Firecracker; here's why each is right for its threat model. **This is the highest-value item — it converts weeks of reading into a demonstrated design decision.**
2. **Portals.** Expose arbitrary in-pod ports through the same wake proxy. Nearly free given the proxy exists.
3. **The writeup.** See below.
4. **Contribute upstream.** opencode's `SessionExecution` local implementation is commented "Future remote placement belongs here," and dax has said publicly they're adding sandbox support with a bring-your-own option. Get into that discussion early.

---

## The writeup

Do this regardless of how far the code gets. It may be worth more than the code.

**Post 1 — "How Amp Orbs actually work."** The reconstruction: E2B/Firecracker underneath, the snapshot cache, the durable server-side loop, bought vs built. Nobody has written this down.

**Post 2 — "Where does the agent loop live?"** The one architectural question that determines everything: Amp keeps it server-side so an orb is disposable at any instant; opencode keeps it in-process so pausing the machine pauses the loop; here's what each buys and costs.

**Post 3 — "Building it: what I got wrong."** The specifics. WAL + `synchronous = NORMAL` surviving `kill -9` but not a hard stop. `FlushInterval: -1`. The 4GB memory finding. Whatever actually bit you.

Post 3 is the one that proves you built rather than read.

---

## Interview answers to have ready

**"Why containers not microVMs?"**
> Single-tenant — my code, my key, my cluster. The multi-tenant threat model that justifies Firecracker for E2B doesn't apply. Grab reached the same conclusion for Palana with far more at stake. And I made it a `RuntimeClass` field, so it's a config change if the threat model shifts.

**"Why turn-boundary pausing?"**
> Amp can pause mid-turn because the loop is server-side. opencode's is in-process, so I pause only between turns. The cost is no mid-generation resumption. But a memory snapshot wouldn't have saved that anyway — the socket to the provider is dead regardless, which is why Amp's `.agents/resume` re-establishes network connections. So the honest tradeoff is smaller than it looks, and turn-boundary pausing makes me backend-agnostic.

**"How do you prevent two controllers waking the same workspace?"**
> Single-writer per workspace. In Kubernetes that's the reconcile loop's per-key serialization plus leader election. Same property as opencode's run-coordinator and Amp's thread actors — it's the actor model, just spelled differently in each system.

**"What would you do with more time?"**
> Invert the event flow — a relay inside the pod pushing to a webhook instead of the controller holding a long-lived SSE connection, because a dropped connection can silently lose the idle event that triggers everything. codecloud hit exactly this in production.

---

## The rules again

1. Week 2 Friday is a gate. It works, or week 3 is a fix week.
2. One CRD.
3. Day 1 deliverable is three sentences.
4. `FlushInterval: -1` on day 5.
5. 8GB memory from the start.
6. Auth before anything is reachable from outside localhost.
7. Ship, then write.
