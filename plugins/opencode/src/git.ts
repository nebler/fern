import { subprocessEnvironment } from "./process.js"

export type GitContext = {
  root: string
  remote: string
  head: string
  branch: string | null
  dirty: boolean
}

export class GitContextError extends Error {
  constructor(message: string) {
    super(message)
    this.name = "GitContextError"
  }
}

export async function readGitContext(directory: string): Promise<GitContext> {
  const remotes = lines(await git(directory, ["remote"]))
  if (remotes.length === 0) throw new GitContextError("This repository has no Git remote.")

  const remoteName = remotes.includes("origin") ? "origin" : remotes.length === 1 ? remotes[0] : undefined
  if (!remoteName) throw new GitContextError("This repository has multiple remotes and no unambiguous origin remote.")

  const remoteURLs = lines(await git(directory, ["remote", "get-url", "--all", remoteName]))
  if (remoteURLs.length !== 1) throw new GitContextError("The selected Git remote does not have exactly one fetch URL.")

  const head = await git(directory, ["rev-parse", "--verify", "HEAD^{commit}"], true)
  if (!head) throw new GitContextError("This repository has no commit at HEAD.")

  const branch = await git(directory, ["symbolic-ref", "--quiet", "--short", "HEAD"], true)
  const status = await git(directory, ["status", "--porcelain=v1", "--untracked-files=normal"])
  return {
    root: await git(directory, ["rev-parse", "--show-toplevel"]),
    remote: canonicalizeRemote(remoteURLs[0]),
    head,
    branch: branch || null,
    dirty: status.length > 0,
  }
}

export function requireRunnableGitContext(context: GitContext) {
  if (context.dirty)
    throw new GitContextError("The working tree is dirty. Commit or remove local changes before starting a Fern run.")
  return context
}

export function requireLocalRunMode(argv: readonly string[] = process.argv) {
  if (
    argv.some((argument) => argument === "--server" || argument.startsWith("--server=")) ||
    argv.slice(1).includes("attach")
  ) {
    throw new GitContextError(
      "Fern run creation is unavailable with a remote OpenCode server because OpenCode paths are server-side. Use a local OpenCode service.",
    )
  }
}

export function canonicalizeRemote(value: string) {
  const input = value.trim()
  if (!input || /[\u0000-\u001f\u007f]/.test(input)) throw unsupportedRemote()
  if (input.includes("://")) return canonicalizeURL(input)

  const scp = input.match(/^(?:(git)@)?([A-Za-z0-9._-]+|\[[0-9A-Fa-f:]+\]):([^?#\\]+)$/)
  if (!scp) throw unsupportedRemote()
  return canonicalURL(scp[2].toLowerCase(), "", scp[3])
}

function canonicalizeURL(input: string) {
  const url = URL.parse(input)
  if (
    !url ||
    !["https:", "http:", "ssh:"].includes(url.protocol) ||
    !url.hostname ||
    url.password ||
    url.search ||
    url.hash ||
    (url.username && (url.protocol !== "ssh:" || url.username !== "git"))
  )
    throw unsupportedRemote()
  const port = url.protocol === "ssh:" && url.port === "22" ? "" : url.port
  return canonicalURL(url.hostname.toLowerCase(), port, url.pathname)
}

function canonicalURL(hostname: string, port: string, path: string) {
  const repository = path.replace(/^\/+|\/+$/g, "").replace(/\.git$/i, "")
  if (
    !repository ||
    /[?#\\]/.test(repository) ||
    repository.split("/").some((part) => !part || part === "." || part === "..")
  ) {
    throw new GitContextError("The Git remote does not identify a repository.")
  }
  const host = hostname.includes(":") && !hostname.startsWith("[") ? `[${hostname}]` : hostname
  return `https://${host}${port ? `:${port}` : ""}/${repository}`
}

function unsupportedRemote() {
  return new GitContextError("The Git remote is not a supported canonical network URL.")
}

function lines(value: string) {
  return value
    .split("\n")
    .map((item) => item.trim())
    .filter(Boolean)
}

async function git(directory: string, args: string[], allowFailure = false) {
  const process = Bun.spawn(["git", "-C", directory, ...args], {
    env: subprocessEnvironment(),
    stdin: "ignore",
    stdout: "pipe",
    stderr: "pipe",
  })
  const [exit, stdout, stderr] = await Promise.all([
    process.exited,
    Bun.readableStreamToText(process.stdout),
    Bun.readableStreamToText(process.stderr),
  ])
  if (exit === 0) return stdout.trim()
  if (allowFailure) return ""
  throw new GitContextError(stderr.trim() || `git ${args[0]} failed with exit code ${exit}.`)
}
