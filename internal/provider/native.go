package provider

import (
	"bufio"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	claudeDesktopDefaultDir = "~/Library/Application Support/Claude/claude-code-sessions"
	claudeCLIDefaultDir     = "~/.claude/projects"
	codexIndexDefaultPath   = "~/.codex/session_index.jsonl"
	nativeSessionListLimit  = 200
	nativePreviewMaxItems   = 800
	nativePreviewUnlimited  = -1
	jsonlTailScanBytes      = 4 * 1024 * 1024
)

var (
	pathishExtRE  = regexp.MustCompile(`(?i)\.(py|js|ts|tsx|jsx|json|jsonl|yaml|yml|toml|md|txt|html|css|sh|go|rs|java|kt|kts|swift|c|h|cpp|hpp|m|mm|rb|php|sql|csv|log|conf|ini|plist|xml|lock|env|gradle)$`)
	oaiInternalRE = regexp.MustCompile(`(?s)<oai-[\w-]+>.*?</oai-[\w-]+>`)
	directiveRE   = regexp.MustCompile(`(?m)^[ \t]*::[\w-]+\{[^\n]*\}[ \t]*\n?`)
	rolloutIDRE   = regexp.MustCompile(`-([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})\.jsonl$`)
	blankRE       = regexp.MustCompile(`\n{3,}`)
)

func msToISO(v any) string {
	n, ok := numberToInt64(v)
	if !ok {
		return ""
	}
	return time.UnixMilli(n).UTC().Format(time.RFC3339Nano)
}

func epochToISO(sec float64) string {
	if sec <= 0 {
		return ""
	}
	ns := int64(sec * float64(time.Second))
	return time.Unix(0, ns).UTC().Format(time.RFC3339Nano)
}

func shortText(text string, n int) string {
	text = strings.Join(strings.Fields(text), " ")
	runes := []rune(text)
	if len(runes) <= n {
		return text
	}
	return string(runes[:n])
}

func numberToInt64(v any) (int64, bool) {
	switch x := v.(type) {
	case float64:
		return int64(x), true
	case int64:
		return x, true
	case int:
		return int64(x), true
	case json.Number:
		n, err := x.Int64()
		return n, err == nil
	}
	return 0, false
}

func claudeDesktopSessions(base string, limit int) []map[string]any {
	base = expandUser(firstNonEmpty(base, claudeDesktopDefaultDir))
	if st, err := os.Stat(base); err != nil || !st.IsDir() {
		return nil
	}
	out := []map[string]any{}
	_ = filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasPrefix(d.Name(), "local_") || !strings.HasSuffix(d.Name(), ".json") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		var rec map[string]any
		if err := json.Unmarshal(b, &rec); err != nil {
			return nil
		}
		row := map[string]any{
			"native_session_id":  firstNonEmpty(stringAny(rec["sessionId"]), filepath.Base(path)),
			"cli_session_id":     nullableString(rec["cliSessionId"]),
			"bridge_session_ids": rec["bridgeSessionIds"],
			"title":              firstNonEmpty(stringAny(rec["title"]), "(untitled)"),
			"cwd":                firstNonEmpty(stringAny(rec["cwd"]), stringAny(rec["originCwd"])),
			"branch":             nullableString(rec["branch"]),
			"worktree":           nullableString(rec["worktreeName"]),
			"model":              nullableString(rec["model"]),
			"completed_turns":    rec["completedTurns"],
			"archived":           boolAny(rec["isArchived"]),
			"created_at":         msToISO(rec["createdAt"]),
			"updated_at":         msToISO(firstValue(rec["lastActivityAt"], rec["lastFocusedAt"])),
			"last_reply_at": msToISO(firstValue(
				firstValue(rec["lastAssistantMessageAt"], rec["lastAssistantAt"]),
				rec["lastResponseAt"],
			)),
			"source": "claude_desktop",
		}
		out = append(out, row)
		return nil
	})
	sortByUpdated(out)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

func claudeCLISessions(base string, limit int) []map[string]any {
	base = expandUser(firstNonEmpty(base, claudeCLIDefaultDir))
	if st, err := os.Stat(base); err != nil || !st.IsDir() {
		return nil
	}
	files, _ := filepath.Glob(filepath.Join(base, "*", "*.jsonl"))
	sort.Slice(files, func(i, j int) bool {
		mi := fileMTime(files[i])
		mj := fileMTime(files[j])
		return mi.After(mj)
	})
	if limit > 0 && len(files) > limit {
		files = files[:limit]
	}
	out := []map[string]any{}
	for _, path := range files {
		sid := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		meta := claudeCLIMeta(path, 80)
		lastReplyAt := claudeCLILastReplyAt(path)
		out = append(out, map[string]any{
			"native_session_id": sid,
			"cli_session_id":    sid,
			"title":             firstNonEmpty(stringAny(meta["title"]), "session "+shortText(sid, 8)),
			"cwd":               nullableString(meta["cwd"]),
			"branch":            nullableString(meta["branch"]),
			"worktree":          nil,
			"model":             nil,
			"completed_turns":   nil,
			"archived":          false,
			"created_at":        "",
			"updated_at":        epochToISO(float64(fileMTime(path).UnixNano()) / 1e9),
			"last_reply_at":     nullableString(lastReplyAt),
			"source":            "claude_cli",
		})
	}
	return out
}

func claudeCLILastReplyAt(path string) string {
	return jsonlTailTimestamp(path, jsonlTailScanBytes, func(rec map[string]any) bool {
		return stringAny(rec["type"]) == "assistant" && stringAny(rec["timestamp"]) != ""
	})
}

func jsonlTailTimestamp(path string, maxBytes int64, match func(map[string]any) bool) string {
	records := jsonlTailRecords(path, maxBytes)
	for i := len(records) - 1; i >= 0; i-- {
		if match(records[i]) {
			return stringAny(records[i]["timestamp"])
		}
	}
	return ""
}

func jsonlTailRecords(path string, maxBytes int64) []map[string]any {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.Size() <= 0 {
		return nil
	}
	start := int64(0)
	if maxBytes > 0 && st.Size() > maxBytes {
		start = st.Size() - maxBytes
	}
	buf := make([]byte, st.Size()-start)
	n, err := f.ReadAt(buf, start)
	if err != nil && err != io.EOF {
		return nil
	}
	lines := strings.Split(string(buf[:n]), "\n")
	if start > 0 && len(lines) > 0 {
		lines = lines[1:]
	}
	records := make([]map[string]any, 0, len(lines))
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		records = append(records, rec)
	}
	return records
}

func claudeCLIMeta(path string, maxLines int) map[string]any {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	meta := map[string]any{}
	queueTitle := ""
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for i := 0; scanner.Scan() && i <= maxLines; i++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if meta["cwd"] == nil && stringAny(rec["cwd"]) != "" {
			meta["cwd"] = stringAny(rec["cwd"])
		}
		if meta["branch"] == nil && stringAny(rec["gitBranch"]) != "" {
			meta["branch"] = stringAny(rec["gitBranch"])
		}
		typ := stringAny(rec["type"])
		if meta["title"] == nil && typ == "summary" && stringAny(rec["summary"]) != "" {
			meta["title"] = shortText(stringAny(rec["summary"]), 80)
		} else if meta["title"] == nil && typ == "user" && !boolAny(rec["isCompactSummary"]) && !boolAny(rec["isMeta"]) {
			txt := msgText(rec["message"])
			if txt != "" && !strings.HasPrefix(txt, "<") {
				meta["title"] = shortText(txt, 80)
			}
		} else if queueTitle == "" && typ == "queue-operation" && stringAny(rec["content"]) != "" {
			queueTitle = shortText(stringAny(rec["content"]), 80)
		}
		if meta["title"] != nil && meta["cwd"] != nil {
			break
		}
	}
	if meta["title"] == nil && queueTitle != "" {
		meta["title"] = queueTitle
	}
	return meta
}

func claudeTranscriptPath(sessionID string, projectsDir string) string {
	base := expandUser(firstNonEmpty(projectsDir, claudeCLIDefaultDir))
	hits, _ := filepath.Glob(filepath.Join(base, "*", sessionID+".jsonl"))
	if len(hits) == 0 {
		return ""
	}
	return hits[0]
}

func claudeSessionMessages(sessionID string, projectsDir string, maxItems int) []map[string]any {
	path := claudeTranscriptPath(sessionID, projectsDir)
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	out := []map[string]any{}
	pending := map[string]map[string]any{}
	curCwd := ""
	var turn *turnUsageState
	seenUsage := map[string]bool{}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if c := stringAny(rec["cwd"]); c != "" {
			curCwd = c
		}
		if claudeHumanTurnStart(rec) {
			if item := turnUsageItem(turn, 0, ""); item != nil {
				out = append(out, item)
			}
			turn = newTurnUsage(stringAny(rec["timestamp"]))
		}
		out = append(out, claudeRecordToItems(rec, curCwd, pending)...)
		if stringAny(rec["type"]) == "assistant" {
			if turn == nil {
				turn = newTurnUsage(stringAny(rec["timestamp"]))
			}
			turn.observe(stringAny(rec["timestamp"]))
			msg := mapAny(rec["message"])
			usage := mapAny(msg["usage"])
			usageID := firstNonEmpty(stringAny(msg["id"]), firstNonEmpty(stringAny(rec["requestId"]), stringAny(rec["uuid"])))
			if len(usage) > 0 && (usageID == "" || !seenUsage[usageID]) {
				seenUsage[usageID] = true
				turn.add(stringAny(msg["model"]), usage)
			}
		}
	}
	if item := turnUsageItem(turn, 0, ""); item != nil {
		out = append(out, item)
	}
	if maxItems == 0 {
		maxItems = nativePreviewMaxItems
	}
	if maxItems > 0 && len(out) > maxItems {
		return out[len(out)-maxItems:]
	}
	return out
}

func claudePendingQuestion(sessionID string, projectsDir string) map[string]any {
	path := claudeTranscriptPath(sessionID, projectsDir)
	if path == "" {
		return nil
	}
	pending := map[string]map[string]any{}
	order := []string{}
	for _, rec := range jsonlTailRecords(path, jsonlTailScanBytes) {
		content := mapAny(rec["message"])["content"]
		switch stringAny(rec["type"]) {
		case "assistant":
			for _, raw := range listAny(content) {
				block := mapAny(raw)
				if stringAny(block["type"]) != "tool_use" || stringAny(block["name"]) != "AskUserQuestion" {
					continue
				}
				id := stringAny(block["id"])
				if id == "" {
					id = stringAny(rec["uuid"])
				}
				q := claudeAskUserQuestionRequest(block["input"], id, stringAny(rec["timestamp"]))
				if q == nil {
					continue
				}
				if pending[id] == nil {
					order = append(order, id)
				}
				pending[id] = q
			}
		case "user":
			clearAll := false
			for _, raw := range listAny(content) {
				block := mapAny(raw)
				if stringAny(block["type"]) != "tool_result" {
					continue
				}
				if id := stringAny(block["tool_use_id"]); id != "" {
					delete(pending, id)
					continue
				}
				if strings.Contains(stringAny(block["content"]), "Your questions have been answered") {
					clearAll = true
				}
			}
			if clearAll {
				pending = map[string]map[string]any{}
			}
		}
	}
	for i := len(order) - 1; i >= 0; i-- {
		if q := pending[order[i]]; q != nil {
			return q
		}
	}
	return nil
}

// claudePendingNativeUIPrompt detects Desktop-owned access prompts that do not
// travel through the Claude CLI control_request protocol. MCP tools such as
// computer-use request_access remain as an unresolved tool_use while Claude
// Desktop shows its native permission card. These requests are deliberately
// non-actionable because the native button may grant session-wide access.
func claudePendingNativeUIPrompt(sessionID string, projectsDir string) map[string]any {
	path := claudeTranscriptPath(sessionID, projectsDir)
	if path == "" {
		return nil
	}
	pending := map[string]map[string]any{}
	order := []string{}
	for _, rec := range jsonlTailRecords(path, jsonlTailScanBytes) {
		content := mapAny(rec["message"])["content"]
		switch stringAny(rec["type"]) {
		case "assistant":
			for _, raw := range listAny(content) {
				block := mapAny(raw)
				name := stringAny(block["name"])
				if stringAny(block["type"]) != "tool_use" || !claudeNativeUIPromptTool(name) {
					continue
				}
				id := firstNonEmpty(stringAny(block["id"]), stringAny(rec["uuid"]))
				if id == "" {
					continue
				}
				if pending[id] == nil {
					order = append(order, id)
				}
				pending[id] = map[string]any{
					"type": "native_ui", "request_id": id, "tool_use_id": id, "tool_name": name,
					"summary": "Claude Desktop is waiting for a local access confirmation",
					"details": "Open Claude Desktop to allow or deny this native permission request.",
					"source":  "claude_transcript_native_ui", "actionable": false,
					"timestamp": stringAny(rec["timestamp"]),
				}
			}
		case "user":
			for _, raw := range listAny(content) {
				block := mapAny(raw)
				if stringAny(block["type"]) == "tool_result" {
					delete(pending, stringAny(block["tool_use_id"]))
				}
			}
		case "result":
			pending = map[string]map[string]any{}
		}
	}
	for i := len(order) - 1; i >= 0; i-- {
		if request := pending[order[i]]; request != nil {
			return request
		}
	}
	return nil
}

func claudeNativeUIPromptTool(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, suffix := range []string{"request_access", "request_permission", "ask_permission", "request_authorization"} {
		if name == suffix || strings.HasSuffix(name, "__"+suffix) || strings.HasSuffix(name, "/"+suffix) {
			return true
		}
	}
	return false
}

func claudeAskUserQuestionRequest(input any, toolUseID string, ts string) map[string]any {
	m := mapAny(input)
	questionsRaw := listAny(m["questions"])
	if len(questionsRaw) == 0 {
		return nil
	}
	questions := make([]map[string]any, 0, len(questionsRaw))
	summary := ""
	for _, raw := range questionsRaw {
		qm := mapAny(raw)
		if len(qm) == 0 {
			continue
		}
		optsRaw := listAny(qm["options"])
		options := make([]map[string]any, 0, len(optsRaw))
		for _, optRaw := range optsRaw {
			om := mapAny(optRaw)
			if len(om) == 0 {
				continue
			}
			options = append(options, map[string]any{
				"label":       stringAny(om["label"]),
				"description": stringAny(om["description"]),
			})
		}
		question := map[string]any{
			"header":      stringAny(qm["header"]),
			"question":    stringAny(qm["question"]),
			"multiSelect": boolAny(qm["multiSelect"]),
			"options":     options,
		}
		if summary == "" {
			summary = firstNonEmpty(stringAny(question["question"]), stringAny(question["header"]))
		}
		questions = append(questions, question)
	}
	if len(questions) == 0 {
		return nil
	}
	return map[string]any{
		"type":        "question",
		"summary":     firstNonEmpty(summary, "Claude has a question"),
		"question":    summary,
		"header":      stringAny(questions[0]["header"]),
		"questions":   questions,
		"tool_use_id": toolUseID,
		"ts":          ts,
	}
}

func claudeSessionModel(sessionID string, projectsDir string) map[string]any {
	path := claudeTranscriptPath(sessionID, projectsDir)
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var model, speed, serviceTier string
	contextTokens, outputTokens := 0, 0
	usageByModel := map[string]*modelTokenUsage{}
	usageOrder := []string{}
	seenUsage := map[string]bool{}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		var rec map[string]any
		if err := json.Unmarshal([]byte(scanner.Text()), &rec); err != nil {
			continue
		}
		msg := mapAny(rec["message"])
		if m := stringAny(msg["model"]); m != "" && m != "<synthetic>" {
			model = m
		}
		usage := mapAny(msg["usage"])
		if len(usage) == 0 {
			continue
		}
		usageID := firstNonEmpty(stringAny(msg["id"]), firstNonEmpty(stringAny(rec["requestId"]), stringAny(rec["uuid"])))
		if usageID != "" && seenUsage[usageID] {
			continue
		}
		seenUsage[usageID] = true
		if s := stringAny(usage["speed"]); s != "" {
			speed = s
		}
		if st := stringAny(usage["service_tier"]); st != "" {
			serviceTier = st
		}
		outputTokens += intAny(usage["output_tokens"])
		ctx := intAny(usage["input_tokens"]) + intAny(usage["cache_read_input_tokens"]) + intAny(usage["cache_creation_input_tokens"])
		if ctx != 0 {
			contextTokens = ctx
		}
		usageModel := model
		if usageModel == "" {
			usageModel = "unknown"
		}
		bucket := usageByModel[usageModel]
		if bucket == nil {
			bucket = &modelTokenUsage{Model: usageModel}
			usageByModel[usageModel] = bucket
			usageOrder = append(usageOrder, usageModel)
		}
		bucket.Input += int64(intAny(usage["input_tokens"]))
		bucket.Output += int64(intAny(usage["output_tokens"]))
		bucket.CacheCreate += int64(intAny(usage["cache_creation_input_tokens"]))
		bucket.CacheRead += int64(intAny(usage["cache_read_input_tokens"]))
		if variant := stringAny(usage["speed"]); variant != "" {
			bucket.Variant = variant
		}
		if tier := stringAny(usage["service_tier"]); tier != "" {
			bucket.ServiceTier = tier
		}
		if geo := stringAny(usage["inference_geo"]); geo != "" {
			bucket.InferenceGeo = geo
		}
	}
	return map[string]any{
		"model": model, "speed": speed, "service_tier": serviceTier,
		"context_tokens": contextTokens, "output_tokens": outputTokens,
		"usage": tokenUsageRows(usageOrder, usageByModel),
	}
}

type modelTokenUsage struct {
	Model        string
	Input        int64
	Output       int64
	CacheCreate  int64
	CacheRead    int64
	Total        int64
	Variant      string
	ServiceTier  string
	InferenceGeo string
}

type turnUsageState struct {
	start   time.Time
	last    time.Time
	order   []string
	buckets map[string]*modelTokenUsage
}

func newTurnUsage(timestamp string) *turnUsageState {
	parsed, _ := time.Parse(time.RFC3339Nano, timestamp)
	return &turnUsageState{start: parsed, last: parsed, buckets: map[string]*modelTokenUsage{}}
}

func (t *turnUsageState) observe(timestamp string) {
	if t == nil {
		return
	}
	parsed, err := time.Parse(time.RFC3339Nano, timestamp)
	if err == nil && parsed.After(t.last) {
		t.last = parsed
	}
}

func (t *turnUsageState) add(model string, usage map[string]any) {
	if t == nil || len(usage) == 0 {
		return
	}
	model = firstNonEmpty(model, "unknown")
	bucket := t.buckets[model]
	if bucket == nil {
		bucket = &modelTokenUsage{Model: model}
		t.buckets[model] = bucket
		t.order = append(t.order, model)
	}
	bucket.Input += int64(intAny(usage["input_tokens"]))
	bucket.Output += int64(intAny(usage["output_tokens"]))
	bucket.CacheCreate += int64(intAny(usage["cache_creation_input_tokens"]))
	bucket.CacheRead += int64(intAny(usage["cache_read_input_tokens"]))
	bucket.Total += int64(intAny(usage["total_tokens"]))
	if variant := stringAny(usage["speed"]); variant != "" {
		bucket.Variant = variant
	}
	if tier := stringAny(usage["service_tier"]); tier != "" {
		bucket.ServiceTier = tier
	}
	if geo := stringAny(usage["inference_geo"]); geo != "" {
		bucket.InferenceGeo = geo
	}
}

func (t *turnUsageState) addDelta(model string, delta modelTokenUsage) {
	if t == nil {
		return
	}
	model = firstNonEmpty(model, "unknown")
	bucket := t.buckets[model]
	if bucket == nil {
		bucket = &modelTokenUsage{Model: model}
		t.buckets[model] = bucket
		t.order = append(t.order, model)
	}
	bucket.Input += delta.Input
	bucket.Output += delta.Output
	bucket.CacheCreate += delta.CacheCreate
	bucket.CacheRead += delta.CacheRead
	bucket.Total += delta.Total
}

func turnUsageItem(turn *turnUsageState, durationMS int64, turnID string) map[string]any {
	if turn == nil || len(turn.order) == 0 {
		return nil
	}
	rows := tokenUsageRows(turn.order, turn.buckets)
	if len(rows) == 0 {
		return nil
	}
	usage := map[string]any{"models": rows}
	var input, output, cacheCreate, cacheRead, total int64
	for _, row := range rows {
		input += int64(intAny(row["input_tokens"]))
		output += int64(intAny(row["output_tokens"]))
		cacheCreate += int64(intAny(row["cache_creation_input_tokens"]))
		cacheRead += int64(intAny(row["cache_read_input_tokens"]))
		total += int64(intAny(row["total_tokens"]))
	}
	usage["input_tokens"] = input
	usage["output_tokens"] = output
	usage["cache_creation_input_tokens"] = cacheCreate
	usage["cache_read_input_tokens"] = cacheRead
	usage["total_tokens"] = total
	if len(rows) == 1 {
		usage["model"] = rows[0]["model"]
	}
	if durationMS <= 0 && !turn.start.IsZero() && !turn.last.IsZero() && !turn.last.Before(turn.start) {
		durationMS = turn.last.Sub(turn.start).Milliseconds()
	}
	usage["duration_ms"] = durationMS
	item := map[string]any{"role": "assistant", "kind": "turn_usage", "usage": usage}
	if !turn.last.IsZero() {
		item["ts"] = turn.last.Format(time.RFC3339Nano)
	}
	if turnID != "" {
		item["turn_id"] = turnID
	}
	return item
}

func claudeHumanTurnStart(rec map[string]any) bool {
	if stringAny(rec["type"]) != "user" {
		return false
	}
	content := mapAny(rec["message"])["content"]
	if text, ok := content.(string); ok {
		text = strings.TrimLeft(text, " \t\r\n")
		return text != "" && !strings.HasPrefix(text, "<")
	}
	for _, raw := range listAny(content) {
		block := mapAny(raw)
		switch stringAny(block["type"]) {
		case "image":
			return true
		case "text":
			text := strings.TrimLeft(stringAny(block["text"]), " \t\r\n")
			if text != "" && !strings.HasPrefix(text, "<") {
				return true
			}
		}
	}
	return false
}

func tokenUsageRows(order []string, buckets map[string]*modelTokenUsage) []map[string]any {
	rows := make([]map[string]any, 0, len(order))
	for _, model := range order {
		usage := buckets[model]
		if usage == nil {
			continue
		}
		total := usage.Total
		if total == 0 {
			total = usage.Input + usage.Output + usage.CacheCreate + usage.CacheRead
		}
		rows = append(rows, map[string]any{
			"model": model, "input_tokens": usage.Input, "output_tokens": usage.Output,
			"cache_creation_input_tokens": usage.CacheCreate, "cache_read_input_tokens": usage.CacheRead,
			"total_tokens": total, "pricing_variant": usage.Variant,
			"service_tier": usage.ServiceTier, "inference_geo": usage.InferenceGeo,
		})
	}
	return rows
}

func claudeRecordToItems(rec map[string]any, curCwd string, pending map[string]map[string]any) []map[string]any {
	role := stringAny(rec["type"])
	if role != "user" && role != "assistant" {
		return nil
	}
	ts := stringAny(rec["timestamp"])
	content := mapAny(rec["message"])["content"]
	if role == "user" {
		if text, ok := content.(string); ok {
			if text != "" && !strings.HasPrefix(strings.TrimLeft(text, " \t\r\n"), "<") {
				return []map[string]any{{"role": "user", "kind": "text", "text": text, "ts": ts}}
			}
			return nil
		}
		out := []map[string]any{}
		for _, b := range listAny(content) {
			block := mapAny(b)
			switch stringAny(block["type"]) {
			case "text":
				if text := stringAny(block["text"]); text != "" {
					if !strings.HasPrefix(strings.TrimLeft(text, " \t\r\n"), "<") {
						out = append(out, map[string]any{"role": "user", "kind": "text", "text": text, "ts": ts})
					}
				}
			case "image":
				if item := claudeImageItem(block, "user", ts); item != nil {
					out = append(out, item)
				}
			case "tool_result":
				if item := pending[stringAny(block["tool_use_id"])]; item != nil {
					item["result"] = resultText(block["content"], 2500)
					item["is_error"] = boolAny(block["is_error"])
				}
				for _, imageBlock := range claudeNestedToolResultImages(block) {
					if item := claudeImageItem(imageBlock, "assistant", ts); item != nil {
						item["tool_use_id"] = stringAny(block["tool_use_id"])
						out = append(out, item)
					}
				}
			}
		}
		return out
	}

	blocks := listAny(content)
	if text, ok := content.(string); ok {
		blocks = []any{map[string]any{"type": "text", "text": text}}
	}
	out := []map[string]any{}
	for _, raw := range blocks {
		block := mapAny(raw)
		switch stringAny(block["type"]) {
		case "text":
			if text := stringAny(block["text"]); text != "" {
				out = append(out, map[string]any{"role": "assistant", "kind": "text", "text": text, "ts": ts})
			}
		case "thinking":
			if text := stringAny(block["thinking"]); text != "" {
				out = append(out, map[string]any{"role": "assistant", "kind": "thinking", "text": text, "ts": ts})
			}
		case "image", "output_image":
			if item := claudeImageItem(block, "assistant", ts); item != nil {
				out = append(out, item)
			}
		case "tool_use":
			item := map[string]any{
				"role": "assistant", "kind": "tool", "name": firstNonEmpty(stringAny(block["name"]), "tool"),
				"text":   toolDetail(block["input"], 160),
				"io":     toolIO(block["name"], block["input"]),
				"files":  extractPaths(block["name"], block["input"], curCwd),
				"result": nil, "ts": ts,
			}
			out = append(out, item)
			if id := stringAny(block["id"]); id != "" {
				pending[id] = item
			}
		}
	}
	return out
}

func transcriptAssetID(source string) string {
	sum := sha256.Sum256([]byte(source))
	return fmt.Sprintf("%x", sum[:16])
}

func claudeImageItem(block map[string]any, role string, ts string) map[string]any {
	source := mapAny(block["source"])
	if stringAny(source["type"]) != "base64" {
		return nil
	}
	data := stringAny(source["data"])
	mime := stringAny(source["media_type"])
	if data == "" || !strings.HasPrefix(mime, "image/") {
		return nil
	}
	return map[string]any{
		"role": role, "kind": "image", "asset_id": transcriptAssetID("claude:" + mime + ":" + data),
		"mime": mime, "ts": ts,
	}
}

func claudeNestedToolResultImages(block map[string]any) []map[string]any {
	if stringAny(block["type"]) != "tool_result" {
		return nil
	}
	out := []map[string]any{}
	for _, raw := range listAny(block["content"]) {
		nested := mapAny(raw)
		if typ := stringAny(nested["type"]); typ == "image" || typ == "output_image" {
			out = append(out, nested)
		}
	}
	return out
}

func claudeMessageImageBlocks(content any) []map[string]any {
	out := []map[string]any{}
	for _, raw := range listAny(content) {
		block := mapAny(raw)
		switch stringAny(block["type"]) {
		case "image", "output_image":
			out = append(out, block)
		case "tool_result":
			out = append(out, claudeNestedToolResultImages(block)...)
		}
	}
	return out
}

func claudeTranscriptAsset(sessionID string, projectsDir string, assetID string) (SessionAsset, bool, error) {
	path := claudeTranscriptPath(sessionID, projectsDir)
	if path == "" {
		return SessionAsset{}, false, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return SessionAsset{}, false, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 32*1024*1024)
	for scanner.Scan() {
		var rec map[string]any
		if json.Unmarshal(scanner.Bytes(), &rec) != nil {
			continue
		}
		for _, block := range claudeMessageImageBlocks(mapAny(rec["message"])["content"]) {
			source := mapAny(block["source"])
			data, mime := stringAny(source["data"]), stringAny(source["media_type"])
			if data == "" || transcriptAssetID("claude:"+mime+":"+data) != assetID {
				continue
			}
			decoded, err := base64.StdEncoding.DecodeString(data)
			if err != nil {
				return SessionAsset{}, false, errors.New("invalid transcript image")
			}
			if len(decoded) > 25*1024*1024 {
				return SessionAsset{}, false, errors.New("transcript image is too large")
			}
			return SessionAsset{MediaType: mime, Data: decoded}, true, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return SessionAsset{}, false, err
	}
	return SessionAsset{}, false, nil
}

func toolDetail(input any, limit int) string {
	if s, ok := input.(string); ok {
		return shortText(s, limit)
	}
	m := mapAny(input)
	for _, k := range []string{"command", "file_path", "path", "pattern", "url", "query", "prompt", "cmd", "old_string", "description"} {
		if v, ok := m[k]; ok && v != nil {
			if list := listAny(v); len(list) > 0 {
				parts := make([]string, 0, len(list))
				for _, x := range list {
					parts = append(parts, stringAny(x))
				}
				return shortText(strings.Join(parts, " "), limit)
			}
			if s := stringAny(v); s != "" {
				return shortText(s, limit)
			}
		}
	}
	for _, v := range m {
		if s := stringAny(v); s != "" {
			return shortText(s, limit)
		}
	}
	return ""
}

func toolIO(name any, input any) map[string]any {
	m := mapAny(input)
	if len(m) == 0 {
		if s := stringAny(input); s != "" {
			_ = json.Unmarshal([]byte(s), &m)
		}
	}
	if len(m) == 0 {
		return nil
	}
	file := firstNonEmpty(stringAny(m["file_path"]), stringAny(m["path"]))
	if m["old_string"] != nil && m["new_string"] != nil {
		return map[string]any{"kind": "edit", "file": nullableNonEmpty(file),
			"edits": []map[string]any{{"old": truncate(stringAny(m["old_string"]), 6000), "new": truncate(stringAny(m["new_string"]), 6000)}}}
	}
	if edits := listAny(m["edits"]); len(edits) > 0 {
		rows := []map[string]any{}
		for _, raw := range edits {
			edit := mapAny(raw)
			if len(edit) == 0 {
				continue
			}
			rows = append(rows, map[string]any{"old": truncate(stringAny(edit["old_string"]), 6000), "new": truncate(stringAny(edit["new_string"]), 6000)})
		}
		if len(rows) > 0 {
			return map[string]any{"kind": "edit", "file": nullableNonEmpty(file), "edits": rows}
		}
	}
	n := strings.ToLower(stringAny(name))
	if n == "write" || (m["content"] != nil && file != "") {
		return map[string]any{"kind": "edit", "file": nullableNonEmpty(file),
			"edits": []map[string]any{{"old": "", "new": truncate(stringAny(m["content"]), 6000)}}}
	}
	cmd := commandString(firstValue(m["command"], m["cmd"]))
	if cmd != "" {
		return map[string]any{"kind": "command", "command": truncate(cmd, 6000), "description": nullableString(m["description"])}
	}
	return nil
}

func resultText(content any, limit int) string {
	if s, ok := content.(string); ok {
		return truncate(s, limit)
	}
	parts := []string{}
	for _, raw := range listAny(content) {
		if s, ok := raw.(string); ok {
			parts = append(parts, s)
			continue
		}
		m := mapAny(raw)
		if text := firstNonEmpty(stringAny(m["text"]), stringAny(m["content"])); text != "" {
			parts = append(parts, text)
		}
	}
	return truncate(strings.Join(parts, "\n"), limit)
}

func extractPaths(toolName any, toolInput any, cwd string) []string {
	m := mapAny(toolInput)
	if len(m) == 0 {
		if s := stringAny(toolInput); s != "" {
			_ = json.Unmarshal([]byte(s), &m)
		}
	}
	if len(m) == 0 {
		return nil
	}
	name := strings.ToLower(stringAny(toolName))
	out := []string{}
	seen := map[string]bool{}
	add := func(p string) {
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		out = append(out, p)
	}
	resolve := func(p string) string {
		p = strings.TrimSpace(expandUser(p))
		if p == "" {
			return ""
		}
		if !filepath.IsAbs(p) {
			if cwd == "" {
				return ""
			}
			p = filepath.Join(expandUser(cwd), p)
		}
		if rp, err := filepath.EvalSymlinks(p); err == nil {
			return rp
		}
		if abs, err := filepath.Abs(p); err == nil {
			return filepath.Clean(abs)
		}
		return ""
	}
	if name != "glob" {
		for _, k := range []string{"file_path", "path", "notebook_path"} {
			v := stringAny(m[k])
			if v != "" && !strings.ContainsAny(v, "*?") {
				add(resolve(v))
			}
		}
	}
	cmd := commandString(firstValue(m["command"], m["cmd"]))
	if cmd != "" {
		for _, tok := range commandTokens(cmd) {
			t := strings.Trim(tok, "'\"`;,()<>")
			candidates := []string{t}
			if strings.Contains(t, "=") && !strings.HasPrefix(t, "-") {
				parts := strings.SplitN(t, "=", 2)
				candidates = append(candidates, parts[1])
			}
			for _, c := range candidates {
				if c == "" || strings.HasPrefix(c, "-") || strings.ContainsAny(c, "*?") {
					continue
				}
				if !strings.Contains(c, "/") && !pathishExtRE.MatchString(c) {
					continue
				}
				rp := resolve(c)
				if rp == "" {
					continue
				}
				if st, err := os.Stat(rp); err == nil && st.Mode().IsRegular() {
					add(rp)
				}
			}
		}
	}
	return out
}

func commandTokens(cmd string) []string {
	var tokens []string
	var b strings.Builder
	quote := rune(0)
	escape := false
	flush := func() {
		if b.Len() > 0 {
			tokens = append(tokens, b.String())
			b.Reset()
		}
	}
	for _, r := range cmd {
		if escape {
			b.WriteRune(r)
			escape = false
			continue
		}
		if r == '\\' {
			escape = true
			continue
		}
		if quote != 0 {
			if r == quote {
				quote = 0
			} else {
				b.WriteRune(r)
			}
			continue
		}
		if r == '\'' || r == '"' {
			quote = r
			continue
		}
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			flush()
			continue
		}
		b.WriteRune(r)
	}
	flush()
	return tokens
}

func commandString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if list := listAny(v); len(list) > 0 {
		parts := make([]string, 0, len(list))
		for _, x := range list {
			parts = append(parts, stringAny(x))
		}
		return strings.Join(parts, " ")
	}
	return ""
}

func referencedFilesFromMessages(messages []map[string]any) map[string]bool {
	out := map[string]bool{}
	for _, msg := range messages {
		for _, raw := range listAny(msg["files"]) {
			if p := stringAny(raw); p != "" {
				out[p] = true
			}
		}
	}
	return out
}

func codexSessions(indexPath string, sessionsDirs []string, limit int) []map[string]any {
	indexPath = expandUser(firstNonEmpty(indexPath, codexIndexDefaultPath))
	rollouts := codexRolloutPaths(sessionsDirs)
	byID := map[string]map[string]any{}
	if f, err := os.Open(indexPath); err == nil {
		defer f.Close()
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			var rec map[string]any
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				continue
			}
			id := stringAny(rec["id"])
			if id == "" {
				continue
			}
			row := byID[id]
			if row == nil {
				row = codexNativeSessionRow(id)
				byID[id] = row
			}
			if title := stringAny(rec["thread_name"]); title != "" {
				row["title"] = title
			}
			if cwd := stringAny(rec["cwd"]); cwd != "" {
				row["cwd"] = cwd
			}
			if created := stringAny(rec["created_at"]); created != "" {
				row["created_at"] = created
			}
			if updated := stringAny(rec["updated_at"]); updated != "" {
				row["updated_at"] = updated
			}
		}
	}
	for id, path := range rollouts {
		row := byID[id]
		if row == nil {
			row = codexNativeSessionRow(id)
			byID[id] = row
		}
		if mt := fileMTime(path); !mt.IsZero() {
			row["updated_at"] = epochToISO(float64(mt.UnixNano()) / 1e9)
		}
		if stringAny(row["cwd"]) == "" {
			row["cwd"] = nullableNonEmpty(codexRolloutCwd(path))
		}
		row["last_reply_at"] = nullableNonEmpty(codexRolloutLastReplyAt(path))
	}
	out := make([]map[string]any, 0, len(byID))
	for _, row := range byID {
		out = append(out, row)
	}
	sortByUpdated(out)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

func codexNativeSessionRow(id string) map[string]any {
	return map[string]any{
		"native_session_id": id,
		"cli_session_id":    id,
		"title":             "session " + shortText(id, 8),
		"cwd":               nil,
		"branch":            nil,
		"worktree":          nil,
		"model":             nil,
		"completed_turns":   nil,
		"archived":          false,
		"created_at":        "",
		"updated_at":        "",
		"last_reply_at":     nil,
		"source":            "codex",
	}
}

func codexSessionMessages(sessionID string, sessionsDirs []string, maxItems int) []map[string]any {
	path := codexFindRollout(sessionID, sessionsDirs)
	if path == "" {
		return nil
	}
	cwd := codexRolloutCwd(path)
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	out := []map[string]any{}
	pending := map[string]map[string]any{}
	model := ""
	turnID := ""
	var turn *turnUsageState
	var latest, base modelTokenUsage
	haveLatest, haveBase := false, false
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		payload := mapAny(rec["payload"])
		recordType, payloadType := stringAny(rec["type"]), stringAny(payload["type"])
		if recordType == "turn_context" {
			model = firstNonEmpty(stringAny(payload["model"]), model)
			turnID = stringAny(payload["turn_id"])
			turn = newTurnUsage(stringAny(rec["timestamp"]))
			base, haveBase = latest, haveLatest
		}
		if recordType == "event_msg" && payloadType == "task_started" {
			if turn == nil {
				turn = newTurnUsage(firstNonEmpty(stringAny(payload["started_at"]), stringAny(rec["timestamp"])))
			}
			turnID = firstNonEmpty(stringAny(payload["turn_id"]), turnID)
			if !haveBase {
				base, haveBase = latest, haveLatest
			}
		}
		if recordType == "event_msg" && payloadType == "token_count" {
			total := mapAny(mapAny(payload["info"])["total_token_usage"])
			if len(total) > 0 {
				latest = modelTokenUsage{
					Input: int64(intAny(total["input_tokens"])), Output: int64(intAny(total["output_tokens"])),
					CacheRead: int64(intAny(total["cached_input_tokens"])), Total: int64(intAny(total["total_tokens"])),
				}
				haveLatest = true
			}
		}
		out = append(out, codexRecordToItems(rec, cwd, pending)...)
		if recordType == "event_msg" && (payloadType == "task_complete" || payloadType == "turn_aborted") {
			if turn == nil {
				turn = newTurnUsage(stringAny(rec["timestamp"]))
			}
			turn.observe(firstNonEmpty(stringAny(payload["completed_at"]), stringAny(rec["timestamp"])))
			if haveLatest {
				delta := latest
				if haveBase {
					delta.Input = cumulativeDelta(latest.Input, base.Input)
					delta.Output = cumulativeDelta(latest.Output, base.Output)
					delta.CacheRead = cumulativeDelta(latest.CacheRead, base.CacheRead)
					delta.Total = cumulativeDelta(latest.Total, base.Total)
				}
				// Codex reports cached input as a subset of input_tokens.
				delta.Input -= delta.CacheRead
				if delta.Input < 0 {
					delta.Input = 0
				}
				if delta.Input != 0 || delta.Output != 0 || delta.CacheRead != 0 || delta.Total != 0 {
					turn.addDelta(model, delta)
				}
			}
			if item := turnUsageItem(turn, int64(intAny(payload["duration_ms"])), turnID); item != nil {
				out = append(out, item)
			}
			turn, turnID, haveBase = nil, "", false
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

func codexSessionUsage(sessionID string, sessionsDirs []string) []map[string]any {
	path := codexFindRollout(sessionID, sessionsDirs)
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	model := ""
	order := []string{}
	buckets := map[string]*modelTokenUsage{}
	var previous modelTokenUsage
	havePrevious := false
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		var rec map[string]any
		if err := json.Unmarshal([]byte(scanner.Text()), &rec); err != nil {
			continue
		}
		payload := mapAny(rec["payload"])
		if stringAny(rec["type"]) == "turn_context" {
			if next := stringAny(payload["model"]); next != "" {
				model = next
			}
			continue
		}
		if stringAny(rec["type"]) != "event_msg" || stringAny(payload["type"]) != "token_count" {
			continue
		}
		total := mapAny(mapAny(payload["info"])["total_token_usage"])
		if len(total) == 0 {
			continue
		}
		current := modelTokenUsage{
			Input: int64(intAny(total["input_tokens"])), Output: int64(intAny(total["output_tokens"])),
			CacheRead: int64(intAny(total["cached_input_tokens"])), Total: int64(intAny(total["total_tokens"])),
		}
		delta := current
		if havePrevious {
			delta.Input = cumulativeDelta(current.Input, previous.Input)
			delta.Output = cumulativeDelta(current.Output, previous.Output)
			delta.CacheRead = cumulativeDelta(current.CacheRead, previous.CacheRead)
			delta.Total = cumulativeDelta(current.Total, previous.Total)
		}
		previous, havePrevious = current, true
		if delta.Input == 0 && delta.Output == 0 && delta.CacheRead == 0 && delta.Total == 0 {
			continue
		}
		usageModel := model
		if usageModel == "" {
			usageModel = "unknown"
		}
		bucket := buckets[usageModel]
		if bucket == nil {
			bucket = &modelTokenUsage{Model: usageModel}
			buckets[usageModel] = bucket
			order = append(order, usageModel)
		}
		// Codex reports cached input as a subset of input_tokens. Present Input
		// as the non-cached portion so the visible columns add up to Total.
		nonCached := delta.Input - delta.CacheRead
		if nonCached < 0 {
			nonCached = 0
		}
		bucket.Input += nonCached
		bucket.Output += delta.Output
		bucket.CacheRead += delta.CacheRead
		bucket.Total += delta.Total
	}
	return tokenUsageRows(order, buckets)
}

func cumulativeDelta(current, previous int64) int64 {
	if current >= previous {
		return current - previous
	}
	// A restarted/compacted rollout can reset cumulative counters.
	return current
}

func codexFindRollout(sessionID string, sessionsDirs []string) string {
	for _, base := range codexSessionDirs(sessionsDirs) {
		if st, err := os.Stat(base); err != nil || !st.IsDir() {
			continue
		}
		var hit string
		_ = filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || hit != "" {
				return nil
			}
			if strings.HasPrefix(d.Name(), "rollout-") && strings.HasSuffix(d.Name(), "-"+sessionID+".jsonl") {
				hit = path
			}
			return nil
		})
		if hit != "" {
			return hit
		}
	}
	return ""
}

func codexRolloutPaths(sessionsDirs []string) map[string]string {
	out := map[string]string{}
	mtimes := map[string]time.Time{}
	for _, base := range codexSessionDirs(sessionsDirs) {
		if st, err := os.Stat(base); err != nil || !st.IsDir() {
			continue
		}
		_ = filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasPrefix(d.Name(), "rollout-") {
				return nil
			}
			m := rolloutIDRE.FindStringSubmatch(d.Name())
			if m == nil {
				return nil
			}
			mt := fileMTime(path)
			if mt.After(mtimes[m[1]]) {
				mtimes[m[1]] = mt
				out[m[1]] = path
			}
			return nil
		})
	}
	return out
}

func codexRolloutLastReplyAt(path string) string {
	if path == "" {
		return ""
	}
	return jsonlTailTimestamp(path, jsonlTailScanBytes, func(rec map[string]any) bool {
		if stringAny(rec["timestamp"]) == "" {
			return false
		}
		rt := stringAny(rec["type"])
		pl := mapAny(rec["payload"])
		pt := stringAny(pl["type"])
		if rt == "response_item" && pt == "message" {
			return stringAny(pl["role"]) == "assistant"
		}
		if rt == "response_item" {
			return pt == "function_call" || pt == "function_call_output" || pt == "reasoning"
		}
		return rt == "event_msg" && pt == "agent_reasoning"
	})
}

func codexSessionDirs(sessionsDirs []string) []string {
	if len(sessionsDirs) == 0 {
		return []string{expandUser("~/.codex/sessions"), expandUser("~/.codex/archived_sessions")}
	}
	out := make([]string, 0, len(sessionsDirs))
	for _, d := range sessionsDirs {
		if d != "" {
			out = append(out, expandUser(d))
		}
	}
	return out
}

func codexRolloutCwd(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for i := 0; scanner.Scan() && i <= 30; i++ {
		var rec map[string]any
		if err := json.Unmarshal([]byte(scanner.Text()), &rec); err != nil {
			continue
		}
		if cwd := stringAny(rec["cwd"]); cwd != "" {
			return cwd
		}
		if cwd := stringAny(mapAny(rec["payload"])["cwd"]); cwd != "" {
			return cwd
		}
	}
	return ""
}

func codexRecordToItems(rec map[string]any, cwd string, pending map[string]map[string]any) []map[string]any {
	rt := stringAny(rec["type"])
	pl := mapAny(rec["payload"])
	pt := stringAny(pl["type"])
	ts := stringAny(rec["timestamp"])
	switch {
	case rt == "response_item" && pt == "message":
		role := stringAny(pl["role"])
		if role != "user" && role != "assistant" {
			return nil
		}
		out := []map[string]any{}
		for _, raw := range listAny(pl["content"]) {
			block := mapAny(raw)
			typ := stringAny(block["type"])
			if typ == "input_text" || typ == "output_text" || typ == "text" {
				text := stripInternalTags(stringAny(block["text"]))
				if text != "" && !strings.HasPrefix(strings.TrimLeft(text, " \t\r\n"), "<") {
					out = append(out, map[string]any{"role": role, "kind": "text", "text": text, "ts": ts})
				}
				continue
			}
			if item := codexImageItem(block, role, ts); item != nil {
				out = append(out, item)
			}
		}
		return out
	case rt == "response_item" && pt == "function_call":
		item := map[string]any{
			"role": "assistant", "kind": "tool", "name": firstNonEmpty(stringAny(pl["name"]), "tool"),
			"text": toolDetail(pl["arguments"], 160), "io": toolIO(pl["name"], pl["arguments"]),
			"files": extractPaths(pl["name"], pl["arguments"], cwd), "result": nil, "ts": ts,
		}
		if cid := stringAny(pl["call_id"]); cid != "" {
			pending[cid] = item
		}
		return []map[string]any{item}
	case rt == "response_item" && pt == "function_call_output":
		if item := pending[stringAny(pl["call_id"])]; item != nil {
			item["result"] = codexOutputText(pl["output"], 2500)
		}
	case rt == "event_msg" && pt == "agent_reasoning" && stringAny(pl["text"]) != "":
		return []map[string]any{{"role": "assistant", "kind": "thinking", "text": stringAny(pl["text"]), "ts": ts}}
	}
	return nil
}

func codexImageSource(block map[string]any) (string, string) {
	typ := stringAny(block["type"])
	if typ != "input_image" && typ != "output_image" && typ != "image" && typ != "local_image" && typ != "localImage" {
		return "", ""
	}
	source := firstNonEmpty(stringAny(block["image_url"]), stringAny(block["url"]))
	if source == "" {
		source = stringAny(block["path"])
	}
	if strings.HasPrefix(source, "data:image/") || filepath.IsAbs(source) || strings.HasPrefix(source, "file://") {
		return typ, source
	}
	return "", ""
}

func codexImageItem(block map[string]any, role string, ts string) map[string]any {
	_, source := codexImageSource(block)
	if source == "" {
		return nil
	}
	mime := ""
	if strings.HasPrefix(source, "data:") {
		if semi := strings.IndexByte(source, ';'); semi > len("data:") {
			mime = source[len("data:"):semi]
		}
	} else {
		mime = imageMediaType(source)
	}
	return map[string]any{
		"role": role, "kind": "image", "asset_id": transcriptAssetID("codex:" + source),
		"mime": mime, "ts": ts,
	}
}

func imageMediaType(path string) string {
	ext := strings.ToLower(filepath.Ext(strings.TrimPrefix(path, "file://")))
	switch ext {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	}
	return "application/octet-stream"
}

func decodeCodexImageSource(source string) (SessionAsset, error) {
	if strings.HasPrefix(source, "data:") {
		comma := strings.IndexByte(source, ',')
		if comma < 0 || !strings.Contains(source[:comma], ";base64") {
			return SessionAsset{}, errors.New("unsupported transcript image data URL")
		}
		mime := strings.TrimPrefix(strings.SplitN(source[:comma], ";", 2)[0], "data:")
		data, err := base64.StdEncoding.DecodeString(source[comma+1:])
		if err != nil {
			return SessionAsset{}, errors.New("invalid transcript image")
		}
		if len(data) > 25*1024*1024 {
			return SessionAsset{}, errors.New("transcript image is too large")
		}
		return SessionAsset{MediaType: mime, Data: data}, nil
	}
	path := strings.TrimPrefix(source, "file://")
	st, err := os.Stat(path)
	if err != nil || st.IsDir() {
		return SessionAsset{}, errors.New("transcript image file is unavailable")
	}
	if st.Size() > 25*1024*1024 {
		return SessionAsset{}, errors.New("transcript image is too large")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return SessionAsset{}, err
	}
	mime := imageMediaType(path)
	if mime == "application/octet-stream" {
		mime = http.DetectContentType(data)
	}
	if !strings.HasPrefix(mime, "image/") {
		return SessionAsset{}, errors.New("transcript asset is not an image")
	}
	return SessionAsset{MediaType: mime, Data: data}, nil
}

func codexTranscriptAsset(sessionID string, sessionsDirs []string, assetID string) (SessionAsset, bool, error) {
	path := codexFindRollout(sessionID, sessionsDirs)
	if path == "" {
		return SessionAsset{}, false, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return SessionAsset{}, false, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 32*1024*1024)
	for scanner.Scan() {
		var rec map[string]any
		if json.Unmarshal(scanner.Bytes(), &rec) != nil {
			continue
		}
		pl := mapAny(rec["payload"])
		if stringAny(rec["type"]) != "response_item" || stringAny(pl["type"]) != "message" {
			continue
		}
		for _, raw := range listAny(pl["content"]) {
			_, source := codexImageSource(mapAny(raw))
			if source == "" || transcriptAssetID("codex:"+source) != assetID {
				continue
			}
			asset, err := decodeCodexImageSource(source)
			return asset, err == nil, err
		}
	}
	if err := scanner.Err(); err != nil {
		return SessionAsset{}, false, err
	}
	return SessionAsset{}, false, nil
}

func stripInternalTags(text string) string {
	if text == "" {
		return text
	}
	if strings.Contains(text, "<oai-") {
		text = oaiInternalRE.ReplaceAllString(text, "")
	}
	if strings.Contains(text, "::") {
		text = directiveRE.ReplaceAllString(text, "")
	}
	return strings.TrimSpace(blankRE.ReplaceAllString(text, "\n\n"))
}

func blocksText(content any, textTypes map[string]bool) string {
	if s, ok := content.(string); ok {
		return s
	}
	parts := []string{}
	for _, raw := range listAny(content) {
		if s, ok := raw.(string); ok {
			parts = append(parts, s)
			continue
		}
		block := mapAny(raw)
		if textTypes[stringAny(block["type"])] {
			if text := stringAny(block["text"]); text != "" {
				parts = append(parts, text)
			}
		}
	}
	return strings.Join(parts, "\n")
}

func codexOutputText(output any, limit int) string {
	if s, ok := output.(string); ok {
		var m map[string]any
		if err := json.Unmarshal([]byte(s), &m); err == nil {
			output = firstValue(firstValue(m["output"], m["stdout"]), m["content"])
		} else {
			output = s
		}
	} else if m := mapAny(output); len(m) > 0 {
		output = firstValue(firstValue(m["output"], m["stdout"]), m["content"])
	}
	return truncate(stringAny(output), limit)
}

func fileMTime(path string) time.Time {
	st, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return st.ModTime()
}

func sortByUpdated(rows []map[string]any) {
	sort.Slice(rows, func(i, j int) bool {
		return sessionSortAt(rows[i]) > sessionSortAt(rows[j])
	})
}

func sessionSortAt(row map[string]any) string {
	return firstNonEmpty(stringAny(row["last_reply_at"]), stringAny(row["updated_at"]))
}

func msgText(message any) string {
	if s, ok := message.(string); ok {
		return s
	}
	m := mapAny(message)
	c := m["content"]
	if s, ok := c.(string); ok {
		return s
	}
	for _, raw := range listAny(c) {
		if s, ok := raw.(string); ok {
			return s
		}
		b := mapAny(raw)
		if stringAny(b["type"]) == "text" {
			return stringAny(b["text"])
		}
	}
	return ""
}

func mapAny(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return nil
}

func listAny(v any) []any {
	switch x := v.(type) {
	case []any:
		return x
	case []string:
		out := make([]any, len(x))
		for i, s := range x {
			out[i] = s
		}
		return out
	}
	return nil
}

func stringAny(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		return x.String()
	case float64:
		if x == float64(int64(x)) {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'f', -1, 64)
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case bool:
		if x {
			return "true"
		}
		return "false"
	}
	return ""
}

func intAny(v any) int {
	n, ok := numberToInt64(v)
	if ok {
		return int(n)
	}
	if s := stringAny(v); s != "" {
		n, _ := strconv.Atoi(s)
		return n
	}
	return 0
}

func boolAny(v any) bool {
	b, _ := v.(bool)
	return b
}

func firstValue(a, b any) any {
	if a != nil && stringAny(a) != "" {
		return a
	}
	return b
}

func nullableString(v any) any {
	if s := stringAny(v); s != "" {
		return s
	}
	return nil
}

func nullableNonEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "\n...(truncated)"
}
