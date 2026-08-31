import type { FernClient } from "./client.js"
import { readGitContext, requireLocalRunMode, requireRunnableGitContext, type GitContext } from "./git.js"

export const SUPPORTED_OPENCODE_VERSION = "1.18.16"
export const FERN_REMOTE_EXECUTION_PROFILE = "source-39fb919a054190498f6d5b7985bde231f93ad7a6"
export const INSTRUCTION_MAX_LENGTH = 4_000

export type RunConfirmation = {
  host: string
  instruction: string
  profile: string
  git: GitContext
}

export interface PendingSubmissionStore {
  get(digest: string): Promise<string | undefined>
  set(digest: string, idempotencyKey: string): Promise<void>
  delete(digest: string): Promise<void>
}

export class InMemoryPendingSubmissionStore implements PendingSubmissionStore {
  readonly #pending = new Map<string, string>()

  async get(digest: string) {
    return this.#pending.get(digest)
  }

  async set(digest: string, idempotencyKey: string) {
    this.#pending.set(digest, idempotencyKey)
  }

  async delete(digest: string) {
    this.#pending.delete(digest)
  }
}

export class CreateRunLatch {
  #active = false

  async run<Value>(action: () => Promise<Value>) {
    if (this.#active) throw new Error("A Fern run submission is already in progress.")
    this.#active = true
    try {
      return await action()
    } finally {
      this.#active = false
    }
  }
}

export async function createRunWorkflow(input: {
  client: Pick<FernClient, "createRun">
  pending: PendingSubmissionStore
  directory: string
  host: string
  instruction: string
  confirm: (confirmation: RunConfirmation) => Promise<boolean>
  idempotencyKey?: () => string
  argv?: readonly string[]
}) {
  requireLocalRunMode(input.argv)
  const instruction = input.instruction.trim()
  if (!instruction) throw new Error("Enter an instruction for the Fern run.")
  if (instruction.length > INSTRUCTION_MAX_LENGTH) {
    throw new Error(`Fern instructions are limited to ${INSTRUCTION_MAX_LENGTH} characters.`)
  }

  const git = requireRunnableGitContext(await readGitContext(input.directory))
  if (!(await input.confirm({ host: input.host, instruction, profile: FERN_REMOTE_EXECUTION_PROFILE, git })))
    return undefined

  const current = await readGitContext(input.directory)
  if (!sameGitContext(current, git)) {
    throw new Error("The repository changed after confirmation. Review the current Git state and try again.")
  }
  requireRunnableGitContext(current)

  const digest = await requestDigest({ instruction, profile: FERN_REMOTE_EXECUTION_PROFILE, git })
  const idempotencyKey = (await input.pending.get(digest)) ?? (input.idempotencyKey ?? crypto.randomUUID)()
  await input.pending.set(digest, idempotencyKey)
  const runID = await input.client.createRun({
    instruction,
    profile: FERN_REMOTE_EXECUTION_PROFILE,
    git,
    idempotencyKey,
  })
  await input.pending.delete(digest)
  return runID
}

export async function stopRunWorkflow(input: {
  client: Pick<FernClient, "stopRun" | "requireRunID">
  runID: string
  confirm: (runID: string) => Promise<boolean>
  idempotencyKey?: () => string
}) {
  const runID = input.client.requireRunID(input.runID.trim())
  if (!(await input.confirm(runID))) return undefined
  return input.client.stopRun(runID, (input.idempotencyKey ?? crypto.randomUUID)())
}

async function requestDigest(input: { instruction: string; profile: string; git: GitContext }) {
  const value = JSON.stringify({
    instruction: input.instruction,
    profile: input.profile,
    root: input.git.root,
    repository: input.git.remote,
    head: input.git.head,
    branch: input.git.branch,
    dirty: input.git.dirty,
  })
  return Array.from(new Uint8Array(await crypto.subtle.digest("SHA-256", new TextEncoder().encode(value))))
    .map((byte) => byte.toString(16).padStart(2, "0"))
    .join("")
}

function sameGitContext(left: GitContext, right: GitContext) {
  return (
    left.root === right.root &&
    left.remote === right.remote &&
    left.head === right.head &&
    left.branch === right.branch &&
    left.dirty === right.dirty
  )
}
