// Package observability provides Fern's in-process readiness registry and its
// liveness and readiness HTTP probes. cmd/fern owns the registry and updates
// component states; proxy mounts the probes on the operator surface. State is
// in memory and lost on restart: this is service health, not the durable run
// ledger.
//
// The component set is fixed at compile time and unknown components are
// rejected. Only blocked and failed make a component unready; healthy,
// qualified, and degraded components are ready. Error values passed to the
// state methods are discarded, so nothing secret can reach a probe.
package observability
