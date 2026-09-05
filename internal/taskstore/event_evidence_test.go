package taskstore

import (
	"encoding/json"
	"testing"
)

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
