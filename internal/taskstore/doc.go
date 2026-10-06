// Package taskstore owns Fern's durable SQLite task state: workspaces,
// background-run admission and lifecycle, sealing, exports, and retained
// results. It commits related records and their
// fences together so HTTP handlers and effect providers never coordinate SQL.
//
// Schema 7 is one complete pre-release schema, not a migration chain. Open
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
//
// A run stores only what inspection cannot re-derive: its phase, the committed
// runtime identity (stop and cleanup authority), the one-way prompt-request
// fence that ends provisioning before dispatch, stop/timeout/seal admission,
// the writer fence, the selected export tuple, and the terminal cleanup proof.
// Only the retained-result commit establishes result_ready, and it does so in
// the same transaction that completes the export and marks the materialization
// ready. Transition order is enforced by these Go compare-and-swap updates plus
// table CHECKs; triggers guard immutability of recorded authority, revision
// progression, and that a sealed run keeps its resources until its result
// commits and no run becomes terminal without cleaning.
//
// Seal admission and the retained-result commit deliberately ignore request
// cancellation after validation; callers reconcile outcomes through durable
// IDs and receipts. Ownership-hiding reads return not-found rather than
// revealing foreign runs.
package taskstore
