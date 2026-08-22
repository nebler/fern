//go:build !unix

package taskpublication

import (
	"os/exec"
	"time"
)

func configureProcessGroup(command *exec.Cmd) {
	command.WaitDelay = 2 * time.Second
}
