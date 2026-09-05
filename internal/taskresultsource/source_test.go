package taskresultsource

import (
	"context"
	"crypto/sha256"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/nebler/fern/internal/task"
	"github.com/nebler/fern/internal/taskartifact"
	"github.com/nebler/fern/internal/taskstore"
)

type artifactStore struct{ value taskstore.RetainedArtifact }

func (s artifactStore) GetRetainedArtifact(context.Context, task.RetainedArtifactID) (taskstore.RetainedArtifact, error) {
	return s.value, nil
}

func TestRetainedSourceUsesFreshValidatedCheckoutAndAlwaysCleans(t *testing.T) {
	root := t.TempDir()
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	cas, work, repository := filepath.Join(root, "cas"), filepath.Join(root, "work"), filepath.Join(root, "repository")
	for _, path := range []string{cas, work, repository} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	git, err = filepath.EvalSymlinks(git)
	if err != nil {
		t.Fatal(err)
	}
	runGit := func(args ...string) string {
		command := exec.Command(git, append([]string{"-C", repository}, args...)...)
		command.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Fern", "GIT_AUTHOR_EMAIL=fern@example.invalid", "GIT_COMMITTER_NAME=Fern", "GIT_COMMITTER_EMAIL=fern@example.invalid")
		output, commandErr := command.Output()
		if commandErr != nil {
			t.Fatalf("git %v: %v", args, commandErr)
		}
		return string(output)
	}
	runGit("init")
	if err := os.WriteFile(filepath.Join(repository, "tracked.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit("add", "tracked.txt")
	runGit("commit", "-m", "base")
	base := task.GitOID(trim(runGit("rev-parse", "HEAD")))
	if err := os.WriteFile(filepath.Join(repository, "tracked.txt"), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	engine, err := taskartifact.New(taskartifact.Config{GitExecutable: git, CASRoot: cas, WorkRoot: work, CommandTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	ids := task.NewSecureGenerator()
	workspaceID, _ := ids.WorkspaceID()
	taskID, _ := ids.TaskID()
	attemptID, _ := ids.AttemptID()
	sealID, _ := ids.SealRequestID()
	resultID, _ := ids.ResultID()
	artifactID, _ := ids.RetainedArtifactID()
	exportID, _ := ids.ArtifactExportID()
	materializationID, _ := ids.MaterializationID()
	source, err := taskartifact.NewSource(repository, workspaceID, taskID, attemptID)
	if err != nil {
		t.Fatal(err)
	}
	profile, _ := taskartifact.NewDigest(sha256.Sum256([]byte("profile")))
	environment, _ := taskartifact.NewDigest(sha256.Sum256([]byte("environment")))
	snapshot, staged, err := engine.Snapshot(context.Background(), taskartifact.SnapshotSpec{Source: source, RepositoryID: 1,
		Generation: 1, SealRequestID: sealID, ImageIdentity: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Profile: "profile", ProfileSHA256: profile, EnvironmentSHA256: environment, ResourceSpecVersion: taskartifact.ResourceSpecVersion,
		OpenCodeSessionID: mustSession(t, ids), OpenCodeMessageID: mustMessage(t, ids), SnapshotPolicyVersion: taskartifact.SnapshotPolicyV1,
		Base: base, EpochSecond: 1})
	if err != nil {
		t.Fatal(err)
	}
	locator, err := engine.Store(context.Background(), staged)
	if err != nil {
		t.Fatal(err)
	}
	artifact := taskstore.RetainedArtifact{ID: artifactID, SealRequestID: sealID, ExportID: exportID,
		MaterializationID: materializationID, ResultID: resultID, WorkspaceID: workspaceID,
		TaskID: taskID, AttemptID: attemptID, Generation: 1,
		BaseSHA: snapshot.Base, ResultCommit: snapshot.Result, TreeOID: snapshot.Tree, ChangesSHA256: snapshot.ChangesSHA256.Bytes(),
		ManifestSHA256: snapshot.ManifestSHA256.Bytes(), CASLocator: locator.String(), BundleSHA256: snapshot.BundleSHA256.Bytes(),
		BundleBytes: snapshot.BundleBytes, OpenCodeSessionID: snapshot.OpenCodeSessionID, OpenCodeMessageID: snapshot.OpenCodeMessageID}
	result := taskstore.Result{ID: resultID, WorkspaceID: workspaceID, TaskID: taskID, AttemptID: attemptID, RepositoryID: 1,
		SourceKind: taskstore.ResultSourceRetainedArtifact, RetainedArtifactID: artifactID, ArtifactExportID: exportID,
		MaterializationID: materializationID, OpenCodeSessionID: snapshot.OpenCodeSessionID,
		OpenCodeMessageID: snapshot.OpenCodeMessageID, BaseSHA: snapshot.Base, ResultCommit: snapshot.Result,
		TreeOID: snapshot.Tree, ManifestSHA256: snapshot.ChangesSHA256.Bytes()}
	resolver, err := New(artifactStore{artifact}, engine)
	if err != nil {
		t.Fatal(err)
	}
	if err := resolver.Verify(context.Background(), result); err != nil {
		t.Fatalf("verify retained result: %v", err)
	}
	otherWorkspace, _ := ids.WorkspaceID()
	otherSeal, _ := ids.SealRequestID()
	otherArtifact, _ := ids.RetainedArtifactID()
	otherExport, _ := ids.ArtifactExportID()
	otherMaterialization, _ := ids.MaterializationID()
	otherSession := mustSession(t, ids)
	for _, test := range []struct {
		name   string
		mutate func(*taskstore.RetainedArtifact, *taskstore.Result)
	}{
		{"repository", func(_ *taskstore.RetainedArtifact, result *taskstore.Result) { result.RepositoryID++ }},
		{"workspace", func(artifact *taskstore.RetainedArtifact, _ *taskstore.Result) { artifact.WorkspaceID = otherWorkspace }},
		{"generation", func(artifact *taskstore.RetainedArtifact, _ *taskstore.Result) { artifact.Generation++ }},
		{"seal", func(artifact *taskstore.RetainedArtifact, _ *taskstore.Result) { artifact.SealRequestID = otherSeal }},
		{"artifact", func(_ *taskstore.RetainedArtifact, result *taskstore.Result) {
			result.RetainedArtifactID = otherArtifact
		}},
		{"export", func(_ *taskstore.RetainedArtifact, result *taskstore.Result) { result.ArtifactExportID = otherExport }},
		{"materialization", func(_ *taskstore.RetainedArtifact, result *taskstore.Result) {
			result.MaterializationID = otherMaterialization
		}},
		{"bundle digest", func(artifact *taskstore.RetainedArtifact, _ *taskstore.Result) { artifact.BundleSHA256[0] ^= 0xff }},
		{"bundle size", func(artifact *taskstore.RetainedArtifact, _ *taskstore.Result) { artifact.BundleBytes++ }},
		{"session", func(artifact *taskstore.RetainedArtifact, _ *taskstore.Result) {
			artifact.OpenCodeSessionID = otherSession
		}},
	} {
		t.Run("rejects "+test.name+" mismatch", func(t *testing.T) {
			changedArtifact, changedResult := artifact, result
			test.mutate(&changedArtifact, &changedResult)
			changedResolver, newErr := New(artifactStore{changedArtifact}, engine)
			if newErr != nil {
				t.Fatal(newErr)
			}
			if path, closeSource, acquireErr := changedResolver.Acquire(context.Background(), changedResult); path != "" || closeSource != nil || acquireErr != taskstore.ErrCorruptStore {
				t.Fatalf("mismatched authority path=%q close=%v error=%v", path, closeSource != nil, acquireErr)
			}
			if verifyErr := changedResolver.Verify(context.Background(), changedResult); verifyErr != taskstore.ErrCorruptStore {
				t.Fatalf("mismatched retention verification error=%v", verifyErr)
			}
		})
	}
	first, closeFirst, err := resolver.Acquire(context.Background(), result)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(first, "dirty"), []byte("dirty"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := closeFirst(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(first); !os.IsNotExist(err) {
		t.Fatalf("first checkout remains: %v", err)
	}
	second, closeSecond, err := resolver.Acquire(context.Background(), result)
	if err != nil {
		t.Fatal(err)
	}
	if second == first {
		t.Fatal("materialization was not fresh")
	}
	if _, err := os.Lstat(filepath.Join(second, "dirty")); !os.IsNotExist(err) {
		t.Fatalf("second checkout inherited dirt: %v", err)
	}
	if err := closeSecond(); err != nil {
		t.Fatal(err)
	}
}

func trim(value string) string {
	for len(value) > 0 && (value[len(value)-1] == '\n' || value[len(value)-1] == '\r') {
		value = value[:len(value)-1]
	}
	return value
}

func mustSession(t *testing.T, ids *task.Generator) task.OpenCodeSessionID {
	t.Helper()
	value, err := ids.OpenCodeSessionID()
	if err != nil {
		t.Fatal(err)
	}
	return value
}
func mustMessage(t *testing.T, ids *task.Generator) task.OpenCodeMessageID {
	t.Helper()
	value, err := ids.OpenCodeMessageID()
	if err != nil {
		t.Fatal(err)
	}
	return value
}
