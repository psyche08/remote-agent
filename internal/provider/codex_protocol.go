package provider

import (
	"errors"
	"sort"
	"strings"
)

const codexAppServerSource = "codex_app_server"

func codexThreadListToSessions(result any) []map[string]any {
	out := []map[string]any{}
	for _, thread := range codexThreadRows(result) {
		if row := codexThreadToSession(thread); row != nil {
			out = append(out, row)
		}
	}
	sortByUpdated(out)
	return out
}

func codexThreadRows(result any) []map[string]any {
	if m := mapAny(result); len(m) > 0 {
		if rows := mapsFromAny(m["data"]); rows != nil {
			return rows
		}
		return mapsFromAny(m["threads"])
	}
	return mapsFromAny(result)
}

func codexThreadToSession(thread map[string]any) map[string]any {
	tid := firstNonEmpty(stringAny(thread["id"]), stringAny(thread["sessionId"]))
	if tid == "" {
		return nil
	}
	turns := codexThreadTurns(thread)
	gitInfo := mapAny(thread["gitInfo"])
	status := codexThreadStatus(thread)
	lastReplyAt := codexThreadLastReplyAt(turns)
	return map[string]any{
		"native_session_id": tid,
		"cli_session_id":    tid,
		"title":             codexThreadTitle(thread),
		"cwd":               nullableString(thread["cwd"]),
		"branch":            nullableString(gitInfo["branch"]),
		"worktree":          nil,
		"model":             nullableString(thread["model"]),
		"completed_turns":   nullableInt(len(turns)),
		"archived":          boolAny(thread["archived"]),
		"status":            nullableNonEmpty(status),
		"live":              status == "active",
		"created_at":        codexTimestampISO(thread["createdAt"]),
		"updated_at": codexTimestampISO(firstValue(
			firstValue(thread["updatedAt"], thread["recencyAt"]),
			thread["createdAt"],
		)),
		"last_reply_at": nullableString(lastReplyAt),
		"source":        codexAppServerSource,
	}
}

func codexThreadLastReplyAt(turns []map[string]any) string {
	for i := len(turns) - 1; i >= 0; i-- {
		turn := turns[i]
		items := mapsFromAny(turn["items"])
		for j := len(items) - 1; j >= 0; j-- {
			if codexItemIsReply(items[j]) {
				return codexTimestampISO(firstValue(
					firstValue(turn["completedAt"], turn["updatedAt"]),
					turn["startedAt"],
				))
			}
		}
	}
	return ""
}

func codexItemIsReply(item map[string]any) bool {
	typ := strings.ToLower(stringAny(item["type"]))
	if typ == "" || strings.Contains(typ, "user") {
		return false
	}
	return strings.Contains(typ, "agent") || strings.Contains(typ, "assistant") ||
		strings.Contains(typ, "tool") || strings.Contains(typ, "function") ||
		strings.Contains(typ, "output") || strings.Contains(typ, "result") ||
		strings.Contains(typ, "reasoning")
}

func codexThreadStatus(thread map[string]any) string {
	status := mapAny(thread["status"])
	if typ := stringAny(status["type"]); typ != "" {
		return typ
	}
	return stringAny(firstValue(thread["status"], thread["state"]))
}

func codexThreadTitle(thread map[string]any) string {
	return firstNonEmpty(
		firstNonEmpty(stringAny(thread["name"]), shortText(stringAny(thread["preview"]), 80)),
		"session "+shortText(firstNonEmpty(stringAny(thread["id"]), stringAny(thread["sessionId"])), 8),
	)
}

func codexTimestampISO(v any) string {
	n, ok := numberToInt64(v)
	if !ok {
		if s := stringAny(v); s != "" {
			if parsed, ok := parseFloat64(s); ok {
				return epochToISO(normalizeEpoch(parsed))
			}
		}
		return ""
	}
	return epochToISO(normalizeEpoch(float64(n)))
}

func normalizeEpoch(ts float64) float64 {
	if ts > 1_000_000_000_000 {
		return ts / 1000
	}
	return ts
}

func codexThreadFromResume(result any) map[string]any {
	m := mapAny(result)
	thread := mapAny(m["thread"])
	if len(thread) == 0 {
		return nil
	}
	if thread["initialTurnsPage"] == nil && m["initialTurnsPage"] != nil {
		cp := map[string]any{}
		for k, v := range thread {
			cp[k] = v
		}
		cp["initialTurnsPage"] = m["initialTurnsPage"]
		return cp
	}
	return thread
}

func codexThreadToMessages(thread map[string]any, maxItems int) []map[string]any {
	cwd := stringAny(thread["cwd"])
	threadID := firstNonEmpty(stringAny(thread["id"]), stringAny(thread["threadId"]))
	out := []map[string]any{}
	pending := map[string]map[string]any{}
	for _, turn := range codexThreadTurns(thread) {
		ts := codexTimestampISO(firstValue(turn["startedAt"], turn["completedAt"]))
		turnID := firstNonEmpty(stringAny(turn["id"]), stringAny(turn["turnId"]))
		for _, item := range listAny(turn["items"]) {
			out = append(out, codexItemToMessages(mapAny(item), cwd, pending, ts, threadID, turnID)...)
		}
	}
	if maxItems == 0 {
		maxItems = nativePreviewMaxItems
	}
	if maxItems > 0 && len(out) > maxItems {
		return out[len(out)-maxItems:]
	}
	return out
}

func codexThreadTurns(thread map[string]any) []map[string]any {
	if rows := mapsFromAny(thread["turns"]); rows != nil {
		return rows
	}
	page := thread["initialTurnsPage"]
	if page == nil {
		page = thread["turnsPage"]
	}
	if p := mapAny(page); len(p) > 0 {
		if rows := mapsFromAny(p["turns"]); rows != nil {
			return rows
		}
		return mapsFromAny(p["items"])
	}
	return mapsFromAny(page)
}

func codexRollbackTurnCount(thread map[string]any, targetTurnID string) (int, error) {
	if targetTurnID == "" {
		return 0, errors.New("turn_id is required")
	}
	turns := codexThreadTurns(thread)
	for i, turn := range turns {
		turnID := firstNonEmpty(stringAny(turn["id"]), stringAny(turn["turnId"]))
		if turnID == targetTurnID {
			return len(turns) - i, nil
		}
	}
	return 0, errors.New("turn_id not found in codex thread")
}

func codexCallID(item map[string]any) string {
	for _, key := range []string{"callId", "call_id", "toolCallId", "functionCallId", "itemId", "id"} {
		if v := stringAny(item[key]); v != "" {
			return v
		}
	}
	return ""
}

func codexItemMeta(item map[string]any, threadID string, turnID string) map[string]any {
	meta := map[string]any{"source": codexAppServerSource}
	if threadID != "" {
		meta["thread_id"] = threadID
	}
	if turnID != "" {
		meta["turn_id"] = turnID
	}
	if cid := codexCallID(item); cid != "" {
		meta["call_id"] = cid
	}
	if itemID := firstNonEmpty(stringAny(item["itemId"]), stringAny(item["id"])); itemID != "" {
		meta["item_id"] = itemID
	}
	if typ := stringAny(item["type"]); typ != "" {
		meta["item_type"] = typ
	}
	return meta
}

func codexItemToMessages(item map[string]any, cwd string, pending map[string]map[string]any, ts string, threadID string, turnID string) []map[string]any {
	if len(item) == 0 {
		return nil
	}
	typ := strings.ToLower(stringAny(item["type"]))
	meta := codexItemMeta(item, threadID, turnID)
	withMeta := func(m map[string]any) map[string]any {
		for k, v := range meta {
			m[k] = v
		}
		return m
	}
	switch typ {
	case "usermessage", "user_message":
		text := blocksText(item["content"], map[string]bool{"text": true, "input_text": true})
		if text != "" {
			return []map[string]any{withMeta(map[string]any{"role": "user", "kind": "text", "text": text, "ts": ts})}
		}
		return nil
	case "agentmessage", "agent_message":
		text := firstNonEmpty(stringAny(item["text"]), blocksText(item["content"], map[string]bool{"text": true, "output_text": true}))
		text = stripInternalTags(text)
		if text != "" {
			return []map[string]any{withMeta(map[string]any{"role": "assistant", "kind": "text", "text": text, "ts": ts})}
		}
		return nil
	case "agentreasoning", "agent_reasoning", "reasoning":
		text := firstNonEmpty(firstNonEmpty(stringAny(item["text"]), stringAny(item["reasoning"])), blocksText(item["content"], map[string]bool{"text": true, "summary": true}))
		if text != "" {
			return []map[string]any{withMeta(map[string]any{"role": "assistant", "kind": "thinking", "text": text, "ts": ts})}
		}
		return nil
	}

	if strings.Contains(typ, "output") || strings.HasSuffix(typ, "result") {
		cid := codexCallID(item)
		output := item["output"]
		if output == nil {
			output = item["result"]
		}
		if output == nil {
			output = item["content"]
		}
		result := codexOutputText(output, 2500)
		if target := pending[cid]; target != nil {
			target["result"] = result
			return nil
		}
		if result != "" {
			return []map[string]any{withMeta(map[string]any{
				"role": "assistant", "kind": "tool", "name": firstNonEmpty(stringAny(item["name"]), typ),
				"text": "", "io": nil, "files": []any{}, "result": result, "ts": ts,
			})}
		}
		return nil
	}

	looksLikeCall := strings.Contains(typ, "call") || strings.Contains(typ, "tool") || strings.Contains(typ, "command") ||
		item["arguments"] != nil || item["input"] != nil || item["command"] != nil
	if looksLikeCall {
		name := firstNonEmpty(firstNonEmpty(firstNonEmpty(stringAny(item["name"]), stringAny(item["tool"])), stringAny(item["toolName"])), firstNonEmpty(typ, "tool"))
		input := item["arguments"]
		if input == nil {
			input = item["input"]
		}
		if input == nil && item["command"] != nil {
			input = map[string]any{"command": item["command"]}
		}
		tool := withMeta(map[string]any{
			"role": "assistant", "kind": "tool", "name": name, "text": toolDetail(input, 160),
			"io": toolIO(name, input), "files": extractPaths(name, input, cwd), "result": nil, "ts": ts,
		})
		if cid := codexCallID(item); cid != "" {
			pending[cid] = tool
		}
		return []map[string]any{tool}
	}
	return nil
}

func mapsFromAny(v any) []map[string]any {
	list := listAny(v)
	if list == nil {
		return nil
	}
	out := []map[string]any{}
	for _, raw := range list {
		if m := mapAny(raw); len(m) > 0 {
			out = append(out, m)
		}
	}
	return out
}

func nullableInt(v int) any {
	if v == 0 {
		return nil
	}
	return v
}

func parseFloat64(s string) (float64, bool) {
	var sign, intPart, fracPart float64
	sign = 1
	if strings.HasPrefix(s, "-") {
		sign = -1
		s = s[1:]
	}
	parts := strings.SplitN(s, ".", 2)
	for _, r := range parts[0] {
		if r < '0' || r > '9' {
			return 0, false
		}
		intPart = intPart*10 + float64(r-'0')
	}
	if len(parts) == 2 {
		div := float64(10)
		for _, r := range parts[1] {
			if r < '0' || r > '9' {
				return 0, false
			}
			fracPart += float64(r-'0') / div
			div *= 10
		}
	}
	return sign * (intPart + fracPart), true
}

func sortSessionRows(rows []map[string]any) {
	sort.Slice(rows, func(i, j int) bool {
		return sessionSortAt(rows[i]) > sessionSortAt(rows[j])
	})
}
