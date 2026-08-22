//go:build !unix

package verification

import (
	"errors"
	"os"
	"os/exec"
	"time"
)

func configureProcessGroup(command *exec.Cmd) error {
	command.WaitDelay = 2 * time.Second
	return errors.New("process-group containment is unsupported")
}

func teardownProcessGroup(*exec.Cmd) error {
	return errors.New("process-group containment is unsupported")
}

func processSignal(*os.ProcessState) string { return "" }
