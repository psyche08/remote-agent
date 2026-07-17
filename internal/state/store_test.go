package state

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStoreReadsMissingFilesAsEmptyArrays(t *testing.T) {
	st := New(t.TempDir())
	sessions, err := st.Sessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 0 {
		t.Fatalf("got %d sessions", len(sessions))
	}
}

func TestStoreRoundTripsUnknownFields(t *testing.T) {
	dir := t.TempDir()
	st := New(dir)
	records := []Record{{"session_id": "s1", "provider_id": "codex", "custom": map[string]any{"x": float64(1)}}}
	if err := st.SaveSessions(records); err != nil {
		t.Fatal(err)
	}
	got, err := st.Sessions()
	if err != nil {
		t.Fatal(err)
	}
	if got[0]["session_id"] != "s1" {
		t.Fatalf("bad record: %#v", got[0])
	}
	if _, err := os.Stat(filepath.Join(dir, "sessions.json")); err != nil {
		t.Fatal(err)
	}
}
