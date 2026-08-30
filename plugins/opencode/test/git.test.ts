import { afterEach, describe, expect, test } from "bun:test"
import { mkdtemp, rm } from "node:fs/promises"
import { join } from "node:path"
import { tmpdir } from "node:os"
import {
  canonicalizeRemote,
  GitContextError,
  readGitContext,
  requireLocalRunMode,
  requireRunnableGitContext,
} from "../src/git.js"

const directories: string[] = []

afterEach(async () => {
  await Promise.all(directories.splice(0).map((directory) => rm(directory, { recursive: true, force: true })))
})

describe("canonicalizeRemote", () => {
  test("normalizes HTTPS, SSH, and scp remotes", () => {
    expect(canonicalizeRemote("https://GitHub.com/fern/repo.git")).toBe("https://github.com/fern/repo")
    expect(canonicalizeRemote("git@github.com:fern/repo.git")).toBe("https://github.com/fern/repo")
    expect(canonicalizeRemote("ssh://git@github.com/fern/repo.git")).toBe("https://github.com/fern/repo")
    expect(canonicalizeRemote("ssh://git@github.com:22/fern/repo.git")).toBe("https://github.com/fern/repo")
    expect(canonicalizeRemote("ssh://git@github.com:2222/fern/repo.git")).toBe("https://github.com:2222/fern/repo")
  })

  test("rejects local and malformed remotes", () => {
    expect(() => canonicalizeRemote("../repo")).toThrow(GitContextError)
    expect(() => canonicalizeRemote("file:///tmp/repo")).toThrow(GitContextError)
    expect(() => canonicalizeRemote("https://github.com@evil.example/fern/repo")).toThrow(GitContextError)
    expect(() => canonicalizeRemote("ssh://owner@github.com/fern/repo")).toThrow(GitContextError)
    expect(() => canonicalizeRemote("https://github.com/fern/repo?redirect=evil")).toThrow(GitContextError)
    expect(() => canonicalizeRemote("git@github.com:fern/repo#fragment")).toThrow(GitContextError)
  })

  test("refuses explicit server mode", () => {
    expect(() => requireLocalRunMode(["opencode2", "--server", "https://remote.example"])).toThrow("local OpenCode")
    expect(() => requireLocalRunMode(["opencode2", "--server=https://remote.example"])).toThrow("local OpenCode")
    expect(() => requireLocalRunMode(["opencode2", "attach", "https://remote.example"])).toThrow("local OpenCode")
  })
})

describe("readGitContext", () => {
  test("reads canonical remote, exact HEAD, branch, and dirty state", async () => {
    const directory = await repository(true)
    await git(directory, "remote", "add", "origin", "git@github.com:fern/example.git")
    const clean = await readGitContext(directory)
    expect(clean).toEqual({
      root: await git(directory, "rev-parse", "--show-toplevel"),
      remote: "https://github.com/fern/example",
      head: await git(directory, "rev-parse", "HEAD"),
      branch: "main",
      dirty: false,
    })

    await Bun.write(join(directory, "dirty.txt"), "dirty")
    const dirty = await readGitContext(directory)
    expect(dirty.dirty).toBe(true)
    expect(() => requireRunnableGitContext(dirty)).toThrow("working tree is dirty")
  })

  test("rejects missing remote and unborn HEAD", async () => {
    const missing = await repository(true)
    await expect(readGitContext(missing)).rejects.toThrow("no Git remote")

    const unborn = await repository(false)
    await git(unborn, "remote", "add", "origin", "https://github.com/fern/example")
    await expect(readGitContext(unborn)).rejects.toThrow("no commit at HEAD")
  })

  test("rejects a remote with multiple fetch URLs", async () => {
    const directory = await repository(true)
    await git(directory, "remote", "add", "origin", "https://github.com/fern/example.git")
    await git(directory, "config", "--add", "remote.origin.url", "https://mirror.example/fern/example.git")
    await expect(readGitContext(directory)).rejects.toThrow("exactly one fetch URL")
  })
})

async function repository(commit: boolean) {
  const directory = await mkdtemp(join(tmpdir(), "fern-opencode-"))
  directories.push(directory)
  await git(directory, "init", "--initial-branch=main")
  if (!commit) return directory
  await Bun.write(join(directory, "README.md"), "fixture\n")
  await git(directory, "add", "README.md")
  await git(directory, "-c", "user.name=Fern Test", "-c", "user.email=fern@example.com", "commit", "-m", "fixture")
  return directory
}

async function git(directory: string, ...args: string[]) {
  const process = Bun.spawn(["git", "-C", directory, ...args], {
    stdout: "pipe",
    stderr: "pipe",
  })
  const [exit, stdout, stderr] = await Promise.all([
    process.exited,
    Bun.readableStreamToText(process.stdout),
    Bun.readableStreamToText(process.stderr),
  ])
  if (exit !== 0) throw new Error(stderr)
  return stdout.trim()
}
