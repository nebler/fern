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

export const RESULT_PHASES = ["seal_requested", "ready"] as const
export type ResultPhase = (typeof RESULT_PHASES)[number]

export type SealRunResult = {
  runID: string
  state: "canceling" | "result_ready"
  resultPhase: ResultPhase
  sealRequestID: string
  committed: true
}

export type RunResult = {
  runID: string
  state: "result_ready"
  result: {
    id: string
    outcome: "changed" | "no_changes"
    repository: string
    baseOID: string
    resultCommit: string
    treeOID: string
    manifestEntries: number
    manifestSHA256: string
  }
  artifact: {
    id: string
    format: "git_bundle_v1"
    sha256: string
    bundleSHA256: string
    bundleSize: number
    manifestSHA256: string
  }
  retention: {
    verified: boolean
    reconstructable: boolean
  }
  cleanup: {
    complete: boolean
  }
}

export const PLUGIN_SCOPES = ["run:create", "run:read", "run:stop", "run:open", "run:result"] as const

export type AuthorizationStart = {
  authorizationID: string
  deviceCode: string
  userCode: string
  verificationURI: URL
  verificationURIComplete: URL
  expiresIn: number
  interval: number
}

export type AuthorizationPoll =
  | { status: "pending"; retryAfter?: number }
  | { status: "denied" | "expired" }
  | { status: "approved"; accessToken: string; credentialID: string; expiresIn: number }

export type FernClientErrorKind = "authentication" | "http" | "protocol" | "transport"

export class FernClientError extends Error {
  readonly status: number | undefined
  readonly kind: FernClientErrorKind
  readonly fernBearerChallenge: boolean

  constructor(
    message: string,
    status?: number,
    kind: FernClientErrorKind = status === undefined ? "protocol" : "http",
    fernBearerChallenge = false,
  ) {
    super(message)
    this.name = "FernClientError"
    this.status = status
    this.kind = kind
    this.fernBearerChallenge = fernBearerChallenge
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

  async startAuthorization(): Promise<AuthorizationStart> {
    const response = await this.#send("/fern/api/plugin-auth/start", { method: "POST", body: "{}" }, false)
    if (response.status !== 201) throw await this.#statusError(response)
    const value = await this.#decode(response, Math.min(this.#responseLimit, 16_384))
    if (
      !hasExactKeys(value, [
        "authorization_id",
        "device_code",
        "user_code",
        "verification_uri",
        "verification_uri_complete",
        "expires_in",
        "interval",
        "scopes",
      ])
    ) {
      throw new FernClientError("Fern returned an invalid authorization start response.")
    }
    if (
      !isCanonicalID(value.authorization_id, "pa_") ||
      !isCredential(value.device_code) ||
      !isUserCode(value.user_code) ||
      !isIntegerIn(value.expires_in, 1, 600) ||
      !isIntegerIn(value.interval, 1, value.expires_in) ||
      !hasFixedScopes(value.scopes)
    ) {
      throw new FernClientError("Fern returned an invalid authorization start response.")
    }
    const verificationURI = safeAuthorizationURL(value.verification_uri, this.#endpoint, false)
    const complete = safeAuthorizationURL(value.verification_uri_complete, this.#endpoint, true)
    if (
      verificationURI.pathname !== "/fern/plugin-auth/authorize" ||
      verificationURI.search ||
      complete.pathname !== verificationURI.pathname ||
      complete.searchParams.size !== 2 ||
      complete.searchParams.get("id") !== value.authorization_id ||
      complete.searchParams.get("code") !== value.user_code
    ) {
      throw new FernClientError("Fern returned an unsafe authorization URL.")
    }
    return {
      authorizationID: value.authorization_id,
      deviceCode: value.device_code,
      userCode: value.user_code,
      verificationURI,
      verificationURIComplete: complete,
      expiresIn: value.expires_in,
      interval: value.interval,
    }
  }

  async pollAuthorization(deviceCode: string, signal?: AbortSignal): Promise<AuthorizationPoll> {
    if (!isCredential(deviceCode)) throw new FernClientError("Fern authorization has an invalid device code.")
    const response = await this.#send(
      "/fern/api/plugin-auth/poll",
      { method: "POST", body: JSON.stringify({ device_code: deviceCode }) },
      false,
      signal,
    )
    if (response.status === 429) {
      try {
        return { status: "pending", retryAfter: parseRetryAfter(response) }
      } finally {
        await response.body?.cancel()
      }
    }
    if (![200, 202, 403, 410].includes(response.status)) throw await this.#statusError(response)
    const value = await this.#decode(response, Math.min(this.#responseLimit, 16_384))
    if (response.status !== 200) {
      const expected = response.status === 202 ? "pending" : response.status === 403 ? "denied" : "expired"
      if (!hasExactKeys(value, ["status"]) || value.status !== expected) {
        throw new FernClientError("Fern returned an invalid authorization poll response.")
      }
      return { status: expected }
    }
    if (
      !hasExactKeys(value, ["access_token", "token_type", "credential_id", "expires_in", "scopes"]) ||
      value.access_token !== deviceCode ||
      value.token_type !== "Bearer" ||
      !isCanonicalID(value.credential_id, "pc_") ||
      !isIntegerIn(value.expires_in, 1, 90 * 24 * 60 * 60) ||
      !hasFixedScopes(value.scopes)
    ) {
      throw new FernClientError("Fern returned an invalid approved authorization response.")
    }
    return {
      status: "approved",
      accessToken: value.access_token,
      credentialID: value.credential_id,
      expiresIn: value.expires_in,
    }
  }

  async revokeSelf() {
    const response = await this.#send("/fern/api/plugin-auth/self/revoke", { method: "POST" }, true)
    if (response.status === 401) {
      const identified = isFernBearerChallenge(response)
      try {
        await response.body?.cancel()
      } catch {}
      throw new FernClientError(
        identified ? "The Fern plugin credential is already ineffective." : "Fern rejected credential revocation.",
        401,
        identified ? "authentication" : "http",
        identified,
      )
    }
    if (response.status !== 204) throw await this.#statusError(response)
    const body = await readBounded(response, 1)
    if (body !== "") throw new FernClientError("Fern returned an invalid revocation response.")
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

  async sealRun(runID: string, idempotencyKey: string): Promise<SealRunResult> {
    const expected = this.requireRunID(runID)
    const response = await this.#request(`/fern/api/runs/${encodeURIComponent(expected)}/seal`, {
      method: "POST",
      headers: { "Idempotency-Key": idempotencyKey },
      body: "{}",
    })
    if (
      !isRecord(response) ||
      response.run_id !== expected ||
      response.committed !== true ||
      !isFernID(response.seal_request_id, "slr_")
    ) {
      throw new FernClientError("Fern returned an invalid seal response.")
    }
    const state = response.state
    const resultPhase = response.result_phase
    if (
      (state !== "canceling" && state !== "result_ready") ||
      !isResultPhase(resultPhase) ||
      (state === "canceling" && resultPhase !== "seal_requested") ||
      (state === "result_ready" && resultPhase !== "ready")
    ) {
      throw new FernClientError("Fern returned an invalid seal response.")
    }
    return {
      runID: expected,
      state,
      resultPhase,
      sealRequestID: response.seal_request_id,
      committed: true,
    }
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
    if (
      !isRecord(response) ||
      response.run_id !== expected ||
      response.state !== "result_ready" ||
      !isRecord(response.result) ||
      !isRecord(response.artifact) ||
      !isRecord(response.retention) ||
      !isRecord(response.cleanup) ||
      hasLocatorField(response)
    ) {
      throw new FernClientError("Fern returned an invalid run result.")
    }
    const result = parseResultAuthority(response.result)
    const artifact = parseResultArtifact(response.artifact)
    if (artifact.manifestSHA256 !== result.manifestSHA256) {
      throw new FernClientError("Fern returned an inconsistent result manifest.")
    }
    const retention = parseBooleanProjection(response.retention, ["verified", "reconstructable"], "retention")
    const cleanup = parseBooleanProjection(response.cleanup, ["complete"], "cleanup")
    return {
      runID: expected,
      state: "result_ready",
      result,
      artifact,
      retention: { verified: retention.verified!, reconstructable: retention.reconstructable! },
      cleanup: { complete: cleanup.complete! },
    }
  }

  async #request(path: string, init: RequestInit = {}) {
    const response = await this.#send(path, init, true)
    if (response.status === 401 && isFernBearerChallenge(response)) {
      const error = new FernClientError(
        "Fern rejected its plugin credential. Reconnect on the next explicit Fern action.",
        401,
        "authentication",
        true,
      )
      try {
        await response.body?.cancel()
      } catch {
        // Continue to local cleanup even if the response stream cannot be canceled.
      }
      try {
        await this.#credentials.delete()
      } catch {
        // Preserve the authoritative authentication error if local cleanup fails.
      }
      throw error
    }
    return this.#decode(response)
  }

  async #send(path: string, init: RequestInit, authenticated: boolean, requestSignal?: AbortSignal) {
    const credential = authenticated ? await this.#credentials.get() : undefined
    if (authenticated && !credential)
      throw new FernClientError("Fern is not connected. Reopen a Fern action to connect.")
    const controller = new AbortController()
    let timedOut = false
    const timeout = setTimeout(() => {
      timedOut = true
      controller.abort()
    }, this.#timeoutMs)
    const cancel = () => controller.abort()
    this.#signal?.addEventListener("abort", cancel, { once: true })
    requestSignal?.addEventListener("abort", cancel, { once: true })
    if (this.#signal?.aborted || requestSignal?.aborted) controller.abort()

    let cleaned = false
    const cleanup = () => {
      if (cleaned) return
      cleaned = true
      clearTimeout(timeout)
      this.#signal?.removeEventListener("abort", cancel)
      requestSignal?.removeEventListener("abort", cancel)
    }
    const requestError = () => {
      if (timedOut)
        return new FernClientError(`Fern request timed out after ${this.#timeoutMs}ms.`, undefined, "transport")
      if (this.#signal?.aborted)
        return new FernClientError("Fern request canceled because the plugin is shutting down.", undefined, "transport")
      if (requestSignal?.aborted) return new FernClientError("Fern authorization was canceled.", undefined, "transport")
      return new FernClientError("Fern request failed before a valid response was received.", undefined, "transport")
    }

    try {
      const response = await this.#fetch(new URL(path, this.#endpoint), {
        ...init,
        redirect: "error",
        signal: controller.signal,
        headers: {
          ...init.headers,
          Accept: "application/json",
          ...(credential ? { Authorization: `Bearer ${credential}` } : {}),
          ...(init.body ? { "Content-Type": "application/json" } : {}),
        },
      })
      if (!response.body) {
        cleanup()
        return response
      }
      const reader = response.body.getReader()
      const body = new ReadableStream<Uint8Array>({
        async pull(stream) {
          try {
            const item = await reader.read()
            if (item.done) {
              cleanup()
              stream.close()
            } else {
              stream.enqueue(item.value)
            }
          } catch {
            cleanup()
            stream.error(requestError())
          }
        },
        async cancel(reason) {
          cleanup()
          await reader.cancel(reason)
        },
      })
      return new Response(body, { status: response.status, statusText: response.statusText, headers: response.headers })
    } catch (error) {
      cleanup()
      if (error instanceof FernClientError) throw error
      throw requestError()
    }
  }

  async #statusError(response: Response) {
    try {
      await readBounded(response, Math.min(this.#responseLimit, 16_384))
    } catch {}
    return new FernClientError(`Fern request failed with HTTP ${response.status}.`, response.status)
  }

  async #decode(response: Response, limit = this.#responseLimit) {
    const statusError = () => new FernClientError(`Fern request failed with HTTP ${response.status}.`, response.status)
    const contentType = response.headers.get("content-type")?.split(";", 1)[0].trim().toLowerCase()
    if (contentType !== "application/json" && !contentType?.endsWith("+json")) {
      if (!response.ok) throw statusError()
      throw new FernClientError("Fern returned a non-JSON response.")
    }

    const length = Number(response.headers.get("content-length"))
    if (Number.isFinite(length) && length > limit) {
      if (!response.ok) throw statusError()
      throw new FernClientError("Fern response exceeded the allowed size.")
    }

    let text: string
    try {
      text = await readBounded(response, limit)
    } catch (error) {
      if (!response.ok) throw statusError()
      throw error
    }
    const parsed = parseJSON(text)
    if (!response.ok) {
      if (!parsed.ok) throw statusError()
      throw statusError()
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

function parseResultAuthority(value: Record<string, unknown>): RunResult["result"] {
  if (
    !isFernID(value.id, "res_") ||
    (value.outcome !== "changed" && value.outcome !== "no_changes") ||
    !isCanonicalGitHubRepository(value.repository) ||
    !isSHA1(value.base_oid) ||
    !isSHA1(value.result_commit) ||
    !isSHA1(value.tree_oid) ||
    !isIntegerIn(value.manifest_entries, 0, 1_000_000) ||
    !isSHA256(value.manifest_sha256)
  ) {
    throw new FernClientError("Fern returned invalid immutable result metadata.")
  }
  if (
    (value.outcome === "no_changes" && (value.result_commit !== value.base_oid || value.manifest_entries !== 0)) ||
    (value.outcome === "changed" && (value.result_commit === value.base_oid || value.manifest_entries === 0))
  ) {
    throw new FernClientError("Fern returned inconsistent immutable result metadata.")
  }
  return {
    id: value.id,
    outcome: value.outcome,
    repository: value.repository,
    baseOID: value.base_oid,
    resultCommit: value.result_commit,
    treeOID: value.tree_oid,
    manifestEntries: value.manifest_entries,
    manifestSHA256: value.manifest_sha256,
  }
}

function parseResultArtifact(value: Record<string, unknown>): RunResult["artifact"] {
  if (
    !isFernID(value.id, "art_") ||
    value.format !== "git_bundle_v1" ||
    !isSHA256(value.sha256) ||
    !isSHA256(value.bundle_sha256) ||
    !isIntegerIn(value.bundle_size, 0, 10 * 1024 * 1024 * 1024) ||
    !isSHA256(value.manifest_sha256)
  ) {
    throw new FernClientError("Fern returned invalid retained artifact metadata.")
  }
  return {
    id: value.id,
    format: value.format,
    sha256: value.sha256,
    bundleSHA256: value.bundle_sha256,
    bundleSize: value.bundle_size,
    manifestSHA256: value.manifest_sha256,
  }
}

function parseBooleanProjection<Value extends string>(
  value: Record<string, unknown>,
  keys: readonly Value[],
  name: string,
) {
  for (const key of keys) {
    if (typeof value[key] !== "boolean") throw new FernClientError(`Fern returned invalid ${name} metadata.`)
  }
  return value as Record<Value, boolean>
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

function isFernID(value: unknown, prefix: string): value is string {
  return (
    typeof value === "string" &&
    new RegExp(`^${prefix}[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).test(value)
  )
}

function isResultPhase(value: unknown): value is ResultPhase {
  return typeof value === "string" && RESULT_PHASES.includes(value as ResultPhase)
}

function isSHA1(value: unknown): value is string {
  return typeof value === "string" && /^[0-9a-f]{40}$/.test(value)
}

function isSHA256(value: unknown): value is string {
  return typeof value === "string" && /^[0-9a-f]{64}$/.test(value)
}

function isCanonicalGitHubRepository(value: unknown): value is string {
  if (typeof value !== "string") return false
  const url = URL.parse(value)
  if (
    !url ||
    url.protocol !== "https:" ||
    url.hostname !== "github.com" ||
    url.port ||
    url.username ||
    url.password ||
    url.search ||
    url.hash
  ) {
    return false
  }
  const match = /^\/([A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?)\/([A-Za-z0-9._-]{1,100})$/.exec(url.pathname)
  return Boolean(match && !match[2].endsWith(".git") && url.href === value)
}

function hasLocatorField(value: unknown): boolean {
  if (Array.isArray(value)) return value.some(hasLocatorField)
  if (!isRecord(value)) return false
  return Object.entries(value).some(
    ([key, item]) => /(^|_)(url|uri|path|locator|storage_key)($|_)/.test(key) || hasLocatorField(item),
  )
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value)
}

function hasExactKeys(value: unknown, keys: readonly string[]): value is Record<string, unknown> {
  if (!isRecord(value)) return false
  const actual = Object.keys(value).sort()
  return actual.length === keys.length && actual.every((key, index) => key === [...keys].sort()[index])
}

function hasFixedScopes(value: unknown) {
  return (
    Array.isArray(value) &&
    value.length === PLUGIN_SCOPES.length &&
    value.every((scope, i) => scope === PLUGIN_SCOPES[i])
  )
}

function isIntegerIn(value: unknown, minimum: number, maximum: number): value is number {
  return typeof value === "number" && Number.isSafeInteger(value) && value >= minimum && value <= maximum
}

function isCredential(value: unknown): value is string {
  return typeof value === "string" && canonicalBase64URL(value, 32)
}

function isCanonicalID(value: unknown, prefix: string): value is string {
  return typeof value === "string" && value.startsWith(prefix) && canonicalBase64URL(value.slice(prefix.length), 16)
}

function isUserCode(value: unknown): value is string {
  return typeof value === "string" && /^[A-Z2-7]{5}-[A-Z2-7]{5}-[A-Z2-7]{2}[ACEGIKMOQSUWY246]$/.test(value)
}

function canonicalBase64URL(value: string, bytes: number) {
  if (!/^[A-Za-z0-9_-]+$/.test(value)) return false
  const decoded = Buffer.from(value, "base64url")
  return decoded.byteLength === bytes && decoded.toString("base64url") === value
}

function safeAuthorizationURL(value: unknown, endpoint: URL, query: boolean) {
  if (typeof value !== "string" || value.length > 2_048)
    throw new FernClientError("Fern returned an unsafe authorization URL.")
  const url = URL.parse(value)
  const localHTTP = endpoint.protocol === "http:" && isLocalhost(endpoint.hostname) && url?.protocol === "http:"
  if (
    !url ||
    (url.protocol !== "https:" && !localHTTP) ||
    url.origin !== endpoint.origin ||
    url.username ||
    url.password ||
    url.hash ||
    (!query && url.search)
  ) {
    throw new FernClientError("Fern returned an unsafe authorization URL.")
  }
  return url
}

function isFernBearerChallenge(response: Response) {
  return /^Bearer realm="fern-plugin"$/i.test(response.headers.get("www-authenticate")?.trim() ?? "")
}

function parseRetryAfter(response: Response) {
  const value = response.headers.get("retry-after")
  if (!value || !/^[1-9][0-9]{0,2}$/.test(value)) {
    throw new FernClientError("Fern returned an invalid authorization retry interval.", response.status)
  }
  const seconds = Number(value)
  if (seconds > 300)
    throw new FernClientError("Fern returned an invalid authorization retry interval.", response.status)
  return seconds
}
