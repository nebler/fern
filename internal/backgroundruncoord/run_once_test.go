package backgroundruncoord

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"
	"github.com/docker/go-connections/nat"
	"github.com/nebler/fern/internal/backgroundopencode"
	"github.com/nebler/fern/internal/backgroundroute"
	"github.com/nebler/fern/internal/task"
	"github.com/nebler/fern/internal/taskenvdocker"
	"github.com/nebler/fern/internal/taskstore"
)

const scanImage = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

// Unimplemented Docker calls panic through the nil embedded client: these
// tests must never create/start a container or contact a Docker daemon.
type scanDocker struct {
	*client.Client
	calls           []string
	item            *volume.Volume
	inspectErr      error
	containerErr    error
	afterVolumeRead func()
	creates         int
}

func (d *scanDocker) VerifyRuntimeStorage(string) error { return nil }

func (d *scanDocker) ImageInspect(context.Context, string, ...client.ImageInspectOption) (image.InspectResponse, error) {
	return image.InspectResponse{ID: scanImage, Config: &container.Config{
		User: "1001:1001",
		Env: []string{
			"PATH=/usr/local/bin:/usr/bin",
			"XDG_DATA_HOME=/home/user/.local/share",
			"XDG_CONFIG_HOME=/home/user/.config",
		},
		Cmd:          []string{"opencode", "serve", "--hostname", "0.0.0.0", "--port", "4096"},
		ExposedPorts: nat.PortSet{"4096/tcp": {}},
		Volumes:      map[string]struct{}{"/home/user/workspace": {}, "/home/user/.local/share/opencode": {}},
		Labels: map[string]string{
			"org.opencontainers.image.source":   "https://github.com/anomalyco/opencode",
			"org.opencontainers.image.revision": "39fb919a054190498f6d5b7985bde231f93ad7a6",
			"org.opencontainers.image.version":  "0.0.0-source-39fb919a054190498f6d5b7985bde231f93ad7a6",
			"ai.fern.opencode.profile":          taskstore.BackgroundRunSourceProfile,
			"ai.fern.runtime.spec":              "10",
		},
	}}, nil
}
func (d *scanDocker) VolumeInspect(ctx context.Context, _ string) (volume.Volume, error) {
	d.calls = append(d.calls, "volume.inspect")
	if err := ctx.Err(); err != nil {
		return volume.Volume{}, err
	}
	if d.inspectErr != nil {
		return volume.Volume{}, d.inspectErr
	}
	if d.item == nil {
		return volume.Volume{}, errdefs.NotFound(errors.New("absent"))
	}
	if d.afterVolumeRead != nil {
		d.afterVolumeRead()
	}
	return *d.item, nil
}
func (d *scanDocker) VolumeCreate(_ context.Context, o volume.CreateOptions) (volume.Volume, error) {
	d.calls = append(d.calls, "volume.create")
	d.creates++
	d.item = &volume.Volume{Name: o.Name, Labels: o.Labels, Driver: "local", Scope: "local", Mountpoint: "/daemon/volume"}
	return *d.item, nil
}
func (d *scanDocker) VolumeRemove(context.Context, string, bool) error {
	d.calls = append(d.calls, "volume.remove")
	d.item = nil
	return nil
}
func (d *scanDocker) VolumeList(_ context.Context, options volume.ListOptions) (volume.ListResponse, error) {
	d.calls = append(d.calls, "volume.list")
	result := volume.ListResponse{}
	if d.item != nil && options.Filters.MatchKVList("label", d.item.Labels) {
		result.Volumes = append(result.Volumes, d.item)
	}
	return result, nil
}
func (d *scanDocker) ContainerInspect(context.Context, string) (container.InspectResponse, error) {
	d.calls = append(d.calls, "container.inspect")
	if d.containerErr != nil {
		return container.InspectResponse{}, d.containerErr
	}
	return container.InspectResponse{}, errdefs.NotFound(errors.New("absent"))
}
func (d *scanDocker) ContainerList(context.Context, container.ListOptions) ([]container.Summary, error) {
	d.calls = append(d.calls, "container.list")
	return nil, nil
}

type unusedArtifact struct{ Artifact }
type scanFixture struct {
	c      *Coordinator
	d      *scanDocker
	params taskstore.AdmitBackgroundRunParams
	now    time.Time
	root   string
}

func newScanFixture(t *testing.T) *scanFixture {
	t.Helper()
	f := &scanFixture{now: time.Now().UTC().Truncate(time.Millisecond), d: &scanDocker{}}
	private := func() string {
		p, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if err = os.Chmod(p, 0700); err != nil {
			t.Fatal(err)
		}
		return p
	}
	f.root = private()
	repo := private()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	git, err = filepath.EvalSymlinks(git)
	if err != nil {
		t.Fatal(err)
	}
	command := func(args ...string) string {
		cmd := exec.Command(git, args...)
		cmd.Dir = repo
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s: %v", args, out, err)
		}
		return strings.TrimSpace(string(out))
	}
	command("init", "--initial-branch=main")
	command("config", "user.name", "Test")
	command("config", "user.email", "test@example.invalid")
	command("remote", "add", "origin", "https://github.com/owner/repository")
	command("-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "fixture")
	provider, err := taskenvdocker.New(context.Background(), taskenvdocker.Config{StateRoot: f.root, Repository: repo, GitExecutable: git, ImageReference: "fern/test:dev", ImageID: scanImage, MemoryBytes: 512 << 20, NanoCPUs: 2_000_000_000, PIDs: 512, WallTimeout: time.Hour, GitTimeout: 30 * time.Second, DockerTimeout: 10 * time.Second, HealthTimeout: 3 * time.Second, GitOutputBytes: 1 << 20, SourceSizeAdmissionBytes: 64 << 20, CloneObservedLimitBytes: 64 << 20, DiskFreeAdmissionBytes: 64 << 20, LogMaxSize: "1m", LogMaxFiles: 3, StopGrace: time.Second}, f.d)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = provider.Close() })
	store, err := taskstore.Open(context.Background(), filepath.Join(private(), "tasks.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	id := func(prefix string, n int) string { return fmt.Sprintf("%s0198d34d-6a50-75fb-b1f2-%012x", prefix, n) }
	workspace := task.WorkspaceID(id("wsp_", 1))
	if err = store.CreateWorkspace(context.Background(), taskstore.Workspace{ID: workspace, Name: "test", State: taskstore.WorkspaceActive, RepositoryPath: repo, GitHubAuthority: taskstore.GitHubAuthorityAppBroker, InstallationID: 123, RepositoryID: 987654321, RepositoryFullName: "owner/repository", ImageDigest: scanImage, OpenCodeProtocol: "v2", RuntimeDesiredState: "running", ReconciliationEpoch: 1, CreatedAt: f.now}); err != nil {
		t.Fatal(err)
	}
	actor := task.ActorSnapshot{Type: task.ActorOpenCode, ID: "pc_owner", DisplayName: "OpenCode", CredentialID: "pc_owner", Authentication: "fern_plugin_bearer", RequestID: "req-1"}
	f.params = taskstore.AdmitBackgroundRunParams{TaskID: task.TaskID(id("tsk_", 2)), AttemptID: task.AttemptID(id("att_", 3)), ReceiptID: task.ReceiptID(id("rcp_", 4)), TaskEventID: task.EventID(id("fev_", 5)), AttemptEventID: task.EventID(id("fev_", 6)), OpenCodeSessionID: "ses_00000000000000000000000000000001", OpenCodeMessageID: "msg_00000000000000000000000000000001", Claim: task.IdempotencyClaim{Scope: task.IdempotencyScope{WorkspaceID: workspace, CommandKind: taskstore.CreateBackgroundRunCommand}, Key: "create", RequestHash: sha256.Sum256([]byte("create")), Actor: actor}, Title: "Test", Prompt: "Do work", RepositoryID: 987654321, BaseRef: "main", BaseSHA: task.GitOID(command("rev-parse", "HEAD")), ObjectFormat: "sha1", ExecutionContractVersion: "exec-v1", Agent: "build", ModelProvider: "provider", Model: "model", Deadline: f.now.Add(time.Hour), APIContractVersion: "v1", AcceptedAt: f.now}
	compact := strings.ReplaceAll(strings.TrimPrefix(string(f.params.TaskID), "tsk_"), "-", "")
	f.params.BackgroundRun = &taskstore.BackgroundRunIntent{RepositoryRemote: "https://github.com/owner/repository", Branch: "main", InstructionSHA256: sha256.Sum256([]byte(f.params.Prompt)), Profile: taskstore.BackgroundRunSourceProfile, ProfileSHA256: sha256.Sum256([]byte(taskstore.BackgroundRunSourceProfile)), EnvironmentSHA256: taskenvdocker.EnvironmentSHA256(nil), ImageIdentity: scanImage, CloneIdentity: "run-" + compact + "-g1-clone", VolumeIdentity: "fern-run-" + compact + "-g1-opencode", ContainerIdentity: "fern-run-" + compact + "-g1", EndpointIdentity: "run-" + compact + "-g1-endpoint"}
	ids, err := task.NewGenerator(rand.Reader, func() time.Time { return f.now })
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	route, err := backgroundroute.New(listener, "https://"+listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	f.c, err = New(store, provider, unusedArtifact{}, ids, Config{WorkspaceID: workspace, WorkerID: "worker", SystemActor: task.ActorSnapshot{Type: task.ActorSystem, ID: "coordinator", DisplayName: "Coordinator", CredentialID: "service", Authentication: "internal", RequestID: "scan"}, Profile: taskstore.BackgroundRunSourceProfile, ImageIdentity: scanImage, EnvironmentSHA256: f.params.BackgroundRun.EnvironmentSHA256, Agent: "build", ModelProvider: "provider", Model: "model", OperationTimeout: 30 * time.Second, LeaseDuration: time.Minute, PollInterval: time.Hour, Now: func() time.Time { return f.now }, HTTPClient: &http.Client{Timeout: time.Second}, Route: route, HistoryBounds: backgroundopencode.HistoryBounds{PageLimit: 10, MaxPages: 10, MaxEvents: 100}})
	if err != nil {
		t.Fatal(err)
	}
	return f
}
func (f *scanFixture) admit(t *testing.T) {
	t.Helper()
	if _, err := f.c.store.AdmitBackgroundRun(context.Background(), f.params); err != nil {
		t.Fatal(err)
	}
}
func (f *scanFixture) run(t *testing.T) taskstore.BackgroundRun {
	t.Helper()
	r, err := f.c.store.GetBackgroundRun(context.Background(), f.c.config.WorkspaceID, f.params.TaskID, f.params.Claim.Actor)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func (f *scanFixture) scan(t *testing.T, phase taskstore.BackgroundRunEffectPhase) {
	t.Helper()
	if err := f.c.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r := f.run(t); r.EffectPhase != phase {
		t.Fatalf("phase=%s want %s", r.EffectPhase, phase)
	}
}

func TestRunOnceNoWorkAndCanceledAdmission(t *testing.T) {
	f := newScanFixture(t)
	if err := f.c.RunOnce(context.Background()); !errors.Is(err, ErrNoWork) {
		t.Fatal(err)
	}
	f.admit(t)
	before := f.run(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := f.c.RunOnce(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if after := f.run(t); !reflect.DeepEqual(before, after) {
		t.Fatal("canceled scan mutated durable run")
	}
	if len(f.d.calls) != 0 {
		t.Fatal(f.d.calls)
	}
}

func TestRunSupervisesRealScans(t *testing.T) {
	for _, mode := range []string{"no work", "progress", "transient failure"} {
		t.Run(mode, func(t *testing.T) {
			f := newScanFixture(t)
			if mode != "no work" {
				f.admit(t)
			}
			failure := errors.New("Docker unavailable")
			if mode == "transient failure" {
				f.scan(t, taskstore.BackgroundRunEffectCloneObserved)
				f.d.inspectErr = failure
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			successes, failures := 0, 0
			f.c.config.OnSuccess = func() { successes++; cancel() }
			f.c.config.OnError = func(err error) {
				failures++
				if !errors.Is(err, failure) {
					t.Errorf("unexpected scan error: %v", err)
				}
				cancel()
			}
			if err := f.c.Run(ctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("Run: %v", err)
			}
			if mode == "transient failure" {
				if successes != 0 || failures != 1 {
					t.Fatalf("callbacks success=%d failure=%d", successes, failures)
				}
				if r := f.run(t); r.EffectPhase != taskstore.BackgroundRunEffectCloneObserved || r.ClaimOwner != "" {
					t.Fatalf("failure not recoverable: %+v", r)
				}
			} else if successes != 1 || failures != 0 {
				t.Fatalf("callbacks success=%d failure=%d", successes, failures)
			}
			if mode == "progress" {
				if r := f.run(t); r.EffectPhase != taskstore.BackgroundRunEffectCloneObserved {
					t.Fatalf("scan did not commit: %+v", r)
				}
			}
		})
	}
}

func TestRunOnceDeadlineAndExpiredClaimBeforeEffect(t *testing.T) {
	for _, expiry := range []string{"attempt", "claim"} {
		t.Run(expiry, func(t *testing.T) {
			f := newScanFixture(t)
			if expiry == "attempt" {
				f.params.Deadline = f.now.Add(time.Second)
			}
			f.admit(t)
			calls := 0
			f.c.config.Now = func() time.Time {
				calls++
				if calls == 2 {
					if expiry == "attempt" {
						f.now = f.params.Deadline
					} else {
						f.now = f.now.Add(f.c.config.LeaseDuration)
					}
				}
				return f.now
			}
			err := f.c.RunOnce(context.Background())
			r := f.run(t)
			if expiry == "attempt" {
				if err != nil || r.TimeoutRequestedAt == nil || r.EffectPhase != taskstore.BackgroundRunEffectStopIntent {
					t.Fatalf("timeout: %+v %v", r, err)
				}
			} else if !errors.Is(err, context.DeadlineExceeded) || r.EffectPhase != taskstore.BackgroundRunEffectProvisionIntent {
				t.Fatalf("claim: %+v %v", r, err)
			}
			if len(f.d.calls) != 0 {
				t.Fatal(f.d.calls)
			}
			if _, err := os.Stat(filepath.Join(f.root, "background-runs", r.CloneIdentity)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("clone effect occurred: %v", err)
			}
			if expiry == "claim" {
				f.c.config.WorkerID = "takeover"
				f.scan(t, taskstore.BackgroundRunEffectCloneObserved)
				if recovered := f.run(t); recovered.ClaimGeneration <= r.ClaimGeneration {
					t.Fatal("expired claim was not fenced by a new generation")
				}
			}
		})
	}
}

func TestRunOnceExecutionMismatchSelectsCleanupWithoutProvisioning(t *testing.T) {
	f := newScanFixture(t)
	f.admit(t)
	f.c.config.Model = "different-model"
	if err := f.c.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	r := f.run(t)
	if r.State != taskstore.BackgroundRunCleanupRequired || r.ClaimOwner != "" {
		t.Fatalf("mismatch not durable: %+v", r)
	}
	if len(f.d.calls) != 0 {
		t.Fatalf("mismatch performed Docker effects: %v", f.d.calls)
	}
	if _, err := os.Stat(filepath.Join(f.root, "background-runs", r.CloneIdentity)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("mismatch created clone: %v", err)
	}
	f.scan(t, taskstore.BackgroundRunEffectWriterInactive)
}

func TestRunOnceCancellationAfterClaimLeavesRecoverableIntent(t *testing.T) {
	f := newScanFixture(t)
	f.admit(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clocks := 0
	f.c.config.Now = func() time.Time {
		clocks++
		if clocks == 2 {
			cancel()
		}
		return f.now
	}
	if err := f.c.RunOnce(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled claimed scan: %v", err)
	}
	r := f.run(t)
	if r.EffectPhase != taskstore.BackgroundRunEffectProvisionIntent || r.ClaimOwner != "worker" {
		t.Fatalf("lost recovery intent: %+v", r)
	}
	if len(f.d.calls) != 0 {
		t.Fatalf("canceled scan called Docker: %v", f.d.calls)
	}
	f.now = f.now.Add(2 * time.Minute)
	f.c.config.WorkerID = "replacement"
	f.scan(t, taskstore.BackgroundRunEffectCloneObserved)
}

func TestRunOnceVolumeFailureAndLostObservationRecover(t *testing.T) {
	for _, mode := range []string{"effect failure", "observation canceled"} {
		t.Run(mode, func(t *testing.T) {
			f := newScanFixture(t)
			f.admit(t)
			f.scan(t, taskstore.BackgroundRunEffectCloneObserved)
			f.d.calls = nil
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			failure := errors.New("Docker unavailable")
			if mode == "effect failure" {
				f.d.inspectErr = failure
			} else {
				f.d.afterVolumeRead = cancel
				failure = context.Canceled
			}
			err := f.c.RunOnce(ctx)
			if !errors.Is(err, failure) {
				t.Fatalf("error=%v want %v", err, failure)
			}
			r := f.run(t)
			if r.EffectPhase != taskstore.BackgroundRunEffectCloneObserved {
				t.Fatalf("advanced after failure: %+v", r)
			}
			if mode == "effect failure" && r.ClaimOwner != "" {
				t.Fatal("transient failure did not release claim")
			}
			if mode == "observation canceled" && (f.d.item == nil || f.d.creates != 1) {
				t.Fatal("volume effect did not succeed")
			}
			for _, call := range f.d.calls {
				if strings.HasPrefix(call, "container.") {
					t.Fatalf("premature next operation: %v", f.d.calls)
				}
			}
			f.d.inspectErr = nil
			f.d.afterVolumeRead = nil
			f.c.config.WorkerID = "replacement"
			f.now = f.now.Add(2 * time.Minute)
			f.scan(t, taskstore.BackgroundRunEffectVolumeObserved)
			if f.d.creates != 1 {
				t.Fatalf("replay created %d volumes", f.d.creates)
			}
		})
	}
}

func TestRunOnceStopCleanupFailureRecoveryPastDeadline(t *testing.T) {
	f := newScanFixture(t)
	f.admit(t)
	f.scan(t, taskstore.BackgroundRunEffectCloneObserved)
	f.scan(t, taskstore.BackgroundRunEffectVolumeObserved)
	stopClaim := f.params.Claim
	stopClaim.Scope.CommandKind = taskstore.StopBackgroundRunCommand
	stopClaim.Key = "stop"
	stopClaim.RequestHash = sha256.Sum256([]byte("stop"))
	_, err := f.c.store.StopBackgroundRun(context.Background(), taskstore.StopBackgroundRunParams{WorkspaceID: f.c.config.WorkspaceID, TaskID: f.params.TaskID, ReceiptID: "rcp_0198d34d-6a50-75fb-b1f2-000000000010", AttemptEventID: "fev_0198d34d-6a50-75fb-b1f2-000000000011", TaskEventID: "fev_0198d34d-6a50-75fb-b1f2-000000000012", Claim: stopClaim, APIContractVersion: "v1", StoppedAt: f.now})
	if err != nil {
		t.Fatal(err)
	}
	f.now = f.params.Deadline.Add(time.Minute)
	// Cleanup must ignore rotated execution configuration and the expired attempt.
	f.c.config.Model = "rotated"
	f.c.config.ImageIdentity = "sha256:" + strings.Repeat("c", 64)
	failure := errors.New("Docker cleanup unavailable")
	f.d.containerErr = failure
	if err = f.c.RunOnce(context.Background()); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	r := f.run(t)
	if r.EffectPhase != taskstore.BackgroundRunEffectStopIntent || r.State != taskstore.BackgroundRunCleanupRequired || r.ClaimOwner != "" {
		t.Fatalf("not recoverable: %+v", r)
	}
	if f.d.item == nil {
		t.Fatal("volume removed before writer proof")
	}
	f.d.containerErr = nil
	f.c.config.WorkerID = "recovery"
	for _, phase := range []taskstore.BackgroundRunEffectPhase{taskstore.BackgroundRunEffectWriterInactive, taskstore.BackgroundRunEffectRouteRemoved, taskstore.BackgroundRunEffectContainerRemoved, taskstore.BackgroundRunEffectVolumeRemoved, taskstore.BackgroundRunEffectCloneRemoved} {
		f.scan(t, phase)
	}
	if err = f.c.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	r = f.run(t)
	if r.State != taskstore.BackgroundRunFailed || r.CancelEpoch != 1 || r.TimeoutRequestedAt != nil {
		t.Fatalf("terminal state: %+v", r)
	}
	if f.d.item != nil {
		t.Fatal("volume survived cleanup")
	}
	if _, err = os.Stat(filepath.Join(f.root, "background-runs", r.CloneIdentity)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("clone survived: %v", err)
	}
	if err = f.c.RunOnce(context.Background()); !errors.Is(err, ErrNoWork) {
		t.Fatal(err)
	}
}
