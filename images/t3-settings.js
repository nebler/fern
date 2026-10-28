// Fern's defaults for T3, where the person hasn't chosen: a new thread is a
// New worktree, so it gets an environment (a Local thread has no box or
// database), and deleting a thread removes its worktree, which takes its
// environment with it (architecture §15). A choice made in T3's settings
// stays. fern-t3.service runs it before T3 starts.
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
if (JSON.stringify(s) !== before) {
  fs.writeFileSync(f + ".tmp", JSON.stringify(s, null, 2) + "\n");
  fs.renameSync(f + ".tmp", f);
}
