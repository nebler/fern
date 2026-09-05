package taskstore

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestRetainedResultEvidencePayloadPreservesBytes(t *testing.T) {
	t.Parallel()
	evidence := json.RawMessage(" \n{\"stage\": \"retained\", \"items\": [1, 2]}\t ")
	digest := sha256.Sum256(evidence)
	payload, err := retainedResultEvidencePayload(evidence, digest)
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf(`{"evidence":%s,"evidenceSha256":"sha256:%x"}`, evidence, digest)
	if string(payload) != want {
		t.Fatalf("evidence encoding changed:\n got %q\nwant %q", payload, want)
	}
}

func TestRetainedResultEvidenceRejectsInvalidPayloads(t *testing.T) {
	t.Parallel()
	for name, evidence := range map[string]json.RawMessage{
		"array":     json.RawMessage(`[]`),
		"malformed": json.RawMessage(`{"stage":`),
		"sensitive": json.RawMessage(`{"nested":{"raw_prompt":"secret"}}`),
		"oversized": json.RawMessage(`{"stage":"` + strings.Repeat("x", maxRetainedResultEvidenceBytes) + `"}`),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := retainedResultEvidencePayload(evidence, sha256.Sum256(evidence)); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("invalid evidence: %v", err)
			}
		})
	}
	if _, err := retainedResultEvidencePayload(json.RawMessage(`{}`), [32]byte{}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("mismatched evidence digest: %v", err)
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
