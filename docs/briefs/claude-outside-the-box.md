# Claude outside the box: threat model (Phase 4, #10)

For review before #8 and #9 are built. Nothing here is built yet.

## What changes

Today a thread's Claude runs **in its box**: T3's server starts `claude`, which
is `fern-claude`, which runs `fern exec` into the box (architecture §15).
Every tool call Claude makes, built-in or Bash, runs there, so the box is awake
for the whole turn and gets the Claude login (`CLAUDE_CODE_OAUTH_TOKEN`) and
Claude's folder.

Phase 4 splits Claude from its tools:

| | Today | Phase 4 |
|---|---|---|
| Claude's process | in the box | in T3's pod (next to T3's server) |
| Read, Edit, Write, Glob, Grep | Claude's built-ins, in the box | Fern MCP tools served by the daemon on the thread's subvolume through `os.Root` |
| Bash | built-in, in the box | a Fern MCP tool: an exec in the box, which wakes on the first call and sleeps a few seconds after the last |
| WebFetch, WebSearch | built-in, from the box's network | off, or a Fern tool that runs in the box (see T8) |
| Claude's login | in every box | only in T3's pod |
| Claude's folder (transcripts) | mounted into the box, writable | T3's pod only; the box no longer mounts it |

The gain: a turn that only reads and edits files never wakes a box, and a box is
awake only while commands run.

## Assets

1. **The Claude login** (the person's subscription).
2. **T3's pod**: its own login, the whole pool (`/var/lib/fern`: every thread's
   workspace, snapshots, goldens), the daemon's socket, the paired app's session.
3. **The daemon**: root, btrfs, the Kubernetes service account.
4. **Other threads' environments.**
5. **The person's main repository** (`repos/<repo>`) and its git folder.

## Trust after the change

- **The box stays untrusted.** It can write anything in its workspace.
- **Claude becomes untrusted input processed in a trusted place.** Claude itself
  is the vendor's binary, but what it reads (files the box wrote, command output,
  web pages) can steer it (prompt injection). Anything Claude can do in T3's pod,
  an attacker who controls the workspace can try to make it do.
- So the rule becomes: **Claude in T3's pod must be able to do nothing but call
  Fern's tools**, and each Fern tool must be safe for any argument.

## Threats and what stops each

**T1. A built-in tool runs in T3's pod.** Bash, Read, Edit, Write, Glob, Grep,
NotebookEdit, WebFetch, Task/Agent subprocesses: any of them running in T3's pod
reaches assets 1–5 directly.
- Stop: `fern-claude` starts Claude with `--tools ""` (Claude 2.1.304: "disable
  all tools") and allows only the Fern MCP tools. T3's SDK passes its own options
  (`tools: {preset: claude_code}`); `fern-claude` must win over them, so it
  rewrites T3's flags rather than appending.
- Test (`check.sh t3`): a turn asks Claude to run `id` with Bash, to read
  `/var/lib/fern` and to write `/tmp/x` in T3's pod; all must fail, and nothing
  may appear in T3's pod.

**T2. The workspace configures Claude.** Claude reads `.claude/settings.json`,
`.claude/settings.local.json` and `.mcp.json` from its working directory, and
those can run commands in Claude's process environment: hooks, `statusLine`,
`apiKeyHelper`, stdio MCP servers, plugins. The box writes the workspace, so
today's harmless file becomes code execution in T3's pod.
- Stop: `--setting-sources user` (no project or local settings),
  `--strict-mcp-config` with only Fern's and T3's MCP servers, and no plugins.
  `CLAUDE.md` in the workspace stays readable: it is text, so prompt injection
  (T5), not code.
- Stop: the user settings themselves live in Claude's folder. Today the box
  mounts that folder writable, and could plant hooks there. Phase 4 removes the
  mount: only T3's pod sees Claude's folder.
- Test: a box writes a `.claude/settings.json` with a hook that touches a file,
  and an `.mcp.json` with a stdio server; the next turn must not run either.

**T3. A Fern file tool reaches outside the workspace.** The daemon is root: a
path with `..`, an absolute path, or a symlink the box planted must not lead out.
- Stop: every call opens the workspace with `os.Root` (no `..`, no escaping
  symlinks), as golden copies already do (§7.5). Special files are refused
  (a FIFO would block the daemon, a device could read the host). Reads and
  writes have size limits. Files the tools create are owned by the box's uid,
  not root.
- Stop: the read-only parts stay read-only: the worktree's `.git` file and,
  if reachable, the repository's git folder. The box can't change them (§13);
  neither may the tools.
- Test: unit tests over a workspace with `..`, absolute paths, symlinks out,
  a FIFO, a huge file and the `.git` file.

**T4. A tool call reaches another environment or the daemon's admin API.**
- Stop: the MCP endpoint takes a token scoped to one environment (planned in
  §13 already), handed to that thread's Claude by `fern-claude`; it gives only
  the file tools and Bash for that environment, no snapshot, restore, delete or
  other environment. The endpoint is not the admin socket.
- Test: a token for thread A naming thread B's path, or calling an admin
  route, is refused.

**T5. Prompt injection through what Claude reads.** A file or command output
tells Claude to do something harmful.
- Unchanged in kind: today Claude can do anything the box can. After the
  change it can do **less**: only Fern's tools, which can only touch its own
  workspace and its own box. Harm stays inside the thread's environment, which a
  revert undoes.

**T6. The login.** Today every box holds it in its environment; a box escape or
any process in the box can read it.
- After the change: boxes hold no login at all. It lives only in T3's pod,
  in each Claude process's environment. This is an improvement.

**T7. Resource exhaustion in T3's pod.** Claude processes (150–450 MiB each)
move from boxes into T3's pod, with no box cgroup around them.
- Stop: hibernation (built) still stops an idle Claude, now in T3's pod. Give
  each Claude a memory limit (a cgroup per Claude, or one for all in T3's pod)
  and cap concurrent Claudes, so one runaway thread can't starve T3's server.
  To decide: a cgroup per Claude inside T3's pod, or a small sandbox per Claude.

**T8. Network from T3's pod.** WebFetch and WebSearch would fetch from T3's
pod, which can reach the Kubernetes API, the node, and T3 itself: server-side
request forgery against assets 2–3.
- Stop: WebFetch and WebSearch stay off in T3's pod. If wanted, a Fern
  `web_fetch` tool runs the fetch in the box, whose network policy already
  allows only the internet (§13).
- Test: with `--tools ""`, a turn asking for WebFetch must fail.

**T9. Git run by Claude in T3's pod.** Claude runs `git` for context (status,
branch) in its working directory, the worktree.
- Same exposure as T3's server today, which runs git in the same worktrees: the
  git folder and the worktree's `.git` file are read-only to the box (§13), so
  the box can't set a config that runs commands. No new risk, but the check in
  `check.sh t3` must cover Claude's git too.

## What `check.sh t3` gains

- T1: built-in tools fail in T3's pod; nothing lands there.
- T2: planted project settings, hooks and MCP servers don't run; the box can't
  see Claude's folder.
- T3: file tools refuse escapes (unit tests).
- T4: a token for one environment can't reach another.
- T8: no WebFetch from T3's pod.
- And the gain: a read-only turn doesn't wake the box (`fern ls` stays asleep),
  a Bash call wakes it, it sleeps a few seconds after.

## To decide in review

1. **Where Claude runs:** in T3's pod (simplest; T7 needs a cgroup per Claude),
   or a small Claude-only sandbox per thread (gVisor, no services, no workspace
   write), which keeps today's isolation of Claude from T3's pod but costs a
   sandbox per active thread. This brief assumes T3's pod.
2. **WebFetch:** off, or a box-side tool (wakes the box).
3. **How long a box stays up after the last Bash call** (a few seconds; the next
   call within it doesn't pay a wake).
4. **Whether Phase 3A (the daemon runs boxes) changes any of this:** the daemon
   would run Claude's sandbox too, if (1) picks a sandbox.
