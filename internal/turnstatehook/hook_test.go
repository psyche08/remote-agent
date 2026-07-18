package turnstatehook

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunWritesTurnstate(t *testing.T) {
	dir := t.TempDir()
	Run("running", strings.NewReader(`{"session_id":"sid1","cwd":"/repo","transcript_path":"/tmp/t.jsonl","hook_event_name":"UserPromptSubmit"}`), dir)
	b, err := os.ReadFile(filepath.Join(dir, "sid1.json"))
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
}

func TestInstallReplacesPythonHookAndPreservesExisting(t *testing.T) {
	settings := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(settings, []byte(`{
	  "theme": "dark",
	  "hooks": {
	    "PreToolUse": [{"hooks": [{"type": "command", "command": "echo hi"}]}],
	    "Stop": [{"hooks": [{"type": "command", "command": "python /repo/hooks/turnstate_hook.py idle"}]}]
	  }
	}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Install(settings, "/repo/bin/remote-agent", "/tmp/turnstate")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(cfg)
	text := string(b)
	if !strings.Contains(text, "echo hi") || !strings.Contains(text, "/repo/bin/remote-agent") {
		t.Fatalf("settings not preserved/installed: %s", text)
	}
	if strings.Contains(text, "turnstate_hook.py") {
		t.Fatalf("old python hook was not replaced: %s", text)
	}
	cmds := InstalledCommands(cfg)
	if len(cmds) != 2 {
		t.Fatalf("commands=%#v", cmds)
	}
}
