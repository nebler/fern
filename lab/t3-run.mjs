// t3-run.mjs drives T3's server the way the T3 app does, so a demo can run
// many times without anyone clicking: it starts a New worktree thread on a
// sample project, sends the scenario's messages, reverts a turn, checks the
// answers and times every step. Run it through lab/t3-run.sh, which pairs
// with the server and passes T3_PAIR_URL, T3_METRICS_CMD, T3_RM_ENV_CMD and
// T3_NODE_MEMORY_CMD.
//
// PARALLEL runs that many threads at once, as several T3 threads working
// side by side would; the server costs of their steps then overlap, so
// only the wall times and the node's peak memory are each run's own.
//
// The threads show up in the T3 app under the server's connection while they
// run. T3's wire protocol is Effect RPC over a WebSocket, one JSON object per
// frame; see T3's packages/contracts/src/orchestrationV2.ts for the payloads.
import { execFile } from "node:child_process";
import { promisify } from "node:util";
import { appendFileSync } from "node:fs";
import { randomUUID } from "node:crypto";

const env = (name, fallback) => process.env[name] ?? fallback;
const scenarioName = env("SCENARIO", "toy");
const runs = Number(env("RUNS", "1"));
const model = env("MODEL", "claude-haiku-5-5");
const keep = env("KEEP", "") !== "";
const out = env("OUT", "lab/t3-runs.jsonl");
const parallel = Number(env("PARALLEL", "1"));
// The commands go through bash, as the wrapper's helpers are bash; not
// blocking, so parallel runs keep polling while one waits on the node.
const bash = async (cmd, ...args) => (await promisify(execFile)("bash", ["-e", "-o", "pipefail", "-c", cmd, "bash", ...args])).stdout;

// CPU counters are cgroup lifetime totals. Guest CPU is a sampled lower
// bound (short-lived children between polls are missed). Pod CPU includes
// sentry/gofers; guest RSS is approximate and may double-count shared pages.
async function resourceSample(path = "", includeGuest = true, boxUID = "") {
  if (!env("T3_RESOURCE_CMD")) return null;
  const values = {}, guest = [];
  for (const line of (await bash(env("T3_RESOURCE_CMD"), path, boxUID, includeGuest ? "guest" : "host")).trim().split("\n")) {
    const parts = line.trim().split(/\s+/);
    if (parts[0] === "BOXUID") boxUID = parts[1];
    else if (parts[0] === "GUEST") guest.push({ id: parts[1], cpu: +parts[2], rss: +parts[3], anon: +parts[4], role: parts[5] });
    else if (parts.length === 2) values[parts[0]] = +parts[1];
  }
  if (values.box_cpu_ms == null) boxUID = "";
  return { values, guest, boxUID };
}

function resourceTracker(before) {
  const peaks = {}, processCPU = new Map();
  let last = before, seenClaude = false, samples = 0;
  const add = (sample) => {
    if (!sample) return;
    samples++;
    const totals = {};
    for (const p of sample.guest) {
      const counter = processCPU.get(p.id) ?? { first: samples === 1 ? p.cpu : 0, last: 0, role: p.role };
      counter.last = Math.max(counter.last, p.cpu);
      // Retain ancestry if a child is later reparented after Claude exits.
      if (p.role === "claude") counter.role = "claude";
      processCPU.set(p.id, counter);
      for (const field of ["rss", "anon"]) totals[`guest_${p.role}_peak_${field}_mib`] = (totals[`guest_${p.role}_peak_${field}_mib`] ?? 0) + p[field];
      if (p.role === "claude") seenClaude = true;
    }
    for (const [k, v] of Object.entries(totals)) peaks[k] = Math.max(peaks[k] ?? 0, v);
    peaks.box_peak_memory_mib = Math.max(peaks.box_peak_memory_mib ?? 0, sample.values.box_memory_mib ?? 0);
    last = sample;
  };
  add(before);
  return { add, result() {
    const values = { ...peaks, samples_count: samples };
    for (const name of ["node", "daemon", "t3", "box"]) {
      const key = `${name}_cpu_ms`;
      if (last?.values[key] != null) values[key] = Math.max(0, last.values[key] - (before?.values[key] ?? 0));
    }
    if (seenClaude) for (const role of ["claude", "other"]) values[`guest_${role}_sampled_cpu_ms`] =
      [...processCPU.values()].filter((p) => p.role === role).reduce((sum, p) => sum + p.last - p.first, 0);
    return values;
  } };
}

// A scenario is a project and its steps. A "send" step is a user message,
// with a pattern its answer must match; a "revert" step goes back to the
// checkpoint before turn N, files and environment too.
const scenarios = {
  sample: {
    project: "sample",
    steps: [
      { send: "How many rows are in the users table? Answer with just the number.", expect: /\b3\b/ },
      { send: "Drop the users table. Then answer with just the word dropped.", expect: /dropped/i },
      { revert: 2 },
      { send: "How many rows are in the users table now? Answer with just the number.", expect: /\b3\b/ },
    ],
  },
  toy: {
    project: "toy",
    steps: [
      { send: "Using curl against localhost:3000/notes, how many notes are there? Answer with just the number.", expect: /\b3\b/ },
      {
        send:
          "Add a note with the text 'from claude' through POST localhost:3000/notes. Then add a GET /notes/:id route " +
          "to app.js with a test for it, and run node --test. Answer with just the word done.",
        expect: /done/i,
      },
      { revert: 2 },
      {
        send:
          "How many notes does curl localhost:3000/notes return now, and does app.js have a GET /notes/:id route? " +
          "Answer exactly as '<number> yes' or '<number> no'.",
        expect: /\b3 no\b/i,
      },
    ],
  },
};

const scenario = scenarios[scenarioName];
if (!scenario) throw new Error(`unknown scenario ${scenarioName}; have ${Object.keys(scenarios).join(", ")}`);

const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

// pair trades the one-time pairing credential for a bearer token.
async function pair(pairURL) {
  const url = new URL(pairURL);
  const credential = new URLSearchParams(url.hash.slice(1)).get("token");
  const res = await fetch(new URL("/oauth/token", url.origin), {
    method: "POST",
    body: new URLSearchParams({
      grant_type: "urn:ietf:params:oauth:grant-type:token-exchange",
      subject_token: credential,
      subject_token_type: "urn:t3:params:oauth:token-type:environment-bootstrap",
      requested_token_type: "urn:ietf:params:oauth:token-type:access_token",
      client_label: "fern t3-run",
    }),
  });
  if (!res.ok) throw new Error(`pair: ${res.status} ${await res.text()}`);
  return { origin: url.origin, token: (await res.json()).access_token };
}

// connect opens T3's WebSocket. Node's WebSocket can't send headers, so it
// authenticates with a short-lived ticket in the URL, as the app does.
async function connect({ origin, token }) {
  const res = await fetch(new URL("/api/auth/websocket-ticket", origin), {
    method: "POST",
    headers: { authorization: `Bearer ${token}` },
  });
  if (!res.ok) throw new Error(`websocket ticket: ${res.status} ${await res.text()}`);
  const { ticket } = await res.json();
  const ws = new WebSocket(`${origin.replace(/^http/, "ws")}/ws?wsTicket=${ticket}&orchestrationProtocol=2`);
  await new Promise((resolve, reject) => {
    ws.onopen = resolve;
    ws.onerror = () => reject(new Error("websocket: could not connect"));
  });
  const pending = new Map();
  const subscriptions = new Map();
  let next = 0;
  ws.onmessage = (event) => {
    const frame = JSON.parse(event.data);
    if (frame._tag === "Chunk") {
      const observe = subscriptions.get(frame.requestId);
      for (const value of frame.values) observe?.(value, Date.now());
      ws.send(JSON.stringify({ _tag: "Ack", requestId: frame.requestId }));
      return;
    }
    if (frame._tag !== "Exit") return;
    subscriptions.delete(frame.requestId);
    const call = pending.get(frame.requestId);
    pending.delete(frame.requestId);
    if (!call) return;
    if (frame.exit._tag === "Success") call?.resolve(frame.exit.value);
    else call?.reject(new Error(`${call.tag}: ${JSON.stringify(frame.exit.cause)}`));
  };
  ws.onclose = () => {
    for (const call of pending.values()) call.reject(new Error(`${call.tag}: websocket closed`));
    pending.clear();
    subscriptions.clear();
  };
  const ping = setInterval(() => ws.send(JSON.stringify({ _tag: "Ping" })), 5000);
  return {
    subscribe(tag, payload, observe) {
      const id = String(++next);
      subscriptions.set(id, observe);
      ws.send(JSON.stringify({ _tag: "Request", id, tag, payload, headers: [] }));
      return () => {
        subscriptions.delete(id);
        if (ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify({ _tag: "Interrupt", requestId: id }));
      };
    },
    call(tag, payload) {
      const id = String(++next);
      ws.send(JSON.stringify({ _tag: "Request", id, tag, payload, headers: [] }));
      return new Promise((resolve, reject) => pending.set(id, { tag, resolve, reject }));
    },
    close() {
      clearInterval(ping);
      ws.close();
    },
  };
}

// metrics reads the daemon's Prometheus counters: each histogram's _sum and
// _count, by name and labels. The difference across a step is what the step
// cost on the server.
async function metrics() {
  const text = await bash(env("T3_METRICS_CMD"));
  const values = new Map();
  for (const line of text.split("\n")) {
    const m = line.match(/^(fern_\w+)_(sum|count)(\{[^}]*\})? ([0-9.e+-]+)$/);
    if (m) values.set(`${m[1]}${m[3] ?? ""} ${m[2]}`, Number(m[4]));
  }
  return values;
}

// serverCost lists what the server did between two metrics readings: for
// each histogram that moved, how many times and how long in total.
function serverCost(before, after) {
  const cost = {};
  for (const [key, value] of after) {
    if (!key.endsWith(" count")) continue;
    const n = value - (before.get(key) ?? 0);
    if (n <= 0) continue;
    const name = key.slice(0, -" count".length);
    const sum = after.get(`${name} sum`) - (before.get(`${name} sum`) ?? 0);
    cost[name.replace(/^fern_/, "")] = { n, ms: Math.round(sum * 1000) };
  }
  return cost;
}

async function waitFor(what, check, timeoutMs = 300_000) {
  const deadline = Date.now() + timeoutMs;
  for (;;) {
    const result = await check();
    if (result) return result;
    if (Date.now() > deadline) throw new Error(`timed out waiting for ${what}`);
    await sleep(500);
  }
}

const done = new Set(["completed", "failed", "interrupted", "cancelled"]);
const ms = (a, b) => (a && b ? new Date(b) - new Date(a) : null);

// These timestamps use the server/node clock, except sentAt and observedEndAt
// on the driver. The lab VM and driver must have synchronized clocks. A reused
// Claude process has no new start or first-event timestamp for that step.
async function turnTiming(p, run, start, end, streamEvents, previousApiMs) {
  const cwd = p.thread.worktreePath;
  const trace = [];
  if (env("T3_TRACE_CMD") && cwd) {
    for (const line of (await bash(env("T3_TRACE_CMD"), cwd)).split("\n")) {
      if (line.startsWith("{")) {
        const event = JSON.parse(line);
        if (event.cwd === cwd && Date.parse(event.at) >= start && Date.parse(event.at) <= end) trace.push(event);
      } else {
        const match = line.match(/^time=(\S+) level=INFO msg="claude (started|first stdout)" .*pid=(\d+) (?:spawn_ms|since_request_ms)=([\d.]+)/);
        if (match && Date.parse(match[1]) >= start && Date.parse(match[1]) <= end) {
          trace.push({ event: `claude.${match[2].replaceAll(" ", "_")}`, at: match[1], pid: Number(match[3]), ms: Number(match[4]) });
        }
      }
    }
  }
  trace.sort((a, b) => Date.parse(a.at) - Date.parse(b.at));
  const at = (event) => trace.find((e) => e.event === event)?.at ?? null;
  const items = p.turnItems.filter((item) => item.runId === run.id && item.type !== "user_message");
  // T3 also represents worktree preparation as a command. It has no
  // provider turn and must not masquerade as Claude's first output.
  const firstContentAt = items.filter((item) => item.providerTurnId && ["assistant_message", "reasoning", "command_execution", "file_change", "tool_call"].includes(item.type))
    .map((item) => item.startedAt).filter(Boolean).sort()[0] ?? null;
  const events = streamEvents.filter((event) => event.observedAt >= start && event.runId === run.id);
  const completed = events.find((event) => event.type === "run.updated" && done.has(event.status));
  const result = trace.find((event) => event.event === "claude.result");
  return {
    sentAt: new Date(start).toISOString(),
    observedEndAt: new Date(end).toISOString(),
    requestedAt: run.requestedAt, startedAt: run.startedAt, completedAt: run.completedAt,
    messageToProcessMs: ms(start, at("claude.started")),
    wrapperToProcessMs: ms(at("wrapper.start"), at("claude.started")),
    processToStdoutMs: ms(at("claude.started"), at("claude.first_stdout")),
    messageToFirstEventMs: ms(start, at("claude.first_event")),
    messageToFirstContentMs: ms(start, firstContentAt),
    // Stream delivery is the user-facing boundary; wallMs retains its old
    // polling-based meaning for compatibility with existing result readers.
    messageToCompletedMs: completed ? completed.observedAt - start : null,
    claudeApiTotalMs: result?.apiMs ?? null,
    claudeApiDeltaMs: result ? result.apiMs - (result.apiMs >= previousApiMs ? previousApiMs : 0) : null,
    claudeTurnMs: result?.cliMs ?? null,
    afterClaudeResultMs: completed ? ms(result?.at, completed.observedAt) : null,
    streamEvents: events.map((event) => ({ ...event, observedAt: new Date(event.observedAt).toISOString() })),
    events: items.map(({ id, type, status, providerTurnId, startedAt, completedAt }) => ({ id, type, status, providerTurnId, startedAt, completedAt })),
    trace,
  };
}

async function runOnce(rpc, project, baseRef, n, idleBoxMiB) {
  const threadId = randomUUID();
  const record = { at: new Date().toISOString(), scenario: scenarioName, model, run: n, threadId, idleBoxMiB, steps: [] };
  const projection = () => rpc.call("orchestration.getThreadProjection", { threadId });
  let turns = 0;
  let previousApiMs = 0;
  const streamEvents = [];
  let unsubscribe;
  let resourcePath = "";
  let boxUID = "";
  const sampleResources = async (includeGuest) => {
    const sample = await resourceSample(resourcePath, includeGuest, boxUID);
    boxUID = sample?.boxUID ?? "";
    return sample;
  };
  try {
    for (const step of scenario.steps) {
      const before = await metrics();
      const resources = resourceTracker(await sampleResources(Boolean(step.send)));
      let sampling = false, sampleError, pendingSample;
      const sampler = setInterval(() => {
        if (sampling) return;
        sampling = true;
        pendingSample = (async () => {
          if (!resourcePath) resourcePath = (await projection()).thread.worktreePath ?? "";
          resources.add(await sampleResources(Boolean(step.send)));
        })().catch((err) => { sampleError = err; }).finally(() => { sampling = false; });
      }, 2000);
      const start = Date.now();
      const result = { step: step.send ? `turn ${turns + 1}` : `revert to before turn ${step.revert}` };
      let finished;
      try {
        if (step.send) {
          turns++;
          const messageId = randomUUID();
          if (turns === 1) {
            await rpc.call("orchestration.launchThread", {
              commandId: randomUUID(),
              creationSource: "web",
              threadId,
              projectId: project.id,
              title: `t3-run ${scenarioName} #${n}`,
              generateTitle: false,
              modelSelection: { instanceId: "claudeAgent", model },
              runtimeMode: "full-access",
              interactionMode: "default",
              workspaceStrategy: { type: "worktree", baseRef },
              initialMessage: { messageId, text: step.send, attachments: [] },
            });
            unsubscribe = rpc.subscribe("orchestration.subscribeThread", { threadId }, (item, observedAt) => {
              if (item.kind !== "event") return;
              const { type, occurredAt, runId, payload } = item.event;
              // Keep lifecycle metadata, never prompts, tool inputs or output.
              streamEvents.push({ type, occurredAt, observedAt, runId: runId ?? payload?.runId ?? (type.startsWith("run.") ? payload?.id : null),
                status: payload?.status, itemType: type.startsWith("turn-item.") ? payload?.type : undefined });
            });
          } else {
            await rpc.call("orchestration.dispatchCommand", {
              type: "message.dispatch",
              commandId: randomUUID(),
              createdBy: "user",
              creationSource: "web",
              threadId,
              messageId,
              text: step.send,
              attachments: [],
              dispatchMode: { type: "start_immediately" },
            });
          }
          const { run, p } = await waitFor(`turn ${turns}`, async () => {
            const p = await projection();
            const run = p.runs.find((r) => r.userMessageId === messageId);
            return run && done.has(run.status) ? { run, p } : null;
          });
          const answer = p.messages
            .filter((m) => m.role === "assistant" && m.runId === run.id && !m.streaming)
            .sort((a, b) => a.createdAt.localeCompare(b.createdAt))
            .at(-1)?.text ?? "";
          Object.assign(result, {
            status: run.status,
            queuedMs: ms(run.requestedAt, run.startedAt),
            runMs: ms(run.startedAt, run.completedAt),
            answer: answer.slice(0, 300),
            pass: run.status === "completed" && step.expect.test(answer),
          });
          finished = { p, run };
        } else {
          const p = await projection();
          // The checkpoint before turn N is the one turn N-1 left, or the
          // thread's baseline for turn 1: what T3's Revert picks.
          const target = p.checkpoints.find((c) =>
            step.revert === 1 ? c.appRunOrdinal === null && c.ordinalWithinScope === 0 : c.appRunOrdinal === step.revert - 1,
          );
          if (target?.status !== "ready") throw new Error(`no ready checkpoint before turn ${step.revert}`);
          const commandId = randomUUID();
          await rpc.call("orchestration.dispatchCommand", {
            type: "checkpoint.rollback",
            commandId,
            threadId,
            scopeId: target.scopeId,
            checkpointId: target.id,
            restoreFiles: true,
          });
          await waitFor("the revert", async () => {
            const p = await projection();
            if (p.thread.rollbackFailure?.requestId === commandId) throw new Error(`revert: ${p.thread.rollbackFailure.message}`);
            return p.runs.some((r) => r.ordinal >= step.revert && r.status === "rolled_back");
          });
          result.pass = true;
        }
        result.wallMs = Date.now() - start;
      } finally {
        clearInterval(sampler);
        await pendingSample;
      }
      if (sampleError) throw sampleError;
      if (!resourcePath) resourcePath = (await projection()).thread.worktreePath ?? "";
      resources.add(await sampleResources(Boolean(step.send)));
      result.resources = resources.result();
      if (idleBoxMiB != null && step.send) result.resources.box_incremental_peak_memory_mib =
        Math.max(0, result.resources.box_peak_memory_mib - idleBoxMiB);
      result.server = serverCost(before, await metrics());
      if (finished) {
        result.timing = await turnTiming(finished.p, finished.run, start, start + result.wallMs, streamEvents, previousApiMs);
        previousApiMs = result.timing.claudeApiTotalMs ?? previousApiMs;
      }
      record.steps.push(result);
      console.log(`  hardware: ${JSON.stringify(result.resources)}`);
      console.log(`  ${result.step}: ${result.pass ? "PASS" : "FAIL"} ${result.wallMs} ms ${JSON.stringify(result.server)}`);
      if (!result.pass) {
        console.log(`    answer: ${JSON.stringify(result.answer)}`);
        break;
      }
    }
  } catch (err) {
    record.error = String(err.message ?? err);
    console.log(`  error: ${record.error}`);
  } finally {
    unsubscribe?.();
    record.pass = !record.error && record.steps.length === scenario.steps.length && record.steps.every((s) => s.pass);
    if (!keep) await remove(rpc, project, threadId);
  }
  return record;
}

// remove deletes the thread and its worktree, as the app's "delete thread
// and worktree" does, and the worktree's Fern environment: Fern doesn't yet
// delete one when its thread goes, and many runs would fill the pool.
async function remove(rpc, project, threadId) {
  try {
    const p = await rpc.call("orchestration.getThreadProjection", { threadId });
    await rpc.call("orchestration.dispatchCommand", { type: "thread.delete", commandId: randomUUID(), threadId });
    const path = p.thread.worktreePath;
    if (path) {
      await bash(env("T3_RM_ENV_CMD"), path);
      await rpc.call("vcs.removeWorktree", { cwd: project.workspaceRoot, path, force: true });
    }
  } catch (err) {
    console.log(`  cleanup: ${err.message ?? err}`);
  }
}

function percentile(values, q) {
  const sorted = [...values].sort((a, b) => a - b);
  return sorted[Math.min(sorted.length - 1, Math.floor(q * sorted.length))];
}

const auth = await pair(env("T3_PAIR_URL"));
const rpc = await connect(auth);
try {
  const res = await fetch(new URL("/api/projects", auth.origin), { headers: { authorization: `Bearer ${auth.token}` } });
  const { projects } = await res.json();
  const project = projects.find((p) => p.title === scenario.project && !p.deletedAt);
  if (!project) throw new Error(`no project titled ${scenario.project}; run lab/t3-pair.sh`);
  const { refs } = await rpc.call("vcs.listRefs", { cwd: project.workspaceRoot, refKind: "local", limit: 50 });
  const baseRef = (refs.find((r) => r.current) ?? refs.find((r) => r.isDefault) ?? refs[0]).name;

  const idleBoxMiB = env("T3_BOX_BASELINE_CMD") ? Number((await bash(env("T3_BOX_BASELINE_CMD"), scenarioName)).trim()) : null;
  if (idleBoxMiB != null && (!Number.isFinite(idleBoxMiB) || idleBoxMiB <= 0)) throw new Error("invalid service-only box memory sample");
  console.log(`service-only box: ${idleBoxMiB ?? "unmeasured"} MiB (host anon + shmem)`);

  // The node's used memory (page cache not counted) before the runs and at
  // its highest while they ran, sampled every 5 s.
  const nodeMemory = async () => Number((await bash(env("T3_NODE_MEMORY_CMD"))).trim());
  const memoryBefore = await nodeMemory();
  let memoryPeak = memoryBefore;
  const sampler = setInterval(() => nodeMemory().then((m) => (memoryPeak = Math.max(memoryPeak, m)), () => {}), 5000);

  const records = [];
  let next = 1;
  const worker = async () => {
    for (let n = next++; n <= runs; n = next++) {
      console.log(`run ${n}/${runs}: ${scenarioName} on ${model}`);
      const record = await runOnce(rpc, project, baseRef, n, idleBoxMiB);
      record.parallel = parallel;
      appendFileSync(out, JSON.stringify(record) + "\n");
      records.push(record);
    }
  };
  const start = Date.now();
  await Promise.all(Array.from({ length: parallel }, worker));
  clearInterval(sampler);
  records.sort((a, b) => a.run - b.run);
  console.log(`\n${runs} runs, ${parallel} at a time: ${Math.round((Date.now() - start) / 1000)} s;` +
    ` node memory ${memoryBefore} MiB before, ${memoryPeak} MiB at the peak`);

  // Per step, across the runs that got that far: wall time, and the wake
  // time the server spent inside it.
  console.log(`${records.filter((r) => r.pass).length}/${runs} runs passed; results in ${out}`);
  const steps = scenario.steps.map((_, i) => records.map((r) => r.steps[i]).filter(Boolean));
  for (const done of steps) {
    if (done.length === 0) continue;
    const wall = done.map((s) => s.wallMs);
    const wakes = done.flatMap((s) => Object.entries(s.server).filter(([k]) => k.startsWith("wake_seconds")).map(([, v]) => v.ms / v.n));
    const wakeText = wakes.length ? `, wake p50 ${percentile(wakes, 0.5)} ms (${wakes.length})` : "";
    console.log(`${done[0].step}: wall p50 ${percentile(wall, 0.5)} ms, p95 ${percentile(wall, 0.95)} ms, max ${Math.max(...wall)} ms${wakeText}`);
  }
} finally {
  rpc.close();
}
