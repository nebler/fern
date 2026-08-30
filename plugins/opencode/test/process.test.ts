import { expect, test } from "bun:test"
import { subprocessEnvironment } from "../src/process.js"

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
