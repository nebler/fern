import { expect, test } from "bun:test"
import { runProcess, subprocessEnvironment } from "../src/process.js"

test("subprocess environment excludes Fern credentials case-insensitively", () => {
  const previous = process.env.FERN_TOKEN
  const previousLower = process.env.fern_token
  process.env.FERN_TOKEN = "secret"
  process.env.fern_token = "also-secret"
  try {
    const environment = subprocessEnvironment()
    expect(environment.FERN_TOKEN).toBeUndefined()
    expect(environment.fern_token).toBeUndefined()
  } finally {
    if (previous === undefined) delete process.env.FERN_TOKEN
    else process.env.FERN_TOKEN = previous
    if (previousLower === undefined) delete process.env.fern_token
    else process.env.fern_token = previousLower
  }
})

test("runProcess bounds output and enforces timeout and cancellation", async () => {
  const overflow = await runProcess([process.execPath, "-e", 'process.stdout.write("x".repeat(10000))'], {
    outputLimit: 32,
  })
  expect(overflow.overflow).toBe(true)
  expect(overflow.stdout.length).toBeLessThanOrEqual(32)

  const timedOut = await runProcess([process.execPath, "-e", "await Bun.sleep(10000)"], { timeoutMs: 5 })
  expect(timedOut.timedOut).toBe(true)

  const controller = new AbortController()
  const canceled = runProcess([process.execPath, "-e", "await Bun.sleep(10000)"], {
    signal: controller.signal,
  })
  controller.abort()
  expect((await canceled).canceled).toBe(true)
})
