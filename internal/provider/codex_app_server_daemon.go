package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

const (
	codexSharedDaemonStatusTimeout = 1500 * time.Millisecond
	codexSharedDaemonMaxTimeout    = 3 * time.Second
	codexSharedDaemonMaxStatus     = 64 * 1024
)

var codexSharedDaemonVersionPattern = regexp.MustCompile(
	`^[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`,
)

// CodexSharedDaemonStatus is the non-secret subset of
// `codex app-server daemon version` needed to connect to the official UDS
// WebSocket endpoint. The socket snapshot is intentionally private and is
// rechecked by NewCodexSharedAppServerClient immediately before dialing.
type CodexSharedDaemonStatus struct {
	SocketPath          string
	CLIVersion          string
	AppServerVersion    string
	ManagedCodexPath    string
	ManagedCodexVersion string

	socketSnapshot os.FileInfo
}

type codexSharedDaemonStatusWire struct {
	Status              string  `json:"status"`
	SocketPath          string  `json:"socketPath"`
	CLIVersion          string  `json:"cliVersion"`
	AppServerVersion    string  `json:"appServerVersion"`
	ManagedCodexPath    *string `json:"managedCodexPath"`
	ManagedCodexVersion *string `json:"managedCodexVersion"`
}

// QueryCodexSharedDaemon executes a bounded, read-only status query through
// the configured Codex executable. It never starts or changes daemon state.
func QueryCodexSharedDaemon(ctx context.Context, codexBinary string, timeout time.Duration) (CodexSharedDaemonStatus, error) {
	var status CodexSharedDaemonStatus
	var err error
	codexBinary, err = validateCodexSharedDaemonExecutable(codexBinary)
	if err != nil {
		return status, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	timeout = boundedCodexSharedDaemonTimeout(timeout)
	queryCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var stdout boundedCodexDaemonOutput
	cmd := exec.CommandContext(queryCtx, codexBinary, "app-server", "daemon", "version")
	cmd.Stdin = nil
	cmd.Stdout = &stdout
	cmd.Stderr = io.Discard
	cmd.WaitDelay = 250 * time.Millisecond
	if err := cmd.Run(); err != nil {
		if queryCtx.Err() != nil {
			return status, errors.New("Codex shared app-server daemon status query timed out")
		}
		return status, fmt.Errorf("Codex shared app-server daemon status query failed: %w", err)
	}
	if stdout.overflow {
		return status, errors.New("Codex shared app-server daemon returned an oversized status")
	}

	var wire codexSharedDaemonStatusWire
	if err := json.Unmarshal(stdout.bytes, &wire); err != nil {
		return status, errors.New("Codex shared app-server daemon returned invalid status JSON")
	}
	if strings.TrimSpace(wire.Status) != "running" {
		return status, errors.New("Codex shared app-server daemon is not running")
	}
	status = CodexSharedDaemonStatus{
		SocketPath:          strings.TrimSpace(wire.SocketPath),
		CLIVersion:          strings.TrimSpace(wire.CLIVersion),
		AppServerVersion:    strings.TrimSpace(wire.AppServerVersion),
		ManagedCodexPath:    trimOptionalString(wire.ManagedCodexPath),
		ManagedCodexVersion: trimOptionalString(wire.ManagedCodexVersion),
	}
	if err := validateCodexSharedDaemonStatus(status); err != nil {
		return CodexSharedDaemonStatus{}, err
	}
	socketSnapshot, err := validateCodexSharedDaemonSocket(status.SocketPath)
	if err != nil {
		return CodexSharedDaemonStatus{}, err
	}
	status.socketSnapshot = socketSnapshot
	return status, nil
}

// StartCodexSharedDaemon starts the already-installed managed daemon. It does
// not install Codex, bootstrap the updater, or enable cloud remote control.
// Callers must gate this mutation behind explicit configuration.
func StartCodexSharedDaemon(ctx context.Context, codexBinary string, timeout time.Duration) error {
	codexBinary, err := validateCodexSharedDaemonExecutable(codexBinary)
	if err != nil {
		return err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	timeout = boundedCodexSharedDaemonTimeout(timeout)
	startCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var stdout boundedCodexDaemonOutput
	cmd := exec.CommandContext(startCtx, codexBinary, "app-server", "daemon", "start")
	cmd.Stdin = nil
	cmd.Stdout = &stdout
	cmd.Stderr = io.Discard
	cmd.WaitDelay = 250 * time.Millisecond
	if err := cmd.Run(); err != nil {
		if startCtx.Err() != nil {
			return errors.New("Codex shared app-server daemon start timed out")
		}
		return fmt.Errorf("Codex shared app-server daemon start failed: %w", err)
	}
	if stdout.overflow {
		return errors.New("Codex shared app-server daemon returned an oversized start response")
	}
	var response map[string]any
	if err := json.Unmarshal(stdout.bytes, &response); err != nil ||
		strings.TrimSpace(stringAny(response["status"])) == "" {
		return errors.New("Codex shared app-server daemon returned an invalid start response")
	}
	return nil
}

func validateCodexSharedDaemonExecutable(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" || !filepath.IsAbs(path) {
		return "", errors.New("Codex shared app-server requires an absolute executable path")
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("Codex shared app-server executable is unavailable: %w", err)
	}
	info, err := os.Lstat(canonical)
	if err != nil {
		return "", fmt.Errorf("Codex shared app-server executable is unavailable: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", errors.New("Codex shared app-server path is not a direct executable file")
	}
	if info.Mode().Perm()&0o022 != 0 {
		return "", errors.New("Codex shared app-server executable is writable by another user")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || (int(stat.Uid) != os.Getuid() && stat.Uid != 0) {
		return "", errors.New("Codex shared app-server executable has an untrusted owner")
	}
	parent, err := os.Lstat(filepath.Dir(canonical))
	if err != nil || !parent.IsDir() || parent.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("Codex shared app-server executable parent is not a direct directory")
	}
	if parent.Mode().Perm()&0o022 != 0 {
		return "", errors.New("Codex shared app-server executable parent is writable by another user")
	}
	parentStat, ok := parent.Sys().(*syscall.Stat_t)
	if !ok || (int(parentStat.Uid) != os.Getuid() && parentStat.Uid != 0) {
		return "", errors.New("Codex shared app-server executable parent has an untrusted owner")
	}
	return canonical, nil
}

func validateCodexSharedDaemonStatus(status CodexSharedDaemonStatus) error {
	if !filepath.IsAbs(status.SocketPath) {
		return errors.New("Codex shared app-server daemon socket path is not absolute")
	}
	if !validCodexSharedDaemonVersion(status.CLIVersion) ||
		!validCodexSharedDaemonVersion(status.AppServerVersion) {
		return errors.New("Codex shared app-server daemon returned invalid version metadata")
	}
	if status.CLIVersion != status.AppServerVersion {
		return fmt.Errorf(
			"Codex shared app-server version mismatch: cli=%s app-server=%s",
			status.CLIVersion,
			status.AppServerVersion,
		)
	}
	if status.ManagedCodexPath != "" && !filepath.IsAbs(status.ManagedCodexPath) {
		return errors.New("Codex shared app-server daemon returned a relative managed Codex path")
	}
	if status.ManagedCodexVersion != "" && !validCodexSharedDaemonVersion(status.ManagedCodexVersion) {
		return errors.New("Codex shared app-server daemon returned an invalid managed Codex version")
	}
	return nil
}

func validateCodexSharedDaemonSocket(path string) (os.FileInfo, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("Codex shared app-server daemon socket path is not absolute")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("Codex shared app-server daemon socket is unavailable: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || info.Mode()&os.ModeSocket == 0 {
		return nil, errors.New("Codex shared app-server daemon path is not a direct Unix socket")
	}
	if info.Mode().Perm() != 0o600 {
		return nil, errors.New("Codex shared app-server daemon socket permissions must be 0600")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Getuid() {
		return nil, errors.New("Codex shared app-server daemon socket is not owned by the current user")
	}

	parent, err := os.Lstat(filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("Codex shared app-server daemon socket parent is unavailable: %w", err)
	}
	if parent.Mode()&os.ModeSymlink != 0 || !parent.IsDir() {
		return nil, errors.New("Codex shared app-server daemon socket parent is not a direct directory")
	}
	if parent.Mode().Perm()&0o022 != 0 {
		return nil, errors.New("Codex shared app-server daemon socket parent is writable by another user")
	}
	parentStat, ok := parent.Sys().(*syscall.Stat_t)
	if !ok || int(parentStat.Uid) != os.Getuid() {
		return nil, errors.New("Codex shared app-server daemon socket parent is not owned by the current user")
	}
	return info, nil
}

func validCodexSharedDaemonVersion(version string) bool {
	return codexSharedDaemonVersionPattern.MatchString(version)
}

func trimOptionalString(value *string) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(*value)
}

func boundedCodexSharedDaemonTimeout(timeout time.Duration) time.Duration {
	if timeout <= 0 {
		return codexSharedDaemonStatusTimeout
	}
	if timeout > codexSharedDaemonMaxTimeout {
		return codexSharedDaemonMaxTimeout
	}
	return timeout
}

type boundedCodexDaemonOutput struct {
	bytes    []byte
	overflow bool
}

func (w *boundedCodexDaemonOutput) Write(payload []byte) (int, error) {
	remaining := codexSharedDaemonMaxStatus - len(w.bytes)
	if remaining > 0 {
		n := len(payload)
		if n > remaining {
			n = remaining
		}
		w.bytes = append(w.bytes, payload[:n]...)
	}
	if len(payload) > remaining {
		w.overflow = true
	}
	// Drain all output so a noisy child cannot block on a full pipe while
	// retaining only the bounded prefix.
	return len(payload), nil
}
