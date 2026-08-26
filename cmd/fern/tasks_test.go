package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/nebler/fern/internal/config"
	"github.com/nebler/fern/internal/observability"
	"github.com/nebler/fern/internal/runtime"
	"github.com/nebler/fern/internal/task"
	"github.com/nebler/fern/internal/taskstore"
	"github.com/nebler/fern/internal/workspace"
)

const taskServiceTestImageID = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

type corruptResultCoordinator struct{}

func (corruptResultCoordinator) RunOnce(context.Context) error { return taskstore.ErrCorruptStore }

func TestTaskServicesAreExplicitlyDisabledWhenPolicyIsAbsent(t *testing.T) {
	t.Parallel()
	services, err := newTaskServices(context.Background(), config.Config{}, nil, nil, structServerAuth(), observability.NewRegistry(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil || services != nil {
		t.Fatalf("services=%v err=%v", services, err)
	}
}

func TestTaskServicesRequireExplicitGitHubAuthorityBeforeRuntimeAccess(t *testing.T) {
	t.Parallel()
	cfg := config.Config{Tasks: &config.TaskPolicy{
		Agent: "build", Model: config.TaskModel{Provider: "test", ID: "test-model"},
		AttemptTimeout: time.Hour, LeaseDuration: time.Minute, Budget: config.TaskBudget{MaxTurns: 10},
	}}
	if _, err := newTaskServices(context.Background(), cfg, nil, nil, structServerAuth(), observability.NewRegistry(), slog.New(slog.NewTextHandler(io.Discard, nil))); err == nil {
		t.Fatal("task service accepted no GitHub authority")
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
		if request.Method != http.MethodGet || request.URL.Path != "/v1.48/images/image:test/json" {
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
					AttemptTimeout: time.Hour, LeaseDuration: time.Minute, Budget: config.TaskBudget{MaxTurns: 10}},
			}
			if verification {
				cfg.Tasks.Verification = &config.TaskVerificationPolicy{CheckName: "repository-tests", Argv: []string{"/bin/sh", "-c", "true"}, Timeout: time.Second, OutputBytes: 1024}
			}
			status := observability.NewRegistry()
			services, err := newTaskServices(serviceCtx, cfg, docker, manager, structServerAuth(), status, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err != nil {
				t.Fatal(err)
			}
			if services == nil || services.store == nil || services.handler == nil || services.coordinator == nil || services.execution == nil ||
				services.result == nil || (services.verification != nil) != verification || services.publication != nil {
				t.Fatalf("task service matrix = %+v", services)
			}
			if err := services.store.Close(); err != nil {
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

func structServerAuth() runtime.ServerAuth {
	return runtime.ServerAuth{Password: "backend-password"}
}
