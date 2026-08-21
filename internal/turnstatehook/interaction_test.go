package turnstatehook

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestObserveInteractionWritesOnlyAllowlistedFields(t *testing.T) {
	dir := filepath.Join(interactionTestTempDir(t), "interactions")
	record, err := ObserveInteraction(strings.NewReader(`{
		"session_id":"session-1",
		"transcript_path":"/private/transcript.jsonl",
		"cwd":"/repo",
		"hook_event_name":"PermissionRequest",
		"tool_name":"Bash",
		"tool_input":{"command":"git status"},
		"tool_use_id":"tool/use/1",
		"permission_suggestions":[{"type":"allow"}],
		"environment":{"ANTHROPIC_API_KEY":"must-not-be-recorded"},
		"api_token":"must-not-be-recorded"
	}`), dir)
	if err != nil {
		t.Fatal(err)
	}
	if record.RequestID != "tool/use/1" || record.ToolUseID != "tool/use/1" {
		t.Fatalf("tool_use_id did not remain authoritative: %#v", record)
	}

	dirInfo, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("interaction dir mode=%#o, want 0700", dirInfo.Mode().Perm())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !isInteractionFilename(entries[0].Name()) {
		t.Fatalf("interaction files=%#v", entries)
	}
	path := filepath.Join(dir, entries[0].Name())
	fileInfo, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fileInfo.Mode().Perm() != 0o600 {
		t.Fatalf("interaction record mode=%#o, want 0600", fileInfo.Mode().Perm())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var stored map[string]any
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	gotKeys := make([]string, 0, len(stored))
	for key := range stored {
		gotKeys = append(gotKeys, key)
	}
	wantKeys := []string{
		"cwd", "expires_at", "hook_event_name", "permission_suggestions",
		"recorded_at", "request_id", "session_id", "tool_input", "tool_name",
		"tool_use_id", "transcript_path",
	}
	sortStrings(gotKeys)
	if !reflect.DeepEqual(gotKeys, wantKeys) {
		t.Fatalf("stored fields=%#v, want exact allowlist %#v; json=%s", gotKeys, wantKeys, raw)
	}
	text := string(raw)
	if strings.Contains(text, "ANTHROPIC_API_KEY") || strings.Contains(text, "must-not-be-recorded") {
		t.Fatalf("observer persisted an unallowlisted environment/token field: %s", text)
	}

	loaded, err := ReadInteraction(dir, "tool/use/1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.SessionID != "session-1" || loaded.ToolName != "Bash" || loaded.HookEventName != "PermissionRequest" {
		t.Fatalf("loaded record=%#v", loaded)
	}
}

func TestObserveInteractionFallbackRequestIDIsNormalizedAndSessionScoped(t *testing.T) {
	dir := filepath.Join(interactionTestTempDir(t), "interactions")
	first, err := ObserveInteraction(strings.NewReader(`{
		"session_id":"session-1","hook_event_name":"PreToolUse",
		"tool_name":"AskUserQuestion","tool_input":{"b":2,"a":1}
	}`), dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ObserveInteraction(strings.NewReader(`{
		"tool_input":{"a":1,"b":2},"tool_name":"AskUserQuestion",
		"hook_event_name":"PreToolUse","session_id":"session-1"
	}`), dir)
	if err != nil {
		t.Fatal(err)
	}
	if first.RequestID != second.RequestID || !strings.HasPrefix(first.RequestID, "content-") {
		t.Fatalf("normalized request ids differ: %q vs %q", first.RequestID, second.RequestID)
	}
	third, err := ObserveInteraction(strings.NewReader(`{
		"session_id":"session-2","hook_event_name":"PreToolUse",
		"tool_name":"AskUserQuestion","tool_input":{"a":1,"b":2}
	}`), dir)
	if err != nil {
		t.Fatal(err)
	}
	if third.RequestID == first.RequestID {
		t.Fatal("fallback request id was not scoped to the Claude session")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("idempotent normalized observations wrote %d files, want 2", len(entries))
	}
}

func TestObserveInteractionRejectsSymlinkDirectoryAndRecord(t *testing.T) {
	root := interactionTestTempDir(t)
	realDir := filepath.Join(root, "real")
	if err := os.Mkdir(realDir, 0o700); err != nil {
		t.Fatal(err)
	}
	linkDir := filepath.Join(root, "linked")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Fatal(err)
	}
	payload := `{"session_id":"session-1","hook_event_name":"PermissionRequest","tool_name":"Bash","tool_use_id":"tool-1"}`
	if _, err := ObserveInteraction(strings.NewReader(payload), linkDir); !errors.Is(err, ErrUnsafeInteractionDir) {
		t.Fatalf("symlink directory err=%v, want ErrUnsafeInteractionDir", err)
	}
	nestedLink := filepath.Join(root, "nested-link")
	if err := os.Symlink(realDir, nestedLink); err != nil {
		t.Fatal(err)
	}
	if _, err := ObserveInteraction(
		strings.NewReader(payload), filepath.Join(nestedLink, "new-child"),
	); !errors.Is(err, ErrUnsafeInteractionDir) {
		t.Fatalf("symlink ancestor err=%v, want ErrUnsafeInteractionDir", err)
	}
	nonDirectory := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(nonDirectory, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ObserveInteraction(
		strings.NewReader(payload), filepath.Join(nonDirectory, "new-child"),
	); !errors.Is(err, ErrUnsafeInteractionDir) {
		t.Fatalf("non-directory ancestor err=%v, want ErrUnsafeInteractionDir", err)
	}

	record, err := ObserveInteraction(strings.NewReader(payload), realDir)
	if err != nil {
		t.Fatal(err)
	}
	recordPath := filepath.Join(realDir, interactionFilename(record.RequestID))
	if err := os.Remove(recordPath); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(root, "victim")
	if err := os.WriteFile(victim, []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, recordPath); err != nil {
		t.Fatal(err)
	}
	if _, err := ObserveInteraction(strings.NewReader(payload), realDir); !errors.Is(err, ErrUnsafeInteractionDir) {
		t.Fatalf("symlink record err=%v, want ErrUnsafeInteractionDir", err)
	}
	contents, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "unchanged" {
		t.Fatalf("symlink target was modified: %q", contents)
	}
	if _, err := ReadInteraction(realDir, "tool-1", time.Minute); !errors.Is(err, ErrUnsafeInteractionDir) {
		t.Fatalf("symlink read err=%v, want ErrUnsafeInteractionDir", err)
	}
}

func TestInteractionReaderEnforcesRecordedExpiry(t *testing.T) {
	dir := filepath.Join(interactionTestTempDir(t), "interactions")
	record, err := ObserveInteraction(strings.NewReader(
		`{"session_id":"session-1","hook_event_name":"PermissionRequest","tool_name":"Bash","tool_use_id":"tool-1"}`,
	), dir)
	if err != nil {
		t.Fatal(err)
	}
	record.RecordedAt = time.Now().UTC().Add(-time.Minute)
	record.ExpiresAt = time.Now().UTC().Add(-time.Second)
	if err := writeInteractionRecord(dir, record); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadInteraction(dir, record.RequestID, 24*time.Hour); !errors.Is(err, ErrInteractionExpired) {
		t.Fatalf("expired read err=%v, want ErrInteractionExpired", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, interactionFilename(record.RequestID))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired interaction was not removed: %v", err)
	}
}

func TestCleanupHooksRetireNativeHandledCandidatesButKeepAttemptLedger(t *testing.T) {
	dir := filepath.Join(interactionTestTempDir(t), "interactions")
	permission, err := ObserveInteraction(strings.NewReader(`{
		"session_id":"session-1","hook_event_name":"PermissionRequest",
		"tool_name":"Bash","tool_use_id":"permission-1","tool_input":{"command":"true"}
	}`), dir)
	if err != nil {
		t.Fatal(err)
	}
	identity, _ := InteractionIdentityDigest(permission)
	decision, _ := InteractionDecisionDigest("allow")
	if _, err := BeginInteractionAttempt(
		dir, permission.RequestID, identity, "logical-1", "permission", decision,
	); err != nil {
		t.Fatal(err)
	}
	if err := CleanupInteraction(strings.NewReader(`{
		"session_id":"session-1","hook_event_name":"PreToolUse",
		"tool_name":"Bash","tool_use_id":"permission-1"
	}`), dir); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadInteraction(dir, "permission-1", time.Minute); !errors.Is(err, ErrInteractionNotFound) {
		t.Fatalf("manually handled permission remained pending: %v", err)
	}
	if attempt, err := ReadInteractionAttempt(dir, "permission-1"); err != nil || attempt.State != "attempted" {
		t.Fatalf("candidate cleanup removed exact-once ledger=%#v err=%v", attempt, err)
	}

	if _, err := ObserveInteraction(strings.NewReader(`{
		"session_id":"session-1","hook_event_name":"PreToolUse",
		"tool_name":"AskUserQuestion","tool_use_id":"question-1","tool_input":{"questions":[]}
	}`), dir); err != nil {
		t.Fatal(err)
	}
	if err := CleanupInteraction(strings.NewReader(`{
		"session_id":"session-1","hook_event_name":"PreToolUse",
		"tool_name":"AskUserQuestion","tool_use_id":"question-1"
	}`), dir); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadInteraction(dir, "question-1", time.Minute); err != nil {
		t.Fatalf("AskUserQuestion observation was cleared by generic PreToolUse: %v", err)
	}
	if err := CleanupInteraction(strings.NewReader(`{
		"session_id":"session-1","hook_event_name":"PostToolUse",
		"tool_name":"AskUserQuestion","tool_use_id":"question-1"
	}`), dir); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadInteraction(dir, "question-1", time.Minute); !errors.Is(err, ErrInteractionNotFound) {
		t.Fatalf("completed question remained pending: %v", err)
	}

	for _, sessionID := range []string{"session-1", "session-2"} {
		if _, err := ObserveInteraction(strings.NewReader(`{
			"session_id":"`+sessionID+`","hook_event_name":"PermissionRequest",
			"tool_name":"Bash","tool_use_id":"stop-`+sessionID+`"
		}`), dir); err != nil {
			t.Fatal(err)
		}
	}
	if err := CleanupInteraction(strings.NewReader(
		`{"session_id":"session-1","hook_event_name":"Stop"}`,
	), dir); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadInteraction(dir, "stop-session-1", time.Minute); !errors.Is(err, ErrInteractionNotFound) {
		t.Fatalf("Stop did not clear matching session: %v", err)
	}
	if _, err := ReadInteraction(dir, "stop-session-2", time.Minute); err != nil {
		t.Fatalf("Stop cleared another session: %v", err)
	}
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

func interactionTestTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}
