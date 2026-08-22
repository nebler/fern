package opencodeapi

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestPermissionExactWire(t *testing.T) {
	t.Parallel()
	var step atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if got := request.Header.Get("Authorization"); got != "Basic "+base64.StdEncoding.EncodeToString([]byte("opencode:contract-password")) {
			t.Errorf("Authorization = %q", got)
		}
		body, _ := io.ReadAll(request.Body)
		switch step.Add(1) {
		case 1:
			assertRequest(t, request, body, http.MethodGet, "/api/session/ses_one/permission", "", "")
			writeJSON(t, writer, `{"data":[{"id":"per_one","sessionID":"ses_one","action":"shell","resources":["echo contract"],"agent":"contract","metadata":{"source":"tool"}}]}`)
		case 2:
			assertRequest(t, request, body, http.MethodGet, "/api/session/ses_one/permission/per_one", "", "")
			writeJSON(t, writer, `{"data":{"id":"per_one","sessionID":"ses_one","action":"shell","resources":["echo contract"],"agent":"contract","metadata":{"source":"tool"}}}`)
		case 3:
			assertRequest(t, request, body, http.MethodPost, "/api/session/ses_one/permission/per_one/reply", "", `{"reply":"once"}`)
			writer.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request")
		}
	}))
	defer server.Close()

	client := testClient(t, server)
	permissions, err := client.ListPermissions(deadlineContext(t), "ses_one")
	if err != nil || len(permissions) != 1 || permissions[0].ID != "per_one" || permissions[0].SessionID != "ses_one" || permissions[0].Action != "shell" {
		t.Fatalf("ListPermissions = %#v, %v", permissions, err)
	}
	if !strings.Contains(string(permissions[0].Bytes()), `"metadata":{"source":"tool"}`) {
		t.Fatalf("permission raw bytes = %s", permissions[0].Bytes())
	}
	permission, err := client.ReadPermission(deadlineContext(t), "ses_one", "per_one")
	if err != nil || permission.ID != "per_one" || len(permission.Resources) != 1 || permission.Agent != "contract" {
		t.Fatalf("ReadPermission = %#v, %v", permission, err)
	}
	if err := client.ReplyPermissionOnce(deadlineContext(t), "ses_one", "per_one"); err != nil {
		t.Fatal(err)
	}
	if got := step.Load(); got != 3 {
		t.Fatalf("requests = %d", got)
	}
}

func TestPermissionOwnershipAndIdentityValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		response string
		read     bool
		want     error
	}{
		{name: "list ownership", response: `{"data":[{"id":"per_one","sessionID":"ses_other","action":"shell","resources":[],"agent":"contract"}]}`, want: ErrProtocolConflict},
		{name: "list malformed ID", response: `{"data":[{"id":"bad","sessionID":"ses_one","action":"shell","resources":[],"agent":"contract"}]}`, want: ErrInvalidResponse},
		{name: "read identity", response: `{"data":{"id":"per_other","sessionID":"ses_one","action":"shell","resources":[],"agent":"contract"}}`, read: true, want: ErrProtocolConflict},
		{name: "read ownership", response: `{"data":{"id":"per_one","sessionID":"ses_other","action":"shell","resources":[],"agent":"contract"}}`, read: true, want: ErrProtocolConflict},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writeJSON(t, writer, test.response)
			}))
			defer server.Close()
			client := testClient(t, server)
			var err error
			if test.read {
				_, err = client.ReadPermission(deadlineContext(t), "ses_one", "per_one")
			} else {
				_, err = client.ListPermissions(deadlineContext(t), "ses_one")
			}
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}

	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("invalid ID reached server")
	}))
	defer server.Close()
	client := testClient(t, server)
	if _, err := client.ReadPermission(deadlineContext(t), "ses_one", "bad"); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("invalid read ID error = %v", err)
	}
	if err := client.ReplyPermissionOnce(deadlineContext(t), "ses_one", "bad"); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("invalid reply ID error = %v", err)
	}
}

func TestPermissionDuplicateReplyReturnsRedactedNotFound(t *testing.T) {
	t.Parallel()
	const secret = "permission-secret-resource"
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if calls.Add(1) == 1 {
			writer.WriteHeader(http.StatusNoContent)
			return
		}
		writer.WriteHeader(http.StatusNotFound)
		writeJSON(t, writer, `{"_tag":"PermissionNotFoundError","detail":"`+secret+`"}`)
	}))
	defer server.Close()
	client := testClient(t, server)
	if err := client.ReplyPermissionOnce(deadlineContext(t), "ses_one", "per_one"); err != nil {
		t.Fatal(err)
	}
	err := client.ReplyPermissionOnce(deadlineContext(t), "ses_one", "per_one")
	if !errors.Is(err, ErrNotFound) || !errors.Is(err, ErrRequestFailed) || strings.Contains(err.Error(), secret) || calls.Load() != 2 {
		t.Fatalf("duplicate reply error = %q, calls = %d", err, calls.Load())
	}
}

func TestPermissionReadNotFoundIsRedacted(t *testing.T) {
	t.Parallel()
	const secret = "missing-permission-secret"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusNotFound)
		writeJSON(t, writer, `{"_tag":"PermissionNotFoundError","detail":"`+secret+`"}`)
	}))
	defer server.Close()
	_, err := testClient(t, server).ReadPermission(deadlineContext(t), "ses_one", "per_one")
	if !errors.Is(err, ErrNotFound) || strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "contract-password") {
		t.Fatalf("read error = %q", err)
	}
}

func TestPermissionReplyResponseLossIsNotRetried(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	httpClient := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		if request.GetBody != nil {
			t.Error("permission reply is replayable")
		}
		return nil, io.ErrUnexpectedEOF
	})}
	client, err := New(Config{BaseURL: "http://127.0.0.1:4096", Username: "user", Password: "secret", HTTPClient: httpClient})
	if err != nil {
		t.Fatal(err)
	}
	err = client.ReplyPermissionOnce(deadlineContext(t), "ses_one", "per_one")
	if !errors.Is(err, ErrRequestFailed) || calls.Load() != 1 {
		t.Fatalf("ReplyPermissionOnce error = %v, calls = %d", err, calls.Load())
	}
}

func TestPermissionBoundsAndStrictEnvelope(t *testing.T) {
	t.Parallel()
	t.Run("entry bound", func(t *testing.T) {
		items := strings.Repeat(`{"id":"per_one","sessionID":"ses_one","action":"shell","resources":[],"agent":"contract"},`, maxListEntries) + `{"id":"per_one","sessionID":"ses_one","action":"shell","resources":[],"agent":"contract"}`
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			writeJSON(t, writer, `{"data":[`+items+`]}`)
		}))
		defer server.Close()
		if _, err := testClient(t, server).ListPermissions(deadlineContext(t), "ses_one"); !errors.Is(err, ErrResponseTooLarge) {
			t.Fatalf("entry bound error = %v", err)
		}
	})
	t.Run("response bound", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			writeJSON(t, writer, `{"data":{"id":"per_one","sessionID":"ses_one","action":"shell","resources":[],"agent":"contract","padding":"`+strings.Repeat("x", maxResponseBytes)+`"}}`)
		}))
		defer server.Close()
		if _, err := testClient(t, server).ReadPermission(deadlineContext(t), "ses_one", "per_one"); !errors.Is(err, ErrResponseTooLarge) {
			t.Fatalf("response bound error = %v", err)
		}
	})
	t.Run("strict envelope", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			writeJSON(t, writer, `{"data":[],"unexpected":true}`)
		}))
		defer server.Close()
		if _, err := testClient(t, server).ListPermissions(deadlineContext(t), "ses_one"); !errors.Is(err, ErrInvalidResponse) {
			t.Fatalf("strict envelope error = %v", err)
		}
	})
}

func TestPermissionDeadlineRequired(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("request without deadline reached server")
	}))
	defer server.Close()
	client := testClient(t, server)
	if _, err := client.ListPermissions(context.Background(), "ses_one"); !errors.Is(err, ErrDeadlineRequired) {
		t.Fatalf("list error = %v", err)
	}
	if _, err := client.ReadPermission(context.Background(), "ses_one", "per_one"); !errors.Is(err, ErrDeadlineRequired) {
		t.Fatalf("read error = %v", err)
	}
	if err := client.ReplyPermissionOnce(context.Background(), "ses_one", "per_one"); !errors.Is(err, ErrDeadlineRequired) {
		t.Fatalf("reply error = %v", err)
	}
}
