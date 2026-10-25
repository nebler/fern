// t3-run.mjs drives T3's server the way the T3 app does, so a demo can run
// many times without anyone clicking: it starts a New worktree thread on a
// sample project, sends the scenario's messages, reverts a turn, checks the
// answers and times every step. Run it through lab/t3-run.sh, which pairs
// with the server and passes T3_PAIR_URL, T3_METRICS_CMD and T3_RM_ENV_CMD.
//
// The threads show up in the T3 app under the server's connection while they
// run. T3's wire protocol is Effect RPC over a WebSocket, one JSON object per
// frame; see T3's packages/contracts/src/orchestrationV2.ts for the payloads.
import { execFileSync } from "node:child_process";
import { appendFileSync } from "node:fs";
import { randomUUID } from "node:crypto";

const env = (name, fallback) => process.env[name] ?? fallback;
const scenarioName = env("SCENARIO", "toy");
const runs = Number(env("RUNS", "1"));
const model = env("MODEL", "claude-haiku-5-5");
const keep = env("KEEP", "") !== "";
const out = env("OUT", "lab/t3-runs.jsonl");

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
  let next = 0;
  ws.onmessage = (event) => {
    const frame = JSON.parse(event.data);
    if (frame._tag !== "Exit") return; // only unary calls: no chunks to ack
    const call = pending.get(frame.requestId);
    pending.delete(frame.requestId);
    if (frame.exit._tag === "Success") call?.resolve(frame.exit.value);
    else call?.reject(new Error(`${call.tag}: ${JSON.stringify(frame.exit.cause)}`));
  };
  ws.onclose = () => {
    for (const call of pending.values()) call.reject(new Error(`${call.tag}: websocket closed`));
    pending.clear();
  };
  const ping = setInterval(() => ws.send(JSON.stringify({ _tag: "Ping" })), 5000);
  return {
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
function metrics() {
  const text = execFileSync("bash", ["-c", env("T3_METRICS_CMD")], { encoding: "utf8" });
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

async function runOnce(rpc, project, baseRef, n) {
  const threadId = randomUUID();
  const record = { at: new Date().toISOString(), scenario: scenarioName, model, run: n, threadId, steps: [] };
  const projection = () => rpc.call("orchestration.getThreadProjection", { threadId });
  let turns = 0;
  try {
    for (const step of scenario.steps) {
      const before = metrics();
      const start = Date.now();
      const result = { step: step.send ? `turn ${turns + 1}` : `revert to before turn ${step.revert}` };
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
      result.server = serverCost(before, metrics());
      record.steps.push(result);
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
      execFileSync("bash", ["-c", env("T3_RM_ENV_CMD"), "rm-env", path], { stdio: "inherit" });
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

  const records = [];
  for (let n = 1; n <= runs; n++) {
    console.log(`run ${n}/${runs}: ${scenarioName} on ${model}`);
    const record = await runOnce(rpc, project, baseRef, n);
    appendFileSync(out, JSON.stringify(record) + "\n");
    records.push(record);
  }

  // Per step, across the runs that got that far: wall time, and the wake
  // time the server spent inside it.
  console.log(`\n${records.filter((r) => r.pass).length}/${runs} runs passed; results in ${out}`);
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
