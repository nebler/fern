package main

import (
	"context"
	"testing"
	"time"

	"github.com/nebler/fern/internal/githubapp"
)

func TestGitHubFixtureScopedTokens(t *testing.T) {
	f, err := newGitHubFixture()
	if err != nil {
		t.Fatal(err)
	}
	defer f.server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	identity, err := githubapp.NewRepositoryIdentity(7, 42)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range dummyGitHubTokens {
		token, err := f.source.InstallationToken(ctx, identity)
		if err != nil {
			t.Fatal("mint dummy scoped token:", err)
		}
		value, err := token.Value(time.Now())
		if err != nil || value != expected || token.Identity() != identity || token.Permissions().Contents() != "write" || token.Permissions().PullRequests() != "write" {
			t.Fatal("dummy scoped token validation failed")
		}
		if remaining := time.Until(token.ExpiresAt()); remaining < 59*time.Minute || remaining > time.Hour {
			t.Fatal("dummy token expiry differs")
		}
	}
	wrong, err := githubapp.NewRepositoryIdentity(7, 43)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.source.InstallationToken(ctx, wrong); err == nil {
		t.Fatal("fake endpoint accepted another repository")
	}
	if f.calls.Load() != 2 {
		t.Fatal("unexpected mint count")
	}
}
