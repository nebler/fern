import { mkdtemp, readdir, rm } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { pathToFileURL } from "node:url"
import { runProcess } from "../src/process.js"

const directory = await mkdtemp(join(tmpdir(), "fern-opencode-smoke-"))
try {
  const packed = await runProcess(["npm", "pack", "--pack-destination", directory], {
    cwd: join(import.meta.dir, ".."),
    timeoutMs: 30_000,
    outputLimit: 16_384,
  })
  if (packed.exitCode !== 0 || packed.timedOut || packed.overflow)
    throw new Error("Could not pack the plugin for smoke testing.")
  const archive = (await readdir(directory)).find((name) => name.endsWith(".tgz"))
  if (!archive) throw new Error("Plugin smoke pack did not create an archive.")
  const extracted = await runProcess(["tar", "-xzf", join(directory, archive), "-C", directory], {
    timeoutMs: 15_000,
    outputLimit: 4_096,
  })
  if (extracted.exitCode !== 0 || extracted.timedOut || extracted.overflow) {
    throw new Error("Could not extract the plugin smoke artifact.")
  }
  const module = await import(`${pathToFileURL(join(directory, "package", "src", "tui.ts")).href}?smoke=1`)
  if (module.default?.id !== "fern.opencode" || typeof module.default?.tui !== "function") {
    throw new Error("Packed plugin did not expose the Fern TUI module.")
  }
} finally {
  await rm(directory, { recursive: true, force: true })
}
