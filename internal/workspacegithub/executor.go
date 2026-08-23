package workspacegithub

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/nebler/fern/internal/runtime"
	"github.com/nebler/fern/internal/workspace"
)

type RequestManager interface {
	AcquireRequest(context.Context, workspace.RequestIntent) (workspace.RequestTarget, func(), error)
}

type GHRuntime interface {
	ExecWorkspaceGH(context.Context, string, string, ...string) (runtime.CommandResult, error)
}

type ManagedExecutor struct {
	manager   RequestManager
	runtime   GHRuntime
	workspace string
	timeout   time.Duration
}

func NewManagedExecutor(manager RequestManager, commandRuntime GHRuntime, workspaceName string, timeout time.Duration) (*ManagedExecutor, error) {
	if manager == nil || commandRuntime == nil || !validWorkspaceName(workspaceName) || timeout <= 0 || timeout > 5*time.Minute {
		return nil, errors.New("valid managed workspace gh executor configuration is required")
	}
	return &ManagedExecutor{manager: manager, runtime: commandRuntime, workspace: workspaceName, timeout: timeout}, nil
}

func validWorkspaceName(value string) bool {
	if len(value) < 1 || len(value) > 128 || value[0] == '-' || strings.ContainsAny(value, "\x00\r\n") {
		return false
	}
	for _, character := range value {
		if character != '-' && character != '_' && character != '.' &&
			(character < 'a' || character > 'z') && (character < 'A' || character > 'Z') &&
			(character < '0' || character > '9') {
			return false
		}
	}
	return true
}

func (e *ManagedExecutor) Run(ctx context.Context, arguments ...string) ([]byte, error) {
	operation, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()
	target, release, err := e.manager.AcquireRequest(operation, workspace.RequestWork)
	if err != nil {
		return nil, ErrUnavailable
	}
	if release == nil {
		return nil, ErrUnavailable
	}
	defer release()
	result, err := e.runtime.ExecWorkspaceGH(operation, e.workspace, target.ImageID, arguments...)
	if operation.Err() != nil {
		return nil, operation.Err()
	}
	if err != nil {
		return nil, ErrUnavailable
	}
	return result.Stdout, nil
}

var _ Executor = (*ManagedExecutor)(nil)
