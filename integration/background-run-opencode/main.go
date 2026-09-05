package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	"github.com/nebler/fern/internal/backgroundopencode"
	"github.com/nebler/fern/internal/backgroundroute"
	"github.com/nebler/fern/internal/backgroundruncoord"
	"github.com/nebler/fern/internal/control"
	"github.com/nebler/fern/internal/task"
	"github.com/nebler/fern/internal/taskartifact"
	"github.com/nebler/fern/internal/taskenvdocker"
	"github.com/nebler/fern/internal/taskresultsource"
	"github.com/nebler/fern/internal/taskstore"
)

const (
	imageTag     = "fern/opencode-background-source:dev"
	providerPort = nat.Port("4100/tcp")
)

func integrationArtifactEngine(root string) (*taskartifact.Engine, error) {
	cas, work := filepath.Join(root, "cas"), filepath.Join(root, "work")
	for _, path := range []string{root, cas, work} {
		if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, err
		}
	}
	git, err := exec.LookPath("git")
	if err != nil {
		return nil, err
	}
	git, err = filepath.EvalSymlinks(git)
	if err != nil {
		return nil, err
	}
	return taskartifact.New(taskartifact.Config{GitExecutable: git, CASRoot: cas, WorkRoot: work, CommandTimeout: 30 * time.Second})
}

type providerStats struct {
	Calls       int               `json:"calls"`
	Disconnects int               `json:"disconnects"`
	Requests    []json.RawMessage `json:"requests"`
}

type lostResponseTransport struct {
	base  http.RoundTripper
	path  string
	calls atomic.Int32
	lost  atomic.Int32
}

type countingPromptTransport struct {
	base  http.RoundTripper
	path  string
	calls atomic.Int32
}

func (t *countingPromptTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.Method == http.MethodPost && request.URL.Path == t.path {
		t.calls.Add(1)
	}
	return t.base.RoundTrip(request)
}

func (t *lostResponseTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := t.base.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	if request.Method != http.MethodPost || request.URL.Path != t.path {
		return response, nil
	}
	t.calls.Add(1)
	if !t.lost.CompareAndSwap(0, 1) {
		return response, nil
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
	_ = response.Body.Close()
	return nil, errors.New("injected response loss with sensitive details that must not escape")
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "background-run-opencode:", err)
		os.Exit(1)
	}
}

func run() (resultErr error) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	imageID := os.Getenv("FERN_OPENCODE_BACKGROUND_SOURCE_IMAGE_ID")
	if imageID == "" {
		return errors.New("FERN_OPENCODE_BACKGROUND_SOURCE_IMAGE_ID is required; run integration/background-run-qualification/run.sh or export the exact local image ID")
	}
	if !canonicalImageID(imageID) {
		return errors.New("FERN_OPENCODE_BACKGROUND_SOURCE_IMAGE_ID must be canonical sha256:<64 lowercase hex>")
	}
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return err
	}
	defer cli.Close()
	image, err := cli.ImageInspect(ctx, imageTag)
	if err != nil || image.ID != imageID {
		return fmt.Errorf("operator-pinned image mismatch: inspect=%v local=%q want=%q", err, image.ID, imageID)
	}

	temporary, err := os.MkdirTemp("", "fern-background-run-opencode-")
	if err != nil {
		return err
	}
	removeRoot := temporary
	defer func() {
		resultErr = errors.Join(resultErr, os.RemoveAll(removeRoot))
		if _, err := os.Lstat(removeRoot); !errors.Is(err, os.ErrNotExist) {
			resultErr = errors.Join(resultErr, fmt.Errorf("temporary root residue: %v", err))
		}
	}()
	temporary, err = filepath.EvalSymlinks(temporary)
	if err != nil {
		return err
	}

	providerName := "fern-background-opencode-provider-" + filepath.Base(temporary)
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()
		err := cli.ContainerRemove(cleanupCtx, providerName, container.RemoveOptions{Force: true})
		if err != nil && !client.IsErrNotFound(err) {
			resultErr = errors.Join(resultErr, fmt.Errorf("remove fake provider: %w", err))
		}
		if _, err := cli.ContainerInspect(cleanupCtx, providerName); !client.IsErrNotFound(err) {
			resultErr = errors.Join(resultErr, fmt.Errorf("fake provider residue: %v", err))
		}
	}()
	_, providerEndpoint, providerIP, err := startProvider(ctx, cli, providerName, imageID)
	if err != nil {
		return err
	}
	if err := waitFor(ctx, "fake provider readiness", func() (bool, error) {
		_, err := readStats(providerEndpoint)
		return err == nil, nil
	}); err != nil {
		return err
	}

	state := filepath.Join(temporary, "state")
	repository := filepath.Join(temporary, "repository")
	if err := os.Mkdir(state, 0o700); err != nil {
		return err
	}
	if err := os.Mkdir(repository, 0o700); err != nil {
		return err
	}
	gitPath, err := exec.LookPath("git")
	if err != nil {
		return err
	}
	gitPath, err = filepath.EvalSymlinks(gitPath)
	if err != nil {
		return err
	}
	if err := initializeRepository(repository, gitPath, providerIP); err != nil {
		return err
	}
	base, err := gitOutput(repository, gitPath, "rev-parse", "HEAD")
	if err != nil {
		return err
	}

	ids := task.NewSecureGenerator()
	workspaceID, err := ids.WorkspaceID()
	if err != nil {
		return err
	}
	taskID, err := ids.TaskID()
	if err != nil {
		return err
	}
	attemptID, err := ids.AttemptID()
	if err != nil {
		return err
	}
	sessionID, err := ids.OpenCodeSessionID()
	if err != nil {
		return err
	}
	messageID, err := ids.OpenCodeMessageID()
	if err != nil {
		return err
	}
	probeSessionID, err := ids.OpenCodeSessionID()
	if err != nil {
		return err
	}
	probeMessageID, err := ids.OpenCodeMessageID()
	if err != nil {
		return err
	}
	compact := strings.ReplaceAll(strings.TrimPrefix(string(taskID), "tsk_"), "-", "")
	run := taskstore.BackgroundRun{
		WorkspaceID: workspaceID, TaskID: taskID, AttemptID: attemptID, Generation: 1,
		RepositoryRemote: "https://github.com/fern-integration/background-run", BaseOID: task.GitOID(base),
		Profile: taskstore.BackgroundRunSourceProfile, EnvironmentSHA256: taskenvdocker.EnvironmentSHA256(nil), ResourceSpecVersion: 9, ImageIdentity: imageID,
		CloneIdentity: "run-" + compact + "-g1-clone", VolumeIdentity: "fern-run-" + compact + "-g1-opencode",
		ContainerIdentity: "fern-run-" + compact + "-g1", EndpointIdentity: "run-" + compact + "-g1-endpoint",
		OpenCodeSessionID: sessionID, OpenCodeMessageID: messageID,
	}
	config := taskenvdocker.Config{
		StateRoot: state, Repository: repository, GitExecutable: gitPath, ImageReference: imageTag, ImageID: imageID,
		MemoryBytes: 512 << 20, NanoCPUs: 2_000_000_000, PIDs: 512, WallTimeout: 3 * time.Minute,
		GitTimeout: 30 * time.Second, DockerTimeout: 20 * time.Second, HealthTimeout: 60 * time.Second,
		GitOutputBytes: 1 << 20, SourceSizeAdmissionBytes: 128 << 20, CloneObservedLimitBytes: 128 << 20,
		DiskFreeAdmissionBytes: 128 << 20, LogMaxSize: "1m", LogMaxFiles: 2, StopGrace: 3 * time.Second,
	}
	var containerID string
	var runtime taskenvdocker.RuntimeIdentity
	clonePath := filepath.Join(state, "background-runs", run.CloneIdentity)
	defer func() {
		resultErr = errors.Join(resultErr, cleanupOpenCode(nil, cli, run, containerID, runtime, clonePath))
	}()
	provider, err := taskenvdocker.New(ctx, config, nil)
	if err != nil {
		return err
	}
	initialProvider := provider
	initialProviderClosed := false
	defer func() {
		if !initialProviderClosed {
			resultErr = errors.Join(resultErr, initialProvider.Close())
		}
	}()

	if _, err := provider.EnsureClone(ctx, run); err != nil {
		return err
	}
	if _, err := provider.EnsureVolume(ctx, run); err != nil {
		return err
	}
	created, err := provider.EnsureContainer(ctx, run)
	if err != nil {
		return err
	}
	containerID = created.ContainerID
	started, err := provider.StartContainer(ctx, run, created.ContainerID)
	if err != nil {
		return err
	}
	runtime = started.RuntimeIdentity()
	if _, err := provider.Health(ctx, run, runtime); err != nil {
		return err
	}
	username, password, err := runtimeCredentials(ctx, cli, created.ContainerID)
	if err != nil {
		return err
	}
	session := backgroundopencode.SessionSpec{ID: string(sessionID), Agent: "contract", ProviderID: "test", ModelID: "test-model", Directory: "/home/user/workspace"}
	sessionTransport := http.DefaultTransport.(*http.Transport).Clone()
	sessionLoss := &lostResponseTransport{base: sessionTransport, path: "/api/session"}
	lostSessionClient, err := backgroundopencode.New(backgroundopencode.Config{
		Endpoint: started.Endpoint, Username: username, Password: password,
		HTTPClient: &http.Client{Timeout: 10 * time.Second, Transport: sessionLoss},
	})
	if err != nil {
		return err
	}
	if err := lostSessionClient.CreateSessionOnce(ctx, session); !errors.Is(err, backgroundopencode.ErrTransport) {
		return fmt.Errorf("lost session response was not transport ambiguity: %v", err)
	}
	if sessionLoss.calls.Load() != 1 || sessionLoss.lost.Load() != 1 {
		return fmt.Errorf("lost session response calls=%d lost=%d", sessionLoss.calls.Load(), sessionLoss.lost.Load())
	}
	sessionTransport.CloseIdleConnections()
	if err := provider.Close(); err != nil {
		return err
	}
	initialProviderClosed = true
	sessionProvider, err := taskenvdocker.New(ctx, config, nil)
	if err != nil {
		return err
	}
	provider = sessionProvider
	sessionProviderClosed := false
	defer func() {
		if !sessionProviderClosed {
			resultErr = errors.Join(resultErr, sessionProvider.Close())
		}
	}()
	if _, err := provider.Health(ctx, run, runtime); err != nil {
		return fmt.Errorf("session reconstructed provider health: %w", err)
	}
	oc, err := backgroundopencode.New(backgroundopencode.Config{
		Endpoint: started.Endpoint, Username: username, Password: password,
		HTTPClient: &http.Client{Timeout: 10 * time.Second},
	})
	if err != nil {
		return err
	}
	if state, err := oc.ReconcileSession(ctx, session); err != nil {
		return fmt.Errorf("lost-response session reconciliation: %w", err)
	} else if state != backgroundopencode.ReconcileExact {
		return fmt.Errorf("lost-response session reconciliation state=%s", state)
	}
	probeSession := backgroundopencode.SessionSpec{ID: string(probeSessionID), Agent: "contract", ProviderID: "test", ModelID: "test-model", Directory: "/home/user/workspace"}
	if err := oc.CreateSessionOnce(ctx, probeSession); err != nil {
		return fmt.Errorf("create admitted-only probe: %w", err)
	}
	probePrompt := backgroundopencode.PromptSpec{ID: string(probeMessageID), Text: "FERN_BACKGROUND_ADMITTED_ONLY", Delivery: "steer", Resume: false}
	if err := oc.AdmitPromptOnce(ctx, string(probeSessionID), probePrompt); err != nil {
		return fmt.Errorf("admit non-resuming probe: %w", err)
	}
	resumeProbe := probePrompt
	resumeProbe.Resume = true
	if state, err := oc.ReconcilePrompt(ctx, string(probeSessionID), resumeProbe, backgroundopencode.HistoryBounds{PageLimit: 2, MaxPages: 100, MaxEvents: 1000}); err != nil || state != backgroundopencode.ReconcileAdmitted {
		return fmt.Errorf("admitted-only reconciliation state=%s error=%v", state, err)
	}
	if state, err := oc.ReconcilePrompt(ctx, string(probeSessionID), probePrompt, backgroundopencode.HistoryBounds{PageLimit: 2, MaxPages: 100, MaxEvents: 1000}); err != nil || state != backgroundopencode.ReconcileExact {
		return fmt.Errorf("resume=false reconciliation state=%s error=%v", state, err)
	}
	prompt := backgroundopencode.PromptSpec{ID: string(messageID), Text: "FERN_BACKGROUND_CLIENT_HANG", Delivery: "steer", Resume: true}
	baseTransport := http.DefaultTransport.(*http.Transport).Clone()
	loss := &lostResponseTransport{base: baseTransport, path: "/api/session/" + string(sessionID) + "/prompt"}
	lossyClient, err := backgroundopencode.New(backgroundopencode.Config{
		Endpoint: started.Endpoint, Username: username, Password: password,
		HTTPClient: &http.Client{Timeout: 10 * time.Second, Transport: loss},
	})
	if err != nil {
		return err
	}
	if err := lossyClient.AdmitPromptOnce(ctx, string(sessionID), prompt); !errors.Is(err, backgroundopencode.ErrTransport) {
		return fmt.Errorf("lost prompt response was not transport ambiguity: %v", err)
	}
	if loss.calls.Load() != 1 || loss.lost.Load() != 1 {
		return fmt.Errorf("lost-response mutation counts calls=%d lost=%d", loss.calls.Load(), loss.lost.Load())
	}
	baseTransport.CloseIdleConnections()

	if err := provider.Close(); err != nil {
		return err
	}
	sessionProviderClosed = true
	nextProvider, err := taskenvdocker.New(ctx, config, nil)
	if err != nil {
		return err
	}
	provider = nextProvider
	defer func(value *taskenvdocker.Provider) { resultErr = errors.Join(resultErr, value.Close()) }(nextProvider)
	if _, err := provider.Health(ctx, run, runtime); err != nil {
		return fmt.Errorf("reconstructed provider health: %w", err)
	}
	oc, err = backgroundopencode.New(backgroundopencode.Config{
		Endpoint: started.Endpoint, Username: username, Password: password,
		HTTPClient: &http.Client{Timeout: 10 * time.Second},
	})
	if err != nil {
		return err
	}
	if err := waitFor(ctx, "durable prompt admission", func() (bool, error) {
		state, err := oc.ReconcilePrompt(ctx, string(sessionID), prompt, backgroundopencode.HistoryBounds{PageLimit: 2, MaxPages: 100, MaxEvents: 1000})
		return state == backgroundopencode.ReconcileExact, err
	}); err != nil {
		return err
	}
	if err := waitFor(ctx, "positive active observation", func() (bool, error) {
		observation, err := oc.ObservePending(ctx, string(sessionID))
		return err == nil && observation.State == backgroundopencode.WorkWorking && observation.Active, err
	}); err != nil {
		return err
	}
	if err := waitFor(ctx, "one provider call", func() (bool, error) {
		stats, err := readStats(providerEndpoint)
		return err == nil && stats.Calls == 1, err
	}); err != nil {
		return err
	}
	stats, err := readStats(providerEndpoint)
	if err != nil {
		return fmt.Errorf("read provider calls after admission: %w", err)
	}
	if state, err := oc.ReconcileSession(ctx, session); err != nil {
		return fmt.Errorf("reconstructed client session: %w", err)
	} else if state != backgroundopencode.ReconcileExact {
		return fmt.Errorf("reconstructed client session state=%s", state)
	}
	if state, err := oc.ReconcilePrompt(ctx, string(sessionID), prompt, backgroundopencode.HistoryBounds{PageLimit: 1, MaxPages: 100, MaxEvents: 1000}); err != nil {
		return fmt.Errorf("reconstructed client prompt: %w", err)
	} else if state != backgroundopencode.ReconcileExact {
		return fmt.Errorf("reconstructed client prompt state=%s", state)
	}
	stats, err = readStats(providerEndpoint)
	if err != nil {
		return fmt.Errorf("read provider calls after reconstruction: %w", err)
	}
	if stats.Calls != 1 {
		return fmt.Errorf("read-only reconstruction replayed prompt; calls=%d", stats.Calls)
	}
	if len(stats.Requests) != 1 || !strings.Contains(string(stats.Requests[0]), prompt.Text) {
		return errors.New("provider did not record exactly one request containing the exact prompt marker")
	}
	disconnects := stats.Disconnects
	if err := oc.InterruptOnce(ctx, string(sessionID)); err != nil {
		return err
	}
	if err := waitFor(ctx, "interrupt inactivity", func() (bool, error) {
		observation, err := oc.ObservePending(ctx, string(sessionID))
		if err != nil || observation.Active || observation.State == backgroundopencode.WorkWorking {
			return false, err
		}
		stats, err := readStats(providerEndpoint)
		return err == nil && stats.Disconnects > disconnects && stats.Calls == 1, err
	}); err != nil {
		return err
	}
	if err := cleanupOpenCode(provider, cli, run, containerID, runtime, clonePath); err != nil {
		return err
	}
	containerID = ""
	runtime = taskenvdocker.RuntimeIdentity{}
	if err := runSerialCoordinator(ctx, temporary, repository, providerEndpoint, provider, cli, imageID, base); err != nil {
		return err
	}
	fmt.Printf("PASS profile=%s image_id=%s session=exact session_response_loss=after_effect session_posts=1 no_session_replay=true no_session_replacement=true prompt=admitted_promoted prompt_response_loss=after_effect active=positive interrupt=204 reconstruction=provider_client no_prompt_replay=true provider_calls=1 marker=exact serial_coordinator=complete retained_result=cas_reconstructed_twice route=paired_root_health open_replay=stable fence_crash=uncertain_zero_post runtime_restart=quarantined_then_operator_removed_cleanup\n", backgroundopencode.Profile, imageID)
	return nil
}

func canonicalImageID(value string) bool {
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	for _, character := range value[len("sha256:"):] {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func runSerialCoordinator(ctx context.Context, root, repository, providerEndpoint string, provider *taskenvdocker.Provider, cli *client.Client, imageID, base string) error {
	artifact, err := integrationArtifactEngine(filepath.Join(root, "serial-artifacts"))
	if err != nil {
		return err
	}
	defer artifact.Close()
	statsBefore, err := readStats(providerEndpoint)
	if err != nil {
		return err
	}
	ids := task.NewSecureGenerator()
	workspaceID, err := ids.WorkspaceID()
	if err != nil {
		return err
	}
	databasePath := filepath.Join(root, "serial-task-store.sqlite")
	store, err := taskstore.Open(ctx, databasePath)
	if err != nil {
		return err
	}
	routeDirectory := filepath.Join(root, "serial-control")
	if err := os.MkdirAll(routeDirectory, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(routeDirectory, 0o700); err != nil {
		return err
	}
	controlStore, err := control.Open(routeDirectory, "serial-background")
	if err != nil {
		return err
	}
	deviceToken := "serial-paired-device-token"
	deviceNow := time.Now()
	if _, err := controlStore.AddDevice(deviceToken, "Serial harness", deviceNow, deviceNow.Add(time.Hour)); err != nil {
		return err
	}
	route, routeURL, routeAddress, stopRoute, err := startSerialRoute(ctx, controlStore, "")
	if err != nil {
		return err
	}
	defer func() { _ = stopRoute() }()
	now := time.Now().UTC().Truncate(time.Millisecond)
	if _, err := store.EnsureWorkspace(ctx, taskstore.Workspace{
		ID: workspaceID, Name: "serial-background", State: taskstore.WorkspaceActive, RepositoryPath: repository,
		GitHubAuthority: taskstore.GitHubAuthorityAppBroker, InstallationID: 1, RepositoryID: 1,
		RepositoryFullName: "fern-integration/background-run", ImageDigest: imageID, OpenCodeProtocol: backgroundopencode.Profile,
		RuntimeDesiredState: "running", ReconciliationEpoch: 1, CreatedAt: now,
	}); err != nil {
		_ = store.Close()
		return err
	}
	generated, err := ids.GenerateAdmissionIDs()
	if err != nil {
		_ = store.Close()
		return err
	}
	prompt := "FERN_BACKGROUND_CLIENT_HANG"
	actor := task.ActorSnapshot{Type: task.ActorOpenCode, ID: "serial-plugin", DisplayName: "Serial integration",
		CredentialID: "serial-plugin", Authentication: "fern_plugin_bearer", RequestID: "serial-request"}
	requestHash := sha256.Sum256([]byte("serial-run-create"))
	compact := strings.ReplaceAll(strings.TrimPrefix(string(generated.TaskID), "tsk_"), "-", "")
	intent := &taskstore.BackgroundRunIntent{
		RepositoryRemote: "https://github.com/fern-integration/background-run", Branch: "main",
		InstructionSHA256: sha256.Sum256([]byte(prompt)), Profile: backgroundopencode.Profile,
		ProfileSHA256: sha256.Sum256([]byte(backgroundopencode.Profile)), EnvironmentSHA256: taskenvdocker.EnvironmentSHA256(nil), ImageIdentity: imageID,
		CloneIdentity: "run-" + compact + "-g1-clone", VolumeIdentity: "fern-run-" + compact + "-g1-opencode",
		ContainerIdentity: "fern-run-" + compact + "-g1", EndpointIdentity: "run-" + compact + "-g1-endpoint",
	}
	admission, err := store.AdmitBackgroundRun(ctx, taskstore.AdmitBackgroundRunParams{
		TaskID: generated.TaskID, AttemptID: generated.AttemptID, ReceiptID: generated.ReceiptID,
		TaskEventID: generated.TaskEventID, AttemptEventID: generated.AttemptEventID,
		OpenCodeSessionID: generated.OpenCodeSessionID, OpenCodeMessageID: generated.OpenCodeMessageID,
		Claim: task.IdempotencyClaim{Scope: task.IdempotencyScope{WorkspaceID: workspaceID, CommandKind: taskstore.CreateBackgroundRunCommand},
			Key: "serial-create", RequestHash: requestHash, Actor: actor},
		Title: "Serial Background Run", Prompt: prompt, RepositoryID: 1, BaseRef: "main", BaseSHA: task.GitOID(base),
		ObjectFormat: "sha1", ExecutionContractVersion: "fern.background-run.v1", Agent: "contract",
		ModelProvider: "test", Model: "test-model", BudgetSnapshot: json.RawMessage(`{"turns":10}`),
		Deadline: now.Add(3 * time.Minute), APIContractVersion: "fern.background-run.v1", AcceptedAt: now, BackgroundRun: intent,
	})
	if err != nil {
		_ = store.Close()
		return err
	}
	clonePath := filepath.Join(root, "state", "background-runs", intent.CloneIdentity)
	if admission.Task.ID == "" {
		_ = store.Close()
		return errors.New("serial admission did not return a durable run ID")
	}
	if _, err := os.Lstat(clonePath); !errors.Is(err, os.ErrNotExist) {
		_ = store.Close()
		return fmt.Errorf("serial effects preceded durable admission: %v", err)
	}
	baseTransport := http.DefaultTransport.(*http.Transport).Clone()
	loss := &lostResponseTransport{base: baseTransport, path: "/api/session/" + string(generated.OpenCodeSessionID) + "/prompt"}
	operationCtx, cancelOperation := context.WithCancel(ctx)
	config := backgroundruncoord.Config{
		WorkspaceID: workspaceID, WorkerID: "serial-worker-a",
		SystemActor: task.ActorSnapshot{Type: task.ActorSystem, ID: "serial-coordinator", DisplayName: "Serial coordinator",
			CredentialID: "service-v1", Authentication: "internal", RequestID: "serial-worker-a"},
		Profile: backgroundopencode.Profile, ImageIdentity: imageID, EnvironmentSHA256: taskenvdocker.EnvironmentSHA256(nil), Agent: "contract", ModelProvider: "test", Model: "test-model",
		OperationTimeout: 20 * time.Second, LeaseDuration: time.Minute, PollInterval: 100 * time.Millisecond,
		HistoryBounds: backgroundopencode.HistoryBounds{PageLimit: 2, MaxPages: 100, MaxEvents: 1000}, Now: time.Now,
		HTTPClient: &http.Client{Timeout: 10 * time.Second, Transport: loss}, AfterPromptCall: func(error) { cancelOperation() }, Route: route,
	}
	coordinator, err := backgroundruncoord.New(store, provider, artifact, ids, config)
	if err != nil {
		_ = store.Close()
		return err
	}
	for step := 0; step < 20; step++ {
		err = coordinator.RunOnce(operationCtx)
		run, readErr := store.GetBackgroundRun(context.Background(), workspaceID, admission.Task.ID, actor)
		if readErr != nil {
			_ = store.Close()
			return readErr
		}
		if run.PromptRequestAttemptedAt != nil {
			break
		}
		if err != nil {
			_ = store.Close()
			return err
		}
	}
	if loss.calls.Load() != 1 || loss.lost.Load() != 1 {
		_ = store.Close()
		return fmt.Errorf("serial lost response calls=%d lost=%d", loss.calls.Load(), loss.lost.Load())
	}
	if err := store.Close(); err != nil {
		return err
	}
	if err := stopRoute(); err != nil {
		return err
	}
	route, routeURL, _, stopRoute, err = startSerialRoute(ctx, controlStore, routeAddress)
	if err != nil {
		return err
	}
	if status, routeErr := serialRouteStatus(routeURL, deviceToken, "/api/health"); routeErr != nil || status != http.StatusNotFound {
		return fmt.Errorf("restarted unbound route status=%d error=%v", status, routeErr)
	}
	store, err = taskstore.Open(context.Background(), databasePath)
	if err != nil {
		return err
	}
	defer store.Close()
	config.WorkerID = "serial-worker-b"
	config.SystemActor.RequestID = config.WorkerID
	config.AfterPromptCall = nil
	config.Now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	config.Route = route
	coordinator, err = backgroundruncoord.New(store, provider, artifact, ids, config)
	if err != nil {
		return err
	}
	var current taskstore.BackgroundRun
	for step := 0; step < 10; step++ {
		if err := coordinator.RunOnce(context.Background()); err != nil && !errors.Is(err, backgroundruncoord.ErrNoWork) {
			return err
		}
		current, err = store.GetBackgroundRun(context.Background(), workspaceID, admission.Task.ID, actor)
		if err != nil {
			return err
		}
		if current.State == taskstore.BackgroundRunWorking {
			break
		}
	}
	if current.State != taskstore.BackgroundRunWorking || loss.calls.Load() != 1 {
		return fmt.Errorf("serial read-only recovery state=%s prompt_calls=%d", current.State, loss.calls.Load())
	}
	if err := verifySerialRoute(routeURL, deviceToken); err != nil {
		return err
	}
	if err := verifySerialOpenReplay(store, ids, current, actor, route, deviceToken); err != nil {
		return err
	}
	if err := waitFor(ctx, "serial provider call", func() (bool, error) {
		stats, readErr := readStats(providerEndpoint)
		return readErr == nil && stats.Calls == statsBefore.Calls+1, readErr
	}); err != nil {
		return err
	}
	statsAfter, err := readStats(providerEndpoint)
	if err != nil || statsAfter.Calls != statsBefore.Calls+1 {
		return fmt.Errorf("serial provider calls before=%d after=%d error=%v", statsBefore.Calls, statsAfter.Calls, err)
	}
	if err := coordinator.RunOnce(context.Background()); err != nil {
		return err
	}
	current, err = store.GetBackgroundRun(context.Background(), workspaceID, admission.Task.ID, actor)
	if err != nil || current.State != taskstore.BackgroundRunWorking || !strings.Contains(current.LastEvidence, "positive_active") {
		return fmt.Errorf("serial positive working observation state=%s evidence=%q error=%v", current.State, current.LastEvidence, err)
	}
	stopSeconds := 1
	if err := cli.ContainerStop(context.Background(), current.ObservedContainerID, container.StopOptions{Timeout: &stopSeconds}); err != nil {
		return fmt.Errorf("serial runtime replacement stop: %w", err)
	}
	if err := cli.ContainerStart(context.Background(), current.ObservedContainerID, container.StartOptions{}); err != nil {
		return fmt.Errorf("serial runtime replacement start: %w", err)
	}
	if status, routeErr := serialRouteStatus(routeURL, deviceToken, "/api/health"); routeErr != nil || status != http.StatusBadGateway {
		return fmt.Errorf("replacement reached route before reconciliation status=%d error=%v", status, routeErr)
	}
	observeErr := coordinator.RunOnce(context.Background())
	current, err = store.GetBackgroundRun(context.Background(), workspaceID, admission.Task.ID, actor)
	if !errors.Is(observeErr, taskenvdocker.ErrIdentityMismatch) || err != nil || current.State != taskstore.BackgroundRunCleanupRequired {
		return fmt.Errorf("serial runtime replacement state=%s observe_error=%v read_error=%v", current.State, observeErr, err)
	}
	if status, routeErr := serialRouteStatus(routeURL, deviceToken, "/api/health"); routeErr != nil || status != http.StatusNotFound {
		return fmt.Errorf("replacement inherited route status=%d error=%v", status, routeErr)
	}
	replacement, err := cli.ContainerInspect(context.Background(), current.ObservedContainerID)
	if err != nil || replacement.State == nil || !replacement.State.Running || replacement.State.StartedAt == current.ObservedContainerStartedAt {
		startedAt := ""
		if replacement.State != nil {
			startedAt = replacement.State.StartedAt
		}
		return fmt.Errorf("replacement was not quarantined intact: running=%v started=%q durable=%q error=%v",
			replacement.State != nil && replacement.State.Running, startedAt, current.ObservedContainerStartedAt, err)
	}
	if err := cli.ContainerStop(context.Background(), current.ObservedContainerID, container.StopOptions{Timeout: &stopSeconds}); err != nil {
		return fmt.Errorf("operator stop quarantined replacement: %w", err)
	}
	if err := cli.ContainerRemove(context.Background(), current.ObservedContainerID, container.RemoveOptions{}); err != nil {
		return fmt.Errorf("operator remove quarantined replacement: %w", err)
	}
	for step := 0; step < 20; step++ {
		if err := coordinator.RunOnce(context.Background()); err != nil && !errors.Is(err, backgroundruncoord.ErrNoWork) {
			return err
		}
		current, err = store.GetBackgroundRun(context.Background(), workspaceID, admission.Task.ID, actor)
		if err != nil {
			return err
		}
		if current.State == taskstore.BackgroundRunFailed {
			break
		}
	}
	if current.State != taskstore.BackgroundRunFailed || current.EffectPhase != taskstore.BackgroundRunEffectCleanupComplete || current.LastError != "runtime_unavailable" {
		return fmt.Errorf("serial terminal run=%s/%s reason=%s", current.State, current.EffectPhase, current.LastError)
	}
	if _, err := cli.ContainerInspect(context.Background(), intent.ContainerIdentity); !client.IsErrNotFound(err) {
		return fmt.Errorf("serial container residue: %v", err)
	}
	if _, err := cli.VolumeInspect(context.Background(), intent.VolumeIdentity); !client.IsErrNotFound(err) {
		return fmt.Errorf("serial volume residue: %v", err)
	}
	if _, err := os.Lstat(clonePath); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("serial clone residue: %v", err)
	}
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		return err
	}
	defer database.Close()
	var taskState, attemptState, taskReason, attemptReason string
	if err := database.QueryRow(`SELECT t.state,a.state,t.terminal_reason,a.terminal_reason FROM tasks t JOIN attempts a ON a.id=t.current_attempt_id WHERE t.id=?`, current.TaskID).
		Scan(&taskState, &attemptState, &taskReason, &attemptReason); err != nil || taskState != "failed" || attemptState != "failed" || taskReason != "runtime_unavailable" || attemptReason != taskReason {
		return fmt.Errorf("serial parent consistency task=%s attempt=%s reasons=%s/%s error=%v", taskState, attemptState, taskReason, attemptReason, err)
	}
	if err := runRetainedResultScenario(ctx, root, repository, store, provider, artifact, ids, workspaceID, imageID, base, route); err != nil {
		return err
	}
	if err := runPreDispatchFenceScenario(ctx, root, provider, artifact, cli, ids, workspaceID, imageID, base, route); err != nil {
		return err
	}
	labelled, err := cli.ContainerList(ctx, container.ListOptions{All: true, Filters: filters.NewArgs(
		filters.Arg("label", "dev.fern.background-run.managed=true"),
		filters.Arg("label", "dev.fern.background-run.workspace="+string(workspaceID)),
	)})
	if err != nil || len(labelled) != 0 {
		return fmt.Errorf("serial exact-label container residue count=%d error=%v", len(labelled), err)
	}
	backgroundRoot := filepath.Join(root, "state", "background-runs")
	if err := filepath.WalkDir(backgroundRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		name := entry.Name()
		if privateBackgroundResidue(name) {
			return fmt.Errorf("private background root residue %s", path)
		}
		return nil
	}); err != nil {
		return err
	}
	gitPath, err := exec.LookPath("git")
	if err != nil {
		return err
	}
	gitPath, err = filepath.EvalSymlinks(gitPath)
	if err != nil {
		return err
	}
	remote, err := gitOutput(repository, gitPath, "remote", "get-url", "origin")
	if err != nil || remote != "https://github.com/fern-integration/background-run" {
		return fmt.Errorf("configured source repository changed remote=%q error=%v", remote, err)
	}
	configuredHead, err := gitOutput(repository, gitPath, "rev-parse", "HEAD")
	if err != nil || configuredHead != base {
		return fmt.Errorf("configured source repository changed head=%q want=%q error=%v", configuredHead, base, err)
	}
	baseTransport.CloseIdleConnections()
	return nil
}

func runRetainedResultScenario(ctx context.Context, root, repository string, store *taskstore.Store, provider *taskenvdocker.Provider,
	artifact *taskartifact.Engine, ids *task.Generator, workspaceID task.WorkspaceID, imageID, base string, route *backgroundroute.Manager,
) error {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		return err
	}
	gitPath, err = filepath.EvalSymlinks(gitPath)
	if err != nil {
		return err
	}
	now := time.Now().UTC().Truncate(time.Millisecond).Add(3 * time.Minute)
	generated, err := ids.GenerateAdmissionIDs()
	if err != nil {
		return err
	}
	prompt := "FERN_BACKGROUND_CLIENT_HANG"
	actor := task.ActorSnapshot{Type: task.ActorOpenCode, ID: "retained-plugin", DisplayName: "Retained integration",
		CredentialID: "retained-plugin", Authentication: "fern_plugin_bearer", RequestID: "retained-request"}
	compact := strings.ReplaceAll(strings.TrimPrefix(string(generated.TaskID), "tsk_"), "-", "")
	intent := &taskstore.BackgroundRunIntent{
		RepositoryRemote: "https://github.com/fern-integration/background-run", Branch: "main",
		InstructionSHA256: sha256.Sum256([]byte(prompt)), Profile: backgroundopencode.Profile,
		ProfileSHA256: sha256.Sum256([]byte(backgroundopencode.Profile)), EnvironmentSHA256: taskenvdocker.EnvironmentSHA256(nil), ImageIdentity: imageID,
		CloneIdentity: "run-" + compact + "-g1-clone", VolumeIdentity: "fern-run-" + compact + "-g1-opencode",
		ContainerIdentity: "fern-run-" + compact + "-g1", EndpointIdentity: "run-" + compact + "-g1-endpoint",
	}
	admission, err := store.AdmitBackgroundRun(ctx, taskstore.AdmitBackgroundRunParams{
		TaskID: generated.TaskID, AttemptID: generated.AttemptID, ReceiptID: generated.ReceiptID,
		TaskEventID: generated.TaskEventID, AttemptEventID: generated.AttemptEventID,
		OpenCodeSessionID: generated.OpenCodeSessionID, OpenCodeMessageID: generated.OpenCodeMessageID,
		Claim: task.IdempotencyClaim{Scope: task.IdempotencyScope{WorkspaceID: workspaceID, CommandKind: taskstore.CreateBackgroundRunCommand},
			Key: "retained-create", RequestHash: sha256.Sum256([]byte("retained-create")), Actor: actor},
		Title: "Retained Background Run", Prompt: prompt, RepositoryID: 1, BaseRef: "main", BaseSHA: task.GitOID(base),
		ObjectFormat: "sha1", ExecutionContractVersion: "fern.background-run.v1", Agent: "contract",
		ModelProvider: "test", Model: "test-model", BudgetSnapshot: json.RawMessage(`{"turns":10}`),
		Deadline: now.Add(3 * time.Minute), APIContractVersion: "fern.background-run.v1", AcceptedAt: now, BackgroundRun: intent,
	})
	if err != nil {
		return err
	}
	config := backgroundruncoord.Config{
		WorkspaceID: workspaceID, WorkerID: "retained-worker",
		SystemActor: task.ActorSnapshot{Type: task.ActorSystem, ID: "retained-coordinator", DisplayName: "Retained coordinator",
			CredentialID: "service-v1", Authentication: "internal", RequestID: "retained-worker"},
		Profile: backgroundopencode.Profile, ImageIdentity: imageID, EnvironmentSHA256: taskenvdocker.EnvironmentSHA256(nil),
		Agent: "contract", ModelProvider: "test", Model: "test-model", OperationTimeout: 30 * time.Second,
		LeaseDuration: time.Minute, PollInterval: 100 * time.Millisecond,
		HistoryBounds: backgroundopencode.HistoryBounds{PageLimit: 2, MaxPages: 100, MaxEvents: 1000},
		Now:           func() time.Time { return time.Now().Add(3 * time.Minute) }, HTTPClient: &http.Client{Timeout: 10 * time.Second}, Route: route,
	}
	coordinator, err := backgroundruncoord.New(store, provider, artifact, ids, config)
	if err != nil {
		return err
	}
	var run taskstore.BackgroundRun
	for step := 0; step < 25; step++ {
		runErr := coordinator.RunOnce(ctx)
		if runErr != nil && !errors.Is(runErr, backgroundruncoord.ErrNoWork) {
			return fmt.Errorf("advance retained run: %w", runErr)
		}
		run, err = store.GetBackgroundRun(ctx, workspaceID, admission.Task.ID, actor)
		if err != nil {
			return err
		}
		if run.State == taskstore.BackgroundRunWorking {
			break
		}
	}
	if run.State != taskstore.BackgroundRunWorking {
		return fmt.Errorf("retained run did not reach working: %s/%s", run.State, run.EffectPhase)
	}
	clonePath := filepath.Join(root, "state", "background-runs", intent.CloneIdentity)
	if err := os.WriteFile(filepath.Join(clonePath, "committed-result.txt"), []byte("committed result\n"), 0o600); err != nil {
		return err
	}
	commit := exec.Command(gitPath, "-C", clonePath, "-c", "user.name=Fern Integration", "-c", "user.email=fern-integration@example.invalid",
		"add", "--", "committed-result.txt")
	if output, commitErr := commit.CombinedOutput(); commitErr != nil {
		return fmt.Errorf("stage retained committed change: %w: %s", commitErr, output)
	}
	commit = exec.Command(gitPath, "-C", clonePath, "-c", "user.name=Fern Integration", "-c", "user.email=fern-integration@example.invalid",
		"commit", "-m", "retained committed change")
	commit.Env = append(os.Environ(), "GIT_AUTHOR_DATE=2026-08-31T12:00:00Z", "GIT_COMMITTER_DATE=2026-08-31T12:00:00Z")
	if output, commitErr := commit.CombinedOutput(); commitErr != nil {
		return fmt.Errorf("commit retained change: %w: %s", commitErr, output)
	}
	if err := os.WriteFile(filepath.Join(clonePath, "dirty-result.txt"), []byte("dirty result\n"), 0o600); err != nil {
		return err
	}
	sealIDs, err := ids.GenerateBackgroundSealIDs()
	if err != nil {
		return err
	}
	owner, attempt, err := store.GetBackgroundRunOwners(ctx, workspaceID, run.TaskID, actor)
	if err != nil {
		return err
	}
	sealAt := config.Now().UTC().Truncate(time.Millisecond)
	seal, err := store.SealBackgroundRun(ctx, taskstore.SealBackgroundRunParams{
		WorkspaceID: workspaceID, TaskID: run.TaskID, AttemptID: run.AttemptID, Generation: run.Generation,
		ExpectedRunRevision: run.Revision, ExpectedTaskRevision: owner.Revision, ExpectedAttemptRevision: attempt.Revision,
		SealRequestID: sealIDs.SealRequestID, ReceiptID: sealIDs.ReceiptID, ExportID: sealIDs.ArtifactExportID,
		ArtifactID: sealIDs.RetainedArtifactID, MaterializationID: sealIDs.MaterializationID, ResultID: sealIDs.ResultID,
		ResultEventID: sealIDs.ResultEventID, TaskEventID: sealIDs.TaskEventID,
		Claim: task.IdempotencyClaim{Scope: task.IdempotencyScope{WorkspaceID: workspaceID, CommandKind: taskstore.SealBackgroundRunCommand},
			Key: "retained-seal", RequestHash: sha256.Sum256([]byte("retained-seal")), Actor: actor},
		CommitEpochSeconds: sealAt.Unix(), PolicyVersion: "fern.background-user-seal.v1",
		APIContractVersion: "fern.background-run.v1", AcceptedAt: sealAt,
	})
	if err != nil {
		return err
	}
	for step := 0; step < 25; step++ {
		runErr := coordinator.RunOnce(ctx)
		if runErr != nil && !errors.Is(runErr, backgroundruncoord.ErrNoWork) {
			current, _ := store.GetBackgroundRun(context.Background(), workspaceID, admission.Task.ID, actor)
			export, _ := store.GetBackgroundRunExport(context.Background(), seal.Request.ExportID)
			return fmt.Errorf("retain result at run=%s/%s authority=%s export=%s/%s revision=%d: %w",
				current.State, current.EffectPhase, current.ResultAuthorityPhase, export.State, export.Phase, export.Revision, runErr)
		}
		run, err = store.GetBackgroundRun(ctx, workspaceID, admission.Task.ID, actor)
		if err != nil {
			return err
		}
		if run.State == taskstore.BackgroundRunResultReady && run.EffectPhase == taskstore.BackgroundRunEffectCleanupComplete {
			break
		}
	}
	if run.State != taskstore.BackgroundRunResultReady || run.EffectPhase != taskstore.BackgroundRunEffectCleanupComplete {
		export, _ := store.GetBackgroundRunExport(ctx, seal.Request.ExportID)
		return fmt.Errorf("retained result incomplete: run=%s/%s export=%s/%s reason=%s", run.State, run.EffectPhase, export.State, export.Phase, export.RecoveryReason)
	}
	projection, err := store.GetBackgroundRunResult(ctx, workspaceID, admission.Task.ID, actor)
	if err != nil {
		return err
	}
	if projection.Result.ID != seal.Request.ResultID || projection.Result.Outcome != task.ResultChanged ||
		projection.Artifact.ResultCommit != projection.Result.ResultCommit || projection.Artifact.TreeOID != projection.Result.TreeOID ||
		projection.Artifact.ChangesSHA256 != projection.Result.ManifestSHA256 {
		return fmt.Errorf("retained result tuple mismatch: %+v", projection)
	}
	locator, err := taskartifact.ParseLocator(projection.Artifact.CASLocator)
	if err != nil {
		return err
	}
	resolver, err := taskresultsource.New(store, artifact)
	if err != nil {
		return err
	}
	resolvedPath, closeResolved, err := resolver.Acquire(ctx, projection.Result)
	if err != nil {
		return fmt.Errorf("resolve retained result for downstream consumer: %w", err)
	}
	if resolvedPath == clonePath || closeResolved == nil {
		if closeResolved != nil {
			_ = closeResolved()
		}
		return errors.New("retained result resolver reused disposable authority")
	}
	if err := closeResolved(); err != nil {
		return err
	}
	var previous string
	for attempt := 0; attempt < 2; attempt++ {
		checkout, materializeErr := artifact.Materialize(ctx, locator)
		if materializeErr != nil {
			return materializeErr
		}
		path := checkout.Path()
		if path == previous || path == clonePath {
			_ = checkout.Close()
			return errors.New("retained materialization reused disposable authority")
		}
		if content, readErr := os.ReadFile(filepath.Join(path, "committed-result.txt")); readErr != nil || string(content) != "committed result\n" {
			_ = checkout.Close()
			return fmt.Errorf("read committed retained materialization: %q %v", content, readErr)
		}
		if content, readErr := os.ReadFile(filepath.Join(path, "dirty-result.txt")); readErr != nil || string(content) != "dirty result\n" {
			_ = checkout.Close()
			return fmt.Errorf("read dirty retained materialization: %q %v", content, readErr)
		}
		previous = path
		if err := checkout.Close(); err != nil {
			return err
		}
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("retained checkout residue: %v", err)
		}
	}
	if _, err := os.Lstat(clonePath); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("retained clone residue: %v", err)
	}
	return nil
}

func privateBackgroundResidue(name string) bool {
	return name == "fern-background-run.json" || strings.HasPrefix(name, ".clone-stage-") || strings.HasPrefix(name, ".clone-quarantine-") ||
		strings.HasPrefix(name, ".clone-authority-") || strings.HasPrefix(name, ".clone-marker-stage-")
}

func startSerialRoute(parent context.Context, store *control.Store, address string) (*backgroundroute.Manager, string, string, func() error, error) {
	if address == "" {
		address = "127.0.0.1:0"
	}
	listener, err := net.Listen("tcp4", address)
	if err != nil {
		return nil, "", "", nil, err
	}
	manager, err := backgroundroute.New(listener, "https://fern.example.ts.net:8443", store)
	if err != nil {
		_ = listener.Close()
		return nil, "", "", nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	done := make(chan error, 1)
	go func() { done <- manager.Run(ctx) }()
	var once sync.Once
	var stopErr error
	stop := func() error {
		once.Do(func() {
			cancel()
			stopErr = <-done
		})
		return stopErr
	}
	bound := listener.Addr().String()
	return manager, "http://" + bound, bound, stop, nil
}

func verifySerialRoute(origin, token string) error {
	status, err := serialRouteStatus(origin, token, "/")
	if err != nil || status != http.StatusOK {
		return fmt.Errorf("paired official root status=%d error=%v", status, err)
	}
	status, err = serialRouteStatus(origin, token, "/api/health")
	if err != nil || status != http.StatusOK {
		return fmt.Errorf("paired authenticated health status=%d error=%v", status, err)
	}
	for name, credential := range map[string]string{"unpaired": "", "wrong": "wrong-device-token"} {
		status, err = serialRouteStatus(origin, credential, "/api/health")
		if err != nil || status != http.StatusUnauthorized {
			return fmt.Errorf("%s route access status=%d error=%v", name, status, err)
		}
	}
	return nil
}

func verifySerialOpenReplay(store *taskstore.Store, ids *task.Generator, run taskstore.BackgroundRun, actor task.ActorSnapshot, route *backgroundroute.Manager, forbiddenToken string) error {
	origin, active := route.ActiveOrigin(run)
	if !active {
		return errors.New("serial open route was not exactly active")
	}
	trusted, err := backgroundopencode.ParseTrustedOrigin(origin)
	if err != nil {
		return err
	}
	deepLink, err := backgroundopencode.DeepLink(trusted, string(run.OpenCodeSessionID))
	if err != nil {
		return err
	}
	receipt, err := ids.ReceiptID()
	if err != nil {
		return err
	}
	hash := sha256.Sum256([]byte("serial-open:" + string(run.TaskID)))
	params := taskstore.OpenBackgroundRunParams{WorkspaceID: run.WorkspaceID, TaskID: run.TaskID, ReceiptID: receipt,
		Claim: task.IdempotencyClaim{Scope: task.IdempotencyScope{WorkspaceID: run.WorkspaceID, CommandKind: taskstore.OpenBackgroundRunCommand},
			Key: "serial-open", RequestHash: hash, Actor: actor}, URL: deepLink, APIContractVersion: "fern.background-run.v1",
		OpenedAt: time.Now().UTC().Truncate(time.Millisecond).Add(2 * time.Minute)}
	first, err := store.OpenBackgroundRun(context.Background(), params)
	if err != nil {
		return err
	}
	params.ReceiptID, err = ids.ReceiptID()
	if err != nil {
		return err
	}
	replay, err := store.OpenBackgroundRun(context.Background(), params)
	if err != nil || !replay.Replayed || first.Receipt.ID != replay.Receipt.ID || string(first.Receipt.ResponseProjection) != string(replay.Receipt.ResponseProjection) ||
		strings.Contains(string(replay.Receipt.ResponseProjection), forbiddenToken) {
		return fmt.Errorf("serial open replay stable=%t receipt=%s/%s error=%v", replay.Replayed, first.Receipt.ID, replay.Receipt.ID, err)
	}
	return nil
}

func serialRouteStatus(origin, token, path string) (int, error) {
	request, err := http.NewRequest(http.MethodGet, origin+path, nil)
	if err != nil {
		return 0, err
	}
	if token != "" {
		request.AddCookie(&http.Cookie{Name: "__Host-fern_device", Value: token})
	}
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	_, readErr := io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
	return response.StatusCode, readErr
}

func runPreDispatchFenceScenario(ctx context.Context, root string, provider *taskenvdocker.Provider, artifact *taskartifact.Engine, cli *client.Client,
	ids *task.Generator, workspaceID task.WorkspaceID, imageID, base string, route *backgroundroute.Manager) error {
	databasePath := filepath.Join(root, "fence-task-store.sqlite")
	store, err := taskstore.Open(ctx, databasePath)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	acceptedAt := time.Now().UTC().Truncate(time.Millisecond).Add(2 * time.Minute)
	if _, err := store.EnsureWorkspace(ctx, taskstore.Workspace{
		ID: workspaceID, Name: "serial-background-fence", State: taskstore.WorkspaceActive,
		RepositoryPath: filepath.Join(root, "repository"), GitHubAuthority: taskstore.GitHubAuthorityAppBroker,
		InstallationID: 1, RepositoryID: 1, RepositoryFullName: "fern-integration/background-run",
		ImageDigest: imageID, OpenCodeProtocol: backgroundopencode.Profile, RuntimeDesiredState: "running",
		ReconciliationEpoch: 1, CreatedAt: acceptedAt,
	}); err != nil {
		return err
	}
	generated, err := ids.GenerateAdmissionIDs()
	if err != nil {
		return err
	}
	prompt := "FERN_BACKGROUND_MUST_NOT_DISPATCH"
	actor := task.ActorSnapshot{Type: task.ActorOpenCode, ID: "serial-plugin", DisplayName: "Serial integration",
		CredentialID: "serial-plugin", Authentication: "fern_plugin_bearer", RequestID: "serial-fence-crash"}
	compact := strings.ReplaceAll(strings.TrimPrefix(string(generated.TaskID), "tsk_"), "-", "")
	intent := &taskstore.BackgroundRunIntent{
		RepositoryRemote: "https://github.com/fern-integration/background-run", Branch: "main",
		InstructionSHA256: sha256.Sum256([]byte(prompt)), Profile: backgroundopencode.Profile,
		ProfileSHA256: sha256.Sum256([]byte(backgroundopencode.Profile)), EnvironmentSHA256: taskenvdocker.EnvironmentSHA256(nil), ImageIdentity: imageID,
		CloneIdentity: "run-" + compact + "-g1-clone", VolumeIdentity: "fern-run-" + compact + "-g1-opencode",
		ContainerIdentity: "fern-run-" + compact + "-g1", EndpointIdentity: "run-" + compact + "-g1-endpoint",
	}
	requestHash := sha256.Sum256([]byte("serial-fence-crash-create"))
	admission, err := store.AdmitBackgroundRun(ctx, taskstore.AdmitBackgroundRunParams{
		TaskID: generated.TaskID, AttemptID: generated.AttemptID, ReceiptID: generated.ReceiptID,
		TaskEventID: generated.TaskEventID, AttemptEventID: generated.AttemptEventID,
		OpenCodeSessionID: generated.OpenCodeSessionID, OpenCodeMessageID: generated.OpenCodeMessageID,
		Claim: task.IdempotencyClaim{Scope: task.IdempotencyScope{WorkspaceID: workspaceID, CommandKind: taskstore.CreateBackgroundRunCommand},
			Key: "serial-fence-crash-create", RequestHash: requestHash, Actor: actor},
		Title: "Serial pre-dispatch fence crash", Prompt: prompt, RepositoryID: 1, BaseRef: "main", BaseSHA: task.GitOID(base),
		ObjectFormat: "sha1", ExecutionContractVersion: "fern.background-run.v1", Agent: "contract",
		ModelProvider: "test", Model: "test-model", BudgetSnapshot: json.RawMessage(`{"turns":10}`),
		Deadline: acceptedAt.Add(10 * time.Minute), APIContractVersion: "fern.background-run.v1", AcceptedAt: acceptedAt, BackgroundRun: intent,
	})
	if err != nil {
		return err
	}
	baseTransport := http.DefaultTransport.(*http.Transport).Clone()
	transport := &countingPromptTransport{base: baseTransport, path: "/api/session/" + string(generated.OpenCodeSessionID) + "/prompt"}
	defer baseTransport.CloseIdleConnections()
	crashCtx, crash := context.WithCancel(ctx)
	config := backgroundruncoord.Config{
		WorkspaceID: workspaceID, WorkerID: "serial-fence-worker-a",
		SystemActor: task.ActorSnapshot{Type: task.ActorSystem, ID: "serial-coordinator", DisplayName: "Serial coordinator",
			CredentialID: "service-v1", Authentication: "internal", RequestID: "serial-fence-worker-a"},
		Profile: backgroundopencode.Profile, ImageIdentity: imageID, EnvironmentSHA256: taskenvdocker.EnvironmentSHA256(nil), Agent: "contract", ModelProvider: "test", Model: "test-model",
		OperationTimeout: 20 * time.Second, LeaseDuration: time.Minute, PollInterval: 100 * time.Millisecond,
		HistoryBounds: backgroundopencode.HistoryBounds{PageLimit: 2, MaxPages: 100, MaxEvents: 1000},
		Now:           func() time.Time { return time.Now().Add(2 * time.Minute) }, HTTPClient: &http.Client{Timeout: 10 * time.Second, Transport: transport}, Route: route,
		AfterPromptFence: crash,
	}
	coordinator, err := backgroundruncoord.New(store, provider, artifact, ids, config)
	if err != nil {
		return err
	}
	var fenced taskstore.BackgroundRun
	for step := 0; step < 20; step++ {
		runErr := coordinator.RunOnce(crashCtx)
		fenced, err = store.GetBackgroundRun(context.Background(), workspaceID, admission.Task.ID, actor)
		if err != nil {
			return err
		}
		if fenced.PromptRequestAttemptedAt != nil {
			if !errors.Is(runErr, context.Canceled) {
				return fmt.Errorf("pre-dispatch fence crash error=%v", runErr)
			}
			break
		}
		if runErr != nil {
			return runErr
		}
	}
	if fenced.PromptRequestAttemptedAt == nil || transport.calls.Load() != 0 {
		return fmt.Errorf("pre-dispatch fence durable=%t prompt_posts=%d", fenced.PromptRequestAttemptedAt != nil, transport.calls.Load())
	}
	if err := store.Close(); err != nil {
		return err
	}
	store, err = taskstore.Open(context.Background(), databasePath)
	if err != nil {
		return err
	}
	config.WorkerID = "serial-fence-worker-b"
	config.SystemActor.RequestID = config.WorkerID
	config.Now = func() time.Time { return time.Now().Add(4 * time.Minute) }
	config.AfterPromptFence = nil
	coordinator, err = backgroundruncoord.New(store, provider, artifact, ids, config)
	if err != nil {
		return err
	}
	if err := coordinator.RunOnce(context.Background()); err != nil {
		return fmt.Errorf("pre-dispatch restart reconciliation: %w", err)
	}
	fenced, err = store.GetBackgroundRun(context.Background(), workspaceID, admission.Task.ID, actor)
	if err != nil || fenced.State != taskstore.BackgroundRunUncertain || fenced.EffectPhase != taskstore.BackgroundRunEffectPromptIntent ||
		fenced.PromptRequestAttemptedAt == nil || transport.calls.Load() != 0 || !strings.Contains(fenced.LastEvidence, `"status":"absent"`) {
		return fmt.Errorf("pre-dispatch restart run=%s/%s fenced=%t posts=%d error=%v", fenced.State, fenced.EffectPhase,
			fenced.PromptRequestAttemptedAt != nil, transport.calls.Load(), err)
	}
	stopReceipt, err := ids.ReceiptID()
	if err != nil {
		return err
	}
	attemptEvent, err := ids.EventID()
	if err != nil {
		return err
	}
	taskEvent, err := ids.EventID()
	if err != nil {
		return err
	}
	stopHash := sha256.Sum256([]byte("serial-fence-crash-stop"))
	if _, err := store.StopBackgroundRun(context.Background(), taskstore.StopBackgroundRunParams{
		WorkspaceID: workspaceID, TaskID: fenced.TaskID, ReceiptID: stopReceipt, AttemptEventID: attemptEvent, TaskEventID: taskEvent,
		Claim: task.IdempotencyClaim{Scope: task.IdempotencyScope{WorkspaceID: workspaceID, CommandKind: taskstore.StopBackgroundRunCommand},
			Key: "serial-fence-crash-stop", RequestHash: stopHash, Actor: actor}, APIContractVersion: "fern.background-run.v1",
		StoppedAt: time.Now().UTC().Truncate(time.Millisecond).Add(4 * time.Minute),
	}); err != nil {
		return err
	}
	for step := 0; step < 20; step++ {
		if err := coordinator.RunOnce(context.Background()); err != nil && !errors.Is(err, backgroundruncoord.ErrNoWork) {
			return err
		}
		fenced, err = store.GetBackgroundRun(context.Background(), workspaceID, admission.Task.ID, actor)
		if err != nil {
			return err
		}
		if fenced.State == taskstore.BackgroundRunFailed {
			break
		}
	}
	if fenced.State != taskstore.BackgroundRunFailed || transport.calls.Load() != 0 {
		return fmt.Errorf("pre-dispatch cleanup state=%s posts=%d", fenced.State, transport.calls.Load())
	}
	if _, err := cli.ContainerInspect(context.Background(), intent.ContainerIdentity); !client.IsErrNotFound(err) {
		return fmt.Errorf("pre-dispatch container residue: %v", err)
	}
	if _, err := cli.VolumeInspect(context.Background(), intent.VolumeIdentity); !client.IsErrNotFound(err) {
		return fmt.Errorf("pre-dispatch volume residue: %v", err)
	}
	clonePath := filepath.Join(root, "state", "background-runs", intent.CloneIdentity)
	if _, err := os.Lstat(clonePath); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("pre-dispatch clone residue: %v", err)
	}
	return nil
}

func initializeRepository(repository, gitPath, providerIP string) error {
	config := map[string]any{
		"$schema": "https://opencode.ai/config.json", "model": "test/test-model",
		"permissions": []map[string]string{{"action": "question", "resource": "*", "effect": "allow"}, {"action": "bash", "resource": "*", "effect": "ask"}, {"action": "shell", "resource": "*", "effect": "ask"}},
		"agents":      map[string]any{"contract": map[string]any{"description": "Background client integration", "permissions": []map[string]string{{"action": "question", "resource": "*", "effect": "allow"}, {"action": "bash", "resource": "*", "effect": "ask"}, {"action": "shell", "resource": "*", "effect": "ask"}}}},
		"providers": map[string]any{"test": map[string]any{
			"name": "Background Client Integration", "env": []string{},
			"api":     map[string]any{"type": "aisdk", "package": "@ai-sdk/openai-compatible", "url": "http://" + providerIP + ":4100/v1"},
			"request": map[string]any{"body": map[string]string{"apiKey": "test-key"}},
			"models": map[string]any{"test-model": map[string]any{
				"name": "Background Client Model", "api": map[string]any{"id": "test-model", "type": "aisdk", "package": "@ai-sdk/openai-compatible", "url": "http://" + providerIP + ":4100/v1"},
				"capabilities": map[string]any{"tools": true, "input": []string{"text"}, "output": []string{"text"}},
				"cost":         map[string]any{"input": 0, "output": 0, "cache": map[string]int{"read": 0, "write": 0}},
				"limit":        map[string]int{"context": 100000, "output": 10000}, "request": map[string]any{"body": map[string]string{"apiKey": "test-key"}},
			}},
		}},
	}
	data, err := json.Marshal(config)
	if err != nil {
		return err
	}
	for _, args := range [][]string{{"init", "--initial-branch=main"}, {"config", "user.name", "Fern Integration"}, {"config", "user.email", "fern@example.invalid"}, {"remote", "add", "origin", "https://github.com/fern-integration/background-run"}} {
		if err := git(repository, gitPath, args...); err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(repository, "opencode.json"), data, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(repository, "README.md"), []byte("background OpenCode client integration\n"), 0o644); err != nil {
		return err
	}
	if err := git(repository, gitPath, "add", "."); err != nil {
		return err
	}
	return git(repository, gitPath, "commit", "-m", "fixture")
}

func startProvider(ctx context.Context, cli *client.Client, name, imageID string) (string, string, string, error) {
	script, err := filepath.Abs("integration/background-run-opencode/fake_provider.mjs")
	if err != nil {
		return "", "", "", err
	}
	created, err := cli.ContainerCreate(ctx, &container.Config{
		Image: imageID, User: "1001:1001", Entrypoint: []string{"node"}, Cmd: []string{"/contract/fake_provider.mjs", "4100"},
		ExposedPorts: nat.PortSet{providerPort: struct{}{}},
	}, &container.HostConfig{
		NetworkMode: "bridge", Mounts: []mount.Mount{{Type: mount.TypeBind, Source: script, Target: "/contract/fake_provider.mjs", ReadOnly: true}},
		PortBindings: nat.PortMap{providerPort: []nat.PortBinding{{HostIP: "127.0.0.1", HostPort: "0"}}},
	}, nil, nil, name)
	if err != nil {
		return "", "", "", err
	}
	if err := cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		return created.ID, "", "", err
	}
	info, err := cli.ContainerInspect(ctx, created.ID)
	if err != nil {
		return created.ID, "", "", err
	}
	binding := info.NetworkSettings.Ports[providerPort]
	bridge := info.NetworkSettings.Networks["bridge"]
	if len(binding) != 1 || binding[0].HostIP != "127.0.0.1" || bridge == nil || bridge.IPAddress == "" {
		return created.ID, "", "", errors.New("fake provider loopback publication or bridge address missing")
	}
	return created.ID, "http://127.0.0.1:" + binding[0].HostPort, bridge.IPAddress, nil
}

func runtimeCredentials(ctx context.Context, cli *client.Client, containerID string) (string, string, error) {
	info, err := cli.ContainerInspect(ctx, containerID)
	if err != nil {
		return "", "", err
	}
	values := map[string]string{}
	for _, item := range info.Config.Env {
		key, value, ok := strings.Cut(item, "=")
		if ok && (key == "OPENCODE_SERVER_USERNAME" || key == "OPENCODE_SERVER_PASSWORD") {
			if _, duplicate := values[key]; duplicate {
				return "", "", errors.New("duplicate runtime credential environment")
			}
			values[key] = value
		}
	}
	if values["OPENCODE_SERVER_USERNAME"] == "" || values["OPENCODE_SERVER_PASSWORD"] == "" {
		return "", "", errors.New("runtime credentials missing")
	}
	return values["OPENCODE_SERVER_USERNAME"], values["OPENCODE_SERVER_PASSWORD"], nil
}

func readStats(endpoint string) (providerStats, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/stats", nil)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return providerStats{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return providerStats{}, fmt.Errorf("stats status %d", response.StatusCode)
	}
	var stats providerStats
	if err := json.NewDecoder(response.Body).Decode(&stats); err != nil {
		return providerStats{}, err
	}
	return stats, nil
}

func waitFor(ctx context.Context, name string, check func() (bool, error)) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var last error
	for {
		ok, err := check()
		if ok {
			return nil
		}
		if err != nil {
			last = err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s: %v: %w", name, last, ctx.Err())
		case <-ticker.C:
		}
	}
}

func cleanupOpenCode(provider *taskenvdocker.Provider, cli *client.Client, run taskstore.BackgroundRun, containerID string, runtime taskenvdocker.RuntimeIdentity, clonePath string) error {
	var result error
	call := func(name string, fn func(context.Context) error) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		err := fn(ctx)
		cancel()
		if err != nil {
			result = errors.Join(result, fmt.Errorf("cleanup %s: %w", name, err))
		}
	}
	authority := taskenvdocker.NeverCreatedAuthority()
	if provider != nil && runtime.ContainerID != "" {
		authority = taskenvdocker.RuntimeCleanupAuthority(runtime)
		call("stop", func(ctx context.Context) error { _, err := provider.StopContainer(ctx, run, runtime); return err })
		call("container", func(ctx context.Context) error { _, err := provider.RemoveContainer(ctx, run, authority); return err })
	} else if provider != nil && containerID != "" {
		authority = taskenvdocker.CreatedContainerAuthority(containerID)
		call("created container", func(ctx context.Context) error { _, err := provider.RemoveContainer(ctx, run, authority); return err })
	}
	call("exact-name container fallback", func(ctx context.Context) error {
		err := cli.ContainerRemove(ctx, run.ContainerIdentity, container.RemoveOptions{Force: true})
		if client.IsErrNotFound(err) {
			return nil
		}
		return err
	})
	if provider != nil {
		call("volume", func(ctx context.Context) error { _, err := provider.RemoveVolume(ctx, run, authority); return err })
	}
	call("exact-name volume fallback", func(ctx context.Context) error {
		err := cli.VolumeRemove(ctx, run.VolumeIdentity, true)
		if client.IsErrNotFound(err) {
			return nil
		}
		return err
	})
	if provider != nil {
		call("clone", func(ctx context.Context) error { _, err := provider.RemoveClone(ctx, run, authority); return err })
	}
	call("exact-path clone fallback", func(context.Context) error { return os.RemoveAll(clonePath) })
	call("residue", func(ctx context.Context) error {
		var residue error
		if _, err := cli.ContainerInspect(ctx, run.ContainerIdentity); !client.IsErrNotFound(err) {
			residue = errors.Join(residue, fmt.Errorf("container: %v", err))
		}
		if _, err := cli.VolumeInspect(ctx, run.VolumeIdentity); !client.IsErrNotFound(err) {
			residue = errors.Join(residue, fmt.Errorf("volume: %v", err))
		}
		if _, err := os.Lstat(clonePath); !errors.Is(err, os.ErrNotExist) {
			residue = errors.Join(residue, fmt.Errorf("clone: %v", err))
		}
		return residue
	})
	return result
}

func git(directory, binary string, args ...string) error {
	command := exec.Command(binary, args...)
	command.Dir = directory
	command.Env = append(os.Environ(), "LC_ALL=C")
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %w: %s", args[0], err, output)
	}
	return nil
}

func gitOutput(directory, binary string, args ...string) (string, error) {
	command := exec.Command(binary, args...)
	command.Dir = directory
	output, err := command.Output()
	return strings.TrimSpace(string(output)), err
}
