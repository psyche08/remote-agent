//go:build !windows

package provider

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

func configureCodexOwnedProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func signalCodexOwnedProcess(process *os.Process, force bool) error {
	if process == nil {
		return nil
	}
	signal := syscall.SIGTERM
	if force {
		signal = syscall.SIGKILL
	}
	err := syscall.Kill(-process.Pid, signal)
	if err == nil || errors.Is(err, syscall.ESRCH) {
		return nil
	}
	if force {
		return process.Kill()
	}
	return process.Signal(signal)
}
