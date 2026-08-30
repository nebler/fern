import { FernClientError, parseEndpoint } from "./client.js"

export const ORIGIN_KV_KEY = "fern.origin"

export interface OriginKV {
  get(key: string, fallback?: unknown): unknown
  set(key: string, value: unknown): void
}

export function configuredOrigin(kv: OriginKV, environment = process.env): URL | undefined {
  const fromEnvironment = environment.FERN_ENDPOINT
  if (fromEnvironment) return parseEndpoint(fromEnvironment)
  const stored = kv.get(ORIGIN_KV_KEY)
  if (typeof stored !== "string") return undefined
  try {
    return parseOnboardingOrigin(stored)
  } catch {
    return undefined
  }
}

export function persistOrigin(kv: OriginKV, input: string) {
  const origin = parseOnboardingOrigin(input)
  kv.set(ORIGIN_KV_KEY, origin.href)
  return origin
}

export function parseOnboardingOrigin(input: string) {
  const origin = parseEndpoint(input.trim())
  if (origin.protocol !== "https:") {
    throw new FernClientError("Enter the root HTTPS origin for Fern, for example https://fern.example.")
  }
  return origin
}
