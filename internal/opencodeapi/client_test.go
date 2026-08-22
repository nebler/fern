package opencodeapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func deadlineContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func testClient(t *testing.T, server *httptest.Server) *Client {
	t.Helper()
	client, err := New(Config{
		BaseURL: server.URL, Username: "opencode", Password: "contract-password", HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func writeJSON(t *testing.T, writer http.ResponseWriter, value string) {
	t.Helper()
	writer.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(writer, value)
}

func TestExactWireRequests(t *testing.T) {
	t.Parallel()
	var step atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if got := request.Header.Get("Authorization"); got != "Basic "+base64.StdEncoding.EncodeToString([]byte("opencode:contract-password")) {
			t.Errorf("Authorization = %q", got)
		}
		if got := request.Header.Get("Accept"); got != "application/json" {
			t.Errorf("Accept = %q", got)
		}
		body, _ := io.ReadAll(request.Body)
		n := step.Add(1)
		switch n {
		case 1:
			assertRequest(t, request, body, http.MethodPost, "/api/session", "", `{"id":"ses_one","title":"Fix it","agent":"build","model":{"providerID":"test","id":"test-model"},"location":{"directory":"/workspace"}}`)
			writeJSON(t, writer, `{"data":{"id":"ses_one","title":"Fix it","agent":"build","model":{"providerID":"test","id":"test-model","variant":"default"},"location":{"directory":"/workspace"}}}`)
		case 2:
			assertRequest(t, request, body, http.MethodGet, "/api/session/ses_one", "", "")
			writeJSON(t, writer, `{"data":{"id":"ses_one"}}`)
		case 3:
			assertRequest(t, request, body, http.MethodPost, "/api/session/ses_one/prompt", "", `{"id":"msg_one","text":"do it","resume":false}`)
			writeJSON(t, writer, `{"data":{"id":"msg_one"}}`)
		case 4:
			assertRequest(t, request, body, http.MethodGet, "/api/session/ses_one/inbox", "", "")
			writeJSON(t, writer, `{"data":[{"id":"msg_one","sessionID":"ses_one","type":"user","delivery":"steer","payload":{"text":"do it"},"timeCreated":10}]}`)
		case 5:
			assertRequest(t, request, body, http.MethodGet, "/api/session/ses_one/message", "cursor=opaque+cursor&limit=2", "")
			writeJSON(t, writer, `{"data":[{"id":"msg_one","time":{"created":10},"parts":[]}],"cursor":{"next":null}}`)
		case 6:
			assertRequest(t, request, body, http.MethodGet, "/api/session/active", "", "")
			writeJSON(t, writer, `{"data":{"ses_one":{"type":"busy"}}}`)
		case 7:
			assertRequest(t, request, body, http.MethodGet, "/api/session/ses_one/form", "", "")
			writeJSON(t, writer, `{"data":[{"id":"frm_one","sessionID":"ses_one","metadata":{"kind":"question"},"fields":[{"key":"q0","options":[{"label":"Choice A"}]}]}]}`)
		case 8:
			assertRequest(t, request, body, http.MethodGet, "/api/session/ses_one/form/frm_one", "", "")
			writeJSON(t, writer, `{"data":{"id":"frm_one","sessionID":"ses_one","metadata":{"kind":"question"},"fields":[{"key":"q0","options":[{"label":"Choice A"}]}]}}`)
		case 9:
			assertRequest(t, request, body, http.MethodGet, "/api/session/ses_one/form/frm_one/state", "", "")
			writeJSON(t, writer, `{"data":{"status":"answered","answer":{"q0":"Choice A"}}}`)
		case 10:
			assertRequest(t, request, body, http.MethodPost, "/api/session/ses_one/form/frm_one/reply", "", `{"answer":{"q0":"Choice A"}}`)
			writer.WriteHeader(http.StatusNoContent)
		case 11:
			assertRequest(t, request, body, http.MethodGet, "/api/session/ses_one/context", "", "")
			writeJSON(t, writer, `{"data":[{"text":"Step interrupted"}]}`)
		case 12:
			assertRequest(t, request, body, http.MethodPost, "/api/session/ses_one/interrupt", "continue=false", "")
			writer.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %d", n)
		}
	}))
	defer server.Close()

	client := testClient(t, server)
	ctx := deadlineContext(t)
	resume := false
	session, err := client.CreateOrReuseSession(ctx, CreateSessionRequest{
		ID: "ses_one", Title: "Fix it", Agent: "build", Model: &Model{ProviderID: "test", ID: "test-model"}, Location: &Location{Directory: "/workspace"},
	})
	if err != nil || session.ID != "ses_one" || !strings.Contains(string(session.Bytes()), `"title":"Fix it"`) {
		t.Fatalf("CreateOrReuseSession = %#v, %v", session, err)
	}
	if _, err := client.ReadSession(ctx, "ses_one"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.AdmitPrompt(ctx, "ses_one", PromptRequest{ID: "msg_one", Text: "do it", Resume: &resume}); err != nil {
		t.Fatal(err)
	}
	if inbox, err := client.ListInbox(ctx, "ses_one"); err != nil || len(inbox) != 1 {
		t.Fatalf("ListInbox = %#v, %v", inbox, err)
	}
	if page, err := client.Messages(ctx, "ses_one", "opaque cursor", 2); err != nil || len(page.Data) != 1 || page.NextCursor != nil {
		t.Fatalf("Messages = %#v, %v", page, err)
	}
	if active, err := client.ActiveSessions(ctx); err != nil || len(active) != 1 {
		t.Fatalf("ActiveSessions = %#v, %v", active, err)
	}
	if forms, err := client.ListForms(ctx, "ses_one"); err != nil || len(forms) != 1 || forms[0].Fields[0].Options[0].Label != "Choice A" {
		t.Fatalf("ListForms = %#v, %v", forms, err)
	}
	if form, err := client.ReadForm(ctx, "ses_one", "frm_one"); err != nil || form.ID != "frm_one" || form.Metadata.Kind != "question" {
		t.Fatalf("ReadForm = %#v, %v", form, err)
	}
	if state, err := client.ReadFormState(ctx, "ses_one", "frm_one"); err != nil || state.Status != "answered" {
		t.Fatalf("ReadFormState = %#v, %v", state, err)
	}
	if err := client.ReplyForm(ctx, "ses_one", "frm_one", FormReplyRequest{Answer: []byte(`{"q0":"Choice A"}`)}); err != nil {
		t.Fatal(err)
	}
	if value, err := client.ReadContext(ctx, "ses_one"); err != nil || !strings.Contains(string(value.Bytes()), "Step interrupted") {
		t.Fatalf("ReadContext = %s, %v", value.Bytes(), err)
	}
	if err := client.Interrupt(ctx, "ses_one"); err != nil {
		t.Fatal(err)
	}
	if got := step.Load(); got != 12 {
		t.Fatalf("requests = %d", got)
	}
}

func assertRequest(t *testing.T, request *http.Request, body []byte, method, path, query, expectedBody string) {
	t.Helper()
	if request.Method != method || request.URL.Path != path || request.URL.RawQuery != query || string(body) != expectedBody {
		t.Errorf("request = %s %s?%s %s, want %s %s?%s %s", request.Method, request.URL.Path, request.URL.RawQuery, body, method, path, query, expectedBody)
	}
	if expectedBody == "" {
		if got := request.Header.Get("Content-Type"); got != "" {
			t.Errorf("Content-Type = %q for empty body", got)
		}
	} else if got := request.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestMutationResponseLossIsNotRetried(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	httpClient := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		if request.GetBody != nil {
			t.Error("mutation request is replayable")
		}
		return nil, io.ErrUnexpectedEOF
	})}
	client, err := New(Config{BaseURL: "http://127.0.0.1:4096", Username: "user", Password: "secret", HTTPClient: httpClient})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.AdmitPrompt(deadlineContext(t), "ses_one", PromptRequest{ID: "msg_one", Text: "sensitive prompt"})
	if !errors.Is(err, ErrRequestFailed) || calls.Load() != 1 {
		t.Fatalf("AdmitPrompt error = %v, calls = %d", err, calls.Load())
	}
	if err := client.CancelInboxOnce(deadlineContext(t), "ses_one", "msg_one"); !errors.Is(err, ErrRequestFailed) || calls.Load() != 2 {
		t.Fatalf("CancelInboxOnce error = %v, calls = %d", err, calls.Load())
	}
}

func TestRedirectRefusedWithoutCredentialForwarding(t *testing.T) {
	t.Parallel()
	var reached atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached.Add(1) }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, target.URL+"/stolen", http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	client := testClient(t, source)
	_, err := client.ReadSession(deadlineContext(t), "ses_one")
	var status *StatusError
	if !errors.As(err, &status) || status.StatusCode() != http.StatusTemporaryRedirect || reached.Load() != 0 {
		t.Fatalf("ReadSession error = %v, target calls = %d", err, reached.Load())
	}
}

func TestDeadlinesRequiredAndPropagated(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		<-request.Context().Done()
	}))
	defer server.Close()
	client := testClient(t, server)
	if _, err := client.ReadSession(context.Background(), "ses_one"); !errors.Is(err, ErrDeadlineRequired) {
		t.Fatalf("missing deadline error = %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := client.ReadSession(ctx, "ses_one"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expired request error = %v", err)
	}
}

func TestStatusAndProtocolErrorsAreRedacted(t *testing.T) {
	t.Parallel()
	secret := "secret-body-and-prompt"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusConflict)
		writeJSON(t, writer, `{"_tag":"ConflictError","detail":"`+secret+`"}`)
	}))
	defer server.Close()
	client := testClient(t, server)
	_, err := client.AdmitPrompt(deadlineContext(t), "ses_one", PromptRequest{ID: "msg_one", Text: secret})
	if !errors.Is(err, ErrConflict) || !errors.Is(err, ErrRequestFailed) || strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "contract-password") {
		t.Fatalf("unsafe conflict error = %q", err)
	}

	notFound := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Error(writer, secret, http.StatusNotFound)
	}))
	defer notFound.Close()
	_, err = testClient(t, notFound).ReadSession(deadlineContext(t), "ses_one")
	if !errors.Is(err, ErrNotFound) || strings.Contains(err.Error(), secret) {
		t.Fatalf("unsafe not-found error = %q", err)
	}

	mismatch := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(t, writer, `{"data":{"id":"ses_other"}}`)
	}))
	defer mismatch.Close()
	_, err = testClient(t, mismatch).ReadSession(deadlineContext(t), "ses_one")
	if !errors.Is(err, ErrProtocolConflict) || strings.Contains(err.Error(), "ses_other") {
		t.Fatalf("unsafe protocol error = %q", err)
	}
}

func TestStrictEnvelopes(t *testing.T) {
	t.Parallel()
	for name, response := range map[string]string{
		"unknown":  `{"data":{"id":"ses_one"},"extra":true}`,
		"trailing": `{"data":{"id":"ses_one"}} {}`,
		"null":     `{"data":null}`,
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) { writeJSON(t, writer, response) }))
			defer server.Close()
			_, err := testClient(t, server).ReadSession(deadlineContext(t), "ses_one")
			if !errors.Is(err, ErrInvalidResponse) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestBodyAndEntryBounds(t *testing.T) {
	t.Parallel()
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		writeJSON(t, writer, `{"data":{"id":"ses_one","padding":"`+strings.Repeat("x", maxResponseBytes)+`"}}`)
	}))
	defer server.Close()
	client := testClient(t, server)
	if _, err := client.ReadSession(deadlineContext(t), "ses_one"); !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("response bound error = %v", err)
	}
	if _, err := client.AdmitPrompt(deadlineContext(t), "ses_one", PromptRequest{ID: "msg_one", Text: strings.Repeat("p", MaxPromptTextBytes+1)}); !errors.Is(err, ErrRequestTooLarge) {
		t.Fatalf("request bound error = %v", err)
	}
	if requests.Load() != 1 {
		t.Fatalf("oversized request reached server: requests = %d", requests.Load())
	}

	tooMany := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		items := strings.Repeat(`{"id":"msg_one"},`, maxListEntries) + `{"id":"msg_one"}`
		writeJSON(t, writer, `{"data":[`+items+`]}`)
	}))
	defer tooMany.Close()
	if _, err := testClient(t, tooMany).ListInbox(deadlineContext(t), "ses_one"); !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("entry bound error = %v", err)
	}
}

func TestPromptTextContractMatchesTaskAdmissionLimit(t *testing.T) {
	t.Parallel()
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		if request.ContentLength <= MaxPromptTextBytes {
			t.Errorf("escaped prompt wire bytes = %d", request.ContentLength)
		}
		var body PromptRequest
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode prompt: %v", err)
		}
		if len(body.Text) != MaxPromptTextBytes {
			t.Errorf("prompt bytes = %d", len(body.Text))
		}
		writeJSON(t, writer, `{"data":{"id":"msg_one"}}`)
	}))
	defer server.Close()
	client := testClient(t, server)
	if _, err := client.AdmitPrompt(deadlineContext(t), "ses_one", PromptRequest{ID: "msg_one", Text: strings.Repeat("<", MaxPromptTextBytes)}); err != nil {
		t.Fatalf("maximum admitted prompt: %v", err)
	}
	for _, text := range []string{"", string([]byte{0xff})} {
		if _, err := client.AdmitPrompt(deadlineContext(t), "ses_one", PromptRequest{ID: "msg_one", Text: text}); !errors.Is(err, ErrInvalidConfiguration) {
			t.Errorf("invalid prompt error = %v", err)
		}
	}
	if requests.Load() != 1 {
		t.Fatalf("invalid prompt reached server: requests = %d", requests.Load())
	}
}

func TestInboxAndMessagePageRejectDuplicateIdentities(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if strings.HasSuffix(request.URL.Path, "/inbox") {
			writeJSON(t, writer, `{"data":[{"id":"msg_one","sessionID":"ses_one","type":"user","delivery":"steer","payload":{"text":"do it"},"timeCreated":1},{"id":"msg_one","sessionID":"ses_one","type":"user","delivery":"steer","payload":{"text":"do it"},"timeCreated":1}]}`)
			return
		}
		writeJSON(t, writer, `{"data":[{"id":"msg_one","time":{"created":1}},{"id":"msg_one","time":{"created":1}}],"cursor":{"next":null}}`)
	}))
	defer server.Close()
	client := testClient(t, server)
	if _, err := client.ListInbox(deadlineContext(t), "ses_one"); !errors.Is(err, ErrProtocolConflict) {
		t.Fatalf("duplicate inbox error = %v", err)
	}
	if _, err := client.Messages(deadlineContext(t), "ses_one", "", 2); !errors.Is(err, ErrProtocolConflict) {
		t.Fatalf("duplicate message page error = %v", err)
	}
}

func TestExactMessageReadAndOneShotInboxCancellation(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch calls.Add(1) {
		case 1:
			assertRequest(t, request, nil, http.MethodGet, "/api/session/ses_one/message/msg_one", "", "")
			writeJSON(t, writer, `{"data":{"id":"msg_one","time":{"created":10},"parts":[]}}`)
		case 2:
			assertRequest(t, request, nil, http.MethodDelete, "/api/session/ses_one/inbox/msg_one", "", "")
			writer.WriteHeader(http.StatusNoContent)
		case 3:
			writer.WriteHeader(http.StatusConflict)
		default:
			t.Errorf("unexpected request %d", calls.Load())
		}
	}))
	defer server.Close()
	client := testClient(t, server)
	if message, err := client.ReadMessage(deadlineContext(t), "ses_one", "msg_one"); err != nil || message.ID != "msg_one" {
		t.Fatalf("ReadMessage = %+v, %v", message, err)
	}
	if err := client.CancelInboxOnce(deadlineContext(t), "ses_one", "msg_one"); err != nil {
		t.Fatal(err)
	}
	if err := client.CancelInboxOnce(deadlineContext(t), "ses_one", "msg_one"); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate cancellation error = %v", err)
	}
	if calls.Load() != 3 {
		t.Fatalf("calls = %d", calls.Load())
	}
}

func TestExactMessageAndInboxCancellationValidateIDsBeforeRequest(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer server.Close()
	client := testClient(t, server)
	if _, err := client.ReadMessage(deadlineContext(t), "bad", "msg_one"); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("invalid read IDs error = %v", err)
	}
	if err := client.CancelInboxOnce(deadlineContext(t), "ses_one", "bad"); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("invalid cancellation IDs error = %v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("invalid IDs reached server %d times", calls.Load())
	}
}

func TestResponsesRejectDuplicateKeysAndInvalidUTF8(t *testing.T) {
	t.Parallel()
	invalidUTF8 := append([]byte(`{"data":{"id":"ses_`), 0xff)
	invalidUTF8 = append(invalidUTF8, []byte(`"}}`)...)
	responses := [][]byte{
		[]byte(`{"data":{"id":"ses_one"},"Data":{"id":"ses_two"}}`),
		[]byte(`{"data":{"id":"ses_one","ID":"ses_two"}}`),
		invalidUTF8,
	}
	for index, payload := range responses {
		payload := payload
		t.Run(strconv.Itoa(index), func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				_, _ = writer.Write(payload)
			}))
			defer server.Close()
			if _, err := testClient(t, server).ReadSession(deadlineContext(t), "ses_one"); !errors.Is(err, ErrInvalidResponse) {
				t.Fatalf("ambiguous response error = %v", err)
			}
		})
	}
}

func TestConfigurationRequiresExplicitLoopbackHTTPAndBasicAuth(t *testing.T) {
	t.Parallel()
	httpClient := &http.Client{}
	for _, config := range []Config{
		{BaseURL: "https://127.0.0.1:4096", Username: "u", Password: "p", HTTPClient: httpClient},
		{BaseURL: "http://192.0.2.1:4096", Username: "u", Password: "p", HTTPClient: httpClient},
		{BaseURL: "http://127.0.0.1:4096/path", Username: "u", Password: "p", HTTPClient: httpClient},
		{BaseURL: "http://127.0.0.1:99999", Username: "u", Password: "p", HTTPClient: httpClient},
		{BaseURL: "http://user:pass@127.0.0.1:4096", Username: "u", Password: "p", HTTPClient: httpClient},
		{BaseURL: "http://127.0.0.1:4096", Username: "", Password: "p", HTTPClient: httpClient},
		{BaseURL: "http://127.0.0.1:4096", Username: "u:bad", Password: "p", HTTPClient: httpClient},
		{BaseURL: "http://127.0.0.1:4096", Username: "u", Password: "", HTTPClient: httpClient},
	} {
		if client, err := New(config); !errors.Is(err, ErrInvalidConfiguration) || client != nil {
			t.Fatalf("New(%#v) = %#v, %v", config, client, err)
		}
	}
	if _, err := New(Config{BaseURL: "http://[::1]:4096", Username: "u", Password: "p", HTTPClient: httpClient}); err != nil {
		t.Fatalf("IPv6 loopback rejected: %v", err)
	}
}

func TestMessagePageLimitEnforced(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(t, writer, `{"data":[{"id":"msg_a","time":{"created":1}},{"id":"msg_b","time":{"created":2}}],"cursor":{"next":null}}`)
	}))
	defer server.Close()
	_, err := testClient(t, server).Messages(deadlineContext(t), "ses_one", "", 1)
	if !errors.Is(err, ErrProtocolConflict) {
		t.Fatalf("page limit error = %v", err)
	}
}
