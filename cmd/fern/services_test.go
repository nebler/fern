package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"testing"

	"github.com/nebler/fern/internal/config"
	"github.com/nebler/fern/internal/githubapp"
)

func TestGitHubAuthorityRequiresHostAppCredentialsAndProvidesTokens(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	github := config.GitHubApp{InstallationID: 123,
		Repository: config.GitHubRepository{ID: 456, FullName: "owner/repository"}}
	if _, err := resolveGitHubAuthority(github); !errors.Is(err, githubapp.ErrCredentialsNotFound) {
		t.Fatalf("missing host credentials = %v", err)
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := githubapp.NewAppCredentials(123, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	if err != nil {
		t.Fatal(err)
	}
	directory, err := statePath("github-app")
	if err != nil {
		t.Fatal(err)
	}
	store, err := githubapp.NewCredentialStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(credentials); err != nil {
		t.Fatal(err)
	}
	tokens, err := resolveGitHubAuthority(github)
	if err != nil {
		t.Fatal(err)
	}
	if tokens == nil {
		t.Fatal("production App authority omitted installation token source")
	}
	github.InstallationID = 0
	if _, err := resolveGitHubAuthority(github); !errors.Is(err, githubapp.ErrInvalidIdentity) {
		t.Fatalf("unbound installation accepted: %v", err)
	}
	github.InstallationID, github.Repository.ID = 123, 0
	if _, err := resolveGitHubAuthority(github); !errors.Is(err, githubapp.ErrInvalidIdentity) {
		t.Fatalf("unbound repository accepted: %v", err)
	}
}
