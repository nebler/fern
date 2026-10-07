// Package domain is Fern's dependency-free vocabulary for runs: typed
// identifiers, actor attribution, idempotency comparison, secure ID and secret
// generation, run lifecycle states and phases, canonical resource names,
// runtime identity, and Git/GitHub reference validation. It contains no
// persistence, transport, or external-authority logic, so ingress, storage,
// the Docker provider, and artifact code can agree on identities and policy
// without depending on each other or on SQLite.
//
// # Identifiers and actors
//
// Typed string casts do not validate. Parse* functions belong at boundaries
// (HTTP input, configuration); IDs Fern generated itself flow as typed values
// and are not re-parsed downstream. ContextActor checks structure, not
// credentials, so only authenticated ingress may call WithActor. Display name
// and request ID are excluded from authority equivalence.
//
// Request hashes are opaque 32-byte caller-computed values; this package never
// canonicalizes JSON. ClassifyIdempotency compares ownership before hashes so
// it cannot disclose another actor's request equality.
//
// # Lifecycle
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
// Resource names identify a run, not a particular process: Runtime does that,
// hashing the container ID with its exact original start timestamp, so a
// restarted container is a different runtime. Non-canonical timestamp
// spellings are rejected because normalizing them would change persisted
// identity.
//
// # Git references
//
// The Validate*/Valid* reference functions are the single source of truth for
// Git references, SHA-1 object IDs, GitHub owner/repository names, canonical
// GitHub remotes, and repository-relative paths. They guard security-sensitive
// boundaries such as GitHub API routes, base branch names, and result manifest
// paths, so packages must delegate here instead of keeping private copies
// whose rules can drift apart. The rules are Fern policy, deliberately more
// conservative than Git (for example the 255-byte ref limit), not an
// equivalence test against Git itself. ValidateGitHubRemote accepts only the
// exact spelling https://github.com/OWNER/REPOSITORY with no .git suffix, and
// is the single remote-identity check for configuration, run admission, and
// the Docker provider. Path checks are lexical: they never touch the
// filesystem or resolve symlinks. Validate* functions return errors; Valid*
// functions return booleans.
package domain
