// Package evidence centralizes the redaction rule applied to task evidence:
// any structured payload containing a key that could carry sensitive content
// (prompts, credentials, tokens, raw request/response bodies) must be rejected
// before it is stored, accepted, or published. Keeping the key list in one
// place guarantees the collector and the store fail closed on the same inputs.
package evidence

import "strings"

// sensitiveKeyReplacer normalizes separators out of candidate keys so variants
// such as "raw_prompt", "raw-prompt", and "raw.prompt" all match.
var sensitiveKeyReplacer = strings.NewReplacer("_", "", "-", "", ".", "")

// ContainsSensitiveKey reports whether value is, or transitively contains, an
// object whose key names sensitive content.
func ContainsSensitiveKey(value any) bool {
	switch value := value.(type) {
	case map[string]any:
		for key, child := range value {
			normalized := sensitiveKeyReplacer.Replace(strings.ToLower(key))
			switch normalized {
			case "prompt", "rawprompt", "credential", "credentials", "authorization", "token", "cookie", "setcookie", "body", "rawbody", "requestbody", "responsebody":
				return true
			}
			if ContainsSensitiveKey(child) {
				return true
			}
		}
	case []any:
		for _, child := range value {
			if ContainsSensitiveKey(child) {
				return true
			}
		}
	}
	return false
}
