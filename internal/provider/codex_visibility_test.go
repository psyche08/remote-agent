package provider

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/psyche08/remote-agent/internal/config"
)

func TestCodexMetadataSubagentDetectionUsesStructuredFields(t *testing.T) {
	tests := []struct {
		name string
		meta map[string]any
		want bool
	}{
		{name: "app server source", meta: map[string]any{
			"source": map[string]any{"subAgent": map[string]any{"thread_spawn": map[string]any{"parent_thread_id": "parent"}}},
		}, want: true},
		{name: "rollout source", meta: map[string]any{
			"source": map[string]any{"subagent": map[string]any{"other": "guardian"}},
		}, want: true},
		{name: "camel parent", meta: map[string]any{"parentThreadId": "parent"}, want: true},
		{name: "snake parent", meta: map[string]any{"parent_thread_id": "parent"}, want: true},
		{name: "legacy thread source", meta: map[string]any{"thread_source": "subagent"}, want: true},
		{name: "normal", meta: map[string]any{"source": "vscode", "threadSource": "user"}, want: false},
		{name: "user fork", meta: map[string]any{"source": "vscode", "forkedFromId": "parent"}, want: false},
		{name: "text is not metadata", meta: map[string]any{"title": "debug subagent sessions"}, want: false},
		{name: "custom source is not subagent source", meta: map[string]any{
			"source": map[string]any{"custom": "subagent"},
		}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := codexMetadataIsSubagent(tt.meta); got != tt.want {
				t.Fatalf("codexMetadataIsSubagent(%#v)=%v want=%v", tt.meta, got, tt.want)
			}
		})
	}
}

func TestCodexHiddenSessionIDsKeepsRolloutsDirectlyAddressable(t *testing.T) {
	dir := t.TempDir()
	parentID := "019f1111-1111-7111-8111-111111111111"
	childID := "019f2222-2222-7222-8222-222222222222"
	guardianID := "019f3333-3333-7333-8333-333333333333"
	writeJSONL(t, filepath.Join(dir, "rollout-parent-"+parentID+".jsonl"), []map[string]any{{
		"type": "session_meta", "payload": map[string]any{"id": parentID, "source": "vscode"},
	}})
	writeJSONL(t, filepath.Join(dir, "rollout-child-"+childID+".jsonl"), []map[string]any{{
		"type": "session_meta", "payload": map[string]any{
			"id": childID, "parent_thread_id": parentID,
			"source": map[string]any{"subagent": map[string]any{"thread_spawn": map[string]any{"parent_thread_id": parentID}}},
		},
	}})
	writeJSONL(t, filepath.Join(dir, "rollout-guardian-"+guardianID+".jsonl"), []map[string]any{{
		"type": "session_meta", "payload": map[string]any{
			"id": guardianID, "source": map[string]any{"subagent": map[string]any{"other": "guardian"}},
		},
	}})
	c := NewCodex("codex", config.ProviderConfig{
		Extra: map[string]any{"codex_sessions_dirs": []any{dir}},
	})

	hidden := c.HiddenSessionIDs()
	if hidden[parentID] || !hidden[childID] || !hidden[guardianID] {
		t.Fatalf("hidden IDs=%#v", hidden)
	}
	if got := codexSessionMessages(childID, []string{dir}, nativePreviewUnlimited); len(got) != 0 {
		t.Fatalf("unexpected messages in empty child rollout: %#v", got)
	}
	if codexFindRollout(childID, []string{dir}) == "" {
		t.Fatal("hidden child rollout is no longer directly addressable")
	}
}

func TestCodexDiscoveryCatalogCachesMetadataAndHiddenIDs(t *testing.T) {
	dir := t.TempDir()
	parentID := "019f4111-1111-7111-8111-111111111111"
	childID := "019f4222-2222-7222-8222-222222222222"
	parentPath := filepath.Join(dir, "rollout-parent-"+parentID+".jsonl")
	childPath := filepath.Join(dir, "rollout-child-"+childID+".jsonl")
	writeJSONL(t, parentPath, []map[string]any{{
		"type": "session_meta", "payload": map[string]any{"id": parentID, "cwd": "/repo/parent", "source": "vscode"},
	}})
	childRecords := []map[string]any{{
		"type": "session_meta", "payload": map[string]any{
			"id": childID, "cwd": "/repo/child", "parent_thread_id": parentID,
		},
	}}
	writeJSONL(t, childPath, childRecords)

	fc := newFakeCodexClient()
	c := NewCodex("codex", config.ProviderConfig{
		Command: "codex",
		Extra: map[string]any{
			"codex_session_index": filepath.Join(dir, "missing-index.jsonl"),
			"codex_sessions_dirs": []any{dir},
		},
	})
	c.client = fc

	rows := c.ListNativeSessions()
	byID := map[string]map[string]any{}
	for _, row := range rows {
		byID[stringAny(row["native_session_id"])] = row
	}
	if byID[parentID]["cwd"] != "/repo/parent" ||
		byID[childID]["cwd"] != "/repo/child" ||
		!boolAny(byID[childID][hiddenFromSessionListsKey]) {
		t.Fatalf("catalog metadata was not merged into native rows: %#v", byID)
	}
	if stringAny(byID[childID]["last_reply_at"]) != "" {
		t.Fatalf("hot discovery path unexpectedly scanned rollout tail: %#v", byID[childID])
	}

	first := c.codexDiscoverySnapshot(false)
	if first.scans != 1 || first.filesParsed != 2 || !first.hidden[childID] {
		t.Fatalf("bad initial catalog counters/visibility: %#v", first)
	}
	hidden := c.HiddenSessionIDs()
	afterHidden := c.codexDiscoverySnapshot(false)
	if !hidden[childID] || afterHidden.scans != first.scans || afterHidden.filesParsed != first.filesParsed {
		t.Fatalf("HiddenSessionIDs rescanned discovery: before=%#v after=%#v hidden=%#v", first, afterHidden, hidden)
	}

	c.discoveryMu.Lock()
	c.discovery.refreshedAt = time.Time{}
	c.discoveryMu.Unlock()
	c.ListNativeSessions()
	unchanged := c.codexDiscoverySnapshot(false)
	if unchanged.scans != first.scans+1 || unchanged.filesParsed != first.filesParsed {
		t.Fatalf("unchanged rollout metadata was reparsed: before=%#v after=%#v", first, unchanged)
	}

	childRecords = append(childRecords, map[string]any{
		"type": "event_msg", "timestamp": "2026-07-26T12:00:00Z",
		"payload": map[string]any{"type": "task_started"},
	})
	writeJSONL(t, childPath, childRecords)
	c.discoveryMu.Lock()
	c.discovery.refreshedAt = time.Time{}
	c.discoveryMu.Unlock()
	c.ListNativeSessions()
	changed := c.codexDiscoverySnapshot(false)
	if changed.filesParsed != unchanged.filesParsed+1 || !changed.hidden[childID] {
		t.Fatalf("changed rollout was not incrementally reparsed: before=%#v after=%#v", unchanged, changed)
	}
}

func TestCodexSessionLimitDoesNotCountHiddenRows(t *testing.T) {
	const (
		hiddenCount  = 119
		visibleCount = 220
	)
	summaries := make(map[string]codexRolloutSummary, hiddenCount+visibleCount)
	hiddenIDs := map[string]bool{}
	for i := 0; i < hiddenCount; i++ {
		id := fmt.Sprintf("hidden-%03d", i)
		hiddenIDs[id] = true
		summaries[id] = codexRolloutSummary{
			UpdatedAt: time.Unix(2_000_000_000+int64(i), 0).UTC().Format(time.RFC3339Nano),
			Hidden:    true,
		}
	}
	for i := 0; i < visibleCount; i++ {
		id := fmt.Sprintf("visible-%03d", i)
		summaries[id] = codexRolloutSummary{
			UpdatedAt: time.Unix(1_000_000_000+int64(i), 0).UTC().Format(time.RFC3339Nano),
		}
	}

	assertBudget := func(stage string, rows []map[string]any) {
		t.Helper()
		visible := 0
		hidden := 0
		seenHidden := map[string]bool{}
		for _, row := range rows {
			id := stringAny(row["native_session_id"])
			if boolAny(row[hiddenFromSessionListsKey]) || boolAny(row["subagent"]) {
				hidden++
				seenHidden[id] = true
				continue
			}
			visible++
		}
		if visible != nativeSessionListLimit || hidden != hiddenCount {
			t.Fatalf("%s budget visible=%d hidden=%d total=%d", stage, visible, hidden, len(rows))
		}
		for id := range hiddenIDs {
			if !seenHidden[id] {
				t.Fatalf("%s dropped internally addressable hidden session %q", stage, id)
			}
		}
	}

	localRows := codexSessionsFromSummaries(
		filepath.Join(t.TempDir(), "missing-index.jsonl"),
		summaries,
		nativeSessionListLimit,
	)
	assertBudget("local discovery", localRows)

	// The final app-server/local merge has its own limit and must preserve the
	// same visible budget instead of reintroducing the pre-filter truncation.
	merged := mergeCodexNativeSessions(nil, localRows, nativeSessionListLimit)
	assertBudget("merged discovery", merged)
}

func TestCodexThreadListMarksSubagentsWithoutDroppingThemInternally(t *testing.T) {
	rows := codexThreadListToSessions(map[string]any{"data": []any{
		map[string]any{"id": "parent", "source": "vscode", "forkedFromId": "older"},
		map[string]any{"id": "child-by-parent", "parentThreadId": "parent", "source": "vscode"},
		map[string]any{"id": "child-by-source", "source": map[string]any{
			"subAgent": map[string]any{"thread_spawn": map[string]any{"parent_thread_id": "parent", "depth": 1}},
		}},
	}})
	if len(rows) != 3 {
		t.Fatalf("internal thread list dropped direct-addressable rows: %#v", rows)
	}
	byID := map[string]map[string]any{}
	for _, row := range rows {
		byID[stringAny(row["native_session_id"])] = row
	}
	if boolAny(byID["parent"][hiddenFromSessionListsKey]) {
		t.Fatalf("normal fork was hidden: %#v", byID["parent"])
	}
	for _, id := range []string{"child-by-parent", "child-by-source"} {
		if !boolAny(byID[id][hiddenFromSessionListsKey]) || !boolAny(byID[id]["subagent"]) {
			t.Fatalf("subagent %s was not marked: %#v", id, byID[id])
		}
	}
}

func TestDesktopRuntimeRowMarksSubagent(t *testing.T) {
	rows := desktopSnapshotLiveRows(map[string]any{
		"type": "broadcast", "method": "thread-stream-state-changed",
		"params": map[string]any{
			"conversationId": "child",
			"change": map[string]any{"conversationState": map[string]any{
				"id": "child", "parentThreadId": "parent", "title": "worker",
			}},
		},
	})
	if len(rows) != 1 || !boolAny(rows[0][hiddenFromSessionListsKey]) {
		t.Fatalf("desktop child row was not marked hidden: %#v", rows)
	}
}
