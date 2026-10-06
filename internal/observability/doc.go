// Package observability provides Fern's bounded in-process readiness registry
// and its liveness, readiness, status, and metrics HTTP handlers. cmd/fern owns
// the registry and updates component states; proxy mounts the handlers on the
// operator surface. State is in memory and lost on restart: this is service
// health, not the durable run ledger.
//
// The component set is fixed at compile time and unknown components are
// rejected. Only blocked and failed make a component unready; degraded stays
// ready but counts as a failure. Error values passed to the state methods are
// deliberately discarded and replaced by fixed details, so secrets and
// unbounded label values can never reach probes or metrics.
package observability
