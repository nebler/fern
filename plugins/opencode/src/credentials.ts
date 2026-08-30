import { runProcess, subprocessEnvironment } from "./process.js"

export const CREDENTIAL_SERVICE = "dev.fern.opencode"
export const CREDENTIAL_ACCOUNT_ATTRIBUTE = "origin"

export interface CredentialStore {
  get(): Promise<string | undefined>
  set(credential: string): Promise<void>
  delete(): Promise<void>
}

export class CredentialStoreError extends Error {
  constructor(message: string) {
    super(message)
    this.name = "CredentialStoreError"
  }
}

export class InMemoryCredentialStore implements CredentialStore {
  #credential: string | undefined

  constructor(credential?: string) {
    this.#credential = credential
  }

  async get() {
    return this.#credential
  }

  async set(credential: string) {
    this.#credential = credential
  }

  async delete() {
    this.#credential = undefined
  }
}

export type SubprocessResult = {
  exitCode: number
  stdout?: string
  stderr?: string
  timedOut?: boolean
  canceled?: boolean
  overflow?: boolean
}
export type SubprocessRunner = (
  argv: readonly string[],
  options: {
    stdin?: string
    env: Record<string, string | undefined>
    signal?: AbortSignal
    timeoutMs: number
    outputLimit: number
  },
) => Promise<SubprocessResult>

export class OSCredentialStore implements CredentialStore {
  readonly #origin: string
  readonly #platform: NodeJS.Platform
  readonly #run: SubprocessRunner
  readonly #signal: AbortSignal | undefined
  readonly #timeoutMs: number

  constructor(
    origin: string,
    options: {
      platform?: NodeJS.Platform
      run?: SubprocessRunner
      signal?: AbortSignal
      timeoutMs?: number
    } = {},
  ) {
    this.#origin = canonicalOrigin(origin)
    this.#platform = options.platform ?? process.platform
    this.#run = options.run ?? runProcess
    this.#signal = options.signal
    this.#timeoutMs = options.timeoutMs ?? 10_000
    if (!Number.isSafeInteger(this.#timeoutMs) || this.#timeoutMs <= 0) {
      throw new CredentialStoreError("Fern keyring timeout must be a positive integer.")
    }
  }

  async available() {
    try {
      if (this.#platform === "darwin") {
        const result = await this.#execute([
          "/usr/bin/security",
          "find-generic-password",
          "-a",
          "availability-probe",
          "-s",
          `${CREDENTIAL_SERVICE}.probe`,
          "-w",
        ])
        return result.exitCode === 0 || result.exitCode === 44
      }
      if (this.#platform === "linux") {
        const result = await this.#execute([
          "secret-tool",
          "lookup",
          "service",
          `${CREDENTIAL_SERVICE}.probe`,
          CREDENTIAL_ACCOUNT_ATTRIBUTE,
          "https://invalid.invalid/",
        ])
        return result.exitCode === 0 || linuxNotFound(result)
      }
      return false
    } catch {
      return false
    }
  }

  async get() {
    const result = await this.#execute(this.#argv("get"))
    if (this.#notFound(result)) return undefined
    if (result.exitCode !== 0) throw keyringFailure("read")
    const credential = (result.stdout ?? "").replace(/[\r\n]+$/, "")
    if (!isCredential(credential)) throw new CredentialStoreError("The Fern keyring returned an invalid credential.")
    return credential
  }

  async set(credential: string) {
    if (!isCredential(credential)) throw new CredentialStoreError("Fern refused to store an invalid credential.")
    const result = await this.#execute(this.#argv("set"), `${credential}\n`)
    if (result.exitCode !== 0) throw keyringFailure("save")
  }

  async delete() {
    if (this.#platform === "linux" && (await this.get()) === undefined) return
    const result = await this.#execute(this.#argv("delete"))
    if (this.#platform === "darwin" && this.#notFound(result)) return
    if (result.exitCode !== 0) throw keyringFailure("remove")
  }

  async #execute(argv: readonly string[], stdin?: string) {
    let result: SubprocessResult
    try {
      result = await this.#run(argv, {
        stdin,
        env: subprocessEnvironment(),
        signal: this.#signal,
        timeoutMs: this.#timeoutMs,
        outputLimit: 4_096,
      })
    } catch {
      throw keyringFailure("access")
    }
    if (result.canceled) throw new CredentialStoreError("Fern keyring access was canceled.")
    if (result.timedOut) throw new CredentialStoreError("Fern keyring access timed out.")
    if (result.overflow) throw new CredentialStoreError("Fern keyring output exceeded the allowed size.")
    return result
  }

  #notFound(result: SubprocessResult) {
    return this.#platform === "darwin" ? result.exitCode === 44 : this.#platform === "linux" && linuxNotFound(result)
  }

  #argv(operation: "get" | "set" | "delete") {
    if (this.#platform === "darwin") {
      const action =
        operation === "get"
          ? "find-generic-password"
          : operation === "set"
            ? "add-generic-password"
            : "delete-generic-password"
      return [
        "/usr/bin/security",
        action,
        ...(operation === "set" ? ["-U"] : []),
        "-a",
        this.#origin,
        "-s",
        CREDENTIAL_SERVICE,
        ...(operation === "get" || operation === "set" ? ["-w"] : []),
      ]
    }
    if (this.#platform === "linux") {
      const attributes = ["service", CREDENTIAL_SERVICE, CREDENTIAL_ACCOUNT_ATTRIBUTE, this.#origin]
      if (operation === "get") return ["secret-tool", "lookup", ...attributes]
      if (operation === "set") return ["secret-tool", "store", "--label=Fern OpenCode plugin", ...attributes]
      return ["secret-tool", "clear", ...attributes]
    }
    throw new CredentialStoreError("Fern durable credentials require macOS Keychain or Linux Secret Service.")
  }
}

export function isCredential(value: string) {
  if (!/^[A-Za-z0-9_-]+$/.test(value)) return false
  const decoded = Buffer.from(value, "base64url")
  return decoded.byteLength === 32 && decoded.toString("base64url") === value
}

function canonicalOrigin(input: string) {
  const url = URL.parse(input)
  if (
    !url ||
    url.protocol !== "https:" ||
    url.username ||
    url.password ||
    url.pathname !== "/" ||
    url.search ||
    url.hash
  ) {
    throw new CredentialStoreError("Fern credentials require a canonical root HTTPS origin.")
  }
  return url.href
}

function linuxNotFound(result: SubprocessResult) {
  return result.exitCode === 1 && !(result.stderr ?? "").trim() && !(result.stdout ?? "").trim()
}

function keyringFailure(action: string) {
  return new CredentialStoreError(`Could not ${action} the Fern credential in the operating system keyring.`)
}
