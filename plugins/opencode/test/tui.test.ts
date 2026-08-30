import { describe, expect, test } from "bun:test"
import type { TuiPluginApi, TuiPluginMeta } from "@opencode-ai/plugin/tui"
import plugin from "../src/tui.js"

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
