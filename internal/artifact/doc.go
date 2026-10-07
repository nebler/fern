// Package artifact is the local retained-artifact engine and the resolver
// that binds its bytes to durable results. The Engine captures the final
// nonignored Git state of a run checkout, independently verifies it, installs
// immutable content-addressed bytes, and hands out owned checkouts. It is not
// a remote artifact service, a store transaction manager, or a GitHub
// publisher, and the engine has no database, network, or container
// dependencies; durable result ownership belongs to store and the Resolver,
// and write fencing to the coordinator.
//
// # Engine
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
//
// # Resolver
//
// The Resolver resolves immutable retained-result Git state. A disposable run
// clone is never post-result authority, even if it still exists on disk.
// Verify and Acquire load retained metadata from the store, freshly verify the
// artifact through the engine, and check the full result/export/artifact/
// snapshot tuple. A mismatch is corruption, never a cue to recreate content
// from the clone. A previous Verify is not an integrity cache.
//
// Every successful Resolver.Acquire returns a cleanup function that must be
// called; it closes the engine-owned checkout rather than deleting a
// caller-chosen path. The resolver holds no resources of its own. cmd/fern
// injects it into runapi as the retention verifier through an interface, so
// runapi does not import this package.
package artifact
