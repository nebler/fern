// Piped into Node in T3 and in its box by t3-start.sh. No user message is
// sent: initialize exercises Claude's SDK startup without a model request.
import { execFileSync, spawn } from "node:child_process";
import { once } from "node:events";
import { request } from "node:http";

const binary = process.argv[2];
const side = process.argv[3];
const cwd = process.env.BENCH_WORKSPACE ?? process.cwd();
const report = (command, samples) => {
  samples.sort((a, b) => a - b);
  console.log(JSON.stringify({ side, command, n: samples.length,
    p50Ms: samples[Math.floor((samples.length - 1) / 2)], p95Ms: samples[Math.ceil(samples.length * 0.95) - 1], samplesMs: samples }));
};
const cases = [["claude --version", binary, ["--version"]]];
if (side === "local") cases.push(
  ["fern which", "fern", ["which", cwd]],
  ["fern exec true", "fern", ["exec", "--here", "--", "true"]],
  ["fern exec claude --version", "fern", ["exec", "--here", "--", "claude", "--version"]],
  ["fern-claude --version", "claude", ["--version"]],
);
const samples = cases.map(() => []);
for (let n = 0; n < 10; n++) {
  for (let i = 0; i < cases.length; i++) {
    const [, command, args] = cases[i];
    const start = performance.now();
    execFileSync(command, args, { cwd, stdio: "pipe" });
    samples[i].push(performance.now() - start);
  }
}
cases.forEach(([label], i) => report(label, samples[i]));

if (side === "local") {
  const id = execFileSync("fern", ["which", cwd], { encoding: "utf8" }).trim();
  const cli = [], socket = [];
  for (let n = 0; n < 20; n++) {
    for (const kind of n % 2 ? ["socket", "cli"] : ["cli", "socket"]) {
      const start = performance.now();
      if (kind === "cli") {
        execFileSync("fern", ["snapshot", "--replace", id, `cli-${n}`], { stdio: "pipe" });
      } else {
        await new Promise((resolve, reject) => {
          const req = request({ socketPath: "/run/fern/fern.sock", path: `/envs/${id}/snapshots`, method: "POST",
            headers: { "content-type": "application/json" }, timeout: 10_000 }, (res) => {
            res.resume();
            res.on("error", reject);
            res.on("end", () => res.statusCode === 204 ? resolve() : reject(new Error(`snapshot: ${res.statusCode}`)));
          });
          req.on("error", reject);
          req.on("timeout", () => req.destroy(new Error("snapshot timeout")));
          req.end(JSON.stringify({ id: `socket-${n}`, replace: true }));
        });
      }
      (kind === "cli" ? cli : socket).push(performance.now() - start);
    }
  }
  report("snapshot: CLI", cli);
  report("snapshot: socket", socket);
}

const variants = [
  ["default", {}],
  ["no JIT", { BUN_JSC_useJIT: "false" }],
  ["one GC marker", { BUN_JSC_numberOfGCMarkers: "1" }],
  ["no telemetry", { DISABLE_TELEMETRY: "1", DISABLE_ERROR_REPORTING: "1" }],
];
const startup = variants.map(() => []);
const startupErrors = variants.map(() => null);
for (let n = 0; n < 10; n++) {
  for (let i = 0; i < variants.length; i++) {
    if (startupErrors[i]) continue;
    const start = performance.now();
    const child = spawn(binary, ["-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose"], {
      cwd, env: { ...process.env, ...variants[i][1] }, stdio: ["pipe", "pipe", "ignore"],
    });
    const exited = once(child, "exit");
    const timeout = setTimeout(() => child.kill("SIGKILL"), 5_000);
    try {
      const output = new Promise((resolve, reject) => {
        let text = "";
        child.once("error", reject);
        child.once("exit", () => reject(new Error("Claude exited before initialize answered")));
        child.stdout.on("data", (chunk) => {
          text += chunk;
          if (text.includes("\n")) resolve(JSON.parse(text.split("\n")[0]));
        });
      });
      child.stdin.write(JSON.stringify({ type: "control_request", request_id: "bench", request: { subtype: "initialize" } }) + "\n");
      const event = await output;
      if (event.type !== "control_response" || event.response?.subtype !== "success") {
        throw new Error(`initialize returned ${event.type}/${event.response?.subtype}`);
      }
      startup[i].push(performance.now() - start);
    } catch (error) {
      startupErrors[i] = error.message;
    } finally {
      child.kill("SIGTERM");
      await exited;
      clearTimeout(timeout);
    }
  }
}
variants.forEach(([label], i) => {
  report(`initialize: ${label}`, startup[i]);
  if (startupErrors[i]) console.log(JSON.stringify({ side, command: `initialize: ${label}`, error: startupErrors[i] }));
});
