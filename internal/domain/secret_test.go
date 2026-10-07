package domain

import (
	"encoding/base64"
	"testing"
)

func TestNewSecretIsUnpadded256BitBase64URL(t *testing.T) {
	first, second := NewSecret(), NewSecret()
	decoded, err := base64.RawURLEncoding.DecodeString(first)
	if err != nil || len(decoded) != 32 || len(first) != 43 {
		t.Fatalf("secret %q decodes to %d bytes: %v", first, len(decoded), err)
	}
	if first == second {
		t.Fatal("two secrets are equal")
	}
}

func TestSecureGeneratorProducesParseableIDs(t *testing.T) {
	id, err := NewSecureGenerator().WorkspaceID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseWorkspaceID(string(id)); err != nil {
		t.Fatalf("workspace ID %q: %v", id, err)
	}
}
