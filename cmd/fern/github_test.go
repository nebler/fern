package main

import (
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestStandaloneGitHubMutationIsRejectedBeforeDependencies(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	err := runGitHubPublish([]string{"--title", "Must persist"}, slog.Default())
	if err == nil || !strings.Contains(err.Error(), "durable") || !strings.Contains(err.Error(), "running") {
		t.Fatalf("mutation rejection = %v", err)
	}
}

func TestGitHubRepositoryName(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"https://github.com/owner/repo.git": "owner/repo",
		"ssh://git@github.com/owner/repo":   "owner/repo",
		"git@github.com:owner/repo.git":     "owner/repo",
	}
	for input, want := range tests {
		if got, err := githubRepositoryName(input); err != nil || got != want {
			t.Errorf("githubRepositoryName(%q) = %q, %v", input, got, err)
		}
	}
	for _, input := range []string{"https://token@github.com/owner/repo", "https://example.com/owner/repo", "https://github.com/owner/repo/extra", "file:///tmp/repo"} {
		if _, err := githubRepositoryName(input); err == nil {
			t.Errorf("githubRepositoryName accepted %q", input)
		}
	}
}

func TestValidateLocalGitConfigRejectsCredentialControls(t *testing.T) {
	t.Parallel()
	for _, key := range []string{"credential.helper", "url.https://evil/.insteadof", "core.sshcommand", "remote.origin.pushurl", "include.path"} {
		if err := validateLocalGitConfig(key + "\nvalue\x00"); err == nil {
			t.Errorf("accepted unsafe key %s", key)
		}
	}
}

func TestInspectPublishRepositoryAcceptsCleanGitHubCheckout(t *testing.T) {
	directory := t.TempDir()
	runGitTest(t, directory, "init", "--initial-branch=main")
	runGitTest(t, directory, "config", "user.name", "Fern Test")
	runGitTest(t, directory, "config", "user.email", "fern@example.invalid")
	if err := os.WriteFile(filepath.Join(directory, "README.md"), []byte("demo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, directory, "add", "README.md")
	runGitTest(t, directory, "commit", "-m", "demo")
	runGitTest(t, directory, "remote", "add", "origin", "https://github.com/owner/repo.git")
	inspection, err := inspectPublishRepository(directory)
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Name != "owner/repo" || len(inspection.Head) != 40 {
		t.Fatalf("inspection = %+v", inspection)
	}
	if err := os.WriteFile(filepath.Join(directory, "dirty"), []byte("dirty"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := inspectPublishRepository(directory); err == nil || !strings.Contains(err.Error(), "clean") {
		t.Fatalf("dirty repository error = %v", err)
	}
}

func runGitTest(t *testing.T, directory string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = directory
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
}
