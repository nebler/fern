package verification

import (
	"context"
	"crypto/sha256"
	"os/exec"
)

type executableIdentity struct {
	device uint64
	inode  uint64
	size   int64
	mode   uint32
	uid    uint32
	gid    uint32
	sha256 [sha256.Size]byte
}

func pinnedCommand(ctx context.Context, configuredPath string, identity executableIdentity, arguments ...string) (*exec.Cmd, func(), error) {
	executable, err := preparePinnedExecutable(configuredPath, identity)
	if err != nil {
		return nil, nil, err
	}
	command := exec.CommandContext(ctx, executable.path, arguments...)
	// Keep the configured path as argv[0]; Cmd.Path selects the pinned executable.
	command.Args[0] = configuredPath
	command.ExtraFiles = executable.extraFiles
	return command, executable.close, nil
}
