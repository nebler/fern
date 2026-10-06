package gitref

import (
	"strings"
	"testing"
)

func TestValidateRef(t *testing.T) {
	t.Parallel()
	for _, ref := range []string{
		"main", "refs/heads/main", "fern/github-test/operation", "release/v1", "feature-x", "v1.2.3",
	} {
		if err := ValidateRef(ref); err != nil {
			t.Errorf("ValidateRef(%q) = %v, want nil", ref, err)
		}
	}
	for _, ref := range []string{
		"", "@", "-leading", "/leading", "trailing/", "trailing.", "double//slash",
		"dot..dot", "at@{brace", "space space", "tilde~", "caret^", "colon:", "question?",
		"star*", "bracket[", "backslash\\", ".hidden", "component/.hidden", "component.lock",
		"component/.LOCK", "bad\x00ref", "café", strings.Repeat("a", 256),
	} {
		if err := ValidateRef(ref); err == nil {
			t.Errorf("ValidateRef(%q) accepted invalid ref", ref)
		}
	}
}

func TestValidateOwnerRepo(t *testing.T) {
	t.Parallel()
	longOwner := strings.Repeat("a", 39) + "/" + strings.Repeat("b", 100)
	for _, fullName := range []string{"o/r", "owner/repository", "owner/repo.name_x-1", longOwner} {
		if err := ValidateOwnerRepo(fullName); err != nil {
			t.Errorf("ValidateOwnerRepo(%q) = %v, want nil", fullName, err)
		}
	}
	tooLongOwner := strings.Repeat("a", 40) + "/repo"
	tooLongRepo := "owner/" + strings.Repeat("b", 101)
	for _, fullName := range []string{
		"", "owner", "owner/repo/extra", "/repo", "owner/", "-owner/repo", "owner-/repo",
		"owner/.git", "owner/repo.git", "owner/repo.GIT", "owner/.", "owner/..", "ow!ner/repo",
		"owner/re!po", tooLongOwner, tooLongRepo, strings.Repeat("a", 200),
	} {
		if err := ValidateOwnerRepo(fullName); err == nil {
			t.Errorf("ValidateOwnerRepo(%q) accepted invalid full name", fullName)
		}
	}
}

func TestValidPath(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"README.md", "internal/gitref/gitref.go", "a/b/c.txt"} {
		if !ValidPath(path) {
			t.Errorf("ValidPath(%q) rejected safe path", path)
		}
	}
	for _, path := range []string{
		"", "/absolute", "trailing/", "a//b", "./current", "../parent", "a/../b", "a/./b",
		"nul\x00byte", strings.Repeat("a", 4097),
	} {
		if ValidPath(path) {
			t.Errorf("ValidPath(%q) accepted unsafe path", path)
		}
	}
	if !ValidPathBytes([]byte("internal/gitref/gitref.go")) || ValidPathBytes([]byte("../escape")) {
		t.Fatal("ValidPathBytes disagrees with ValidPath")
	}
}

func TestValidateGitHubRemote(t *testing.T) {
	t.Parallel()
	for _, remote := range []string{"https://github.com/owner/repository", "https://github.com/o/r", "https://github.com/owner/repo.name_x-1"} {
		if err := ValidateGitHubRemote(remote); err != nil {
			t.Errorf("ValidateGitHubRemote(%q) = %v, want nil", remote, err)
		}
	}
	for _, remote := range []string{
		"", "https://github.com/", "https://github.com/owner", "https://github.com/owner/", "https://github.com/owner/repo/",
		"https://github.com/owner/repo.git", "https://github.com/owner/repo.GIT", "https://github.com/owner/repo/extra",
		"http://github.com/owner/repo", "https://GitHub.com/owner/repo", "HTTPS://github.com/owner/repo",
		"https://user@github.com/owner/repo", "https://github.com:443/owner/repo", "https://github.com/owner/repo?x=1",
		"https://github.com/owner/repo#frag", "https://github.com/own%65r/repo", "https://github.com//owner/repo",
		"https://example.com/owner/repo", "git@github.com:owner/repo", "https://github.com/owner/re po",
	} {
		if err := ValidateGitHubRemote(remote); err == nil {
			t.Errorf("ValidateGitHubRemote(%q) accepted invalid remote", remote)
		}
	}
}
