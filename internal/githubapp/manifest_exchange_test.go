package githubapp

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
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

func TestManifestClientExchangesCodeOnceAndReturnsRedactedCredentials(t *testing.T) {
	t.Parallel()
	privateKeyPEM := testPrivateKeyPEM(t)
	clientSecret := "client-secret-must-not-be-formatted"
	webhookSecret := "webhook-secret-must-not-be-formatted"
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		if request.Method != http.MethodPost || request.URL.Path != "/app-manifests/one_time-code/conversions" {
			t.Errorf("request = %s %s", request.Method, request.URL.Path)
		}
		if request.Header.Get("Accept") != "application/vnd.github+json" || request.Header.Get("X-GitHub-Api-Version") != "2022-11-28" {
			t.Errorf("headers = %#v", request.Header)
		}
		writer.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"id":             1234,
			"client_id":      "Iv1.fern",
			"client_secret":  clientSecret,
			"webhook_secret": webhookSecret,
			"pem":            string(privateKeyPEM),
		})
	}))
	defer server.Close()

	client := newTestManifestClient(t, server)
	code, err := NewManifestCode("one_time-code")
	if err != nil {
		t.Fatal(err)
	}
	codeCopy := code
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	credentials, err := client.Exchange(ctx, code)
	if err != nil {
		t.Fatal(err)
	}
	if credentials.AppID() != 1234 || credentials.ClientID() != "Iv1.fern" || credentials.ClientSecret() != clientSecret || credentials.WebhookSecret() != webhookSecret {
		t.Fatal("credential metadata did not round trip")
	}
	if credentials.PrivateKey() == nil || string(credentials.PrivateKeyPEM()) != string(privateKeyPEM) {
		t.Fatal("private key did not round trip")
	}
	formatted := fmt.Sprintf("%v %#v %v %#v", credentials, credentials, code, code)
	for _, secret := range []string{clientSecret, webhookSecret, "one_time-code", "PRIVATE KEY"} {
		if strings.Contains(formatted, secret) {
			t.Fatalf("formatted credentials contain %q: %s", secret, formatted)
		}
	}
	if _, err := client.Exchange(ctx, codeCopy); !errors.Is(err, ErrManifestCodeUsed) || requests.Load() != 1 {
		t.Fatalf("second exchange error = %v, requests = %d", err, requests.Load())
	}
}

func TestManifestClientRequiresDeadlineBeforeConsumingCode(t *testing.T) {
	t.Parallel()
	var requested atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requested.Store(true)
	}))
	defer server.Close()
	client := newTestManifestClient(t, server)
	code, _ := NewManifestCode("bounded-code")
	if _, err := client.Exchange(context.Background(), code); !errors.Is(err, ErrDeadlineRequired) || requested.Load() {
		t.Fatalf("error = %v, requested = %t", err, requested.Load())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.Exchange(ctx, code); !errors.Is(err, ErrDeadlineRequired) {
		t.Fatalf("cancelled deadline-free error = %v", err)
	}
}

func TestManifestClientRefusesRedirects(t *testing.T) {
	t.Parallel()
	var redirected atomic.Bool
	destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirected.Store(true)
	}))
	defer destination.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Location", destination.URL)
		writer.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	client := newTestManifestClient(t, origin)
	code, _ := NewManifestCode("redirect-code")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, err := client.Exchange(ctx, code)
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode() != http.StatusTemporaryRedirect || redirected.Load() {
		t.Fatalf("error = %v, redirected = %t", err, redirected.Load())
	}
}

func TestManifestClientBoundsAndStrictlyDecodesRedactedResponses(t *testing.T) {
	t.Parallel()
	remoteSecret := "remote-secret-must-not-escape"
	privateKeyPEM := testPrivateKeyPEM(t)
	valid := fmt.Sprintf(`{"id":1,"client_id":"Iv1.fern","client_secret":"secret","webhook_secret":"","pem":%q}`, privateKeyPEM)
	tests := []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{name: "HTTP error", status: http.StatusBadRequest, body: remoteSecret, want: ErrRequestFailed},
		{name: "oversized", status: http.StatusCreated, body: strings.Repeat(remoteSecret, maxManifestResponseBytes), want: ErrResponseTooLarge},
		{name: "unknown field", status: http.StatusCreated, body: strings.TrimSuffix(valid, "}") + `,"unexpected":"` + remoteSecret + `"}`, want: ErrInvalidAppCredentials},
		{name: "trailing JSON", status: http.StatusCreated, body: valid + ` {"secret":"` + remoteSecret + `"}`, want: ErrInvalidAppCredentials},
		{name: "invalid key", status: http.StatusCreated, body: `{"id":1,"client_id":"Iv1.fern","client_secret":"secret","pem":"` + remoteSecret + `"}`, want: ErrInvalidAppCredentials},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.WriteHeader(test.status)
				_, _ = io.WriteString(writer, test.body)
			}))
			defer server.Close()
			client := newTestManifestClient(t, server)
			code, _ := NewManifestCode("single-use-code")
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			_, err := client.Exchange(ctx, code)
			if !errors.Is(err, test.want) || strings.Contains(err.Error(), remoteSecret) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestNewManifestCodeRejectsUnsafeValues(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"", "path/code", "query?code", strings.Repeat("a", maxManifestCodeBytes+1)} {
		if code, err := NewManifestCode(value); !errors.Is(err, ErrInvalidManifestCode) || !strings.Contains(fmt.Sprint(code), "redacted") {
			t.Fatalf("code = %v, error = %v", code, err)
		}
	}
}

func newTestManifestClient(t *testing.T, server *httptest.Server) *ManifestClient {
	t.Helper()
	client, err := NewManifestClient(server.Client())
	if err != nil {
		t.Fatal(err)
	}
	client.apiBase = server.URL
	return client
}

func testPrivateKeyPEM(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
}
