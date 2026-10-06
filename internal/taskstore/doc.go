// Package taskstore owns Fern's durable SQLite task state: workspaces,
// background-run admission, effect observations, sealing, exports,
// materialization, and retained results. It commits related records and their
// fences together so HTTP handlers and effect providers never coordinate SQL.
//
// Schema 6 is one complete pre-release schema, not a migration chain. Open
// rejects incompatible versions rather than upgrading or deleting data, and
// refuses symlinked or foreign-owned database paths. The database runs in WAL
// mode with foreign keys and FULL synchronization.
//
// Package run owns state/phase vocabulary and classification; this package owns
// durable representation and effect authority. IDs, timestamps, actor
// snapshots, and external evidence come from callers: the store never calls
// Docker, Git, OpenCode, or GitHub, and recorded bundle proofs are supplied
// evidence, not a filesystem read.
//
// There are no per-run claims or leases: the host lease admits one coordinator
// per workspace, and the only other writer is the in-process stop/seal API.
// Effect mutations compare a BackgroundRunRef or BackgroundRunExportRef
// (generation, revision, state/phase) in SQL and require exactly one affected
// row, so a write prepared before a concurrent stop or seal fails. A partial
// unique index permits at most one effecting background run per workspace.
// Prompt intent is durable before dispatch, and the prompt-request fence is
// one-way so a restarted coordinator cannot silently resend a prompt. Only the retained-result commit establishes
// result_ready. Transition order is enforced by these Go compare-and-swap
// updates plus table CHECKs; SQL triggers only guard immutability of recorded
// authority, revision progression, the prompt fence, and cleanup gating.
//
// Seal admission and the retained-result commit deliberately ignore request
// cancellation after validation; callers reconcile outcomes through durable
// IDs and receipts. Ownership-hiding reads return not-found rather than
// revealing foreign runs.
package taskstore
