package turnstatehook

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestInteractionAttemptLedgerIsExactOnceAcrossObserverRewriteAndRestart(t *testing.T) {
	dir := filepath.Join(interactionTestTempDir(t), "interactions")
	payload := `{
		"session_id":"session-1","transcript_path":"/tmp/transcript.jsonl",
		"hook_event_name":"PermissionRequest","tool_name":"Bash",
		"tool_input":{"command":"git status"},"tool_use_id":"tool-1"
	}`
	record, err := ObserveInteraction(strings.NewReader(payload), dir)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := InteractionIdentityDigest(record)
	if err != nil {
		t.Fatal(err)
	}
	decision, err := InteractionDecisionDigest(map[string]any{"decision": "allow_once"})
	if err != nil {
		t.Fatal(err)
	}

	const contenders = 16
	start := make(chan struct{})
	results := make(chan error, contenders)
	attempts := make(chan InteractionAttempt, contenders)
	var wg sync.WaitGroup
	for range contenders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			attempt, err := BeginInteractionAttempt(
				dir, record.RequestID, identity, "logical-1", "permission", decision,
			)
			attempts <- attempt
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(attempts)
	close(results)
	successes := 0
	operationID := ""
	for attempt := range attempts {
		if attempt.OperationID != "" && operationID == "" {
			operationID = attempt.OperationID
		}
	}
	for err := range results {
		if err == nil {
			successes++
			continue
		}
		if !errors.Is(err, ErrInteractionAlreadyAttempted) {
			t.Fatalf("contending attempt err=%v", err)
		}
	}
	if successes != 1 || operationID == "" {
		t.Fatalf("attempt successes=%d operation_id=%q", successes, operationID)
	}
	ledgerPath := filepath.Join(dir, interactionAttemptDir, interactionFilename(record.RequestID))
	info, err := os.Lstat(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("attempt ledger mode=%v", info.Mode())
	}

	// The hook may observe the same native card again. Its atomic candidate
	// rewrite cannot reset the independent attempted tombstone.
	if _, err := ObserveInteraction(strings.NewReader(payload), dir); err != nil {
		t.Fatal(err)
	}
	restarted, err := ReadInteractionAttempt(dir, record.RequestID)
	if err != nil || restarted.State != "attempted" || restarted.OperationID != operationID {
		t.Fatalf("restarted attempt=%#v err=%v", restarted, err)
	}
	resolved, err := ResolveInteractionAttempt(dir, record.RequestID, operationID)
	if err != nil || resolved.State != "resolved" || resolved.ResolvedAt == nil {
		t.Fatalf("resolved attempt=%#v err=%v", resolved, err)
	}
	if _, err := ObserveInteraction(strings.NewReader(payload), dir); err != nil {
		t.Fatal(err)
	}
	afterRewrite, err := ReadInteractionAttempt(dir, record.RequestID)
	if err != nil || afterRewrite.State != "resolved" || afterRewrite.OperationID != operationID {
		t.Fatalf("post-rewrite tombstone=%#v err=%v", afterRewrite, err)
	}
	if _, err := BeginInteractionAttempt(
		dir, record.RequestID, identity, "logical-1", "permission", decision,
	); !errors.Is(err, ErrInteractionAlreadyAttempted) {
		t.Fatalf("resolved request became actionable: %v", err)
	}
}

func TestInteractionAttemptLedgerRejectsConflictsAndSymlinks(t *testing.T) {
	dir := filepath.Join(interactionTestTempDir(t), "interactions")
	identity, _ := InteractionDecisionDigest("identity")
	allow, _ := InteractionDecisionDigest("allow")
	deny, _ := InteractionDecisionDigest("deny")
	if _, err := BeginInteractionAttempt(
		dir, "request-1", identity, "logical-1", "permission", allow,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := BeginInteractionAttempt(
		dir, "request-1", identity, "logical-1", "permission", deny,
	); !errors.Is(err, ErrInteractionAttemptConflict) {
		t.Fatalf("conflicting decision err=%v", err)
	}

	attemptDir, err := secureInteractionAttemptDir(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(interactionTestTempDir(t), "victim")
	if err := os.WriteFile(victim, []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	requestID := "symlink-request"
	if err := os.Symlink(victim, filepath.Join(attemptDir, interactionFilename(requestID))); err != nil {
		t.Fatal(err)
	}
	if _, err := BeginInteractionAttempt(
		dir, requestID, identity, "logical-1", "question", allow,
	); !errors.Is(err, ErrInteractionAttemptConflict) {
		t.Fatalf("symlink attempt err=%v, want fail-closed conflict", err)
	}
	contents, err := os.ReadFile(victim)
	if err != nil || string(contents) != "unchanged" {
		t.Fatalf("symlink victim=%q err=%v", contents, err)
	}
}

func TestInteractionAttemptResolveRequiresOperationIdentity(t *testing.T) {
	dir := filepath.Join(interactionTestTempDir(t), "interactions")
	identity, _ := InteractionDecisionDigest("identity")
	decision, _ := InteractionDecisionDigest("answer")
	attempt, err := BeginInteractionAttempt(
		dir, "question-1", identity, "logical-1", "question", decision,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveInteractionAttempt(dir, "question-1", "wrong"); !errors.Is(err, ErrInteractionAttemptConflict) {
		t.Fatalf("wrong operation resolve err=%v", err)
	}
	current, err := ReadInteractionAttempt(dir, "question-1")
	if err != nil || current.State != "attempted" || current.ResolvedAt != nil {
		t.Fatalf("wrong resolve changed ledger=%#v err=%v", current, err)
	}
	resolved, err := ResolveInteractionAttempt(dir, "question-1", attempt.OperationID)
	if err != nil || resolved.ResolvedAt == nil || time.Since(*resolved.ResolvedAt) > time.Minute {
		t.Fatalf("valid resolve=%#v err=%v", resolved, err)
	}
}

func TestInteractionIdentityDigestCoversExactObserverIdentity(t *testing.T) {
	base := InteractionRecord{
		RequestID: "request-1", SessionID: "session-1",
		TranscriptPath: "/tmp/transcript.jsonl", CWD: "/tmp/project",
		HookEventName: "PermissionRequest", ToolName: "Bash", ToolUseID: "tool-1",
		ToolInput:             map[string]any{"command": "git status"},
		PermissionSuggestions: []any{map[string]any{"type": "allow"}},
	}
	want, err := InteractionIdentityDigest(base)
	if err != nil {
		t.Fatal(err)
	}
	mutations := map[string]func(*InteractionRecord){
		"tool use id": func(record *InteractionRecord) { record.ToolUseID = "tool-2" },
		"permission suggestions": func(record *InteractionRecord) {
			record.PermissionSuggestions = []any{map[string]any{"type": "deny"}}
		},
		"cwd": func(record *InteractionRecord) { record.CWD = "/tmp/other" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			changed := base
			mutate(&changed)
			got, err := InteractionIdentityDigest(changed)
			if err != nil {
				t.Fatal(err)
			}
			if got == want {
				t.Fatalf("identity digest ignored %s", name)
			}
		})
	}
}

func TestInteractionCandidateStateIsFailClosedAcrossObserverRewrite(t *testing.T) {
	dir := filepath.Join(interactionTestTempDir(t), "interactions")
	payload := `{
		"session_id":"session-1","transcript_path":"/tmp/transcript.jsonl","cwd":"/tmp/project",
		"hook_event_name":"PermissionRequest","tool_name":"Bash",
		"tool_input":{"command":"git status"},"tool_use_id":"tool-1",
		"permission_suggestions":[{"type":"allow"}]
	}`
	record, err := ObserveInteraction(strings.NewReader(payload), dir)
	if err != nil {
		t.Fatal(err)
	}
	if state, err := InteractionCandidateStateForRecord(dir, record); err != nil || state != InteractionCandidatePending {
		t.Fatalf("pending state=%q err=%v", state, err)
	}
	identity, _ := InteractionIdentityDigest(record)
	decision, _ := InteractionDecisionDigest("allow")
	attempt, err := BeginInteractionAttempt(
		dir, record.RequestID, identity, "logical-1", "permission", decision,
	)
	if err != nil {
		t.Fatal(err)
	}
	if state, err := InteractionCandidateStateForRecord(dir, record); err != nil || state != InteractionCandidateAttempted {
		t.Fatalf("attempted state=%q err=%v", state, err)
	}

	// An identical observer rewrite cannot reset the independent ledger.
	rewritten, err := ObserveInteraction(strings.NewReader(payload), dir)
	if err != nil {
		t.Fatal(err)
	}
	if state, err := InteractionCandidateStateForRecord(dir, rewritten); err != nil || state != InteractionCandidateAttempted {
		t.Fatalf("rewritten attempted state=%q err=%v", state, err)
	}
	if _, err := ResolveInteractionAttempt(dir, record.RequestID, attempt.OperationID); err != nil {
		t.Fatal(err)
	}
	if state, err := InteractionCandidateStateForRecord(dir, rewritten); err != nil || state != InteractionCandidateResolved {
		t.Fatalf("resolved state=%q err=%v", state, err)
	}

	// Reusing the same native tool id with changed contents conflicts rather
	// than becoming a new pending action.
	changed := rewritten
	changed.ToolInput = map[string]any{"command": "rm -rf /tmp/other"}
	if state, err := InteractionCandidateStateForRecord(dir, changed); state != "" || !errors.Is(err, ErrInteractionAttemptConflict) {
		t.Fatalf("changed identity state=%q err=%v", state, err)
	}
}
