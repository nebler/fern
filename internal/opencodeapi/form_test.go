package opencodeapi

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFormOwnershipIdentityAndDuplicateValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		response string
		read     bool
	}{
		{name: "list ownership", response: `{"data":[{"id":"frm_one","sessionID":"ses_other","metadata":{"kind":"question"},"fields":[]}]}`},
		{name: "list duplicate", response: `{"data":[{"id":"frm_one","sessionID":"ses_one","metadata":{"kind":"question"},"fields":[]},{"id":"frm_one","sessionID":"ses_one","metadata":{"kind":"question"},"fields":[]}]}`},
		{name: "list incompatible duplicate", response: `{"data":[{"id":"frm_one","sessionID":"ses_one","metadata":{"kind":"one"},"fields":[]},{"id":"frm_one","sessionID":"ses_one","metadata":{"kind":"two"},"fields":[]}]}`},
		{name: "read identity", response: `{"data":{"id":"frm_other","sessionID":"ses_one","metadata":{"kind":"question"},"fields":[]}}`, read: true},
		{name: "read ownership", response: `{"data":{"id":"frm_one","sessionID":"ses_other","metadata":{"kind":"question"},"fields":[]}}`, read: true},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writeJSON(t, writer, test.response)
			}))
			defer server.Close()
			client := testClient(t, server)
			var err error
			if test.read {
				_, err = client.ReadForm(deadlineContext(t), "ses_one", "frm_one")
			} else {
				_, err = client.ListForms(deadlineContext(t), "ses_one")
			}
			if !errors.Is(err, ErrProtocolConflict) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestReadFormRejectsUnsafeIDBeforeNetwork(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("invalid form ID reached server")
	}))
	defer server.Close()
	client := testClient(t, server)
	if _, err := client.ReadForm(deadlineContext(t), "ses_one", "bad/id"); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("error = %v", err)
	}
}
