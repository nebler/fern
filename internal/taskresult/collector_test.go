package taskresult

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/nebler/fern/internal/task"
)

const (
	testSession = task.OpenCodeSessionID("ses_0123456789abcdef0123456789abcdef")
	testMessage = task.OpenCodeMessageID("msg_0123456789abcdef0123456789abcdef")
)

type fixture struct {
	git       string
	repo      string
	base      task.GitOID
	collector *Collector
}

func TestCollectNoChangesAndDoesNotMutate(t *testing.T) {
	fixture := newFixture(t)
	request := fixture.request()
	beforeHead := gitOutput(t, fixture.git, fixture.repo, "rev-parse", "HEAD")
	indexPath := filepath.Join(fixture.repo, ".git", "index")
	beforeIndex, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}

	result, err := fixture.collector.Collect(context.Background(), request)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if result.Tuple.Outcome != task.ResultNoChanges || result.Tuple.ResultCommit != fixture.base || result.Tuple.ManifestEntries != 0 || !result.Tuple.WorktreeClean {
		t.Fatalf("unexpected tuple: %+v", result.Tuple)
	}
	if len(result.Manifest) != 0 || result.ManifestSHA256 != sha256.Sum256([]byte("[]")) {
		t.Fatalf("unexpected empty manifest: %#v %x", result.Manifest, result.ManifestSHA256)
	}
	if result.OpenCodeSessionID != testSession || result.OpenCodeMessageID != testMessage || result.PolicyVersion != "result-v1" {
		t.Fatalf("identity was not preserved: %+v", result)
	}
	if result.CollectedAt.Nanosecond()%int(time.Millisecond) != 0 || result.EvidenceSHA256 != request.EvidenceSHA256 || !bytes.Equal(result.EvidencePayload, request.EvidencePayload) {
		t.Fatalf("evidence or timestamp mismatch: %+v", result)
	}
	result.EvidencePayload[0] = '['
	if request.EvidencePayload[0] != '{' {
		t.Fatal("result aliases request evidence")
	}
	afterIndex, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if beforeHead != gitOutput(t, fixture.git, fixture.repo, "rev-parse", "HEAD") || !bytes.Equal(beforeIndex, afterIndex) {
		t.Fatal("collector mutated HEAD or index")
	}
}

func TestCollectChangedManifestCanonical(t *testing.T) {
	fixture := newFixture(t)
	if err := os.WriteFile(filepath.Join(fixture.repo, "modified.txt"), []byte{0, 1, 0xff, 3}, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(fixture.repo, "deleted.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.repo, "added.bin"), []byte{0xff, 0, 0xfe}, 0755); err != nil {
		t.Fatal(err)
	}
	runGit(t, fixture.git, fixture.repo, "add", "-A")
	runGit(t, fixture.git, fixture.repo, "commit", "-qm", "result")
	resultCommit := task.GitOID(gitOutput(t, fixture.git, fixture.repo, "rev-parse", "HEAD"))

	result, err := fixture.collector.Collect(context.Background(), fixture.request())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if result.Tuple.Outcome != task.ResultChanged || result.Tuple.ResultCommit != resultCommit || result.Tuple.ManifestEntries != 3 {
		t.Fatalf("unexpected tuple: %+v", result.Tuple)
	}
	wantPaths := []string{"added.bin", "deleted.txt", "modified.txt"}
	wantKinds := []string{"added", "deleted", "modified"}
	wantOldOIDs := []string{"", gitOutput(t, fixture.git, fixture.repo, "rev-parse", string(fixture.base)+":deleted.txt"), gitOutput(t, fixture.git, fixture.repo, "rev-parse", string(fixture.base)+":modified.txt")}
	wantNewOIDs := []string{gitOutput(t, fixture.git, fixture.repo, "rev-parse", string(resultCommit)+":added.bin"), "", gitOutput(t, fixture.git, fixture.repo, "rev-parse", string(resultCommit)+":modified.txt")}
	for index, entry := range result.Manifest {
		path, err := base64.StdEncoding.DecodeString(entry.PathBase64)
		if err != nil || string(path) != wantPaths[index] || entry.ChangeKind != wantKinds[index] {
			t.Fatalf("entry %d: %#v path=%q err=%v", index, entry, path, err)
		}
		if entry.ChangeKind == "added" && (entry.OldMode != nil || entry.NewSize == nil || *entry.NewSize != 3) {
			t.Fatalf("bad added entry: %#v", entry)
		}
		if entry.ChangeKind == "deleted" && (entry.NewMode != nil || entry.OldSize == nil) {
			t.Fatalf("bad deleted entry: %#v", entry)
		}
		if entry.ChangeKind == "modified" && (entry.OldSize == nil || *entry.OldSize != int64(len("base modified\n")) || entry.NewSize == nil || *entry.NewSize != 4) {
			t.Fatalf("bad modified entry: %#v", entry)
		}
		if (entry.OldBlobOID == nil) != (wantOldOIDs[index] == "") || entry.OldBlobOID != nil && *entry.OldBlobOID != wantOldOIDs[index] {
			t.Fatalf("entry %d old OID mismatch: %#v", index, entry)
		}
		if (entry.NewBlobOID == nil) != (wantNewOIDs[index] == "") || entry.NewBlobOID != nil && *entry.NewBlobOID != wantNewOIDs[index] {
			t.Fatalf("entry %d new OID mismatch: %#v", index, entry)
		}
	}
	encoded, err := json.Marshal(result.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	if result.ManifestSHA256 != sha256.Sum256(encoded) {
		t.Fatal("manifest digest is not taskstore canonical JSON digest")
	}
	tree := task.GitOID(gitOutput(t, fixture.git, fixture.repo, "rev-parse", "HEAD^{tree}"))
	if result.TreeOID != tree {
		t.Fatalf("tree=%s want %s", result.TreeOID, tree)
	}
}

func TestCollectRepresentsRenameAsDeleteAdd(t *testing.T) {
	fixture := newFixture(t)
	runGit(t, fixture.git, fixture.repo, "mv", "modified.txt", "renamed.txt")
	runGit(t, fixture.git, fixture.repo, "commit", "-qm", "rename")
	result, err := fixture.collector.Collect(context.Background(), fixture.request())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(result.Manifest) != 2 || result.Manifest[0].ChangeKind != "deleted" || result.Manifest[1].ChangeKind != "added" {
		t.Fatalf("rename was not delete/add: %#v", result.Manifest)
	}
}

func TestCollectRecursesIntoNestedTrees(t *testing.T) {
	fixture := newFixture(t)
	writeFile(t, filepath.Join(fixture.repo, "nested", "deeper", "result.txt"), []byte("nested result\n"))
	runGit(t, fixture.git, fixture.repo, "add", "nested/deeper/result.txt")
	runGit(t, fixture.git, fixture.repo, "commit", "-qm", "nested result")
	result, err := fixture.collector.Collect(context.Background(), fixture.request())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(result.Manifest) != 1 {
		t.Fatalf("manifest = %#v", result.Manifest)
	}
	path, err := base64.StdEncoding.DecodeString(result.Manifest[0].PathBase64)
	if err != nil || string(path) != "nested/deeper/result.txt" {
		t.Fatalf("nested path = %q, %v", path, err)
	}
}

func TestCollectNonUTF8Path(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows filenames do not preserve arbitrary path bytes")
	}
	fixture := newFixture(t)
	pathBytes := []byte{'z', '-', 0xff, '.', 'b', 'i', 'n'}
	if err := os.WriteFile(filepath.Join(fixture.repo, string(pathBytes)), []byte("x"), 0600); err != nil {
		t.Skipf("filesystem does not permit non-UTF8 path: %v", err)
	}
	runGit(t, fixture.git, fixture.repo, "add", "--", string(pathBytes))
	runGit(t, fixture.git, fixture.repo, "commit", "-qm", "non utf8")
	result, err := fixture.collector.Collect(context.Background(), fixture.request())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	found := false
	for _, entry := range result.Manifest {
		decoded, _ := base64.StdEncoding.DecodeString(entry.PathBase64)
		if bytes.Equal(decoded, pathBytes) {
			found = true
		}
	}
	if !found {
		t.Fatalf("non-UTF8 path absent: %#v", result.Manifest)
	}
}

func TestCollectRejectsDirtyAndUntracked(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, fixture)
	}{
		{"tracked", func(t *testing.T, f fixture) {
			if err := os.WriteFile(filepath.Join(f.repo, "modified.txt"), []byte("dirty"), 0600); err != nil {
				t.Fatal(err)
			}
		}},
		{"untracked", func(t *testing.T, f fixture) {
			if err := os.WriteFile(filepath.Join(f.repo, "untracked.txt"), []byte("dirty"), 0600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newFixture(t)
			test.mutate(t, fixture)
			if _, err := fixture.collector.Collect(context.Background(), fixture.request()); !errors.Is(err, ErrRepositoryProof) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestCollectRejectsNonAncestor(t *testing.T) {
	fixture := newFixture(t)
	runGit(t, fixture.git, fixture.repo, "checkout", "--orphan", "unrelated")
	runGit(t, fixture.git, fixture.repo, "rm", "-q", "-rf", ".")
	if err := os.WriteFile(filepath.Join(fixture.repo, "other.txt"), []byte("other"), 0600); err != nil {
		t.Fatal(err)
	}
	runGit(t, fixture.git, fixture.repo, "add", "other.txt")
	runGit(t, fixture.git, fixture.repo, "commit", "-qm", "unrelated")
	if _, err := fixture.collector.Collect(context.Background(), fixture.request()); !errors.Is(err, ErrRepositoryProof) {
		t.Fatalf("err=%v", err)
	}
}

func TestCollectRejectsSymlinkGitDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink setup is platform-dependent")
	}
	fixture := newFixture(t)
	realGit := filepath.Join(filepath.Dir(fixture.repo), "git-data")
	if err := os.Rename(filepath.Join(fixture.repo, ".git"), realGit); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realGit, filepath.Join(fixture.repo, ".git")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := fixture.collector.Collect(context.Background(), fixture.request()); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("err=%v", err)
	}
}

func TestCollectorRejectsUnsafeGitExecutable(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	writable := filepath.Join(directory, "writable-git")
	if err := os.WriteFile(writable, []byte("#!/bin/sh\nexit 0\n"), 0777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(writable, 0777); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Config{GitExecutable: writable}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("writable executable error = %v", err)
	}
	if runtime.GOOS == "windows" {
		return
	}
	target := filepath.Join(directory, "target-git")
	writeExecutable(t, target, "#!/bin/sh\nexit 0\n")
	link := filepath.Join(directory, "linked-git")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := New(Config{GitExecutable: link}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("symlink executable error = %v", err)
	}
}

func TestCollectorRejectsReplacedGitExecutable(t *testing.T) {
	fixture := newFixture(t)
	directory := t.TempDir()
	executable := filepath.Join(directory, "git")
	body := fmt.Sprintf("#!/bin/sh\nexec %q \"$@\"\n", fixture.git)
	writeExecutable(t, executable, body)
	collector, err := New(Config{GitExecutable: executable})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(executable, executable+".old"); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, executable, body)
	if _, err := collector.Collect(context.Background(), fixture.request()); !errors.Is(err, ErrRepositoryProof) {
		t.Fatalf("replaced executable error = %v", err)
	}
}

func TestCollectRejectsRepositorySubdirectory(t *testing.T) {
	fixture := newFixture(t)
	subdirectory := filepath.Join(fixture.repo, "subdirectory")
	if err := os.Mkdir(subdirectory, 0700); err != nil {
		t.Fatal(err)
	}
	request := fixture.request()
	request.RepositoryPath = subdirectory
	if _, err := fixture.collector.Collect(context.Background(), request); !errors.Is(err, ErrInvalidRequest) && !errors.Is(err, ErrRepositoryProof) {
		t.Fatalf("err=%v", err)
	}
}

func TestCollectRejectsUnsafeRepositoryFeatures(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, fixture)
	}{
		{"config", func(t *testing.T, f fixture) { runGit(t, f.git, f.repo, "config", "core.fsmonitor", "true") }},
		{"alternates", func(t *testing.T, f fixture) {
			writeFile(t, filepath.Join(f.repo, ".git", "objects", "info", "alternates"), []byte("/tmp/objects\n"))
		}},
		{"grafts", func(t *testing.T, f fixture) {
			writeFile(t, filepath.Join(f.repo, ".git", "info", "grafts"), []byte(f.base+"\n"))
		}},
		{"shallow", func(t *testing.T, f fixture) {
			writeFile(t, filepath.Join(f.repo, ".git", "shallow"), []byte(f.base+"\n"))
		}},
		{"replace", func(t *testing.T, f fixture) {
			result := commitResult(t, f)
			runGit(t, f.git, f.repo, "replace", string(f.base), string(result))
		}},
		{"gitmodules", func(t *testing.T, f fixture) {
			writeFile(t, filepath.Join(f.repo, ".gitmodules"), []byte("[submodule \"x\"]\n\tpath=x\n\turl=x\n"))
			runGit(t, f.git, f.repo, "add", ".gitmodules")
			runGit(t, f.git, f.repo, "commit", "-qm", "gitmodules")
		}},
		{"gitlink", func(t *testing.T, f fixture) {
			runGit(t, f.git, f.repo, "update-index", "--add", "--cacheinfo", "160000,"+string(f.base)+",nested")
			runGit(t, f.git, f.repo, "commit", "-qm", "gitlink")
		}},
		{"intermediate gitlink", func(t *testing.T, f fixture) {
			runGit(t, f.git, f.repo, "update-index", "--add", "--cacheinfo", "160000,"+string(f.base)+",nested")
			runGit(t, f.git, f.repo, "commit", "-qm", "gitlink")
			runGit(t, f.git, f.repo, "rm", "-q", "--cached", "nested")
			runGit(t, f.git, f.repo, "commit", "-qm", "remove gitlink")
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newFixture(t)
			test.mutate(t, fixture)
			if _, err := fixture.collector.Collect(context.Background(), fixture.request()); !errors.Is(err, ErrRepositoryProof) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestCollectRejectsInvalidEvidence(t *testing.T) {
	fixture := newFixture(t)
	for _, payload := range []json.RawMessage{json.RawMessage(`[]`), json.RawMessage(`{"token":"secret"}`), json.RawMessage(`{"response_body":"secret"}`)} {
		request := fixture.request()
		request.EvidencePayload = payload
		request.EvidenceSHA256 = sha256.Sum256(payload)
		if _, err := fixture.collector.Collect(context.Background(), request); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("payload=%s err=%v", payload, err)
		}
	}
}

func TestCollectTimeoutAndOutputBounds(t *testing.T) {
	fixture := newFixture(t)
	for _, test := range []struct {
		name, body string
		timeout    time.Duration
		output     int
		want       error
	}{
		{"timeout", "sleep 30\n", 100 * time.Millisecond, 1024, ErrGitTimeout},
		{"output", "i=0; while [ $i -lt 4096 ]; do printf x; i=$((i+1)); done\n", 2 * time.Second, 128, ErrGitOutputLimit},
	} {
		t.Run(test.name, func(t *testing.T) {
			script := filepath.Join(t.TempDir(), "fake-git")
			writeExecutable(t, script, "#!/bin/sh\n"+test.body)
			collector, err := New(Config{GitExecutable: script, Timeout: test.timeout, OutputBytes: test.output})
			if err != nil {
				t.Fatal(err)
			}
			started := time.Now()
			_, err = collector.Collect(context.Background(), fixture.request())
			if !errors.Is(err, test.want) {
				t.Fatalf("err=%v want %v", err, test.want)
			}
			if time.Since(started) > 5*time.Second {
				t.Fatalf("bounded command took %s", time.Since(started))
			}
		})
	}
}

func TestCollectUsesExplicitEnvironmentAndHardenedGitOptions(t *testing.T) {
	fixture := newFixture(t)
	logPath := filepath.Join(t.TempDir(), "invocation")
	proxy := filepath.Join(t.TempDir(), "git-proxy")
	script := fmt.Sprintf("#!/bin/sh\n/usr/bin/env > %q\nprintf 'ARGS:%%s\\n' \"$*\" >> %q\nexec %q \"$@\"\n", logPath, logPath, fixture.git)
	writeExecutable(t, proxy, script)
	collector, err := New(Config{GitExecutable: proxy, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("AMBIENT_RESULT_SECRET", "must-not-leak")
	if _, err := collector.Collect(context.Background(), fixture.request()); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(log)
	for _, wanted := range []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_OPTIONAL_LOCKS=0", "GIT_PAGER=cat", "--no-replace-objects", "core.hooksPath=" + os.DevNull, "core.fsmonitor=false", "protocol.allow=never"} {
		if !strings.Contains(text, wanted) {
			t.Fatalf("missing %q in %s", wanted, text)
		}
	}
	if strings.Contains(text, "AMBIENT_RESULT_SECRET") {
		t.Fatal("ambient environment leaked")
	}
}

func TestCollectDetectsRaceBetweenProofPasses(t *testing.T) {
	fixture := newFixture(t)
	resultCommit := commitResult(t, fixture)
	countPath := filepath.Join(t.TempDir(), "count")
	proxy := filepath.Join(t.TempDir(), "racing-git")
	script := fmt.Sprintf(`#!/bin/sh
case " $* " in
  *" diff-tree "*)
    count=0
    [ ! -f %q ] || count=$(cat %q)
    count=$((count+1))
    printf '%%s' "$count" > %q
    if [ "$count" -eq 2 ]; then %q -C %q reset --hard -q %q || exit 90; fi
    ;;
esac
exec %q "$@"
`, countPath, countPath, countPath, fixture.git, fixture.repo, fixture.base, fixture.git)
	writeExecutable(t, proxy, script)
	collector, err := New(Config{GitExecutable: proxy, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := collector.Collect(context.Background(), fixture.request()); !errors.Is(err, ErrRepositoryProof) {
		t.Fatalf("result=%s err=%v", resultCommit, err)
	}
}

func TestCollectorConcurrentAndRepeated(t *testing.T) {
	fixture := newFixture(t)
	request := fixture.request()
	want, err := fixture.collector.Collect(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	errorsChannel := make(chan error, 8)
	for range 8 {
		go func() {
			got, err := fixture.collector.Collect(context.Background(), request)
			if err == nil && (got.Tuple != want.Tuple || got.ManifestSHA256 != want.ManifestSHA256 || !manifestsEqual(got.Manifest, want.Manifest)) {
				err = errors.New("inconsistent concurrent result")
			}
			errorsChannel <- err
		}()
	}
	for range 8 {
		if err := <-errorsChannel; err != nil {
			t.Error(err)
		}
	}
}

func TestRawDiffRejectsMoreThanManifestLimit(t *testing.T) {
	var output bytes.Buffer
	zero := strings.Repeat("0", 40)
	oid := strings.Repeat("1", 40)
	for index := 0; index <= MaxManifestFiles; index++ {
		fmt.Fprintf(&output, ":000000 100644 %s %s A%cpath-%05d%c", zero, oid, byte(0), index, byte(0))
	}
	if _, err := parseRawDiff(output.Bytes()); err == nil {
		t.Fatal("oversized manifest diff was accepted")
	}
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	git := findGit(t)
	repo := filepath.Join(t.TempDir(), "repository")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	repo, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	runGit(t, git, repo, "init", "--object-format=sha1", "-q")
	runGit(t, git, repo, "config", "user.name", "Task Result Test")
	runGit(t, git, repo, "config", "user.email", "task-result@example.invalid")
	writeFile(t, filepath.Join(repo, "modified.txt"), []byte("base modified\n"))
	writeFile(t, filepath.Join(repo, "deleted.txt"), []byte("base deleted\n"))
	runGit(t, git, repo, "add", ".")
	runGit(t, git, repo, "commit", "-qm", "base")
	base := task.GitOID(gitOutput(t, git, repo, "rev-parse", "HEAD"))
	collector, err := New(Config{GitExecutable: git, Timeout: 5 * time.Second, Now: func() time.Time { return time.Unix(123, 456789123) }})
	if err != nil {
		t.Fatal(err)
	}
	return fixture{git: git, repo: repo, base: base, collector: collector}
}

func (fixture fixture) request() Request {
	evidence := json.RawMessage(`{"scan":"complete","objects":2}`)
	return Request{RepositoryPath: fixture.repo, Repository: task.RepositoryTuple{RepositoryID: 42, BaseSHA: fixture.base}, OpenCodeSessionID: testSession, OpenCodeMessageID: testMessage, EvidencePayload: evidence, EvidenceSHA256: sha256.Sum256(evidence), PolicyVersion: "result-v1"}
}

func commitResult(t *testing.T, fixture fixture) task.GitOID {
	t.Helper()
	writeFile(t, filepath.Join(fixture.repo, "result.txt"), []byte("result\n"))
	runGit(t, fixture.git, fixture.repo, "add", "result.txt")
	runGit(t, fixture.git, fixture.repo, "commit", "-qm", "result")
	return task.GitOID(gitOutput(t, fixture.git, fixture.repo, "rev-parse", "HEAD"))
}

func findGit(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is required")
	}
	path, err = filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Clean(path)
}

func runGit(t *testing.T, git, repo string, arguments ...string) {
	t.Helper()
	command := exec.Command(git, append([]string{"-C", repo}, arguments...)...)
	command.Env = append(os.Environ(), "GIT_AUTHOR_DATE=2001-02-03T04:05:06Z", "GIT_COMMITTER_DATE=2001-02-03T04:05:06Z", "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", arguments, err, output)
	}
}

func gitOutput(t *testing.T, git, repo string, arguments ...string) string {
	t.Helper()
	command := exec.Command(git, append([]string{"-C", repo}, arguments...)...)
	command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	output, err := command.Output()
	if err != nil {
		t.Fatalf("git %v: %v", arguments, err)
	}
	return strings.TrimSpace(string(output))
}

func writeFile(t *testing.T, path string, value []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, value, 0600); err != nil {
		t.Fatal(err)
	}
}

func writeExecutable(t *testing.T, path, value string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(value), 0700); err != nil {
		t.Fatal(err)
	}
}
