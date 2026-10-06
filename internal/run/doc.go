// Package run is the storage-independent owner of background-run execution
// vocabulary: lifecycle states and phases, canonical resource names, and exact
// runtime identity. Docker providers, routing, the coordinator, and taskstore
// share this policy without depending on each other or on SQLite.
//
// Classify is a policy query, not a transition engine: it reports whether a
// state/phase pair is valid, enforces the attempt deadline and execution
// configuration, is timeout-eligible, or is a retryable cleanup step.
// Revisions, evidence, and persistence belong to callers. Recovery, sealing,
// export, and cleanup may outlive the attempt deadline, and ResultReady does
// not mean cleanup is complete.
//
// ResourceSpecVersion and SourceProfile pin the current execution contract;
// recognizing an older provider resource is not permission to start it.
// Resource names identify a task generation, not a particular process: Runtime
// does that, hashing the container ID with its exact original start timestamp,
// so a restarted container is a different runtime. Non-canonical timestamp
// spellings are rejected because normalizing them would change persisted
// identity.
package run
