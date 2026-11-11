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
	"golang.org/x/crypto/nacl/box"

	"github.com/nebler/fern/internal/github"
)

// fakeGitHub answers as GitHub's API does for an App installed on
// owner/app only, after checking the App's JWT against key. Its tokens
// expire after ttl.
type fakeGitHub struct {
	key    *rsa.PublicKey
	ttl    time.Duration
	minted atomic.Int32
	// noWorkflows is an App that may not change workflows.
	noWorkflows bool
	// box is the repository's key for Actions secrets.
	boxPub, boxPriv *[32]byte
	mu              sync.Mutex
	asked           []map[string]any  // the access token requests' bodies
	secrets         map[string]string // name to the sealed value, as base64
}

func (f *fakeGitHub) requests() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.asked
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	// The secrets' routes take an installation token; the others the JWT.
	if path, ok := strings.CutPrefix(r.URL.Path, "/repos/owner/app/actions/secrets/"); ok {
		if !strings.HasPrefix(bearer, "ghs_") {
			http.Error(w, "not a token", http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && path == "public-key":
			_ = json.NewEncoder(w).Encode(map[string]string{"key_id": "k1", "key": base64.StdEncoding.EncodeToString(f.boxPub[:])})
		case r.Method == http.MethodPut:
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["key_id"] != "k1" {
				http.Error(w, "key_id", http.StatusUnprocessableEntity)
				return
			}
			f.mu.Lock()
			f.secrets[path] = body["encrypted_value"]
			f.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusTeapot)
		}
		return
	}
	if err := f.verify(bearer); err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/app/installations":
		_, _ = fmt.Fprint(w, `[{"account": {"login": "owner"}, "permissions": {"contents": "write", "pull_requests": "write", "metadata": "read", "secrets": "read"}}]`)
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
		if perms, _ := body["permissions"].(map[string]any); f.noWorkflows && perms["workflows"] != nil {
			http.Error(w, `{"message":"The permissions requested are not granted to this installation."}`, http.StatusUnprocessableEntity)
			return
		}
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
	pub, priv, err := box.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeGitHub{key: &key.PublicKey, ttl: ttl, boxPub: pub, boxPriv: priv, secrets: make(map[string]string)}
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
		"permissions":  map[string]any{"contents": "write", "pull_requests": "write", "metadata": "read", "workflows": "write"},
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

// An App that may not change workflows still gets a token, without them.
func TestTokenWithoutWorkflows(t *testing.T) {
	app, fake := newApp(t, time.Hour)
	fake.noWorkflows = true
	if tok, err := app.Token(t.Context(), "owner", "app"); err != nil || tok.Token != "ghs_1" {
		t.Fatalf("Token() = %+v, %v; want ghs_1", tok, err)
	}
	asked := fake.requests()
	if len(asked) != 2 {
		t.Fatalf("token requests = %d, want 2: with workflows, then without", len(asked))
	}
	if _, ok := asked[1]["permissions"].(map[string]any)["workflows"]; ok {
		t.Errorf("the second request = %v, want it without workflows", asked[1])
	}
}

// A secret is sealed with the repository's key, with a token that may only
// write secrets.
func TestSetSecret(t *testing.T) {
	app, fake := newApp(t, time.Hour)
	if err := app.SetSecret(t.Context(), "owner", "app", "CLOUDFLARE_API_TOKEN", "cf-1"); err != nil {
		t.Fatalf("SetSecret() error: %v", err)
	}
	sealed, err := base64.StdEncoding.DecodeString(fake.secrets["CLOUDFLARE_API_TOKEN"])
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := box.OpenAnonymous(nil, sealed, fake.boxPub, fake.boxPriv); !ok || string(got) != "cf-1" {
		t.Errorf("the secret opens to %q, %t; want cf-1", got, ok)
	}
	want := map[string]any{"secrets": "write", "metadata": "read"}
	if diff := cmp.Diff(want, fake.requests()[0]["permissions"]); diff != "" {
		t.Errorf("token permissions (-want +got):\n%s", diff)
	}
	for _, tc := range []struct{ owner, repo, secret string }{
		{"owner", "other", "X"},
		{"owner", "app", "lower"},
		{"owner", "../app", "X"},
	} {
		if err := app.SetSecret(t.Context(), tc.owner, tc.repo, tc.secret, "v"); err == nil {
			t.Errorf("SetSecret(%s/%s, %s) succeeded, want an error", tc.owner, tc.repo, tc.secret)
		}
	}
}

// A token that expires within five minutes isn't handed out again: a push
// The App's installations come with what each account accepted, and say
// which of Fern's permissions are missing: read isn't enough for secrets.
func TestInstallations(t *testing.T) {
	app, _ := newApp(t, time.Hour)
	got, err := app.Installations(t.Context())
	if err != nil {
		t.Fatalf("Installations() error: %v", err)
	}
	if len(got) != 1 || got[0].Account != "owner" {
		t.Fatalf("Installations() = %+v, want owner's", got)
	}
	if diff := cmp.Diff([]string{"Workflows", "Secrets"}, got[0].Lacks()); diff != "" {
		t.Errorf("Lacks() (-want +got):\n%s", diff)
	}
}

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
