// Package run is the storage-independent owner of background-run execution
// vocabulary: lifecycle states and phases, canonical resource names, and exact
// runtime identity. Docker providers, routing, the coordinator, and taskstore
// share this policy without depending on each other or on SQLite.
//
// Phases are the durable steps of a reconcile loop, not a record of each
// effect: absent, provisioning, prompt_pending, admitted, sealing, cleaning,
// cleanup_complete. Classify is a policy query, not a transition engine: it
// reports whether a state/phase pair is valid, executing (bound by the run
// deadline and execution configuration), or timeout-eligible. Revisions,
// evidence, and persistence belong to callers. Sealing and cleanup outlive the
// run deadline, and ResultReady does not mean cleanup is complete.
//
// ResourceSpecVersion and SourceProfile pin the one execution contract.
// Resource names identify a run, not a particular process: Runtime
// does that, hashing the container ID with its exact original start timestamp,
// so a restarted container is a different runtime. Non-canonical timestamp
// spellings are rejected because normalizing them would change persisted
// identity.
package run
