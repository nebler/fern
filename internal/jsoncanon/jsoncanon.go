// Package jsoncanon is the single source of truth for strictly scanning
// untrusted JSON payloads before they are decoded into application types.
// Every remote or stored response that feeds a security-sensitive decoder is
// checked here so duplicate object keys (compared case-insensitively, the way
// downstream merge logic resolves them), excessive nesting, invalid UTF-8,
// and trailing garbage are rejected exactly once and identically everywhere.
package jsoncanon

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode/utf8"
)

var (
	errEncoding = errors.New("invalid JSON encoding")
	errTrailing = errors.New("invalid trailing JSON")
)

// Check verifies that payload holds exactly one valid UTF-8 JSON value whose
// object keys are unique case-insensitively, whose nesting never exceeds
// maxDepth, and which carries no trailing content after the top-level value.
// Malformed bytes surface as the underlying encoding/json scanner error;
// structural violations use the descriptive errors documented on each branch
// below. Callers wrap or map these errors with their own context.
func Check(payload []byte, maxDepth int) error {
	if len(payload) == 0 || !utf8.Valid(payload) {
		return errEncoding
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	if err := scan(decoder, 0, maxDepth); err != nil {
		return err
	}
	if token, err := decoder.Token(); err != io.EOF || token != nil {
		return errTrailing
	}
	return nil
}

func scan(decoder *json.Decoder, depth, maxDepth int) error {
	if depth > maxDepth {
		return errors.New("JSON nesting exceeds limit")
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		keys := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			key, ok := keyToken.(string)
			if err != nil || !ok {
				return errors.New("invalid JSON object key")
			}
			canonical := strings.ToLower(key)
			if _, exists := keys[canonical]; exists {
				return errors.New("duplicate JSON object key")
			}
			keys[canonical] = struct{}{}
			if err := scan(decoder, depth+1, maxDepth); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return errors.New("invalid JSON object")
		}
	case '[':
		for decoder.More() {
			if err := scan(decoder, depth+1, maxDepth); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return errors.New("invalid JSON array")
		}
	default:
		return errors.New("invalid JSON delimiter")
	}
	return nil
}
