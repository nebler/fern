package publication

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nebler/fern/internal/gitref"
)

func TestPrepareRecordsConfiguredIdentityAndFetchedBaseWithoutMutation(t *testing.T) {
	directory := t.TempDir()
	bin, repo := testCommandDirectories(t, directory)
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(directory, "commands.log")
	installFakePreflightCommands(t, bin, log, false)
	publisher, err := New("demo", repo, testRepositoryBinding())
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := publisher.Prepare(context.Background(), Request{Operation: "operation", Base: "main", Title: "Demo"})
	if err != nil {
		t.Fatal(err)
	}
	if prepared.RepositoryID != 123 || prepared.RepositoryFullName != "owner/repo" || prepared.BaseRef != "main" || prepared.BaseSHA != strings.Repeat("b", 40) || prepared.ResultCommit != strings.Repeat("a", 40) || prepared.Branch != "fern/demo/operation" {
		t.Fatalf("prepared tuple = %+v", prepared)
	}
	commands, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	api := strings.Index(string(commands), "repositories/123")
	fetch := strings.Index(string(commands), " fetch ")
	if api < 0 || fetch < 0 || api > fetch || strings.Contains(string(commands), " push ") || strings.Contains(string(commands), " --method POST ") {
		t.Fatalf("preflight command boundary violated:\n%s", commands)
	}
}

func TestPrepareRejectsRepositoryAPIMismatchBeforeFetch(t *testing.T) {
	directory := t.TempDir()
	bin, repo := testCommandDirectories(t, directory)
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(directory, "commands.log")
	installFakePreflightCommands(t, bin, log, true)
	publisher, err := New("demo", repo, testRepositoryBinding())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := publisher.Prepare(context.Background(), Request{Operation: "operation", Base: "main", Title: "Demo"}); err == nil || !strings.Contains(err.Error(), "identity") {
		t.Fatalf("repository mismatch error = %v", err)
	}
	commands, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(commands), " fetch ") || strings.Contains(string(commands), " push ") {
		t.Fatalf("API mismatch reached Git mutation:\n%s", commands)
	}
}

func TestCredentialBearingCommandErrorsAreRedacted(t *testing.T) {
	token := "abcdefghijklmnopqrstuvwxyz012345"
	err := run(context.Background(), "", []string{"FERN_GITHUB_TOKEN=" + token}, "/bin/sh", "-c", `printf '%s' "$FERN_GITHUB_TOKEN" >&2; exit 1`)
	if err == nil || strings.Contains(err.Error(), token) {
		t.Fatalf("credential-bearing error = %v", err)
	}
}

func TestValidBranchAllowsFernWorkspaceHyphen(t *testing.T) {
	t.Parallel()
	if gitref.ValidateRef("fern/github-test/operation") != nil {
		t.Fatal("canonical ref validation rejected a generated branch containing a workspace hyphen")
	}
	for _, invalid := range []string{"bad branch", "bad\x00branch", "bad..branch", "bad//branch", "bad.lock"} {
		if gitref.ValidateRef(invalid) == nil {
			t.Fatalf("canonical ref validation accepted %q", invalid)
		}
	}
}

func TestValidatePublicationPathsRejectsWorkflowChanges(t *testing.T) {
	t.Parallel()
	for _, path := range []string{".github/workflows/ordinary.yml", ".github/workflows/evil\n.yml", "README.md\x00.github/workflows/evil\t.yml"} {
		if err := validatePublicationPaths(path + "\x00"); err == nil {
			t.Fatalf("validatePublicationPaths accepted %q", path)
		}
	}
	if err := validatePublicationPaths("README.md\x00.github/actions/test/action.yml\x00"); err != nil {
		t.Fatalf("validatePublicationPaths rejected safe paths: %v", err)
	}
}

func TestGitHubRepositoryNameRejectsUnsupportedSchemes(t *testing.T) {
	t.Parallel()
	for _, remote := range []string{"http://github.com/owner/repo.git", "git://github.com/owner/repo.git", "ftp://github.com/owner/repo.git", "//github.com/owner/repo.git"} {
		if _, err := GitHubRepositoryName(remote); err == nil {
			t.Errorf("GitHubRepositoryName accepted %q", remote)
		}
	}
	for _, remote := range []string{"https://github.com/owner/repo.git", "ssh://git@github.com/owner/repo.git", "git@github.com:owner/repo.git"} {
		if got, err := GitHubRepositoryName(remote); err != nil || got != "owner/repo" {
			t.Errorf("GitHubRepositoryName(%q) = %q, %v", remote, got, err)
		}
	}
}

func TestValidateLocalGitConfigRejectsWorktreeOverrides(t *testing.T) {
	t.Parallel()
	for _, key := range []string{"core.worktree", "extensions.worktreeConfig"} {
		if err := ValidateLocalGitConfig(key + "\n/tmp/other\x00"); err == nil {
			t.Errorf("ValidateLocalGitConfig accepted %q", key)
		}
	}
}

func TestDecodeAPIJSONRejectsDuplicateKeysAndInvalidUTF8(t *testing.T) {
	t.Parallel()
	invalidUTF8 := append([]byte(`{"id":"`), 0xff)
	invalidUTF8 = append(invalidUTF8, []byte(`"}`)...)
	for _, value := range []string{`{"id":1,"id":2}`, `{"id":1,"ID":2}`, string(invalidUTF8)} {
		var destination struct {
			ID int64 `json:"id"`
		}
		if err := decodeAPIJSON(value, &destination); err == nil {
			t.Fatalf("ambiguous API JSON accepted: %q", value)
		}
	}
}

func TestInspectRepositoryRejectsGitlinkWithoutGitmodules(t *testing.T) {
	repo := t.TempDir()
	runTestGit(t, repo, "init", "--initial-branch=main")
	runTestGit(t, repo, "config", "user.name", "Fern Test")
	runTestGit(t, repo, "config", "user.email", "fern@example.invalid")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("demo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, repo, "add", "README.md")
	runTestGit(t, repo, "commit", "-m", "initial")
	head := strings.TrimSpace(runTestGit(t, repo, "rev-parse", "HEAD"))
	runTestGit(t, repo, "update-index", "--add", "--cacheinfo", "160000,"+head+",vendor/sub")
	runTestGit(t, repo, "commit", "-m", "gitlink")
	runTestGit(t, repo, "remote", "add", "origin", "https://github.com/owner/repo.git")
	if _, err := InspectRepository(context.Background(), repo); err == nil || !strings.Contains(err.Error(), "submodules") {
		t.Fatalf("InspectRepository gitlink error = %v", err)
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
	if _, err := New("demo", repo, testRepositoryBinding()); err == nil || !strings.Contains(err.Error(), "inside the repository") {
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
	if _, err := New("demo", repo, testRepositoryBinding()); err == nil || !strings.Contains(err.Error(), "group or world writable") {
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

func testRepositoryBinding() RepositoryBinding {
	return RepositoryBinding{ID: 123, FullName: "owner/repo"}
}

func runTestGit(t *testing.T, directory string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
	return string(output)
}

func installFakePreflightCommands(t *testing.T, directory, log string, mismatch bool) {
	t.Helper()
	git := fmt.Sprintf(`#!/bin/sh
printf 'git %%s\n' "$*" >> %[1]q
case " $* " in
  *" config --local --null --list "*) exit 0 ;;
  *" remote get-url origin "*) printf 'https://github.com/owner/repo.git\n'; exit 0 ;;
  *" ls-files --stage "*) exit 0 ;;
  *" status --porcelain=v1 "*) exit 0 ;;
  *" fetch "*) exit 0 ;;
  *" rev-parse --verify FETCH_HEAD"*) printf 'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\n'; exit 0 ;;
  *" rev-parse --verify HEAD"*) printf 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n'; exit 0 ;;
  *" merge-base --is-ancestor "*) exit 0 ;;
  *" diff --name-only "*) exit 0 ;;
  *" push "*) exit 97 ;;
esac
exit 1
`, log)
	repositoryID := 123
	if mismatch {
		repositoryID = 999
	}
	gh := fmt.Sprintf(`#!/bin/sh
printf 'gh %%s\n' "$*" >> %[1]q
case " $* " in
  *" auth token "*) printf 'abcdefghijklmnopqrstuvwxyz012345\n'; exit 0 ;;
  *" api --hostname github.com --method GET repositories/123 "*)
    printf '{"id":%[2]d,"full_name":"owner/repo","name":"repo","default_branch":"main","owner":{"login":"owner"}}\n'
    exit 0 ;;
  *" --method POST "*) exit 97 ;;
esac
exit 1
`, log, repositoryID)
	for name, script := range map[string]string{"git": git, "gh": gh} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", directory)
}
