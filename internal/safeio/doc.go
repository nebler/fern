// Package safeio holds Fern's small, security-relevant I/O primitives: atomic
// private-file replacement and bounded reads, trust-neutral filesystem
// helpers, the exclusive host lease, and strict JSON scanning.
//
// # Private files
//
// WriteFile, WriteFileExclusive, ReadFile, and PrivateDir are meant for paths
// only the Fern user (or root) can write: the containing directory is checked
// once by PrivateDir when a store opens, and individual files are then read
// and written without per-file link-count, mode, or owner checks. They are not
// suitable for directories an agent or other user can modify.
//
// The filesystem primitives RenameNoReplace, SyncDir, Identity,
// QuarantineRemove, and RemoveTree make no trust assumption about the
// directory and are shared by packages that do guard agent-writable trees.
//
// # Host lease
//
// AcquireLease takes Fern's exclusive host-process lock for one repository
// binding. cmd/fern takes it for startup, backup, and credential operations so
// cooperating Fern processes never mutate a binding concurrently. It is not a
// distributed lease, an expiring database claim, or anything the container
// agent sees: the lock never expires and is held until Release or process
// exit. Ownership is the kernel flock on a private, singly linked lock file
// inside a private directory; the hostname and PID written into the file are
// diagnostics only. Contention fails immediately rather than waiting. Lock
// files are left on disk after release, and deleting one to "unlock" a live
// process would break inode-based exclusion.
//
// # Strict JSON
//
// CheckJSON is the single source of truth for strictly scanning untrusted
// JSON payloads before they are decoded into application types. Every remote
// or stored response that feeds a security-sensitive decoder is checked here
// so duplicate object keys (compared case-insensitively, the way downstream
// merge logic resolves them), excessive nesting, invalid UTF-8, and trailing
// garbage are rejected exactly once and identically everywhere. CheckJSON is a
// validator only, not a canonical serializer or digest. Passing it does not
// mean a payload satisfies any schema: callers still own size limits, typed
// decoding with unknown fields disallowed, and mapping errors to their public
// contracts.
package safeio
