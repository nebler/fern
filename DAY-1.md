# Day 1: Can an OpenCode Session Survive Process Death?

## Result first

**Yes, the durable session survives a process crash. No, an in-progress provider turn does not resume automatically, and the newest streamed text can be lost.**

That means fern's safe first implementation is:

1. Watch OpenCode until the whole turn is idle.
2. Start an idle countdown.
3. Cancel that countdown if the session becomes busy or enters retry.
4. Pause only after the countdown expires while the session is still idle.
5. Never claim that fern can preserve or resume a request paused halfway through generation.

The strict three-sentence gate answer is in [NOTES.md](./NOTES.md). This document explains how we reached it from scratch.

## What was analyzed

The requested repository was cloned from <https://github.com/anomalyco/opencode.git> into [opencode/](./opencode/).

The source findings below are pinned to commit [`39fb919a054190498f6d5b7985bde231f93ad7a6`](https://github.com/anomalyco/opencode/tree/39fb919a054190498f6d5b7985bde231f93ad7a6), committed on 2026-08-12. Pinning matters because GitHub's default branch can move after this report is written; every GitHub link in this report points to the exact code that was read.

The empirical experiment used the separately installed `opencode` CLI version `1.18.16`. This distinction matters: source inspection proves what the pinned checkout says, while the experiment proves what the installed binary did. They agree on the important persistence behavior, but they are not the same build.

## Vocabulary from absolute scratch

### Process memory

When a program runs, it keeps temporary values in RAM. Examples here include:

- the currently open HTTP connection to the model provider;
- text fragments received so far;
- a map saying which sessions are currently running;
- timers, fibers, promises, and pending permission waits.

`kill -9` terminates the process immediately. The process gets no chance to run cleanup code. Everything that exists only in process memory disappears.

### SQLite

SQLite is a database stored in files on disk. OpenCode uses it for durable sessions, events, projected messages, inputs, and parts. A newly started OpenCode process can query those rows again.

"Durable" does not mean every byte the UI has ever displayed is already in SQLite. It means a value survives only after code commits it to the database.

### Event and projection

The V2 session core is event-driven:

- A **durable event** records that something happened, such as a prompt being admitted or a step starting.
- A **projector** handles that event inside the database transaction and updates convenient tables such as `session_message`.
- Later reads use those projected tables instead of reconstructing the screen from scratch every time.

For example, a `session.next.step.started` event creates an incomplete assistant message projection. A later `session.next.step.ended` event marks that assistant message complete.

### Provider turn, step, and drain

For this report:

- A **provider turn** is one request to an LLM provider and the streamed response from it.
- A **step** is effectively one provider turn plus any tool calls produced by it.
- A **drain** is one serialized runner execution that consumes eligible durable work. A drain can contain several provider turns when tool results require the model to continue.
- A **turn boundary** is the safe point after the complete user-to-agent interaction is no longer busy.

### Fiber

Effect, the TypeScript framework used here, calls lightweight concurrent tasks **fibers**. For a beginner, treating a fiber as a cancellable asynchronous task is sufficient for this analysis.

### WAL and `synchronous`

SQLite's write-ahead log, or WAL, records changes in a separate `-wal` file before those changes are merged into the main database file. OpenCode configures:

- `journal_mode = WAL`;
- `synchronous = NORMAL`.

This combination is designed to survive an ordinary process crash. `NORMAL` does not promise the strongest machine-power-loss durability because the operating system may not have physically flushed every recent write to storage. `kill -9` is process death, not literal power removal, so this experiment does not prove abrupt host or VM power-loss safety.

## The execution path

The shortest useful mental model is:

```text
prompt
  -> commit durable session_input row
  -> wake(session ID)
  -> coordinator starts or marks one follow-up drain
  -> local execution resolves the session's location
  -> runner checks pending durable work
  -> runner promotes the prompt
  -> runner loads history from SQLite
  -> runner opens one provider stream
  -> durable stream boundaries update SQLite
  -> live-only deltas are sent to listeners
  -> runner repeats while continuation work exists
  -> drain ends
```

The important split is between the database-backed boxes and the process-memory boxes.

| Durable in SQLite | Process-local only |
|---|---|
| Session record | Active coordinator map |
| Admitted prompt | Running coordinator fiber |
| Durable event sequence | `pendingWake` flag |
| Projected user messages | Open provider HTTP stream |
| Started assistant message | Text/reasoning fragment buffers before their end events |
| Completed text/reasoning boundaries | Tool fibers currently executing |
| Tool progress checkpoints and results | Loop variables such as current step and `needsContinuation` |

## File-by-file explanation

### 1. Coordinator: one owner per session ID

Read [local `run-coordinator.ts`](./opencode/packages/core/src/session/run-coordinator.ts) or [the pinned GitHub source](https://github.com/anomalyco/opencode/blob/39fb919a054190498f6d5b7985bde231f93ad7a6/packages/core/src/session/run-coordinator.ts).

The file's first key statement is its purpose: it serializes execution for each key while allowing different keys to run concurrently ([local lines 5-15](./opencode/packages/core/src/session/run-coordinator.ts#L5-L15), [GitHub](https://github.com/anomalyco/opencode/blob/39fb919a054190498f6d5b7985bde231f93ad7a6/packages/core/src/session/run-coordinator.ts#L5-L15)). Here, the key is a session ID.

Its `active` value is a plain in-memory `Map` ([local lines 27-35](./opencode/packages/core/src/session/run-coordinator.ts#L27-L35)). Each entry holds:

- `done`: a signal completed when the current drain finishes;
- `owner`: the fiber running the drain;
- `pendingWake`: whether work arrived during that drain;
- `stopping`: whether interruption is underway.

None of that coordinator bookkeeping is stored in SQLite.

### 2. Exactly what `run` does

`run(key)` is implemented at [local lines 67-79](./opencode/packages/core/src/session/run-coordinator.ts#L67-L79) and [GitHub lines 67-79](https://github.com/anomalyco/opencode/blob/39fb919a054190498f6d5b7985bde231f93ad7a6/packages/core/src/session/run-coordinator.ts#L67-L79).

It behaves as follows:

1. If the same session already has an active drain, join it by waiting for its `done` signal.
2. If that drain is being stopped, wait for cleanup and then try `run` again.
3. If the session is inactive, create an entry and call `start(..., force=true)`.
4. Wait until that newly started drain finishes.

The `force=true` detail means an explicit run performs a provider attempt even if the normal pending-work check finds nothing eligible. This is also documented by the runner interface ([local `runner/index.ts` lines 19-25](./opencode/packages/core/src/session/runner/index.ts#L19-L25)).

### 3. Exactly what `wake` and coalescing mean

`wake(key)` is implemented at [local lines 81-92](./opencode/packages/core/src/session/run-coordinator.ts#L81-L92) and [GitHub](https://github.com/anomalyco/opencode/blob/39fb919a054190498f6d5b7985bde231f93ad7a6/packages/core/src/session/run-coordinator.ts#L81-L92).

If no drain is active, `wake` starts a non-forced drain and returns without joining it. If a drain is active, `wake` only sets `pendingWake = true`.

**Coalescing concretely means that 1 wake or 100 wakes received during the same active drain all set the same boolean to `true`; they produce at most one successor drain, not 100 drains.** When the active drain succeeds, `settle` clears that boolean and starts one non-forced successor ([local lines 51-65](./opencode/packages/core/src/session/run-coordinator.ts#L51-L65)). The successor re-queries durable work, so it does not need a count of wake notifications.

This is safe only because the notification is advisory while the actual work is durable in the database.

### 4. Session execution routes by ID, not cached runner state

Read [local `execution.ts`](./opencode/packages/core/src/session/execution.ts) and [local `execution/local.ts`](./opencode/packages/core/src/session/execution/local.ts).

The interface exposes only `active`, `resume`, `wake`, and `interrupt` ([local `execution.ts` lines 9-21](./opencode/packages/core/src/session/execution.ts#L9-L21)). The comment deliberately says that execution routes from a session ID to the runner owned by that session's location.

When a drain starts, the local implementation:

1. calls `store.get(sessionID)`;
2. obtains the session's persisted location;
3. gets the runner service for that location;
4. invokes `runner.run({ sessionID, force })`.

See [local `execution/local.ts` lines 14-29](./opencode/packages/core/src/session/execution/local.ts#L14-L29) or [GitHub](https://github.com/anomalyco/opencode/blob/39fb919a054190498f6d5b7985bde231f93ad7a6/packages/core/src/session/execution/local.ts#L14-L29).

This tells us each new drain rediscovers placement from persistent session data. It does **not** tell us that an interrupted provider call can restart, because the open call and its exact execution point are not represented by a durable drain identity.

### 5. Prompt admission is durable before wake

The V2 public session service admits the input in an uninterruptible region and only then calls `execution.wake` ([local `session.ts` lines 360-385](./opencode/packages/core/src/session.ts#L360-L385), [GitHub](https://github.com/anomalyco/opencode/blob/39fb919a054190498f6d5b7985bde231f93ad7a6/packages/core/src/session.ts#L360-L385)).

That order matters:

```text
commit work first -> notify executor second
```

If the wake notification is duplicated, coalescing handles it. If the process dies immediately after admission but before wake, the prompt remains in `session_input`; however, the current code does not include an automatic process-start recovery scanner that wakes every such session. A later explicit resume or another wake is needed.

The `session_input` schema stores `admitted_seq` and nullable `promoted_seq` ([local `sql.ts` lines 140-165](./opencode/packages/core/src/session/sql.ts#L140-L165)). A null `promoted_seq` means durable input exists but has not yet become a visible user message consumed by the runner.

### 6. What `SessionMessageTable.seq` means

The table declares `seq` as non-null and adds a unique index on `(session_id, seq)` ([local `sql.ts` lines 119-137](./opencode/packages/core/src/session/sql.ts#L119-L137), [GitHub](https://github.com/anomalyco/opencode/blob/39fb919a054190498f6d5b7985bde231f93ad7a6/packages/core/src/session/sql.ts#L119-L137)).

The value is **assigned when a durable event is committed**, not when history is read. The event service reads the latest aggregate sequence and chooses `latest + 1` inside an immediate SQLite transaction ([local `event.ts` lines 237-353](./opencode/packages/core/src/event.ts#L237-L353), especially [lines 243-250](./opencode/packages/core/src/event.ts#L243-L250) and [294-348](./opencode/packages/core/src/event.ts#L294-L348)). The session projector copies that durable event sequence into the message row ([local `projector.ts` lines 193-208](./opencode/packages/core/src/session/projector.ts#L193-L208)).

Therefore:

- It is monotonic within one session's durable event stream.
- It is unique for each projected message in a session.
- It is not a global sequence across all sessions.
- Message-row sequences can have gaps because some durable events update an existing message rather than insert a new message.
- Reading history does not invent or renumber sequences; it orders rows by the stored values.

### 7. History is loaded from SQLite at every provider-turn attempt

The runner's decisive line is [local `runner/llm.ts` line 200](./opencode/packages/core/src/session/runner/llm.ts#L200):

```ts
const entries = yield* SessionHistory.entriesForRunner(db, session.id, system.baselineSeq)
```

That line sits inside `runTurnAttempt` ([local lines 173-214](./opencode/packages/core/src/session/runner/llm.ts#L173-L214), [GitHub](https://github.com/anomalyco/opencode/blob/39fb919a054190498f6d5b7985bde231f93ad7a6/packages/core/src/session/runner/llm.ts#L173-L214)). Each provider-turn attempt re-fetches the session, prepares the system context, and reloads ordered history before constructing the LLM request.

`entriesForRunner` executes a SQLite query and sorts by ascending `seq` ([local `history.ts` lines 24-53](./opencode/packages/core/src/session/history.ts#L24-L53) and [90-99](./opencode/packages/core/src/session/history.ts#L90-L99)). `SessionStore.runnerContext` is only a database-backed wrapper over that loader ([local `store.ts` lines 39-44](./opencode/packages/core/src/session/store.ts#L39-L44)). There is no cached conversation array shared between provider turns in this path.

This is the strongest positive Day 1 result: **completed durable boundaries are sufficient to reconstruct the next provider request.**

### 8. What still exists only in memory during a turn

Reloading history at the start does not make the active turn itself durable.

The runner opens exactly one provider stream at [local `runner/llm.ts` lines 228-275](./opencode/packages/core/src/session/runner/llm.ts#L228-L275). During that stream it holds in memory:

- `needsContinuation` and `currentStep` ([lines 184-186](./opencode/packages/core/src/session/runner/llm.ts#L184-L186));
- a set of active tool fibers ([line 184](./opencode/packages/core/src/session/runner/llm.ts#L184));
- the live provider stream ([lines 232-275](./opencode/packages/core/src/session/runner/llm.ts#L232-L275));
- publisher maps and fragment arrays.

The publisher's text, reasoning, and tool-input fragment helper stores chunks in an in-memory `Map<string, string[]>` ([local `publish-llm-event.ts` lines 91-119](./opencode/packages/core/src/session/runner/publish-llm-event.ts#L91-L119)).

### 9. Where partially streamed assistant text lives

In V2, text delta events are explicitly **live-only**. The schema comment says the full `Text.Ended` value is the replayable boundary ([local `schema/session-event.ts` lines 197-231](./opencode/packages/schema/src/session-event.ts#L197-L231), [GitHub](https://github.com/anomalyco/opencode/blob/39fb919a054190498f6d5b7985bde231f93ad7a6/packages/schema/src/session-event.ts#L197-L231)).

The sequence is:

1. `Text.Started` is durable and creates an empty text item in the assistant projection.
2. Every `Text.Delta` is published to live listeners but is not a durable event.
3. The publisher accumulates delta strings in RAM.
4. `Text.Ended` joins the fragments into a full string and publishes a durable event.
5. The projector updates the assistant message row with that complete text.

The relevant publisher code is [local `publish-llm-event.ts` lines 121-142](./opencode/packages/core/src/session/runner/publish-llm-event.ts#L121-L142) and [239-267](./opencode/packages/core/src/session/runner/publish-llm-event.ts#L239-L267). The projector handles durable session events in the same SQLite transaction as event commit ([local `event.ts` lines 237-353](./opencode/packages/core/src/event.ts#L237-L353)) and updates `SessionMessageTable` through [local `projector.ts` lines 112-190](./opencode/packages/core/src/session/projector.ts#L112-L190).

Therefore a crash after `Text.Started` but before `Text.Ended` can leave a durable incomplete assistant message with empty text. That is exactly what the experiment observed.

### 10. What the old `updatePart` path does

The build plan asks, "`updatePart` publishes an event; who persists it?" That name belongs to the current compatibility/V1 runtime rather than the new V2 publisher.

`updatePart` publishes `SessionV1.Event.PartUpdated` ([local `packages/opencode/src/session/session.ts` lines 631-645](./opencode/packages/opencode/src/session/session.ts#L631-L645)). The session projector subscribes to that event and performs an insert-or-update in `PartTable` ([local `projector.ts` lines 312-329](./opencode/packages/core/src/session/projector.ts#L312-L329)).

However, `updatePartDelta` publishes `message.part.delta` without updating the table ([local `session.ts` lines 879-887](./opencode/packages/opencode/src/session/session.ts#L879-L887)). The compatibility processor builds the current text in memory, publishes deltas during streaming, and calls full `updatePart` on text end or graceful cleanup ([local `processor.ts` lines 486-531](./opencode/packages/opencode/src/session/processor.ts#L486-L531) and [539-560](./opencode/packages/opencode/src/session/processor.ts#L539-L560)). `kill -9` prevents graceful cleanup, so the latest deltas can disappear.

### 11. The drain loop has in-memory control state

The outer runner loop is at [local `runner/llm.ts` lines 383-406](./opencode/packages/core/src/session/runner/llm.ts#L383-L406).

It queries pending `steer` and `queue` inputs from SQLite, but then controls the active drain with local variables:

- `promotion`;
- `shouldRun`;
- `needsContinuation`;
- `step`.

After each provider turn, the next turn reloads history, which is good. But if the process dies, the loop itself is gone. There is no durable record saying, "this exact drain was on step 3 and must be restarted." The repository's own [AGENTS.md](./opencode/AGENTS.md) states that post-crash continuation recovery needs a separate explicit design and that a drain has no durable identity or transcript boundary.

### 12. Permission prompts are another in-memory wait

The new permission service stores pending requests in a process-local map with a deferred waiter; it publishes an asked event and waits for the deferred reply ([local `core/src/permission.ts` lines 176-218](./opencode/packages/core/src/permission.ts#L176-L218)). The compatibility permission service follows the same pattern ([local `opencode/src/permission/index.ts` lines 67-106](./opencode/packages/opencode/src/permission/index.ts#L67-L106)).

This means a pending permission event can be visible to clients, but the exact waiter that resumes tool execution is process memory. A hard process restart is not guaranteed to recreate it. For fern, "waiting for permission" must not be casually treated as an ordinary safe-to-stop idle state until this is tested separately on Day 3/4.

### 13. SQLite settings

The database setup explicitly executes the pragmas at [local `database.ts` lines 22-35](./opencode/packages/core/src/database/database.ts#L22-L35), including:

```sql
PRAGMA journal_mode = WAL;
PRAGMA synchronous = NORMAL;
```

It also sets a five-second busy timeout, enables foreign keys, and performs a passive WAL checkpoint.

## Controlled kill experiment

### Purpose

The experiment answered three questions:

1. Does the session survive `kill -9`?
2. Does partial streamed text survive?
3. Is SQLite corrupted?

### Safety and isolation

The test used:

- a throwaway git repository at [experiment/fixture/](./experiment/fixture/);
- a dedicated database at `experiment/opencode-day1.db`;
- a local fake model at [experiment/fake-llm.mjs](./experiment/fake-llm.mjs);
- a fixture-only provider configuration at [experiment/fixture/opencode.json](./experiment/fixture/opencode.json);
- no external API key and no paid model request.

The fake server sent a unique marker, `FERN_PARTIAL_MARKER`, and deliberately never sent the stream's finish event. The test subscribed to OpenCode's own `/event` SSE endpoint and waited until OpenCode emitted a `message.part.delta` containing that marker before killing the process; this proves OpenCode processed the delta, not merely that the fake server wrote it to a socket.

### Procedure

The core procedure was:

```bash
# Start the fake OpenAI-compatible provider.
node experiment/fake-llm.mjs

# Start OpenCode with an isolated database.
OPENCODE_DB="$PWD/experiment/opencode-day1.db" \
  OPENCODE_DISABLE_AUTOUPDATE=1 \
  OPENCODE_DISABLE_MODELS_FETCH=1 \
  opencode serve --port 4096 --hostname 127.0.0.1 --pure \
  --print-logs --log-level DEBUG

# Create a session and POST a prompt through /session/:id/message.
# Watch /event and wait until OpenCode emits a message.part.delta containing
# FERN_PARTIAL_MARKER.

# Kill the OpenCode server immediately, with no cleanup opportunity.
kill -9 <opencode-server-pid>

# Restart with the same OPENCODE_DB path, then inspect the session/messages.

sqlite3 experiment/opencode-day1.db 'PRAGMA integrity_check;'
```

The full artifacts are preserved here:

- [Effective test config](./experiment/config-effective.json)
- [Created session](./experiment/session-verified-created.json)
- [OpenCode SSE stream proving it processed the marker](./experiment/events-verified.log#L129)
- [Fake provider log](./experiment/fake-llm-verified.log)
- [OpenCode log before the kill](./experiment/server-verified-before.log)
- [Health response after restart](./experiment/health-verified-after.json)
- [Sessions after restart](./experiment/sessions-verified-after.json)
- [Messages after restart](./experiment/messages-verified-after.json)
- [Session status after restart](./experiment/status-verified-after.json)
- [SQLite checks](./experiment/sqlite-verified-check.txt)
- [Client error caused by killing the server](./experiment/prompt-verified-error.log)

### Observed results

| Question | Result | Evidence |
|---|---|---|
| Did the session survive? | Yes | The same session ID appears after restart in [sessions-verified-after.json](./experiment/sessions-verified-after.json). |
| Did the user prompt survive? | Yes | The user message and its complete text appear in [messages-verified-after.json](./experiment/messages-verified-after.json). |
| Did an assistant message survive? | Partly | The assistant row, step-start part, and empty text part survived. |
| Did OpenCode process the streamed marker? | Yes | OpenCode's own SSE output contains `message.part.delta` with the marker in [events-verified.log line 129](./experiment/events-verified.log#L129). |
| Did the streamed marker survive? | No | [messages-verified-after.json](./experiment/messages-verified-after.json) contains `"text":""` for the same part ID seen in the delta event. |
| Did the turn finish? | No | The assistant message has no completion time or finish reason. |
| Did OpenCode resume the provider call after restart? | No | The restarted process reported healthy, but no continuation was initiated; status was `{}` in [status-verified-after.json](./experiment/status-verified-after.json). |
| Was SQLite corrupted? | No | [sqlite-verified-check.txt](./experiment/sqlite-verified-check.txt) reports `integrity_check=ok`. |
| Was WAL active? | Yes | [sqlite-verified-check.txt](./experiment/sqlite-verified-check.txt) reports `journal_mode=wal`. |
| Was synchronous mode NORMAL? | Yes | SQLite reports numeric value `1`, which is `NORMAL`. |

### Why two fake-model requests occurred

[fake-llm-verified.log](./experiment/fake-llm-verified.log) shows two requests before the kill. The server log identifies the first as the small title-generation model request and the second as the real build-agent request. Both used the fake test model because the fixture set both `model` and `small_model` to `test/test-model`.

### What the experiment proves and does not prove

It proves:

- committed session and message data survived ordinary process death;
- an incomplete turn can leave readable, non-corrupt database state;
- live-only streamed text can be lost;
- restart did not automatically continue the request.

It does not prove:

- survival of sudden host power loss;
- behavior of the exact pinned source commit as a compiled binary;
- safety of pausing during a tool side effect;
- safety of pausing while waiting for a permission response;
- behavior of Docker pause, Docker stop, or Kubernetes scale-to-zero.

Those are later experiments, not conclusions to smuggle into Day 1.

## Direct answers to the gate questions

### Does the runner reconstruct everything from SQLite?

**It reconstructs the durable conversation context needed to start each provider turn, but not the active turn's execution machinery.** Each turn reloads ordered history from SQLite. The provider connection, fragment buffers, tool fibers, loop counters, permission waiters, and coordinator state are in RAM.

### Is `SessionMessageTable.seq` monotonic per session?

**Yes.** It comes from the session's durable aggregate event sequence, is allocated at commit time as the previous sequence plus one, and is protected by unique indexes. It is not assigned at read time. Message rows can have sequence gaps because the aggregate sequence includes durable events that do not insert a new message.

### Does every drain load history from the database?

**More precisely, every provider-turn attempt inside a drain loads history from the database.** A single drain can perform several provider turns, and each attempt calls `SessionHistory.entriesForRunner(...)` again.

### What is the difference between `run` and `wake`?

**`run` starts or joins a forced drain and waits for it; `wake` is a non-blocking advisory notification.** Repeated wakes during an active drain collapse into one boolean and therefore one follow-up drain.

### Where does partial assistant output live?

**The started assistant/text shape is durable, but V2 text deltas are live-only and buffered in process memory until `Text.Ended` commits the complete value.** In the compatibility path used by the installed CLI, delta events similarly remain unprojected until a full part update occurs. A hard kill can therefore leave an empty incomplete text part.

### Did the partial turn survive?

**Structurally, yes; textually, no.** The user message, assistant row, step-start, and empty text part survived. The unique streamed marker did not.

### Did the session survive?

**Yes.** It remained listable and its committed history remained readable after restart.

### Was the database corrupted?

**No.** `PRAGMA integrity_check` returned `ok`.

## Architecture decision for fern

fern should pause only after receiving and validating a true turn-boundary idle signal. This decision is not merely conservative; it follows directly from the persistence model:

- Before the turn, the admitted prompt is durable.
- During the turn, progress is a mix of durable boundaries and live process state.
- At the complete idle boundary, the useful transcript is committed and no provider request or tool execution should still depend on process memory.

Even then, the supervisor must treat events as signals rather than absolute truth. Before pausing, it should re-check the current session status because an idle event can race with newly submitted work.

## Day 2 implications

These findings constrain the Docker harness:

1. Persist OpenCode's home/data directory, not only the checked-out repository. The SQLite database must live on durable storage.
2. Do not pause based on network quietness. A permission wait, retry backoff, or long tool call can be quiet while still active.
3. Treat `busy` and `retry` as non-idle.
4. Do not advertise mid-turn recovery.
5. Re-inspect health and endpoint state after every resume.
6. Plan a separate recovery policy for incomplete assistant rows after an unclean crash.

The health path in the plan is correct for this checkout and installed binary: it is declared at [local `global.ts` lines 65-93](./opencode/packages/opencode/src/server/routes/instance/httpapi/groups/global.ts#L65-L93), and the experiment received `{"healthy":true,"version":"1.18.16"}` from `/global/health`.

## Environment notes

The local prerequisites checked during this task were:

| Tool | Result |
|---|---|
| Go | `go1.24.2`, sufficient for the plan's Go 1.23+ requirement |
| OpenCode | `1.18.16` |
| Bun | `1.2.5` |
| Docker CLI | `28.0.1` |
| Docker daemon | Not running during this Day 1 task |
| kubectl | `v1.32.2` client |
| kind | Not installed |
| `ANTHROPIC_API_KEY` | Not set; not needed because the experiment used a local fake provider |

Docker, kind, and Kubernetes are not Day 1 requirements, but these gaps should be resolved before their scheduled phases.

## Final beginner summary

OpenCode is neither "all in memory" nor "every streaming byte safely durable." It has a carefully durable session/event backbone, while active execution still lives in the process. That is enough for fern's thesis if fern sleeps only at turn boundaries: completed history survives, a later request can start from the persisted context, and the workspace does not need a memory snapshot to preserve an idle conversation.
