// Package control owns one workspace's durable browser-device identities and
// its stable operator credential identifier. It is a private JSON state store
// (schema 2), not HTTP authentication middleware or a task database. Historical
// schemas are rejected without migration. Pairing state belongs to proxy, and
// plugin state to pluginauth (AuxiliaryStatePath only names a sibling file).
//
// Raw device bearer tokens are never persisted; devices are keyed by SHA-256
// digest. Operator credential IDs are random audit identifiers, not secrets or
// password hashes.
//
// Every mutation rewrites the whole file under one in-process mutex with
// atomicfile.Write. Failures before the rename roll back memory; a
// directory-sync failure after it (atomicfile.ErrNotDurable) is an uncertain
// commit and is not rolled back. The mutex does not coordinate separate Store
// instances or processes.
//
// Some apparent reads write: AuthenticateDeviceIdentity refreshes LastSeen at
// most hourly and removes a matched expired device, and Devices persists
// pruning, so both can return I/O errors. RevokeDevice only persists removal;
// callers must then call CancelDeviceRequests to cancel registered in-flight
// work.
package control
