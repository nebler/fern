package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/nebler/fern/internal/backgroundroute"
	"github.com/nebler/fern/internal/config"
	"github.com/nebler/fern/internal/control"
	"github.com/nebler/fern/internal/githubapp"
	"github.com/nebler/fern/internal/observability"
	"github.com/nebler/fern/internal/runtime"
	"github.com/nebler/fern/internal/task"
	"github.com/nebler/fern/internal/taskstore"
	"github.com/nebler/fern/internal/workspace"
)

const taskServiceTestImageID = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
const taskServiceBackgroundImageID = "sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"

type corruptResultCoordinator struct{}

func (corruptResultCoordinator) RunOnce(context.Context) error { return taskstore.ErrCorruptStore }

func TestTaskServicesAreExplicitlyDisabledWhenPolicyIsAbsent(t *testing.T) {
	t.Parallel()
	services, err := newTaskServices(context.Background(), config.Config{}, nil, nil, nil, structServerAuth(), observability.NewRegistry(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil || services != nil {
		t.Fatalf("services=%v err=%v", services, err)
	}
}

func TestBackgroundRunEnvironmentNeverInheritsWorkspaceCustody(t *testing.T) {
	cfg := config.Config{
		Workspace: config.Workspace{Env: map[string]string{
			"OPENCODE_PASSWORD": "workspace-password", "OPENAI_API_KEY": "workspace-provider-key",
		}},
		Tasks: &config.TaskPolicy{BackgroundEnvironment: map[string]string{"OPENAI_API_KEY": "explicit-background-key"}},
	}
	got := backgroundRunEnvironment(cfg)
	if len(got) != 1 || got["OPENAI_API_KEY"] != "explicit-background-key" {
		t.Fatalf("background environment = %#v", got)
	}
	if _, exists := got["OPENCODE_PASSWORD"]; exists {
		t.Fatal("workspace password crossed into disposable background environment")
	}
	got["OPENAI_API_KEY"] = "changed"
	if cfg.Tasks.BackgroundEnvironment["OPENAI_API_KEY"] != "explicit-background-key" {
		t.Fatal("production background environment was not copied")
	}
}

func TestTaskServicesRequireExplicitGitHubAuthorityBeforeRuntimeAccess(t *testing.T) {
	t.Parallel()
	cfg := config.Config{Tasks: &config.TaskPolicy{
		Agent: "build", Model: config.TaskModel{Provider: "test", ID: "test-model"},
		AttemptTimeout: time.Hour, LeaseDuration: time.Minute, Budget: config.TaskBudget{MaxTurns: 10},
	}}
	if _, err := newTaskServices(context.Background(), cfg, nil, nil, nil, structServerAuth(), observability.NewRegistry(), slog.New(slog.NewTextHandler(io.Discard, nil))); err == nil {
		t.Fatal("task service accepted no GitHub authority")
	}
}

func TestResolveGitHubAuthorityDefersRemoteValidationToOperations(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	installTaskAppCredentials(t)
	github := &config.WorkspaceGitHub{
		Mode: config.GitHubModeGitHubAppBroker, InstallationID: 123,
		Repository: config.GitHubRepository{ID: 456, FullName: "owner/repository"},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	authority, err := resolveGitHubAuthority(github, nil, nil, "demo")
	if err != nil {
		t.Fatalf("composition used canceled remote context: %v", err)
	}
	if authority.kind != taskstore.GitHubAuthorityAppBroker || authority.baseResolver == nil || authority.repositories == nil {
		t.Fatalf("authority = %+v", authority)
	}
	if _, err := authority.baseResolver(ctx, "main"); !errors.Is(err, context.Canceled) {
		t.Fatalf("runtime repository read error = %v", err)
	}
}

func TestResolveGitHubAuthorityRejectsMalformedLocalCredentials(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	directory, err := statePath("github-app")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := githubapp.NewCredentialStore(directory); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "app-credentials.json"), []byte(`{"version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	github := &config.WorkspaceGitHub{
		Mode: config.GitHubModeGitHubAppBroker, InstallationID: 123,
		Repository: config.GitHubRepository{ID: 456, FullName: "owner/repository"},
	}
	if _, err := resolveGitHubAuthority(github, nil, nil, "demo"); !errors.Is(err, githubapp.ErrStoredCredentialsInvalid) || errors.Is(err, githubapp.ErrCredentialsNotFound) {
		t.Fatalf("malformed credential error = %v", err)
	}
}

func TestMissingAppCredentialsBlockTasksButKeepOnboardingAvailable(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]string{"Id": taskServiceTestImageID})
	}))
	defer server.Close()
	t.Setenv("DOCKER_HOST", "tcp://"+strings.TrimPrefix(server.URL, "http://"))
	t.Setenv("DOCKER_API_VERSION", "1.48")
	docker, err := runtime.NewDocker(slog.New(slog.NewTextHandler(io.Discard, nil)), taskServiceIntentStore{}, runtime.SuspendStop)
	if err != nil {
		t.Fatal(err)
	}
	defer docker.Close()
	cfg := config.Config{
		RemoteOrigin: "https://fern.example.ts.net", OperatorListen: "127.0.0.1:8081",
		Workspace: config.Workspace{Name: "blocked-tasks", Image: "image:test", Repo: t.TempDir(), GitHub: &config.WorkspaceGitHub{
			Mode: config.GitHubModeGitHubAppBroker, InstallationID: 123,
			Repository: config.GitHubRepository{ID: 456, FullName: "owner/repository"},
		}},
		Tasks: &config.TaskPolicy{Agent: "build", Model: config.TaskModel{Provider: "fixture", ID: "fixture-model"},
			AttemptTimeout: time.Hour, LeaseDuration: time.Minute, Budget: config.TaskBudget{MaxTurns: 10}},
	}
	status := observability.NewRegistry()
	services, err := newTaskServices(context.Background(), cfg, docker, nil, nil, structServerAuth(), status, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if services != nil || !errors.Is(err, githubapp.ErrCredentialsNotFound) {
		t.Fatalf("services=%v error=%v", services, err)
	}
	snapshot := status.Snapshot()
	dependency := snapshot.Components[0]
	foundDependency := false
	for _, component := range snapshot.Components {
		if component.Component == observability.ComponentGitHubTaskDependency {
			dependency = component
			foundDependency = true
			break
		}
	}
	if !foundDependency || snapshot.Ready || dependency.State != observability.StateBlocked || dependency.Ready {
		t.Fatalf("missing credential snapshot = %+v", snapshot)
	}
	if onboarding, err := newGitHubOnboarding(cfg); err != nil || onboarding == nil {
		t.Fatalf("onboarding=%v error=%v", onboarding, err)
	}
}

type taskServiceIntentStore struct{}

func (taskServiceIntentStore) BeginPause(string, string) error                { return nil }
func (taskServiceIntentStore) CommitPause(string, string) error               { return nil }
func (taskServiceIntentStore) CommitFailedStart(string, string) error         { return nil }
func (taskServiceIntentStore) CommitShutdown(string, string, time.Time) error { return nil }
func (taskServiceIntentStore) PauseStatus(string, string, time.Time) (runtime.PauseIntentStatus, error) {
	return runtime.PauseIntentNone, nil
}
func (taskServiceIntentStore) Clear(string) error { return nil }

type taskServiceRuntime struct{}

func (taskServiceRuntime) EnsureRunningObserved(context.Context, runtime.Spec) (runtime.RunningResult, error) {
	return runtime.RunningResult{}, nil
}
func (taskServiceRuntime) ReconcileStartup(context.Context, runtime.Spec) (runtime.StartupResult, error) {
	return runtime.StartupResult{}, nil
}
func (taskServiceRuntime) Pause(context.Context, string) error { return nil }
func (taskServiceRuntime) Status(context.Context, string) (runtime.Observation, error) {
	return runtime.Observation{State: runtime.StateAbsent}, nil
}

func TestTaskServicesSuccessfulWorkspaceGHMatrix(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			t.Errorf("startup performed Docker effect %s %s", request.Method, request.URL.Path)
			http.NotFound(writer, request)
			return
		}
		if request.URL.Path == "/v1.48/images/background:test/json" {
			writer.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(writer).Encode(map[string]any{"Id": taskServiceBackgroundImageID, "Config": map[string]any{
				"Labels": map[string]string{
					"org.opencontainers.image.source":   runtime.BackgroundOpenCodeSource,
					"org.opencontainers.image.revision": runtime.BackgroundOpenCodeRevision,
					"org.opencontainers.image.version":  runtime.BackgroundOpenCodeVersion,
					"ai.fern.opencode.profile":          runtime.BackgroundOpenCodeProfile,
				},
				"User": "1001:1001", "Cmd": []string{"opencode", "serve", "--hostname", "0.0.0.0", "--port", "4096"},
				"ExposedPorts": map[string]any{"4096/tcp": map[string]any{}}, "Volumes": map[string]any{
					"/home/user/workspace": map[string]any{}, "/home/user/.local/share/opencode": map[string]any{},
				}, "Env": []string{"XDG_DATA_HOME=/home/user/.local/share"},
			}})
			return
		}
		if request.URL.Path != "/v1.48/images/image:test/json" {
			http.NotFound(writer, request)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]string{"Id": taskServiceTestImageID})
	}))
	defer server.Close()
	t.Setenv("DOCKER_HOST", "tcp://"+strings.TrimPrefix(server.URL, "http://"))
	t.Setenv("DOCKER_API_VERSION", "1.48")
	t.Setenv("HOME", t.TempDir())
	docker, err := runtime.NewDocker(slog.New(slog.NewTextHandler(io.Discard, nil)), taskServiceIntentStore{}, runtime.SuspendStop)
	if err != nil {
		t.Fatal(err)
	}
	defer docker.Close()

	for _, verification := range []bool{false, true} {
		t.Run(map[bool]string{false: "without verification", true: "with verification"}[verification], func(t *testing.T) {
			serviceCtx, cancel := context.WithCancel(context.Background())
			defer cancel()
			repository := t.TempDir()
			manager := workspace.NewManager(serviceCtx, taskServiceRuntime{}, runtime.Spec{Name: "task-service"}, nil,
				func(context.Context, runtime.Endpoint) (bool, error) { return true, nil }, nil)
			cfg := config.Config{
				Workspace: config.Workspace{Name: map[bool]string{false: "tasks-basic", true: "tasks-verified"}[verification], Image: "image:test", Repo: repository,
					GitHub: &config.WorkspaceGitHub{Mode: config.GitHubModeWorkspaceGH, Hostname: "github.com", Repository: config.GitHubRepository{ID: 123, FullName: "owner/repository"}}},
				Tasks: &config.TaskPolicy{Agent: "build", Model: config.TaskModel{Provider: "fixture", ID: "fixture-model"},
					AttemptTimeout: time.Hour, LeaseDuration: time.Minute, BackgroundImage: "background:test", BackgroundImageID: taskServiceBackgroundImageID,
					Budget: config.TaskBudget{MaxTurns: 10}},
			}
			if verification {
				cfg.Tasks.Verification = &config.TaskVerificationPolicy{CheckName: "repository-tests", Argv: []string{"/bin/sh", "-c", "true"}, Timeout: time.Second, OutputBytes: 1024}
			}
			status := observability.NewRegistry()
			listener, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			controlDirectory := t.TempDir()
			if err := os.Chmod(controlDirectory, 0o700); err != nil {
				t.Fatal(err)
			}
			controlStore, err := control.Open(controlDirectory, cfg.Workspace.Name)
			if err != nil {
				t.Fatal(err)
			}
			route, err := backgroundroute.New(listener, "https://fern.example.ts.net:"+strings.TrimPrefix(listener.Addr().String(), "127.0.0.1:"), controlStore)
			if err != nil {
				t.Fatal(err)
			}
			defer route.Close()
			services, err := newTaskServices(serviceCtx, cfg, docker, manager, route, structServerAuth(), status, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err != nil {
				t.Fatal(err)
			}
			if services == nil || services.store == nil || services.handler == nil || services.coordinator == nil || services.execution == nil ||
				services.result == nil || (services.verification != nil) != verification || services.publication != nil {
				t.Fatalf("task service matrix = %+v", services)
			}
			qualified := false
			for _, component := range status.Snapshot().Components {
				if component.Component == observability.ComponentBackgroundRunProfile && component.State == observability.StateQualified && component.Ready {
					qualified = true
				}
			}
			if !qualified {
				t.Fatalf("background source profile was not reported qualified: %+v", status.Snapshot())
			}
			if services.background == nil || services.provider == nil {
				t.Fatal("configured background run coordinator was not composed")
			}
			if err := services.Close(); err != nil {
				t.Fatal(err)
			}
			if err := manager.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestTaskResultStoreCorruptionIsFatal(t *testing.T) {
	service := &taskServices{
		result: corruptResultCoordinator{}, resultWake: make(chan struct{}, 1), status: observability.NewRegistry(),
	}
	service.resultWake <- struct{}{}
	err := runTaskResultCoordinator(context.Background(), service, slog.New(slog.NewTextHandler(io.Discard, nil)), "demo")
	if !errors.Is(err, taskstore.ErrCorruptStore) {
		t.Fatalf("result coordinator error = %v", err)
	}
}

func TestTaskWorkerIDIsBoundedAndUnique(t *testing.T) {
	t.Parallel()
	first, err := taskWorkerID()
	if err != nil {
		t.Fatal(err)
	}
	second, err := taskWorkerID()
	if err != nil {
		t.Fatal(err)
	}
	if first == second || len(first) > 64 || len(first) < 2 {
		t.Fatalf("worker IDs = %q %q", first, second)
	}
}

func TestVerificationGitExecutableIsNativeHostBinary(t *testing.T) {
	path := verificationGitExecutable()
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		t.Fatalf("verification Git path = %q", path)
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		t.Fatalf("verification Git is not executable: %q: %v", path, err)
	}
	if goruntime.GOOS == "darwin" && path == "/usr/bin/git" {
		t.Fatal("verification selected Apple's non-relocatable /usr/bin/git shim")
	}
}

func TestGitHubOnboardingRequiresRemoteHTTPSAndUsesOperatorSetupOrigin(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if handler, err := newGitHubOnboarding(config.Config{}); err != nil || handler != nil {
		t.Fatalf("local onboarding = %v, %v", handler, err)
	}
	handler, err := newGitHubOnboarding(config.Config{
		RemoteOrigin: "https://fern.example.ts.net", OperatorListen: "127.0.0.1:8081",
		Workspace: config.Workspace{Name: "demo", GitHub: &config.WorkspaceGitHub{Mode: config.GitHubModeGitHubAppBroker}},
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/fern/github/app/setup?return=%2Ffern%2Fcontrol", nil)
	request.Host = "127.0.0.1:8081"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("setup = %d %s", response.Code, response.Body.String())
	}
}

func TestGitHubOnboardingIsDisabledForWorkspaceGH(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	handler, err := newGitHubOnboarding(config.Config{
		RemoteOrigin: "https://fern.example.ts.net", OperatorListen: "127.0.0.1:8081",
		Workspace: config.Workspace{Name: "demo", GitHub: &config.WorkspaceGitHub{Mode: config.GitHubModeWorkspaceGH}},
	})
	if err != nil || handler != nil {
		t.Fatalf("workspace-gh onboarding = %v, %v", handler, err)
	}
}

func TestPublicationCoordinatorIsAbsentForWorkspaceGH(t *testing.T) {
	coordinator, err := newPublicationCoordinator(context.Background(), "", config.Config{
		Workspace: config.Workspace{GitHub: &config.WorkspaceGitHub{Mode: config.GitHubModeWorkspaceGH}},
	}, "", "", nil, task.ActorSnapshot{}, nil, nil, nil, nil, nil)
	if err != nil || coordinator != nil {
		t.Fatalf("workspace-gh publication coordinator = %v, %v", coordinator, err)
	}
}

func installTaskAppCredentials(t *testing.T) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	privateKey := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	payload, err := json.Marshal(struct {
		Version       int    `json:"version"`
		AppID         int64  `json:"app_id"`
		ClientID      string `json:"client_id"`
		ClientSecret  string `json:"client_secret"`
		WebhookSecret string `json:"webhook_secret"`
		PrivateKeyPEM string `json:"private_key_pem"`
	}{1, 789, "client-id", "client-secret", "", string(privateKey)})
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := githubapp.ParseStoredCredentials(payload)
	if err != nil {
		t.Fatal(err)
	}
	directory, err := statePath("github-app")
	if err != nil {
		t.Fatal(err)
	}
	store, err := githubapp.NewCredentialStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(credentials); err != nil {
		t.Fatal(err)
	}
}

func structServerAuth() runtime.ServerAuth {
	return runtime.ServerAuth{Password: "backend-password"}
}
