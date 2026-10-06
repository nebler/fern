// Package taskartifact is the local retained-artifact engine: it captures the
// final nonignored Git state of a background-run checkout, independently
// verifies it, installs immutable content-addressed bytes, and hands out owned
// checkouts. It is not a remote artifact service, a taskstore transaction
// manager, or a GitHub publisher, and has no database, network, or container
// dependencies; durable result ownership belongs to taskstore and
// taskresultsource, and write fencing to the coordinator.
//
// A changed result is a normalized commit whose parent is the admitted base; a
// no-change result is the base itself. Final content (including untracked
// nonignored files, deletions, modes, and symlinks) is preserved, not the
// agent's commit sequence. Capture never rewrites the source HEAD, index, or
// worktree, though Git may add objects.
//
// Inspect and Acquire always perform full verification (independent bare
// import, exact refs, fsck --strict --full, rebuilt digests); no earlier result
// acts as an integrity cache, so Inspect is not cheap. Acquire additionally
// rehashes the bytes it materializes so a changed CAS source cannot bypass the
// proof.
//
// Every successful stage must be stored or discarded, and every checkout
// closed. StagedLocator is an opaque in-process capability, not a durable path.
// Only one engine may use a given set of roots: startup reconciles temporary
// directories without a cross-process lock. Private roots and ownership checks
// are a trusted-host boundary, not a sandbox.
package taskartifact
