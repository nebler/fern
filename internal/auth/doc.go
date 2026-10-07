// Package auth owns Fern's durable caller identities: one workspace's paired
// browser devices and stable operator credential identifier (DeviceStore), and
// the fixed-scope OpenCode plugin device-authorization protocol and its grants
// (PluginStore). It is not HTTP authentication middleware: pairing and HTTP
// routing belong to web. GitHub credentials are unrelated and never appear in
// plugin grants.
//
// State lives in the devices, operator_credential, and plugin_* tables of
// Fern's SQLite database (schema owned by store). Each store issues its own
// SQL through the shared handle.
//
// # Devices
//
// Raw device bearer tokens are never persisted; devices are keyed by SHA-256
// digest and the device ID is that digest's prefix. Operator credential IDs
// are random audit identifiers, not secrets or password hashes.
//
// Some apparent reads write: AuthenticateDeviceIdentity refreshes LastSeen at
// most hourly and removes a matched expired device, and Devices deletes
// expired devices, so both can return I/O errors. RevokeDevice only persists
// removal; callers must then call CancelDeviceRequests to cancel in-flight
// work registered through RegisterDeviceRequest. That registry is in memory,
// and the store's mutex fences registration against revocation within the
// process.
//
// # Plugins
//
// The scope set is closed (run:create, run:read, run:stop, run:attach,
// run:result). The plugin bearer is independent of paired-browser cookies and
// the operator password, and approval requires an authenticated device or
// operator actor. Only domain-separated SHA-256 digests of device and user
// codes are persisted; on approval the HTTP adapter returns the original
// device code as the bearer, so Poll never mints a new secret.
//
// Each PluginStore operation is one transaction that rolls back on any error;
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
package auth
