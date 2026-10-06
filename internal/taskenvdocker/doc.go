// Package taskenvdocker is the provider for one serial, disposable Docker
// Background Run. It owns exact resource attestation, clone/volume/container
// lifecycle effects, runtime-fenced transport, and repository-scoped GitHub
// credential delivery. It never schedules work or writes taskstore state; it
// imports taskstore only for record types.
//
// Resources are created or positively attested, never adopted by name.
// Failure to prove identity yields an IdentityError (ErrIdentityMismatch and
// ErrQuarantined) that needs operator attention, not permission to delete a
// similarly named object. Health and route transports re-attest the exact
// runtime (container ID plus start time), so a restarted process never inherits
// prior authority. Destructive removal requires an explicit WriterFence, and
// ProveWriterInactive may stop a container. AcquireExportSource holds the
// exclusive clone lock after a fresh inactivity proof; its Close must be called
// and does not remove the clone.
//
// The worker container has fixed hardening (read-only root, bounded tmpfs, all
// capabilities dropped, default-deny seccomp that forbids changing XFS project
// IDs, fixed CPU and PID limits) but unrestricted bridge egress. It is not a
// hostile-code sandbox, and no host environment is injected. Execution requires
// an operator-provisioned XFS project quota on a filesystem separate from
// durable state; see docs/usage.md for the operator contract. Without one, the
// provider permits cleanup only.
//
// GitHub tokens are minted for the exact configured repository, re-delivered
// to the running container five minutes before expiry, and written as private
// files in the OpenCode volume. Token bytes never appear in exec arguments,
// labels, evidence, or errors. Stopping a container does not revoke its token,
// and the agent's own tools, not Fern, perform any publication.
package taskenvdocker
