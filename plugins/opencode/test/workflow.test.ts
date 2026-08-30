import { afterEach, describe, expect, test } from "bun:test"
import { mkdtemp, rm } from "node:fs/promises"
import { join } from "node:path"
import { tmpdir } from "node:os"
import type { FernClient } from "../src/client.js"
import {
  CreateRunLatch,
  INSTRUCTION_MAX_LENGTH,
  InMemoryPendingSubmissionStore,
  createRunWorkflow,
  stopRunWorkflow,
} from "../src/workflow.js"

const directories: string[] = []

afterEach(async () => {
  await Promise.all(directories.splice(0).map((directory) => rm(directory, { recursive: true, force: true })))
})

describe("createRunWorkflow", () => {
  test("confirms clean exact Git state before creating with a caller key", async () => {
    const directory = await repository()
    const calls: string[] = []
    const client = {
      async createRun(input) {
        calls.push("create")
        expect(input.idempotencyKey).toBe("workflow-key")
        expect(input.git.remote).toBe("https://github.com/fern/example")
        return "run_123"
      },
    } satisfies Pick<FernClient, "createRun">

    const runID = await createRunWorkflow({
      client,
      pending: new InMemoryPendingSubmissionStore(),
      directory,
      host: "fern.example",
      instruction: "  Fix the race  ",
      confirm: async (input) => {
        calls.push("confirm")
        expect(input.git.dirty).toBe(false)
        expect(input.instruction).toBe("Fix the race")
        return true
      },
      idempotencyKey: () => "workflow-key",
    })

    expect(runID).toBe("run_123")
    expect(calls).toEqual(["confirm", "create"])
  })

  test("does not create when confirmation is canceled", async () => {
    const directory = await repository()
    const client = { createRun: async () => "unexpected" } satisfies Pick<FernClient, "createRun">
    expect(
      await createRunWorkflow({
        client,
        pending: new InMemoryPendingSubmissionStore(),
        directory,
        host: "fern.example",
        instruction: "Fix it",
        confirm: async () => false,
      }),
    ).toBeUndefined()
  })

  test("rejects a dirty tree before confirmation or POST", async () => {
    const directory = await repository()
    await Bun.write(join(directory, "dirty.txt"), "dirty")
    let confirmed = false
    const client = { createRun: async () => "unexpected" } satisfies Pick<FernClient, "createRun">
    await expect(
      createRunWorkflow({
        client,
        pending: new InMemoryPendingSubmissionStore(),
        directory,
        host: "fern.example",
        instruction: "Fix it",
        confirm: async () => {
          confirmed = true
          return true
        },
      }),
    ).rejects.toThrow("working tree is dirty")
    expect(confirmed).toBe(false)
  })

  test("does not create if the repository changes after confirmation", async () => {
    const directory = await repository()
    let created = false
    const client = {
      createRun: async () => {
        created = true
        return "unexpected"
      },
    } satisfies Pick<FernClient, "createRun">
    await expect(
      createRunWorkflow({
        client,
        pending: new InMemoryPendingSubmissionStore(),
        directory,
        host: "fern.example",
        instruction: "Fix it",
        confirm: async () => {
          await Bun.write(join(directory, "dirty.txt"), "changed\n")
          return true
        },
      }),
    ).rejects.toThrow("changed after confirmation")
    expect(created).toBe(false)
  })

  test("reuses a pending idempotency key after response loss and clears it on commit", async () => {
    const directory = await repository()
    const pending = new InMemoryPendingSubmissionStore()
    const keys: string[] = []
    let attempts = 0
    let generated = 0
    const client = {
      async createRun(input) {
        keys.push(input.idempotencyKey)
        attempts++
        if (attempts === 1) throw new Error("response lost")
        return "run_123"
      },
    } satisfies Pick<FernClient, "createRun">
    const input = {
      client,
      pending,
      directory,
      host: "fern.example",
      instruction: "Fix it",
      confirm: async () => true,
      idempotencyKey: () => (generated++ === 0 ? "stable-key" : "new-key"),
    }

    await expect(createRunWorkflow(input)).rejects.toThrow("response lost")
    expect(await createRunWorkflow(input)).toBe("run_123")
    expect(keys).toEqual(["stable-key", "stable-key"])
    expect(await createRunWorkflow(input)).toBe("run_123")
    expect(keys).toEqual(["stable-key", "stable-key", "new-key"])
  })

  test("bounds instructions and refuses explicit server before inspecting local Git", async () => {
    const client = { createRun: async () => "unexpected" } satisfies Pick<FernClient, "createRun">
    const base = {
      client,
      pending: new InMemoryPendingSubmissionStore(),
      directory: "/does/not/exist",
      host: "fern.example",
      confirm: async () => true,
    }
    await expect(
      createRunWorkflow({ ...base, instruction: "x", argv: ["opencode2", "--server=https://remote.example"] }),
    ).rejects.toThrow("local OpenCode")

    const directory = await repository()
    await expect(
      createRunWorkflow({ ...base, directory, instruction: "x".repeat(INSTRUCTION_MAX_LENGTH + 1), argv: [] }),
    ).rejects.toThrow(`${INSTRUCTION_MAX_LENGTH}`)
  })

  test("prevents concurrent creates", async () => {
    const latch = new CreateRunLatch()
    let release = () => {}
    const first = latch.run(() => new Promise<void>((resolve) => (release = resolve)))
    await expect(latch.run(async () => undefined)).rejects.toThrow("already in progress")
    release()
    await first
    await expect(latch.run(async () => "ready")).resolves.toBe("ready")
  })

  test("validates a stop run ID before confirmation", async () => {
    let confirmed = false
    const client = {
      requireRunID() {
        throw new Error("invalid ID")
      },
      async stopRun() {
        return "canceling" as const
      },
    } satisfies Pick<FernClient, "requireRunID" | "stopRun">
    await expect(
      stopRunWorkflow({
        client,
        runID: "x",
        confirm: async () => {
          confirmed = true
          return true
        },
      }),
    ).rejects.toThrow("invalid ID")
    expect(confirmed).toBe(false)
  })
})

async function repository() {
  const directory = await mkdtemp(join(tmpdir(), "fern-opencode-workflow-"))
  directories.push(directory)
  await git(directory, "init", "--initial-branch=main")
  await Bun.write(join(directory, "README.md"), "fixture\n")
  await git(directory, "add", "README.md")
  await git(directory, "-c", "user.name=Fern Test", "-c", "user.email=fern@example.com", "commit", "-m", "fixture")
  await git(directory, "remote", "add", "origin", "https://github.com/fern/example.git")
  return directory
}

async function git(directory: string, ...args: string[]) {
  const process = Bun.spawn(["git", "-C", directory, ...args], {
    stdout: "ignore",
    stderr: "pipe",
  })
  const [exit, stderr] = await Promise.all([process.exited, Bun.readableStreamToText(process.stderr)])
  if (exit !== 0) throw new Error(stderr)
}
