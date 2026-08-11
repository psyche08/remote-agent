package turnstatehook

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func withDirectorySyncFailure(t *testing.T, fail func(string) bool) error {
	t.Helper()
	want := errors.New("injected directory fsync failure")
	original := syncDirectoryForDurability
	syncDirectoryForDurability = func(path string) error {
		if fail(path) {
			return want
		}
		return original(path)
	}
	t.Cleanup(func() { syncDirectoryForDurability = original })
	return want
}

func TestBeginAttemptDirectorySyncFailureLeavesFailClosedTombstone(t *testing.T) {
	dir := filepath.Join(interactionTestTempDir(t), "interactions")
	identity, _ := InteractionDecisionDigest("identity")
	decision, _ := InteractionDecisionDigest("allow")
	want := withDirectorySyncFailure(t, func(path string) bool {
		return filepath.Base(path) == interactionAttemptDir
	})
	if _, err := BeginInteractionAttempt(
		dir, "request-1", identity, "logical-1", "permission", decision,
	); !errors.Is(err, want) {
		t.Fatalf("begin directory sync err=%v", err)
	}
	attempt, err := ReadInteractionAttempt(dir, "request-1")
	if err != nil || attempt.State != "attempted" {
		t.Fatalf("failed durability acknowledgement lost tombstone=%#v err=%v", attempt, err)
	}
	if _, err := BeginInteractionAttempt(
		dir, "request-1", identity, "logical-1", "permission", decision,
	); !errors.Is(err, ErrInteractionAlreadyAttempted) {
		t.Fatalf("request became actionable after directory sync failure: %v", err)
	}
}

func TestResolveDirectorySyncFailureLeavesResolvedTombstone(t *testing.T) {
	dir := filepath.Join(interactionTestTempDir(t), "interactions")
	identity, _ := InteractionDecisionDigest("identity")
	decision, _ := InteractionDecisionDigest("answer")
	attempt, err := BeginInteractionAttempt(
		dir, "request-1", identity, "logical-1", "question", decision,
	)
	if err != nil {
		t.Fatal(err)
	}
	want := withDirectorySyncFailure(t, func(path string) bool {
		return filepath.Base(path) == interactionAttemptDir
	})
	if _, err := ResolveInteractionAttempt(
		dir, "request-1", attempt.OperationID,
	); !errors.Is(err, want) {
		t.Fatalf("resolve directory sync err=%v", err)
	}
	resolved, err := ReadInteractionAttempt(dir, "request-1")
	if err != nil || resolved.State != "resolved" || resolved.ResolvedAt == nil {
		t.Fatalf("failed resolve acknowledgement lost tombstone=%#v err=%v", resolved, err)
	}
}

func TestObserverAndSettingsReturnDirectorySyncFailures(t *testing.T) {
	root := interactionTestTempDir(t)
	interactionDir := filepath.Join(root, "interactions")
	settings := filepath.Join(root, "settings.json")
	want := withDirectorySyncFailure(t, func(string) bool { return true })
	payload := `{
		"session_id":"session-1","hook_event_name":"PermissionRequest",
		"tool_name":"Bash","tool_input":{},"tool_use_id":"tool-1"
	}`
	if _, err := ObserveInteraction(
		strings.NewReader(payload), interactionDir,
	); !errors.Is(err, want) {
		t.Fatalf("observer directory sync err=%v", err)
	}
	// The atomic rename is visible, but the caller did not receive a false
	// durability acknowledgement.
	if _, err := ReadInteraction(interactionDir, "tool-1", time.Minute); err != nil {
		t.Fatalf("renamed observer record not readable after sync failure: %v", err)
	}
	if err := writeClaudeSettings(settings, []byte("{}\n"), 0o600); !errors.Is(err, want) {
		t.Fatalf("settings directory sync err=%v", err)
	}
	if got, err := os.ReadFile(settings); err != nil || string(got) != "{}\n" {
		t.Fatalf("atomic settings rename got=%q err=%v", got, err)
	}
}
