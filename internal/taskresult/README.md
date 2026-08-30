# Task Result Collector

`taskresult.Collector` is a read-only host Git boundary. It is intended to run
under one of two explicit authorities:

- an external observer has proved success for the persisted exact OpenCode
  session/message while `workspace.Manager.AcquireQuiesced` remains held; or
- a user has authorized one exact previewed snapshot while
  `workspace.Manager.AcquirePaused` remains held.

The user-authorized path supersedes the attempt and does not claim OpenCode
success.

## Coordinator obligations

The coordinator MUST:

1. Bind the configured canonical checkout path to the task's numeric GitHub
   repository ID. Git has no numeric GitHub repository identity to prove.
2. Establish either authoritative terminal success or an exact durable user
   seal request. Volatile events and idle state are insufficient.
3. Supply a bounded sanitized evidence JSON object and its SHA-256. The
   collector rejects the same sensitive key classes and size bound as
   `taskstore.SealResult` but does not obtain OpenCode evidence itself.
4. Acquire the authority-appropriate fence before `Collect`, prevent every
   non-Manager filesystem/Git writer, and retain that fence until the matching
   seal transaction commits with the exact returned values and current
   revisions. Observer authority uses `AcquireQuiesced`; user authority uses
   `AcquirePaused` and revalidates the previewed snapshot.
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
