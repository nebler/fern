// Package task defines the dependency-free domain contract for Fern durable
// tasks: typed identifiers, actor attribution, idempotency comparison, and
// secure ID generation. It contains no persistence, transport, or
// external-authority logic, so ingress, storage, and artifact code can agree on
// identities without depending on each other.
//
// Typed string casts do not validate. Parse* functions belong at boundaries
// (HTTP input, configuration); IDs Fern generated itself flow as typed values
// and are not re-parsed downstream. ContextActor checks structure, not
// credentials, so only authenticated ingress may call WithActor. Display name and request ID are
// excluded from authority equivalence.
//
// Request hashes are opaque 32-byte caller-computed values; this package never
// canonicalizes JSON. ClassifyIdempotency compares ownership before hashes so it
// cannot disclose another actor's request equality.
//
// Package run owns run lifecycle state and phase.
package task
