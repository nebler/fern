//go:build darwin

package verification

import (
	"context"
	"os/exec"
)

func gitCommand(ctx context.Context, path string, identity executableIdentity, arguments ...string) (*exec.Cmd, func(), error) {
	return pinnedCommand(ctx, path, identity, arguments...)
}
