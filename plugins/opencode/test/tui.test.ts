import { describe, expect, test } from "bun:test"
import type { TuiPluginApi, TuiPluginMeta } from "@opencode-ai/plugin/tui"
import type { FernClient } from "../src/client.js"
import { InMemoryCredentialStore, OSCredentialStore } from "../src/credentials.js"
import plugin, { authorize, createTuiPlugin, openBrowser } from "../src/tui.js"

describe("TUI compatibility", () => {
  test("refuses an unsupported OpenCode runtime before registering commands", async () => {
    const toasts: unknown[] = []
    let registered = false
    const api = {
      app: { version: "1.18.17" },
      ui: { toast: (input: unknown) => toasts.push(input) },
      keymap: {
        registerLayer() {
          registered = true
        },
      },
    } as unknown as TuiPluginApi

    await plugin.tui(api, undefined, {} as TuiPluginMeta)
    expect(registered).toBe(false)
    expect(toasts).toEqual([
      {
        variant: "error",
        title: "Fern plugin disabled",
        message: "Requires OpenCode 1.18.16; found 1.18.17.",
        duration: 10_000,
      },
    ])
  })

  test("registers the complete command set on the exact runtime", async () => {
    let commands: Array<{ name: string }> = []
    const api = {
      app: { version: "1.18.16" },
      lifecycle: { signal: new AbortController().signal },
      kv: { get: () => ({}), set: () => {} },
      keymap: {
        registerLayer(layer: { commands: Array<{ name: string }> }) {
          commands = layer.commands
        },
      },
    } as unknown as TuiPluginApi

    await plugin.tui(api, undefined, {} as TuiPluginMeta)
    expect(commands.map((command) => command.name)).toEqual([
      "fern",
      "fern.run",
      "fern.runs",
      "fern.stop",
      "fern.seal",
      "fern.result",
      "fern.disconnect",
    ])
  })

  test("dispatches run, list, stop, seal, and result actions", async () => {
    const fixture = actionAPI()
    const calls: string[] = []
    const client = {
      requireRunID(value: string) {
        calls.push(`validate:${value}`)
        return value
      },
      async listRuns() {
        calls.push("list")
        return [{ id: "run_list", state: "working" as const, repository: "https://github.com/fern/repo" }]
      },
      async getResult(runID: string) {
        calls.push(`result:${runID}`)
        return readyResult(runID)
      },
      async sealRun(runID: string, key: string) {
        calls.push(`seal:${runID}:${key}`)
        return {
          runID,
          state: "canceling" as const,
          resultPhase: "seal_requested" as const,
          sealRequestID: "slr_0198d34d-7007-7007-8007-000000000007",
          committed: true as const,
        }
      },
    } as unknown as FernClient
    const credentials = new InMemoryCredentialStore("secret")
    const actionPlugin = createTuiPlugin({
      connection: async (_api, onboard) => {
        calls.push(`connect:${onboard}`)
        return { client, endpoint: new URL("https://fern.example/"), credentials }
      },
      createRun: async (input) => {
        calls.push(`run:${input.instruction}:${input.directory}`)
        const approved = await input.confirm({
          host: input.host,
          instruction: input.instruction,
          profile: "test-profile",
          git: {
            root: input.directory,
            remote: "https://github.com/fern/repo",
            head: "a".repeat(40),
            branch: "main",
            dirty: false,
          },
        })
        calls.push(`run-confirmed:${approved}`)
        return approved ? "run_created" : undefined
      },
      stopRun: async (input) => {
        calls.push(`stop:${input.runID}`)
        const approved = await input.confirm(input.runID)
        calls.push(`stop-confirmed:${approved}`)
        return approved ? "canceling" : undefined
      },
    })

    await actionPlugin.tui(fixture.api, undefined, {} as TuiPluginMeta)

    await fixture.command("fern.run").run()
    fixture.prompt().onConfirm?.("Fix action routing")
    await waitFor(() => fixture.currentTitle() === "Run on Fern?")
    fixture.confirm().onConfirm?.()
    await waitFor(() => calls.includes("run-confirmed:true"))

    await fixture.command("fern.runs").run()
    expect(fixture.current()).toMatchObject({ title: "Fern runs" })

    await fixture.command("fern.stop").run()
    fixture.prompt().onConfirm?.("run_stop")
    await waitFor(() => fixture.currentTitle() === "Stop Fern run run_stop?")
    fixture.confirm().onConfirm?.()
    await waitFor(() => calls.includes("stop-confirmed:true"))

    await fixture.command("fern.seal").run()
    fixture.prompt().onConfirm?.("run_seal")
    await waitFor(() => fixture.currentTitle() === "Seal Fern run run_seal?", "seal confirmation")
    expect(fixture.confirm().message).toBe(
      "This is irreversible. The exact remote writer will stop, and its Git work will be retained as an immutable result.",
    )
    fixture.confirm().onConfirm?.()
    await Bun.sleep(2)
    expect(fixture.toasts().filter((toast) => toast.variant === "error")).toEqual([])
    await waitFor(() => calls.some((call) => call.startsWith("seal:run_seal:")), "seal request")

    await fixture.command("fern.seal").run()
    fixture.prompt().onConfirm?.("run_canceled")
    await waitFor(() => fixture.currentTitle() === "Seal Fern run run_canceled?", "seal cancellation")
    fixture.confirm().onCancel?.()
    await Bun.sleep(2)

    await fixture.command("fern.result").run()
    fixture.prompt().onConfirm?.("run_result")
    await waitFor(() => calls.includes("result:run_result"))
    expect(fixture.current()).toMatchObject({ title: "Fern result run_result" })
    expect(fixture.alert().message).toContain(`Commit: ${"b".repeat(40)}`)
    expect(fixture.alert().message).toContain("Artifact: art_0198d34d-7008-7008-8008-000000000008 (git_bundle_v1)")
    expect(fixture.alert().message).toContain("Retention verified: yes")
    expect(fixture.alert().message).toContain("Cleanup complete: no")
    expect(fixture.alert().message).not.toContain("URL")
    fixture.alert().onConfirm?.()

    expect(calls).toContain("run:Fix action routing:/worktree")
    expect(calls).toContain("run-confirmed:true")
    expect(calls).toContain("list")
    expect(calls).toContain("stop:run_stop")
    expect(calls).toContain("stop-confirmed:true")
    expect(calls.filter((call) => call.startsWith("seal:"))).toHaveLength(1)
    expect(calls.find((call) => call.startsWith("seal:run_seal:"))?.split(":")[2]).toMatch(
      /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/,
    )
    expect(calls).toContain("result:run_result")
  })
})

test("browser launch uses cancellation, timeout, and output bounds", async () => {
  let options: { timeoutMs?: number; outputLimit?: number } | undefined
  await expect(
    openBrowser(new URL("https://fern.example/authorize"), undefined, async (_argv, input) => {
      options = input
      return { exitCode: 0, stdout: "", stderr: "", timedOut: false, canceled: false, overflow: false }
    }),
  ).resolves.toBeUndefined()
  expect(options).toMatchObject({ timeoutMs: 15_000, outputLimit: 1_024 })

  await expect(
    openBrowser(new URL("https://fern.example/authorize"), undefined, async () => ({
      exitCode: -1,
      stdout: "",
      stderr: "",
      timedOut: true,
      canceled: false,
      overflow: false,
    })),
  ).rejects.toThrow("timed out")
})

describe("TUI authorization lifecycle", () => {
  test("preflights durable persistence before activating a grant", async () => {
    const fixture = authorizationAPI()
    let starts = 0
    const client = {
      async startAuthorization() {
        starts++
        return authorizationStart()
      },
    } as FernClient
    const credentials = new OSCredentialStore("https://fern.example", {
      platform: "linux",
      run: async () => ({ exitCode: 1, stdout: "", stderr: "service unavailable" }),
    })
    await expect(authorize(fixture.api, new URL("https://fern.example/"), client, credentials)).rejects.toThrow(
      "available macOS Keychain or Linux Secret Service",
    )
    expect(starts).toBe(0)
    expect(fixture.clears()).toBe(0)
  })

  test("revokes an approved grant when persistence fails and reports transport ambiguity", async () => {
    const fixture = authorizationAPI()
    let deleted = 0
    let revokedToken: string | undefined
    const credentials = {
      get: async () => undefined,
      set: async () => {
        throw new Error("save failed")
      },
      delete: async () => {
        deleted++
      },
    }
    await expect(
      authorize(fixture.api, new URL("https://fern.example/"), authorizationClient(), credentials, {
        openBrowser: async () => {},
        poll: async () => approvedAuthorization(),
        revokeApproved: async (token) => {
          revokedToken = token
          return "ambiguous_transport"
        },
      }),
    ).rejects.toThrow("response was lost")
    expect(revokedToken).toBe("A".repeat(43))
    expect(deleted).toBe(1)
    expect(fixture.clears()).toBe(1)
  })

  test("checks cancellation before saving and closes its owned dialog on errors", async () => {
    const fixture = authorizationAPI()
    let saves = 0
    let revokes = 0
    const credentials = new InMemoryCredentialStore()
    credentials.set = async () => {
      saves++
    }
    await expect(
      authorize(fixture.api, new URL("https://fern.example/"), authorizationClient(), credentials, {
        openBrowser: async () => {},
        poll: async () => {
          fixture.controller.abort()
          return approvedAuthorization()
        },
        revokeApproved: async () => {
          revokes++
          return "revoked"
        },
      }),
    ).rejects.toThrow("could not be persisted")
    expect(saves).toBe(0)
    expect(revokes).toBe(1)
    expect(fixture.clears()).toBe(1)
  })
})

function authorizationAPI() {
  const controller = new AbortController()
  let clearCount = 0
  let onClose: (() => void) | undefined
  const dialog = {
    clear() {
      clearCount++
      onClose?.()
      onClose = undefined
    },
    replace(render: () => unknown, close?: () => void) {
      onClose?.()
      onClose = close
      render()
    },
    setSize() {},
    size: "xlarge",
    depth: 1,
    open: true,
  }
  const api = {
    lifecycle: { signal: controller.signal },
    ui: {
      dialog,
      DialogAlert: (props: unknown) => props,
      toast: () => {},
    },
  } as unknown as TuiPluginApi
  return { api, controller, clears: () => clearCount }
}

function actionAPI() {
  type Command = { name: string; run: () => void | Promise<void> }
  type Prompt = { title: string; onConfirm?: (value: string) => void }
  type Alert = { title: string; message: string; onConfirm?: () => void }
  type Confirm = { title: string; message: string; onConfirm?: () => void; onCancel?: () => void }
  let commands: Command[] = []
  let rendered: unknown
  const toasts: Array<{ variant?: string; message?: string }> = []
  const dialog = {
    clear() {
      rendered = undefined
    },
    replace(render: () => unknown) {
      rendered = render()
    },
    setSize() {},
    size: "xlarge" as const,
    depth: 1,
    open: true,
  }
  const api = {
    app: { version: "1.18.16" },
    lifecycle: { signal: new AbortController().signal },
    state: { path: { worktree: "/worktree", directory: "/directory" } },
    kv: { get: (_key: string, fallback: unknown) => fallback, set: () => {} },
    keymap: {
      registerLayer(layer: { commands: Command[] }) {
        commands = layer.commands
      },
    },
    ui: {
      dialog,
      DialogPrompt: (props: unknown) => props,
      DialogSelect: (props: unknown) => props,
      DialogAlert: (props: unknown) => props,
      DialogConfirm: (props: unknown) => props,
      toast: (toast: { variant?: string; message?: string }) => toasts.push(toast),
    },
  } as unknown as TuiPluginApi
  return {
    api,
    command(name: string) {
      const found = commands.find((command) => command.name === name)
      if (!found) throw new Error(`Missing command ${name}`)
      return found
    },
    current: () => rendered,
    currentTitle: () => (rendered as { title?: string } | undefined)?.title,
    prompt: () => rendered as Prompt,
    alert: () => rendered as Alert,
    confirm: () => rendered as Confirm,
    toasts: () => toasts,
  }
}

function readyResult(runID: string) {
  return {
    runID,
    state: "result_ready" as const,
    result: {
      id: "res_0198d34d-7007-7007-8007-000000000007",
      outcome: "changed" as const,
      repository: "https://github.com/fern/repo",
      baseOID: "a".repeat(40),
      resultCommit: "b".repeat(40),
      treeOID: "c".repeat(40),
      manifestEntries: 2,
      manifestSHA256: "d".repeat(64),
    },
    artifact: {
      id: "art_0198d34d-7008-7008-8008-000000000008",
      format: "git_bundle_v1" as const,
      sha256: "e".repeat(64),
      bundleSHA256: "f".repeat(64),
      bundleSize: 1234,
      manifestSHA256: "d".repeat(64),
    },
    retention: { verified: true, reconstructable: true },
    cleanup: { complete: false },
  }
}

async function waitFor(predicate: () => boolean, action = "TUI action") {
  for (let attempt = 0; attempt < 100; attempt++) {
    if (predicate()) return
    await Bun.sleep(1)
  }
  throw new Error(`Timed out waiting for ${action}.`)
}

function authorizationClient() {
  return { startAuthorization: async () => authorizationStart() } as FernClient
}

function authorizationStart() {
  return {
    authorizationID: `pa_${"A".repeat(22)}`,
    deviceCode: "A".repeat(43),
    userCode: "ABCDE-FGHIJ-KLM",
    verificationURI: new URL("https://fern.example/fern/plugin-auth/authorize"),
    verificationURIComplete: new URL(
      `https://fern.example/fern/plugin-auth/authorize?code=ABCDE-FGHIJ-KLM&id=pa_${"A".repeat(22)}`,
    ),
    expiresIn: 600,
    interval: 5,
  }
}

function approvedAuthorization() {
  return {
    status: "approved" as const,
    accessToken: "A".repeat(43),
    credentialID: `pc_${"A".repeat(22)}`,
    expiresIn: 60,
  }
}
