import { FernClient, FernClientError, type AuthorizationStart } from "./client.js"
import type { CredentialStore } from "./credentials.js"

export type PollOptions = {
  signal?: AbortSignal
  now?: () => number
  sleep?: (milliseconds: number, signal?: AbortSignal) => Promise<void>
}

export type RevokeOutcome = "revoked" | "already_ineffective" | "definitive_failure" | "ambiguous_transport"

export class DisconnectError extends Error {
  readonly outcome: RevokeOutcome

  constructor(outcome: RevokeOutcome) {
    super(`Fern revocation was ${outcome.replaceAll("_", " ")}, but the local credential could not be removed.`)
    this.name = "DisconnectError"
    this.outcome = outcome
  }
}

export class OnboardingLatch {
  #active: Promise<void> | undefined

  run(action: () => Promise<void>) {
    if (!this.#active) {
      this.#active = action().finally(() => {
        this.#active = undefined
      })
    }
    return this.#active
  }
}

export async function pollForAuthorization(
  client: Pick<FernClient, "pollAuthorization">,
  started: AuthorizationStart,
  options: PollOptions = {},
) {
  const now = options.now ?? Date.now
  const sleep = options.sleep ?? abortableSleep
  const expiresAt = now() + started.expiresIn * 1_000
  let delay = started.interval * 1_000

  while (true) {
    if (options.signal?.aborted) throw canceled()
    const remaining = expiresAt - now()
    if (remaining <= 0) throw new FernClientError("Fern authorization expired before it was approved.")
    await sleep(Math.min(delay, remaining), options.signal)
    if (now() >= expiresAt) throw new FernClientError("Fern authorization expired before it was approved.")

    const result = await client.pollAuthorization(started.deviceCode, options.signal)
    if (result.status === "approved") return result
    if (result.status === "denied") throw new FernClientError("Fern authorization was denied.")
    if (result.status === "expired") throw new FernClientError("Fern authorization expired before it was approved.")
    if (result.status !== "pending") throw new FernClientError("Fern returned an unknown authorization state.")
    delay = Math.max(started.interval, result.retryAfter ?? 0) * 1_000
  }
}

export async function disconnectFern(
  client: Pick<FernClient, "revokeSelf">,
  credentials: CredentialStore,
): Promise<RevokeOutcome> {
  const outcome = await revokeFern(client)
  try {
    await credentials.delete()
  } catch {
    throw new DisconnectError(outcome)
  }
  return outcome
}

export async function revokeFern(client: Pick<FernClient, "revokeSelf">): Promise<RevokeOutcome> {
  try {
    await client.revokeSelf()
    return "revoked"
  } catch (error) {
    if (error instanceof FernClientError) {
      if (error.kind === "authentication" && error.fernBearerChallenge) return "already_ineffective"
      if (error.kind !== "transport") return "definitive_failure"
    }
    return "ambiguous_transport"
  }
}

function abortableSleep(milliseconds: number, signal?: AbortSignal) {
  return new Promise<void>((resolve, reject) => {
    if (signal?.aborted) {
      reject(canceled())
      return
    }
    const timeout = setTimeout(finish, milliseconds)
    const cancel = () => {
      clearTimeout(timeout)
      signal?.removeEventListener("abort", cancel)
      reject(canceled())
    }
    function finish() {
      signal?.removeEventListener("abort", cancel)
      resolve()
    }
    signal?.addEventListener("abort", cancel, { once: true })
  })
}

function canceled() {
  return new FernClientError("Fern authorization was canceled.")
}
