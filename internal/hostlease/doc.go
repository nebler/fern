// Package hostlease owns Fern's exclusive host-process lock for one repository
// binding. cmd/fern takes it for startup, backup, and credential operations so
// cooperating Fern processes never mutate a binding concurrently. It is not a
// distributed lease, an expiring database claim, or anything the container
// agent sees: despite the name, the lock never expires and is held until
// Release or process exit.
//
// Ownership is the kernel flock on a private, singly linked lock file inside a
// private directory; the hostname and PID written into the file are diagnostics
// only. Contention fails immediately rather than waiting. Lock files are left
// on disk after release, and deleting one to "unlock" a live process would break
// inode-based exclusion.
package hostlease
