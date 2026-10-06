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

type countingArtifact struct {
	*taskartifact.Engine
	inspects, acquisitions int
	lastCheckout           *taskartifact.Checkout
	lastPath               string
}

func (a *countingArtifact) Inspect(ctx context.Context, locator taskartifact.Locator) (taskartifact.Snapshot, error) {
	a.inspects++
	return a.Engine.Inspect(ctx, locator)
}

func (a *countingArtifact) Acquire(ctx context.Context, locator taskartifact.Locator) (taskartifact.Snapshot, *taskartifact.Checkout, error) {
	a.acquisitions++
	snapshot, checkout, err := a.Engine.Acquire(ctx, locator)
	a.lastCheckout, a.lastPath = checkout, checkout.Path()
	return snapshot, checkout, err
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
	resultID, _ := ids.ResultID()
	sessionID, messageID := mustSession(t, ids), mustMessage(t, ids)
	source, err := taskartifact.NewSource(repository, workspaceID, taskID)
	if err != nil {
		t.Fatal(err)
	}
	profile, _ := taskartifact.NewDigest(sha256.Sum256([]byte("profile")))
	environment, _ := taskartifact.NewDigest(sha256.Sum256([]byte("environment")))
	snapshot, staged, err := engine.Snapshot(context.Background(), taskartifact.SnapshotSpec{Source: source, RepositoryID: 1,
		ResultID: resultID, ImageIdentity: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Profile: "profile", ProfileSHA256: profile, EnvironmentSHA256: environment, ResourceSpecVersion: taskartifact.ResourceSpecVersion,
		OpenCodeSessionID: sessionID, OpenCodeMessageID: messageID, SnapshotPolicyVersion: taskartifact.SnapshotPolicyV1,
		Base: base, EpochSecond: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Store(context.Background(), staged); err != nil {
		t.Fatal(err)
	}
	sealedAt := time.UnixMilli(2)
	projection := taskstore.BackgroundRunResultProjection{
		Run: taskstore.BackgroundRun{WorkspaceID: workspaceID, TaskID: taskID, RepositoryID: 1, BaseOID: base,
			OpenCodeSessionID: sessionID, OpenCodeMessageID: messageID, Seal: &taskstore.Seal{ResultID: resultID}},
		Result: taskstore.Result{ID: resultID, TaskID: taskID, State: taskstore.ResultSealed, BaseSHA: snapshot.Base,
			ResultCommit: snapshot.Result, TreeOID: snapshot.Tree, ChangeCount: len(snapshot.Changes), ChangesSHA256: snapshot.ChangesSHA256.Bytes(),
			ManifestSHA256: snapshot.ManifestSHA256.Bytes(), BundleSHA256: snapshot.BundleSHA256.Bytes(), BundleBytes: snapshot.BundleBytes,
			SealedAt: &sealedAt},
	}
	counted := &countingArtifact{Engine: engine}
	resolver, err := New(counted)
	if err != nil {
		t.Fatal(err)
	}
	if err := resolver.Verify(context.Background(), projection); err != nil {
		t.Fatalf("verify retained result: %v", err)
	}
	if counted.inspects != 1 || counted.acquisitions != 0 {
		t.Fatalf("Verify calls: Inspect=%d Acquire=%d", counted.inspects, counted.acquisitions)
	}
	otherWorkspace, _ := ids.WorkspaceID()
	otherResult, _ := ids.ResultID()
	otherSession := mustSession(t, ids)
	for _, test := range []struct {
		name   string
		mutate func(*taskstore.BackgroundRunResultProjection)
	}{
		{"repository", func(p *taskstore.BackgroundRunResultProjection) { p.Run.RepositoryID++ }},
		{"workspace", func(p *taskstore.BackgroundRunResultProjection) { p.Run.WorkspaceID = otherWorkspace }},
		{"result", func(p *taskstore.BackgroundRunResultProjection) {
			p.Result.ID, p.Run.Seal.ResultID = otherResult, otherResult
		}},
		{"unsealed", func(p *taskstore.BackgroundRunResultProjection) { p.Result.State = taskstore.ResultSelected }},
		{"change count", func(p *taskstore.BackgroundRunResultProjection) { p.Result.ChangeCount++ }},
		{"bundle digest", func(p *taskstore.BackgroundRunResultProjection) { p.Result.BundleSHA256[0] ^= 0xff }},
		{"bundle size", func(p *taskstore.BackgroundRunResultProjection) { p.Result.BundleBytes++ }},
		{"session", func(p *taskstore.BackgroundRunResultProjection) { p.Run.OpenCodeSessionID = otherSession }},
	} {
		t.Run("rejects "+test.name+" mismatch", func(t *testing.T) {
			changed := projection
			seal := *projection.Run.Seal
			changed.Run.Seal = &seal
			test.mutate(&changed)
			changedEngine := &countingArtifact{Engine: engine}
			changedResolver, newErr := New(changedEngine)
			if newErr != nil {
				t.Fatal(newErr)
			}
			if path, closeSource, acquireErr := changedResolver.Acquire(context.Background(), changed); path != "" || closeSource != nil || acquireErr != taskstore.ErrCorruptStore {
				t.Fatalf("mismatched authority path=%q close=%v error=%v", path, closeSource != nil, acquireErr)
			}
			if verifyErr := changedResolver.Verify(context.Background(), changed); verifyErr != taskstore.ErrCorruptStore {
				t.Fatalf("mismatched retention verification error=%v", verifyErr)
			}
			if changedEngine.acquisitions != 1 || changedEngine.inspects != 1 {
				t.Fatalf("unexpected calls: Acquire=%d Inspect=%d", changedEngine.acquisitions, changedEngine.inspects)
			}
			if changedEngine.lastPath == "" || changedEngine.lastCheckout.Path() != "" {
				t.Fatal("tuple mismatch did not close the acquired checkout")
			}
			if _, err := os.Lstat(changedEngine.lastPath); !os.IsNotExist(err) {
				t.Fatalf("mismatched checkout remains: %v", err)
			}
			if entries, err := os.ReadDir(work); err != nil || len(entries) != 0 {
				t.Fatalf("tuple mismatch leaked work directories: %v, %v", entries, err)
			}
		})
	}
	first, closeFirst, err := resolver.Acquire(context.Background(), projection)
	if err != nil {
		t.Fatal(err)
	}
	if counted.acquisitions != 1 || counted.inspects != 1 {
		t.Fatalf("Acquire must not independently Inspect: Acquire=%d Inspect=%d", counted.acquisitions, counted.inspects)
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
	second, closeSecond, err := resolver.Acquire(context.Background(), projection)
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
