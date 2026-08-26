package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	goruntime "runtime"
	"testing"
	"time"

	"github.com/nebler/fern/internal/config"
	"github.com/nebler/fern/internal/observability"
	"github.com/nebler/fern/internal/runtime"
	"github.com/nebler/fern/internal/taskstore"
)

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

func structServerAuth() runtime.ServerAuth {
	return runtime.ServerAuth{Password: "backend-password"}
}
