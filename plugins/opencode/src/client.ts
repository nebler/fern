import type { CredentialStore } from "./credentials.js"
import type { GitContext } from "./git.js"

export const RUN_STATES = [
  "queued",
  "setting_up",
  "working",
  "needs_you",
  "canceling",
  "uncertain",
  "result_ready",
  "failed",
  "cleanup_required",
] as const
export type RunState = (typeof RUN_STATES)[number]

export type CreateRunInput = {
  instruction: string
  profile: string
  git: GitContext
  idempotencyKey: string
}

export type Run = {
  id: string
  state: RunState
  repository?: string
  head?: string
  branch?: string | null
}

export type RunResult = {
  runID: string
  state: RunState
  summary?: string
  resultCommit?: string
  url?: string
}

export class FernClientError extends Error {
  readonly status: number | undefined

  constructor(message: string, status?: number) {
    super(message)
    this.name = "FernClientError"
    this.status = status
  }
}

export class FernClient {
  readonly #endpoint: URL
  readonly #credentials: CredentialStore
  readonly #fetch: typeof fetch
  readonly #timeoutMs: number
  readonly #signal: AbortSignal | undefined
  readonly #responseLimit: number

  constructor(
    endpoint: URL,
    credentials: CredentialStore,
    options: {
      fetch?: typeof fetch
      timeoutMs?: number
      signal?: AbortSignal
      responseLimit?: number
    } = {},
  ) {
    this.#endpoint = parseEndpoint(endpoint.href)
    this.#credentials = credentials
    this.#fetch = options.fetch ?? fetch
    this.#timeoutMs = options.timeoutMs ?? 15_000
    this.#signal = options.signal
    this.#responseLimit = options.responseLimit ?? 1024 * 1024
    if (!Number.isSafeInteger(this.#timeoutMs) || this.#timeoutMs <= 0) {
      throw new FernClientError("Fern request timeout must be a positive integer.")
    }
    if (!Number.isSafeInteger(this.#responseLimit) || this.#responseLimit <= 0) {
      throw new FernClientError("Fern response limit must be a positive integer.")
    }
  }

  requireRunID(value: string) {
    if (!isID(value)) throw new FernClientError("Enter a valid Fern run ID.")
    return value
  }

  async createRun(input: CreateRunInput) {
    const response = await this.#request("/fern/api/runs", {
      method: "POST",
      headers: { "Idempotency-Key": input.idempotencyKey },
      body: JSON.stringify({
        repository: input.git.remote,
        base_oid: input.git.head,
        branch: input.git.branch,
        instruction: input.instruction,
        profile: input.profile,
      }),
    })
    if (!isRecord(response) || response.committed !== true || !isID(response.run_id)) {
      throw new FernClientError("Fern did not return a committed run ID.")
    }
    return response.run_id
  }

  async listRuns() {
    const response = await this.#request("/fern/api/runs")
    if (!isRecord(response) || !Array.isArray(response.runs))
      throw new FernClientError("Fern returned an invalid run list.")
    return response.runs.map(parseRun)
  }

  async getRun(runID: string) {
    const expected = this.requireRunID(runID)
    const run = parseRun(await this.#request(`/fern/api/runs/${encodeURIComponent(expected)}`))
    if (run.id !== expected) throw new FernClientError("Fern returned a run with the wrong identity.")
    return run
  }

  async stopRun(runID: string, idempotencyKey: string) {
    const expected = this.requireRunID(runID)
    const response = await this.#request(`/fern/api/runs/${encodeURIComponent(expected)}/stop`, {
      method: "POST",
      headers: { "Idempotency-Key": idempotencyKey },
      body: "{}",
    })
    if (!isRecord(response) || response.run_id !== expected) {
      throw new FernClientError("Fern returned an invalid stop response.")
    }
    return parseState(response.state)
  }

  async resolveOpen(runID: string, idempotencyKey: string) {
    const expected = this.requireRunID(runID)
    const response = await this.#request(`/fern/api/runs/${encodeURIComponent(expected)}/open`, {
      method: "POST",
      headers: { "Idempotency-Key": idempotencyKey },
      body: "{}",
    })
    if (!isRecord(response) || response.run_id !== expected || typeof response.url !== "string") {
      throw new FernClientError("Fern returned an invalid open capability.")
    }
    const url = URL.parse(response.url)
    const localHTTP = url?.protocol === "http:" && isLocalhost(url.hostname)
    if (
      !url ||
      (url.protocol !== "https:" && !localHTTP) ||
      url.username ||
      url.password ||
      url.hostname.toLowerCase() !== this.#endpoint.hostname.toLowerCase()
    ) {
      throw new FernClientError("Fern returned an unsafe open capability.")
    }
    return url
  }

  async getResult(runID: string): Promise<RunResult> {
    const expected = this.requireRunID(runID)
    const response = await this.#request(`/fern/api/runs/${encodeURIComponent(expected)}/result`)
    if (!isRecord(response) || response.run_id !== expected)
      throw new FernClientError("Fern returned an invalid run result.")
    return {
      runID: expected,
      state: parseState(response.state),
      summary: optionalString(response, "summary", 32_000),
      resultCommit: optionalOID(response, "result_commit"),
      url: optionalURL(response, "url"),
    }
  }

  async #request(path: string, init: RequestInit = {}) {
    const credential = await this.#credentials.get()
    if (!credential)
      throw new FernClientError("Fern is not connected. Onboarding is not wired; set FERN_TOKEN for this process.")

    const controller = new AbortController()
    let timedOut = false
    const timeout = setTimeout(() => {
      timedOut = true
      controller.abort()
    }, this.#timeoutMs)
    const cancel = () => controller.abort()
    this.#signal?.addEventListener("abort", cancel, { once: true })
    if (this.#signal?.aborted) controller.abort()

    try {
      const response = await this.#fetch(new URL(path, this.#endpoint), {
        ...init,
        redirect: "error",
        signal: controller.signal,
        headers: {
          ...init.headers,
          Accept: "application/json",
          Authorization: `Bearer ${credential}`,
          ...(init.body ? { "Content-Type": "application/json" } : {}),
        },
      })
      return await this.#decode(response)
    } catch (error) {
      if (error instanceof FernClientError) throw error
      if (timedOut) throw new FernClientError(`Fern request timed out after ${this.#timeoutMs}ms.`)
      if (this.#signal?.aborted) throw new FernClientError("Fern request canceled because the plugin is shutting down.")
      throw new FernClientError("Fern request failed before a valid response was received.")
    } finally {
      clearTimeout(timeout)
      this.#signal?.removeEventListener("abort", cancel)
    }
  }

  async #decode(response: Response) {
    const statusError = () => new FernClientError(`Fern request failed with HTTP ${response.status}.`, response.status)
    const contentType = response.headers.get("content-type")?.split(";", 1)[0].trim().toLowerCase()
    if (contentType !== "application/json" && !contentType?.endsWith("+json")) {
      if (!response.ok) throw statusError()
      throw new FernClientError("Fern returned a non-JSON response.")
    }

    const length = Number(response.headers.get("content-length"))
    if (Number.isFinite(length) && length > this.#responseLimit) {
      if (!response.ok) throw statusError()
      throw new FernClientError("Fern response exceeded the allowed size.")
    }

    let text: string
    try {
      text = await readBounded(response, this.#responseLimit)
    } catch (error) {
      if (!response.ok) throw statusError()
      throw error
    }
    const parsed = parseJSON(text)
    if (!response.ok) {
      if (!parsed.ok) throw statusError()
      const message = isRecord(parsed.value) && typeof parsed.value.error === "string" ? parsed.value.error : undefined
      throw new FernClientError(message || statusError().message, response.status)
    }
    if (!parsed.ok) throw new FernClientError("Fern returned invalid JSON.")
    return parsed.value
  }
}

export function parseEndpoint(input: string) {
  const endpoint = URL.parse(input)
  const localHTTP = endpoint?.protocol === "http:" && isLocalhost(endpoint.hostname)
  if (
    !endpoint ||
    (endpoint.protocol !== "https:" && !localHTTP) ||
    endpoint.username ||
    endpoint.password ||
    endpoint.pathname !== "/" ||
    endpoint.search ||
    endpoint.hash
  ) {
    throw new FernClientError(
      "FERN_ENDPOINT must be a root HTTPS origin (root HTTP is allowed only for localhost development).",
    )
  }
  return endpoint
}

function parseRun(value: unknown): Run {
  if (!isRecord(value) || !isID(value.id)) throw new FernClientError("Fern returned an invalid run.")
  return {
    id: value.id,
    state: parseState(value.state),
    repository: optionalRepository(value, "repository"),
    head: optionalOID(value, "head"),
    branch: optionalNullableString(value, "branch", 255),
  }
}

function parseState(value: unknown): RunState {
  if (typeof value !== "string" || !RUN_STATES.includes(value as RunState)) {
    throw new FernClientError("Fern returned an unknown run state.")
  }
  return value as RunState
}

function optionalString(record: Record<string, unknown>, key: string, max = 4_096) {
  if (!(key in record)) return undefined
  if (typeof record[key] !== "string" || record[key].length > max) {
    throw new FernClientError(`Fern returned an invalid ${key} field.`)
  }
  return record[key]
}

function optionalNullableString(record: Record<string, unknown>, key: string, max?: number) {
  if (!(key in record)) return undefined
  if (record[key] === null) return null
  const value = optionalString(record, key, max)
  if (!value || /[\u0000-\u001f\u007f]/.test(value)) throw new FernClientError(`Fern returned an invalid ${key} field.`)
  return value
}

function optionalOID(record: Record<string, unknown>, key: string) {
  const value = optionalString(record, key)
  if (value !== undefined && !/^[0-9a-f]{40,64}$/.test(value)) {
    throw new FernClientError(`Fern returned an invalid ${key} field.`)
  }
  return value
}

function optionalRepository(record: Record<string, unknown>, key: string) {
  const value = optionalString(record, key)
  if (value === undefined) return undefined
  const url = URL.parse(value)
  if (
    !url ||
    url.protocol !== "https:" ||
    !url.hostname ||
    url.username ||
    url.password ||
    url.search ||
    url.hash ||
    url.pathname === "/"
  ) {
    throw new FernClientError(`Fern returned an invalid ${key} field.`)
  }
  return value
}

function optionalURL(record: Record<string, unknown>, key: string) {
  const value = optionalString(record, key)
  if (value === undefined) return undefined
  const url = URL.parse(value)
  const localHTTP = url?.protocol === "http:" && isLocalhost(url.hostname)
  if (!url || (url.protocol !== "https:" && !localHTTP) || url.username || url.password) {
    throw new FernClientError(`Fern returned an invalid ${key} field.`)
  }
  return value
}

async function readBounded(response: Response, limit: number) {
  if (!response.body) return ""
  const reader = response.body.getReader()
  const decoder = new TextDecoder()
  let total = 0
  let text = ""
  while (true) {
    const item = await reader.read()
    if (item.done) return text + decoder.decode()
    total += item.value.byteLength
    if (total > limit) {
      await reader.cancel()
      throw new FernClientError("Fern response exceeded the allowed size.")
    }
    text += decoder.decode(item.value, { stream: true })
  }
}

function parseJSON(value: string): { ok: true; value: unknown } | { ok: false } {
  try {
    return { ok: true, value: JSON.parse(value) }
  } catch {
    return { ok: false }
  }
}

function isLocalhost(hostname: string) {
  return ["127.0.0.1", "localhost", "::1", "[::1]"].includes(hostname.toLowerCase())
}

function isID(value: unknown): value is string {
  return typeof value === "string" && /^[A-Za-z0-9][A-Za-z0-9._-]{2,127}$/.test(value)
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value)
}
