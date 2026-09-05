package githubapp

import (
	"errors"
	"strings"
	"testing"
)

func TestDecodeGitHubJSONRejectsAmbiguousAndMalformedResponses(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		payload string
	}{
		{name: "duplicate", payload: `{"id":1,"id":2}`},
		{name: "case folded duplicate", payload: `{"id":1,"ID":2}`},
		{name: "nested duplicate", payload: `{"extra":{"id":1,"id":2}}`},
		{name: "trailing JSON", payload: `{} {}`},
		{name: "malformed", payload: `{"id":`},
		{name: "invalid UTF8", payload: "{\"extra\":\"\xff\"}"},
		{name: "excessive depth", payload: `{"extra":` + strings.Repeat("[", maxJSONDepth+1) + "0" + strings.Repeat("]", maxJSONDepth+1) + "}"},
		{name: "wrong field type", payload: `{"id":"1"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var response struct {
				ID int64 `json:"id"`
			}
			if err := decodeGitHubJSON([]byte(test.payload), &response); !errors.Is(err, ErrInvalidResponse) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestDecodeGitHubJSONAllowsUnknownFields(t *testing.T) {
	t.Parallel()
	var response struct {
		ID int64 `json:"id"`
	}
	if err := decodeGitHubJSON([]byte(`{"id":1,"extra":{"value":true}}`), &response); err != nil || response.ID != 1 {
		t.Fatalf("response = %+v, error = %v", response, err)
	}
}
