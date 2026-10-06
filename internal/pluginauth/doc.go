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
// State is a private auxiliary JSON file located through control.Store but
// versioned separately. Mutations rewrite the whole file under one in-process
// mutex with atomicfile.Write, like control; a directory-sync failure after
// rename is reported as uncertain, not rolled back. Pending and active records
// are never evicted to admit new requests.
//
// Poll, Authenticate, and Credentials can all write (rate-limit timing,
// expiry), so their errors must be handled. Revoke durably transitions first,
// then cancels requests registered through RegisterRequest. The request-context
// helpers trust their caller: only middleware that already verified the bearer
// may install them.
package pluginauth
