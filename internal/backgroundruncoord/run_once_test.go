package backgroundruncoord

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
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
	"github.com/nebler/fern/internal/backgroundopencode"
	"github.com/nebler/fern/internal/backgroundroute"
	"github.com/nebler/fern/internal/task"
	"github.com/nebler/fern/internal/taskenvdocker"
	"github.com/nebler/fern/internal/taskstore"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
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
	info            *container.InspectResponse
	afterStart      func()
	createErr       error
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
func (d *scanDocker) ContainerInspect(_ context.Context, identity string) (container.InspectResponse, error) {
	d.calls = append(d.calls, "container.inspect")
	if d.containerErr != nil {
		return container.InspectResponse{}, d.containerErr
	}
	if d.info == nil || (identity != d.info.ID && identity != strings.TrimPrefix(d.info.Name, "/")) {
		return container.InspectResponse{}, errdefs.NotFound(errors.New("absent"))
	}
	return *d.info, nil
}
func (d *scanDocker) ContainerList(_ context.Context, options container.ListOptions) ([]container.Summary, error) {
	d.calls = append(d.calls, "container.list")
	if d.info == nil || !options.Filters.MatchKVList("label", d.info.Config.Labels) {
		return nil, nil
	}
	return []container.Summary{{ID: d.info.ID, Names: []string{d.info.Name}, Labels: d.info.Config.Labels, State: d.info.State.Status}}, nil
}

const scanContainerID = "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"

func (d *scanDocker) ContainerCreate(_ context.Context, c *container.Config, h *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, name string) (container.CreateResponse, error) {
	d.calls = append(d.calls, "container.create")
	if d.createErr != nil {
		return container.CreateResponse{}, d.createErr
	}
	c.Hostname = scanContainerID[:12]
	d.info = &container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{ID: scanContainerID, Name: "/" + name, Image: c.Image, HostConfig: h, State: &container.State{Status: "created"}},
		Config:            c,
		Mounts: []container.MountPoint{
			{Type: mount.TypeBind, Source: h.Mounts[0].Source, Destination: h.Mounts[0].Target, RW: true, Propagation: mount.PropagationRPrivate},
			{Type: mount.TypeVolume, Name: h.Mounts[1].Source, Source: "/daemon/volume", Destination: h.Mounts[1].Target, Driver: "local", RW: true},
		},
		NetworkSettings: &container.NetworkSettings{NetworkSettingsBase: container.NetworkSettingsBase{Ports: nat.PortMap{}}, Networks: map[string]*network.EndpointSettings{"bridge": {}}},
	}
	return container.CreateResponse{ID: scanContainerID}, nil
}
func (d *scanDocker) ContainerStart(context.Context, string, container.StartOptions) error {
	d.calls = append(d.calls, "container.start")
	d.info.State = &container.State{Status: "running", Running: true, StartedAt: "2026-08-31T12:00:00.123456789Z"}
	d.info.NetworkSettings.Ports = nat.PortMap{"4096/tcp": []nat.PortBinding{{HostIP: "127.0.0.1", HostPort: "49152"}}}
	if d.afterStart != nil {
		d.afterStart()
	}
	return nil
}
func (d *scanDocker) ContainerStop(context.Context, string, container.StopOptions) error {
	d.calls = append(d.calls, "container.stop")
	d.info.State.Running = false
	d.info.State.Status = "exited"
	return nil
}
func (d *scanDocker) ContainerRemove(context.Context, string, container.RemoveOptions) error {
	d.calls = append(d.calls, "container.remove")
	d.info = nil
	return nil
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
	provider, err := taskenvdocker.New(context.Background(), taskenvdocker.Config{StateRoot: f.root, Repository: repo, GitExecutable: git, ImageReference: "fern/test:dev", ImageID: scanImage, MemoryBytes: 512 << 20, WallTimeout: time.Hour, GitTimeout: 30 * time.Second, DockerTimeout: 10 * time.Second, HealthTimeout: 300 * time.Millisecond, GitOutputBytes: 1 << 20, SourceSizeAdmissionBytes: 64 << 20, CloneObservedLimitBytes: 64 << 20, DiskFreeAdmissionBytes: 64 << 20, LogMaxSize: "1m", LogMaxFiles: 3, StopGrace: time.Second}, f.d)
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
	f.c, err = New(store, provider, unusedArtifact{}, ids, Config{WorkspaceID: workspace, SystemActor: task.ActorSnapshot{Type: task.ActorSystem, ID: "coordinator", DisplayName: "Coordinator", CredentialID: "service", Authentication: "internal", RequestID: "scan"}, Profile: taskstore.BackgroundRunSourceProfile, ImageIdentity: scanImage, EnvironmentSHA256: f.params.BackgroundRun.EnvironmentSHA256, Agent: "build", ModelProvider: "provider", Model: "model", OperationTimeout: 30 * time.Second, PollInterval: time.Hour, Now: func() time.Time { return f.now }, HTTPClient: &http.Client{Timeout: time.Second}, Route: route, HistoryBounds: backgroundopencode.HistoryBounds{PageLimit: 10, MaxPages: 10, MaxEvents: 100}})
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
			failure := errors.New("Docker unavailable")
			switch mode {
			case "progress":
				f.admitStopped(t)
			case "transient failure":
				f.admit(t)
				f.d.containerErr = failure
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
				if r := f.run(t); r.EffectPhase != taskstore.BackgroundRunEffectProvisioning || r.ObservedContainerID != "" {
					t.Fatalf("failure not recoverable: %+v", r)
				}
			} else if successes != 1 || failures != 0 {
				t.Fatalf("callbacks success=%d failure=%d", successes, failures)
			}
			if mode == "progress" {
				if r := f.run(t); r.EffectPhase == taskstore.BackgroundRunEffectProvisioning {
					t.Fatalf("scan did not commit: %+v", r)
				}
			}
		})
	}
}

// admitStopped admits a run, starts provisioning without effects, and stops it,
// leaving cleanup work that needs no Docker resources.
func (f *scanFixture) admitStopped(t *testing.T) {
	t.Helper()
	f.admit(t)
	queued := f.run(t)
	if _, err := f.c.store.StartBackgroundRunProvisioning(context.Background(), ref(queued, f.now)); err != nil {
		t.Fatal(err)
	}
	f.stop(t)
}

func TestRunAdvancesSeveralPhasesWithoutWaitingForTicks(t *testing.T) {
	f := newScanFixture(t) // PollInterval is one hour.
	f.admitStopped(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	f.c.config.OnSuccess = func() {
		if f.run(t).State == taskstore.BackgroundRunFailed {
			cancel()
		}
	}
	f.c.config.OnError = func(err error) { t.Errorf("unexpected scan error: %v", err); cancel() }
	if err := f.c.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run: %v", err)
	}
	if r := f.run(t); r.State != taskstore.BackgroundRunFailed || r.EffectPhase != taskstore.BackgroundRunEffectCleanupComplete {
		t.Fatalf("cleanup did not finish without ticks: %s/%s", r.State, r.EffectPhase)
	}
}

func TestRunOnceAttemptDeadlineBeforeEffect(t *testing.T) {
	f := newScanFixture(t)
	f.params.Deadline = f.now.Add(time.Second)
	f.admit(t)
	calls := 0
	f.c.config.Now = func() time.Time {
		calls++
		if calls == 2 {
			f.now = f.params.Deadline
		}
		return f.now
	}
	err := f.c.RunOnce(context.Background())
	r := f.run(t)
	if err != nil || r.TimeoutRequestedAt == nil || r.EffectPhase != taskstore.BackgroundRunEffectCleaning {
		t.Fatalf("timeout: %+v %v", r, err)
	}
	if len(f.d.calls) != 0 {
		t.Fatal(f.d.calls)
	}
	if _, err := os.Stat(filepath.Join(f.root, "background-runs", r.CloneIdentity)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("clone effect occurred: %v", err)
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
	if r.State != taskstore.BackgroundRunCleanupRequired {
		t.Fatalf("mismatch not durable: %+v", r)
	}
	if len(f.d.calls) != 0 {
		t.Fatalf("mismatch performed Docker effects: %v", f.d.calls)
	}
	if _, err := os.Stat(filepath.Join(f.root, "background-runs", r.CloneIdentity)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("mismatch created clone: %v", err)
	}
	f.scan(t, taskstore.BackgroundRunEffectCleanupComplete)
}

func TestRunOnceCancellationAfterProvisioningStartLeavesRecoverableRun(t *testing.T) {
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
		t.Fatalf("canceled scan: %v", err)
	}
	r := f.run(t)
	if r.EffectPhase != taskstore.BackgroundRunEffectProvisioning {
		t.Fatalf("lost provisioning: %+v", r)
	}
	if len(f.d.calls) != 0 {
		t.Fatalf("canceled scan called Docker: %v", f.d.calls)
	}
}

// Provisioning keeps no per-resource record: a pass interrupted after any
// effect reconciles it by inspection on the next pass instead of repeating it.
func TestRunOnceProvisioningFailureAndLostObservationReconcile(t *testing.T) {
	for _, mode := range []string{"effect failure", "observation canceled"} {
		t.Run(mode, func(t *testing.T) {
			f := newScanFixture(t)
			f.admit(t)
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
			if r.EffectPhase != taskstore.BackgroundRunEffectProvisioning || r.ObservedContainerID != "" {
				t.Fatalf("advanced after failure: %+v", r)
			}
			if mode == "observation canceled" && (f.d.item == nil || f.d.creates != 1) {
				t.Fatal("volume effect did not succeed")
			}
			for _, call := range f.d.calls {
				if call == "container.create" || call == "container.start" {
					t.Fatalf("premature next operation: %v", f.d.calls)
				}
			}
			f.d.inspectErr = nil
			f.d.afterVolumeRead = nil
			// The next pass reaches the started runtime; health has no server.
			if err := f.c.RunOnce(context.Background()); err == nil {
				t.Fatal("health succeeded without an OpenCode server")
			}
			r = f.run(t)
			if r.EffectPhase != taskstore.BackgroundRunEffectProvisioning || r.ObservedContainerID != scanContainerID || f.d.creates != 1 {
				t.Fatalf("replay run=%+v volume creates=%d", r, f.d.creates)
			}
		})
	}
}

func (f *scanFixture) stop(t *testing.T) {
	t.Helper()
	stopClaim := f.params.Claim
	stopClaim.Scope.CommandKind = taskstore.StopBackgroundRunCommand
	stopClaim.Key = "stop"
	stopClaim.RequestHash = sha256.Sum256([]byte("stop"))
	if _, err := f.c.store.StopBackgroundRun(context.Background(), taskstore.StopBackgroundRunParams{WorkspaceID: f.c.config.WorkspaceID, TaskID: f.params.TaskID, ReceiptID: "rcp_0198d34d-6a50-75fb-b1f2-000000000010", AttemptEventID: "fev_0198d34d-6a50-75fb-b1f2-000000000011", TaskEventID: "fev_0198d34d-6a50-75fb-b1f2-000000000012", Claim: stopClaim, APIContractVersion: "v1", StoppedAt: f.now}); err != nil {
		t.Fatal(err)
	}
}

// A started container whose runtime was never recorded (a stop committed
// between start and record, or Fern crashed there and the container exited)
// must still be cleaned up instead of leaving the run active forever.
func TestRunOnceCleansUpStartedContainerWithUnrecordedRuntime(t *testing.T) {
	for _, mode := range []string{"stop races record", "crash then exit"} {
		t.Run(mode, func(t *testing.T) {
			f := newScanFixture(t)
			f.admit(t)
			if mode == "stop races record" {
				f.d.afterStart = func() { f.stop(t) }
				if err := f.c.RunOnce(context.Background()); err == nil {
					t.Fatal("runtime record succeeded after a committed stop")
				}
			} else {
				queued := f.run(t)
				run, err := f.c.store.StartBackgroundRunProvisioning(context.Background(), ref(queued, f.now))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.c.provider.Provision(context.Background(), run); err != nil {
					t.Fatal(err)
				}
				f.d.info.State.Running = false
				f.d.info.State.Status = "exited"
				if err := f.c.RunOnce(context.Background()); !errors.Is(err, taskenvdocker.ErrRuntimeExited) {
					t.Fatalf("exited container error=%v", err)
				}
			}
			r := f.run(t)
			if (r.State != taskstore.BackgroundRunCleanupRequired && r.State != taskstore.BackgroundRunCanceling) || r.EffectPhase != taskstore.BackgroundRunEffectCleaning || r.ObservedContainerID != "" {
				t.Fatalf("run not in cleanup with unrecorded runtime: %+v", r)
			}
			f.now = f.now.Add(2 * time.Minute)
			f.scan(t, taskstore.BackgroundRunEffectCleanupComplete)
			if r := f.run(t); r.State != taskstore.BackgroundRunFailed {
				t.Fatalf("terminal state: %+v", r)
			}
			if f.d.info != nil || f.d.item != nil {
				t.Fatal("container or volume survived cleanup")
			}
		})
	}
}

func TestRunOnceStopCleanupFailureRecoveryPastDeadline(t *testing.T) {
	f := newScanFixture(t)
	f.admit(t)
	// Clone and volume exist; container creation fails.
	createErr := errors.New("Docker container create unavailable")
	f.d.createErr = createErr
	if err := f.c.RunOnce(context.Background()); !errors.Is(err, createErr) {
		t.Fatal(err)
	}
	f.d.createErr = nil
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
	if r.EffectPhase != taskstore.BackgroundRunEffectCleaning || r.State != taskstore.BackgroundRunCleanupRequired {
		t.Fatalf("not recoverable: %+v", r)
	}
	if f.d.item == nil {
		t.Fatal("volume removed before writer proof")
	}
	f.d.containerErr = nil
	f.scan(t, taskstore.BackgroundRunEffectCleanupComplete)
	r = f.run(t)
	if r.State != taskstore.BackgroundRunFailed || r.StopReceiptID == "" || r.TimeoutRequestedAt != nil {
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

type blockingTransport struct {
	entered chan struct{}
	release chan struct{}
}

// RoundTrip ignores request cancellation, like a wedged upstream connection.
func (b blockingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	close(b.entered)
	<-b.release
	return nil, errors.New("released")
}

func TestCleanupRequiredBoundsRouteDrain(t *testing.T) {
	f := newScanFixture(t)
	f.admit(t)
	run := f.run(t)
	started := "2026-08-31T12:00:00.123456789Z"
	parsed, err := time.Parse(time.RFC3339Nano, started)
	if err != nil {
		t.Fatal(err)
	}
	run.ObservedContainerID, run.ObservedContainerStartedAt, run.RuntimeEpoch = strings.Repeat("a", 64), started, parsed.UnixNano()
	identity, err := f.c.validatedRouteIdentity(run)
	if err != nil {
		t.Fatal(err)
	}
	transport := blockingTransport{entered: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(func() { close(transport.release) })
	target, err := backgroundroute.NewTarget("http://127.0.0.1:9", transport)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.c.config.Route.Activate(identity, target); err != nil {
		t.Fatal(err)
	}
	attachment, active, err := f.c.config.Route.IssueAttachment(run)
	if err != nil || !active {
		t.Fatalf("attachment active=%t error=%v", active, err)
	}
	request := httptest.NewRequest(http.MethodGet, "https://fern.example/api/health", nil)
	request.SetBasicAuth(backgroundroute.AttachmentUsername, attachment.Password)
	go f.c.config.Route.ServeHTTP(httptest.NewRecorder(), request)
	<-transport.entered

	f.now = time.Now().UTC()
	f.c.config.OperationTimeout = 200 * time.Millisecond
	done := make(chan error, 1)
	go func() {
		done <- f.c.cleanupRequired(context.Background(), taskstore.BackgroundRunWork{Run: run}, "test cleanup")
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("cleanupRequired error=%v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("route removal was not bounded by the effect context")
	}
}
