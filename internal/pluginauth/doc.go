// Package pluginauth owns the fixed-scope OpenCode plugin device-authorization
// protocol and its durable grants. HTTP routing lives in proxy; GitHub
// credentials are unrelated and never appear in plugin grants.
//
// The scope set is closed (run:create, run:read, run:stop, run:attach,
// run:result). The plugin bearer is independent of paired-browser cookies and
// the operator password, and approval requires an authenticated device or
// operator actor. Only domain-separated SHA-256 digests of device and user codes
// are persisted; on approval the HTTP adapter returns the original device code
// as the bearer, so Poll never mints a new secret.
//
// State lives in the plugin_* tables of Fern's SQLite database (schema owned by
// store). Each operation is one transaction that rolls back on any error;
// time-driven transitions (expiry, retention pruning, the invalid-poll window)
// are applied inside the operation that observes them. Pending and active
// records are never evicted to admit new requests.
//
// Poll and Credentials can write (rate-limit timing, expiry), so their errors
// must be handled; Authenticate only reads. Revoke durably transitions first,
// then cancels requests registered through RegisterRequest; the in-process
// mutex fences that registry against revocation. The request-context helpers
// trust their caller: only middleware that already verified the bearer may
// install them.
package pluginauth
