package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/psyche08/remote-agent/internal/config"
)

func TestClaudeObserverHookReturnsWithoutOutputOrDecision(t *testing.T) {
	interactionRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	interactionDir := filepath.Join(interactionRoot, "interactions")
	input, err := os.CreateTemp(t.TempDir(), "claude-hook-input-*.json")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = input.Close() })
	if _, err := input.WriteString(`{
		"session_id":"session-1","hook_event_name":"PreToolUse",
		"tool_name":"AskUserQuestion","tool_input":{"questions":[]},
		"tool_use_id":"question-1"
	}`); err != nil {
		t.Fatal(err)
	}
	if _, err := input.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	stdoutRead, stdoutWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stderrRead, stderrWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldStdin, oldStdout, oldStderr := os.Stdin, os.Stdout, os.Stderr
	os.Stdin, os.Stdout, os.Stderr = input, stdoutWrite, stderrWrite
	code := runHook([]string{"claude-observe", "--interaction-dir", interactionDir})
	os.Stdin, os.Stdout, os.Stderr = oldStdin, oldStdout, oldStderr
	_ = stdoutWrite.Close()
	_ = stderrWrite.Close()
	stdout, _ := io.ReadAll(stdoutRead)
	stderr, _ := io.ReadAll(stderrRead)
	_ = stdoutRead.Close()
	_ = stderrRead.Close()
	if code != 0 || len(stdout) != 0 || len(stderr) != 0 {
		t.Fatalf("observer result code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	entries, err := os.ReadDir(interactionDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("observer records=%d, want 1", len(entries))
	}
	raw, err := os.ReadFile(filepath.Join(interactionDir, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"permissionDecision", "hookSpecificOutput", "decision", "defer"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("observer persisted a hook decision %q: %s", forbidden, raw)
		}
	}
}

func TestEmbeddedDesktopHelperReloadsAcrossEnabledToDisabledConfig(t *testing.T) {
	previousEmbedded := desktopHelperEmbedded
	previousEnsure := desktopHelperEnsureCurrent
	previousPath := desktopHelperDefaultPath
	desktopHelperEmbedded = func() bool { return true }
	desktopHelperDefaultPath = func() string { return "/tmp/agenthalo-desktop-test" }
	calls := 0
	desktopHelperEnsureCurrent = func(path string, socketPath string) (bool, error) {
		calls++
		if path != "/tmp/agenthalo-desktop-test" {
			t.Fatalf("helper path=%q", path)
		}
		if socketPath != "/tmp/agenthalo-desktop-test.sock" {
			t.Fatalf("helper socket=%q", socketPath)
		}
		return false, nil
	}
	t.Cleanup(func() {
		desktopHelperEmbedded = previousEmbedded
		desktopHelperEnsureCurrent = previousEnsure
		desktopHelperDefaultPath = previousPath
	})

	for _, enabled := range []bool{true, false} {
		cfg := &config.Config{}
		cfg.ComputerUse.Enabled = enabled
		cfg.ComputerUse.HelperSocket = "/tmp/agenthalo-desktop-test.sock"
		if err := ensureDesktopHelperCurrent(cfg); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 2 {
		t.Fatalf("helper reload calls=%d, want one for enabled and one for disabled startup", calls)
	}
}

func TestEmbeddedDesktopHelperReloadPreservesLaunchctlFailure(t *testing.T) {
	previousEmbedded := desktopHelperEmbedded
	previousEnsure := desktopHelperEnsureCurrent
	previousPath := desktopHelperDefaultPath
	desktopHelperEmbedded = func() bool { return true }
	desktopHelperDefaultPath = func() string { return "/tmp/agenthalo-desktop-test" }
	want := errors.New("launchctl kickstart failed")
	desktopHelperEnsureCurrent = func(string, string) (bool, error) { return false, want }
	t.Cleanup(func() {
		desktopHelperEmbedded = previousEmbedded
		desktopHelperEnsureCurrent = previousEnsure
		desktopHelperDefaultPath = previousPath
	})

	cfg := &config.Config{}
	cfg.ComputerUse.Enabled = true
	cfg.ComputerUse.LockedUse.Enabled = true
	cfg.ComputerUse.DebugHTTPActions = true
	if err := refreshDesktopHelperCapability(cfg); !errors.Is(err, want) {
		t.Fatalf("reload err=%v, want wrapped launchctl failure", err)
	}
	if cfg.ComputerUse.Enabled || cfg.ComputerUse.LockedUse.Enabled || cfg.ComputerUse.DebugHTTPActions {
		t.Fatalf("stale helper remained reachable after refresh failure: %#v", cfg.ComputerUse)
	}
	if !cfg.ComputerUse.HelperRefreshFailed {
		t.Fatal("refresh failure did not retain the fail-closed legacy capture marker")
	}
}

func TestApplyListenerOverridesUsesExplicitUDSForInternalHealth(t *testing.T) {
	cfg := &config.Config{Host: "127.0.0.1", Port: 8765, UDS: "/configured.sock"}
	if err := applyListenerOverrides(cfg, "127.0.0.1:0", "/tmp/explicit.sock"); err != nil {
		t.Fatal(err)
	}
	if cfg.UDS != "/tmp/explicit.sock" || cfg.Host != "127.0.0.1" || cfg.Port != 8765 {
		t.Fatalf("UDS override was not made authoritative: %#v", cfg)
	}
}

func TestApplyListenerOverridesUpdatesDevelopmentTCP(t *testing.T) {
	cfg := &config.Config{Host: "127.0.0.1", Port: 8765, UDS: "/configured.sock"}
	if err := applyListenerOverrides(cfg, "localhost:18765", ""); err != nil {
		t.Fatal(err)
	}
	if cfg.UDS != "" || cfg.Host != "localhost" || cfg.Port != 18765 {
		t.Fatalf("TCP override was not reflected in runtime config: %#v", cfg)
	}
}
