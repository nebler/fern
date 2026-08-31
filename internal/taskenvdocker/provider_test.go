package taskenvdocker

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"
	"github.com/docker/go-connections/nat"
	"github.com/nebler/fern/internal/task"
	"github.com/nebler/fern/internal/taskstore"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const testImageID = "sha256:f493fc1cf2ffb087ef9733eb7f6f14fc0ae0966392fe54ccf695633570c82a82"

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (roundTrip roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

func TestCloneIsolationReconciliationUsageAndPrivateAuthority(t *testing.T) {
	provider, _, run := testProvider(t)
	first, err := provider.EnsureClone(context.Background(), run)
	if err != nil || !strings.Contains(first.Evidence, `"status":"created"`) {
		t.Fatalf("create clone: evidence=%q error=%v", first.Evidence, err)
	}
	clone := filepath.Join(provider.root, run.CloneIdentity)
	if common := gitOutput(t, provider.config.GitExecutable, clone, "rev-parse", "--git-common-dir"); common != ".git" {
		t.Fatalf("git common dir = %q", common)
	}
	if _, err := os.Stat(filepath.Join(clone, ".git", "objects", "info", "alternates")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("alternates exists: %v", err)
	}
	if _, err := os.Stat(filepath.Join(clone, ".git", markerName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("writable clone contains deletion authority: %v", err)
	}
	marker, err := os.Stat(provider.cloneMarkerPath(run))
	if err != nil || marker.Mode().Perm() != 0o600 {
		t.Fatalf("private marker info=%v error=%v", marker, err)
	}
	second, err := provider.EnsureClone(context.Background(), run)
	if err != nil || !strings.Contains(second.Evidence, `"status":"reconciled"`) {
		t.Fatalf("reconcile clone: evidence=%q error=%v", second.Evidence, err)
	}
	usage, err := provider.ObserveUsage(context.Background(), run)
	if err != nil || usage.CloneBytes <= 0 || usage.ObservedLimitBytes != provider.config.CloneObservedLimitBytes || usage.VolumeBytesAvailable {
		t.Fatalf("usage=%+v error=%v", usage, err)
	}
	if err := os.WriteFile(provider.cloneMarkerPath(run), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.EnsureClone(context.Background(), run); !errors.Is(err, ErrQuarantined) {
		t.Fatalf("marker mismatch error = %v, want quarantine", err)
	}
	if _, err := os.Stat(clone); err != nil {
		t.Fatalf("quarantined clone was removed: %v", err)
	}
}

func TestGitFsmonitorHooksAndHelpersCannotExecuteOnHost(t *testing.T) {
	t.Run("source fsmonitor", func(t *testing.T) {
		provider, _, run := testProvider(t)
		sentinel, helper := maliciousGitHelper(t)
		runGit(t, provider.config.GitExecutable, provider.config.Repository, "config", "core.fsmonitor", helper)
		if _, err := provider.EnsureClone(context.Background(), run); err == nil {
			t.Fatal("command-bearing source config was accepted")
		}
		if _, err := os.Stat(sentinel); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("source fsmonitor executed on host: %v", err)
		}
	})
	t.Run("source checkout hook", func(t *testing.T) {
		provider, _, run := testProvider(t)
		sentinel, helper := maliciousGitHelper(t)
		hook := filepath.Join(provider.config.Repository, ".git", "hooks", "post-checkout")
		data, err := os.ReadFile(helper)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(hook, data, 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := provider.EnsureClone(context.Background(), run); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(sentinel); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("source checkout hook executed on host: %v", err)
		}
	})
	t.Run("source filter process", func(t *testing.T) {
		provider, _, run := testProvider(t)
		sentinel, helper := maliciousGitHelper(t)
		runGit(t, provider.config.GitExecutable, provider.config.Repository, "config", "filter.evil.process", helper)
		if _, err := provider.EnsureClone(context.Background(), run); err == nil {
			t.Fatal("source filter process was accepted")
		}
		if _, err := os.Stat(sentinel); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("source filter process executed on host: %v", err)
		}
	})
	t.Run("writable clone config", func(t *testing.T) {
		provider, _, run := testProvider(t)
		sentinel, helper := maliciousGitHelper(t)
		if _, err := provider.EnsureClone(context.Background(), run); err != nil {
			t.Fatal(err)
		}
		clone := filepath.Join(provider.root, run.CloneIdentity)
		runGit(t, provider.config.GitExecutable, clone, "config", "core.fsmonitor", helper)
		runGit(t, provider.config.GitExecutable, clone, "config", "filter.evil.clean", helper)
		if _, err := provider.EnsureClone(context.Background(), run); !errors.Is(err, ErrQuarantined) {
			t.Fatalf("dangerous local config error=%v", err)
		}
		if _, err := os.Stat(sentinel); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("workload-controlled Git executable ran on host: %v", err)
		}
	})
}

func maliciousGitHelper(t *testing.T) (string, string) {
	t.Helper()
	sentinel := filepath.Join(t.TempDir(), "executed")
	helper := filepath.Join(t.TempDir(), "helper")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\ntouch \""+sentinel+"\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return sentinel, helper
}

func TestCloneRejectsUnreachableCommitIndexFlagsAndCriticalSymlink(t *testing.T) {
	t.Run("unreachable", func(t *testing.T) {
		provider, _, run := testProvider(t)
		runGit(t, provider.config.GitExecutable, provider.config.Repository, "checkout", "--orphan", "unreachable")
		if err := os.WriteFile(filepath.Join(provider.config.Repository, "orphan"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		runGit(t, provider.config.GitExecutable, provider.config.Repository, "add", "orphan")
		runGit(t, provider.config.GitExecutable, provider.config.Repository, "commit", "-m", "orphan")
		run.BaseOID = task.GitOID(gitOutput(t, provider.config.GitExecutable, provider.config.Repository, "rev-parse", "HEAD"))
		runGit(t, provider.config.GitExecutable, provider.config.Repository, "checkout", "main")
		runGit(t, provider.config.GitExecutable, provider.config.Repository, "branch", "-D", "unreachable")
		if _, err := provider.EnsureClone(context.Background(), run); err == nil {
			t.Fatal("unreachable exact commit was accepted")
		}
		assertNoStagingTrees(t, provider.root)
	})
	t.Run("index flag", func(t *testing.T) {
		provider, _, run := testProvider(t)
		if _, err := provider.EnsureClone(context.Background(), run); err != nil {
			t.Fatal(err)
		}
		clone := filepath.Join(provider.root, run.CloneIdentity)
		runGit(t, provider.config.GitExecutable, clone, "update-index", "--skip-worktree", "README.md")
		if _, err := provider.EnsureClone(context.Background(), run); !errors.Is(err, ErrQuarantined) {
			t.Fatalf("skip-worktree error=%v", err)
		}
	})
	t.Run("critical symlink", func(t *testing.T) {
		provider, _, run := testProvider(t)
		if _, err := provider.EnsureClone(context.Background(), run); err != nil {
			t.Fatal(err)
		}
		clone := filepath.Join(provider.root, run.CloneIdentity)
		config := filepath.Join(clone, ".git", "config")
		data, err := os.ReadFile(config)
		if err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(t.TempDir(), "config")
		if err := os.WriteFile(target, data, 0o666); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(config); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, config); err != nil {
			t.Fatal(err)
		}
		if _, err := provider.EnsureClone(context.Background(), run); !errors.Is(err, ErrQuarantined) {
			t.Fatalf("critical symlink error=%v", err)
		}
	})
}

func TestCloneCleanupQuarantinesSamePathReplacement(t *testing.T) {
	provider, _, run := testProvider(t)
	if _, err := provider.EnsureClone(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	clone := filepath.Join(provider.root, run.CloneIdentity)
	original := filepath.Join(provider.root, ".saved-original-clone")
	if err := os.Rename(clone, original); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(clone, 0o700); err != nil {
		t.Fatal(err)
	}
	unknown := filepath.Join(clone, "unknown")
	if err := os.WriteFile(unknown, []byte("retain"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.RemoveClone(context.Background(), run, NeverCreatedAuthority()); !errors.Is(err, ErrQuarantined) {
		t.Fatalf("same-path replacement error=%v", err)
	}
	if data, err := os.ReadFile(unknown); err != nil || string(data) != "retain" {
		t.Fatalf("same-path replacement was deleted: data=%q error=%v", data, err)
	}
}

func TestCloneCrashRecoveryFinishesStageAndQuarantine(t *testing.T) {
	t.Run("finish staged publication", func(t *testing.T) {
		provider, _, run := testProvider(t)
		if _, err := provider.EnsureClone(context.Background(), run); err != nil {
			t.Fatal(err)
		}
		canonical := filepath.Join(provider.root, run.CloneIdentity)
		stage := filepath.Join(provider.root, ".clone-stage-aaaaaaaaaaaa")
		if err := os.Mkdir(stage, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(canonical, filepath.Join(stage, "clone")); err != nil {
			t.Fatal(err)
		}
		observation, err := provider.EnsureClone(context.Background(), run)
		if err != nil || !strings.Contains(observation.Evidence, `"status":"recovered"`) {
			t.Fatalf("recover stage: evidence=%q error=%v", observation.Evidence, err)
		}
		if _, err := os.Stat(canonical); err != nil {
			t.Fatalf("canonical clone was not published: %v", err)
		}
		if _, err := os.Stat(stage); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stage remains after recovery: %v", err)
		}
	})
	t.Run("finish quarantined deletion", func(t *testing.T) {
		provider, _, run := testProvider(t)
		if _, err := provider.EnsureClone(context.Background(), run); err != nil {
			t.Fatal(err)
		}
		canonical := filepath.Join(provider.root, run.CloneIdentity)
		quarantine := filepath.Join(provider.root, ".clone-quarantine-bbbbbbbbbbbb")
		if err := os.Rename(canonical, quarantine); err != nil {
			t.Fatal(err)
		}
		observation, err := provider.RemoveClone(context.Background(), run, NeverCreatedAuthority())
		if err != nil || !strings.Contains(observation.Evidence, `"status":"recovered"`) {
			t.Fatalf("recover deletion: evidence=%q error=%v", observation.Evidence, err)
		}
		if _, err := os.Stat(quarantine); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("quarantine remains after recovery: %v", err)
		}
		if _, err := os.Stat(provider.cloneMarkerPath(run)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("marker remains after recovered deletion: %v", err)
		}
	})
	t.Run("unknown recovery location remains quarantined", func(t *testing.T) {
		provider, _, run := testProvider(t)
		if _, err := provider.EnsureClone(context.Background(), run); err != nil {
			t.Fatal(err)
		}
		canonical := filepath.Join(provider.root, run.CloneIdentity)
		unknown := filepath.Join(provider.root, ".unexpected-clone-location")
		if err := os.Rename(canonical, unknown); err != nil {
			t.Fatal(err)
		}
		if _, err := provider.RemoveClone(context.Background(), run, NeverCreatedAuthority()); !errors.Is(err, ErrQuarantined) {
			t.Fatalf("unknown marker-bound location error=%v", err)
		}
		if _, err := os.Stat(unknown); err != nil {
			t.Fatalf("unknown marker-bound inode was removed: %v", err)
		}
		if _, err := os.Stat(provider.cloneMarkerPath(run)); err != nil {
			t.Fatalf("authority marker was removed while inode remained: %v", err)
		}
	})
}

func TestCloneFilesystemWorkHonorsCancellationAndRemainsRecoverable(t *testing.T) {
	t.Run("tree sizing", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "data"), []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
		canceled, cancel := context.WithCancel(context.Background())
		cancel()
		for _, test := range []struct {
			name string
			ctx  context.Context
			want error
		}{
			{"canceled", canceled, context.Canceled},
			{"deadline", expiredContext(t), context.DeadlineExceeded},
		} {
			if _, err := treeSize(test.ctx, root); !errors.Is(err, test.want) {
				t.Fatalf("%s tree size error = %v", test.name, err)
			}
		}
	})

	t.Run("rollback quarantine", func(t *testing.T) {
		root := t.TempDir()
		stage := filepath.Join(root, ".clone-stage-aaaaaaaaaaaa")
		if err := os.Mkdir(stage, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(stage, "data"), []byte("retain"), 0o600); err != nil {
			t.Fatal(err)
		}
		expected, err := os.Lstat(stage)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := removeCreatedTree(ctx, root, stage, expected); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled rollback error = %v", err)
		}
		if _, err := os.Lstat(stage); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("rollback staging path remains: %v", err)
		}
		entries, err := os.ReadDir(root)
		if err != nil || len(entries) != 1 || !strings.HasPrefix(entries[0].Name(), ".clone-quarantine-") {
			t.Fatalf("rollback quarantine entries=%v error=%v", entries, err)
		}
		quarantined, err := os.Lstat(filepath.Join(root, entries[0].Name()))
		if err != nil || !os.SameFile(expected, quarantined) {
			t.Fatalf("rollback quarantine changed inode: info=%v error=%v", quarantined, err)
		}
	})

	t.Run("recursive deletion does not follow symlinks", func(t *testing.T) {
		root := t.TempDir()
		target := filepath.Join(root, "target")
		outside := t.TempDir()
		sentinel := filepath.Join(outside, "retain")
		if err := os.Mkdir(target, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(sentinel, []byte("retain"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(target, "outside")); err != nil {
			t.Fatal(err)
		}
		if err := removeCloneTree(context.Background(), root, target); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("clone tree remains: %v", err)
		}
		if data, err := os.ReadFile(sentinel); err != nil || string(data) != "retain" {
			t.Fatalf("recursive deletion followed symlink: data=%q error=%v", data, err)
		}
	})

	t.Run("marker-bound quarantine retry", func(t *testing.T) {
		provider, _, run := testProvider(t)
		if _, err := provider.EnsureClone(context.Background(), run); err != nil {
			t.Fatal(err)
		}
		digest, err := provider.validateRun(run)
		if err != nil {
			t.Fatal(err)
		}
		canonical := filepath.Join(provider.root, run.CloneIdentity)
		quarantine := filepath.Join(provider.root, ".clone-quarantine-cccccccccccc")
		if err := os.Rename(canonical, quarantine); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := provider.finishMarkerBoundDeletion(ctx, run, digest); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled quarantine cleanup error = %v", err)
		}
		if _, err := os.Lstat(quarantine); err != nil {
			t.Fatalf("canceled cleanup removed attested quarantine: %v", err)
		}
		recovered, err := provider.finishMarkerBoundDeletion(context.Background(), run, digest)
		if err != nil || !recovered {
			t.Fatalf("retry marker-bound cleanup: recovered=%t error=%v", recovered, err)
		}
		if _, err := os.Lstat(quarantine); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("retried cleanup left quarantine: %v", err)
		}
		if _, err := os.Lstat(provider.cloneMarkerPath(run)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("retried cleanup left marker: %v", err)
		}
	})
}

func expiredContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithDeadline(context.Background(), time.Unix(0, 0))
	t.Cleanup(cancel)
	return ctx
}

func TestConcurrentClonePublicationConvergesOnOneWinner(t *testing.T) {
	provider, docker, run := testProvider(t)
	other, err := New(context.Background(), provider.config, docker)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	start := make(chan struct{})
	type result struct {
		observation Observation
		err         error
	}
	results := make(chan result, 2)
	for _, candidate := range []*Provider{provider, other} {
		go func() {
			<-start
			observation, err := candidate.EnsureClone(context.Background(), run)
			results <- result{observation: observation, err: err}
		}()
	}
	close(start)
	created, reconciled := 0, 0
	for range 2 {
		result := <-results
		if result.err != nil {
			t.Fatal(result.err)
		}
		created += strings.Count(result.observation.Evidence, `"status":"created"`)
		reconciled += strings.Count(result.observation.Evidence, `"status":"reconciled"`)
	}
	if created != 1 || reconciled != 1 {
		t.Fatalf("concurrent publication statuses: created=%d reconciled=%d", created, reconciled)
	}
	digest, err := provider.validateRun(run)
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.attestCloneMarker(run, digest, filepath.Join(provider.root, run.CloneIdentity)); err != nil {
		t.Fatalf("published winner marker does not bind the canonical clone: %v", err)
	}
	assertNoStagingTrees(t, provider.root)
}

func TestStaleClonePublisherAndMarkerCleanupCannotReplaceWinner(t *testing.T) {
	provider, _, run := testProvider(t)
	if _, err := provider.EnsureClone(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	digest, err := provider.validateRun(run)
	if err != nil {
		t.Fatal(err)
	}
	canonical := filepath.Join(provider.root, run.CloneIdentity)
	winnerClone, err := os.Lstat(canonical)
	if err != nil {
		t.Fatal(err)
	}
	winnerMarker, err := provider.readCloneMarkerSnapshot(run, digest)
	if err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(provider.root, ".stale-publication")
	if err := os.Mkdir(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	staleInfo, err := os.Lstat(stale)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.writeCloneMarker(run, digest, staleInfo); err == nil {
		t.Fatal("stale worker overwrote the winner marker")
	}
	if err := renameNoReplace(stale, canonical); err == nil {
		t.Fatal("stale worker overwrote the canonical clone")
	}
	afterClone, err := os.Lstat(canonical)
	if err != nil || !os.SameFile(winnerClone, afterClone) {
		t.Fatalf("canonical clone winner changed: %v", err)
	}
	afterMarker, err := provider.readCloneMarkerSnapshot(run, digest)
	if err != nil || afterMarker != winnerMarker {
		t.Fatalf("marker winner changed: snapshot=%+v error=%v", afterMarker, err)
	}
	if err := os.RemoveAll(stale); err != nil {
		t.Fatal(err)
	}

	// Keep the old marker inode alive so the filesystem cannot reuse it, then
	// publish an equivalent new winner and exercise stale cleanup authority.
	retired := filepath.Join(provider.root, ".retired-marker")
	if err := renameNoReplace(provider.cloneMarkerPath(run), retired); err != nil {
		t.Fatal(err)
	}
	newWinner, err := provider.writeCloneMarker(run, digest, winnerClone)
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.removeExactCloneMarker(run, digest, winnerMarker, winnerMarker.marker.Device, winnerMarker.marker.Inode); err == nil {
		t.Fatal("stale cleanup removed a newer winner marker")
	}
	current, err := provider.readCloneMarkerSnapshot(run, digest)
	if err != nil || current != newWinner {
		t.Fatalf("new marker winner was disturbed: snapshot=%+v error=%v", current, err)
	}
	if err := os.Remove(retired); err != nil {
		t.Fatal(err)
	}
}

func TestSourceOriginMustBeOneExactFetchURL(t *testing.T) {
	t.Run("canonical Git suffix", func(t *testing.T) {
		provider, _, run := testProvider(t)
		runGit(t, provider.config.GitExecutable, provider.config.Repository, "remote", "set-url", "origin", run.RepositoryRemote+".git")
		if _, err := provider.EnsureClone(context.Background(), run); err != nil {
			t.Fatalf("canonical GitHub .git suffix was rejected: %v", err)
		}
	})
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, *Provider)
	}{
		{"missing", func(t *testing.T, p *Provider) {
			runGit(t, p.config.GitExecutable, p.config.Repository, "remote", "remove", "origin")
		}},
		{"mismatch", func(t *testing.T, p *Provider) {
			runGit(t, p.config.GitExecutable, p.config.Repository, "remote", "set-url", "origin", "https://github.com/other/repository")
		}},
		{"multiple", func(t *testing.T, p *Provider) {
			runGit(t, p.config.GitExecutable, p.config.Repository, "config", "--add", "remote.origin.url", "https://github.com/fern-test/repository")
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			provider, _, run := testProvider(t)
			test.mutate(t, provider)
			if _, err := provider.EnsureClone(context.Background(), run); err == nil {
				t.Fatal("unsafe source origin was accepted")
			}
		})
	}
}

func TestDurableKeyRecoveryAndLossHandling(t *testing.T) {
	provider, docker, run := testProvider(t)
	first := provider.password(run)
	keyInfo, err := os.Stat(filepath.Join(provider.root, hostKeyName))
	if err != nil || keyInfo.Mode().Perm() != 0o600 || keyInfo.Size() != 32 {
		t.Fatalf("first initialization key info=%v error=%v", keyInfo, err)
	}
	stale := filepath.Join(provider.root, ".host-key-stale")
	if err := os.Link(filepath.Join(provider.root, hostKeyName), stale); err != nil {
		t.Fatal(err)
	}
	reconstructed, err := New(context.Background(), provider.config, docker)
	if err != nil {
		t.Fatalf("recover stale post-link key: %v", err)
	}
	defer reconstructed.Close()
	if reconstructed.password(run) != first {
		t.Fatal("provider reconstruction changed derived password")
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale same-inode key link remains: %v", err)
	}
	if err := os.Remove(filepath.Join(provider.root, hostKeyName)); err != nil {
		t.Fatal(err)
	}
	if candidate, err := New(context.Background(), provider.config, docker); err == nil {
		candidate.Close()
		t.Fatal("missing key in initialized root was silently replaced")
	}
}

func TestConcurrentRootInitializationAdoptsOneAtomicWinner(t *testing.T) {
	provider, docker, run := testProvider(t)
	config := provider.config
	if err := os.RemoveAll(provider.root); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan *Provider, 2)
	errorsOut := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			candidate, err := New(context.Background(), config, docker)
			if err != nil {
				errorsOut <- err
				return
			}
			results <- candidate
		}()
	}
	close(start)
	var candidates []*Provider
	for range 2 {
		select {
		case err := <-errorsOut:
			t.Fatal(err)
		case candidate := <-results:
			candidates = append(candidates, candidate)
		}
	}
	defer candidates[0].Close()
	defer candidates[1].Close()
	if candidates[0].password(run) != candidates[1].password(run) {
		t.Fatal("concurrent initialization did not adopt one host-key winner")
	}
	info, err := os.Stat(filepath.Join(provider.root, hostKeyName))
	if err != nil || info.Mode().Perm() != 0o600 || info.Size() != 32 {
		t.Fatalf("atomic winner key info=%v error=%v", info, err)
	}
}

func TestVolumeAttestationAndLostCreateReconciliation(t *testing.T) {
	provider, docker, run := testProvider(t)
	if _, err := provider.EnsureClone(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	docker.loseVolumeCreateResponse = true
	if _, err := provider.EnsureVolume(context.Background(), run); err != nil {
		t.Fatalf("lost volume create response: %v", err)
	}
	if docker.freshReads == 0 || docker.wantFreshRead != 0 {
		t.Fatal("lost volume create was not reconciled with a fresh context")
	}
	item := docker.volumes[run.VolumeIdentity]
	item.Options = map[string]string{"type": "tmpfs"}
	docker.volumes[run.VolumeIdentity] = item
	if _, err := provider.EnsureVolume(context.Background(), run); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("volume options mismatch error=%v", err)
	}
	if docker.volumeRemoves != 0 {
		t.Fatal("mismatched volume was removed")
	}
}

func TestProviderEnforcesCloneVolumeContainerLifetimeOrder(t *testing.T) {
	provider, _, run := testProvider(t)
	if _, err := provider.EnsureVolume(context.Background(), run); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("volume without clone authority = %v", err)
	}
	if _, err := provider.EnsureClone(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.EnsureVolume(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	_, authority, err := provider.ProveWriterInactive(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.RemoveClone(context.Background(), run, authority); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("clone removal while volume exists = %v", err)
	}
	if _, err := os.Lstat(provider.cloneMarkerPath(run)); err != nil {
		t.Fatalf("clone authority disappeared before volume cleanup: %v", err)
	}
	if _, err := provider.RemoveVolume(context.Background(), run, authority); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.RemoveClone(context.Background(), run, authority); err != nil {
		t.Fatal(err)
	}
}

func TestConstructorRejectsUnboundedOrUnsafePolicy(t *testing.T) {
	provider, docker, _ := testProvider(t)
	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{"image reference", func(c *Config) { c.ImageReference = "" }},
		{"memory", func(c *Config) { c.MemoryBytes = 1 }},
		{"CPU", func(c *Config) { c.NanoCPUs = 1 }},
		{"PIDs", func(c *Config) { c.PIDs = 0 }},
		{"wall", func(c *Config) { c.WallTimeout = 8 * 24 * time.Hour }},
		{"disk", func(c *Config) { c.DiskFreeAdmissionBytes = c.CloneObservedLimitBytes - 1 }},
		{"observed", func(c *Config) { c.CloneObservedLimitBytes = c.SourceSizeAdmissionBytes - 1 }},
		{"logs", func(c *Config) { c.LogMaxSize = "2t" }},
		{"reserved environment", func(c *Config) { c.Environment[passwordEnv] = "forbidden" }},
		{"legacy OpenCode password", func(c *Config) { c.Environment["OPENCODE_PASSWORD"] = "forbidden" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := cloneConfig(provider.config)
			test.mutate(&config)
			if candidate, err := New(context.Background(), config, docker); err == nil {
				candidate.Close()
				t.Fatal("unsafe policy was accepted")
			}
		})
	}
}

func TestContainerLostResponsesAttestationAndExactEpochFence(t *testing.T) {
	provider, docker, run := preparedProvider(t)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := docker.ContainerInspect(canceled, run.ContainerIdentity); !errors.Is(err, context.Canceled) {
		t.Fatalf("fake Docker ignored canceled inspect context: %v", err)
	}
	docker.loseCreateResponse = true
	created, err := provider.EnsureContainer(context.Background(), run)
	if err != nil || created.ContainerID != fakeContainerID {
		t.Fatalf("lost create reconciliation: id=%q error=%v", created.ContainerID, err)
	}
	if docker.starts != 0 || docker.createConfig.User != containerUser || docker.createHost.Memory != provider.config.MemoryBytes || docker.createHost.PidsLimit == nil || *docker.createHost.PidsLimit != 512 || !slices.Equal(docker.createHost.CapDrop, []string{"ALL"}) {
		t.Fatal("container create was not constrained")
	}
	docker.loseStartResponse = true
	docker.hostPort = "49172"
	started, err := provider.StartContainer(context.Background(), run, created.ContainerID)
	if err != nil || started.RuntimeToken == "" || started.RuntimeEpoch <= 0 || started.ContainerStarted != "2026-08-31T12:00:00.123456789Z" {
		t.Fatalf("lost start reconciliation: observation=%+v error=%v", started, err)
	}
	if docker.freshReads < 2 || docker.wantFreshRead != 0 {
		t.Fatal("lost create/start responses were not reconciled with fresh contexts")
	}
	runtime := started.RuntimeIdentity()
	docker.info.State.StartedAt = "2026-08-31T12:00:01.123456789Z"
	if _, err := provider.Health(context.Background(), run, runtime); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("restarted same-container health error=%v", err)
	}
	if _, err := provider.StopContainer(context.Background(), run, runtime); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("restarted same-container stop error=%v", err)
	}
}

func TestBackgroundRouteTransportAttestsExactEpochBeforeForwarding(t *testing.T) {
	provider, docker, run := preparedProvider(t)
	created, err := provider.EnsureContainer(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	started, err := provider.StartContainer(context.Background(), run, created.ContainerID)
	if err != nil {
		t.Fatal(err)
	}
	run.ObservedContainerID = started.ContainerID
	run.ObservedContainerStartedAt = started.ContainerStarted
	run.RuntimeEpoch = started.RuntimeEpoch
	run.HostPort = started.HostPort
	var forwarded atomic.Int32
	provider.http.Transport = roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		forwarded.Add(1)
		if username, password, ok := request.BasicAuth(); !ok || username != provider.config.BasicUsername || password != provider.password(run) {
			t.Errorf("forwarded route credentials were not exact")
		}
		return &http.Response{StatusCode: http.StatusNoContent, Header: make(http.Header), Body: http.NoBody, Request: request}, nil
	})
	digest, err := provider.validateRun(run)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/", nil)
	managerTransport := &routeTransport{base: provider.http.Transport, provider: provider, run: run, digest: digest,
		runtime: started.RuntimeIdentity(), hostPort: run.HostPort, username: provider.config.BasicUsername, password: provider.password(run)}
	response, err := managerTransport.RoundTrip(request)
	if err != nil || response.StatusCode != http.StatusNoContent || forwarded.Load() != 1 {
		t.Fatalf("exact route response=%v forwarded=%d error=%v", response, forwarded.Load(), err)
	}
	docker.info.State.StartedAt = "2026-08-31T12:00:01.123456789Z"
	if _, err := managerTransport.RoundTrip(request); !errors.Is(err, ErrIdentityMismatch) || forwarded.Load() != 1 {
		t.Fatalf("replacement route forwarded=%d error=%v", forwarded.Load(), err)
	}
}

func TestExistingContainerFencesHostGitButNotFilesystemUsage(t *testing.T) {
	provider, docker, run := preparedProvider(t)
	if _, err := provider.EnsureContainer(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	sentinel, helper := maliciousGitHelper(t)
	clone := filepath.Join(provider.root, run.CloneIdentity)
	runGit(t, provider.config.GitExecutable, clone, "config", "core.fsmonitor", helper)
	usage, err := provider.ObserveUsage(context.Background(), run)
	if err != nil || usage.CloneBytes == 0 {
		t.Fatalf("filesystem-only usage observation=%+v error=%v", usage, err)
	}
	if _, err := provider.EnsureClone(context.Background(), run); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("host Git was not fenced by exact container: %v", err)
	}
	docker.info.Name = "/renamed-running-workload"
	if _, err := provider.EnsureClone(context.Background(), run); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("host Git was not fenced by exact-labeled renamed container: %v", err)
	}
	if _, err := os.Stat(sentinel); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workload-controlled Git process executed while container existed: %v", err)
	}
}

func TestCleanupAuthorityAlgebraAndRenamedNeverCreatedFence(t *testing.T) {
	t.Run("zero and mixed invalid", func(t *testing.T) {
		provider, _, run := testProvider(t)
		for _, authority := range []CleanupAuthority{{}, {NeverCreated: true, ContainerID: "unexpected"}, {ContainerID: "id", StartedAt: "time"}} {
			if _, err := provider.RemoveClone(context.Background(), run, authority); err == nil {
				t.Fatalf("invalid cleanup authority was accepted: %+v", authority)
			}
		}
	})
	t.Run("created exact ID", func(t *testing.T) {
		provider, _, run := preparedProvider(t)
		created, err := provider.EnsureContainer(context.Background(), run)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := provider.RemoveContainer(context.Background(), run, CreatedContainerAuthority(created.ContainerID)); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("started rejects created authority", func(t *testing.T) {
		provider, _, run := preparedProvider(t)
		created, err := provider.EnsureContainer(context.Background(), run)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := provider.StartContainer(context.Background(), run, created.ContainerID); err != nil {
			t.Fatal(err)
		}
		if _, err := provider.RemoveContainer(context.Background(), run, CreatedContainerAuthority(created.ContainerID)); err == nil {
			t.Fatal("started container accepted ID-only cleanup authority")
		}
	})
	t.Run("NeverCreated lists renamed labels", func(t *testing.T) {
		provider, docker, run := preparedProvider(t)
		if _, err := provider.EnsureContainer(context.Background(), run); err != nil {
			t.Fatal(err)
		}
		docker.info.Name = "/renamed-before-observation"
		authority := NeverCreatedAuthority()
		if _, err := provider.RemoveContainer(context.Background(), run, authority); !errors.Is(err, ErrIdentityMismatch) {
			t.Fatalf("renamed NeverCreated container removal error=%v", err)
		}
		if _, err := provider.RemoveVolume(context.Background(), run, authority); !errors.Is(err, ErrIdentityMismatch) {
			t.Fatalf("renamed NeverCreated volume cleanup error=%v", err)
		}
		if _, err := provider.RemoveClone(context.Background(), run, authority); !errors.Is(err, ErrIdentityMismatch) {
			t.Fatalf("renamed NeverCreated clone cleanup error=%v", err)
		}
	})
}

func TestAcquireExportSourceRejectsActiveAndReplacementWriters(t *testing.T) {
	t.Run("created fence became active", func(t *testing.T) {
		provider, _, run := preparedProvider(t)
		created, err := provider.EnsureContainer(context.Background(), run)
		if err != nil {
			t.Fatal(err)
		}
		_, fence, err := provider.ProveWriterInactive(context.Background(), run)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := provider.StartContainer(context.Background(), run, created.ContainerID); err != nil {
			t.Fatal(err)
		}
		if source, err := provider.AcquireExportSource(context.Background(), run, fence); source != nil || !errors.Is(err, ErrIdentityMismatch) {
			t.Fatalf("active writer export source=%v error=%v", source, err)
		}
	})

	t.Run("replacement process epoch", func(t *testing.T) {
		provider, docker, run, fence := stoppedExportFixture(t)
		docker.info.State.StartedAt = "2026-08-31T12:00:01.123456789Z"
		if source, err := provider.AcquireExportSource(context.Background(), run, fence); source != nil || !errors.Is(err, ErrIdentityMismatch) {
			t.Fatalf("replacement runtime export source=%v error=%v", source, err)
		}
	})

	t.Run("renamed exact-labeled runtime", func(t *testing.T) {
		provider, docker, run, fence := stoppedExportFixture(t)
		docker.info.Name = "/replacement-exact-labeled-runtime"
		if source, err := provider.AcquireExportSource(context.Background(), run, fence); source != nil || !errors.Is(err, ErrIdentityMismatch) {
			t.Fatalf("renamed runtime export source=%v error=%v", source, err)
		}
	})
}

func TestAcquireExportSourceExactStoppedRuntimeAndFenceValidation(t *testing.T) {
	provider, _, run, fence := stoppedExportFixture(t)
	source, err := provider.AcquireExportSource(context.Background(), run, fence)
	if err != nil {
		t.Fatal(err)
	}
	wantPath := filepath.Join(provider.root, run.CloneIdentity)
	info, err := os.Lstat(wantPath)
	if err != nil {
		t.Fatal(err)
	}
	device, inode, err := fileIdentity(info)
	if err != nil {
		t.Fatal(err)
	}
	if source.RepositoryPath() != wantPath || source.CloneIdentity() != run.CloneIdentity || source.Device() != device || source.Inode() != inode {
		t.Fatalf("export identity path=%q clone=%q device=%d inode=%d", source.RepositoryPath(), source.CloneIdentity(), source.Device(), source.Inode())
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatalf("double close: %v", err)
	}

	staleRun := run
	staleRun.OpenCodeMessageID = "msg_stale"
	if source, err := provider.AcquireExportSource(context.Background(), staleRun, fence); source != nil || err == nil {
		t.Fatalf("stale spec export source=%v error=%v", source, err)
	}
	staleGeneration := run
	staleGeneration.Generation++
	if source, err := provider.AcquireExportSource(context.Background(), staleGeneration, fence); source != nil || err == nil {
		t.Fatalf("stale generation export source=%v error=%v", source, err)
	}
	invalidFence := fence
	invalidFence.NeverCreated = true
	if source, err := provider.AcquireExportSource(context.Background(), run, invalidFence); source != nil || err == nil {
		t.Fatalf("mixed fence export source=%v error=%v", source, err)
	}
}

func TestAcquireExportSourceReattestsMarkerAndCloneInode(t *testing.T) {
	tests := []struct {
		name    string
		replace func(*testing.T, *Provider, taskstore.BackgroundRun)
	}{
		{
			name: "marker",
			replace: func(t *testing.T, provider *Provider, run taskstore.BackgroundRun) {
				path := provider.cloneMarkerPath(run)
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				retained, err := os.Open(path)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = retained.Close() })
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, data, 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "clone inode",
			replace: func(t *testing.T, provider *Provider, run taskstore.BackgroundRun) {
				path := filepath.Join(provider.root, run.CloneIdentity)
				retired := filepath.Join(provider.root, ".retired-export-clone")
				if err := os.Rename(path, retired); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider, docker, run, fence := stoppedExportFixture(t)
			inspectEntered := make(chan struct{})
			inspectRelease := make(chan struct{})
			docker.inspectHook = func() {
				close(inspectEntered)
				<-inspectRelease
			}
			type result struct {
				source *ExportSource
				err    error
			}
			resultCh := make(chan result, 1)
			go func() {
				source, err := provider.AcquireExportSource(context.Background(), run, fence)
				resultCh <- result{source: source, err: err}
			}()
			<-inspectEntered
			test.replace(t, provider, run)
			close(inspectRelease)
			got := <-resultCh
			if got.source != nil || !errors.Is(got.err, ErrIdentityMismatch) {
				t.Fatalf("replacement export source=%v error=%v", got.source, got.err)
			}
		})
	}
}

func TestExportSourceSerializesCloneRemovalAndProviderClose(t *testing.T) {
	provider, _, run, fence := stoppedExportFixture(t)
	if _, err := provider.RemoveContainer(context.Background(), run, fence); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.RemoveVolume(context.Background(), run, fence); err != nil {
		t.Fatal(err)
	}
	source, err := provider.AcquireExportSource(context.Background(), run, fence)
	if err != nil {
		t.Fatal(err)
	}

	lockWait := &observedDoneContext{Context: context.Background(), observed: make(chan struct{})}
	removeResult := make(chan error, 1)
	go func() {
		_, err := provider.RemoveClone(lockWait, run, fence)
		removeResult <- err
	}()
	<-lockWait.observed
	select {
	case err := <-removeResult:
		t.Fatalf("clone removal escaped export lease: %v", err)
	default:
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-removeResult; err != nil {
		t.Fatalf("serialized clone removal: %v", err)
	}

	provider, _, run = testProvider(t)
	if _, err := provider.EnsureClone(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	_, neverCreated, err := provider.ProveWriterInactive(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	source, err = provider.AcquireExportSource(context.Background(), run, neverCreated)
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.Close(); err != nil {
		t.Fatal(err)
	}
	if candidate, err := provider.AcquireExportSource(context.Background(), run, neverCreated); candidate != nil || !errors.Is(err, ErrProviderClosed) {
		t.Fatalf("closed provider export source=%v error=%v", candidate, err)
	}
	if err := source.Close(); err != nil {
		t.Fatalf("close lease after provider: %v", err)
	}
}

func TestExportAuthorityErrorsAndEvidenceDoNotLeakSecretsOrPaths(t *testing.T) {
	provider, docker, run := testProvider(t)
	if _, err := provider.EnsureClone(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	observation, fence, err := provider.ProveWriterInactive(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	secret := provider.password(run)
	for _, forbidden := range []string{provider.root, provider.config.Repository, secret, provider.config.Environment["FERN_MODEL"]} {
		if forbidden != "" && strings.Contains(observation.Evidence, forbidden) {
			t.Fatalf("writer evidence leaked %q: %s", forbidden, observation.Evidence)
		}
	}
	docker.inspectErr = errors.New("raw Docker diagnostics " + provider.root + " " + secret)
	_, err = provider.AcquireExportSource(context.Background(), run, fence)
	if err == nil {
		t.Fatal("raw Docker error was accepted")
	}
	for _, forbidden := range []string{provider.root, provider.config.Repository, secret, provider.config.Environment["FERN_MODEL"], "raw Docker diagnostics"} {
		if forbidden != "" && strings.Contains(err.Error(), forbidden) {
			t.Fatalf("export error leaked %q: %v", forbidden, err)
		}
	}
}

func TestContainerDimensionMismatchFailsClosed(t *testing.T) {
	provider, docker, run := preparedProvider(t)
	if _, err := provider.EnsureContainer(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(*container.InspectResponse)
	}{
		{"network", func(i *container.InspectResponse) { i.HostConfig.NetworkMode = "host" }},
		{"attachment", func(i *container.InspectResponse) { i.NetworkSettings.Networks["extra"] = &network.EndpointSettings{} }},
		{"IPC", func(i *container.InspectResponse) { i.HostConfig.IpcMode = "host" }},
		{"auto remove", func(i *container.InspectResponse) { i.HostConfig.AutoRemove = true }},
		{"port", func(i *container.InspectResponse) { i.HostConfig.PortBindings[serverPort][0].HostIP = "0.0.0.0" }},
		{"mount propagation", func(i *container.InspectResponse) { i.Mounts[0].Propagation = mount.PropagationRShared }},
		{"DNS", func(i *container.InspectResponse) { i.HostConfig.DNS = []string{"8.8.8.8"} }},
		{"device", func(i *container.InspectResponse) {
			i.HostConfig.Devices = []container.DeviceMapping{{PathOnHost: "/dev/null"}}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			original := docker.info
			original.HostConfig = copyHostConfig(docker.info.HostConfig)
			original.NetworkSettings = copyNetworkSettings(docker.info.NetworkSettings)
			original.Mounts = slices.Clone(docker.info.Mounts)
			test.mutate(&docker.info)
			if _, err := provider.EnsureContainer(context.Background(), run); !errors.Is(err, ErrQuarantined) {
				t.Fatalf("dimension mismatch error=%v", err)
			}
			docker.info = original
		})
	}
}

func TestPortableLocalVolumeMountFactsDoNotAssumeDaemonRootOrMode(t *testing.T) {
	provider, docker, run := preparedProvider(t)
	item := docker.volumes[run.VolumeIdentity]
	item.Mountpoint = "/custom-docker-data/volumes/opaque/mount"
	docker.volumes[run.VolumeIdentity] = item
	if _, err := provider.EnsureVolume(context.Background(), run); err != nil {
		t.Fatalf("portable local volume inspection failed: %v", err)
	}
	if _, err := provider.EnsureContainer(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	docker.info.Mounts[1].Source = "/custom-docker-data/volumes/opaque/mount"
	docker.info.Mounts[1].Mode = "daemon-specific"
	if _, err := provider.EnsureContainer(context.Background(), run); err != nil {
		t.Fatalf("daemon-specific volume source/mode was treated as identity: %v", err)
	}
}

func TestHealthExactContractAndRedirectCredentialNonLeak(t *testing.T) {
	provider, docker, run := preparedProvider(t)
	created, err := provider.EnsureContainer(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	password := provider.password(run)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		user, candidate, ok := r.BasicAuth()
		if !ok || user != provider.config.BasicUsername || candidate != password {
			w.Header().Set("WWW-Authenticate", `Basic realm="Secure Area"`)
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"_tag":"UnauthorizedError","message":"Authentication required"}`))
			return
		}
		_, _ = w.Write([]byte(`{"healthy":true}`))
	}))
	defer server.Close()
	docker.hostPort = server.Listener.Addr().String()[strings.LastIndex(server.Listener.Addr().String(), ":")+1:]
	started, err := provider.StartContainer(context.Background(), run, created.ContainerID)
	if err != nil {
		t.Fatal(err)
	}
	health, err := provider.Health(context.Background(), run, started.RuntimeIdentity())
	if err != nil || health.Endpoint != server.URL || strings.Contains(health.Evidence, password) {
		t.Fatalf("health evidence=%q endpoint=%q error=%v", health.Evidence, health.Endpoint, err)
	}

	var leaked atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { leaked.Store(true) }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		http.Redirect(w, nil, target.URL, http.StatusFound)
	}))
	defer redirect.Close()
	config := provider.config
	config.HTTPClient = &http.Client{}
	redirectProvider, err := New(context.Background(), config, docker)
	if err != nil {
		t.Fatal(err)
	}
	defer redirectProvider.Close()
	if _, err := redirectProvider.requestHealth(context.Background(), redirect.URL, "user", "redirect-secret"); err != nil {
		t.Fatal(err)
	}
	if leaked.Load() {
		t.Fatal("cloned HTTP client followed a different-port redirect with credentials")
	}
}

func TestHealthRejectsNoncanonicalJSONAndHeaders(t *testing.T) {
	provider, _, _ := testProvider(t)
	tests := []struct {
		name, contentType, body string
		duplicateContentType    bool
	}{
		{"duplicate JSON key", "application/json", `{"healthy":true,"healthy":true}`, false},
		{"unknown", "application/json", `{"healthy":true,"extra":false}`, false},
		{"case", "application/json", `{"Healthy":true}`, false},
		{"whitespace", "application/json", " {\"healthy\":true}\n", false},
		{"content type parameters", "application/json; charset=utf-8", `{"healthy":true}`, false},
		{"duplicate content type", "application/json", `{"healthy":true}`, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Add("Content-Type", test.contentType)
				if test.duplicateContentType {
					w.Header().Add("Content-Type", test.contentType)
				}
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			response, err := provider.requestHealth(context.Background(), server.URL, "user", "password")
			if err == nil {
				if err := validateHealthProbe(response, http.StatusOK, []byte(`{"healthy":true}`)); err == nil {
					t.Fatal("noncanonical health response was accepted")
				}
			}
		})
	}
	if err := validateHealthProbe(healthResponse{status: http.StatusUnauthorized, body: []byte(`{"_tag":"UnauthorizedError","message":"Authentication required"}`), challenges: []string{`Basic realm="Secure Area"`, `Basic realm="Secure Area"`}}, http.StatusUnauthorized, []byte(`{"_tag":"UnauthorizedError","message":"Authentication required"}`)); err == nil {
		t.Fatal("duplicate WWW-Authenticate headers were accepted")
	}
	if err := validateHealthProbe(healthResponse{status: http.StatusOK, body: []byte(`{"healthy":true}`), challenges: []string{`Basic realm="Secure Area"`}}, http.StatusOK, []byte(`{"healthy":true}`)); err == nil {
		t.Fatal("successful health response challenge was accepted")
	}
}

func TestCleanupIsIdempotentAndDetectsRenamedContainer(t *testing.T) {
	provider, docker, run := preparedProvider(t)
	created, err := provider.EnsureContainer(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	started, err := provider.StartContainer(context.Background(), run, created.ContainerID)
	if err != nil {
		t.Fatal(err)
	}
	runtime := started.RuntimeIdentity()
	if err := os.WriteFile(filepath.Join(provider.root, run.CloneIdentity, "result.txt"), []byte("dirty\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.StopContainer(context.Background(), run, runtime); err != nil {
		t.Fatal(err)
	}
	docker.renameOnRemove = true
	authority := RuntimeCleanupAuthority(runtime)
	if _, err := provider.RemoveContainer(context.Background(), run, authority); !errors.Is(err, ErrIdentityMismatch) && err == nil {
		t.Fatalf("renamed post-remove container error=%v", err)
	}
	if docker.info.ID == "" {
		t.Fatal("renamed unknown container was deleted")
	}
	docker.renameOnRemove = false
	docker.info.Name = "/" + run.ContainerIdentity
	if _, err := provider.RemoveContainer(context.Background(), run, authority); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.RemoveVolume(context.Background(), run, authority); err != nil {
		t.Fatal(err)
	}
	// Deletion authority is the private marker, not workload-writable .git state.
	runGit(t, provider.config.GitExecutable, filepath.Join(provider.root, run.CloneIdentity), "config", "core.fsmonitor", "workload-controlled")
	if _, err := provider.RemoveClone(context.Background(), run, authority); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.RemoveContainer(context.Background(), run, authority); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.RemoveVolume(context.Background(), run, authority); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.RemoveClone(context.Background(), run, authority); err != nil {
		t.Fatal(err)
	}
	assertNoStagingTrees(t, provider.root)
}

func TestCleanupSurvivesImageAndEnvironmentConfigurationRotation(t *testing.T) {
	provider, docker, run := testProvider(t)
	if _, err := provider.EnsureClone(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.EnsureVolume(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	created, err := provider.EnsureContainer(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	started, err := provider.StartContainer(context.Background(), run, created.ContainerID)
	if err != nil {
		t.Fatal(err)
	}

	recovery := *provider
	recovery.config = cloneConfig(provider.config)
	recovery.config.ImageID = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	recovery.config.Environment["FERN_MODEL"] = "rotated-model"
	recovery.imageLabels = cloneMap(provider.imageLabels)
	recovery.imageLabels["org.opencontainers.image.created"] = "2026-09-01T00:00:00Z"
	if _, err := recovery.EnsureClone(context.Background(), run); err == nil {
		t.Fatal("rotated execution configuration was allowed to resume the run")
	}
	observation, authority, err := recovery.ProveWriterInactive(context.Background(), run)
	if err != nil || observation.RuntimeToken != started.RuntimeToken {
		t.Fatalf("rotated cleanup stop = %+v, error=%v", observation, err)
	}
	if _, err := recovery.RemoveContainer(context.Background(), run, authority); err != nil {
		t.Fatal(err)
	}
	if _, err := recovery.RemoveVolume(context.Background(), run, authority); err != nil {
		t.Fatal(err)
	}
	if _, err := recovery.RemoveClone(context.Background(), run, authority); err != nil {
		t.Fatal(err)
	}
	if docker.containerRemoves != 1 || docker.volumeRemoves != 1 {
		t.Fatalf("cleanup calls container=%d volume=%d", docker.containerRemoves, docker.volumeRemoves)
	}
}

func TestMigratedSchemaEightRunUsesLegacyCleanupDigest(t *testing.T) {
	provider, docker, run := testProvider(t)
	run.ResourceSpecVersion = 8
	run.EnvironmentSHA256 = EnvironmentSHA256(nil)
	digest, err := provider.legacySpecDigest(run)
	if err != nil {
		t.Fatal(err)
	}
	clonePath := filepath.Join(provider.root, run.CloneIdentity)
	if err := os.Mkdir(clonePath, 0o700); err != nil {
		t.Fatal(err)
	}
	cloneInfo, err := os.Lstat(clonePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.writeCloneMarker(run, digest, cloneInfo); err != nil {
		t.Fatal(err)
	}
	docker.volumes[run.VolumeIdentity] = volume.Volume{
		Name: run.VolumeIdentity, Driver: "local", Scope: "local", Mountpoint: "/var/lib/docker/volumes/legacy/_data",
		Labels: provider.labels(run, digest),
	}
	provider.config.Environment = map[string]string{"OPENAI_API_KEY": "rotated"}
	provider.config.ImageReference = "fern/opencode-background-source:rotated"
	if _, err := provider.RemoveVolume(context.Background(), run, NeverCreatedAuthority()); err != nil {
		t.Fatalf("rotated legacy schema-8 volume cleanup: %v", err)
	}
}

func TestLostContainerRemoveResponseReconcilesWithFreshReads(t *testing.T) {
	provider, docker, run := preparedProvider(t)
	created, err := provider.EnsureContainer(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	started, err := provider.StartContainer(context.Background(), run, created.ContainerID)
	if err != nil {
		t.Fatal(err)
	}
	runtime := started.RuntimeIdentity()
	if _, err := provider.StopContainer(context.Background(), run, runtime); err != nil {
		t.Fatal(err)
	}
	docker.loseRemoveResponse = true
	if _, err := provider.RemoveContainer(context.Background(), run, RuntimeCleanupAuthority(runtime)); err != nil {
		t.Fatalf("lost remove response did not reconcile: %v", err)
	}
	if docker.freshReads == 0 || docker.wantFreshRead != 0 {
		t.Fatal("lost remove response was not reconciled with a fresh context")
	}
}

func preparedProvider(t *testing.T) (*Provider, *fakeDocker, taskstore.BackgroundRun) {
	t.Helper()
	provider, docker, run := testProvider(t)
	if _, err := provider.EnsureClone(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.EnsureVolume(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	return provider, docker, run
}

func testProvider(t *testing.T) (*Provider, *fakeDocker, taskstore.BackgroundRun) {
	t.Helper()
	state, repository := t.TempDir(), t.TempDir()
	state, _ = filepath.EvalSymlinks(state)
	repository, _ = filepath.EvalSymlinks(repository)
	if err := os.Chmod(state, 0o700); err != nil {
		t.Fatal(err)
	}
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	gitPath, err = filepath.EvalSymlinks(gitPath)
	if err != nil {
		t.Fatal(err)
	}
	runGit(t, gitPath, repository, "init", "--initial-branch=main")
	runGit(t, gitPath, repository, "config", "user.name", "Fern Test")
	runGit(t, gitPath, repository, "config", "user.email", "fern@example.invalid")
	runGit(t, gitPath, repository, "remote", "add", "origin", "https://github.com/fern-test/repository")
	if err := os.WriteFile(filepath.Join(repository, "README.md"), []byte("fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, gitPath, repository, "add", "README.md")
	runGit(t, gitPath, repository, "commit", "-m", "fixture")
	base := gitOutput(t, gitPath, repository, "rev-parse", "HEAD")
	docker := newFakeDocker()
	config := Config{
		StateRoot: state, Repository: repository, GitExecutable: gitPath,
		ImageReference: "fern/opencode-background-source:dev", ImageID: testImageID,
		MemoryBytes: 512 << 20, NanoCPUs: 2_000_000_000, PIDs: 512, WallTimeout: time.Minute,
		GitTimeout: 30 * time.Second, DockerTimeout: 10 * time.Second, HealthTimeout: 3 * time.Second,
		GitOutputBytes: 1 << 20, SourceSizeAdmissionBytes: 64 << 20, CloneObservedLimitBytes: 64 << 20,
		DiskFreeAdmissionBytes: 64 << 20, LogMaxSize: "1m", LogMaxFiles: 3, StopGrace: time.Second,
		Environment: map[string]string{"FERN_MODEL_PROVIDER": "test", "FERN_MODEL": "test-model"},
	}
	provider, err := New(context.Background(), config, docker)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = provider.Close() })
	compact := "0198d34d6a5075fbb1f2000000000201"
	run := taskstore.BackgroundRun{WorkspaceID: task.WorkspaceID("wsp_0198d34d-6a50-75fb-b1f2-000000000001"), TaskID: task.TaskID("tsk_0198d34d-6a50-75fb-b1f2-000000000201"), AttemptID: task.AttemptID("att_0198d34d-6a50-75fb-b1f2-000000000301"), Generation: 1, RepositoryRemote: "https://github.com/fern-test/repository", BaseOID: task.GitOID(base), Profile: taskstore.BackgroundRunSourceProfile, EnvironmentSHA256: EnvironmentSHA256(config.Environment), ResourceSpecVersion: 9, ImageIdentity: testImageID, CloneIdentity: "run-" + compact + "-g1-clone", VolumeIdentity: "fern-run-" + compact + "-g1-opencode", ContainerIdentity: "fern-run-" + compact + "-g1", EndpointIdentity: "run-" + compact + "-g1-endpoint", OpenCodeSessionID: "ses_test", OpenCodeMessageID: "msg_test"}
	return provider, docker, run
}

func qualifiedImage() image.InspectResponse {
	return image.InspectResponse{ID: testImageID, Config: &container.Config{User: containerUser, Env: []string{"PATH=/usr/local/bin:/usr/bin", "XDG_DATA_HOME=/home/user/.local/share", "XDG_CONFIG_HOME=/home/user/.config"}, Cmd: []string{"opencode", "serve", "--hostname", "0.0.0.0", "--port", "4096"}, ExposedPorts: nat.PortSet{serverPort: struct{}{}}, Volumes: map[string]struct{}{workspaceTarget: {}, opencodeTarget: {}}, Labels: map[string]string{"org.opencontainers.image.source": expectedSource, "org.opencontainers.image.revision": expectedRevision, "org.opencontainers.image.version": expectedVersion, "ai.fern.opencode.profile": expectedProfile}}}
}

const fakeContainerID = "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"

type fakeDocker struct {
	volumes                                                         map[string]volume.Volume
	info                                                            container.InspectResponse
	createConfig                                                    *container.Config
	createHost                                                      *container.HostConfig
	loseCreateResponse, loseVolumeCreateResponse, loseStartResponse bool
	loseRemoveResponse                                              bool
	renameOnRemove                                                  bool
	hostPort                                                        string
	starts, stops, containerRemoves, volumeRemoves                  int
	wantFreshRead, freshReads                                       int
	inspectErr                                                      error
	inspectHook                                                     func()
}

func newFakeDocker() *fakeDocker {
	return &fakeDocker{volumes: map[string]volume.Volume{}, hostPort: "49152"}
}

func (f *fakeDocker) ImageInspect(ctx context.Context, _ string, _ ...client.ImageInspectOption) (image.InspectResponse, error) {
	if err := ctx.Err(); err != nil {
		return image.InspectResponse{}, err
	}
	return qualifiedImage(), nil
}

func (f *fakeDocker) VolumeInspect(ctx context.Context, name string) (volume.Volume, error) {
	if err := f.observeReadContext(ctx); err != nil {
		return volume.Volume{}, err
	}
	item, ok := f.volumes[name]
	if !ok {
		return volume.Volume{}, errdefs.NotFound(errors.New("missing volume"))
	}
	return item, nil
}

func (f *fakeDocker) VolumeCreate(ctx context.Context, o volume.CreateOptions) (volume.Volume, error) {
	if err := ctx.Err(); err != nil {
		return volume.Volume{}, err
	}
	item := volume.Volume{Name: o.Name, Driver: "local", Scope: "local", Mountpoint: "/daemon-data/volumes/" + o.Name + "/mount", Labels: cloneMap(o.Labels), Options: map[string]string{}}
	f.volumes[o.Name] = item
	if f.loseVolumeCreateResponse {
		f.wantFreshRead++
		return item, context.DeadlineExceeded
	}
	return item, nil
}

func (f *fakeDocker) VolumeRemove(ctx context.Context, name string, _ bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, ok := f.volumes[name]; !ok {
		return errdefs.NotFound(errors.New("missing volume"))
	}
	delete(f.volumes, name)
	f.volumeRemoves++
	return nil
}

func (f *fakeDocker) ContainerInspect(ctx context.Context, identity string) (container.InspectResponse, error) {
	if err := f.observeReadContext(ctx); err != nil {
		return container.InspectResponse{}, err
	}
	if f.inspectHook != nil {
		f.inspectHook()
	}
	if f.inspectErr != nil {
		return container.InspectResponse{}, f.inspectErr
	}
	if f.info.ContainerJSONBase == nil || (identity != f.info.ID && identity != strings.TrimPrefix(f.info.Name, "/")) {
		return container.InspectResponse{}, errdefs.NotFound(errors.New("missing container"))
	}
	return f.info, nil
}

type observedDoneContext struct {
	context.Context
	observed chan struct{}
	once     sync.Once
}

func (ctx *observedDoneContext) Done() <-chan struct{} {
	ctx.once.Do(func() { close(ctx.observed) })
	return ctx.Context.Done()
}

func stoppedExportFixture(t *testing.T) (*Provider, *fakeDocker, taskstore.BackgroundRun, WriterFence) {
	t.Helper()
	provider, docker, run := preparedProvider(t)
	created, err := provider.EnsureContainer(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.StartContainer(context.Background(), run, created.ContainerID); err != nil {
		t.Fatal(err)
	}
	_, fence, err := provider.ProveWriterInactive(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	if docker.info.State == nil || docker.info.State.Running || docker.info.State.Status != "exited" {
		t.Fatal("fixture writer did not stop")
	}
	return provider, docker, run, fence
}

func (f *fakeDocker) ContainerList(ctx context.Context, options container.ListOptions) ([]container.Summary, error) {
	if err := f.observeReadContext(ctx); err != nil {
		return nil, err
	}
	if f.info.ContainerJSONBase == nil || f.info.Config == nil || !options.Filters.MatchKVList("label", f.info.Config.Labels) {
		return nil, nil
	}
	return []container.Summary{{ID: f.info.ID, Names: []string{f.info.Name}, Labels: cloneMap(f.info.Config.Labels), State: f.info.State.Status}}, nil
}

func (f *fakeDocker) ContainerCreate(ctx context.Context, c *container.Config, h *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, name string) (container.CreateResponse, error) {
	if err := ctx.Err(); err != nil {
		return container.CreateResponse{}, err
	}
	f.createConfig = c
	f.createHost = h
	c.Hostname = fakeContainerID[:12]
	points := []container.MountPoint{
		{Type: mount.TypeBind, Source: h.Mounts[0].Source, Destination: h.Mounts[0].Target, RW: true, Propagation: mount.PropagationRPrivate},
		{Type: mount.TypeVolume, Name: h.Mounts[1].Source, Source: "/daemon-data/volumes/" + h.Mounts[1].Source + "/mount", Destination: h.Mounts[1].Target, Driver: "local", Mode: "daemon-default", RW: true},
	}
	f.info = container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{ID: fakeContainerID, Name: "/" + name, Image: c.Image, HostConfig: h, State: &container.State{Status: "created"}},
		Config:            c, Mounts: points,
		NetworkSettings: &container.NetworkSettings{NetworkSettingsBase: container.NetworkSettingsBase{Ports: nat.PortMap{}}, Networks: map[string]*network.EndpointSettings{"bridge": {}}},
	}
	response := container.CreateResponse{ID: fakeContainerID}
	if f.loseCreateResponse {
		f.wantFreshRead++
		return response, context.DeadlineExceeded
	}
	return response, nil
}

func (f *fakeDocker) ContainerStart(ctx context.Context, id string, _ container.StartOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if id != f.info.ID || f.info.State.Status != "created" {
		return errors.New("invalid start target or state")
	}
	f.starts++
	f.info.State = &container.State{Status: "running", Running: true, StartedAt: "2026-08-31T12:00:00.123456789Z"}
	f.info.NetworkSettings.Ports = nat.PortMap{serverPort: []nat.PortBinding{{HostIP: "127.0.0.1", HostPort: f.hostPort}}}
	if f.loseStartResponse {
		f.wantFreshRead++
		return context.DeadlineExceeded
	}
	return nil
}

func (f *fakeDocker) ContainerStop(ctx context.Context, id string, _ container.StopOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if id != f.info.ID {
		return errors.New("invalid stop target")
	}
	f.stops++
	f.info.State.Running = false
	f.info.State.Status = "exited"
	return nil
}

func (f *fakeDocker) ContainerRemove(ctx context.Context, id string, _ container.RemoveOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if id != f.info.ID || f.info.State.Running {
		return errors.New("invalid remove target or state")
	}
	f.containerRemoves++
	if f.renameOnRemove {
		f.info.Name = "/renamed-after-lost-remove"
		f.wantFreshRead++
		return context.DeadlineExceeded
	}
	f.info = container.InspectResponse{}
	if f.loseRemoveResponse {
		f.wantFreshRead++
		return context.DeadlineExceeded
	}
	return nil
}

func (f *fakeDocker) observeReadContext(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if f.wantFreshRead > 0 {
		f.wantFreshRead--
		f.freshReads++
	}
	return nil
}

func copyHostConfig(source *container.HostConfig) *container.HostConfig {
	copy := *source
	copy.DNS = slices.Clone(source.DNS)
	copy.Devices = slices.Clone(source.Devices)
	copy.PortBindings = cloneMap(source.PortBindings)
	for port, bindings := range copy.PortBindings {
		copy.PortBindings[port] = slices.Clone(bindings)
	}
	return &copy
}

func copyNetworkSettings(source *container.NetworkSettings) *container.NetworkSettings {
	copy := *source
	copy.Networks = cloneMap(source.Networks)
	return &copy
}

func assertNoStagingTrees(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".clone-stage-") || strings.HasPrefix(entry.Name(), ".clone-quarantine-") {
			t.Fatalf("clone staging residue remains: %s", entry.Name())
		}
	}
}

func runGit(t *testing.T, git, directory string, args ...string) {
	t.Helper()
	command := exec.Command(git, args...)
	command.Dir = directory
	command.Env = append(os.Environ(), "LC_ALL=C")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
}

func gitOutput(t *testing.T, git, directory string, args ...string) string {
	t.Helper()
	command := exec.Command(git, args...)
	command.Dir = directory
	output, err := command.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(output))
}
