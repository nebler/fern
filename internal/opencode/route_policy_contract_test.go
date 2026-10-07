package opencode

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/nebler/fern/internal/domain"
)

// This literal is an upgrade gate, not an alias of the production profile.
// Review attachment routes and wire projections against this upstream commit
// before consciously changing it, and rerun the live qualification.
func TestAttachmentPinnedProfileContract(t *testing.T) {
	const reviewed = "source-39fb919a054190498f6d5b7985bde231f93ad7a6"
	if Profile != reviewed || domain.SourceProfile != reviewed {
		t.Fatal("OpenCode profile changed: review attachment policy and live envelope contracts before updating the reviewed commit")
	}
}

func TestAttachmentPinnedDenyContract(t *testing.T) {
	const own = "ses_0123456789abcdef0123456789abcdef"
	for _, path := range []string{
		"/api/management/future", "/api/provider", "/api/session/active",
		"/api/session/" + own + "/future", "/api/session/" + own + "/prompt/extra",
		"/api/session/" + own + "/revert/future", "/api/experimental/session/" + own + "/prompt",
	} {
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions} {
			t.Run(method+path, func(t *testing.T) {
				if attachmentRequestAllowed(httptest.NewRequest(method, path, nil), own) {
					t.Fatal("unreviewed write was allowed")
				}
			})
		}
	}
	for _, prefix := range []string{"/session/", "/api/session/", "/api/experimental/session/"} {
		for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPatch, http.MethodDelete} {
			for _, suffix := range []string{"", "/prompt", "/message", "/interrupt", "/question/qst_1/reply"} {
				request := httptest.NewRequest(method, prefix+own+"a"+suffix, nil)
				if attachmentRequestAllowed(request, own) {
					t.Fatalf("session prefix collision allowed: %s %s", method, request.URL.Path)
				}
			}
		}
	}
	for _, path := range []string{
		"/api/session?parentID=" + own + "&parentID=ses_foreign",
		"/api/session?parentID=", "/api/provider?workspace=",
		"/api/provider?directory=/home/user/workspace&directory=/etc",
		"/api/provider?location%5Bdirectory%5D=/home/user/workspace&location%5Bdirectory%5D=/etc",
		"/session?path=home/user/workspace&path=etc",
		"/file/content", "/file/content?path=README.md&path=../secret",
		"/file?path=%2Fetc%2Fpasswd", "/file?path=..%5Csecret",
		"/api/session/%73es_0123456789abcdef0123456789abcdef", "/api/session//" + own,
	} {
		t.Run(path, func(t *testing.T) {
			if attachmentRequestAllowed(httptest.NewRequest(http.MethodGet, path, nil), own) {
				t.Fatal("ambiguous or foreign selection was allowed")
			}
		})
	}
	request := httptest.NewRequest(http.MethodGet, "/api/provider", nil)
	request.Header.Add("X-OpenCode-Directory", attachmentWorkspace)
	request.Header.Add("X-OpenCode-Directory", attachmentWorkspace)
	if attachmentRequestAllowed(request, own) {
		t.Fatal("duplicate directory headers were allowed")
	}
}

func TestAttachmentAPIEnvelopeProjectionContract(t *testing.T) {
	for _, tc := range []struct{ path, input, want string }{
		{"/api/session", `{"data":[null,17,{"id":42},{},{"id":"ses_foreign","title":"private"},{"id":"ses_own","title":"retained"}],"next":"cursor"}`, `{"data":[{"id":"ses_own","title":"retained"}],"next":"cursor"}`},
		{"/api/session", `{"data":[{"id":"ses_foreign"}]}`, `{"data":[]}`},
		{"/api/session/active", `{"data":{"ses_foreign":{"type":"running"},"ses_own":{"type":"running"}},"meta":{"version":1}}`, `{"data":{"ses_own":{"type":"running"}},"meta":{"version":1}}`},
		{"/api/session/active", `{"data":{"ses_foreign":{}}}`, `{"data":{}}`},
	} {
		t.Run(tc.path+tc.input, func(t *testing.T) {
			response := contractResponse(tc.path, tc.input)
			if err := filterAttachmentResponse(response, "ses_own"); err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			got, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			var actual, expected any
			if json.Unmarshal(got, &actual) != nil || json.Unmarshal([]byte(tc.want), &expected) != nil {
				t.Fatal("invalid projection JSON")
			}
			a, _ := json.Marshal(actual)
			b, _ := json.Marshal(expected)
			if string(a) != string(b) {
				t.Fatalf("projection = %s; want %s", a, b)
			}
			if response.ContentLength != int64(len(got)) || response.Header.Get("Content-Length") != strconv.Itoa(len(got)) {
				t.Fatal("projection retained stale content length")
			}
		})
	}
}

func TestAttachmentMalformedProjectionContract(t *testing.T) {
	for _, path := range []string{"/session", "/session/status", "/api/session", "/api/session/active"} {
		for _, body := range []string{`not-json`, `{"data":`, `"wrong-shape"`, strings.Repeat(" ", maxAttachmentProjectionBytes+1)} {
			if err := filterAttachmentResponse(contractResponse(path, body), "ses_own"); err == nil {
				t.Errorf("%s accepted malformed or oversized projection", path)
			}
		}
	}
	for _, path := range []string{"/api/session", "/api/session/active"} {
		for _, body := range []string{`{}`, `{"data":false}`, `{"data":"private"}`} {
			if err := filterAttachmentResponse(contractResponse(path, body), "ses_own"); err == nil {
				t.Errorf("%s accepted missing or invalid data", path)
			}
		}
	}
	for _, tc := range []struct{ path, body string }{{"/api/session", `{"data":{}}`}, {"/api/session/active", `{"data":[]}`}} {
		if err := filterAttachmentResponse(contractResponse(tc.path, tc.body), "ses_own"); err == nil {
			t.Errorf("%s accepted wrong data container", tc.path)
		}
	}
}

func contractResponse(path, body string) *http.Response {
	return &http.Response{Request: httptest.NewRequest(http.MethodGet, path, nil), Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{"Content-Length": {"999999"}}, ContentLength: 999999}
}

func TestAttachmentAPISSEIdentityContract(t *testing.T) {
	// Mixed identities must be dropped even when nested in an API envelope.
	const own = "data: {\"type\":\"message.updated\",\"properties\":{\"sessionID\":\"ses_own\"}}\n\n"
	for _, payload := range []string{
		`{"data":{"sessionId":"ses_own","items":[{"parent_id":"ses_foreign"}]}}`,
		`{"type":"permission.updated","properties":{"sessionID":17}}`,
		`{"type":"question.updated","properties":{"sessionID":null}}`,
		`{"type":"todo.updated","properties":{"sessionID":"not-a-session"}}`,
		`{"data":{"session":"ses_foreign"}}`, `{"type":"message.updated",`,
	} {
		response := contractResponse("/api/event", "data: "+payload+"\n\n"+own)
		if err := filterAttachmentResponse(response, "ses_own"); err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if err != nil || string(got) != own {
			t.Fatalf("SSE projection = %q, %v", got, err)
		}
		if response.ContentLength != -1 || response.Header.Get("Content-Length") != "" {
			t.Fatal("SSE retained a fixed content length")
		}
	}
}
