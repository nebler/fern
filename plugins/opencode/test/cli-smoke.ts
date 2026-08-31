import { mkdir, mkdtemp, readdir, rm } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join } from "node:path"

const binary = process.env.OPENCODE_BIN || "opencode"
const directory = await mkdtemp(join(tmpdir(), "fern-opencode-cli-smoke-"))
try {
  const version = await run([binary, "--version"], directory)
  if (version.stdout.trim() !== "1.18.16") {
    throw new Error(`Installed CLI smoke requires OpenCode 1.18.16; found ${version.stdout.trim() || "unknown"}.`)
  }

  await run(["npm", "pack", "--pack-destination", directory], join(import.meta.dir, ".."), 30_000)
  const archive = (await readdir(directory)).find((name) => name.endsWith(".tgz"))
  if (!archive) throw new Error("Installed CLI smoke pack did not create an archive.")
  const packageRoot = join(directory, "package")
  await mkdir(packageRoot)
  await run(["tar", "-xzf", join(directory, archive), "--strip-components=1", "-C", packageRoot], directory)
  const project = join(directory, "project")
  await mkdir(project)
  const installed = await run([binary, "plugin", packageRoot], project, 60_000)
  if (!installed.stdout.includes("Detected tui target") || !installed.stdout.includes("Installed")) {
    throw new Error("OpenCode did not detect and install the packed plugin as a TUI target.")
  }
} finally {
  await rm(directory, { recursive: true, force: true })
}

async function run(argv: string[], cwd: string, timeout = 15_000) {
  const home = join(directory, "home")
  const env = {
    ...Object.fromEntries(Object.entries(process.env).filter(([key]) => !key.toUpperCase().startsWith("FERN_"))),
    HOME: home,
    XDG_CACHE_HOME: join(home, "cache"),
    XDG_CONFIG_HOME: join(home, "config"),
    XDG_DATA_HOME: join(home, "data"),
    XDG_STATE_HOME: join(home, "state"),
  }
  const child = Bun.spawn(argv, { cwd, env, stdout: "pipe", stderr: "pipe" })
  const timer = setTimeout(() => child.kill(), timeout)
  const [exitCode, stdout, stderr] = await Promise.all([
    child.exited,
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
  ]).finally(() => clearTimeout(timer))
  if (exitCode !== 0) throw new Error(`${argv.join(" ")} failed:\n${stdout}\n${stderr}`)
  return { stdout, stderr }
}
