//go:build darwin

package desktopasset

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// LaunchAgentLabel is the launchd job that runs the helper. It must match the
// label written by mac/launchagent/install.sh.
const LaunchAgentLabel = "com.psyche08.remote-agent-desktop"

// DefaultHelperPath is where the agent writes the helper. It is under the
// user's own Application Support because the helper runs in the user's GUI
// session — the only place a process can hold the display shield and post
// synthetic input.
func DefaultHelperPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "Library", "Application Support", "remote-agent", "bin",
		"remote-agent-desktop")
}

// EnsureCurrent writes the embedded helper to path and, when the bytes changed,
// asks launchd to restart the job so the binary that is running is the one this
// release shipped.
//
// Without the restart an update would land on disk and change nothing: launchd
// keeps running the process it already started, so a device would report the
// new version while the old helper kept enforcing the old safeguards.
//
// A missing or unloaded job is not an error. The LaunchAgent is installed once
// by an operator; until then the agent's job is only to keep the binary on disk
// and current.
func EnsureCurrent(path string) (bool, error) {
	if path == "" {
		return false, fmt.Errorf("no helper path")
	}
	replaced, err := Materialize(path)
	if err != nil || !replaced {
		return replaced, err
	}
	target := fmt.Sprintf("gui/%d/%s", os.Getuid(), LaunchAgentLabel)
	// kickstart -k stops the running job and starts it again. The helper's
	// SIGTERM handler closes any open window and waits for the relock to be
	// confirmed before exiting, so this cannot swap the binary out from under
	// an unlocked desktop.
	cmd := exec.Command("launchctl", "kickstart", "-k", target)
	cmd.WaitDelay = 10 * time.Second
	_ = cmd.Run()
	return true, nil
}
