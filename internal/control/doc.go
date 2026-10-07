// Package control owns one workspace's paired browser devices and its stable
// operator credential identifier. It is not HTTP authentication middleware;
// pairing belongs to proxy and plugin grants to pluginauth.
//
// State lives in the devices and operator_credential tables of Fern's SQLite
// database (schema owned by taskstore). Raw device bearer tokens are never
// persisted; devices are keyed by SHA-256 digest and the device ID is that
// digest's prefix. Operator credential IDs are random audit identifiers, not
// secrets or password hashes.
//
// Some apparent reads write: AuthenticateDeviceIdentity refreshes LastSeen at
// most hourly and removes a matched expired device, and Devices deletes expired
// devices, so both can return I/O errors. RevokeDevice only persists removal;
// callers must then call CancelDeviceRequests to cancel in-flight work
// registered through RegisterDeviceRequest. That registry is in memory, and the
// store's mutex fences registration against revocation within the process.
package control
