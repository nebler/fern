// traces.mjs TIMES TRACES...: summarizes, as Markdown, the traces Jaeger
// kept of the operations lab/profile.sh timed. TIMES has one window per
// line, "start end" in seconds since the epoch; each TRACES file is Jaeger's
// /api/v3/traces answer (OTLP JSON, one or more {"result": ...} objects) for
// one service. For the daemon's traces that start in a window it prints
// every step's median and p95 time, by its path from the request, and the
// timeline of the median operation, with kubelet's spans in the same
// window beside it.
import { readFileSync } from "node:fs";

const [timesFile, ...traceFiles] = process.argv.slice(2);
const windows = readFileSync(timesFile, "utf8").trim().split("\n").map((l) => l.split(" ").map((s) => Number(s) * 1e6));

// The answer is a stream of JSON objects, which may share a line.
const objects = (text) => {
  const out = [];
  let depth = 0, from = -1, inString = false;
  for (let i = 0; i < text.length; i++) {
    const c = text[i];
    if (inString) {
      if (c === "\\") i++;
      else if (c === '"') inString = false;
    } else if (c === '"') inString = true;
    else if (c === "{") { if (depth++ === 0) from = i; }
    else if (c === "}" && --depth === 0) out.push(JSON.parse(text.slice(from, i + 1)));
  }
  return out;
};

// Every span, with its service, its parent and times in µs since the epoch.
const spans = [];
const us = (ns) => Number(BigInt(ns) / 1000n);
for (const file of traceFiles) {
  for (const answer of objects(readFileSync(file, "utf8"))) {
    for (const rs of answer.result?.resourceSpans ?? []) {
      const service = rs.resource?.attributes?.find((a) => a.key === "service.name")?.value?.stringValue ?? "?";
      for (const ss of rs.scopeSpans ?? []) {
        for (const s of ss.spans ?? []) {
          spans.push({ id: s.spanId, trace: s.traceId, name: s.name, parent: s.parentSpanId || undefined,
            start: us(s.startTimeUnixNano), end: us(s.endTimeUnixNano), service, error: s.status?.code === 2 });
        }
      }
    }
  }
}
// The same span can come in more than one answer.
const unique = new Map(spans.map((s) => [s.trace + s.id, s]));
spans.length = 0;
spans.push(...unique.values());
const byID = new Map(spans.map((s) => [s.trace + s.id, s]));
const children = new Map();
for (const s of spans) {
  if (s.parent === undefined) continue;
  const key = s.trace + s.parent;
  children.set(key, [...(children.get(key) ?? []), s]);
}
const kids = (s) => (children.get(s.trace + s.id) ?? []).sort((a, b) => a.start - b.start);
const isRoot = (s) => s.parent === undefined || !byID.has(s.trace + s.parent);

// The daemon's request in each window: its longest root span there.
const ops = windows.map(([from, to]) => spans
  .filter((s) => s.service === "fern-daemon" && isRoot(s) && s.start >= from - 50e3 && s.start <= to)
  .sort((a, b) => (b.end - b.start) - (a.end - a.start))[0]).filter(Boolean);
if (ops.length === 0) {
  console.log("No daemon traces in the windows: is profiling on (lab/profile.sh on)?");
  process.exit(0);
}

const ms = (us) => (us / 1000).toFixed(1);
const quantile = (values, q) => {
  const sorted = [...values].sort((a, b) => a - b);
  return sorted[Math.min(sorted.length - 1, Math.floor(q * sorted.length))];
};

// Steps by path: the same step of every operation, side by side.
const steps = new Map();
const walk = (s, path) => {
  const p = path ? `${path} › ${s.name}` : s.name;
  steps.set(p, [...(steps.get(p) ?? []), s.end - s.start]);
  for (const k of kids(s)) walk(k, p);
};
for (const op of ops) walk(op, "");
console.log(`## Steps (${ops.length} operations)\n`);
console.log("| step | n | p50 ms | p95 ms |\n|---|---|---|---|");
for (const [path, d] of steps) console.log(`| ${path} | ${d.length} | ${ms(quantile(d, 0.5))} | ${ms(quantile(d, 0.95))} |`);

// The median operation's timeline, and the node's spans in its time.
const median = [...ops].sort((a, b) => (a.end - a.start) - (b.end - b.start))[Math.floor(ops.length / 2)];
const lines = [];
const show = (s, depth) => {
  lines.push({ at: s.start, text: `${"  ".repeat(depth)}${s.name}${s.error ? " (error)" : ""}`, s });
  for (const k of kids(s)) show(k, depth + 1);
};
show(median, 0);
// Only what started in it: kubelet's watches run for minutes.
const others = spans.filter((s) => s.service !== "fern-daemon" && s.start >= median.start && s.start < median.end);
const depthOf = (s) => (isRoot(s) ? 0 : 1 + depthOf(byID.get(s.trace + s.parent)));
for (const s of others) lines.push({ at: s.start, text: `${"  ".repeat(depthOf(s))}[${s.service}] ${s.name}`, s });
lines.sort((a, b) => a.at - b.at);
console.log(`\n## Timeline of the median operation (${ms(median.end - median.start)} ms)\n`);
console.log("```\n   start      ms  span");
for (const { text, s } of lines) {
  console.log(`${ms(s.start - median.start).padStart(8)} ${ms(s.end - s.start).padStart(7)}  ${text}`);
}
console.log("```");
