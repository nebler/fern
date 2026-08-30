import { describe, expect, test } from "bun:test"
import type { TuiPluginApi, TuiPluginMeta } from "@opencode-ai/plugin/tui"
import type { FernClient } from "../src/client.js"
import { InMemoryCredentialStore, OSCredentialStore } from "../src/credentials.js"
import plugin, { authorize, openBrowser } from "../src/tui.js"

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
      "fern.open",
      "fern.stop",
      "fern.result",
      "fern.disconnect",
    ])
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
