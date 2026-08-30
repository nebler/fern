import { describe, expect, test } from "bun:test"
import { ORIGIN_KV_KEY, configuredOrigin, persistOrigin } from "../src/origin.js"

describe("Fern origin persistence", () => {
  test("stores only a canonical root HTTPS origin", () => {
    const values = new Map<string, unknown>()
    const kv = {
      get(key: string) {
        return values.get(key)
      },
      set(key: string, value: unknown) {
        values.set(key, value)
      },
    }
    expect(persistOrigin(kv, "  https://Fern.Example  ").href).toBe("https://fern.example/")
    expect(values.get(ORIGIN_KV_KEY)).toBe("https://fern.example/")
    expect(configuredOrigin(kv, {})?.href).toBe("https://fern.example/")
    expect([...values.values()]).not.toContain(expect.stringContaining("Bearer"))
  })

  test("rejects unsafe onboarding origins and ignores corrupt persisted values", () => {
    const kv = { get: () => "https://fern.example/path", set: () => {} }
    expect(configuredOrigin(kv, {})).toBeUndefined()
    expect(() => persistOrigin(kv, "http://fern.example")).toThrow("root HTTPS origin")
    expect(() => persistOrigin(kv, "https://user:password@fern.example")).toThrow("root HTTPS origin")
  })
})
