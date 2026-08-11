package turnstatehook

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const maxClaudeSettingsBytes = 8 << 20

var events = []struct {
	name  string
	state string
}{
	{name: "UserPromptSubmit", state: "running"},
	{name: "Stop", state: "idle"},
}

func Run(state string, input io.Reader, turnstateDir string) {
	defer func() { _ = recover() }()
	if state == "" {
		state = "idle"
	}
	raw, err := io.ReadAll(input)
	if err != nil {
		return
	}
	var payload map[string]any
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := json.Unmarshal(raw, &payload); err != nil {
			return
		}
	}
	sessionID, _ := payload["session_id"].(string)
	if !safeTurnstateID(sessionID) {
		return
	}
	if turnstateDir == "" {
		turnstateDir = defaultTurnstateDir()
	}
	dir, err := secureInteractionDir(turnstateDir, true)
	if err != nil {
		return
	}
	rec := map[string]any{
		"session_id":      sessionID,
		"state":           state,
		"ts":              float64(time.Now().UnixNano()) / 1e9,
		"cwd":             payload["cwd"],
		"transcript_path": payload["transcript_path"],
		"event":           payload["hook_event_name"],
	}
	path := filepath.Join(dir, sessionID+".json")
	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	_ = writePrivateAtomicFile(path, b)
}

func safeTurnstateID(value string) bool {
	if value == "" || len(value) > 128 || value == "." || value == ".." {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}

func writePrivateAtomicFile(path string, data []byte) error {
	parent := filepath.Dir(path)
	tmp, err := os.CreateTemp(parent, ".agenthalo-turnstate-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	keep := false
	defer func() {
		_ = tmp.Close()
		if !keep {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	if err := syncDirectoryForDurability(parent); err != nil {
		return fmt.Errorf("persist Claude turnstate rename: %w", err)
	}
	keep = true
	return nil
}

func Install(settingsPath string, binaryPath string, turnstateDir string) (map[string]any, error) {
	return InstallWithInteractionDir(settingsPath, binaryPath, turnstateDir, "")
}

// InstallWithInteractionDir installs both lifecycle tracking and the native UI
// observer hooks. The observer hooks never answer Claude: PermissionRequest and
// AskUserQuestion remain owned by the Claude app's native UI.
func InstallWithInteractionDir(
	settingsPath string, binaryPath string, turnstateDir string, interactionDir string,
) (map[string]any, error) {
	if settingsPath == "" {
		settingsPath = filepath.Join("~", ".claude", "settings.json")
	}
	settingsPath = expandUser(settingsPath)
	if binaryPath == "" {
		exe, err := os.Executable()
		if err != nil {
			return nil, err
		}
		binaryPath = exe
	}
	cfg, settingsMode, err := readClaudeSettings(settingsPath)
	if err != nil {
		return nil, err
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	hooks := objectMap(cfg["hooks"])
	cfg["hooks"] = hooks
	for _, ev := range events {
		cmd := shellQuote(binaryPath) + " hook turnstate " + ev.state
		if turnstateDir != "" {
			cmd = "AGENTHALO_TURNSTATE_DIR=" + shellQuote(turnstateDir) + " " + cmd
		}
		groups := objectList(hooks[ev.name])
		replaced := false
		for _, group := range groups {
			items := objectList(group["hooks"])
			for _, item := range items {
				if isTurnstateCommand(stringAny(item["command"])) {
					item["type"] = "command"
					item["command"] = cmd
					replaced = true
				}
			}
			group["hooks"] = anyList(items)
		}
		if !replaced {
			groups = append(groups, map[string]any{"hooks": []any{map[string]any{"type": "command", "command": cmd}}})
		}
		hooks[ev.name] = anyList(groups)
	}
	if interactionDir == "" {
		interactionDir = defaultInteractionDir()
	}
	observerCommand := shellQuote(binaryPath) + " hook claude-observe --interaction-dir " + shellQuote(interactionDir)
	cleanupCommand := observerCommand + " --cleanup"
	hooks["PermissionRequest"] = anyList(installInteractionHook(
		objectList(hooks["PermissionRequest"]), "", observerCommand, isInteractionObserveCommand,
	))
	hooks["PreToolUse"] = anyList(installInteractionHook(
		objectList(hooks["PreToolUse"]), "AskUserQuestion", observerCommand, isInteractionObserveCommand,
	))
	for _, event := range []string{"PreToolUse", "PostToolUse", "PostToolUseFailure", "Stop"} {
		hooks[event] = anyList(installInteractionHook(
			objectList(hooks[event]), "", cleanupCommand, isInteractionCleanupCommand,
		))
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := writeClaudeSettings(settingsPath, append(b, '\n'), settingsMode); err != nil {
		return nil, err
	}
	return cfg, nil
}

func installInteractionHook(
	groups []map[string]any, matcher, command string, isManaged func(string) bool,
) []map[string]any {
	target := -1
	installed := false
	keptGroups := make([]map[string]any, 0, len(groups)+1)
	for _, group := range groups {
		isTarget := strings.TrimSpace(stringAny(group["matcher"])) == matcher
		if isTarget && target < 0 {
			target = len(keptGroups)
		}
		items := objectList(group["hooks"])
		keptItems := make([]map[string]any, 0, len(items)+1)
		for _, item := range items {
			if !isManaged(stringAny(item["command"])) {
				keptItems = append(keptItems, item)
				continue
			}
			if isTarget && !installed {
				item["type"] = "command"
				item["command"] = command
				keptItems = append(keptItems, item)
				installed = true
			}
		}
		group["hooks"] = anyList(keptItems)
		keptGroups = append(keptGroups, group)
	}
	if target < 0 {
		group := map[string]any{
			"hooks": []any{map[string]any{"type": "command", "command": command}},
		}
		if matcher != "" {
			group["matcher"] = matcher
		}
		return append(keptGroups, group)
	}
	if !installed {
		items := objectList(keptGroups[target]["hooks"])
		items = append(items, map[string]any{"type": "command", "command": command})
		keptGroups[target]["hooks"] = anyList(items)
	}
	return keptGroups
}

func readClaudeSettings(path string) (map[string]any, os.FileMode, error) {
	mode := os.FileMode(0o600)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{}, mode, nil
	}
	if err != nil {
		return nil, 0, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, 0, errors.New("Claude settings path must be a regular file, not a symlink")
	}
	if perm := info.Mode().Perm(); perm != 0 && perm&0o077 == 0 {
		mode = perm
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, 0, err
	}
	b, readErr := io.ReadAll(io.LimitReader(f, maxClaudeSettingsBytes+1))
	closeErr := f.Close()
	if readErr != nil {
		return nil, 0, readErr
	}
	if closeErr != nil {
		return nil, 0, closeErr
	}
	if len(b) > maxClaudeSettingsBytes {
		return nil, 0, errors.New("Claude settings file is too large")
	}
	cfg := map[string]any{}
	if len(strings.TrimSpace(string(b))) == 0 {
		return cfg, mode, nil
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, 0, fmt.Errorf("parse Claude settings: %w", err)
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	return cfg, mode, nil
}

func writeClaudeSettings(path string, data []byte, mode os.FileMode) error {
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return errors.New("Claude settings path must remain a regular file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if mode == 0 || mode&0o077 != 0 {
		mode = 0o600
	}
	tmp, err := os.CreateTemp(parent, ".agenthalo-settings-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	keep := false
	defer func() {
		_ = tmp.Close()
		if !keep {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(mode); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	if err := syncDirectoryForDurability(parent); err != nil {
		return fmt.Errorf("persist Claude settings rename: %w", err)
	}
	keep = true
	return nil
}

func defaultTurnstateDir() string {
	if v := os.Getenv("AGENTHALO_TURNSTATE_DIR"); v != "" {
		return v
	}
	return filepath.Join("~", ".claude", "agenthalo-turnstate")
}

func objectMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

func objectList(v any) []map[string]any {
	raw, ok := v.([]any)
	if !ok {
		return nil
	}
	out := []map[string]any{}
	for _, item := range raw {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func anyList(items []map[string]any) []any {
	out := make([]any, 0, len(items))
	for _, item := range items {
		out = append(out, item)
	}
	return out
}

func isTurnstateCommand(cmd string) bool {
	return strings.Contains(cmd, "turnstate_hook.py") || strings.Contains(cmd, " hook turnstate ")
}

func isInteractionObserverCommand(cmd string) bool {
	return strings.Contains(cmd, "--agenthalo-claude-hook") || strings.Contains(cmd, "hook claude-observe")
}

func isInteractionObserveCommand(cmd string) bool {
	return strings.Contains(cmd, "--agenthalo-claude-hook") ||
		(strings.Contains(cmd, "hook claude-observe") && !strings.Contains(cmd, "--cleanup"))
}

func isInteractionCleanupCommand(cmd string) bool {
	return strings.Contains(cmd, "hook claude-observe") && strings.Contains(cmd, "--cleanup")
}

func stringAny(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func shellQuote(v string) string {
	if v == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(v, "'", "'\"'\"'") + "'"
}

func expandUser(p string) string {
	if p == "~" {
		if h, err := os.UserHomeDir(); err == nil {
			return h
		}
	}
	if strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, p[2:])
		}
	}
	return p
}

func InstalledCommands(cfg map[string]any) []string {
	hooks := objectMap(cfg["hooks"])
	out := []string{}
	for _, ev := range events {
		for _, group := range objectList(hooks[ev.name]) {
			for _, item := range objectList(group["hooks"]) {
				cmd := stringAny(item["command"])
				if isTurnstateCommand(cmd) {
					out = append(out, fmt.Sprintf("%s: %s", ev.name, cmd))
				}
			}
		}
	}
	return out
}

func InstalledInteractionCommands(cfg map[string]any) []string {
	hooks := objectMap(cfg["hooks"])
	out := []string{}
	for _, ev := range []string{"PermissionRequest", "PreToolUse", "PostToolUse", "PostToolUseFailure", "Stop"} {
		for _, group := range objectList(hooks[ev]) {
			for _, item := range objectList(group["hooks"]) {
				cmd := stringAny(item["command"])
				if isInteractionObserverCommand(cmd) {
					out = append(out, fmt.Sprintf("%s: %s", ev, cmd))
				}
			}
		}
	}
	return out
}
