package provider

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/psyche08/remote-agent/internal/config"
)

func TestClaudeSessionImagesAreOpaqueAndReadable(t *testing.T) {
	base := t.TempDir()
	proj := filepath.Join(base, "-p")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	want := []byte("small-image-bytes")
	encoded := base64.StdEncoding.EncodeToString(want)
	writeJSONL(t, filepath.Join(proj, "image-session.jsonl"), []map[string]any{{
		"type": "user", "timestamp": "t1", "message": map[string]any{"content": []any{
			map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/webp", "data": encoded}},
			map[string]any{"type": "text", "text": "describe"},
		}},
	}})
	items := claudeSessionMessages("image-session", base, 800)
	if len(items) != 2 || items[0]["kind"] != "image" || items[0]["mime"] != "image/webp" {
		t.Fatalf("unexpected image items: %#v", items)
	}
	if _, leaked := items[0]["data"]; leaked {
		t.Fatalf("image data leaked into preview: %#v", items[0])
	}
	assetID := stringAny(items[0]["asset_id"])
	asset, ok, err := claudeTranscriptAsset("image-session", base, assetID)
	if err != nil || !ok || asset.MediaType != "image/webp" || string(asset.Data) != string(want) {
		t.Fatalf("asset=%#v ok=%v err=%v", asset, ok, err)
	}
}

func TestClaudeToolResultImagesAreRenderedAndReadable(t *testing.T) {
	base := t.TempDir()
	proj := filepath.Join(base, "-p")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	want := []byte("computer-use-screenshot")
	encoded := base64.StdEncoding.EncodeToString(want)
	writeJSONL(t, filepath.Join(proj, "nested-image-session.jsonl"), []map[string]any{
		{"type": "assistant", "timestamp": "t1", "message": map[string]any{"content": []any{
			map[string]any{"type": "tool_use", "id": "tool-1", "name": "computer", "input": map[string]any{}},
		}}},
		{"type": "user", "timestamp": "t2", "message": map[string]any{"content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "tool-1", "content": []any{
				map[string]any{"type": "text", "text": "screenshot captured"},
				map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": encoded}},
			}},
		}}},
	})
	items := claudeSessionMessages("nested-image-session", base, 800)
	if len(items) != 2 || items[0]["kind"] != "tool" || items[1]["kind"] != "image" || items[1]["role"] != "assistant" {
		t.Fatalf("nested tool-result image missing or out of order: %#v", items)
	}
	assetID := stringAny(items[1]["asset_id"])
	asset, ok, err := claudeTranscriptAsset("nested-image-session", base, assetID)
	if err != nil || !ok || asset.MediaType != "image/png" || string(asset.Data) != string(want) {
		t.Fatalf("nested asset=%#v ok=%v err=%v", asset, ok, err)
	}
}

func TestClaudeSessionMessagesCanReturnUntruncatedHistoryForAPIPaging(t *testing.T) {
	base := t.TempDir()
	proj := filepath.Join(base, "-p")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSONL(t, filepath.Join(proj, "long-session.jsonl"), []map[string]any{
		{"type": "user", "message": map[string]any{"content": "one"}},
		{"type": "assistant", "message": map[string]any{"content": "two"}},
		{"type": "assistant", "message": map[string]any{"content": "three"}},
	})
	if got := claudeSessionMessages("long-session", base, 2); len(got) != 2 {
		t.Fatalf("bounded preview len=%d want=2", len(got))
	}
	if got := claudeSessionMessages("long-session", base, nativePreviewUnlimited); len(got) != 3 {
		t.Fatalf("unlimited API history len=%d want=3", len(got))
	}
}

func TestCodexSessionMessagesCanReturnUntruncatedHistoryForAPIPaging(t *testing.T) {
	base := t.TempDir()
	id := "019ef111-1111-7111-8111-111111111112"
	records := make([]map[string]any, nativePreviewMaxItems+5)
	for i := range records {
		records[i] = map[string]any{
			"timestamp": "2026-07-15T00:00:00Z",
			"type":      "response_item",
			"payload": map[string]any{
				"type": "message", "role": "assistant",
				"content": []any{map[string]any{"type": "output_text", "text": fmt.Sprintf("message-%d", i)}},
			},
		}
	}
	writeJSONL(t, filepath.Join(base, "rollout-x-"+id+".jsonl"), records)

	if got := codexSessionMessages(id, []string{base}, 2); len(got) != 2 {
		t.Fatalf("bounded preview len=%d want=2", len(got))
	}
	got := codexSessionMessages(id, []string{base}, nativePreviewUnlimited)
	if len(got) != len(records) {
		t.Fatalf("unlimited API history len=%d want=%d", len(got), len(records))
	}
	if got[0]["text"] != "message-0" || got[len(got)-1]["text"] != fmt.Sprintf("message-%d", len(records)-1) {
		t.Fatalf("unlimited API history lost its stable endpoints: first=%#v last=%#v", got[0], got[len(got)-1])
	}
}

func TestCodexSessionImagesAreOpaqueAndReadable(t *testing.T) {
	base := t.TempDir()
	id := "019ef111-1111-7111-8111-111111111111"
	want := []byte("codex-image")
	dataURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(want)
	writeJSONL(t, filepath.Join(base, "rollout-x-"+id+".jsonl"), []map[string]any{{
		"timestamp": "t1", "type": "response_item", "payload": map[string]any{
			"type": "message", "role": "user", "content": []any{
				map[string]any{"type": "input_image", "image_url": dataURL},
				map[string]any{"type": "input_text", "text": "inspect"},
			},
		},
	}})
	items := codexSessionMessages(id, []string{base}, 800)
	if len(items) != 2 || items[0]["kind"] != "image" || items[0]["mime"] != "image/png" {
		t.Fatalf("unexpected image items: %#v", items)
	}
	asset, ok, err := codexTranscriptAsset(id, []string{base}, stringAny(items[0]["asset_id"]))
	if err != nil || !ok || string(asset.Data) != string(want) {
		t.Fatalf("asset=%#v ok=%v err=%v", asset, ok, err)
	}
}

func TestClaudeCLISessionsDerivesTitleAndSkipsCompactSummary(t *testing.T) {
	base := t.TempDir()
	proj := filepath.Join(base, "-Users-me-proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSONL(t, filepath.Join(proj, "cont-1.jsonl"), []map[string]any{
		{"type": "user", "isCompactSummary": true, "message": map[string]any{"content": "This session is being continued from a previous conversation..."}},
		{"type": "user", "message": map[string]any{"content": "修复登录页的 502"}, "cwd": "/Users/me/proj", "gitBranch": "dev"},
		{"type": "assistant", "timestamp": "2026-06-24T10:00:00Z", "message": map[string]any{"content": "done"}},
	})

	rows := claudeCLISessions(base, 60)
	if len(rows) != 1 {
		t.Fatalf("rows=%d", len(rows))
	}
	if rows[0]["title"] != "修复登录页的 502" {
		t.Fatalf("bad title: %#v", rows[0]["title"])
	}
	if rows[0]["cwd"] != "/Users/me/proj" || rows[0]["branch"] != "dev" {
		t.Fatalf("bad cwd/branch: %#v", rows[0])
	}
	if rows[0]["last_reply_at"] != "2026-06-24T10:00:00Z" {
		t.Fatalf("bad last_reply_at: %#v", rows[0])
	}
}

func TestClaudeSessionMessagesToolsThinkingResultsAndFiles(t *testing.T) {
	base := t.TempDir()
	proj := filepath.Join(base, "-p")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(base, "x.py")
	if err := os.WriteFile(target, []byte("print(1)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	targetReal, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	writeJSONL(t, filepath.Join(proj, "id1.jsonl"), []map[string]any{
		{"type": "user", "timestamp": "t1", "cwd": base, "message": map[string]any{"content": "go"}},
		{"type": "assistant", "timestamp": "t2", "message": map[string]any{"content": []any{
			map[string]any{"type": "thinking", "thinking": "let me think"},
			map[string]any{"type": "text", "text": "on it"},
			map[string]any{"type": "tool_use", "id": "tu1", "name": "Read", "input": map[string]any{"file_path": "x.py"}},
		}}},
		{"type": "user", "timestamp": "t3", "message": map[string]any{"content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "tu1", "content": "file contents"},
		}}},
	})

	items := claudeSessionMessages("id1", base, 800)
	if len(items) != 4 {
		t.Fatalf("items=%#v", items)
	}
	tool := findKind(items, "tool")
	if tool == nil {
		t.Fatalf("missing tool: %#v", items)
	}
	if tool["name"] != "Read" || tool["text"] != "x.py" || tool["result"] != "file contents" {
		t.Fatalf("bad tool: %#v", tool)
	}
	files := listAny(tool["files"])
	if len(files) != 1 || files[0] != targetReal {
		t.Fatalf("bad files: %#v want %s", files, targetReal)
	}
	if findKind(items, "thinking")["text"] != "let me think" {
		t.Fatalf("missing thinking: %#v", items)
	}
}

func TestCodexSessionsUseRolloutLastReplyAt(t *testing.T) {
	base := t.TempDir()
	index := filepath.Join(base, "session_index.jsonl")
	sessions := filepath.Join(base, "sessions")
	if err := os.MkdirAll(sessions, 0o755); err != nil {
		t.Fatal(err)
	}
	idOldReply := "019ef111-1111-7111-8111-111111111111"
	idNewReply := "019ef222-2222-7222-8222-222222222222"
	writeJSONL(t, index, []map[string]any{
		{"id": idOldReply, "thread_name": "updated newer", "updated_at": "2026-06-24T12:00:00Z"},
		{"id": idNewReply, "thread_name": "reply newer", "updated_at": "2026-06-24T11:00:00Z"},
	})
	writeJSONL(t, filepath.Join(sessions, "rollout-2026-06-24T10-00-00-"+idOldReply+".jsonl"), []map[string]any{
		{"timestamp": "2026-06-24T10:00:00Z", "type": "response_item", "payload": map[string]any{
			"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "old"}},
		}},
	})
	writeJSONL(t, filepath.Join(sessions, "rollout-2026-06-24T11-30-00-"+idNewReply+".jsonl"), []map[string]any{
		{"timestamp": "2026-06-24T11:30:00Z", "type": "response_item", "payload": map[string]any{
			"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "new"}},
		}},
	})

	rows := codexSessions(index, []string{sessions}, 10)
	if len(rows) != 2 {
		t.Fatalf("rows=%#v", rows)
	}
	if rows[0]["native_session_id"] != idNewReply {
		t.Fatalf("expected latest reply first: %#v", rows)
	}
	if rows[0]["last_reply_at"] != "2026-06-24T11:30:00Z" {
		t.Fatalf("bad last_reply_at: %#v", rows[0])
	}
}

func TestCodexSessionsIncludeRolloutsMissingFromIndex(t *testing.T) {
	base := t.TempDir()
	index := filepath.Join(base, "session_index.jsonl")
	sessions := filepath.Join(base, "sessions")
	if err := os.MkdirAll(sessions, 0o755); err != nil {
		t.Fatal(err)
	}
	indexedID := "019ef111-1111-7111-8111-111111111111"
	rolloutOnlyID := "019ef222-2222-7222-8222-222222222222"
	writeJSONL(t, index, []map[string]any{{"id": indexedID, "thread_name": "indexed"}})
	writeJSONL(t, filepath.Join(sessions, "rollout-2026-07-14T20-15-23-"+rolloutOnlyID+".jsonl"), []map[string]any{
		{"timestamp": "2026-07-14T12:15:23Z", "type": "session_meta", "payload": map[string]any{"cwd": "/repo/current"}},
	})

	rows := codexSessions(index, []string{sessions}, 10)
	if len(rows) != 2 {
		t.Fatalf("rows=%#v", rows)
	}
	var rolloutOnly map[string]any
	for _, row := range rows {
		if row["native_session_id"] == rolloutOnlyID {
			rolloutOnly = row
		}
	}
	if rolloutOnly == nil || rolloutOnly["cwd"] != "/repo/current" || rolloutOnly["source"] != "codex" {
		t.Fatalf("rollout-only session missing metadata: %#v", rolloutOnly)
	}
}

func TestClaudePendingQuestionTracksUnansweredAskUserQuestion(t *testing.T) {
	base := t.TempDir()
	proj := filepath.Join(base, "-p")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(proj, "qid.jsonl")
	writeJSONL(t, path, []map[string]any{
		{"type": "assistant", "timestamp": "2026-06-24T12:52:04Z", "message": map[string]any{"content": []any{
			map[string]any{"type": "tool_use", "id": "toolu_1", "name": "AskUserQuestion", "input": map[string]any{"questions": []any{
				map[string]any{"header": "Push 方式", "question": "怎么更新远程分支?", "multiSelect": false, "options": []any{
					map[string]any{"label": "删远程 develop 再 push 同名", "description": "可能被保护"},
					map[string]any{"label": "基于真实历史重新 commit", "description": "推荐"},
				}},
			}}},
		}}},
	})

	q := claudePendingQuestion("qid", base)
	if q == nil || q["type"] != "question" || q["summary"] != "怎么更新远程分支?" {
		t.Fatalf("bad question: %#v", q)
	}
	questions := q["questions"].([]map[string]any)
	if len(questions) != 1 || questions[0]["header"] != "Push 方式" {
		t.Fatalf("bad questions: %#v", questions)
	}
	opts := questions[0]["options"].([]map[string]any)
	if len(opts) != 2 || opts[1]["label"] != "基于真实历史重新 commit" {
		t.Fatalf("bad options: %#v", opts)
	}

	writeJSONL(t, path, []map[string]any{
		{"type": "assistant", "message": map[string]any{"content": []any{
			map[string]any{"type": "tool_use", "id": "toolu_1", "name": "AskUserQuestion", "input": map[string]any{"questions": []any{
				map[string]any{"header": "Push 方式", "question": "怎么更新远程分支?", "options": []any{map[string]any{"label": "A"}}},
			}}},
		}}},
		{"type": "user", "message": map[string]any{"content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": "Your questions have been answered"},
		}}},
	})
	if q := claudePendingQuestion("qid", base); q != nil {
		t.Fatalf("answered question should not remain pending: %#v", q)
	}
}

func TestClaudePendingNativeUIPromptLifecycle(t *testing.T) {
	base := t.TempDir()
	proj := filepath.Join(base, "-project")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(proj, "native-ui.jsonl")
	writeJSONL(t, path, []map[string]any{
		{"type": "assistant", "message": map[string]any{"content": []any{
			map[string]any{"type": "tool_use", "id": "tool-access-1", "name": "mcp__computer-use__request_access", "input": map[string]any{}},
		}}},
	})

	request := claudePendingNativeUIPrompt("native-ui", base)
	if request == nil || request["type"] != "native_ui" || request["request_id"] != "tool-access-1" || boolAny(request["actionable"]) {
		t.Fatalf("bad native UI request: %#v", request)
	}

	writeJSONL(t, path, []map[string]any{
		{"type": "assistant", "message": map[string]any{"content": []any{
			map[string]any{"type": "tool_use", "id": "tool-access-1", "name": "mcp__computer-use__request_access", "input": map[string]any{}},
		}}},
		{"type": "user", "message": map[string]any{"content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "tool-access-1", "content": "resolved"},
		}}},
	})
	if request := claudePendingNativeUIPrompt("native-ui", base); request != nil {
		t.Fatalf("resolved native UI request should not remain pending: %#v", request)
	}
}

func TestClaudePendingNativeUIPromptScansTranscriptTail(t *testing.T) {
	base := t.TempDir()
	proj := filepath.Join(base, "-project")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSONL(t, filepath.Join(proj, "tail-native.jsonl"), []map[string]any{
		{"type": "assistant", "message": map[string]any{"content": strings.Repeat("x", jsonlTailScanBytes+1024)}},
		{"type": "assistant", "message": map[string]any{"content": []any{
			map[string]any{"type": "tool_use", "id": "tool-tail", "name": "request_access", "input": map[string]any{}},
		}}},
	})
	if request := claudePendingNativeUIPrompt("tail-native", base); request == nil || request["request_id"] != "tool-tail" {
		t.Fatalf("pending request after a large transcript prefix was missed: %#v", request)
	}
}

func TestClaudeSessionModelAggregatesUsage(t *testing.T) {
	base := t.TempDir()
	proj := filepath.Join(base, "-p")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSONL(t, filepath.Join(proj, "sx.jsonl"), []map[string]any{
		{"type": "assistant", "message": map[string]any{"model": "<synthetic>", "content": []any{}}},
		{"type": "assistant", "message": map[string]any{"model": "claude-opus-4-8", "usage": map[string]any{
			"speed": "standard", "service_tier": "standard", "input_tokens": 100, "cache_read_input_tokens": 4000, "cache_creation_input_tokens": 300, "output_tokens": 50,
		}}},
		{"type": "assistant", "message": map[string]any{"model": "claude-opus-4-8", "usage": map[string]any{
			"speed": "standard", "input_tokens": 200, "cache_read_input_tokens": 9000, "output_tokens": 70,
		}}},
	})

	model := claudeSessionModel("sx", base)
	if model["model"] != "claude-opus-4-8" || model["speed"] != "standard" || model["service_tier"] != "standard" {
		t.Fatalf("bad model: %#v", model)
	}
	if model["context_tokens"] != 9200 || model["output_tokens"] != 120 {
		t.Fatalf("bad usage: %#v", model)
	}
	usage, _ := model["usage"].([]map[string]any)
	if len(usage) != 1 {
		t.Fatalf("expected one model usage row: %#v", model)
	}
	row := usage[0]
	if row["input_tokens"] != int64(300) || row["output_tokens"] != int64(120) ||
		row["cache_creation_input_tokens"] != int64(300) || row["cache_read_input_tokens"] != int64(13000) ||
		row["total_tokens"] != int64(13720) {
		t.Fatalf("bad per-model usage: %#v", row)
	}
}

func TestCodexSessionUsageAttributesCumulativeDeltasByModel(t *testing.T) {
	base := t.TempDir()
	sessionID := "019ecffb-fd7d-7422-b05c-2e1c3ebec53d"
	path := filepath.Join(base, "rollout-2026-07-13T00-00-00-"+sessionID+".jsonl")
	writeJSONL(t, path, []map[string]any{
		{"type": "turn_context", "payload": map[string]any{"model": "gpt-a"}},
		{"type": "event_msg", "payload": map[string]any{"type": "token_count", "info": map[string]any{"total_token_usage": map[string]any{
			"input_tokens": 100, "cached_input_tokens": 40, "output_tokens": 20, "total_tokens": 120,
		}}}},
		// A repeated total at the next turn boundary must not be double-counted.
		{"type": "event_msg", "payload": map[string]any{"type": "token_count", "info": map[string]any{"total_token_usage": map[string]any{
			"input_tokens": 100, "cached_input_tokens": 40, "output_tokens": 20, "total_tokens": 120,
		}}}},
		{"type": "turn_context", "payload": map[string]any{"model": "gpt-b"}},
		{"type": "event_msg", "payload": map[string]any{"type": "token_count", "info": map[string]any{"total_token_usage": map[string]any{
			"input_tokens": 250, "cached_input_tokens": 100, "output_tokens": 50, "total_tokens": 300,
		}}}},
	})

	rows := codexSessionUsage(sessionID, []string{base})
	if len(rows) != 2 {
		t.Fatalf("expected two per-model rows: %#v", rows)
	}
	if rows[0]["model"] != "gpt-a" || rows[0]["input_tokens"] != int64(60) || rows[0]["cache_read_input_tokens"] != int64(40) ||
		rows[0]["output_tokens"] != int64(20) || rows[0]["total_tokens"] != int64(120) {
		t.Fatalf("bad first model usage: %#v", rows[0])
	}
	if rows[1]["model"] != "gpt-b" || rows[1]["input_tokens"] != int64(90) || rows[1]["cache_read_input_tokens"] != int64(60) ||
		rows[1]["output_tokens"] != int64(30) || rows[1]["total_tokens"] != int64(180) {
		t.Fatalf("bad second model usage: %#v", rows[1])
	}
}

func TestClaudeSessionMessagesAddsOneDeduplicatedUsageAnnotationPerTurn(t *testing.T) {
	base := t.TempDir()
	proj := filepath.Join(base, "-p")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	usage := map[string]any{
		"input_tokens": 100, "cache_creation_input_tokens": 20,
		"cache_read_input_tokens": 300, "output_tokens": 40,
	}
	writeJSONL(t, filepath.Join(proj, "turns.jsonl"), []map[string]any{
		{"type": "user", "timestamp": "2026-07-14T01:00:00Z", "message": map[string]any{"content": "first"}},
		{"type": "assistant", "timestamp": "2026-07-14T01:00:02Z", "requestId": "req-1", "message": map[string]any{
			"id": "msg-1", "model": "claude-opus-4-8", "content": []any{map[string]any{"type": "thinking", "thinking": "x"}}, "usage": usage,
		}},
		// Claude writes separate thinking/text records for the same API message.
		// The repeated usage object must be counted only once.
		{"type": "assistant", "timestamp": "2026-07-14T01:00:03Z", "requestId": "req-1", "message": map[string]any{
			"id": "msg-1", "model": "claude-opus-4-8", "content": []any{map[string]any{"type": "text", "text": "done"}}, "usage": usage,
		}},
		{"type": "user", "timestamp": "2026-07-14T01:01:00Z", "message": map[string]any{"content": "second"}},
	})

	items := claudeSessionMessages("turns", base, 800)
	var annotations []map[string]any
	for _, item := range items {
		if item["kind"] == "turn_usage" {
			annotations = append(annotations, item)
		}
	}
	if len(annotations) != 1 {
		t.Fatalf("annotations=%#v items=%#v", annotations, items)
	}
	u := mapAny(annotations[0]["usage"])
	if u["input_tokens"] != int64(100) || u["output_tokens"] != int64(40) ||
		u["cache_creation_input_tokens"] != int64(20) || u["cache_read_input_tokens"] != int64(300) ||
		u["duration_ms"] != int64(3000) {
		t.Fatalf("bad Claude turn usage: %#v", u)
	}
	model := claudeSessionModel("turns", base)
	rows := model["usage"].([]map[string]any)
	if rows[0]["total_tokens"] != int64(460) {
		t.Fatalf("footer usage double-counted repeated message: %#v", rows)
	}
}

func TestCodexSessionMessagesAddsCompletedTurnUsageDeltaAndDuration(t *testing.T) {
	base := t.TempDir()
	id := "019ef222-2222-7222-8222-222222222222"
	writeJSONL(t, filepath.Join(base, "rollout-x-"+id+".jsonl"), []map[string]any{
		{"timestamp": "2026-07-14T01:00:00Z", "type": "event_msg", "payload": map[string]any{"type": "token_count", "info": map[string]any{"total_token_usage": map[string]any{
			"input_tokens": 100, "cached_input_tokens": 40, "output_tokens": 20, "total_tokens": 120,
		}}}},
		{"timestamp": "2026-07-14T01:01:00Z", "type": "turn_context", "payload": map[string]any{"model": "gpt-5.6-sol", "turn_id": "turn-1"}},
		{"timestamp": "2026-07-14T01:01:00Z", "type": "event_msg", "payload": map[string]any{"type": "task_started", "turn_id": "turn-1"}},
		{"timestamp": "2026-07-14T01:01:01Z", "type": "response_item", "payload": map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "done"}}}},
		{"timestamp": "2026-07-14T01:01:02Z", "type": "event_msg", "payload": map[string]any{"type": "token_count", "info": map[string]any{"total_token_usage": map[string]any{
			"input_tokens": 350, "cached_input_tokens": 140, "output_tokens": 70, "total_tokens": 420,
		}}}},
		{"timestamp": "2026-07-14T01:01:03Z", "type": "event_msg", "payload": map[string]any{"type": "task_complete", "turn_id": "turn-1", "duration_ms": 3210}},
	})

	items := codexSessionMessages(id, []string{base}, 800)
	last := items[len(items)-1]
	if last["kind"] != "turn_usage" || last["turn_id"] != "turn-1" {
		t.Fatalf("missing Codex turn annotation: %#v", items)
	}
	u := mapAny(last["usage"])
	// Input is shown exclusive of cached input: (350-100) - (140-40) = 150.
	if u["input_tokens"] != int64(150) || u["cache_read_input_tokens"] != int64(100) ||
		u["output_tokens"] != int64(50) || u["total_tokens"] != int64(300) || u["duration_ms"] != int64(3210) {
		t.Fatalf("bad Codex turn usage: %#v", u)
	}
}

func TestClaudeListNativeSessionsMergesCLIAndDesktop(t *testing.T) {
	base := t.TempDir()
	cliBase := filepath.Join(base, "projects")
	deskBase := filepath.Join(base, "desktop")
	proj := filepath.Join(cliBase, "-p")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(deskBase, "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSONL(t, filepath.Join(proj, "dup.jsonl"), []map[string]any{
		{"type": "user", "message": map[string]any{"content": "from cli"}, "cwd": "/tmp/project"},
	})
	writeJSONFile(t, filepath.Join(deskBase, "x", "local_dup.json"), map[string]any{
		"sessionId": "desktop-local", "cliSessionId": "dup", "title": "from desktop", "lastActivityAt": float64(1000),
	})
	writeJSONFile(t, filepath.Join(deskBase, "x", "local_desk.json"), map[string]any{
		"sessionId": "desktop-only", "cliSessionId": "onlydesk", "title": "desk", "lastActivityAt": float64(2000),
	})
	c := NewClaudeCLI("claude", config.ProviderConfig{Command: "/bin/echo", Extra: map[string]any{"claude_projects_dir": cliBase, "claude_code_sessions_dir": deskBase}})

	rows := c.ListNativeSessions()
	byID := map[string]map[string]any{}
	for _, row := range rows {
		byID[stringAny(row["cli_session_id"])] = row
	}
	if len(byID) != 2 {
		t.Fatalf("bad rows: %#v", rows)
	}
	if byID["dup"]["origin"] != "both" {
		t.Fatalf("dup not merged: %#v", byID["dup"])
	}
	if byID["dup"]["title"] != "from desktop" || byID["dup"]["desktop_session_id"] != "desktop-local" {
		t.Fatalf("desktop metadata not merged into CLI transcript: %#v", byID["dup"])
	}
	if byID["onlydesk"]["origin"] != "desktop" {
		t.Fatalf("desktop origin: %#v", byID["onlydesk"])
	}
}

func findKind(items []map[string]any, kind string) map[string]any {
	for _, item := range items {
		if item["kind"] == kind {
			return item
		}
	}
	return nil
}

func writeJSONL(t *testing.T, path string, rows []map[string]any) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, row := range rows {
		b, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write(append(b, '\n')); err != nil {
			t.Fatal(err)
		}
	}
}

func writeJSONFile(t *testing.T, path string, row map[string]any) {
	t.Helper()
	b, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}
