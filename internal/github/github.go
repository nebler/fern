// Package github mints GitHub tokens for T3's git and gh, from a GitHub App
// the person installed on their repositories: each token is for one
// repository, lasts an hour, and may only read and write its contents and
// pull requests. The App's private key stays on the node, readable by root
// only; T3's server asks the daemon for a token over its socket, which
// boxes can't reach, so no token is ever in a box, a workspace or a
// snapshot.
package github

import (
	"bytes"
	"context"
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
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrNotInstalled means the App isn't installed on the repository, or the
// repository doesn't exist.
var ErrNotInstalled = errors.New("the GitHub App isn't installed on this repository")

// ErrBadRepository means a name that can't be a GitHub repository's.
var ErrBadRepository = errors.New("not a repository")

// Token is an installation access token.
type Token struct {
	Token   string    `json:"token"`
	Expires time.Time `json:"expires_at"`
}

// App is a GitHub App: its ID and private key. Its tokens are cached until
// five minutes before they expire.
type App struct {
	id   int64
	key  *rsa.PrivateKey
	base string // GitHub's API
	hc   *http.Client

	mu     sync.Mutex
	tokens map[string]Token // by owner/repo
}

// API is GitHub's API.
const API = "https://api.github.com"

// renewBefore is how long before it expires a cached token is replaced: a
// push that starts with it must not outlive it.
const renewBefore = 5 * time.Minute

// New returns the App with id and the PEM private key, which talks to the
// API at base (API, or a test server's).
func New(id int64, keyPEM []byte, base string) (*App, error) {
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		return nil, errors.New("the GitHub App's key isn't PEM")
	}
	// GitHub's downloads are PKCS #1; a key converted since may be PKCS #8.
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		k, err8 := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err8 != nil {
			return nil, fmt.Errorf("the GitHub App's key: %w", err)
		}
		var ok bool
		if key, ok = k.(*rsa.PrivateKey); !ok {
			return nil, errors.New("the GitHub App's key isn't RSA")
		}
	}
	return &App{
		id: id, key: key, base: strings.TrimSuffix(base, "/"),
		hc: &http.Client{Timeout: 30 * time.Second}, tokens: make(map[string]Token),
	}, nil
}

// Load returns the App in dir: its ID in the file id and its private key in
// key.pem, or nil if dir doesn't exist.
func Load(dir, base string) (*App, error) {
	data, err := os.ReadFile(filepath.Join(dir, "id"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	id, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("the GitHub App's id: %w", err)
	}
	key, err := os.ReadFile(filepath.Join(dir, "key.pem"))
	if err != nil {
		return nil, err
	}
	return New(id, key, base)
}

// name is an owner's or a repository's name as GitHub allows them.
var name = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}$`)

// Token returns a token for repository owner/repo.
func (a *App) Token(ctx context.Context, owner, repo string) (Token, error) {
	if !name.MatchString(owner) || !name.MatchString(repo) {
		return Token{}, fmt.Errorf("%q/%q: %w", owner, repo, ErrBadRepository)
	}
	full := owner + "/" + repo
	a.mu.Lock()
	t, ok := a.tokens[full]
	a.mu.Unlock()
	if ok && time.Until(t.Expires) > renewBefore {
		return t, nil
	}
	jwt, err := a.jwt()
	if err != nil {
		return Token{}, err
	}
	var inst struct {
		ID int64 `json:"id"`
	}
	if err := a.call(ctx, http.MethodGet, "/repos/"+url.PathEscape(owner)+"/"+url.PathEscape(repo)+"/installation", jwt, nil, &inst); err != nil {
		return Token{}, err
	}
	// Narrower than the App: this repository, and only what git and gh need.
	body := map[string]any{
		"repositories": []string{repo},
		"permissions":  map[string]string{"contents": "write", "pull_requests": "write", "metadata": "read"},
	}
	if err := a.call(ctx, http.MethodPost, fmt.Sprintf("/app/installations/%d/access_tokens", inst.ID), jwt, body, &t); err != nil {
		return Token{}, err
	}
	a.mu.Lock()
	a.tokens[full] = t
	a.mu.Unlock()
	return t, nil
}

// jwt returns the App's JSON Web Token, which lasts nine minutes (GitHub
// takes ten at most) and starts a minute ago, for clocks that differ.
func (a *App) jwt() (string, error) {
	now := time.Now()
	enc := base64.RawURLEncoding
	header := enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, err := json.Marshal(map[string]int64{
		"iat": now.Add(-time.Minute).Unix(), "exp": now.Add(9 * time.Minute).Unix(), "iss": a.id,
	})
	if err != nil {
		return "", err
	}
	signed := header + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signed))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signed + "." + enc.EncodeToString(sig), nil
}

// call makes a request to GitHub's API as the App and decodes its answer
// into out.
func (a *App) call(ctx context.Context, method, path, jwt string, in, out any) error {
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := a.hc.Do(req)
	if err != nil {
		return fmt.Errorf("github %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return ErrNotInstalled
	}
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("github %s %s: %s: %s", method, path, resp.Status, bytes.TrimSpace(msg))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// TokenRequest asks the daemon for a token: POST /github/token.
type TokenRequest struct {
	Repository string `json:"repository"` // owner/repo
}

// Handler serves POST /github/token for app; with no App, every request is
// 404, saying none is set up.
func Handler(app *App) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fail := func(code int, err error) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		}
		if app == nil {
			fail(http.StatusNotFound, errors.New("no GitHub App is set up: see install.sh"))
			return
		}
		var req TokenRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
			fail(http.StatusBadRequest, err)
			return
		}
		owner, repo, _ := strings.Cut(req.Repository, "/")
		t, err := app.Token(r.Context(), owner, strings.TrimSuffix(repo, ".git"))
		switch {
		case errors.Is(err, ErrBadRepository):
			fail(http.StatusBadRequest, err)
		case errors.Is(err, ErrNotInstalled):
			fail(http.StatusNotFound, fmt.Errorf("%s: %w", req.Repository, err))
		case err != nil:
			fail(http.StatusBadGateway, err)
		default:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(t)
		}
	})
}
