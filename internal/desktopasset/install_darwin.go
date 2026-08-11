//go:build darwin

package desktopasset

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/psyche08/remote-agent/internal/computeruse"
)

// LaunchAgentLabel is the AgentHalo launchd job that runs the helper. It must
// match the label written by mac/launchagent/install.sh.
const LaunchAgentLabel = "dev.linsheng.agenthalo.desktop"

// DefaultHelperPath is where the agent writes the helper. It is under the
// user's own Application Support because the helper runs in the user's GUI
// session — the only place a process can hold the display shield and post
// synthetic input.
func DefaultHelperPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "Library", "Application Support", "AgentHalo", "bin",
		"agenthalo-desktop")
}

// EnsureCurrent writes the embedded helper to path and asks launchd to restart
// an already loaded job. The restart is required even when bytes are current:
// the helper reads computer-use enablement, socket paths and TTLs only at
// startup, so a config-only deployment must reload it too.
//
// Without the restart an update would land on disk and change nothing: launchd
// keeps running the process it already started, so a device would report the
// new version while the old helper kept enforcing the old safeguards.
//
// A missing or unloaded job is not an error. The LaunchAgent is installed once
// by an operator; until then the agent's job is only to keep the binary on disk
// and current.
func EnsureCurrent(path string, helperSocket string) (bool, error) {
	if path == "" {
		return false, fmt.Errorf("no helper path")
	}
	replaced, err := materializeCurrent(path)
	if err != nil {
		return false, err
	}
	target := fmt.Sprintf("gui/%d/%s", os.Getuid(), LaunchAgentLabel)
	if err := restartLaunchAgentIfLoaded(target, helperSocket); err != nil {
		return replaced, err
	}
	return replaced, nil
}

var materializeCurrent = Materialize

var launchctlRun = func(args ...string) ([]byte, error) {
	cmd := exec.Command("launchctl", args...)
	cmd.WaitDelay = 10 * time.Second
	return cmd.CombinedOutput()
}

var prepareHelperRestart = func(socketPath string) error {
	socketPath = expandUserPath(socketPath)
	if socketPath == "" {
		return errors.New("no desktop helper socket path")
	}
	controller := computeruse.NewController(socketPath, true, true)
	return controller.PrepareForRestart()
}

func expandUserPath(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			if path == "~" {
				return home
			}
			return filepath.Join(home, strings.TrimPrefix(path, "~/"))
		}
	}
	return path
}

func restartLaunchAgentIfLoaded(target string, helperSocket string) error {
	loaded, err := launchAgentIsLoaded(target)
	if err != nil || !loaded {
		return err
	}
	return restartLoadedLaunchAgent(target, helperSocket)
}

func launchAgentIsLoaded(target string) (bool, error) {
	out, err := launchctlRun("print", target)
	if err != nil {
		if launchctlJobMissing(out) {
			return false, nil
		}
		return false, launchctlFailure("inspect", target, out, err)
	}
	return true, nil
}

func restartLoadedLaunchAgent(target string, helperSocket string) error {
	// kickstart -k does not promise that the old process gets to finish its
	// SIGTERM cleanup. Ask the still-running helper to atomically stop admission,
	// drain operations and confirm safe_to_restart first. An older helper that
	// does not implement that barrier is intentionally not force-upgraded; it
	// stays alive until an operator performs a controlled migration.
	if err := prepareHelperRestart(helperSocket); err != nil {
		return fmt.Errorf("desktop helper is not safe to restart: %w", err)
	}

	// The helper has now synchronously confirmed that it owns no transition,
	// quarantine, or live shield. Replacing the process cannot uncover an
	// unlocked Locked Use desktop.
	out, err := launchctlRun("kickstart", "-k", target)
	if err != nil {
		// An initially unloaded job is allowed above. Disappearing after a
		// successful safety handshake is different: there is now no confirmed
		// replacement helper, so the agent must disable the capability.
		return launchctlFailure("restart", target, out, err)
	}
	return nil
}

func launchctlJobMissing(output []byte) bool {
	detail := strings.ToLower(string(output))
	for _, marker := range []string{
		"could not find service",
		"could not find domain",
		"service not found",
		"no such process",
	} {
		if strings.Contains(detail, marker) {
			return true
		}
	}
	return false
}

func launchctlFailure(operation, target string, output []byte, err error) error {
	detail := strings.TrimSpace(string(output))
	if detail == "" {
		return fmt.Errorf("launchctl could not %s %s: %w", operation, target, err)
	}
	return fmt.Errorf("launchctl could not %s %s: %w: %s", operation, target, err, detail)
}
