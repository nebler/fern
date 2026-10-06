// Package taskresultsource resolves immutable retained-result Git state. A
// disposable Background Run clone is never post-result authority, even if it
// still exists on disk.
//
// Verify and Acquire load retained metadata from the store, freshly verify the
// artifact through the engine, and check the full result/export/artifact/
// snapshot tuple. A mismatch is corruption, never a cue to recreate content
// from the clone. A previous Verify is not an integrity cache.
//
// Every successful Acquire returns a cleanup function that must be called; it
// closes the engine-owned checkout rather than deleting a caller-chosen path.
// The resolver holds no resources of its own. cmd/fern injects it into runapi
// as the retention verifier through an interface, so runapi does not import
// this package.
package taskresultsource
