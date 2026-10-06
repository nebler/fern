// Package task defines the dependency-free domain contract for Fern durable
// tasks: typed identifiers, actor attribution, idempotency comparison, result
// tuples, and secure ID generation. It contains no persistence, transport, or
// external-authority logic, so ingress, storage, and artifact code can agree on
// identities without depending on each other.
//
// Typed string casts do not validate; boundary callers must use the Parse*
// functions. ContextActor checks structure, not credentials, so only
// authenticated ingress may call WithActor. Display name and request ID are
// excluded from authority equivalence.
//
// Request hashes are opaque 32-byte caller-computed values; this package never
// canonicalizes JSON. ClassifyIdempotency compares ownership before hashes so it
// cannot disclose another actor's request equality.
//
// Task/attempt states describe parent records only; package run owns
// background execution state and phase. Result tuples do not prove Git object
// existence or bundle integrity.
package task
