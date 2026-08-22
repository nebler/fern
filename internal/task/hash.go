package task

import (
	"encoding/hex"
	"fmt"
	"strings"
)

// RequestHash is a caller-computed SHA-256 over the canonical request. This
// package does not implement JSON canonicalization and must not hash raw JSON.
type RequestHash [32]byte

func ParseRequestHash(v string) (RequestHash, error) {
	var h RequestHash
	if len(v) != 64 || v != strings.ToLower(v) {
		return h, fmt.Errorf("%w: expected lowercase SHA-256", ErrInvalidHash)
	}
	b, err := hex.DecodeString(v)
	if err != nil {
		return h, fmt.Errorf("%w: expected SHA-256", ErrInvalidHash)
	}
	copy(h[:], b)
	return h, nil
}

func (h RequestHash) String() string { return hex.EncodeToString(h[:]) }
