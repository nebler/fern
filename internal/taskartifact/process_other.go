//go:build !unix

package taskartifact

import (
	"os/exec"
	"time"
)

func configureProcessGroup(command *exec.Cmd) { command.WaitDelay = 2 * time.Second }
