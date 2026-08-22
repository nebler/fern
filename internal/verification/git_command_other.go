//go:build !darwin && !linux

package verification

import (
	"context"
	"errors"
	"os/exec"
)

func gitCommand(context.Context, string, executableIdentity, ...string) (*exec.Cmd, func(), error) {
	return nil, nil, errors.New("pinned Git launch is unsupported")
}
