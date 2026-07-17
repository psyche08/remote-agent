package turnstatehook

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

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
	if sessionID == "" {
		return
	}
	if turnstateDir == "" {
		turnstateDir = defaultTurnstateDir()
	}
	dir := expandUser(turnstateDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
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
	tmp := path + ".tmp"
	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

func Install(settingsPath string, binaryPath string, turnstateDir string) (map[string]any, error) {
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
	cfg := map[string]any{}
	if b, err := os.ReadFile(settingsPath); err == nil && len(strings.TrimSpace(string(b))) > 0 {
		_ = json.Unmarshal(b, &cfg)
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	hooks := objectMap(cfg["hooks"])
	cfg["hooks"] = hooks
	for _, ev := range events {
		cmd := shellQuote(binaryPath) + " hook turnstate " + ev.state
		if turnstateDir != "" {
			cmd = "RC_TURNSTATE_DIR=" + shellQuote(turnstateDir) + " " + cmd
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
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		return nil, err
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(settingsPath, append(b, '\n'), 0o644); err != nil {
		return nil, err
	}
	return cfg, nil
}

func defaultTurnstateDir() string {
	if v := os.Getenv("RC_TURNSTATE_DIR"); v != "" {
		return v
	}
	return filepath.Join("~", ".claude", "remote-coding-turnstate")
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
