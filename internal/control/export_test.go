package control

import "time"

// authenticateDevice is the test-only boolean form of
// AuthenticateDeviceIdentity. Production callers use the identity-returning
// method directly; keeping this wrapper out of store.go avoids exporting a
// second authentication surface.
func authenticateDevice(store *Store, token string, now time.Time) (bool, error) {
	_, valid, err := store.AuthenticateDeviceIdentity(token, now)
	return valid, err
}
