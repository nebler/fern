# Task Result Collector

`taskresult.Collector` is a read-only host Git boundary. It is intended to run
only after the coordinator has proved success for the persisted exact OpenCode
session/message and while `workspace.Manager.AcquireQuiesced` remains held.

## Coordinator obligations

The coordinator MUST:

1. Bind the configured canonical checkout path to the task's numeric GitHub
   repository ID. Git has no numeric GitHub repository identity to prove.
2. Prove terminal success from a complete authoritative OpenCode scan for the
   exact persisted session and message IDs. Volatile events are insufficient.
3. Supply a bounded sanitized evidence JSON object and its SHA-256. The
   collector rejects the same sensitive key classes and size bound as
   `taskstore.SealResult` but does not obtain OpenCode evidence itself.
4. Acquire `workspace.Manager.AcquireQuiesced` before `Collect`, prevent every
   non-Manager filesystem/Git writer, and retain that fence until
   `taskstore.SealResult` commits with the exact returned values and current
   discovered revisions.
5. Treat every collection failure, timeout, output bound, or concurrent change
   as an integrity failure. The collector never repairs, resets, stages, or
   commits repository state.

## Limits

Git and filesystem reads are not an atomic snapshot. The collector performs two
full object-derived passes and a final clean/HEAD check, but exclusion of writers
is still a coordinator/environment obligation. Ignored files follow Git's clean
worktree semantics and are not reported by `status --untracked-files=all`.
Repositories using linked worktrees, SHA-256 objects, shallow history, grafts,
alternates, replace refs, `.gitmodules` or gitlinks anywhere in the complete
`base..result` commit range, unsafe local execution/config features, more than
10,000 changed paths, paths over 4096 bytes, or proof output over the configured
bound fail closed. Rename detection is disabled, so renames are represented
canonically as one deletion and one addition.
