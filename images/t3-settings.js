// Fern's defaults for T3, where the person hasn't chosen: a new thread is a
// New worktree, so it gets an environment (a Local thread has no box or
// database), and deleting a thread removes its worktree, which takes its
// environment with it (architecture §15), OpenCode is on and Codex and T3's
// browser tools, which the server can't run, are off. A choice made in T3's
// settings stays. fern-t3.service runs it before T3 starts.
const fs = require("fs");
const f = "/var/lib/fern/userdata/settings.json";
let s = {};
try {
  s = JSON.parse(fs.readFileSync(f, "utf8"));
} catch (e) {
  if (e.code !== "ENOENT") throw e;
}
const before = JSON.stringify(s);
s.defaultThreadEnvMode ??= "worktree";
s.storageCleanup ??= {};
s.storageCleanup.worktreeOnDelete ??= true;
// With that setting T3 asks nothing when a thread is deleted, and keeps the
// worktree, and so its environment and snapshots, if git status shows
// anything at all, untracked files included: with "tracked-changes" it
// cleans those away first and keeps only a worktree with edits to tracked
// files that were never committed.
s.storageCleanup.worktreeKeepWhen ??= "tracked-changes";
// T3's own browser, which it offers Claude as tools, runs on the server: it
// has no Chromium there and the image's root can't take one, and a
// localhost in it would be the server's, not a box's. Previews are Fern's
// (architecture §5).
s.enableAgentBrowserAccess ??= false;
// OpenCode is off in T3 until someone turns it on; Fern's image has it, with
// Claude's login (images/fern-opencode).
s.providerInstances ??= {};
s.providerInstances.opencode ??= { driver: "opencode", enabled: true };
// T3 writes pull request text and commit messages with Codex unless told
// otherwise, and only looks for another provider when Codex is turned off.
// Fern's image has no codex (roadmap step 8), so "Create PR" and a commit
// with no message failed: off, until it is there.
s.providerInstances.codex ??= { driver: "codex", enabled: false };
if (JSON.stringify(s) !== before) {
  fs.writeFileSync(f + ".tmp", JSON.stringify(s, null, 2) + "\n");
  fs.renameSync(f + ".tmp", f);
}
