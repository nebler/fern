package workspacegithub

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/nebler/fern/internal/runtime"
	"github.com/nebler/fern/internal/workspace"
)

type fakeManager struct {
	target   workspace.RequestTarget
	err      error
	intent   workspace.RequestIntent
	releases int
}

func (manager *fakeManager) AcquireRequest(_ context.Context, intent workspace.RequestIntent) (workspace.RequestTarget, func(), error) {
	manager.intent = intent
	if manager.err != nil {
		return workspace.RequestTarget{}, nil, manager.err
	}
	return manager.target, func() { manager.releases++ }, nil
}

type fakeGHRuntime struct {
	workspace string
	image     string
	args      []string
	result    runtime.CommandResult
	err       error
}

func (command *fakeGHRuntime) ExecWorkspaceGH(_ context.Context, workspaceName, image string, args ...string) (runtime.CommandResult, error) {
	command.workspace, command.image, command.args = workspaceName, image, append([]string(nil), args...)
	return command.result, command.err
}

func TestManagedExecutorUsesWorkLeaseAndAttestedImage(t *testing.T) {
	manager := &fakeManager{target: workspace.RequestTarget{ImageID: "sha256:trusted", Generation: 3}}
	command := &fakeGHRuntime{result: runtime.CommandResult{Stdout: []byte(`{"ok":true}`), Stderr: []byte("ignored")}}
	executor, err := NewManagedExecutor(manager, command, "demo", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	output, err := executor.Run(context.Background(), "auth", "status")
	if err != nil || string(output) != `{"ok":true}` || manager.intent != workspace.RequestWork || manager.releases != 1 ||
		command.workspace != "demo" || command.image != "sha256:trusted" || !reflect.DeepEqual(command.args, []string{"auth", "status"}) {
		t.Fatalf("output=%q err=%v manager=%+v command=%+v", output, err, manager, command)
	}
}

func TestManagedExecutorRedactsRuntimeErrorsAndReleases(t *testing.T) {
	manager := &fakeManager{target: workspace.RequestTarget{ImageID: "sha256:trusted"}}
	command := &fakeGHRuntime{err: errors.New("gho_secret")}
	executor, _ := NewManagedExecutor(manager, command, "demo", time.Second)
	if _, err := executor.Run(context.Background(), "api", "user"); !errors.Is(err, ErrUnavailable) || manager.releases != 1 {
		t.Fatalf("error=%v releases=%d", err, manager.releases)
	}
}

func TestNewManagedExecutorRejectsUnsafeWorkspaceAndTimeout(t *testing.T) {
	for _, test := range []struct {
		workspace string
		timeout   time.Duration
	}{
		{"../demo", time.Second}, {"-demo", time.Second}, {"demo\nother", time.Second}, {"demo", 0}, {"demo", 5*time.Minute + time.Second},
	} {
		if _, err := NewManagedExecutor(&fakeManager{}, &fakeGHRuntime{}, test.workspace, test.timeout); err == nil {
			t.Fatalf("workspace=%q timeout=%s accepted", test.workspace, test.timeout)
		}
	}
}
