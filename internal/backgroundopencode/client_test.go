package backgroundopencode

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testSession = "ses_0123456789abcdef0123456789abcdef"
	testMessage = "msg_0123456789abcdef0123456789abcdef"
	testSecret  = "secret-not-for-errors"
)

var testSessionSpec = SessionSpec{
	ID: testSession, Agent: "contract", ProviderID: "test", ModelID: "test-model", Directory: "/home/user/workspace",
}

func deadline(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func testClient(t *testing.T, handler http.Handler) (*Client, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := New(Config{Endpoint: server.URL, Username: "opencode", Password: testSecret, HTTPClient: &http.Client{Timeout: 3 * time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	return client, server
}

func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func sessionJSON(spec SessionSpec) string {
	return fmt.Sprintf(`{"data":{"id":%q,"projectID":"prj_contract","agent":%q,"model":{"id":%q,"providerID":%q,"variant":"default"},"cost":0,"tokens":{"input":0,"output":0,"reasoning":0,"cache":{"read":0,"write":0}},"time":{"created":1,"updated":1},"title":"Contract","location":{"directory":%q}}}`, spec.ID, spec.Agent, spec.ModelID, spec.ProviderID, spec.Directory)
}

func admittedEvent(seq int, sessionID, messageID, text, delivery string) string {
	return fmt.Sprintf(`{"id":"evt_%d","type":"session.next.prompt.admitted","durable":{"aggregateID":%q,"seq":%d,"version":1},"data":{"timestamp":1,"sessionID":%q,"messageID":%q,"prompt":{"text":%q},"delivery":%q}}`, seq, sessionID, seq, sessionID, messageID, text, delivery)
}

func promptedEvent(seq int, sessionID, messageID, text, delivery string) string {
	return strings.Replace(admittedEvent(seq, sessionID, messageID, text, delivery), "session.next.prompt.admitted", "session.next.prompted", 1)
}

func notFoundJSON(sessionID string) string {
	return fmt.Sprintf(`{"_tag":"SessionNotFoundError","sessionID":%q,"message":%q}`, sessionID, "Session not found: "+sessionID)
}

func conflictJSON(messageID string) string {
	return fmt.Sprintf(`{"_tag":"ConflictError","message":%q,"resource":%q}`, "Prompt message ID conflicts with an existing durable record: "+messageID, messageID)
}

func TestNewRequiresExactLoopbackAndBoundedClient(t *testing.T) {
	valid := Config{Endpoint: "http://127.0.0.1:4096", Username: "opencode", Password: "pw", HTTPClient: &http.Client{Timeout: time.Second}}
	if _, err := New(valid); err != nil {
		t.Fatal(err)
	}
	tests := []Config{
		{Endpoint: "http://localhost:4096", Username: "opencode", Password: "pw", HTTPClient: valid.HTTPClient},
		{Endpoint: "https://127.0.0.1:4096", Username: "opencode", Password: "pw", HTTPClient: valid.HTTPClient},
		{Endpoint: "http://127.0.0.1:4096/", Username: "opencode", Password: "pw", HTTPClient: valid.HTTPClient},
		{Endpoint: "http://127.0.0.1:4096?x=1", Username: "opencode", Password: "pw", HTTPClient: valid.HTTPClient},
		{Endpoint: "http://user@127.0.0.1:4096", Username: "opencode", Password: "pw", HTTPClient: valid.HTTPClient},
		{Endpoint: "http://192.0.2.1:4096", Username: "opencode", Password: "pw", HTTPClient: valid.HTTPClient},
		{Endpoint: valid.Endpoint, Username: "bad:name", Password: "pw", HTTPClient: valid.HTTPClient},
		{Endpoint: valid.Endpoint, Username: "opencode", Password: "pw"},
		{Endpoint: valid.Endpoint, Username: "opencode", Password: "pw", HTTPClient: &http.Client{}},
		{Endpoint: valid.Endpoint, Username: "opencode", Password: "pw", HTTPClient: &http.Client{Timeout: time.Minute}},
	}
	for i, input := range tests {
		if _, err := New(input); !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("case %d: error = %v", i, err)
		}
	}
}

func TestCreateSessionOnceExactRequestAndResponse(t *testing.T) {
	var calls atomic.Int32
	client, _ := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/api/session" || r.Header.Get("Accept") != "application/json" || r.Header.Get("Content-Type") != "application/json" || r.GetBody != nil {
			t.Errorf("request changed: method=%s path=%s getBody=%t", r.Method, r.URL.Path, r.GetBody != nil)
		}
		user, password, ok := r.BasicAuth()
		if !ok || user != "opencode" || password != testSecret {
			t.Error("Basic credentials missing")
		}
		body, _ := io.ReadAll(r.Body)
		want := `{"id":"ses_0123456789abcdef0123456789abcdef","agent":"contract","model":{"id":"test-model","providerID":"test"},"location":{"directory":"/home/user/workspace"}}`
		if string(body) != want {
			t.Errorf("body = %s", body)
		}
		writeJSON(w, http.StatusOK, sessionJSON(testSessionSpec))
	}))
	if err := client.CreateSessionOnce(deadline(t), testSessionSpec); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d", calls.Load())
	}
}

func TestStrictJSONAndEnvelope(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		body        string
	}{
		{"duplicate outer", "application/json", `{"data":{},"DATA":{}}`},
		{"duplicate nested", "application/json", `{"data":{"id":"a","ID":"b"}}`},
		{"malformed", "application/json", `{"data":`},
		{"trailing", "application/json", sessionJSON(testSessionSpec) + `{}`},
		{"unknown envelope", "application/json", `{"data":{},"extra":true}`},
		{"null", "application/json", `null`},
		{"parameterized content type", "application/json; charset=utf-8", sessionJSON(testSessionSpec)},
		{"two content types", "application/json", sessionJSON(testSessionSpec)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client, _ := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", test.contentType)
				if test.name == "two content types" {
					w.Header().Add("Content-Type", "application/json")
				}
				_, _ = io.WriteString(w, test.body)
			}))
			_, err := client.ReadSession(deadline(t), testSession)
			if !errors.Is(err, ErrProtocol) {
				t.Fatalf("error = %v", err)
			}
			if strings.Contains(fmt.Sprint(err), testSecret) || strings.Contains(fmt.Sprint(err), testSession) {
				t.Fatal("error exposed sensitive request data")
			}
		})
	}
}

func TestResponseBodyBound(t *testing.T) {
	client, _ := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, strings.Repeat("x", maxResponseBytes+1))
	}))
	_, err := client.ReadSession(deadline(t), testSession)
	if !errors.Is(err, ErrProtocol) {
		t.Fatalf("error = %v", err)
	}
}

func TestTypedNotFoundConflictAndSessionReconciliation(t *testing.T) {
	t.Run("not found", func(t *testing.T) {
		client, _ := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusNotFound, notFoundJSON(testSession))
		}))
		state, err := client.ReconcileSession(deadline(t), testSessionSpec)
		if err != nil || state != ReconcileAbsent {
			t.Fatalf("state=%s err=%v", state, err)
		}
	})
	t.Run("conflicting tuple", func(t *testing.T) {
		other := testSessionSpec
		other.Agent = "other"
		client, _ := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, sessionJSON(other)) }))
		state, err := client.ReconcileSession(deadline(t), testSessionSpec)
		if err != nil || state != ReconcileConflict {
			t.Fatalf("state=%s err=%v", state, err)
		}
	})
	t.Run("HTTP conflict", func(t *testing.T) {
		client, _ := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusConflict, conflictJSON(testMessage))
		}))
		err := client.AdmitPromptOnce(deadline(t), testSession, PromptSpec{ID: testMessage, Text: "do it", Delivery: "steer", Resume: false})
		var typed *ConflictError
		if !errors.Is(err, ErrConflict) || !errors.As(err, &typed) {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestStatusRequiresQualifiedTypedErrorAuthority(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		contentType string
		body        string
		conflict    bool
	}{
		{"not-found empty", 404, "application/json", "", false},
		{"not-found HTML", 404, "text/html", "<html>missing</html>", false},
		{"not-found wrong tag", 404, "application/json", `{"_tag":"MessageNotFoundError","sessionID":"` + testSession + `","message":"Session not found: ` + testSession + `"}`, false},
		{"not-found wrong identity", 404, "application/json", notFoundJSON("ses_other"), false},
		{"not-found duplicate", 404, "application/json", `{"_tag":"SessionNotFoundError","_TAG":"SessionNotFoundError","sessionID":"` + testSession + `","message":"Session not found: ` + testSession + `"}`, false},
		{"conflict empty", 409, "application/json", "", true},
		{"conflict HTML", 409, "text/html", "conflict", true},
		{"conflict wrong tag", 409, "application/json", `{"_tag":"InvalidRequestError","message":"bad","resource":"` + testMessage + `"}`, true},
		{"conflict wrong resource", 409, "application/json", conflictJSON("msg_other"), true},
		{"conflict duplicate", 409, "application/json", `{"_tag":"ConflictError","message":"x","resource":"` + testMessage + `","RESOURCE":"` + testMessage + `"}`, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client, _ := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", test.contentType)
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.body)
			}))
			var err error
			if test.conflict {
				err = client.AdmitPromptOnce(deadline(t), testSession, PromptSpec{ID: testMessage, Text: "do it", Delivery: "steer"})
			} else {
				_, err = client.ReadSession(deadline(t), testSession)
			}
			if !errors.Is(err, ErrProtocol) || errors.Is(err, ErrNotFound) || errors.Is(err, ErrConflict) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestPromptAdmissionRequiresExactSemantics(t *testing.T) {
	tests := []string{
		`{"data":{"admittedSeq":1,"id":"msg_other","sessionID":"` + testSession + `","prompt":{"text":"do it"},"delivery":"steer","timeCreated":1}}`,
		`{"data":{"admittedSeq":1,"id":"` + testMessage + `","sessionID":"ses_other","prompt":{"text":"do it"},"delivery":"steer","timeCreated":1}}`,
		`{"data":{"admittedSeq":1,"id":"` + testMessage + `","sessionID":"` + testSession + `","prompt":{"text":"changed"},"delivery":"steer","timeCreated":1}}`,
		`{"data":{"admittedSeq":1,"id":"` + testMessage + `","sessionID":"` + testSession + `","prompt":{"text":"do it"},"delivery":"queue","timeCreated":1}}`,
		`{"data":{"admittedSeq":1,"id":"` + testMessage + `","sessionID":"` + testSession + `","prompt":{"text":"do it"},"delivery":"steer","timeCreated":1,"promotedSeq":1}}`,
		`{"data":{"admittedSeq":2,"id":"` + testMessage + `","sessionID":"` + testSession + `","prompt":{"text":"do it"},"delivery":"steer","timeCreated":1,"promotedSeq":1}}`,
	}
	for i, body := range tests {
		client, _ := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, body) }))
		err := client.AdmitPromptOnce(deadline(t), testSession, PromptSpec{ID: testMessage, Text: "do it", Delivery: "steer", Resume: false})
		if !errors.Is(err, ErrProtocol) {
			t.Errorf("case %d: error = %v", i, err)
		}
	}
	t.Run("resume false accepts later promotion", func(t *testing.T) {
		body := `{"data":{"admittedSeq":1,"id":"` + testMessage + `","sessionID":"` + testSession + `","prompt":{"text":"do it"},"delivery":"steer","timeCreated":1,"promotedSeq":2}}`
		client, _ := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, body) }))
		if err := client.AdmitPromptOnce(deadline(t), testSession, PromptSpec{ID: testMessage, Text: "do it", Delivery: "steer", Resume: false}); err != nil {
			t.Fatal(err)
		}
	})
}

func TestReconcilePromptFiniteHistory(t *testing.T) {
	t.Run("exact across pages", func(t *testing.T) {
		var calls atomic.Int32
		client, _ := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			after := r.URL.Query().Get("after")
			switch after {
			case "0":
				writeJSON(w, 200, `{"data":[`+admittedEvent(1, testSession, "msg_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "other", "steer")+`],"hasMore":true}`)
			case "1":
				writeJSON(w, 200, `{"data":[`+admittedEvent(2, testSession, testMessage, "do it", "steer")+`],"hasMore":true}`)
			case "2":
				writeJSON(w, 200, `{"data":[`+promptedEvent(3, testSession, testMessage, "do it", "steer")+`],"hasMore":false}`)
			default:
				t.Errorf("unexpected after=%s", after)
			}
		}))
		state, err := client.ReconcilePrompt(deadline(t), testSession, PromptSpec{ID: testMessage, Text: "do it", Delivery: "steer", Resume: true}, HistoryBounds{PageLimit: 1, MaxPages: 4, MaxEvents: 4})
		if err != nil || state != ReconcileExact || calls.Load() != 3 {
			t.Fatalf("state=%s calls=%d err=%v", state, calls.Load(), err)
		}
	})
	t.Run("absent", func(t *testing.T) {
		client, _ := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, `{"data":[],"hasMore":false}`) }))
		state, err := client.ReconcilePrompt(deadline(t), testSession, PromptSpec{ID: testMessage, Text: "do it", Delivery: "steer"}, HistoryBounds{PageLimit: 2, MaxPages: 2, MaxEvents: 2})
		if err != nil || state != ReconcileAbsent {
			t.Fatalf("state=%s err=%v", state, err)
		}
	})
	t.Run("same ID conflicting text", func(t *testing.T) {
		client, _ := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, 200, `{"data":[`+admittedEvent(1, testSession, testMessage, "changed", "steer")+`],"hasMore":false}`)
		}))
		state, err := client.ReconcilePrompt(deadline(t), testSession, PromptSpec{ID: testMessage, Text: "do it", Delivery: "steer"}, HistoryBounds{1, 2, 2})
		if err != nil || state != ReconcileConflict {
			t.Fatalf("state=%s err=%v", state, err)
		}
	})
	t.Run("duplicate prompt identity", func(t *testing.T) {
		body := `{"data":[` + admittedEvent(1, testSession, testMessage, "do it", "steer") + `,` + admittedEvent(2, testSession, testMessage, "do it", "steer") + `],"hasMore":false}`
		client, _ := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, body) }))
		state, err := client.ReconcilePrompt(deadline(t), testSession, PromptSpec{ID: testMessage, Text: "do it", Delivery: "steer"}, HistoryBounds{2, 2, 3})
		if state != ReconcileUncertain || !errors.Is(err, ErrProtocol) {
			t.Fatalf("state=%s err=%v", state, err)
		}
	})
}

func TestReconcilePromptResumeAndIdentitySemantics(t *testing.T) {
	test := func(t *testing.T, body string, spec PromptSpec, want ReconcileState, target error) {
		t.Helper()
		client, _ := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, body) }))
		state, err := client.ReconcilePrompt(deadline(t), testSession, spec, HistoryBounds{PageLimit: 10, MaxPages: 2, MaxEvents: 20})
		if state != want || (target == nil && err != nil) || (target != nil && !errors.Is(err, target)) {
			t.Fatalf("state=%s err=%v", state, err)
		}
	}
	resume := PromptSpec{ID: testMessage, Text: "do it", Delivery: "steer", Resume: true}
	noResume := resume
	noResume.Resume = false
	t.Run("admitted not promoted", func(t *testing.T) {
		test(t, `{"data":[`+admittedEvent(1, testSession, testMessage, "do it", "steer")+`],"hasMore":false}`, resume, ReconcileAdmitted, nil)
	})
	t.Run("resume false exact admission", func(t *testing.T) {
		test(t, `{"data":[`+admittedEvent(1, testSession, testMessage, "do it", "steer")+`],"hasMore":false}`, noResume, ReconcileExact, nil)
	})
	t.Run("resume false accepts later promotion", func(t *testing.T) {
		body := `{"data":[` + admittedEvent(1, testSession, testMessage, "do it", "steer") + `,` + promptedEvent(2, testSession, testMessage, "do it", "steer") + `],"hasMore":false}`
		test(t, body, noResume, ReconcileExact, nil)
	})
	t.Run("promotion before admission", func(t *testing.T) {
		body := `{"data":[` + promptedEvent(1, testSession, testMessage, "do it", "steer") + `,` + admittedEvent(2, testSession, testMessage, "do it", "steer") + `],"hasMore":false}`
		test(t, body, resume, ReconcileUncertain, ErrProtocol)
	})
	t.Run("promotion differs", func(t *testing.T) {
		body := `{"data":[` + admittedEvent(1, testSession, testMessage, "do it", "steer") + `,` + promptedEvent(2, testSession, testMessage, "changed", "steer") + `],"hasMore":false}`
		test(t, body, resume, ReconcileConflict, nil)
	})
	t.Run("same identity attachment differs", func(t *testing.T) {
		event := strings.Replace(admittedEvent(1, testSession, testMessage, "do it", "steer"), `"prompt":{"text":"do it"}`, `"prompt":{"text":"do it","files":[{"uri":"file:///x"}]}`, 1)
		test(t, `{"data":[`+event+`],"hasMore":false}`, resume, ReconcileConflict, nil)
	})
	t.Run("identical duplicate promotion corrupt", func(t *testing.T) {
		body := `{"data":[` + admittedEvent(1, testSession, testMessage, "do it", "steer") + `,` + promptedEvent(2, testSession, testMessage, "do it", "steer") + `,` + promptedEvent(3, testSession, testMessage, "do it", "steer") + `],"hasMore":false}`
		test(t, body, resume, ReconcileUncertain, ErrProtocol)
	})
	t.Run("differing duplicate admission conflicts", func(t *testing.T) {
		body := `{"data":[` + admittedEvent(1, testSession, testMessage, "do it", "steer") + `,` + admittedEvent(2, testSession, testMessage, "changed", "steer") + `],"hasMore":false}`
		test(t, body, resume, ReconcileConflict, nil)
	})
}

func TestReconcilePromptUnrelatedCorruptionIsUncertain(t *testing.T) {
	other := "msg_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	test := func(t *testing.T, events string, spec PromptSpec) {
		t.Helper()
		client, _ := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, 200, `{"data":[`+events+`],"hasMore":false}`)
		}))
		state, err := client.ReconcilePrompt(deadline(t), testSession, spec, HistoryBounds{PageLimit: 10, MaxPages: 2, MaxEvents: 20})
		if state != ReconcileUncertain || !errors.Is(err, ErrProtocol) {
			t.Fatalf("state=%s err=%v", state, err)
		}
	}
	exact := PromptSpec{ID: testMessage, Text: "do it", Delivery: "steer", Resume: false}
	t.Run("differing duplicate admission", func(t *testing.T) {
		events := admittedEvent(1, testSession, testMessage, "do it", "steer") + `,` +
			admittedEvent(2, testSession, other, "first", "steer") + `,` + admittedEvent(3, testSession, other, "changed", "steer")
		test(t, events, exact)
	})
	t.Run("promotion conflicts with admission", func(t *testing.T) {
		events := admittedEvent(1, testSession, testMessage, "do it", "steer") + `,` +
			admittedEvent(2, testSession, other, "first", "steer") + `,` + promptedEvent(3, testSession, other, "changed", "steer")
		test(t, events, exact)
	})
	t.Run("target conflict cannot hide later malformed event", func(t *testing.T) {
		malformed := strings.Replace(admittedEvent(2, testSession, other, "other", "steer"), `"timestamp":1`, `"timestamp":-1`, 1)
		test(t, admittedEvent(1, testSession, testMessage, "changed", "steer")+`,`+malformed, exact)
	})
	t.Run("duplicate key in unrelated event", func(t *testing.T) {
		malformed := strings.Replace(admittedEvent(2, testSession, other, "other", "steer"), `"timestamp":1`, `"timestamp":1,"timestamp":1`, 1)
		test(t, admittedEvent(1, testSession, testMessage, "do it", "steer")+`,`+malformed, exact)
	})
}

func TestReconcilePromptRejectsPaginationLoopsAndBounds(t *testing.T) {
	t.Run("sequence does not advance", func(t *testing.T) {
		client, _ := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, 200, `{"data":[`+admittedEvent(1, testSession, "msg_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "other", "steer")+`],"hasMore":true}`)
		}))
		state, err := client.ReconcilePrompt(deadline(t), testSession, PromptSpec{ID: testMessage, Text: "do it", Delivery: "steer"}, HistoryBounds{1, 3, 3})
		if state != ReconcileUncertain || !errors.Is(err, ErrProtocol) {
			t.Fatalf("state=%s err=%v", state, err)
		}
	})
	t.Run("empty continuation", func(t *testing.T) {
		client, _ := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, `{"data":[],"hasMore":true}`) }))
		state, err := client.ReconcilePrompt(deadline(t), testSession, PromptSpec{ID: testMessage, Text: "do it", Delivery: "steer"}, HistoryBounds{1, 2, 2})
		if state != ReconcileUncertain || !errors.Is(err, ErrProtocol) {
			t.Fatalf("state=%s err=%v", state, err)
		}
	})
	t.Run("duplicate event identity", func(t *testing.T) {
		first := admittedEvent(1, testSession, "msg_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "first", "steer")
		second := strings.Replace(admittedEvent(2, testSession, "msg_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "second", "steer"), `"id":"evt_2"`, `"id":"evt_1"`, 1)
		client, _ := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, 200, `{"data":[`+first+`,`+second+`],"hasMore":false}`)
		}))
		state, err := client.ReconcilePrompt(deadline(t), testSession, PromptSpec{ID: testMessage, Text: "do it", Delivery: "steer"}, HistoryBounds{2, 2, 3})
		if state != ReconcileUncertain || !errors.Is(err, ErrProtocol) {
			t.Fatalf("state=%s err=%v", state, err)
		}
	})
	t.Run("page bound", func(t *testing.T) {
		client, _ := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			after, _ := strconvAtoi64(r.URL.Query().Get("after"))
			writeJSON(w, 200, fmt.Sprintf(`{"data":[%s],"hasMore":true}`, admittedEvent(int(after+1), testSession, fmt.Sprintf("msg_%032x", after+1), "other", "steer")))
		}))
		state, err := client.ReconcilePrompt(deadline(t), testSession, PromptSpec{ID: testMessage, Text: "do it", Delivery: "steer"}, HistoryBounds{1, 1, 3})
		if state != ReconcileUncertain || !errors.Is(err, ErrScanBound) {
			t.Fatalf("state=%s err=%v", state, err)
		}
	})
}

func TestReconcilePromptRejectsInvalidDurableIdentity(t *testing.T) {
	base := admittedEvent(1, testSession, testMessage, "do it", "steer")
	tests := []struct {
		name  string
		event string
	}{
		{"event prefix", strings.Replace(base, `"id":"evt_1"`, `"id":"event_1"`, 1)},
		{"version", strings.Replace(base, `"version":1`, `"version":2`, 1)},
		{"aggregate", strings.Replace(base, `"aggregateID":"`+testSession+`"`, `"aggregateID":"ses_other"`, 1)},
		{"negative timestamp", strings.Replace(base, `"timestamp":1`, `"timestamp":-1`, 1)},
		{"fractional timestamp", strings.Replace(base, `"timestamp":1`, `"timestamp":1.5`, 1)},
		{"missing timestamp", strings.Replace(base, `"timestamp":1,`, ``, 1)},
		{"unknown event type", strings.Replace(base, `"session.next.prompt.admitted"`, `"session.next.unknown"`, 1)},
		{"text delta is live only", strings.Replace(base, `"session.next.prompt.admitted"`, `"session.next.text.delta"`, 1)},
		{"reasoning delta is live only", strings.Replace(base, `"session.next.prompt.admitted"`, `"session.next.reasoning.delta"`, 1)},
		{"tool input delta is live only", strings.Replace(base, `"session.next.prompt.admitted"`, `"session.next.tool.input.delta"`, 1)},
		{"compaction delta is live only", strings.Replace(base, `"session.next.prompt.admitted"`, `"session.next.compaction.delta"`, 1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client, _ := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, 200, `{"data":[`+test.event+`],"hasMore":false}`)
			}))
			state, err := client.ReconcilePrompt(deadline(t), testSession, PromptSpec{ID: testMessage, Text: "do it", Delivery: "steer"}, HistoryBounds{2, 2, 3})
			if state != ReconcileUncertain || !errors.Is(err, ErrProtocol) {
				t.Fatalf("state=%s err=%v", state, err)
			}
		})
	}
}

func strconvAtoi64(value string) (int64, error) {
	var result int64
	_, err := fmt.Sscan(value, &result)
	return result, err
}

type failingRoundTripper struct {
	calls atomic.Int32
	t     *testing.T
}

type cancelingRoundTripper struct {
	calls  atomic.Int32
	cancel context.CancelFunc
}

func (r *cancelingRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	r.calls.Add(1)
	r.cancel()
	<-request.Context().Done()
	return nil, request.Context().Err()
}

func (r *failingRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	r.calls.Add(1)
	if request.Method != http.MethodPost || request.GetBody != nil {
		r.t.Errorf("mutation replay metadata changed: method=%s getBody=%t", request.Method, request.GetBody != nil)
	}
	return nil, errors.New("leak http://user:password@127.0.0.1/" + testSession + "?prompt=do-it " + testSecret)
}

func TestMutationTransportLossNeverRetries(t *testing.T) {
	transport := &failingRoundTripper{t: t}
	client, err := New(Config{Endpoint: "http://127.0.0.1:4096", Username: "opencode", Password: testSecret, HTTPClient: &http.Client{Timeout: time.Second, Transport: transport}})
	if err != nil {
		t.Fatal(err)
	}
	err = client.AdmitPromptOnce(deadline(t), testSession, PromptSpec{ID: testMessage, Text: "do it", Delivery: "steer", Resume: true})
	var typed *TransportError
	if !errors.Is(err, ErrTransport) || !errors.As(err, &typed) || transport.calls.Load() != 1 {
		t.Fatalf("calls=%d err=%v", transport.calls.Load(), err)
	}
	if strings.Contains(err.Error(), testSecret) || strings.Contains(err.Error(), "do it") {
		t.Fatal("transport error exposed request data")
	}
	if errors.Unwrap(err) != nil || strings.Contains(fmt.Sprintf("%+v", err), testSession) {
		t.Fatal("transport error retained the raw failure")
	}
}

func TestDeadlineCancellationAndRequirement(t *testing.T) {
	started := make(chan struct{})
	var calls atomic.Int32
	client, _ := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		close(started)
		<-r.Context().Done()
	}))
	if _, err := client.ReadSession(context.Background(), testSession); !errors.Is(err, ErrDeadline) {
		t.Fatalf("missing deadline error = %v", err)
	}
	canceled, cancelNow := context.WithCancel(deadline(t))
	cancelNow()
	if _, err := client.ReadSession(canceled, testSession); !errors.Is(err, context.Canceled) || errors.Is(err, ErrTransport) {
		t.Fatalf("cancellation error = %v", err)
	}
	expired, expireCancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer expireCancel()
	if _, err := client.ReadSession(expired, testSession); !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrTransport) {
		t.Fatalf("pre-dispatch deadline error = %v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("pre-dispatch contexts made %d requests", calls.Load())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := client.ReadSession(ctx, testSession)
	<-started
	if !errors.Is(err, ErrTransport) || errors.Is(err, context.DeadlineExceeded) || errors.Unwrap(err) != nil || calls.Load() != 1 {
		t.Fatalf("calls=%d deadline error=%v", calls.Load(), err)
	}
	t.Run("mutation cancellation after dispatch is ambiguous", func(t *testing.T) {
		requestCtx, requestCancel := context.WithTimeout(context.Background(), time.Second)
		transport := &cancelingRoundTripper{cancel: requestCancel}
		client, err := New(Config{Endpoint: "http://127.0.0.1:4096", Username: "opencode", Password: testSecret, HTTPClient: &http.Client{Timeout: time.Second, Transport: transport}})
		if err != nil {
			t.Fatal(err)
		}
		err = client.AdmitPromptOnce(requestCtx, testSession, PromptSpec{ID: testMessage, Text: "do it", Delivery: "steer", Resume: true})
		if !errors.Is(err, ErrTransport) || errors.Is(err, context.Canceled) || errors.Unwrap(err) != nil || transport.calls.Load() != 1 {
			t.Fatalf("calls=%d error=%v", transport.calls.Load(), err)
		}
	})
}

func TestRedirectRefused(t *testing.T) {
	var destination atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { destination.Add(1) }))
	defer target.Close()
	client, _ := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	err := client.CreateSessionOnce(deadline(t), testSessionSpec)
	if !errors.Is(err, ErrProtocol) || destination.Load() != 0 {
		t.Fatalf("destination calls=%d err=%v", destination.Load(), err)
	}
}

func TestPendingObservationOwnershipAndConservatism(t *testing.T) {
	t.Run("active positive", func(t *testing.T) {
		client, _ := testClient(t, observationHandler(`{"data":{"`+testSession+`":{"type":"running"}}}`, `{"data":[]}`, `{"data":[]}`))
		got, err := client.ObservePending(deadline(t), testSession)
		if err != nil || got.State != WorkWorking || !got.Active {
			t.Fatalf("got=%+v err=%v", got, err)
		}
	})
	t.Run("absence unknown", func(t *testing.T) {
		client, _ := testClient(t, observationHandler(`{"data":{}}`, `{"data":[]}`, `{"data":[]}`))
		got, err := client.ObservePending(deadline(t), testSession)
		if err != nil || got.State != WorkUnknown {
			t.Fatalf("got=%+v err=%v", got, err)
		}
	})
	t.Run("question needs you", func(t *testing.T) {
		question := `{"data":[{"id":"que_one","sessionID":"` + testSession + `","questions":[{"question":"Choose","header":"Contract","options":[{"label":"A","description":"First"}]}]}]}`
		client, _ := testClient(t, observationHandler(`{"data":{}}`, question, `{"data":[]}`))
		got, err := client.ObservePending(deadline(t), testSession)
		if err != nil || got.State != WorkNeedsYou || got.Questions != 1 {
			t.Fatalf("got=%+v err=%v", got, err)
		}
	})
	t.Run("empty and free-text question shapes", func(t *testing.T) {
		question := `{"data":[{"id":"que_free","sessionID":"` + testSession + `","questions":[{"question":"","header":"","options":[],"custom":true},{"question":"Free text","header":"Input","options":[{"label":"","description":""}]}]}]}`
		client, _ := testClient(t, observationHandler(`{"data":{}}`, question, `{"data":[]}`))
		got, err := client.ObservePending(deadline(t), testSession)
		if err != nil || got.State != WorkNeedsYou || got.Questions != 1 {
			t.Fatalf("got=%+v err=%v", got, err)
		}
	})
	t.Run("permission wrong owner", func(t *testing.T) {
		permission := `{"data":[{"id":"per_one","sessionID":"ses_other","action":"shell","resources":[]}]}`
		client, _ := testClient(t, observationHandler(`{"data":{}}`, `{"data":[]}`, permission))
		_, err := client.ObservePending(deadline(t), testSession)
		if !errors.Is(err, ErrProtocol) {
			t.Fatalf("error = %v", err)
		}
	})
}

func observationHandler(active, questions, permissions string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/session/active":
			writeJSON(w, 200, active)
		case sessionPath(testSession) + "/question":
			writeJSON(w, 200, questions)
		case sessionPath(testSession) + "/permission":
			writeJSON(w, 200, permissions)
		default:
			w.WriteHeader(404)
		}
	})
}

func TestInterruptExactAndLostResponseAmbiguity(t *testing.T) {
	t.Run("exact empty 204", func(t *testing.T) {
		var calls atomic.Int32
		client, _ := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			if r.Method != http.MethodPost || r.URL.Path != sessionPath(testSession)+"/interrupt" || r.GetBody != nil {
				t.Errorf("request changed")
			}
			w.WriteHeader(http.StatusNoContent)
		}))
		if err := client.InterruptOnce(deadline(t), testSession); err != nil || calls.Load() != 1 {
			t.Fatalf("calls=%d err=%v", calls.Load(), err)
		}
	})
	t.Run("204 body rejected", func(t *testing.T) {
		client, _ := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNoContent)
		}))
		if err := client.InterruptOnce(deadline(t), testSession); !errors.Is(err, ErrProtocol) {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("lost response one shot", func(t *testing.T) {
		transport := &failingRoundTripper{t: t}
		client, err := New(Config{Endpoint: "http://127.0.0.1:4096", Username: "opencode", Password: testSecret, HTTPClient: &http.Client{Timeout: time.Second, Transport: transport}})
		if err != nil {
			t.Fatal(err)
		}
		err = client.InterruptOnce(deadline(t), testSession)
		if !errors.Is(err, ErrTransport) || transport.calls.Load() != 1 {
			t.Fatalf("calls=%d err=%v", transport.calls.Load(), err)
		}
	})
}

func TestTrustedOrigin(t *testing.T) {
	origin, err := ParseTrustedOrigin("https://fern.example.test:8443")
	if err != nil {
		t.Fatal(err)
	}
	if origin.value != "https://fern.example.test:8443" {
		t.Fatalf("origin = %q", origin.value)
	}
	for _, raw := range []string{
		"http://fern.example.test", "https://user:pw@fern.example.test", "https://fern.example.test/",
		"https://fern.example.test?token=x", "https://127.0.0.1:8443", "https://localhost",
		"https://localhost.", "https://api.localhost", "https://api.localhost.", "https://127.1",
		"https://2130706433", "https://017700000001", "https://0x7f000001", "https://127.000.000.001",
		"https://FERN.example.test", "https://fern.example.test.", "https://fern.example.test:443",
	} {
		if _, err := ParseTrustedOrigin(raw); !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("origin %q error = %v", raw, err)
		}
	}
}
