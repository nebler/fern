//go:build unix

package verification

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

func configureProcessGroup(command *exec.Cmd) error {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		if command.Process == nil {
			return os.ErrProcessDone
		}
		err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		if err == syscall.ESRCH {
			return os.ErrProcessDone
		}
		return err
	}
	command.WaitDelay = 2 * time.Second
	return nil
}

func teardownProcessGroup(command *exec.Cmd) error {
	if command.Process == nil {
		return errors.New("process group was not established")
	}
	err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	if err == nil || err == syscall.ESRCH {
		return nil
	}
	return err
}

func processSignal(state *os.ProcessState) string {
	status, ok := state.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() {
		return ""
	}
	return status.Signal().String()
}
