package publication

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPublishPreparedReconcilesLostPushAndPullRequestResponses(t *testing.T) {
	directory := t.TempDir()
	bin, repo := testCommandDirectories(t, directory)
	remote := filepath.Join(directory, "remote")
	pull := filepath.Join(directory, "pull.json")
	log := filepath.Join(directory, "commands.log")
	installFakeGitHubCommands(t, bin, remote, pull, log, true, true, false)
	publisher, err := New("demo", repo)
	if err != nil {
		t.Fatal(err)
	}
	prepared := Prepared{
		Repository: "owner/repo", Base: "main", Commit: strings.Repeat("a", 40), Branch: "fern/demo/operation",
	}
	result, err := publisher.PublishPrepared(context.Background(), prepared, "Demo change", "Body")
	if err != nil {
		t.Fatal(err)
	}
	if result.URL != "https://github.com/owner/repo/pull/1" {
		t.Fatalf("pull URL = %q", result.URL)
	}
	commands, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(commands), "--force") || strings.Count(string(commands), " pr create ") != 1 {
		t.Fatalf("unsafe or duplicate command sequence:\n%s", commands)
	}
}

func TestPublishPreparedDoesNotCreateWhenLookupFails(t *testing.T) {
	directory := t.TempDir()
	bin, repo := testCommandDirectories(t, directory)
	remote := filepath.Join(directory, "remote")
	pull := filepath.Join(directory, "pull.json")
	log := filepath.Join(directory, "commands.log")
	installFakeGitHubCommands(t, bin, remote, pull, log, false, false, true)
	if err := os.WriteFile(remote, []byte(strings.Repeat("a", 40)), 0o600); err != nil {
		t.Fatal(err)
	}
	publisher, err := New("demo", repo)
	if err != nil {
		t.Fatal(err)
	}
	_, err = publisher.PublishPrepared(context.Background(), Prepared{
		Repository: "owner/repo", Base: "main", Commit: strings.Repeat("a", 40), Branch: "fern/demo/operation",
	}, "Demo change", "Body")
	if err == nil || !strings.Contains(err.Error(), "inspect existing") {
		t.Fatalf("lookup error = %v", err)
	}
	commands, readErr := os.ReadFile(log)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if strings.Contains(string(commands), " pr create ") {
		t.Fatalf("PR create followed failed lookup:\n%s", commands)
	}
}

func TestPublishPreparedRejectsConflictingRemoteBranch(t *testing.T) {
	directory := t.TempDir()
	bin, repo := testCommandDirectories(t, directory)
	remote := filepath.Join(directory, "remote")
	pull := filepath.Join(directory, "pull.json")
	log := filepath.Join(directory, "commands.log")
	installFakeGitHubCommands(t, bin, remote, pull, log, false, false, false)
	if err := os.WriteFile(remote, []byte(strings.Repeat("b", 40)), 0o600); err != nil {
		t.Fatal(err)
	}
	publisher, err := New("demo", repo)
	if err != nil {
		t.Fatal(err)
	}
	_, err = publisher.PublishPrepared(context.Background(), Prepared{
		Repository: "owner/repo", Base: "main", Commit: strings.Repeat("a", 40), Branch: "fern/demo/operation",
	}, "Demo change", "Body")
	if err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("conflict error = %v", err)
	}
}

func TestValidBranchAllowsFernWorkspaceHyphen(t *testing.T) {
	t.Parallel()
	if !ValidBranch("fern/github-test/operation") {
		t.Fatal("ValidBranch rejected a generated branch containing a workspace hyphen")
	}
	for _, invalid := range []string{"bad branch", "bad\x00branch", "bad..branch", "bad//branch", "bad.lock"} {
		if ValidBranch(invalid) {
			t.Fatalf("ValidBranch accepted %q", invalid)
		}
	}
}

func TestValidatePublicationPathsRejectsUnusualWorkflowFilename(t *testing.T) {
	t.Parallel()
	for _, path := range []string{
		".github/workflows/ordinary.yml",
		".github/workflows/evil\n.yml",
		"README.md\x00.github/workflows/evil\t.yml",
	} {
		if err := validatePublicationPaths(path + "\x00"); err == nil {
			t.Fatalf("validatePublicationPaths accepted %q", path)
		}
	}
	if err := validatePublicationPaths("README.md\x00.github/actions/test/action.yml\x00"); err != nil {
		t.Fatalf("validatePublicationPaths rejected safe paths: %v", err)
	}
}

func TestNewRejectsRepositoryExecutable(t *testing.T) {
	repo := t.TempDir()
	for _, name := range []string{"git", "gh"} {
		if err := os.WriteFile(filepath.Join(repo, name), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", repo)
	if _, err := New("demo", repo); err == nil || !strings.Contains(err.Error(), "inside the repository") {
		t.Fatalf("New error = %v", err)
	}
}

func TestNewRejectsWritableExecutable(t *testing.T) {
	root := t.TempDir()
	bin, repo := testCommandDirectories(t, root)
	for _, name := range []string{"git", "gh"} {
		path := filepath.Join(bin, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o722); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin)
	if _, err := New("demo", repo); err == nil || !strings.Contains(err.Error(), "group or world writable") {
		t.Fatalf("New error = %v", err)
	}
}

func testCommandDirectories(t *testing.T, root string) (string, string) {
	t.Helper()
	bin, repo := filepath.Join(root, "bin"), filepath.Join(root, "repo")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	return bin, repo
}

func installFakeGitHubCommands(t *testing.T, directory, remote, pull, log string, losePush, loseCreate, failLookup bool) {
	t.Helper()
	git := fmt.Sprintf(`#!/bin/sh
printf 'git %%s\n' "$*" >> %[1]q
case " $* " in
  *" cat-file "*) exit 0 ;;
  *" ls-remote "*)
    if test -f %[2]q; then printf '%%s\trefs/heads/fern/demo/operation\n' "$(/bin/cat %[2]q)"; fi
    exit 0 ;;
  *" push "*)
    printf 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' > %[2]q
    test %[3]t = true && exit 1
    exit 0 ;;
esac
exit 1
`, log, remote, losePush)
	gh := fmt.Sprintf(`#!/bin/sh
printf 'gh %%s\n' "$*" >> %[1]q
case " $* " in
  *" auth status "*) exit 0 ;;
  *" auth token "*) printf 'abcdefghijklmnopqrstuvwxyz012345\n'; exit 0 ;;
  *" pr list "*)
    test %[3]t = true && exit 1
    if test -f %[2]q; then /bin/cat %[2]q; else printf '[]\n'; fi
    exit 0 ;;
  *" pr create "*)
    printf '[{"number":1,"url":"https://github.com/owner/repo/pull/1","state":"OPEN","isDraft":true,"headRefOid":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","headRefName":"fern/demo/operation","baseRefName":"main"}]' > %[2]q
    test %[4]t = true && exit 1
    printf 'https://github.com/owner/repo/pull/1\n'
    exit 0 ;;
esac
exit 1
`, log, pull, failLookup, loseCreate)
	for name, script := range map[string]string{"git": git, "gh": gh} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", directory)
}
