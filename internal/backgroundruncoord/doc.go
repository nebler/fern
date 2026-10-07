// Package backgroundruncoord serially coordinates the one qualified OpenCode
// Background Run profile, connecting durable store state to Docker,
// OpenCode, route, and retained-artifact effects. It is intentionally not a
// generic executor framework: there is one lane, no parallelism, and no
// per-run claim or lease. fern up's host lease makes it the workspace's only
// coordinator.
//
// It is an observe-and-act loop. RunOnce reconciles the next run's current
// phase in one pass: provisioning ensures clone, volume, container, start,
// health, route, and session, then fences and dispatches the prompt; cleaning
// drains the route, stops the exact writer, and removes container, volume, and
// clone. Each step inspects deterministic resources before acting, so a pass
// cut short by failure, crash, or the operation deadline is simply repeated.
// store owns transition legality, and every durable write is a revision
// compare-and-swap. Passes are bounded by the operation timeout (at most five
// minutes) and, while executing, the run deadline. Recovery and final
// result writes ignore caller cancellation but remain bounded by the operation
// timeout.
//
// The few non-reconcilable effects keep durable records. The started runtime
// is recorded before anything uses it. The one-way prompt-request fence is
// committed before the single admission call; afterwards the prompt is only
// reconciled against bounded history, and anything short of confirmed
// admission is recorded as uncertain. Unknown working observations record
// nothing rather than inferring idle or success. Changed execution
// configuration requests cleanup instead of adopting it.
//
// GitHub credentials are refreshed only during execution and never gate
// teardown; remote publication is the agent's own action, and there is no host
// publisher. Sealing records a writer fence once, then exports under it: the
// selected result is durable, and CAS install and materialization are
// re-derived and checked against it until the result is sealed. Snapshot
// mismatch records the run's last error for a retry rather than accepting
// different content. A run becomes terminal only once every resource is proven
// absent. The coordinator does not close its dependencies; composition stops
// it first.
package backgroundruncoord
