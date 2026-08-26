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

func TestPublishPreparedReconcilesLostPushAndPullRequestResponses(t *testing.T) {
	directory := t.TempDir()
	bin, repo := testCommandDirectories(t, directory)
	remote := filepath.Join(directory, "remote")
	pull := filepath.Join(directory, "pull.json")
	log := filepath.Join(directory, "commands.log")
	installFakeGitHubCommands(t, bin, remote, pull, log, true, true, false, false, false)
	publisher, err := New("demo", repo, testRepositoryBinding())
	if err != nil {
		t.Fatal(err)
	}
	prepared := Prepared{
		RepositoryID: 123, RepositoryFullName: "owner/repo", BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ResultCommit: strings.Repeat("a", 40), Branch: "fern/demo/operation",
	}
	result, err := publisher.PublishPrepared(context.Background(), prepared, "Demo change", "Body")
	if err != nil {
		t.Fatal(err)
	}
	if result.PullRequest.URL != "https://github.com/owner/repo/pull/1" {
		t.Fatalf("pull URL = %q", result.PullRequest.URL)
	}
	commands, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(commands), "--force") || strings.Count(string(commands), " --method POST ") != 1 {
		t.Fatalf("unsafe or duplicate command sequence:\n%s", commands)
	}
}

func TestPublishPreparedDoesNotCreateWhenLookupFails(t *testing.T) {
	directory := t.TempDir()
	bin, repo := testCommandDirectories(t, directory)
	remote := filepath.Join(directory, "remote")
	pull := filepath.Join(directory, "pull.json")
	log := filepath.Join(directory, "commands.log")
	installFakeGitHubCommands(t, bin, remote, pull, log, false, false, true, false, false)
	if err := os.WriteFile(remote, []byte(strings.Repeat("a", 40)), 0o600); err != nil {
		t.Fatal(err)
	}
	publisher, err := New("demo", repo, testRepositoryBinding())
	if err != nil {
		t.Fatal(err)
	}
	_, err = publisher.PublishPrepared(context.Background(), Prepared{
		RepositoryID: 123, RepositoryFullName: "owner/repo", BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ResultCommit: strings.Repeat("a", 40), Branch: "fern/demo/operation",
	}, "Demo change", "Body")
	if err == nil || !strings.Contains(err.Error(), "inspect existing") {
		t.Fatalf("lookup error = %v", err)
	}
	commands, readErr := os.ReadFile(log)
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatal(readErr)
	}
	if strings.Contains(string(commands), " --method POST ") {
		t.Fatalf("PR create followed failed lookup:\n%s", commands)
	}
}

func TestPublishPreparedRejectsConflictingRemoteBranch(t *testing.T) {
	directory := t.TempDir()
	bin, repo := testCommandDirectories(t, directory)
	remote := filepath.Join(directory, "remote")
	pull := filepath.Join(directory, "pull.json")
	log := filepath.Join(directory, "commands.log")
	installFakeGitHubCommands(t, bin, remote, pull, log, false, false, false, false, false)
	if err := os.WriteFile(remote, []byte(strings.Repeat("b", 40)), 0o600); err != nil {
		t.Fatal(err)
	}
	publisher, err := New("demo", repo, testRepositoryBinding())
	if err != nil {
		t.Fatal(err)
	}
	_, err = publisher.PublishPrepared(context.Background(), Prepared{
		RepositoryID: 123, RepositoryFullName: "owner/repo", BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ResultCommit: strings.Repeat("a", 40), Branch: "fern/demo/operation",
	}, "Demo change", "Body")
	if err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("conflict error = %v", err)
	}
}

func TestPublishPreparedRejectsInvalidRecoveredBranch(t *testing.T) {
	directory := t.TempDir()
	bin, repo := testCommandDirectories(t, directory)
	remote := filepath.Join(directory, "remote")
	pull := filepath.Join(directory, "pull.json")
	log := filepath.Join(directory, "commands.log")
	installFakeGitHubCommands(t, bin, remote, pull, log, false, false, false, false, false)
	publisher, err := New("demo", repo, testRepositoryBinding())
	if err != nil {
		t.Fatal(err)
	}
	commit := strings.Repeat("a", 40)
	tests := []Prepared{
		{RepositoryID: 123, RepositoryFullName: "owner/repo", BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ResultCommit: commit, Branch: "fern/demo/"},
		{RepositoryID: 123, RepositoryFullName: "owner/repo", BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ResultCommit: commit, Branch: "fern/other/operation"},
		{RepositoryID: 123, RepositoryFullName: "owner/repo", BaseSHA: strings.Repeat("b", 40), BaseRef: "fern/demo/operation", ResultCommit: commit, Branch: "fern/demo/operation"},
	}
	for _, prepared := range tests {
		if _, err := publisher.PublishPrepared(context.Background(), prepared, "Demo change", "Body"); err == nil || !strings.Contains(err.Error(), "recorded publication branch") {
			t.Errorf("PublishPrepared(%+v) error = %v", prepared, err)
		}
	}
	if commands, err := os.ReadFile(log); err == nil && len(commands) != 0 {
		t.Fatalf("invalid prepared records executed commands:\n%s", commands)
	} else if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestPublishPreparedReconcilesSuccessfulCreate(t *testing.T) {
	directory := t.TempDir()
	bin, repo := testCommandDirectories(t, directory)
	remote := filepath.Join(directory, "remote")
	pull := filepath.Join(directory, "pull.json")
	log := filepath.Join(directory, "commands.log")
	installFakeGitHubCommands(t, bin, remote, pull, log, false, false, false, true, false)
	if err := os.WriteFile(remote, []byte(strings.Repeat("a", 40)), 0o600); err != nil {
		t.Fatal(err)
	}
	publisher, err := New("demo", repo, testRepositoryBinding())
	if err != nil {
		t.Fatal(err)
	}
	_, err = publisher.PublishPrepared(context.Background(), Prepared{
		RepositoryID: 123, RepositoryFullName: "owner/repo", BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ResultCommit: strings.Repeat("a", 40), Branch: "fern/demo/operation",
	}, "Demo change", "Body")
	if err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("create reconciliation error = %v", err)
	}
	commands, readErr := os.ReadFile(log)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if strings.Count(string(commands), "pulls?") != 2 || !strings.Contains(string(commands), "--method GET repos/owner/repo/pulls/1") {
		t.Fatalf("successful create was not read back:\n%s", commands)
	}
}

func TestPublishPreparedRejectsPullURLNumberMismatch(t *testing.T) {
	directory := t.TempDir()
	bin, repo := testCommandDirectories(t, directory)
	remote := filepath.Join(directory, "remote")
	pull := filepath.Join(directory, "pull.json")
	log := filepath.Join(directory, "commands.log")
	installFakeGitHubCommands(t, bin, remote, pull, log, false, false, false, false, false)
	if err := os.WriteFile(remote, []byte(strings.Repeat("a", 40)), 0o600); err != nil {
		t.Fatal(err)
	}
	value := `{"number":2,"html_url":"https://github.com/owner/repo/pull/1","state":"open","draft":true,"base":{"ref":"main","sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","repo":{"id":123,"full_name":"owner/repo","name":"repo","owner":{"login":"owner"}}},"head":{"ref":"fern/demo/operation","sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","repo":{"id":123,"full_name":"owner/repo","name":"repo","owner":{"login":"owner"}}}}`
	if err := os.WriteFile(pull, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
	publisher, err := New("demo", repo, testRepositoryBinding())
	if err != nil {
		t.Fatal(err)
	}
	_, err = publisher.PublishPrepared(context.Background(), Prepared{
		RepositoryID: 123, RepositoryFullName: "owner/repo", BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ResultCommit: strings.Repeat("a", 40), Branch: "fern/demo/operation",
	}, "Demo change", "Body")
	if err == nil || !strings.Contains(err.Error(), "invalid pull request URL") {
		t.Fatalf("pull URL mismatch error = %v", err)
	}
}

func TestPublishPreparedRejectsRepositoryDifferentFromOrigin(t *testing.T) {
	directory := t.TempDir()
	bin, repo := testCommandDirectories(t, directory)
	remote := filepath.Join(directory, "remote")
	pull := filepath.Join(directory, "pull.json")
	log := filepath.Join(directory, "commands.log")
	installFakeGitHubCommands(t, bin, remote, pull, log, false, false, false, false, false)
	publisher, err := New("demo", repo, testRepositoryBinding())
	if err != nil {
		t.Fatal(err)
	}
	_, err = publisher.PublishPrepared(context.Background(), Prepared{
		RepositoryID: 123, RepositoryFullName: "other/repo", BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ResultCommit: strings.Repeat("a", 40), Branch: "fern/demo/operation",
	}, "Demo change", "Body")
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("repository binding error = %v", err)
	}
	commands, readErr := os.ReadFile(log)
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatal(readErr)
	}
	if strings.Contains(string(commands), "gh ") || strings.Contains(string(commands), "ls-remote") || strings.Contains(string(commands), " push ") {
		t.Fatalf("repository mismatch reached external commands:\n%s", commands)
	}
}

func TestPublishPreparedRejectsGitlinkInRecordedCommit(t *testing.T) {
	directory := t.TempDir()
	bin, repo := testCommandDirectories(t, directory)
	remote := filepath.Join(directory, "remote")
	pull := filepath.Join(directory, "pull.json")
	log := filepath.Join(directory, "commands.log")
	installFakeGitHubCommands(t, bin, remote, pull, log, false, false, false, false, true)
	publisher, err := New("demo", repo, testRepositoryBinding())
	if err != nil {
		t.Fatal(err)
	}
	_, err = publisher.PublishPrepared(context.Background(), Prepared{
		RepositoryID: 123, RepositoryFullName: "owner/repo", BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ResultCommit: strings.Repeat("a", 40), Branch: "fern/demo/operation",
	}, "Demo change", "Body")
	if err == nil || !strings.Contains(err.Error(), "submodules") {
		t.Fatalf("recorded gitlink error = %v", err)
	}
	commands, readErr := os.ReadFile(log)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if strings.Contains(string(commands), "gh ") || strings.Contains(string(commands), "ls-remote") || strings.Contains(string(commands), " push ") {
		t.Fatalf("recorded gitlink reached external commands:\n%s", commands)
	}
}

func TestPreparePersistsConfiguredIdentityAndExactFetchedBase(t *testing.T) {
	directory := t.TempDir()
	bin, repo := testCommandDirectories(t, directory)
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(directory, "commands.log")
	installFakeGitHubCommands(t, bin, filepath.Join(directory, "remote"), filepath.Join(directory, "pull.json"), log, false, false, false, false, false)
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
	if api < 0 || fetch < 0 || api > fetch {
		t.Fatalf("repository API was not verified before fetch:\n%s", commands)
	}
}

func TestPrepareRejectsRepositoryAPIMismatchBeforeFetch(t *testing.T) {
	directory := t.TempDir()
	bin, repo := testCommandDirectories(t, directory)
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(directory, "commands.log")
	installFakeGitHubCommands(t, bin, filepath.Join(directory, "remote"), filepath.Join(directory, "pull.json"), log, false, false, false, false, false)
	ghPath := filepath.Join(bin, "gh")
	script, err := os.ReadFile(ghPath)
	if err != nil {
		t.Fatal(err)
	}
	script = []byte(strings.Replace(string(script), `{"id":123,`, `{"id":999,`, 1))
	if !strings.Contains(string(script), `{"id":999,`) {
		t.Fatal("failed to alter fake repository response")
	}
	if err := os.WriteFile(ghPath, script, 0o700); err != nil {
		t.Fatal(err)
	}
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

func TestPublishPreparedRejectsBaseMovementBeforePush(t *testing.T) {
	directory := t.TempDir()
	bin, repo := testCommandDirectories(t, directory)
	log := filepath.Join(directory, "commands.log")
	installFakeGitHubCommands(t, bin, filepath.Join(directory, "remote"), filepath.Join(directory, "pull.json"), log, false, false, false, false, false)
	publisher, err := New("demo", repo, testRepositoryBinding())
	if err != nil {
		t.Fatal(err)
	}
	prepared := Prepared{RepositoryID: 123, RepositoryFullName: "owner/repo", BaseSHA: strings.Repeat("c", 40), BaseRef: "main", ResultCommit: strings.Repeat("a", 40), Branch: "fern/demo/operation"}
	if _, err := publisher.PublishPrepared(context.Background(), prepared, "Demo", ""); err == nil || !strings.Contains(err.Error(), "moved") {
		t.Fatalf("base movement error = %v", err)
	}
	commands, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(commands), " push ") || strings.Contains(string(commands), " --method POST ") {
		t.Fatalf("base movement reached mutation:\n%s", commands)
	}
}

func TestCredentialBearingCommandErrorsAreRedacted(t *testing.T) {
	token := "abcdefghijklmnopqrstuvwxyz012345"
	err := run(context.Background(), "", []string{"FERN_GITHUB_TOKEN=" + token}, "/bin/sh", "-c", `printf '%s' "$FERN_GITHUB_TOKEN" >&2; exit 1`)
	if err == nil || strings.Contains(err.Error(), token) {
		t.Fatalf("credential-bearing error = %v", err)
	}
}

func TestPullRequestDiscoveryRejectsMultipleAndNullResponses(t *testing.T) {
	prepared := Prepared{RepositoryID: 123, RepositoryFullName: "owner/repo", BaseSHA: strings.Repeat("b", 40), BaseRef: "main", ResultCommit: strings.Repeat("a", 40), Branch: "fern/demo/operation"}
	pull := `{"number":1,"state":"open","draft":true,"base":{"ref":"main","sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","repo":{"id":123,"full_name":"owner/repo","name":"repo","owner":{"login":"owner"}}},"head":{"ref":"fern/demo/operation","sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","repo":{"id":123,"full_name":"owner/repo","name":"repo","owner":{"login":"owner"}}}}`
	responses := map[string]string{"multiple": "[" + pull + "," + pull + "]", "null": "null"}
	for name, response := range responses {
		t.Run(name, func(t *testing.T) {
			gh := filepath.Join(t.TempDir(), "gh")
			script := "#!/bin/sh\nprintf '%s\\n' " + fmt.Sprintf("%q", response) + "\n"
			if err := os.WriteFile(gh, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			if _, _, err := findPullRequest(context.Background(), nil, gh, prepared); err == nil {
				t.Fatalf("discovery accepted %s response", name)
			}
		})
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

func TestGitHubRepositoryNameRejectsUnsupportedSchemes(t *testing.T) {
	t.Parallel()
	for _, remote := range []string{
		"http://github.com/owner/repo.git",
		"git://github.com/owner/repo.git",
		"ftp://github.com/owner/repo.git",
		"//github.com/owner/repo.git",
	} {
		if _, err := GitHubRepositoryName(remote); err == nil {
			t.Errorf("GitHubRepositoryName accepted %q", remote)
		}
	}
	for _, remote := range []string{
		"https://github.com/owner/repo.git",
		"ssh://git@github.com/owner/repo.git",
		"git@github.com:owner/repo.git",
	} {
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

func TestValidatePullURLRequiresCanonicalPullRoute(t *testing.T) {
	t.Parallel()
	for _, value := range []string{
		"https://user@github.com/owner/repo/pull/1",
		"https://github.com/owner/repo/pull/1/files",
		"https://github.com/owner/repo/pull/1/",
		"https://github.com/owner/repo/pull/0",
		"https://github.com/owner/repo/pull/01",
		"https://github.com/owner/repo/pull/%31",
	} {
		if err := validatePullURL(value, "owner/repo"); err == nil {
			t.Errorf("validatePullURL accepted %q", value)
		}
	}
	if err := validatePullURL("https://github.com/owner/repo/pull/123", "owner/repo"); err != nil {
		t.Fatalf("validatePullURL rejected canonical URL: %v", err)
	}
}

func TestDecodeAPIJSONRejectsDuplicateKeysAndInvalidUTF8(t *testing.T) {
	t.Parallel()
	invalidUTF8 := append([]byte(`{"id":"`), 0xff)
	invalidUTF8 = append(invalidUTF8, []byte(`"}`)...)
	for _, value := range []string{
		`{"id":1,"id":2}`,
		`{"id":1,"ID":2}`,
		string(invalidUTF8),
	} {
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

func installFakeGitHubCommands(t *testing.T, directory, remote, pull, log string, losePush, loseCreate, failLookup, conflictingCreate, gitlinkCommit bool) {
	t.Helper()
	git := fmt.Sprintf(`#!/bin/sh
printf 'git %%s\n' "$*" >> %[1]q
case " $* " in
  *" config --local --null --list "*) exit 0 ;;
  *" remote get-url origin "*) printf 'https://github.com/owner/repo.git\n'; exit 0 ;;
	*" ls-files --stage "*) exit 0 ;;
	*" status --porcelain=v1 "*) exit 0 ;;
  *" cat-file "*) exit 0 ;;
	*" fetch "*) exit 0 ;;
	*" rev-parse --verify FETCH_HEAD"*) printf 'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\n'; exit 0 ;;
	*" rev-parse --verify HEAD"*) printf 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n'; exit 0 ;;
	*" merge-base --is-ancestor "*) exit 0 ;;
	*" diff --name-only "*) exit 0 ;;
  *" ls-tree "*)
    test %[4]t = true && printf '160000 commit bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\tvendor/sub\000'
    exit 0 ;;
  *" ls-remote "*)
    if test -f %[2]q; then printf '%%s\trefs/heads/fern/demo/operation\n' "$(/bin/cat %[2]q)"; fi
    exit 0 ;;
  *" push "*)
    printf 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' > %[2]q
    test %[3]t = true && exit 1
    exit 0 ;;
esac
exit 1
`, log, remote, losePush, gitlinkCommit)
	gh := fmt.Sprintf(`#!/bin/sh
printf 'gh %%s\n' "$*" >> %[1]q
case " $* " in
  *" auth token "*) printf 'abcdefghijklmnopqrstuvwxyz012345\n'; exit 0 ;;
  *" api --hostname github.com --method GET repositories/123 "*)
	printf '{"id":123,"full_name":"owner/repo","name":"repo","default_branch":"main","owner":{"login":"owner"}}\n'
	exit 0 ;;
  *" api --hostname github.com --method GET "*"pulls?"*)
    test %[3]t = true && exit 1
	if test -f %[2]q; then printf '['; /bin/cat %[2]q; printf ']\n'; else printf '[]\n'; fi
    exit 0 ;;
  *" api --hostname github.com --method GET "*"pulls/"*)
	/bin/cat %[2]q
	exit 0 ;;
  *" api --hostname github.com --method POST "*)
    if test %[5]t = true; then
	  printf '{"number":1,"html_url":"https://github.com/owner/repo/pull/1","state":"closed","draft":false,"base":{"ref":"main","sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","repo":{"id":123,"full_name":"owner/repo","name":"repo","owner":{"login":"owner"}}},"head":{"ref":"fern/demo/operation","sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","repo":{"id":123,"full_name":"owner/repo","name":"repo","owner":{"login":"owner"}}}}' > %[2]q
    else
	  printf '{"number":1,"html_url":"https://github.com/owner/repo/pull/1","state":"open","draft":true,"base":{"ref":"main","sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","repo":{"id":123,"full_name":"owner/repo","name":"repo","owner":{"login":"owner"}}},"head":{"ref":"fern/demo/operation","sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","repo":{"id":123,"full_name":"owner/repo","name":"repo","owner":{"login":"owner"}}}}' > %[2]q
    fi
    test %[4]t = true && exit 1
	/bin/cat %[2]q
    exit 0 ;;
esac
exit 1
`, log, pull, failLookup, loseCreate, conflictingCreate)
	for name, script := range map[string]string{"git": git, "gh": gh} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", directory)
}
