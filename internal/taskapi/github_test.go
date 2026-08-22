package taskapi

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nebler/fern/internal/githubapp"
)

type fakeReferenceReader struct {
	read func(context.Context, githubapp.RepositoryIdentity, string, string) (githubapp.GitReferenceObservation, error)
}

func (reader fakeReferenceReader) BranchReference(ctx context.Context, identity githubapp.RepositoryIdentity, fullName, ref string) (githubapp.GitReferenceObservation, error) {
	return reader.read(ctx, identity, fullName, ref)
}

func TestGitHubBaseResolverUsesBoundedExactObservation(t *testing.T) {
	t.Parallel()
	identity, err := githubapp.NewRepositoryIdentity(10, 20)
	if err != nil {
		t.Fatal(err)
	}
	// An observation can only be constructed by the GitHub client, so use a
	// real client contract test for success and keep adapter failures closed.
	reader := fakeReferenceReader{read: func(ctx context.Context, got githubapp.RepositoryIdentity, fullName, ref string) (githubapp.GitReferenceObservation, error) {
		if _, ok := ctx.Deadline(); !ok || got != identity || fullName != "owner/repo" || ref != "main" {
			t.Fatal("resolver did not preserve bounded authority")
		}
		return githubapp.GitReferenceObservation{}, errors.New("unavailable")
	}}
	resolver, err := GitHubBaseResolver(reader, identity, "owner/repo", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolver(context.Background(), "main"); err == nil {
		t.Fatal("reader failure was not preserved")
	}
}

func TestGitHubBaseResolverRejectsIncompleteConfiguration(t *testing.T) {
	t.Parallel()
	identity, _ := githubapp.NewRepositoryIdentity(10, 20)
	reader := fakeReferenceReader{read: func(context.Context, githubapp.RepositoryIdentity, string, string) (githubapp.GitReferenceObservation, error) {
		return githubapp.GitReferenceObservation{}, nil
	}}
	for _, build := range []func() (BaseResolver, error){
		func() (BaseResolver, error) { return GitHubBaseResolver(nil, identity, "owner/repo", time.Second) },
		func() (BaseResolver, error) {
			return GitHubBaseResolver(reader, githubapp.RepositoryIdentity{}, "owner/repo", time.Second)
		},
		func() (BaseResolver, error) { return GitHubBaseResolver(reader, identity, "", time.Second) },
		func() (BaseResolver, error) { return GitHubBaseResolver(reader, identity, "owner/repo", 0) },
	} {
		if _, err := build(); err == nil {
			t.Fatal("invalid configuration accepted")
		}
	}
}
