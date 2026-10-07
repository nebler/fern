package domain

import (
	"crypto/rand"
	"encoding/base64"
)

// NewSecret returns 256 random bits, base64url-encoded without padding: the
// format of plugin device codes, pairing codes, device sessions, and run
// attachment passwords. crypto/rand never returns an error since Go 1.24 (it
// aborts the process instead).
func NewSecret() string {
	var secret [32]byte
	_, _ = rand.Read(secret[:])
	return base64.RawURLEncoding.EncodeToString(secret[:])
}
