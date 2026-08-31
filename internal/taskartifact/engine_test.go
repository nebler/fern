package taskartifact

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/nebler/fern/internal/task"
)

const (
	testWorkspace = task.WorkspaceID("wsp_0198d34d-6a50-75fb-b1f2-000000000001")
	testTask      = task.TaskID("tsk_0198d34d-6a50-75fb-b1f2-000000000002")
	testAttempt   = task.AttemptID("att_0198d34d-6a50-75fb-b1f2-000000000003")
)

func TestChangedSnapshotDeterministicAndMaterializable(t *testing.T) {
	engine, repository, base := testEngineRepository(t)
	beforeHead := gitCommand(t, repository, "rev-parse", "HEAD")

	writeFile(t, filepath.Join(repository, "modified"), []byte("staged\n"), 0o644)
	gitRun(t, repository, "add", "modified")
	writeFile(t, filepath.Join(repository, "modified"), []byte("final\n"), 0o644)
	if err := os.Remove(filepath.Join(repository, "deleted")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(repository, "mode"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(repository, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("new-target", filepath.Join(repository, "link")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(repository, "untracked"), []byte("new\n"), 0o644)
	writeFile(t, filepath.Join(repository, "ignored"), []byte("not retained\n"), 0o644)
	nonUTF8 := string([]byte{'r', 'a', 'w', '-', 0xff})
	expectedChanges := 5
	if runtime.GOOS == "linux" {
		writeFile(t, filepath.Join(repository, nonUTF8), []byte("bytes\n"), 0o644)
		expectedChanges++
	}
	beforeIndex, err := os.ReadFile(filepath.Join(repository, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	beforeStatus := gitCommand(t, repository, "status", "--porcelain=v1", "-z", "--untracked-files=all")

	source := mustSource(t, repository)
	spec := SnapshotSpec{Source: source, Base: base, EpochSecond: 1_700_000_000}
	first, firstStage, err := engine.Snapshot(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	second, secondStage, err := engine.Snapshot(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if first.ManifestSHA256 != second.ManifestSHA256 || first.BundleSHA256 != second.BundleSHA256 || first.Result != second.Result || first.Tree != second.Tree {
		t.Fatalf("snapshots differ:\n%+v\n%+v", first, second)
	}
	if first.Result == base || len(first.Changes) != expectedChanges {
		t.Fatalf("unexpected changed snapshot: %+v", first)
	}
	paths := decodedPaths(t, first.Changes)
	if runtime.GOOS == "linux" && !bytes.Equal(paths[len(paths)-1], []byte(nonUTF8)) {
		t.Fatalf("raw-byte path missing: %q", paths)
	}
	if got := gitCommand(t, repository, "rev-parse", "HEAD"); got != beforeHead {
		t.Fatalf("HEAD changed: %q != %q", got, beforeHead)
	}
	afterIndex, err := os.ReadFile(filepath.Join(repository, ".git", "index"))
	if err != nil || !bytes.Equal(beforeIndex, afterIndex) {
		t.Fatal("source index changed")
	}
	if afterStatus := gitCommand(t, repository, "status", "--porcelain=v1", "-z", "--untracked-files=all"); afterStatus != beforeStatus {
		t.Fatalf("source worktree status changed: %q != %q", afterStatus, beforeStatus)
	}

	locator, err := engine.Store(context.Background(), firstStage)
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := engine.Store(context.Background(), secondStage)
	if err != nil || duplicate != locator {
		t.Fatalf("dedupe failed: %v, %v", duplicate, err)
	}
	if parsed, err := ParseLocator(locator.String()); err != nil || parsed != locator {
		t.Fatalf("locator round trip failed: %v", err)
	}
	if err := os.RemoveAll(repository); err != nil {
		t.Fatal(err)
	}
	inspected, err := engine.Inspect(context.Background(), locator)
	if err != nil || inspected.ManifestSHA256 != first.ManifestSHA256 {
		t.Fatalf("inspect without source failed: %+v, %v", inspected, err)
	}
	left, err := engine.Materialize(context.Background(), locator)
	if err != nil {
		t.Fatal(err)
	}
	right, err := engine.Materialize(context.Background(), locator)
	if err != nil {
		t.Fatal(err)
	}
	if left.Path() == right.Path() {
		t.Fatal("materializations alias")
	}
	content, err := os.ReadFile(filepath.Join(left.Path(), "modified"))
	if err != nil || string(content) != "final\n" {
		t.Fatalf("wrong materialized content: %q, %v", content, err)
	}
	if mode, err := os.Stat(filepath.Join(left.Path(), "mode")); err != nil || mode.Mode().Perm()&0o111 == 0 {
		t.Fatal("executable mode was not materialized")
	}
	if target, err := os.Readlink(filepath.Join(left.Path(), "link")); err != nil || target != "new-target" {
		t.Fatalf("symlink was not materialized: %q, %v", target, err)
	}
	if _, err := os.Stat(filepath.Join(left.Path(), "deleted")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("deleted path was materialized")
	}
	if err := left.Close(); err != nil || left.Path() != "" {
		t.Fatalf("close failed: %v", err)
	}
	if err := right.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestNoChangeSnapshotReturnsBase(t *testing.T) {
	engine, repository, base := testEngineRepository(t)
	snapshot, staged, err := engine.Snapshot(context.Background(), SnapshotSpec{Source: mustSource(t, repository), Base: base, EpochSecond: 42})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Result != base || len(snapshot.Changes) != 0 {
		t.Fatalf("unexpected no-change result: %+v", snapshot)
	}
	locator, err := engine.Store(context.Background(), staged)
	if err != nil {
		t.Fatal(err)
	}
	checkout, err := engine.Materialize(context.Background(), locator)
	if err != nil {
		t.Fatal(err)
	}
	defer checkout.Close()
	if got := gitCommand(t, checkout.Path(), "rev-parse", "HEAD"); strings.TrimSpace(got) != string(base) {
		t.Fatalf("wrong detached result: %q", got)
	}
}

func TestDiscardStagedArtifact(t *testing.T) {
	engine, repository, base := testEngineRepository(t)
	_, staged, err := engine.Snapshot(context.Background(), SnapshotSpec{Source: mustSource(t, repository), Base: base, EpochSecond: 43})
	if err != nil {
		t.Fatal(err)
	}
	path := staged.path
	if err := engine.Discard(staged); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staged artifact remains after discard: %v", err)
	}
	if err := engine.Discard(staged); err != nil {
		t.Fatalf("discard is not idempotent: %v", err)
	}
	if _, err := engine.Store(context.Background(), staged); err == nil {
		t.Fatal("discarded staged capability was accepted")
	}
}

func TestUnsafeRepositoryStructuresRejected(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, string)
	}{
		{"filter config", func(t *testing.T, repository string) { gitRun(t, repository, "config", "filter.bad.clean", "cat") }},
		{"external diff", func(t *testing.T, repository string) { gitRun(t, repository, "config", "diff.bad.command", "cat") }},
		{"shared", func(t *testing.T, repository string) {
			gitRun(t, repository, "config", "core.sharedRepository", "group")
		}},
		{"transform attribute", func(t *testing.T, repository string) {
			writeFile(t, filepath.Join(repository, ".gitattributes"), []byte("* text\n"), 0o644)
		}},
		{"hook", func(t *testing.T, repository string) {
			writeFile(t, filepath.Join(repository, ".git", "hooks", "pre-commit"), []byte("exit 0\n"), 0o700)
		}},
		{"shallow", func(t *testing.T, repository string) {
			writeFile(t, filepath.Join(repository, ".git", "shallow"), []byte(strings.Repeat("0", 40)+"\n"), 0o600)
		}},
		{"alternates", func(t *testing.T, repository string) {
			writeFile(t, filepath.Join(repository, ".git", "objects", "info", "alternates"), []byte("/tmp\n"), 0o600)
		}},
		{"graft", func(t *testing.T, repository string) {
			writeFile(t, filepath.Join(repository, ".git", "info", "grafts"), []byte(strings.Repeat("0", 40)+"\n"), 0o600)
		}},
		{"promisor", func(t *testing.T, repository string) {
			gitRun(t, repository, "config", "remote.origin.promisor", "true")
		}},
		{"replace ref", func(t *testing.T, repository string) {
			base := strings.TrimSpace(gitCommand(t, repository, "rev-parse", "HEAD"))
			gitRun(t, repository, "replace", base, base)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			engine, repository, base := testEngineRepository(t)
			test.mutate(t, repository)
			_, _, err := engine.Snapshot(context.Background(), SnapshotSpec{Source: mustSource(t, repository), Base: base, EpochSecond: 1})
			if !errors.Is(err, ErrUnsafeSource) && !errors.Is(err, ErrGitFailed) {
				t.Fatalf("unsafe repository accepted: %v", err)
			}
		})
	}
}

func TestSubmoduleMetadataAndGitlinksRejected(t *testing.T) {
	t.Run("gitmodules", func(t *testing.T) {
		engine, repository, _ := testEngineRepository(t)
		writeFile(t, filepath.Join(repository, ".gitmodules"), []byte("[submodule \"x\"]\n\tpath = x\n\turl = x\n"), 0o644)
		gitRun(t, repository, "add", ".gitmodules")
		gitRun(t, repository, "commit", "-m", "gitmodules")
		base, err := task.ParseGitOID(strings.TrimSpace(gitCommand(t, repository, "rev-parse", "HEAD")))
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = engine.Snapshot(context.Background(), SnapshotSpec{Source: mustSource(t, repository), Base: base, EpochSecond: 1})
		if !errors.Is(err, ErrUnsafeSource) {
			t.Fatalf(".gitmodules accepted: %v", err)
		}
	})

	t.Run("gitlink", func(t *testing.T) {
		engine, repository, parent := testEngineRepository(t)
		gitRun(t, repository, "update-index", "--add", "--cacheinfo", "160000,"+string(parent)+",nested")
		gitRun(t, repository, "commit", "-m", "gitlink")
		base, err := task.ParseGitOID(strings.TrimSpace(gitCommand(t, repository, "rev-parse", "HEAD")))
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = engine.Snapshot(context.Background(), SnapshotSpec{Source: mustSource(t, repository), Base: base, EpochSecond: 1})
		if !errors.Is(err, ErrUnsafeSource) {
			t.Fatalf("gitlink accepted: %v", err)
		}
	})
}

func TestLinkedWorktreeRejected(t *testing.T) {
	engine, repository, base := testEngineRepository(t)
	linked := filepath.Join(filepath.Dir(repository), "linked")
	gitRun(t, repository, "worktree", "add", "--detach", linked, string(base))
	linked, err := filepath.EvalSymlinks(linked)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = engine.Snapshot(context.Background(), SnapshotSpec{Source: mustSource(t, linked), Base: base, EpochSecond: 1})
	if !errors.Is(err, ErrUnsafeSource) {
		t.Fatalf("linked worktree accepted: %v", err)
	}
}

func TestBundleAndCASAttacksRejected(t *testing.T) {
	t.Run("truncated staged bundle", func(t *testing.T) {
		engine, repository, base := testEngineRepository(t)
		writeFile(t, filepath.Join(repository, "new"), []byte("new"), 0o644)
		_, staged, err := engine.Snapshot(context.Background(), SnapshotSpec{Source: mustSource(t, repository), Base: base, EpochSecond: 2})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Truncate(filepath.Join(staged.path, bundleName), 16); err != nil {
			t.Fatal(err)
		}
		if _, err := engine.Store(context.Background(), staged); err == nil {
			t.Fatal("truncated bundle accepted")
		}
	})

	t.Run("CAS symlink collision", func(t *testing.T) {
		engine, repository, base := testEngineRepository(t)
		_, staged, err := engine.Snapshot(context.Background(), SnapshotSpec{Source: mustSource(t, repository), Base: base, EpochSecond: 3})
		if err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(engine.casRoot, staged.digest.String())
		if err := os.Symlink(staged.path, target); err != nil {
			t.Fatal(err)
		}
		if _, err := engine.Store(context.Background(), staged); err == nil {
			t.Fatal("CAS symlink collision accepted")
		}
	})

	t.Run("stored mode", func(t *testing.T) {
		engine, repository, base := testEngineRepository(t)
		_, staged, err := engine.Snapshot(context.Background(), SnapshotSpec{Source: mustSource(t, repository), Base: base, EpochSecond: 4})
		if err != nil {
			t.Fatal(err)
		}
		locator, err := engine.Store(context.Background(), staged)
		if err != nil {
			t.Fatal(err)
		}
		object := filepath.Join(engine.casRoot, locator.digest.String())
		if err := os.Chmod(filepath.Join(object, manifestName), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := engine.Inspect(context.Background(), locator); err == nil {
			t.Fatal("unsafe manifest mode accepted")
		}
	})

	t.Run("stored hardlink", func(t *testing.T) {
		engine, repository, base := testEngineRepository(t)
		_, staged, err := engine.Snapshot(context.Background(), SnapshotSpec{Source: mustSource(t, repository), Base: base, EpochSecond: 7})
		if err != nil {
			t.Fatal(err)
		}
		locator, err := engine.Store(context.Background(), staged)
		if err != nil {
			t.Fatal(err)
		}
		bundle := filepath.Join(engine.casRoot, locator.digest.String(), bundleName)
		if err := os.Link(bundle, filepath.Join(engine.casRoot, "hostile-link")); err != nil {
			t.Fatal(err)
		}
		if _, err := engine.Inspect(context.Background(), locator); err == nil {
			t.Fatal("hard-linked bundle accepted")
		}
	})

	t.Run("regular collision", func(t *testing.T) {
		engine, repository, base := testEngineRepository(t)
		_, staged, err := engine.Snapshot(context.Background(), SnapshotSpec{Source: mustSource(t, repository), Base: base, EpochSecond: 8})
		if err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(engine.casRoot, staged.digest.String())
		if err := os.Mkdir(target, 0o700); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(target, manifestName), []byte("hostile\n"), 0o600)
		writeFile(t, filepath.Join(target, bundleName), []byte("hostile\n"), 0o600)
		if _, err := engine.Store(context.Background(), staged); err == nil {
			t.Fatal("regular CAS collision accepted")
		}
	})
}

func TestIndependentVerificationAfterSourceDeletion(t *testing.T) {
	engine, repository, base := testEngineRepository(t)
	writeFile(t, filepath.Join(repository, "new"), []byte("new"), 0o644)
	_, staged, err := engine.Snapshot(context.Background(), SnapshotSpec{Source: mustSource(t, repository), Base: base, EpochSecond: 5})
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := os.ReadFile(filepath.Join(staged.path, manifestName))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(repository); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.verifyArtifact(context.Background(), manifest, filepath.Join(staged.path, bundleName), staged.digest); err != nil {
		t.Fatalf("verification consulted source: %v", err)
	}
}

func TestConfigurationAndCheckoutIdentity(t *testing.T) {
	engine, repository, base := testEngineRepository(t)
	symlink := filepath.Join(filepath.Dir(engine.casRoot), "cas-link")
	if err := os.Symlink(engine.casRoot, symlink); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Config{GitExecutable: engine.gitExecutable, CASRoot: symlink, WorkRoot: engine.workRoot}); err == nil {
		t.Fatal("symlink root accepted")
	}
	_, staged, err := engine.Snapshot(context.Background(), SnapshotSpec{Source: mustSource(t, repository), Base: base, EpochSecond: 6})
	if err != nil {
		t.Fatal(err)
	}
	locator, err := engine.Store(context.Background(), staged)
	if err != nil {
		t.Fatal(err)
	}
	checkout, err := engine.Materialize(context.Background(), locator)
	if err != nil {
		t.Fatal(err)
	}
	path := checkout.Path()
	if err := os.Remove(checkoutMarkerPath(path)); err != nil {
		t.Fatal(err)
	}
	if err := checkout.Close(); err == nil {
		t.Fatal("checkout marker attack accepted")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("attacked checkout was removed")
	}
	_ = os.RemoveAll(path)
}

func testEngineRepository(t *testing.T) (*Engine, string, task.GitOID) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	casRoot, workRoot, repository := filepath.Join(root, "cas"), filepath.Join(root, "work"), filepath.Join(root, "repository")
	for _, path := range []string{casRoot, workRoot, repository} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git unavailable")
	}
	gitPath, err = filepath.Abs(gitPath)
	if err != nil {
		t.Fatal(err)
	}
	gitPath, err = filepath.EvalSymlinks(gitPath)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := New(Config{GitExecutable: gitPath, CASRoot: casRoot, WorkRoot: workRoot, BundleBytes: 16 << 20, BlobBytes: 4 << 20})
	if err != nil {
		t.Fatal(err)
	}
	gitRun(t, repository, "init")
	gitRun(t, repository, "config", "user.name", "Fixture")
	gitRun(t, repository, "config", "user.email", "fixture@example.invalid")
	writeFile(t, filepath.Join(repository, "modified"), []byte("base\n"), 0o644)
	writeFile(t, filepath.Join(repository, "deleted"), []byte("delete\n"), 0o644)
	writeFile(t, filepath.Join(repository, "mode"), []byte("mode\n"), 0o644)
	writeFile(t, filepath.Join(repository, ".gitignore"), []byte("ignored\n"), 0o644)
	if runtime.GOOS == "windows" {
		t.Skip("symlink repository fixture requires Unix")
	}
	if err := os.Symlink("old-target", filepath.Join(repository, "link")); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repository, "add", "--all")
	gitRun(t, repository, "commit", "-m", "base")
	base, err := task.ParseGitOID(strings.TrimSpace(gitCommand(t, repository, "rev-parse", "HEAD")))
	if err != nil {
		t.Fatal(err)
	}
	return engine, repository, base
}

func mustSource(t *testing.T, repository string) Source {
	t.Helper()
	source, err := NewSource(repository, testWorkspace, testTask, testAttempt)
	if err != nil {
		t.Fatal(err)
	}
	return source
}

func gitRun(t *testing.T, repository string, arguments ...string) {
	t.Helper()
	_ = gitCommand(t, repository, arguments...)
}

func gitCommand(t *testing.T, repository string, arguments ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", repository}, arguments...)...)
	command.Env = append(os.Environ(), "LC_ALL=C")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", arguments, err, output)
	}
	return string(output)
}

func writeFile(t *testing.T, path string, content []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func decodedPaths(t *testing.T, entries []ChangeEntry) [][]byte {
	t.Helper()
	paths := make([][]byte, len(entries))
	for index, entry := range entries {
		decoded, err := base64.StdEncoding.DecodeString(entry.PathBase64)
		if err != nil {
			t.Fatal(err)
		}
		paths[index] = decoded
	}
	if !sort.SliceIsSorted(paths, func(i, j int) bool { return bytes.Compare(paths[i], paths[j]) < 0 }) {
		t.Fatal("manifest paths are not raw-byte sorted")
	}
	return paths
}
