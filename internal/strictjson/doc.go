// Package strictjson is the single source of truth for strictly scanning
// untrusted JSON payloads before they are decoded into application types.
// Every remote or stored response that feeds a security-sensitive decoder is
// checked here so duplicate object keys (compared case-insensitively, the way
// downstream merge logic resolves them), excessive nesting, invalid UTF-8,
// and trailing garbage are rejected exactly once and identically everywhere.
//
// Check is a validator only, not a canonical serializer or digest. Passing it
// does not mean a payload satisfies any schema: callers still own size limits,
// typed decoding with unknown fields disallowed, and mapping errors to their
// public contracts.
package strictjson
