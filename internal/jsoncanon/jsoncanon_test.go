package jsoncanon

import (
	"strings"
	"testing"
)

func TestCheckAcceptsCanonicalJSON(t *testing.T) {
	t.Parallel()
	payloads := []string{
		`{"id":1,"name":"fern"}`,
		`[1,2,[3,{"nested":null}]]`,
		`"scalar"`,
		`42`,
		`{"a":{"B":1},"b":2}`,
		"\n\t {\"spaced\": true} \n",
	}
	for _, payload := range payloads {
		if err := Check([]byte(payload), 64); err != nil {
			t.Errorf("Check(%q) = %v, want nil", payload, err)
		}
	}
}

func TestCheckRejectsDuplicateKeysCaseInsensitively(t *testing.T) {
	t.Parallel()
	for _, payload := range []string{`{"id":1,"id":2}`, `{"id":1,"ID":2}`, `{"outer":{"k":1,"K":2}}`} {
		err := Check([]byte(payload), 64)
		if err == nil || err.Error() != "duplicate JSON object key" {
			t.Errorf("Check(%q) = %v, want duplicate JSON object key", payload, err)
		}
	}
}

func TestCheckRejectsDepthBeyondLimit(t *testing.T) {
	t.Parallel()
	nested := strings.Repeat("[", 5) + strings.Repeat("]", 5)
	if err := Check([]byte(nested), 64); err != nil {
		t.Fatalf("Check(nested within limit) = %v", err)
	}
	if err := Check([]byte(nested), 3); err == nil || err.Error() != "JSON nesting exceeds limit" {
		t.Fatalf("Check(nested beyond limit) = %v", err)
	}
}

func TestCheckRejectsEncodingAndStructureViolations(t *testing.T) {
	t.Parallel()
	invalidUTF8 := append([]byte(`{"id":"`), 0xff)
	invalidUTF8 = append(invalidUTF8, []byte(`"}`)...)
	cases := []struct {
		payload []byte
		want    string
	}{
		{nil, "invalid JSON encoding"},
		{[]byte("   "), "EOF"},
		{invalidUTF8, "invalid JSON encoding"},
		{[]byte(`{"id":1} trailing`), "invalid trailing JSON"},
		{[]byte(`{}{}`), "invalid trailing JSON"},
		{[]byte(`{"id":`), "EOF"},
		{[]byte(`{"id" 1}`), "invalid character '1'"},
		{[]byte(`[1,2}`), "invalid JSON array"},
		{[]byte(`{"a":1]`), "invalid JSON object"},
	}
	for _, testCase := range cases {
		err := Check(testCase.payload, 64)
		if err == nil || !strings.Contains(err.Error(), testCase.want) {
			t.Errorf("Check(%q) = %v, want containing %q", testCase.payload, err, testCase.want)
		}
	}
}
