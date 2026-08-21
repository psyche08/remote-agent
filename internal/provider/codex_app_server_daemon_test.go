package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/psyche08/remote-agent/internal/config"
)

const (
	codexSharedDaemonTestStatusEnv = "AGENTHALO_TEST_CODEX_DAEMON_STATUS"
	codexSharedDaemonTestStartEnv  = "AGENTHALO_TEST_CODEX_DAEMON_START"
	codexSharedDaemonTestSleepEnv  = "AGENTHALO_TEST_CODEX_DAEMON_SLEEP"
	codexSharedDaemonTestExitEnv   = "AGENTHALO_TEST_CODEX_DAEMON_EXIT"
)

func TestQueryCodexSharedDaemon(t *testing.T) {
	socket, listener := listenCodexSharedDaemonTestSocket(t)
	defer listener.Close()
	binary := writeCodexSharedDaemonTestBinary(t)
	t.Setenv(codexSharedDaemonTestStatusEnv, marshalCodexSharedDaemonTestStatus(t, map[string]any{
		"status":                "running",
		"socketPath":            socket,
		"cliVersion":            "0.146.0-alpha.3.1",
		"appServerVersion":      "0.146.0-alpha.3.1",
		"managedCodexPath":      "/Users/test/.codex/packages/standalone/current/codex",
		"managedCodexVersion":   nil,
		"futureCompatibleField": true,
	}))

	status := mustQueryCodexSharedDaemon(t, binary)
	if status.SocketPath != socket {
		t.Fatalf("socket path=%q want=%q", status.SocketPath, socket)
	}
	if status.CLIVersion != "0.146.0-alpha.3.1" || status.AppServerVersion != status.CLIVersion {
		t.Fatalf("unexpected versions: %#v", status)
	}
}

func TestQueryCodexSharedDaemonRejectsNonRunningStatus(t *testing.T) {
	binary := writeCodexSharedDaemonTestBinary(t)
	t.Setenv(codexSharedDaemonTestStatusEnv, marshalCodexSharedDaemonTestStatus(t, map[string]any{
		"status":           "stopped",
		"socketPath":       "/private/tmp/not-running.sock",
		"cliVersion":       "0.146.0",
		"appServerVersion": "0.146.0",
	}))

	_, err := QueryCodexSharedDaemon(context.Background(), binary, time.Second)
	requireCodexSharedDaemonFailure(t, err, "not running")
}

func TestStartCodexSharedDaemonIsExplicitAndBounded(t *testing.T) {
	binary := writeCodexSharedDaemonTestBinary(t)
	t.Setenv(codexSharedDaemonTestStartEnv, `{"status":"started"}`)
	if err := StartCodexSharedDaemon(context.Background(), binary, time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestCodexSharedDaemonDoesNotAutostartWithoutExplicitConfig(t *testing.T) {
	binary := writeCodexSharedDaemonTestBinary(t)
	t.Setenv(codexSharedDaemonTestExitEnv, "1")
	// If ensureClient unexpectedly invokes start, this deliberately malformed
	// response changes the returned error and makes the mutation observable.
	t.Setenv(codexSharedDaemonTestStartEnv, `not-json`)
	c := NewCodex("codex", config.ProviderConfig{Extra: map[string]any{
		"app_server_transport":  "shared_daemon",
		"shared_daemon_command": binary,
	}})
	defer c.Shutdown()

	_, err := c.ensureClient()
	if err == nil || !strings.Contains(err.Error(), "status query failed") {
		t.Fatalf("ensureClient error=%v, want status failure without daemon start", err)
	}
}

func TestCodexSharedDaemonAutostartSurfacesStartFailure(t *testing.T) {
	binary := writeCodexSharedDaemonTestBinary(t)
	t.Setenv(codexSharedDaemonTestExitEnv, "1")
	t.Setenv(codexSharedDaemonTestStartEnv, `not-json`)
	c := NewCodex("codex", config.ProviderConfig{Extra: map[string]any{
		"app_server_transport":    "shared_daemon",
		"shared_daemon_command":   binary,
		"shared_daemon_autostart": true,
	}})
	defer c.Shutdown()

	_, err := c.ensureClient()
	if err == nil || !strings.Contains(err.Error(), "invalid start response") {
		t.Fatalf("ensureClient error=%v, want explicit daemon start failure", err)
	}
}

func TestQueryCodexSharedDaemonRejectsInvalidVersion(t *testing.T) {
	socket, listener := listenCodexSharedDaemonTestSocket(t)
	defer listener.Close()
	binary := writeCodexSharedDaemonTestBinary(t)
	t.Setenv(codexSharedDaemonTestStatusEnv, marshalCodexSharedDaemonTestStatus(t, map[string]any{
		"status":           "running",
		"socketPath":       socket,
		"cliVersion":       "release-current",
		"appServerVersion": "0.146.0",
	}))

	_, err := QueryCodexSharedDaemon(context.Background(), binary, time.Second)
	requireCodexSharedDaemonFailure(t, err, "invalid version")
}

func TestQueryCodexSharedDaemonRejectsInsecureSocket(t *testing.T) {
	socket, listener := listenCodexSharedDaemonTestSocket(t)
	defer listener.Close()
	if err := os.Chmod(socket, 0o660); err != nil {
		t.Fatal(err)
	}
	binary := writeCodexSharedDaemonTestBinary(t)
	t.Setenv(codexSharedDaemonTestStatusEnv, runningCodexSharedDaemonTestStatus(t, socket))

	_, err := QueryCodexSharedDaemon(context.Background(), binary, time.Second)
	requireCodexSharedDaemonFailure(t, err, "permissions must be 0600")
}

func TestQueryCodexSharedDaemonRejectsSymlinkSocket(t *testing.T) {
	socket, listener := listenCodexSharedDaemonTestSocket(t)
	defer listener.Close()
	link := socket + ".link"
	if err := os.Symlink(socket, link); err != nil {
		t.Fatal(err)
	}
	binary := writeCodexSharedDaemonTestBinary(t)
	t.Setenv(codexSharedDaemonTestStatusEnv, runningCodexSharedDaemonTestStatus(t, link))

	_, err := QueryCodexSharedDaemon(context.Background(), binary, time.Second)
	requireCodexSharedDaemonFailure(t, err, "not a direct Unix socket")
}

func TestQueryCodexSharedDaemonRejectsRelativeSocket(t *testing.T) {
	binary := writeCodexSharedDaemonTestBinary(t)
	t.Setenv(codexSharedDaemonTestStatusEnv, runningCodexSharedDaemonTestStatus(t, "control.sock"))

	_, err := QueryCodexSharedDaemon(context.Background(), binary, time.Second)
	requireCodexSharedDaemonFailure(t, err, "path is not absolute")
}

func TestQueryCodexSharedDaemonRejectsWritableParent(t *testing.T) {
	socket, listener := listenCodexSharedDaemonTestSocket(t)
	defer listener.Close()
	parent := filepath.Dir(socket)
	if err := os.Chmod(parent, 0o770); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })
	binary := writeCodexSharedDaemonTestBinary(t)
	t.Setenv(codexSharedDaemonTestStatusEnv, runningCodexSharedDaemonTestStatus(t, socket))

	_, err := QueryCodexSharedDaemon(context.Background(), binary, time.Second)
	requireCodexSharedDaemonFailure(t, err, "parent is writable by another user")
}

func TestCodexSharedDaemonDialRejectsSocketReplacement(t *testing.T) {
	socket, first := listenCodexSharedDaemonTestSocket(t)
	binary := writeCodexSharedDaemonTestBinary(t)
	t.Setenv(codexSharedDaemonTestStatusEnv, runningCodexSharedDaemonTestStatus(t, socket))

	status := mustQueryCodexSharedDaemon(t, binary)
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(socket)
	second, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if err := os.Chmod(socket, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := dialCodexAppServerWebSocketStatus(status, time.Second); err == nil || !strings.Contains(err.Error(), "changed after discovery") {
		t.Fatalf("error=%v, want replacement failure", err)
	}
}

func TestQueryCodexSharedDaemonRejectsVersionSkew(t *testing.T) {
	socket, listener := listenCodexSharedDaemonTestSocket(t)
	defer listener.Close()
	binary := writeCodexSharedDaemonTestBinary(t)
	t.Setenv(codexSharedDaemonTestStatusEnv, marshalCodexSharedDaemonTestStatus(t, map[string]any{
		"status":           "running",
		"socketPath":       socket,
		"cliVersion":       "0.146.0",
		"appServerVersion": "0.145.0",
	}))

	_, err := QueryCodexSharedDaemon(context.Background(), binary, time.Second)
	requireCodexSharedDaemonFailure(t, err, "version mismatch")
}

func TestQueryCodexSharedDaemonIsBounded(t *testing.T) {
	binary := writeCodexSharedDaemonTestBinary(t)
	t.Setenv(codexSharedDaemonTestStatusEnv, "{}")
	t.Setenv(codexSharedDaemonTestSleepEnv, "2")

	started := time.Now()
	_, err := QueryCodexSharedDaemon(context.Background(), binary, 50*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("error=%v, want timeout", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("bounded status query took %s", elapsed)
	}
}

func TestQueryCodexSharedDaemonRejectsOversizedStatus(t *testing.T) {
	binary := writeCodexSharedDaemonTestBinary(t)
	t.Setenv(codexSharedDaemonTestStatusEnv, strings.Repeat("x", codexSharedDaemonMaxStatus+1))

	_, err := QueryCodexSharedDaemon(context.Background(), binary, time.Second)
	requireCodexSharedDaemonFailure(t, err, "oversized")
}

func TestValidCodexSharedDaemonVersion(t *testing.T) {
	for _, version := range []string{"0.146.0", "0.146.0-alpha.3.1", "12.3.4-rc.1+build.9"} {
		if !validCodexSharedDaemonVersion(version) {
			t.Errorf("valid version %q rejected", version)
		}
	}
	for _, version := range []string{"", "0.146", "v0.146.0", "0.146.0-", "0.146.0-alpha..1"} {
		if validCodexSharedDaemonVersion(version) {
			t.Errorf("invalid version %q accepted", version)
		}
	}
}

func TestLiveCodexSharedDaemonResume(t *testing.T) {
	if os.Getenv("AGENTHALO_TEST_CODEX_SHARED_DAEMON") != "1" {
		t.Skip("set AGENTHALO_TEST_CODEX_SHARED_DAEMON=1 to probe the local managed daemon")
	}
	threadID := strings.TrimSpace(os.Getenv("AGENTHALO_TEST_CODEX_SHARED_THREAD"))
	if threadID == "" {
		t.Skip("set AGENTHALO_TEST_CODEX_SHARED_THREAD to an existing thread UUID")
	}
	binary := strings.TrimSpace(os.Getenv("AGENTHALO_TEST_CODEX_SHARED_BINARY"))
	if binary == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Fatal(err)
		}
		binary = filepath.Join(home, ".codex", "packages", "standalone", "current", "codex")
	}
	status, err := QueryCodexSharedDaemon(context.Background(), binary, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	client := NewCodexSharedAppServerClient(status, t.TempDir(), nil, nil)
	if err := client.Start(); err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.Initialize("agenthalo-live-probe"); err != nil {
		t.Fatal(err)
	}
	result, err := client.ThreadResume(threadID, nil, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	thread := mapAny(mapAny(result)["thread"])
	if got := stringAny(thread["id"]); got != threadID {
		t.Fatalf("thread/resume route mismatch: got=%q want=%q", got, threadID)
	}
	t.Logf("shared daemon %s resumed thread %s", status.AppServerVersion, threadID)
}

func TestLiveCodexSharedDaemonTurn(t *testing.T) {
	if os.Getenv("AGENTHALO_TEST_CODEX_SHARED_TURN") != "1" {
		t.Skip("set AGENTHALO_TEST_CODEX_SHARED_TURN=1 to create a throwaway thread and deliver a real turn")
	}
	binary := strings.TrimSpace(os.Getenv("AGENTHALO_TEST_CODEX_SHARED_BINARY"))
	if binary == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Fatal(err)
		}
		binary = filepath.Join(home, ".codex", "packages", "standalone", "current", "codex")
	}
	const expected = "AGENTHALO_SHARED_DAEMON_OK"
	cwd := t.TempDir()
	codex := NewCodex("codex", config.ProviderConfig{
		AppName: "Codex",
		Cwd:     cwd,
		Extra: map[string]any{
			"shared_daemon_command": binary,
			"approval_policy":       "never",
			"sandbox":               "workspace-write",
		},
	})
	defer codex.Shutdown()

	threadID, err := codex.OpenOrCreateSession("shared-daemon-live-turn", StartOptions{Cwd: cwd})
	if err != nil {
		t.Fatal(err)
	}
	result := codex.SendPrompt("shared-daemon-live-turn", "Reply with exactly: "+expected)
	if !result.OK {
		t.Fatalf("turn delivery failed: %#v", result)
	}

	deadline := time.Now().Add(2 * time.Minute)
	for codex.getLastState() == "running" && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if state := codex.getLastState(); state != "idle" {
		t.Fatalf("turn did not complete: state=%q error=%q", state, codex.getLastError())
	}
	client, err := codex.ensureClient()
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := client.ThreadResume(threadID, nil, 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var reply string
	for _, message := range codexThreadToMessages(codexThreadFromResume(resumed), nativePreviewUnlimited) {
		if stringAny(message["role"]) == "assistant" && stringAny(message["kind"]) == "text" {
			reply = stringAny(message["text"])
		}
	}
	if strings.TrimSpace(reply) != expected {
		t.Fatalf("assistant reply=%q want=%q", reply, expected)
	}
	t.Logf("shared daemon delivered and completed thread %s", threadID)
}

func listenCodexSharedDaemonTestSocket(t *testing.T) (string, net.Listener) {
	t.Helper()
	// Keep the path below Darwin's short sockaddr_un.sun_path limit.
	dir, err := os.MkdirTemp("/tmp", "rcd-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "control.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(socket, 0o600); err != nil {
		listener.Close()
		t.Fatal(err)
	}
	return socket, listener
}

// Writing a fresh fake Codex per test made every bounded status query absorb a
// first-exec evaluation (see writeWarmTestExecutable), which regularly ran
// longer than the timeout under test. Build the fake once per package run and
// warm it, so each test measures only the code under test.
var (
	codexSharedDaemonTestBinaryOnce sync.Once
	codexSharedDaemonTestBinaryDir  string
	codexSharedDaemonTestBinaryPath string
	codexSharedDaemonTestBinaryErr  error
)

// cleanupCodexSharedDaemonTestBinary is called from TestMain, because the fake
// outlives the individual test that first asked for it.
func cleanupCodexSharedDaemonTestBinary() {
	if codexSharedDaemonTestBinaryDir != "" {
		_ = os.RemoveAll(codexSharedDaemonTestBinaryDir)
	}
}

func writeCodexSharedDaemonTestBinary(t *testing.T) string {
	t.Helper()
	codexSharedDaemonTestBinaryOnce.Do(func() {
		codexSharedDaemonTestBinaryPath, codexSharedDaemonTestBinaryErr = buildWarmCodexSharedDaemonTestBinary()
	})
	if codexSharedDaemonTestBinaryErr != nil {
		t.Fatal(codexSharedDaemonTestBinaryErr)
	}
	return codexSharedDaemonTestBinaryPath
}

func buildWarmCodexSharedDaemonTestBinary() (string, error) {
	dir, err := os.MkdirTemp("", "rc-codex-fake-")
	if err != nil {
		return "", fmt.Errorf("create fake Codex directory: %w", err)
	}
	codexSharedDaemonTestBinaryDir = dir
	// QueryCodexSharedDaemon rejects an executable whose parent is writable by
	// another user, so pin the mode instead of inheriting the process umask.
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", fmt.Errorf("restrict fake Codex directory: %w", err)
	}
	path := filepath.Join(dir, "codex-test")
	script := `#!/bin/sh
if [ "$1" != "app-server" ] || [ "$2" != "daemon" ]; then
  exit 97
fi
if [ "$3" = "start" ]; then
  printf '%s' "$AGENTHALO_TEST_CODEX_DAEMON_START"
  exit 0
fi
if [ "$3" != "version" ]; then
  exit 97
fi
if [ -n "$AGENTHALO_TEST_CODEX_DAEMON_SLEEP" ]; then
  sleep "$AGENTHALO_TEST_CODEX_DAEMON_SLEEP"
fi
if [ -n "$AGENTHALO_TEST_CODEX_DAEMON_EXIT" ]; then
  exit "$AGENTHALO_TEST_CODEX_DAEMON_EXIT"
fi
printf '%s' "$AGENTHALO_TEST_CODEX_DAEMON_STATUS"
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		return "", fmt.Errorf("write fake Codex: %w", err)
	}
	if err := warmTestExecutable(path); err != nil {
		return "", err
	}
	return path, nil
}

// mustQueryCodexSharedDaemon runs the status query that a test needs to
// succeed before it can assert anything, and reports a timeout as a harness
// failure rather than a plain query error.
func mustQueryCodexSharedDaemon(t *testing.T, binary string) CodexSharedDaemonStatus {
	t.Helper()
	status, err := QueryCodexSharedDaemon(context.Background(), binary, time.Second)
	if err == nil {
		return status
	}
	if strings.Contains(err.Error(), "status query timed out") {
		t.Fatalf("harness failure: the fake Codex status query timed out before the assertion could run: %v", err)
	}
	t.Fatal(err)
	return CodexSharedDaemonStatus{}
}

// requireCodexSharedDaemonFailure asserts a specific rejection reason and
// reports a status-query timeout as a distinct harness failure, so an
// environment problem can never masquerade as a security assertion that ran
// and merely produced a different message.
func requireCodexSharedDaemonFailure(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("error=nil, want %q failure", want)
	}
	if strings.Contains(err.Error(), "status query timed out") {
		t.Fatalf("harness failure: the fake Codex status query timed out before the %q assertion could run: %v", want, err)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error=%v, want %q failure", err, want)
	}
}

func runningCodexSharedDaemonTestStatus(t *testing.T, socket string) string {
	t.Helper()
	return marshalCodexSharedDaemonTestStatus(t, map[string]any{
		"status":                "running",
		"socketPath":            socket,
		"cliVersion":            "0.146.0",
		"appServerVersion":      "0.146.0",
		"managedCodexPath":      nil,
		"managedCodexVersion":   nil,
		"futureCompatibleField": true,
	})
}

func marshalCodexSharedDaemonTestStatus(t *testing.T, status map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	return string(payload)
}
