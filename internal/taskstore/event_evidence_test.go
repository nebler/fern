package taskstore

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestRetainedResultEvidenceRejectsInvalidPayloads(t *testing.T) {
	t.Parallel()
	for name, evidence := range map[string]json.RawMessage{
		"array":     json.RawMessage(`[]`),
		"malformed": json.RawMessage(`{"stage":`),
		"sensitive": json.RawMessage(`{"nested":{"raw_prompt":"secret"}}`),
		"oversized": json.RawMessage(`{"stage":"` + strings.Repeat("x", maxRetainedResultEvidenceBytes) + `"}`),
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateRetainedResultEvidence(evidence, sha256.Sum256(evidence)); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("invalid evidence: %v", err)
			}
		})
	}
	if err := validateRetainedResultEvidence(json.RawMessage(`{}`), [32]byte{}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("mismatched evidence digest: %v", err)
	}
	valid := json.RawMessage(`{"stage":"retained"}`)
	if err := validateRetainedResultEvidence(valid, sha256.Sum256(valid)); err != nil {
		t.Fatalf("valid evidence: %v", err)
	}
}

func TestContainsSensitiveEvidenceKey(t *testing.T) {
	t.Parallel()
	for _, payload := range []string{
		`{"nested":{"raw_prompt":"x"}}`,
		`{"authorization":"Bearer x"}`,
		`{"set-cookie":"a=b"}`,
		`{"ResponseBody":"..."}`,
		`{"items":[{"token":"t"}]}`,
		`{"RAW-BODY":"x"}`,
	} {
		var candidate any
		if err := json.Unmarshal([]byte(payload), &candidate); err != nil {
			t.Fatal(err)
		}
		if !containsSensitiveEvidenceKey(candidate) {
			t.Errorf("sensitive key missed in %s", payload)
		}
	}
	var benign any
	if err := json.Unmarshal([]byte(`{"stage":"push","bytes":10}`), &benign); err != nil {
		t.Fatal(err)
	}
	if containsSensitiveEvidenceKey(benign) {
		t.Fatal("benign evidence was rejected")
	}
}
