// Package docker is the provider for one serial, disposable Docker
// run. It owns exact resource attestation, clone/volume/container
// lifecycle effects, runtime-fenced transport, and repository-scoped GitHub
// credential delivery. It never schedules work or writes store state; it
// imports store only for record types.
//
// Every resource has a deterministic identity, and each Ensure*/Remove* call
// inspects before it acts, so callers can repeat any step to reconcile.
// Resources are created or attested, never adopted by name alone: a container
// must carry its canonical name, the qualified image, and Fern's ownership and
// spec-digest labels; its remaining configuration is Fern's own create request
// and is not re-checked. Failure to prove identity yields an IdentityError
// (ErrIdentityMismatch and ErrQuarantined) that needs operator attention, not
// permission to delete a similarly named object. Health, route dials,
// credential writes, stop, and removal also require the exact runtime
// (container ID plus start time), so a restarted process never inherits prior
// authority. Destructive removal requires an explicit WriterFence, and
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
package docker
