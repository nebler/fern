import { mkdir, mkdtemp, readFile, readdir, rm } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join } from "node:path"

const directory = await mkdtemp(join(tmpdir(), "fern-opencode-smoke-"))
try {
  await run(["npm", "pack", "--pack-destination", directory], join(import.meta.dir, ".."), 30_000)
  const archive = (await readdir(directory)).find((name) => name.endsWith(".tgz"))
  if (!archive) throw new Error("Plugin smoke pack did not create an archive.")

  const listing = (await run(["tar", "-tzf", join(directory, archive)], undefined, 15_000)).stdout
    .trim()
    .split("\n")
    .sort()
  const required = [
    "package/LICENSE",
    "package/README.md",
    "package/dist/index.d.ts",
    "package/dist/index.js",
    "package/dist/tui.d.ts",
    "package/dist/tui.js",
    "package/package.json",
  ]
  for (const path of required) {
    if (!listing.includes(path)) throw new Error(`Packed plugin is missing ${path}.`)
  }
  if (listing.some((path) => /^package\/(src|test|node_modules)\//.test(path) || /tsconfig|bun\.lock/.test(path))) {
    throw new Error("Packed plugin contains source, tests, dependencies, or internal build files.")
  }
  if (listing.some((path) => !/^package\/(dist\/[^/]+\.(js|d\.ts)|LICENSE|README\.md|package\.json)$/.test(path))) {
    throw new Error("Packed plugin contains an unexpected file.")
  }

  const packageRoot = join(directory, "consumer", "node_modules", "@fern", "opencode")
  await mkdir(packageRoot, { recursive: true })
  await run(["tar", "-xzf", join(directory, archive), "--strip-components=1", "-C", packageRoot], undefined, 15_000)
  const manifest = JSON.parse(await readFile(join(packageRoot, "package.json"), "utf8"))
  if (
    manifest.name !== "@fern/opencode" ||
    manifest.version !== "0.1.0" ||
    manifest.license !== "MIT" ||
    manifest.exports?.["."]?.import !== "./dist/index.js" ||
    manifest.exports?.["."]?.types !== "./dist/index.d.ts"
  ) {
    throw new Error("Packed plugin metadata or compiled root export is invalid.")
  }

  await run(
    [
      "bun",
      "-e",
      'const root = await import("@fern/opencode"); const tui = await import("@fern/opencode/tui"); if (root.default?.id !== "fern.opencode" || typeof root.default?.tui !== "function" || tui.default !== root.default) process.exit(1)',
    ],
    join(directory, "consumer"),
    15_000,
  )
} finally {
  await rm(directory, { recursive: true, force: true })
}

async function run(argv: string[], cwd?: string, timeout = 15_000) {
  const process = Bun.spawn(argv, { cwd, stdout: "pipe", stderr: "pipe" })
  const timer = setTimeout(() => process.kill(), timeout)
  const [exitCode, stdout, stderr] = await Promise.all([
    process.exited,
    new Response(process.stdout).text(),
    new Response(process.stderr).text(),
  ]).finally(() => clearTimeout(timer))
  if (exitCode !== 0) throw new Error(`${argv.join(" ")} failed: ${stderr || stdout}`)
  return { stdout, stderr }
}
