import { describe, expect, test } from "bun:test"
import { OSCredentialStore, type SubprocessRunner } from "../src/credentials.js"

const token = "s".repeat(43)

describe("OSCredentialStore", () => {
  test("uses fixed macOS argv and passes a credential only through stdin", async () => {
    const calls: Array<{ argv: readonly string[]; stdin?: string; env: Record<string, string | undefined> }> = []
    const run: SubprocessRunner = async (argv, options) => {
      calls.push({ argv, ...options })
      return { exitCode: 0, stdout: argv[1] === "find-generic-password" ? `${token}\n` : "" }
    }
    const previous = process.env.FERN_TOKEN
    process.env.FERN_TOKEN = token
    try {
      const store = new OSCredentialStore("https://fern.example/", { platform: "darwin", run })
      await store.set(token)
      expect(await store.get()).toBe(token)
      await store.delete()
    } finally {
      if (previous === undefined) delete process.env.FERN_TOKEN
      else process.env.FERN_TOKEN = previous
    }

    expect(calls[0].argv).toEqual([
      "/usr/bin/security",
      "add-generic-password",
      "-U",
      "-a",
      "https://fern.example/",
      "-s",
      "dev.fern.opencode",
      "-w",
    ])
    expect(calls[0].stdin).toBe(`${token}\n`)
    for (const call of calls) {
      expect(call.argv.join(" ")).not.toContain(token)
      expect(Object.values(call.env)).not.toContain(token)
    }
  })

  test("keys Linux Secret Service entries by canonical origin", async () => {
    const commands: string[][] = []
    const run: SubprocessRunner = async (argv) => {
      commands.push([...argv])
      return { exitCode: 0, stdout: "" }
    }
    const store = new OSCredentialStore("https://fern.example", { platform: "linux", run })
    await store.set(token)
    expect(commands[0]).toEqual([
      "secret-tool",
      "store",
      "--label=Fern OpenCode plugin",
      "service",
      "dev.fern.opencode",
      "origin",
      "https://fern.example/",
    ])
  })

  test("refuses unsupported durable storage", async () => {
    const store = new OSCredentialStore("https://fern.example", {
      platform: "win32",
      run: async () => ({ exitCode: 0, stdout: "" }),
    })
    await expect(store.available()).resolves.toBe(false)
    await expect(store.set(token)).rejects.toThrow("require macOS Keychain or Linux Secret Service")
  })

  test("distinguishes absence from operational failure and deletes idempotently", async () => {
    const macMissing = new OSCredentialStore("https://fern.example", {
      platform: "darwin",
      run: async () => ({ exitCode: 44, stdout: "", stderr: "item not found" }),
    })
    await expect(macMissing.available()).resolves.toBe(true)
    await expect(macMissing.get()).resolves.toBeUndefined()
    await expect(macMissing.delete()).resolves.toBeUndefined()

    const linuxCommands: string[][] = []
    const linuxMissing = new OSCredentialStore("https://fern.example", {
      platform: "linux",
      run: async (argv) => {
        linuxCommands.push([...argv])
        return { exitCode: 1, stdout: "", stderr: "" }
      },
    })
    await expect(linuxMissing.available()).resolves.toBe(true)
    await expect(linuxMissing.get()).resolves.toBeUndefined()
    await expect(linuxMissing.delete()).resolves.toBeUndefined()
    expect(linuxCommands.filter((argv) => argv[1] === "clear")).toHaveLength(0)

    const broken = new OSCredentialStore("https://fern.example", {
      platform: "linux",
      run: async () => ({ exitCode: 1, stdout: "", stderr: "secret service unavailable" }),
    })
    await expect(broken.available()).resolves.toBe(false)
    await expect(broken.get()).rejects.toThrow("read the Fern credential")
    await expect(broken.delete()).rejects.toThrow("read the Fern credential")
  })

  test("surfaces bounded subprocess timeout, cancellation, and overflow", async () => {
    for (const result of [{ timedOut: true }, { canceled: true }, { overflow: true }]) {
      const store = new OSCredentialStore("https://fern.example", {
        platform: "darwin",
        run: async () => ({ exitCode: -1, stdout: "", stderr: "", ...result }),
      })
      await expect(store.get()).rejects.toThrow(
        result.timedOut ? "timed out" : result.canceled ? "canceled" : "exceeded the allowed size",
      )
    }
  })
})
