// Package backgroundruncoord serially coordinates the one qualified OpenCode
// Background Run profile, connecting durable taskstore claims to Docker,
// OpenCode, route, and retained-artifact effects. It is intentionally not a
// generic executor framework: there is one lane, no parallelism, and no lease
// renewal goroutine.
//
// taskstore owns transition legality; the coordinator records provider
// observations only under claim and revision fences, and a filesystem or Docker
// effect is never treated as done until its durable write succeeds. RunOnce
// processes one claimed run's current phase, which may involve several external
// effects. Effects are bounded by the operation timeout (shorter than the lease,
// at most five minutes), claim expiry, and, where policy requires, the attempt
// deadline. Recovery and final result writes ignore caller cancellation but
// remain bounded by the operation timeout.
//
// Prompt dispatch is never blindly replayed: durable intent and the one-way
// prompt-request fence are committed before the single admission call, and any
// outcome other than confirmed admission is recorded as uncertain. Unknown
// working observations release the claim rather than inferring idle or success.
// Changed execution configuration requests cleanup instead of adopting it.
//
// GitHub credentials are refreshed only during execution and never gate
// teardown; remote publication is the agent's own action, and there is no host
// publisher. Snapshot mismatch marks export recovery required rather than
// accepting different content. Teardown proves writer inactivity and drains
// the route before removing container, volume, and clone, and only positive
// absence evidence permits terminal cleanup. This is a sequence of recoverable
// ordered effects, not an atomic transaction. The coordinator does not close
// its dependencies; composition stops it first.
package backgroundruncoord
