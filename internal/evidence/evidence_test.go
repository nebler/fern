package evidence

import (
	"encoding/json"
	"testing"
)

func TestContainsSensitiveKey(t *testing.T) {
	t.Parallel()
	var decoded map[string]any
	if err := json.Unmarshal([]byte(`{"stage":"push","exitCode":0,"nested":{"raw_prompt":"x"}}`), &decoded); err != nil {
		t.Fatal(err)
	}
	if !ContainsSensitiveKey(decoded) {
		t.Fatal("ContainsSensitiveKey missed a nested sensitive key")
	}
	for _, payload := range []string{
		`{"authorization":"Bearer x"}`,
		`{"set-cookie":"a=b"}`,
		`{"ResponseBody":"..."}`,
		`{"items":[{"token":"t"}]}`,
		`{"RAW-BODY":"x"}`,
	} {
		var candidate map[string]any
		if err := json.Unmarshal([]byte(payload), &candidate); err != nil {
			t.Fatal(err)
		}
		if !ContainsSensitiveKey(candidate) {
			t.Errorf("ContainsSensitiveKey missed %s", payload)
		}
	}
	var benign map[string]any
	if err := json.Unmarshal([]byte(`{"stage":"push","bytes":10}`), &benign); err != nil {
		t.Fatal(err)
	}
	if ContainsSensitiveKey(benign) {
		t.Fatal("ContainsSensitiveKey flagged benign evidence")
	}
}
