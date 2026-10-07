package domain

import "encoding/hex"

// RequestHash is a caller-computed SHA-256 over the canonical request. This
// package does not implement JSON canonicalization and must not hash raw JSON.
type RequestHash [32]byte

func (h RequestHash) String() string { return hex.EncodeToString(h[:]) }
