package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	"github.com/nebler/fern/internal/backgroundopencode"
	"github.com/nebler/fern/internal/task"
	"github.com/nebler/fern/internal/taskenvdocker"
	"github.com/nebler/fern/internal/taskstore"
)

const (
	defaultImageID = "sha256:f493fc1cf2ffb087ef9733eb7f6f14fc0ae0966392fe54ccf695633570c82a82"
	imageTag       = "fern/opencode-background-source:dev"
	providerPort   = nat.Port("4100/tcp")
)

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

func (t *lostResponseTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	t.calls.Add(1)
	response, err := t.base.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	if request.Method != http.MethodPost || request.URL.Path != t.path || !t.lost.CompareAndSwap(0, 1) {
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
		imageID = defaultImageID
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
		Profile: taskstore.BackgroundRunSourceProfile, ImageIdentity: imageID,
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
	oc, err := backgroundopencode.New(backgroundopencode.Config{
		Endpoint: started.Endpoint, Username: username, Password: password,
		HTTPClient: &http.Client{Timeout: 10 * time.Second},
	})
	if err != nil {
		return err
	}
	session := backgroundopencode.SessionSpec{ID: string(sessionID), Agent: "contract", ProviderID: "test", ModelID: "test-model", Directory: "/home/user/workspace"}
	if err := oc.CreateSessionOnce(ctx, session); err != nil {
		return err
	}
	if state, err := oc.ReconcileSession(ctx, session); err != nil {
		return fmt.Errorf("session reconciliation: %w", err)
	} else if state != backgroundopencode.ReconcileExact {
		return fmt.Errorf("session reconciliation state=%s", state)
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
	initialProviderClosed = true
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
	fmt.Printf("PASS profile=%s image_id=%s session=exact prompt=admitted_promoted response_loss=after_effect active=positive interrupt=204 reconstruction=provider_client no_prompt_replay=true provider_calls=1 marker=exact\n", backgroundopencode.Profile, imageID)
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
