package provider

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTurnstateIdleFailOpenAndStale(t *testing.T) {
	dir := t.TempDir()
	if !turnstateIdle("missing", dir, 90*time.Second) {
		t.Fatal("missing turnstate should fail open as idle")
	}
	transcript := filepath.Join(dir, "t.jsonl")
	if err := os.WriteFile(transcript, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeTurnstateForTest(t, dir, "live", map[string]any{"session_id": "live", "state": "running", "transcript_path": transcript})
	if turnstateIdle("live", dir, 90*time.Second) {
		t.Fatal("fresh running transcript should not be idle")
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(transcript, old, old); err != nil {
		t.Fatal(err)
	}
	if !turnstateIdle("live", dir, 90*time.Second) {
		t.Fatal("stale running transcript should be idle")
	}
	writeTurnstateForTest(t, dir, "idle", map[string]any{"session_id": "idle", "state": "idle"})
	if !turnstateIdle("idle", dir, 90*time.Second) {
		t.Fatal("idle state should be idle")
	}
}

func writeTurnstateForTest(t *testing.T, dir string, sid string, rec map[string]any) {
	t.Helper()
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, sid+".json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}
