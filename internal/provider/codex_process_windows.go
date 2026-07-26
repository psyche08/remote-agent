//go:build windows

package provider

import (
	"os"
	"os/exec"
)

func configureCodexOwnedProcess(cmd *exec.Cmd) {}

func signalCodexOwnedProcess(process *os.Process, force bool) error {
	if process == nil {
		return nil
	}
	return process.Kill()
}
