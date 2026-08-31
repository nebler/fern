package taskartifact

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
	"sort"
	"strings"
	"testing"

	"github.com/nebler/fern/internal/task"
)

const (
	testWorkspace  = task.WorkspaceID("wsp_0198d34d-6a50-75fb-b1f2-000000000001")
	testTask       = task.TaskID("tsk_0198d34d-6a50-75fb-b1f2-000000000002")
	testAttempt    = task.AttemptID("att_0198d34d-6a50-75fb-b1f2-000000000003")
	testAttempt2   = task.AttemptID("att_0198d34d-6a50-75fb-b1f2-000000000004")
	testSeal       = task.SealRequestID("slr_0198d34d-6a50-75fb-b1f2-000000000005")
	testSession    = task.OpenCodeSessionID("ses_0123456789abcdef0123456789abcdef")
	testMessage    = task.OpenCodeMessageID("msg_fedcba9876543210fedcba9876543210")
	testWorkspace2 = task.WorkspaceID("wsp_0198d34d-6a50-75fb-b1f2-000000000011")
	testTask2      = task.TaskID("tsk_0198d34d-6a50-75fb-b1f2-000000000012")
	testSeal2      = task.SealRequestID("slr_0198d34d-6a50-75fb-b1f2-000000000015")
	testSession2   = task.OpenCodeSessionID("ses_11111111111111111111111111111111")
	testMessage2   = task.OpenCodeMessageID("msg_22222222222222222222222222222222")
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
	spec := testSnapshotSpec(t, source, base, 1_700_000_000)
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
	for _, name := range []string{manifestName, bundleName} {
		info, err := os.Lstat(filepath.Join(engine.casRoot, locator.digest.String(), name))
		if err != nil || info.Mode().Perm() != 0o400 {
			t.Fatalf("stored %s mode = %v, %v", name, info, err)
		}
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
	snapshot, staged, err := engine.Snapshot(context.Background(), testSnapshotSpec(t, mustSource(t, repository), base, 42))
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

func TestManifestBindsExecutionIdentityAndCanonicalChanges(t *testing.T) {
	engine, repository, base := testEngineRepository(t)
	spec := testSnapshotSpec(t, mustSource(t, repository), base, 44)
	snapshot, staged, err := engine.Snapshot(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Discard(staged)
	for _, name := range []string{manifestName, bundleName} {
		info, err := os.Lstat(filepath.Join(staged.path, name))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("staged %s mode = %v, %v", name, info, err)
		}
	}
	manifestBytes, err := os.ReadFile(filepath.Join(staged.path, manifestName))
	if err != nil {
		t.Fatal(err)
	}
	manifest, digest, err := decodeManifest(manifestBytes)
	if err != nil || digest != snapshot.ManifestSHA256 {
		t.Fatalf("decode manifest: %v", err)
	}
	if snapshot.RepositoryID != spec.RepositoryID || snapshot.WorkspaceID != spec.Source.WorkspaceID || snapshot.TaskID != spec.Source.TaskID ||
		snapshot.AttemptID != spec.Source.AttemptID || snapshot.Generation != spec.Generation || snapshot.SealRequestID != spec.SealRequestID ||
		snapshot.ImageIdentity != spec.ImageIdentity || snapshot.Profile != spec.Profile || snapshot.ProfileSHA256 != spec.ProfileSHA256 ||
		snapshot.EnvironmentSHA256 != spec.EnvironmentSHA256 || snapshot.ResourceSpecVersion != ResourceSpecVersion ||
		snapshot.OpenCodeSessionID != spec.OpenCodeSessionID || snapshot.OpenCodeMessageID != spec.OpenCodeMessageID ||
		snapshot.SnapshotPolicyVersion != SnapshotPolicyV1 || snapshot.CompletionAuthority != CompletionUserSeal {
		t.Fatalf("snapshot identity differs from spec: %+v", snapshot)
	}
	if manifest.RepositoryID != spec.RepositoryID || manifest.WorkspaceID != spec.Source.WorkspaceID || manifest.TaskID != spec.Source.TaskID ||
		manifest.AttemptID != spec.Source.AttemptID || manifest.Generation != spec.Generation || manifest.SealRequestID != spec.SealRequestID ||
		manifest.ImageIdentity != spec.ImageIdentity || manifest.Profile != spec.Profile || manifest.ProfileSHA256 != spec.ProfileSHA256 ||
		manifest.EnvironmentSHA256 != spec.EnvironmentSHA256 || manifest.ResourceSpecVersion != ResourceSpecVersion ||
		manifest.OpenCodeSessionID != spec.OpenCodeSessionID || manifest.OpenCodeMessageID != spec.OpenCodeMessageID ||
		manifest.SnapshotPolicyVersion != SnapshotPolicyV1 || manifest.CompletionAuthority != CompletionUserSeal {
		t.Fatalf("manifest identity differs from spec: %+v", manifest)
	}
	const emptyChangesSHA256 = "4f53cda18c2baa0c0354bb5f9a3ecbe5ed12ab4d8e11ba873c2f11161202b945"
	if snapshot.ChangesSHA256.String() != emptyChangesSHA256 || manifest.ChangesSHA256 != snapshot.ChangesSHA256 {
		t.Fatalf("changes digest = %s", snapshot.ChangesSHA256.String())
	}
	for _, key := range []string{"remote", "repository_remote", "path", "host_path", "prompt", "instruction", "env", "environment", "credential", "actor", "opencode_data"} {
		if bytes.Contains(manifestBytes, []byte(`"`+key+`"`)) {
			t.Fatalf("forbidden manifest key %q in %s", key, manifestBytes)
		}
	}
	for _, value := range []string{repository, "https://example.invalid/private.git", "raw prompt", "canonical-test-environment", "secret-credential", "fixture@example.invalid"} {
		if bytes.Contains(manifestBytes, []byte(value)) {
			t.Fatalf("forbidden manifest value %q in %s", value, manifestBytes)
		}
	}
	locator, err := engine.Store(context.Background(), staged)
	if err != nil {
		t.Fatal(err)
	}
	inspected, err := engine.Inspect(context.Background(), locator)
	if err != nil {
		t.Fatal(err)
	}
	if inspected.RepositoryID != snapshot.RepositoryID || inspected.WorkspaceID != snapshot.WorkspaceID || inspected.TaskID != snapshot.TaskID ||
		inspected.AttemptID != snapshot.AttemptID || inspected.Generation != snapshot.Generation || inspected.SealRequestID != snapshot.SealRequestID ||
		inspected.ImageIdentity != snapshot.ImageIdentity || inspected.Profile != snapshot.Profile || inspected.ProfileSHA256 != snapshot.ProfileSHA256 ||
		inspected.EnvironmentSHA256 != snapshot.EnvironmentSHA256 || inspected.ResourceSpecVersion != snapshot.ResourceSpecVersion ||
		inspected.OpenCodeSessionID != snapshot.OpenCodeSessionID || inspected.OpenCodeMessageID != snapshot.OpenCodeMessageID ||
		inspected.SnapshotPolicyVersion != snapshot.SnapshotPolicyVersion || inspected.CompletionAuthority != snapshot.CompletionAuthority ||
		inspected.ChangesSHA256 != snapshot.ChangesSHA256 || inspected.ManifestSHA256 != snapshot.ManifestSHA256 {
		t.Fatalf("inspected identity differs from snapshot: %+v", inspected)
	}
}

func TestManifestTamperingRejected(t *testing.T) {
	engine, repository, base := testEngineRepository(t)
	_, staged, err := engine.Snapshot(context.Background(), testSnapshotSpec(t, mustSource(t, repository), base, 45))
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Discard(staged)
	manifestBytes, err := os.ReadFile(filepath.Join(staged.path, manifestName))
	if err != nil {
		t.Fatal(err)
	}
	var original manifestWire
	if err := json.Unmarshal(manifestBytes, &original); err != nil {
		t.Fatal(err)
	}
	marshal := func(t *testing.T, wire manifestWire) []byte {
		t.Helper()
		encoded, err := json.Marshal(wire)
		if err != nil {
			t.Fatal(err)
		}
		return append(encoded, '\n')
	}
	invalid := []struct {
		name   string
		mutate func(*manifestWire)
	}{
		{"repository", func(w *manifestWire) { w.RepositoryID = 0 }},
		{"workspace", func(w *manifestWire) { w.WorkspaceID = "bad" }},
		{"task", func(w *manifestWire) { w.TaskID = "bad" }},
		{"attempt", func(w *manifestWire) { w.AttemptID = "bad" }},
		{"generation", func(w *manifestWire) { w.Generation = 0 }},
		{"seal request", func(w *manifestWire) { w.SealRequestID = "bad" }},
		{"image", func(w *manifestWire) { w.ImageIdentity = "sha256:" + strings.Repeat("A", 64) }},
		{"profile", func(w *manifestWire) { w.Profile = "bad profile" }},
		{"profile hash", func(w *manifestWire) { w.ProfileSHA256 = strings.Repeat("0", 64) }},
		{"environment hash", func(w *manifestWire) { w.EnvironmentSHA256 = strings.Repeat("0", 64) }},
		{"resource spec", func(w *manifestWire) { w.ResourceSpecVersion = 8 }},
		{"session", func(w *manifestWire) { w.OpenCodeSessionID = "bad" }},
		{"message", func(w *manifestWire) { w.OpenCodeMessageID = "bad" }},
		{"snapshot policy", func(w *manifestWire) { w.SnapshotPolicyVersion = "other" }},
		{"completion authority", func(w *manifestWire) { w.CompletionAuthority = "execution_success" }},
		{"changes hash", func(w *manifestWire) { w.ChangesSHA256 = strings.Repeat("0", 64) }},
	}
	for _, test := range invalid {
		t.Run("invalid "+test.name, func(t *testing.T) {
			wire := original
			test.mutate(&wire)
			if _, _, err := decodeManifest(marshal(t, wire)); err == nil {
				t.Fatal("invalid manifest accepted")
			}
		})
	}
	validTampering := []struct {
		name   string
		mutate func(*manifestWire)
	}{
		{"repository", func(w *manifestWire) { w.RepositoryID++ }},
		{"workspace", func(w *manifestWire) { w.WorkspaceID = testWorkspace2 }},
		{"task", func(w *manifestWire) { w.TaskID = testTask2 }},
		{"attempt", func(w *manifestWire) { w.AttemptID = testAttempt2 }},
		{"generation", func(w *manifestWire) { w.Generation++ }},
		{"seal request", func(w *manifestWire) { w.SealRequestID = testSeal2 }},
		{"image", func(w *manifestWire) { w.ImageIdentity = "sha256:" + strings.Repeat("b", 64) }},
		{"profile", func(w *manifestWire) { w.Profile = "opencode-test-2.0" }},
		{"profile hash", func(w *manifestWire) { w.ProfileSHA256 = digestString("other-profile") }},
		{"environment hash", func(w *manifestWire) { w.EnvironmentSHA256 = digestString("other-environment") }},
		{"session", func(w *manifestWire) { w.OpenCodeSessionID = testSession2 }},
		{"message", func(w *manifestWire) { w.OpenCodeMessageID = testMessage2 }},
	}
	for _, test := range validTampering {
		t.Run("digest-bound "+test.name, func(t *testing.T) {
			wire := original
			test.mutate(&wire)
			tampered := marshal(t, wire)
			if _, _, err := decodeManifest(tampered); err != nil {
				t.Fatalf("representative tamper is not independently valid: %v", err)
			}
			if _, err := engine.verifyArtifact(context.Background(), tampered, filepath.Join(staged.path, bundleName), 0o600, staged.digest); err == nil {
				t.Fatal("manifest identity tampering escaped CAS digest")
			}
		})
	}
	unknown := append([]byte(nil), manifestBytes[:len(manifestBytes)-2]...)
	unknown = append(unknown, []byte(`,"unknown":1}`+"\n")...)
	if _, _, err := decodeManifest(unknown); err == nil {
		t.Fatal("unknown manifest field accepted")
	}
	noncanonical := append([]byte(" "), manifestBytes...)
	if _, _, err := decodeManifest(noncanonical); err == nil {
		t.Fatal("noncanonical manifest whitespace accepted")
	}
}

func TestExecutionIdentityProducesDistinctLocators(t *testing.T) {
	engine, repository, base := testEngineRepository(t)
	firstSpec := testSnapshotSpec(t, mustSource(t, repository), base, 46)
	first, firstStage, err := engine.Snapshot(context.Background(), firstSpec)
	if err != nil {
		t.Fatal(err)
	}
	secondSource, err := NewSource(repository, testWorkspace, testTask, testAttempt2)
	if err != nil {
		t.Fatal(err)
	}
	secondSpec := testSnapshotSpec(t, secondSource, base, 46)
	secondSpec.Generation = 2
	second, secondStage, err := engine.Snapshot(context.Background(), secondSpec)
	if err != nil {
		t.Fatal(err)
	}
	if first.Result != second.Result || first.Tree != second.Tree || first.BundleSHA256 != second.BundleSHA256 || first.ManifestSHA256 == second.ManifestSHA256 {
		t.Fatalf("unexpected content/identity relationship:\n%+v\n%+v", first, second)
	}
	firstLocator, err := engine.Store(context.Background(), firstStage)
	if err != nil {
		t.Fatal(err)
	}
	secondLocator, err := engine.Store(context.Background(), secondStage)
	if err != nil {
		t.Fatal(err)
	}
	if firstLocator == secondLocator {
		t.Fatal("different attempt/generation identities deduplicated")
	}
}

func TestVerificationRebuildsChangesManifest(t *testing.T) {
	engine, repository, base := testEngineRepository(t)
	writeFile(t, filepath.Join(repository, "new"), []byte("new content\n"), 0o644)
	_, staged, err := engine.Snapshot(context.Background(), testSnapshotSpec(t, mustSource(t, repository), base, 47))
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Discard(staged)
	manifestBytes, err := os.ReadFile(filepath.Join(staged.path, manifestName))
	if err != nil {
		t.Fatal(err)
	}
	var wire manifestWire
	if err := json.Unmarshal(manifestBytes, &wire); err != nil || len(wire.Changes) != 1 || wire.Changes[0].New == nil {
		t.Fatalf("unexpected changed manifest: %+v, %v", wire.Changes, err)
	}
	wire.Changes[0].New.Size++
	changesJSON, err := json.Marshal(wire.Changes)
	if err != nil {
		t.Fatal(err)
	}
	wire.ChangesSHA256 = fmt.Sprintf("%x", sha256.Sum256(changesJSON))
	tampered, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	tampered = append(tampered, '\n')
	tamperedDigest := sha256Bytes(tampered)
	if _, _, err := decodeManifest(tampered); err != nil {
		t.Fatalf("self-consistent tampered changes rejected before Git verification: %v", err)
	}
	if _, err := engine.verifyArtifact(context.Background(), tampered, filepath.Join(staged.path, bundleName), 0o600, tamperedDigest); err == nil {
		t.Fatal("Git verification accepted self-consistent tampered changes")
	}
}

func TestDiscardStagedArtifact(t *testing.T) {
	engine, repository, base := testEngineRepository(t)
	_, staged, err := engine.Snapshot(context.Background(), testSnapshotSpec(t, mustSource(t, repository), base, 43))
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
			_, _, err := engine.Snapshot(context.Background(), testSnapshotSpec(t, mustSource(t, repository), base, 1))
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
		_, _, err = engine.Snapshot(context.Background(), testSnapshotSpec(t, mustSource(t, repository), base, 1))
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
		_, _, err = engine.Snapshot(context.Background(), testSnapshotSpec(t, mustSource(t, repository), base, 1))
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
	_, _, err = engine.Snapshot(context.Background(), testSnapshotSpec(t, mustSource(t, linked), base, 1))
	if !errors.Is(err, ErrUnsafeSource) {
		t.Fatalf("linked worktree accepted: %v", err)
	}
}

func TestBundleAndCASAttacksRejected(t *testing.T) {
	t.Run("truncated staged bundle", func(t *testing.T) {
		engine, repository, base := testEngineRepository(t)
		writeFile(t, filepath.Join(repository, "new"), []byte("new"), 0o644)
		_, staged, err := engine.Snapshot(context.Background(), testSnapshotSpec(t, mustSource(t, repository), base, 2))
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
		_, staged, err := engine.Snapshot(context.Background(), testSnapshotSpec(t, mustSource(t, repository), base, 3))
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
		for _, name := range []string{manifestName, bundleName} {
			info, err := os.Lstat(filepath.Join(staged.path, name))
			if err != nil || info.Mode().Perm() != 0o600 {
				t.Fatalf("failed publication left staged %s mode = %v, %v", name, info, err)
			}
		}
	})

	t.Run("stored mode", func(t *testing.T) {
		engine, repository, base := testEngineRepository(t)
		_, staged, err := engine.Snapshot(context.Background(), testSnapshotSpec(t, mustSource(t, repository), base, 4))
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
		_, staged, err := engine.Snapshot(context.Background(), testSnapshotSpec(t, mustSource(t, repository), base, 7))
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
		_, staged, err := engine.Snapshot(context.Background(), testSnapshotSpec(t, mustSource(t, repository), base, 8))
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
	_, staged, err := engine.Snapshot(context.Background(), testSnapshotSpec(t, mustSource(t, repository), base, 5))
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
	if _, err := engine.verifyArtifact(context.Background(), manifest, filepath.Join(staged.path, bundleName), 0o600, staged.digest); err != nil {
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
	_, staged, err := engine.Snapshot(context.Background(), testSnapshotSpec(t, mustSource(t, repository), base, 6))
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

func testSnapshotSpec(t *testing.T, source Source, base task.GitOID, epoch int64) SnapshotSpec {
	t.Helper()
	profileDigest, err := NewDigest(sha256.Sum256([]byte("opencode-test-profile")))
	if err != nil {
		t.Fatal(err)
	}
	environmentDigest, err := NewDigest(sha256.Sum256([]byte("canonical-test-environment")))
	if err != nil {
		t.Fatal(err)
	}
	return SnapshotSpec{
		Source: source, RepositoryID: 123, Generation: 1, SealRequestID: testSeal,
		ImageIdentity: "sha256:" + strings.Repeat("a", 64), Profile: "opencode-test-1.0", ProfileSHA256: profileDigest,
		EnvironmentSHA256: environmentDigest, ResourceSpecVersion: ResourceSpecVersion,
		OpenCodeSessionID: testSession, OpenCodeMessageID: testMessage, SnapshotPolicyVersion: SnapshotPolicyV1,
		Base: base, EpochSecond: epoch,
	}
}

func digestString(value string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(value)))
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
