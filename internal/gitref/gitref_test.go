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

func TestValidSHA1(t *testing.T) {
	t.Parallel()
	if !ValidSHA1("0123456789abcdef0123456789abcdef01234567") {
		t.Fatal("ValidSHA1 rejected a lowercase hex SHA-1")
	}
	for _, sha := range []string{
		"", "short", "0123456789ABCDEF0123456789abcdef01234567",
		"0123456789abcdef0123456789abcdef0123456g",
		"0123456789abcdef0123456789abcdef012345678",
	} {
		if ValidSHA1(sha) {
			t.Errorf("ValidSHA1(%q) accepted invalid SHA-1", sha)
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
