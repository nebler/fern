package github_test

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/nebler/fern/internal/github"
)

// fakeGitHub answers as GitHub's API does for an App installed on
// owner/app only, after checking the App's JWT against key. Its tokens
// expire after ttl.
type fakeGitHub struct {
	key    *rsa.PublicKey
	ttl    time.Duration
	minted atomic.Int32
	mu     sync.Mutex
	asked  []map[string]any // the access token requests' bodies
}

func (f *fakeGitHub) requests() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.asked
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := f.verify(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")); err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/repos/owner/app/installation":
		_, _ = fmt.Fprint(w, `{"id": 42}`)
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/installation"):
		http.NotFound(w, r)
	case r.Method == http.MethodPost && r.URL.Path == "/app/installations/42/access_tokens":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.asked = append(f.asked, body)
		f.mu.Unlock()
		n := f.minted.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"token": fmt.Sprintf("ghs_%d", n), "expires_at": time.Now().Add(f.ttl)})
	default:
		http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusTeapot)
	}
}

// verify checks a JWT the App signed: RS256, issued by App 7, valid now.
func (f *fakeGitHub) verify(jwt string) error {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return fmt.Errorf("a JWT has 3 parts, not %d", len(parts))
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(f.key, crypto.SHA256, sum[:], sig); err != nil {
		return fmt.Errorf("signature: %w", err)
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return err
	}
	var claims struct{ Iat, Exp, Iss int64 }
	if err := json.Unmarshal(data, &claims); err != nil {
		return err
	}
	now := time.Now().Unix()
	if claims.Iss != 7 || claims.Iat > now || claims.Exp <= now || claims.Exp-claims.Iat > 600 {
		return fmt.Errorf("claims %+v", claims)
	}
	return nil
}

// newApp returns App 7 with a new key, and a fake GitHub for it whose
// tokens last ttl.
func newApp(t *testing.T, ttl time.Duration) (*github.App, *fakeGitHub) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeGitHub{key: &key.PublicKey, ttl: ttl}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	// As GitHub hands it out: PKCS #1.
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "id"), []byte("7\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "key.pem"), keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	app, err := github.Load(dir, srv.URL)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	return app, fake
}

func TestToken(t *testing.T) {
	app, fake := newApp(t, time.Hour)
	tok, err := app.Token(t.Context(), "owner", "app")
	if err != nil || tok.Token != "ghs_1" {
		t.Fatalf("Token() = %+v, %v; want ghs_1", tok, err)
	}
	// Narrower than the App: the one repository, and what git and gh need.
	want := []map[string]any{{
		"repositories": []any{"app"},
		"permissions":  map[string]any{"contents": "write", "pull_requests": "write", "metadata": "read"},
	}}
	if diff := cmp.Diff(want, fake.requests()); diff != "" {
		t.Errorf("token request (-want +got):\n%s", diff)
	}
	if tok, err := app.Token(t.Context(), "owner", "app"); err != nil || tok.Token != "ghs_1" {
		t.Errorf("Token() again = %+v, %v; want the cached ghs_1", tok, err)
	}
	if _, err := app.Token(t.Context(), "owner", "other"); !errors.Is(err, github.ErrNotInstalled) {
		t.Errorf("Token() of a repository without the App error = %v, want ErrNotInstalled", err)
	}
	if _, err := app.Token(t.Context(), "owner", "../app"); err == nil {
		t.Errorf("Token() of a bad name succeeded")
	}
}

// A token that expires within five minutes isn't handed out again: a push
// that starts with it must not outlive it.
func TestTokenRenewed(t *testing.T) {
	app, fake := newApp(t, 4*time.Minute)
	for range 2 {
		if _, err := app.Token(t.Context(), "owner", "app"); err != nil {
			t.Fatal(err)
		}
	}
	if n := fake.minted.Load(); n != 2 {
		t.Errorf("tokens minted = %d, want 2", n)
	}
}

func TestLoadNone(t *testing.T) {
	if app, err := github.Load(filepath.Join(t.TempDir(), "none"), github.API); app != nil || err != nil {
		t.Errorf("Load() of a missing folder = %v, %v; want nil, nil", app, err)
	}
}

func TestHandler(t *testing.T) {
	app, _ := newApp(t, time.Hour)
	tests := []struct {
		name string
		app  *github.App
		body string
		want int
	}{
		{"a token", app, `{"repository": "owner/app.git"}`, http.StatusOK},
		{"not installed", app, `{"repository": "owner/other"}`, http.StatusNotFound},
		{"a bad name", app, `{"repository": "owner/a b"}`, http.StatusBadRequest},
		{"no App set up", nil, `{"repository": "owner/app"}`, http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			github.Handler(tt.app).ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/github/token", strings.NewReader(tt.body)))
			if rec.Code != tt.want {
				t.Errorf("status = %d, want %d: %s", rec.Code, tt.want, rec.Body)
			}
		})
	}
}
