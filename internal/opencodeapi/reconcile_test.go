package opencodeapi

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestReconcilePromptExactInboxAndProjectedMessage(t *testing.T) {
	t.Parallel()
	resume := true
	tests := []struct {
		name     string
		inbox    string
		message  string
		want     PromptObservation
		admitted bool
	}{
		{
			name:     "pending exact",
			inbox:    `{"data":[{"id":"msg_one","sessionID":"ses_one","type":"user","delivery":"steer","payload":{"text":"do it"},"timeCreated":10}]}`,
			message:  `not found`,
			want:     PromptObservation{Session: MatchExact, Inbox: MatchExact, Message: MatchAbsent, Resume: MatchUnobservable},
			admitted: true,
		},
		{
			name:     "projected exact",
			inbox:    `{"data":[]}`,
			message:  `{"data":{"id":"msg_one","type":"user","text":"do it","time":{"created":10}}}`,
			want:     PromptObservation{Session: MatchExact, Inbox: MatchAbsent, Message: MatchExact, Resume: MatchUnobservable},
			admitted: true,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := reconciliationServer(t, test.inbox, test.message)
			defer server.Close()
			got, err := testClient(t, server).ReconcilePrompt(deadlineContext(t), "ses_one", PromptRequest{ID: "msg_one", Text: "do it", Resume: &resume})
			if err != nil || got != test.want || got.Admitted() != test.admitted {
				t.Fatalf("ReconcilePrompt = %+v, %v, admitted=%v", got, err, got.Admitted())
			}
		})
	}
}

func TestReconcileSessionCompleteTuple(t *testing.T) {
	t.Parallel()
	expected := CreateSessionRequest{ID: "ses_one", Title: "Fix it", Agent: "build", Model: &Model{ProviderID: "test", ID: "test-model"}, Location: &Location{Directory: "/workspace"}}
	tests := []struct {
		name string
		body string
		want MatchState
	}{
		{"exact", `{"data":{"id":"ses_one","title":"Fix it","agent":"build","model":{"providerID":"test","id":"test-model","variant":"default"},"location":{"directory":"/workspace"}}}`, MatchExact},
		{"title conflict", `{"data":{"id":"ses_one","title":"Other","agent":"build","model":{"providerID":"test","id":"test-model"},"location":{"directory":"/workspace"}}}`, MatchConflict},
		{"agent conflict", `{"data":{"id":"ses_one","title":"Fix it","agent":"other","model":{"providerID":"test","id":"test-model"},"location":{"directory":"/workspace"}}}`, MatchConflict},
		{"provider conflict", `{"data":{"id":"ses_one","title":"Fix it","agent":"build","model":{"providerID":"other","id":"test-model"},"location":{"directory":"/workspace"}}}`, MatchConflict},
		{"model conflict", `{"data":{"id":"ses_one","title":"Fix it","agent":"build","model":{"providerID":"test","id":"other"},"location":{"directory":"/workspace"}}}`, MatchConflict},
		{"location conflict", `{"data":{"id":"ses_one","title":"Fix it","agent":"build","model":{"providerID":"test","id":"test-model"},"location":{"directory":"/other"}}}`, MatchConflict},
		{"missing model", `{"data":{"id":"ses_one","title":"Fix it","agent":"build","location":{"directory":"/workspace"}}}`, MatchConflict},
		{"absent", `not found`, MatchAbsent},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				if test.body == "not found" {
					writer.WriteHeader(http.StatusNotFound)
					return
				}
				writeJSON(t, writer, test.body)
			}))
			defer server.Close()
			got, err := testClient(t, server).ReconcileSession(deadlineContext(t), expected)
			if err != nil || got != test.want {
				t.Fatalf("ReconcileSession = %s, %v", got, err)
			}
		})
	}
}

func TestCreateSessionRejectsSemanticMismatchAndInvalidInput(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writeJSON(t, writer, `{"data":{"id":"ses_one","title":"Other","agent":"build","model":{"providerID":"test","id":"test-model"},"location":{"directory":"/workspace"}}}`)
	}))
	defer server.Close()
	client := testClient(t, server)
	expected := CreateSessionRequest{ID: "ses_one", Title: "Fix it", Agent: "build", Model: &Model{ProviderID: "test", ID: "test-model"}, Location: &Location{Directory: "/workspace"}}
	if _, err := client.CreateOrReuseSession(deadlineContext(t), expected); !errors.Is(err, ErrProtocolConflict) {
		t.Fatalf("semantic mismatch error = %v", err)
	}
	invalid := expected
	invalid.Location = &Location{Directory: "/workspace/../other"}
	if _, err := client.CreateOrReuseSession(deadlineContext(t), invalid); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("invalid location error = %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("invalid request reached server: calls=%d", calls.Load())
	}
}

func TestReconcilePromptClassifiesSemanticConflicts(t *testing.T) {
	t.Parallel()
	resume := true
	tests := []struct {
		name    string
		inbox   string
		message string
		want    PromptObservation
	}{
		{
			name:    "pending text",
			inbox:   `{"data":[{"id":"msg_one","sessionID":"ses_one","type":"user","delivery":"steer","payload":{"text":"changed"},"timeCreated":10}]}`,
			message: `not found`,
			want:    PromptObservation{Session: MatchExact, Inbox: MatchConflict, Message: MatchAbsent, Resume: MatchUnobservable},
		},
		{
			name:    "projected text",
			inbox:   `{"data":[]}`,
			message: `{"data":{"id":"msg_one","type":"user","text":"changed","time":{"created":10}}}`,
			want:    PromptObservation{Session: MatchExact, Inbox: MatchAbsent, Message: MatchConflict, Resume: MatchUnobservable},
		},
		{
			name:    "projected type",
			inbox:   `{"data":[]}`,
			message: `{"data":{"id":"msg_one","type":"assistant","text":"do it","time":{"created":10}}}`,
			want:    PromptObservation{Session: MatchExact, Inbox: MatchAbsent, Message: MatchConflict, Resume: MatchUnobservable},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := reconciliationServer(t, test.inbox, test.message)
			defer server.Close()
			got, err := testClient(t, server).ReconcilePrompt(deadlineContext(t), "ses_one", PromptRequest{ID: "msg_one", Text: "do it", Resume: &resume})
			if err != nil || got != test.want || got.Admitted() {
				t.Fatalf("ReconcilePrompt = %+v, %v", got, err)
			}
		})
	}
}

func TestReconcilePromptRejectsForeignInboxOwner(t *testing.T) {
	t.Parallel()
	resume := true
	server := reconciliationServer(t,
		`{"data":[{"id":"msg_one","sessionID":"ses_other","type":"user","delivery":"steer","payload":{"text":"do it"},"timeCreated":10}]}`,
		`not found`,
	)
	defer server.Close()
	if _, err := testClient(t, server).ReconcilePrompt(deadlineContext(t), "ses_one", PromptRequest{ID: "msg_one", Text: "do it", Resume: &resume}); !errors.Is(err, ErrProtocolConflict) {
		t.Fatalf("foreign inbox owner error = %v", err)
	}
}

func TestReconcilePromptAbsentSessionAndContradiction(t *testing.T) {
	t.Parallel()
	resume := true
	var calls atomic.Int32
	absent := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writer.WriteHeader(http.StatusNotFound)
	}))
	defer absent.Close()
	got, err := testClient(t, absent).ReconcilePrompt(deadlineContext(t), "ses_one", PromptRequest{ID: "msg_one", Text: "do it", Resume: &resume})
	if err != nil || got != (PromptObservation{Session: MatchAbsent, Inbox: MatchAbsent, Message: MatchAbsent, Resume: MatchAbsent}) || calls.Load() != 1 {
		t.Fatalf("absent reconciliation = %+v, %v, calls=%d", got, err, calls.Load())
	}

	contradictory := reconciliationServer(t,
		`{"data":[{"id":"msg_one","sessionID":"ses_one","type":"user","delivery":"steer","payload":{"text":"do it"},"timeCreated":10}]}`,
		`{"data":{"id":"msg_one","type":"user","text":"do it","time":{"created":10}}}`,
	)
	defer contradictory.Close()
	got, err = testClient(t, contradictory).ReconcilePrompt(deadlineContext(t), "ses_one", PromptRequest{ID: "msg_one", Text: "do it", Resume: &resume})
	if !errors.Is(err, ErrProtocolConflict) || got.Inbox != MatchExact || got.Message != MatchExact || got.Admitted() {
		t.Fatalf("contradictory reconciliation = %+v, %v", got, err)
	}
}

func TestReconcilePromptRequiresExplicitResumeWithoutSending(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer server.Close()
	client := testClient(t, server)
	for _, request := range []PromptRequest{
		{ID: "msg_one", Text: "do it"},
		{ID: "msg_one", Text: ""},
		{ID: "msg_one", Text: strings.Repeat("x", MaxPromptTextBytes+1)},
	} {
		if _, err := client.ReconcilePrompt(deadlineContext(t), "ses_one", request); err == nil {
			t.Fatalf("invalid reconciliation request accepted: %+v", request)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("invalid reconciliation sent %d requests", calls.Load())
	}
}

func TestReconcilePromptRejectsProjectedResumeFalse(t *testing.T) {
	t.Parallel()
	resume := false
	server := reconciliationServer(t, `{"data":[]}`, `{"data":{"id":"msg_one","type":"user","text":"do it","time":{"created":10}}}`)
	defer server.Close()
	got, err := testClient(t, server).ReconcilePrompt(deadlineContext(t), "ses_one", PromptRequest{ID: "msg_one", Text: "do it", Resume: &resume})
	if err != nil || got.Message != MatchConflict || got.Admitted() {
		t.Fatalf("resume-false projection = %+v, %v", got, err)
	}
}

func reconciliationServer(t *testing.T, inbox, message string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.URL.Path == "/api/session/ses_one":
			writeJSON(t, writer, `{"data":{"id":"ses_one"}}`)
		case strings.HasSuffix(request.URL.Path, "/inbox"):
			writeJSON(t, writer, inbox)
		case strings.HasSuffix(request.URL.Path, "/message/msg_one"):
			if message == "not found" {
				writer.WriteHeader(http.StatusNotFound)
				return
			}
			writeJSON(t, writer, message)
		default:
			t.Errorf("unexpected reconciliation route %s", request.URL.Path)
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
}
