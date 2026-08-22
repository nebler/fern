package githubapp

import (
	"errors"
	"testing"
)

func TestRepositoryIdentityValidation(t *testing.T) {
	t.Parallel()
	identity, err := NewRepositoryIdentity(101, 202)
	if err != nil {
		t.Fatal(err)
	}
	if identity.InstallationID() != 101 || identity.RepositoryID() != 202 {
		t.Fatalf("identity = installation %d, repository %d", identity.InstallationID(), identity.RepositoryID())
	}
	for _, ids := range [][2]int64{{0, 1}, {-1, 1}, {1, 0}, {1, -1}} {
		if _, err := NewRepositoryIdentity(ids[0], ids[1]); !errors.Is(err, ErrInvalidIdentity) {
			t.Errorf("NewRepositoryIdentity(%d, %d) error = %v", ids[0], ids[1], err)
		}
	}
}

func TestRequiredRepositoryPermissions(t *testing.T) {
	t.Parallel()
	permissions, err := ValidateRepositoryPermissions(map[string]string{
		"contents":      "write",
		"pull_requests": "write",
		"metadata":      "read",
	})
	if err != nil {
		t.Fatal(err)
	}
	if permissions.Contents() != "write" || permissions.PullRequests() != "write" {
		t.Fatalf("permissions = contents:%q pull_requests:%q", permissions.Contents(), permissions.PullRequests())
	}

	for _, candidate := range []map[string]string{
		nil,
		{"contents": "read", "pull_requests": "write"},
		{"contents": "write", "pull_requests": "read"},
		{"contents": "write"},
		{"pull_requests": "write"},
	} {
		if _, err := ValidateRepositoryPermissions(candidate); !errors.Is(err, ErrInsufficientPermissions) {
			t.Errorf("permissions %v error = %v", candidate, err)
		}
	}
}
