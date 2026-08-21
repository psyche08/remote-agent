package turnstatehook

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunWritesTurnstate(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	Run("running", strings.NewReader(`{"session_id":"sid1","cwd":"/repo","transcript_path":"/tmp/t.jsonl","hook_event_name":"UserPromptSubmit"}`), dir)
	path := filepath.Join(dir, "sid1.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var rec map[string]any
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatal(err)
	}
	if rec["state"] != "running" || rec["cwd"] != "/repo" || rec["event"] != "UserPromptSubmit" {
		t.Fatalf("bad rec: %#v", rec)
	}
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("turnstate dir mode=%#o, want 0700", dirInfo.Mode().Perm())
	}
	fileInfo, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !fileInfo.Mode().IsRegular() || fileInfo.Mode().Perm() != 0o600 {
		t.Fatalf("turnstate file mode=%#o, want regular 0600", fileInfo.Mode().Perm())
	}
}

func TestRunRejectsUnsafeTurnstateIdentity(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(filepath.Dir(dir), "turnstate-victim.json")
	defer os.Remove(victim)
	Run("running", strings.NewReader(`{"session_id":"../turnstate-victim","hook_event_name":"UserPromptSubmit"}`), dir)
	if _, err := os.Lstat(victim); !os.IsNotExist(err) {
		t.Fatalf("unsafe turnstate identity escaped private dir: %v", err)
	}
}

func TestInstallReplacesPythonHookAndPreservesExisting(t *testing.T) {
	settings := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(settings, []byte(`{
	  "theme": "dark",
	  "hooks": {
	    "PermissionRequest": [{"matcher":"Bash","hooks": [
	      {"type": "command", "command": "/Applications/AgentHalo.app/Contents/MacOS/AgentHalo --agenthalo-claude-hook"},
	      {"type": "command", "command": "echo permission-audit"}
	    ]}],
	    "PreToolUse": [
	      {"hooks": [{"type": "command", "command": "echo hi"}]},
	      {"matcher":"AskUserQuestion","hooks": [{"type":"command","command":"old-agent --agenthalo-claude-hook"}]},
	      {"matcher":"App","hooks": [{"type":"command","command":"echo keep-app-hook"}]}
	    ],
	    "Stop": [{"hooks": [{"type": "command", "command": "python /repo/hooks/turnstate_hook.py idle"}]}]
	  }
	}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Install(settings, "/repo/bin/agenthalo", "/tmp/turnstate")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(cfg)
	text := string(b)
	if !strings.Contains(text, "echo hi") || !strings.Contains(text, "echo permission-audit") ||
		!strings.Contains(text, "echo keep-app-hook") ||
		!strings.Contains(text, "/repo/bin/agenthalo") {
		t.Fatalf("settings not preserved/installed: %s", text)
	}
	if strings.Contains(text, "turnstate_hook.py") || strings.Contains(text, "--agenthalo-claude-hook") {
		t.Fatalf("old AgentHalo hook was not replaced: %s", text)
	}
	cmds := InstalledCommands(cfg)
	if len(cmds) != 2 {
		t.Fatalf("commands=%#v", cmds)
	}
	observerCommands := InstalledInteractionCommands(cfg)
	if len(observerCommands) != 6 {
		t.Fatalf("observer commands=%#v", observerCommands)
	}
	for _, command := range observerCommands {
		if !strings.Contains(command, "hook claude-observe") ||
			!strings.Contains(command, "~/.claude/agenthalo-interactions") {
			t.Fatalf("bad observer command: %s", command)
		}
	}

	// Reinstalling updates in place rather than multiplying managed hooks.
	cfg, err = Install(settings, "/new/bin/agenthalo", "/tmp/turnstate")
	if err != nil {
		t.Fatal(err)
	}
	if got := InstalledInteractionCommands(cfg); len(got) != 6 {
		t.Fatalf("idempotent observer commands=%#v", got)
	} else if !strings.Contains(strings.Join(got, "\n"), "/new/bin/agenthalo") {
		t.Fatalf("observer binary was not replaced: %#v", got)
	}
	info, err := os.Lstat(settings)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("settings mode=%#o, want tightened 0600", info.Mode().Perm())
	}
}

func TestInstallRejectsMalformedAndSymlinkSettingsWithoutClobber(t *testing.T) {
	root := t.TempDir()
	malformed := filepath.Join(root, "malformed.json")
	wantMalformed := []byte(`{"theme":"dark",`)
	if err := os.WriteFile(malformed, wantMalformed, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(malformed, "/bin/agenthalo", ""); err == nil {
		t.Fatal("malformed non-empty settings were silently rebuilt")
	}
	got, err := os.ReadFile(malformed)
	if err != nil || string(got) != string(wantMalformed) {
		t.Fatalf("malformed settings changed=%q err=%v", got, err)
	}

	victim := filepath.Join(root, "victim.json")
	wantVictim := []byte(`{"theme":"keep"}`)
	if err := os.WriteFile(victim, wantVictim, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "settings-link.json")
	if err := os.Symlink(victim, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(link, "/bin/agenthalo", ""); err == nil {
		t.Fatal("symlink settings path was accepted")
	}
	got, err = os.ReadFile(victim)
	if err != nil || string(got) != string(wantVictim) {
		t.Fatalf("symlink victim changed=%q err=%v", got, err)
	}
	if _, err := Install(root, "/bin/agenthalo", ""); err == nil {
		t.Fatal("directory settings path was accepted")
	}
}

func TestInstallWritesSettingsAtomicallyAndPreservesPrivateMode(t *testing.T) {
	root := t.TempDir()
	settings := filepath.Join(root, "settings.json")
	if err := os.WriteFile(settings, []byte(`{"theme":"dark"}`), 0o400); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(settings, "/Applications/AgentHalo.app/Contents/MacOS/AgentHalo", ""); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(settings)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o400 {
		t.Fatalf("private settings mode=%#o, want preserved 0400", info.Mode().Perm())
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "settings.json" {
		t.Fatalf("atomic install left temporary files: %#v", entries)
	}
	b, err := os.ReadFile(settings)
	if err != nil || !strings.Contains(string(b), "claude-observe") {
		t.Fatalf("installed settings=%s err=%v", b, err)
	}
}
