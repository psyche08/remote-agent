package provider

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/psyche08/remote-agent/internal/config"
)

type claudeRuntimeSession struct {
	SessionID  string
	PID        int
	Cwd        string
	Executable string
	Connected  time.Time
	Updated    time.Time
	Running    bool
}

type claudeControlRequest struct {
	RequestID   string
	SessionID   string
	Source      string
	ToolName    string
	ToolUseID   string
	Input       map[string]any
	Suggestions []any
}

type Claude struct {
	id                    string
	cfg                   config.ProviderConfig
	command               string
	cwd                   string
	permissionMode        string
	preferDesktop         bool
	turnstateDir          string
	resumeWait            time.Duration
	staleAfter            time.Duration
	killGrace             time.Duration
	sessions              map[string]string
	mappingMu             sync.RWMutex
	cliIDs                map[string]string
	lastSessionID         string
	lastState             string
	lastError             string
	lastChange            time.Time
	streamMu              sync.RWMutex
	streamPublisher       func(target string, frame map[string]any)
	streamTargets         map[string]map[string]bool
	streamTextDelta       map[string]bool
	streamTools           map[string]map[string]map[string]any
	streamControls        map[string]map[string]*claudeControlRequest
	streamControlOrder    map[string][]string
	recoveredQuestions    map[string]map[string]any
	recoveredQuestionSeen map[string]bool
	cliStream             *claudeStreamBackend
	desktopProcesses      claudeDesktopProcessManager
}

var (
	desktopVersionRE = regexp.MustCompile(`^v?(\d+(?:\.\d+)*)`)
	claudeUUIDRE     = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

func NewClaude(id string, cfg config.ProviderConfig) *Claude {
	c := &Claude{
		id:                    id,
		cfg:                   cfg,
		command:               firstNonEmpty(cfg.Command, "claude"),
		cwd:                   expandUser(firstNonEmpty(cfg.Cwd, "~/Developer")),
		permissionMode:        stringExtra(cfg.Extra, "permission_mode", "auto"),
		preferDesktop:         boolExtra(cfg.Extra, "prefer_desktop_claude", false),
		turnstateDir:          stringExtra(cfg.Extra, "turnstate_dir", "~/.claude/remote-agent-turnstate"),
		resumeWait:            durationExtra(cfg.Extra, "resume_wait_request_cap", 8*time.Second),
		staleAfter:            durationExtra(cfg.Extra, "turnstate_stale_after", 90*time.Second),
		killGrace:             durationExtra(cfg.Extra, "kill_grace", 3*time.Second),
		sessions:              map[string]string{},
		cliIDs:                map[string]string{},
		lastState:             "idle",
		streamTargets:         map[string]map[string]bool{},
		streamTextDelta:       map[string]bool{},
		streamTools:           map[string]map[string]map[string]any{},
		streamControls:        map[string]map[string]*claudeControlRequest{},
		streamControlOrder:    map[string][]string{},
		recoveredQuestions:    map[string]map[string]any{},
		recoveredQuestionSeen: map[string]bool{},
		desktopProcesses:      newSystemClaudeDesktopProcessManager(),
	}
	c.cliStream = newClaudeStreamBackend(c.onCLIStreamEvent, c.setTranscriptID)
	return c
}

func NewClaudeCLI(id string, cfg config.ProviderConfig) *Claude {
	return NewClaude(id, cfg)
}

func (c *Claude) ID() string { return c.id }

// Installed reports whether a runnable Claude CLI exists on this device.
// The registered provider prefers PATH; an explicitly configured absolute
// path or extra.prefer_desktop_claude=true may opt into another binary.
func (c *Claude) Installed() bool { return c.resolveCommand() != "" }

func (c *Claude) SetStreamPublisher(publish func(target string, frame map[string]any)) {
	c.streamMu.Lock()
	c.streamPublisher = publish
	c.streamMu.Unlock()
}

func (c *Claude) StopCLIStream() {
	if c.cliStream != nil {
		_ = c.cliStream.Close()
	}
}

func (c *Claude) Status() Status {
	cli := c.resolveCommand()
	err := (*string)(nil)
	if c.lastError != "" {
		err = &c.lastError
	}
	return Status{
		ProviderID:  c.id,
		AppName:     firstNonEmpty(c.cfg.AppName, "Claude"),
		IsRunning:   cli != "",
		IsFrontmost: false,
		Installed:   cli != "",
		State:       c.lastState,
		LastError:   err,
		Capabilities: map[string]bool{
			"native_sessions": true, "native_task_status": true, "clipboard_output": true,
			"screenshot": true, "ocr": false, "approval": true, "interrupt": true, "steer": false,
			"streaming": true, "desktop_wrapper": false, "tmux": false,
			"stream_json": true, "create_session": true,
		},
		Backend: "claude_stream_json_go",
		Command: c.command,
		Cwd:     c.cwd,
	}
}

func (c *Claude) ModelSelect() ModelSelect {
	return ModelSelect{
		Models:  []ModelOption{{ID: "opus", Label: "Opus"}, {ID: "sonnet", Label: "Sonnet"}, {ID: "haiku", Label: "Haiku"}},
		Efforts: []string{"low", "medium", "high", "xhigh"},
		Mode:    c.permissionMode,
		Modes:   []ModeOption{{ID: "auto", Label: "Auto"}, {ID: "edit", Label: "Edit"}, {ID: "plan", Label: "Plan"}, {ID: "default", Label: "Ask"}},
	}
}

func (c *Claude) ListNativeSessions() []map[string]any {
	cli := claudeCLISessions(stringExtra(c.cfg.Extra, "claude_projects_dir", ""), nativeSessionListLimit)
	desktop := claudeDesktopSessions(stringExtra(c.cfg.Extra, "claude_code_sessions_dir", ""), nativeSessionListLimit)
	byID := map[string]map[string]any{}
	origins := map[string]map[string]bool{}
	for _, group := range []struct {
		origin string
		rows   []map[string]any
	}{
		{origin: "cli", rows: cli},
		{origin: "desktop", rows: desktop},
	} {
		for _, row := range group.rows {
			uid := stringAny(row["cli_session_id"])
			if uid == "" {
				continue
			}
			if origins[uid] == nil {
				origins[uid] = map[string]bool{}
			}
			origins[uid][group.origin] = true
			if byID[uid] == nil {
				cp := map[string]any{}
				for k, v := range row {
					cp[k] = v
				}
				byID[uid] = cp
			} else {
				mergeClaudeNativeMetadata(byID[uid], row)
			}
		}
	}
	out := make([]map[string]any, 0, len(byID))
	for uid, row := range byID {
		if origins[uid]["cli"] && origins[uid]["desktop"] {
			row["origin"] = "both"
		} else if origins[uid]["cli"] {
			row["origin"] = "cli"
		} else {
			row["origin"] = "desktop"
		}
		out = append(out, row)
	}
	sortByUpdated(out)
	return out
}

func mergeClaudeNativeMetadata(dst map[string]any, src map[string]any) {
	if dst == nil || src == nil {
		return
	}
	for _, key := range []string{"title", "cwd", "branch", "worktree", "model", "created_at", "last_reply_at"} {
		if stringAny(dst[key]) == "" && stringAny(src[key]) != "" {
			dst[key] = src[key]
		}
	}
	if title := stringAny(src["title"]); stringAny(src["source"]) == "claude_desktop" && title != "" && title != "(untitled)" {
		dst["title"] = title
	}
	if stringAny(src["updated_at"]) > stringAny(dst["updated_at"]) {
		dst["updated_at"] = src["updated_at"]
	}
	if stringAny(src["native_session_id"]) != "" && stringAny(src["source"]) == "claude_desktop" {
		dst["desktop_session_id"] = src["native_session_id"]
	}
}

func (c *Claude) RuntimeSessions() []map[string]any {
	native := c.ListNativeSessions()
	nativeByCLI := map[string]map[string]any{}
	for _, row := range native {
		if cliID := stringAny(row["cli_session_id"]); cliID != "" {
			nativeByCLI[cliID] = row
		}
	}
	byKey := map[string]map[string]any{}
	add := func(row map[string]any) {
		key := firstNonEmpty(stringAny(row["transcript_id"]), stringAny(row["session_id"]))
		if key == "" {
			return
		}
		if prev := byKey[key]; prev != nil {
			for k, v := range row {
				if prev[k] == nil || stringAny(prev[k]) == "" {
					prev[k] = v
				}
			}
			if boolAny(row["live"]) {
				prev["live"] = true
			}
			if stringAny(row["status"]) == "running" {
				prev["status"] = "running"
				prev["state"] = "running"
			}
			if stringAny(row["updated_at"]) > stringAny(prev["updated_at"]) {
				prev["updated_at"] = row["updated_at"]
			}
			return
		}
		byKey[key] = row
	}
	if c.cliStream != nil {
		for _, session := range c.cliStream.Sessions() {
			status := "idle"
			state := "idle"
			if session.Running {
				status = "running"
				state = "running"
			}
			row := map[string]any{
				"session_id": c.logicalForTranscript(session.SessionID), "provider_id": c.id,
				"transcript_id": session.SessionID, "native_session_id": session.SessionID,
				"cwd": session.Cwd, "updated_at": session.Updated.UTC().Format(time.RFC3339Nano),
				"connected_at": session.Connected.UTC().Format(time.RFC3339Nano), "live": true,
				"status": status, "state": state, "source": "claude_cli_stream", "pid": session.PID,
			}
			mergeClaudeNativeRuntime(row, nativeByCLI[session.SessionID])
			byKey[session.SessionID] = row
		}
	}
	for _, rec := range listTurnstates(c.turnstateDir) {
		if turnstateRecordIdle(rec, c.staleAfter) {
			continue
		}
		tid := rec.SessionID
		row := map[string]any{
			"session_id":        c.logicalForTranscript(tid),
			"provider_id":       c.id,
			"transcript_id":     tid,
			"native_session_id": tid,
			"cwd":               rec.Cwd,
			"updated_at":        turnstateRecordUpdatedAt(rec),
			"live":              true,
			"status":            "running",
			"state":             "running",
			"source":            "claude_turnstate",
		}
		mergeClaudeNativeRuntime(row, nativeByCLI[tid])
		add(row)
	}
	rows := make([]map[string]any, 0, len(byKey))
	for _, row := range byKey {
		rows = append(rows, row)
	}
	sortByUpdated(rows)
	return rows
}

func (c *Claude) PendingApprovalSessionIDs() []string {
	c.streamMu.RLock()
	seen := map[string]bool{}
	for transcriptID, pending := range c.streamControls {
		if transcriptID != "" && len(pending) > 0 {
			seen[transcriptID] = true
		}
	}
	for transcriptID, question := range c.recoveredQuestions {
		if transcriptID != "" && question != nil {
			seen[transcriptID] = true
		}
	}
	c.streamMu.RUnlock()
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func mergeClaudeNativeRuntime(row map[string]any, native map[string]any) {
	if row == nil || native == nil {
		return
	}
	for _, k := range []string{"title", "cwd", "branch", "worktree", "model", "updated_at", "last_reply_at", "created_at", "origin"} {
		if stringAny(row[k]) == "" && stringAny(native[k]) != "" {
			row[k] = native[k]
		}
	}
	if stringAny(row["native_session_id"]) == "" && stringAny(native["native_session_id"]) != "" {
		row["native_session_id"] = native["native_session_id"]
	}
	if stringAny(row["transcript_id"]) == "" && stringAny(native["cli_session_id"]) != "" {
		row["transcript_id"] = native["cli_session_id"]
	}
	if stringAny(native["source"]) != "" {
		row["native_source"] = native["source"]
	}
}

func (c *Claude) OpenOrCreateSession(sessionID string, opts StartOptions) (string, error) {
	if !c.ensureReady() {
		return "", nil
	}
	cid := c.transcriptID(sessionID)
	if cid == sessionID && !claudeUUIDRE.MatchString(sessionID) {
		cid = ""
	}
	if cid == "" && claudeUUIDRE.MatchString(sessionID) {
		cid = sessionID
	}
	if cid == "" {
		cid = newUUID()
	}
	targetCwd := c.cwd
	if opts.Cwd != "" {
		targetCwd = opts.Cwd
	}
	if err := c.startCLIStream(sessionID, cid, targetCwd, false, false, opts); err != nil {
		c.lastError = err.Error()
		c.lastState = "error"
		return "", err
	}
	c.sessions[sessionID] = cid
	c.setTranscriptID(sessionID, cid)
	c.lastSessionID = sessionID
	c.lastState = "idle"
	c.lastChange = time.Now()
	return cid, nil
}

func (c *Claude) OpenResumeSession(sessionID string, resumeID string, cwd string, fork bool) (string, error) {
	if !c.ensureReady() {
		return "", CodexAppServerError{firstNonEmpty(c.lastError, "Claude CLI unavailable")}
	}
	handedOff, err := c.prepareCLIResume(resumeID, fork)
	if err != nil {
		c.lastError = err.Error()
		c.lastState = "error"
		return "", CodexAppServerError{c.lastError}
	}
	if !handedOff && !turnstateIdle(resumeID, c.turnstateDir, c.staleAfter) {
		c.lastError = "turn 进行中（" + resumeID + " 仍 running），稍后再试"
		c.lastState = "running"
		return "", CodexAppServerError{c.lastError}
	}
	targetCwd := firstNonEmpty(cwd, c.cwd)
	if st, err := os.Stat(expandUser(targetCwd)); err != nil || !st.IsDir() {
		targetCwd = c.cwd
	}
	if err := c.startCLIStream(sessionID, resumeID, targetCwd, true, fork, StartOptions{}); err != nil {
		c.lastError = err.Error()
		c.lastState = "error"
		return "", err
	}
	c.sessions[sessionID] = resumeID
	c.setTranscriptID(sessionID, resumeID)
	c.lastSessionID = sessionID
	c.lastState = "idle"
	c.lastChange = time.Now()
	return resumeID, nil
}

func (c *Claude) WaitResumable(resumeID string) bool {
	// A CLI-only provider performs a precise Desktop-process handoff inside
	// OpenResumeSession. Let the mutating resume request reach that guard even
	// while Desktop's turnstate still says running.
	if len(c.desktopSessionAliases(resumeID)) > 0 {
		return true
	}
	return waitTurnstateIdle(resumeID, c.turnstateDir, c.resumeWait, c.staleAfter)
}

func (c *Claude) desktopSessionAliases(resumeID string) []string {
	aliases := map[string]bool{resumeID: true}
	found := false
	for _, row := range claudeDesktopSessions(stringExtra(c.cfg.Extra, "claude_code_sessions_dir", ""), 0) {
		if stringAny(row["cli_session_id"]) != resumeID {
			continue
		}
		found = true
		if nativeID := stringAny(row["native_session_id"]); nativeID != "" {
			aliases[nativeID] = true
		}
		for _, bridgeID := range listAny(row["bridge_session_ids"]) {
			if id := stringAny(bridgeID); id != "" {
				aliases[id] = true
			}
		}
	}
	if !found {
		return nil
	}
	out := make([]string, 0, len(aliases))
	for alias := range aliases {
		out = append(out, alias)
	}
	sort.Strings(out)
	return out
}

func (c *Claude) prepareCLIResume(resumeID string, fork bool) (bool, error) {
	if fork {
		return false, nil
	}
	aliases := c.desktopSessionAliases(resumeID)
	if len(aliases) == 0 {
		return false, nil
	}
	if c.desktopProcesses == nil {
		return false, errors.New("Claude Desktop process inspection unavailable; refusing competing CLI owner")
	}
	stopped, err := c.desktopProcesses.StopSession(aliases, c.killGrace)
	if err != nil {
		return false, fmt.Errorf("Claude Desktop handoff failed: %w", err)
	}
	return stopped, nil
}

func (c *Claude) CloseSession(sessionID string) map[string]any {
	existed := c.cliStream != nil && c.cliStream.CloseSession(sessionID)
	delete(c.sessions, sessionID)
	if c.lastSessionID == sessionID {
		c.lastSessionID = ""
	}
	return map[string]any{"ok": true, "killed": existed, "session": sessionID, "backend": "stream-json"}
}

func (c *Claude) SendPrompt(sessionID string, prompt string) SendResult {
	return c.SendPromptWithAttachments(sessionID, prompt, nil)
}

func (c *Claude) SendPromptWithAttachments(sessionID string, prompt string, attachments []Attachment) SendResult {
	if strings.TrimSpace(prompt) == "" && len(attachments) == 0 {
		msg := "empty prompt"
		return SendResult{OK: false, State: c.lastState, Error: &msg}
	}
	content, contentErr := claudeUserContent(prompt, attachments)
	if contentErr != nil {
		msg := contentErr.Error()
		return SendResult{OK: false, State: "error", Error: &msg}
	}
	if !c.ensureReady() {
		msg := firstNonEmpty(c.lastError, "Claude CLI unavailable")
		return SendResult{OK: false, State: "needs_manual", Error: &msg}
	}
	return c.sendCLIStreamPrompt(sessionID, content)
}

func (c *Claude) LatestOutput(sessionID string) map[string]any {
	text := ""
	if c.cliStream != nil {
		text = c.cliStream.Output(sessionID)
	}
	return map[string]any{"source": "claude_cli_stream", "text": text, "approval_required": c.pendingControl(c.transcriptID(sessionID), "") != nil}
}

func (c *Claude) DetectState(sessionID string) string {
	if sessionID == "" {
		sessionID = c.lastSessionID
	}
	if sessionID == "" {
		return c.lastState
	}
	if c.pendingControl(c.transcriptID(sessionID), "") != nil {
		return "waiting_approval"
	}
	if c.recoveredTranscriptQuestion(c.transcriptID(sessionID)) != nil {
		return "waiting_approval"
	}
	if claudePendingNativeUIPrompt(c.transcriptID(sessionID), stringExtra(c.cfg.Extra, "claude_projects_dir", "")) != nil {
		return "waiting_input"
	}
	if c.PendingQuestion(sessionID) != nil {
		return "waiting_approval"
	}
	if c.cliStream != nil {
		if running, ok := c.cliStream.SessionRunning(sessionID); ok {
			if running {
				return "running"
			}
			return "idle"
		}
	}
	if c.lastState == "running" {
		return "completed"
	}
	return c.lastState
}

func (c *Claude) RelayApproval(sessionID string, decision string) map[string]any {
	if decision != "allow" && decision != "deny" {
		return map[string]any{"ok": false, "detail": "decision must be allow or deny"}
	}
	if pending := c.pendingControl(c.transcriptID(sessionID), ""); pending != nil {
		return c.RelayApprovalRequest(sessionID, pending.RequestID, decision)
	}
	return map[string]any{"ok": false, "detail": "Claude control request is no longer pending"}
}

func (c *Claude) PendingQuestion(sessionID string) map[string]any {
	tid := c.transcriptID(sessionID)
	if pending := c.pendingControl(tid, ""); pending != nil {
		if pending.ToolName == "AskUserQuestion" {
			question := claudeAskUserQuestionRequest(pending.Input, pending.ToolUseID, "")
			if question != nil {
				question["request_id"] = pending.RequestID
				question["source"] = firstNonEmpty(pending.Source, "claude_cli_stream")
				return question
			}
		}
		return nil
	}
	if q := c.recoveredTranscriptQuestion(tid); q != nil {
		return q
	}
	q := claudePendingQuestion(tid, stringExtra(c.cfg.Extra, "claude_projects_dir", ""))
	markClaudeTranscriptQuestion(q)
	return q
}

func markClaudeTranscriptQuestion(q map[string]any) {
	if q == nil {
		return
	}
	// A transcript proves the question exists but cannot recreate the live
	// control callback after its stdio owner is gone. Keep it visible without
	// offering an answer button that would inevitably fail or hit a new owner.
	q["actionable"] = false
	q["source"] = "claude_transcript"
	q["summary"] = "Claude is waiting for an answer in the session that owns this turn"
}

func (c *Claude) ApprovalRequest(sessionID string) map[string]any {
	tid := c.transcriptID(sessionID)
	if q := c.PendingQuestion(sessionID); q != nil {
		return q
	}
	if pending := c.pendingControl(tid, ""); pending != nil {
		detail := toolDetail(pending.Input, 240)
		typ := "operation"
		switch strings.ToLower(pending.ToolName) {
		case "bash":
			typ = "command"
		case "edit", "write", "notebookedit":
			typ = "file_change"
		}
		details := mustJSON(pending.Input)
		if len(details) > 12000 {
			details = details[:12000] + "\n... (truncated)"
		}
		owner := "Claude Desktop"
		if pending.Source == "claude_cli_stream" {
			owner = "Claude CLI"
		}
		summary := owner + " requests " + firstNonEmpty(pending.ToolName, "tool access")
		if detail != "" {
			summary += ": " + detail
		}
		return map[string]any{
			"type": typ, "summary": summary, "details": details,
			"request_id": pending.RequestID, "tool_use_id": pending.ToolUseID,
			"tool_name": pending.ToolName, "source": firstNonEmpty(pending.Source, "claude_cli_stream"),
		}
	}
	if request := claudePendingNativeUIPrompt(tid, stringExtra(c.cfg.Extra, "claude_projects_dir", "")); request != nil {
		return request
	}
	return nil
}

func (c *Claude) SendKeys(sessionID string, keys []string) map[string]any {
	return map[string]any{"ok": false, "detail": "stream-json sessions accept structured messages, not raw keys"}
}

func (c *Claude) Interrupt(sessionID string) map[string]any {
	if c.cliStream == nil || !c.cliStream.HasSession(sessionID) {
		return map[string]any{"ok": false, "detail": "Claude stream-json session is not running"}
	}
	if _, err := c.cliStream.ControlRequest(sessionID, map[string]any{"subtype": "interrupt"}, 10*time.Second); err != nil {
		return map[string]any{"ok": false, "detail": err.Error()}
	}
	c.lastState = "idle"
	return map[string]any{"ok": true, "detail": "interrupt acknowledged by Claude stream-json CLI"}
}

func (c *Claude) SetSessionModel(sessionID string, model string, effort string) map[string]any {
	return map[string]any{"ok": false, "error": "model and effort are applied when a stream-json session starts"}
}

func (c *Claude) BindTranscript(sessionID string, transcriptID string) {
	if sessionID == "" || transcriptID == "" {
		return
	}
	c.setTranscriptID(sessionID, transcriptID)
	c.streamMu.Lock()
	if c.streamTargets[transcriptID] == nil {
		c.streamTargets[transcriptID] = map[string]bool{}
	}
	c.streamTargets[transcriptID][sessionID] = true
	c.streamMu.Unlock()
}

func (c *Claude) recoverTranscriptQuestion(transcriptID string) map[string]any {
	if transcriptID == "" {
		return nil
	}
	c.streamMu.RLock()
	seen := c.recoveredQuestionSeen[transcriptID]
	q := c.recoveredQuestions[transcriptID]
	c.streamMu.RUnlock()
	if seen {
		return q
	}
	q = claudePendingQuestion(transcriptID, stringExtra(c.cfg.Extra, "claude_projects_dir", ""))
	markClaudeTranscriptQuestion(q)
	c.streamMu.Lock()
	c.recoveredQuestionSeen[transcriptID] = true
	if q == nil {
		delete(c.recoveredQuestions, transcriptID)
	} else {
		c.recoveredQuestions[transcriptID] = q
	}
	c.streamMu.Unlock()
	return q
}

func (c *Claude) recoveredTranscriptQuestion(transcriptID string) map[string]any {
	c.streamMu.RLock()
	defer c.streamMu.RUnlock()
	return c.recoveredQuestions[transcriptID]
}

func (c *Claude) SessionRunning(sessionID string) *bool {
	if sessionID == "" {
		return nil
	}
	tid := c.transcriptID(sessionID)
	if tid == "" {
		return nil
	}
	if c.cliStream != nil {
		if running, ok := c.cliStream.SessionRunning(sessionID); ok {
			return &running
		}
	}
	if rec, err := readTurnstate(tid, c.turnstateDir); err == nil && rec != nil {
		running := !turnstateRecordIdle(rec, c.staleAfter)
		return &running
	}
	return nil
}

func (c *Claude) onCLIStreamEvent(session claudeRuntimeSession, payload json.RawMessage) {
	c.onClaudeEvent(session, payload, "claude_cli_stream")
}

func (c *Claude) onClaudeEvent(session claudeRuntimeSession, payload json.RawMessage, source string) {
	c.streamMu.Lock()
	frames := c.streamFramesLocked(session, payload, source)
	publish := c.streamPublisher
	targets := []string{session.SessionID}
	for target := range c.streamTargets[session.SessionID] {
		if target != session.SessionID {
			targets = append(targets, target)
		}
	}
	c.streamMu.Unlock()
	if publish == nil {
		return
	}
	for _, target := range targets {
		for _, frame := range frames {
			publish(target, frame)
		}
	}
}

func (c *Claude) streamFramesLocked(session claudeRuntimeSession, payload json.RawMessage, source string) []map[string]any {
	var event map[string]any
	if json.Unmarshal(payload, &event) != nil {
		return nil
	}
	sessionID := session.SessionID
	typ := stringAny(event["type"])
	switch typ {
	case "control_request":
		delete(c.recoveredQuestions, sessionID)
		request := mapAny(event["request"])
		if stringAny(request["subtype"]) != "can_use_tool" {
			return nil
		}
		requestID := stringAny(event["request_id"])
		if requestID == "" {
			return nil
		}
		if c.streamControls[sessionID] == nil {
			c.streamControls[sessionID] = map[string]*claudeControlRequest{}
		}
		if c.streamControls[sessionID][requestID] == nil {
			c.streamControlOrder[sessionID] = append(c.streamControlOrder[sessionID], requestID)
		}
		c.streamControls[sessionID][requestID] = &claudeControlRequest{
			RequestID: requestID, SessionID: sessionID, Source: source,
			ToolName: stringAny(request["tool_name"]), ToolUseID: stringAny(request["tool_use_id"]),
			Input: mapAny(request["input"]), Suggestions: listAny(request["permission_suggestions"]),
		}
		return []map[string]any{{"type": "approval_changed", "status": "pending", "request_id": requestID}}
	case "wrapper_control_resolved", "control_cancel_request":
		requestID := stringAny(event["request_id"])
		c.removeControlLocked(sessionID, requestID)
		return []map[string]any{{"type": "approval_changed", "status": "resolved", "request_id": requestID}}
	case "stream_event":
		streamEvent := mapAny(event["event"])
		switch stringAny(streamEvent["type"]) {
		case "message_start":
			return []map[string]any{{"type": "turn", "status": "started", "turn_id": nil}}
		case "content_block_delta":
			delta := mapAny(streamEvent["delta"])
			if stringAny(delta["type"]) == "text_delta" {
				text := stringAny(delta["text"])
				if text != "" {
					c.streamTextDelta[sessionID] = true
					return []map[string]any{{"type": "delta", "turn_id": nil, "text": text}}
				}
			}
		}
	case "assistant":
		return c.streamAssistantFramesLocked(session, event)
	case "user":
		delete(c.recoveredQuestions, sessionID)
		delete(c.recoveredQuestionSeen, sessionID)
		c.streamTextDelta[sessionID] = false
		return c.streamToolResultFramesLocked(sessionID, event)
	case "result":
		delete(c.recoveredQuestions, sessionID)
		delete(c.recoveredQuestionSeen, sessionID)
		delete(c.streamTextDelta, sessionID)
		delete(c.streamTools, sessionID)
		delete(c.streamControls, sessionID)
		delete(c.streamControlOrder, sessionID)
		return []map[string]any{{"type": "turn", "status": "completed", "turn_id": nil}}
	}
	return nil
}

func (c *Claude) RelayApprovalRequest(sessionID string, requestID string, decision string) map[string]any {
	if decision != "allow" && decision != "deny" {
		return map[string]any{"ok": false, "detail": "decision must be allow or deny"}
	}
	tid := c.transcriptID(sessionID)
	pending := c.pendingControl(tid, requestID)
	if pending == nil {
		return map[string]any{"ok": false, "detail": "Desktop control request is no longer pending"}
	}
	if pending.ToolName == "AskUserQuestion" && decision == "allow" {
		return map[string]any{"ok": false, "detail": "AskUserQuestion requires structured answers"}
	}
	response := map[string]any{"behavior": decision}
	if decision == "allow" {
		response["updatedInput"] = pending.Input
		response["decisionClassification"] = "user_temporary"
	} else {
		response["message"] = "User denied this request"
		response["interrupt"] = false
		response["decisionClassification"] = "user_reject"
	}
	if err := c.sendControlResponse(tid, pending.RequestID, response); err != nil {
		return map[string]any{"ok": false, "detail": err.Error()}
	}
	c.resolveCLIStreamControl(tid, pending)
	return map[string]any{"ok": true, "detail": "control response sent to Claude", "request_id": pending.RequestID, "decision": decision}
}

func (c *Claude) AnswerQuestion(sessionID string, requestID string, answers map[string]string) map[string]any {
	tid := c.transcriptID(sessionID)
	pending := c.pendingControl(tid, requestID)
	if pending == nil || pending.ToolName != "AskUserQuestion" {
		return map[string]any{"ok": false, "detail": "AskUserQuestion request is no longer pending"}
	}
	questions := listAny(pending.Input["questions"])
	if len(questions) == 0 {
		return map[string]any{"ok": false, "detail": "question payload is empty"}
	}
	clean := map[string]string{}
	for _, raw := range questions {
		question := strings.TrimSpace(stringAny(mapAny(raw)["question"]))
		answer := strings.TrimSpace(answers[question])
		if question == "" || answer == "" {
			return map[string]any{"ok": false, "detail": "every question requires a non-empty answer"}
		}
		clean[question] = answer
	}
	updated := map[string]any{}
	for key, value := range pending.Input {
		updated[key] = value
	}
	updated["answers"] = clean
	if pending.ToolUseID != "" {
		updated["_toolUseBlockId"] = pending.ToolUseID
	}
	response := map[string]any{
		"behavior": "allow", "updatedInput": updated, "decisionClassification": "user_temporary",
	}
	if err := c.sendControlResponse(tid, pending.RequestID, response); err != nil {
		return map[string]any{"ok": false, "detail": err.Error()}
	}
	c.resolveCLIStreamControl(tid, pending)
	return map[string]any{"ok": true, "detail": "question answers sent to Claude", "request_id": pending.RequestID}
}

func (c *Claude) sendControlResponse(sessionID string, requestID string, response map[string]any) error {
	payload, err := json.Marshal(map[string]any{
		"type":     "control_response",
		"response": map[string]any{"subtype": "success", "request_id": requestID, "response": response},
	})
	if err != nil {
		return err
	}
	if c.cliStream == nil {
		return fmt.Errorf("Claude stream-json session is not connected: %s", sessionID)
	}
	return c.cliStream.Send(sessionID, payload)
}

func (c *Claude) resolveCLIStreamControl(sessionID string, pending *claudeControlRequest) {
	if pending == nil || pending.Source != "claude_cli_stream" {
		return
	}
	c.streamMu.Lock()
	c.removeControlLocked(sessionID, pending.RequestID)
	publish := c.streamPublisher
	targets := []string{sessionID}
	for target := range c.streamTargets[sessionID] {
		if target != sessionID {
			targets = append(targets, target)
		}
	}
	c.streamMu.Unlock()
	if publish != nil {
		for _, target := range targets {
			publish(target, map[string]any{"type": "approval_changed", "status": "resolved", "request_id": pending.RequestID})
		}
	}
}

func (c *Claude) pendingControl(sessionID string, requestID string) *claudeControlRequest {
	c.streamMu.RLock()
	defer c.streamMu.RUnlock()
	return c.pendingControlLocked(sessionID, requestID, "")
}

func (c *Claude) pendingControlLocked(sessionID string, requestID string, toolName string) *claudeControlRequest {
	pending := c.streamControls[sessionID]
	if requestID != "" {
		control := pending[requestID]
		if control != nil && (toolName == "" || control.ToolName == toolName) {
			return control
		}
		return nil
	}
	order := c.streamControlOrder[sessionID]
	for i := len(order) - 1; i >= 0; i-- {
		control := pending[order[i]]
		if control != nil && (toolName == "" || control.ToolName == toolName) {
			return control
		}
	}
	return nil
}

func (c *Claude) removeControlLocked(sessionID string, requestID string) {
	if requestID == "" {
		return
	}
	delete(c.streamControls[sessionID], requestID)
	if len(c.streamControls[sessionID]) == 0 {
		delete(c.streamControls, sessionID)
		delete(c.streamControlOrder, sessionID)
	}
}

func (c *Claude) streamAssistantFramesLocked(session claudeRuntimeSession, event map[string]any) []map[string]any {
	sessionID := session.SessionID
	if c.streamTools[sessionID] == nil {
		c.streamTools[sessionID] = map[string]map[string]any{}
	}
	message := mapAny(event["message"])
	content := message["content"]
	blocks := listAny(content)
	if text, ok := content.(string); ok {
		blocks = []any{map[string]any{"type": "text", "text": text}}
	}
	ts := firstNonEmpty(stringAny(event["timestamp"]), time.Now().UTC().Format(time.RFC3339Nano))
	frames := []map[string]any{}
	for _, raw := range blocks {
		block := mapAny(raw)
		switch stringAny(block["type"]) {
		case "text":
			if c.streamTextDelta[sessionID] {
				continue
			}
			if text := stringAny(block["text"]); text != "" {
				frames = append(frames, map[string]any{"type": "item", "item": map[string]any{
					"role": "assistant", "kind": "text", "text": text, "ts": ts,
				}})
			}
		case "thinking":
			if text := stringAny(block["thinking"]); text != "" {
				frames = append(frames, map[string]any{"type": "item", "item": map[string]any{
					"role": "assistant", "kind": "thinking", "text": text, "ts": ts,
					"item_id": nullableNonEmpty(stringAny(block["id"])),
				}})
			}
		case "tool_use":
			id := stringAny(block["id"])
			item := map[string]any{
				"role": "assistant", "kind": "tool", "name": firstNonEmpty(stringAny(block["name"]), "tool"),
				"text": toolDetail(block["input"], 160), "io": toolIO(block["name"], block["input"]),
				"files": extractPaths(block["name"], block["input"], session.Cwd), "result": nil, "ts": ts,
				"item_id": nullableNonEmpty(id), "call_id": nullableNonEmpty(id),
			}
			if id != "" {
				c.streamTools[sessionID][id] = item
			}
			frames = append(frames, map[string]any{"type": "item", "item": item})
		}
	}
	return frames
}

func (c *Claude) streamToolResultFramesLocked(sessionID string, event map[string]any) []map[string]any {
	pending := c.streamTools[sessionID]
	if len(pending) == 0 {
		return nil
	}
	frames := []map[string]any{}
	for _, raw := range listAny(mapAny(event["message"])["content"]) {
		block := mapAny(raw)
		if stringAny(block["type"]) != "tool_result" {
			continue
		}
		item := pending[stringAny(block["tool_use_id"])]
		if item == nil {
			continue
		}
		item["result"] = resultText(block["content"], 2500)
		item["is_error"] = boolAny(block["is_error"])
		frames = append(frames, map[string]any{"type": "item_update", "item": item})
	}
	return frames
}

func (c *Claude) SessionMessages(sessionID string) ([]map[string]any, error) {
	// Return the full logical history here; /session_preview applies tail/offset
	// pagination after extraction. Truncating before that layer made older
	// Desktop images impossible to load even when the UI requested them.
	return claudeSessionMessages(c.transcriptID(sessionID), stringExtra(c.cfg.Extra, "claude_projects_dir", ""), nativePreviewUnlimited), nil
}

func (c *Claude) ReadSessionAsset(sessionID string, assetID string) (SessionAsset, bool, error) {
	return claudeTranscriptAsset(c.transcriptID(sessionID), stringExtra(c.cfg.Extra, "claude_projects_dir", ""), assetID)
}

func (c *Claude) SessionModel(sessionID string) map[string]any {
	return claudeSessionModel(c.transcriptID(sessionID), stringExtra(c.cfg.Extra, "claude_projects_dir", ""))
}

func (c *Claude) ReferencedFiles(sessionID string) map[string]bool {
	return referencedFilesFromMessages(claudeSessionMessages(c.transcriptID(sessionID), stringExtra(c.cfg.Extra, "claude_projects_dir", ""), nativePreviewMaxItems))
}

func (c *Claude) transcriptID(sessionID string) string {
	c.mappingMu.RLock()
	defer c.mappingMu.RUnlock()
	if cid := c.cliIDs[sessionID]; cid != "" {
		return cid
	}
	return sessionID
}

func (c *Claude) logicalForTranscript(transcriptID string) string {
	c.mappingMu.RLock()
	defer c.mappingMu.RUnlock()
	logical := []string{}
	for sid, tid := range c.cliIDs {
		if tid == transcriptID {
			if sid != transcriptID {
				logical = append(logical, sid)
			}
		}
	}
	if len(logical) > 0 {
		sort.Strings(logical)
		return logical[0]
	}
	return transcriptID
}

func (c *Claude) setTranscriptID(sessionID string, transcriptID string) {
	if sessionID == "" || transcriptID == "" {
		return
	}
	c.mappingMu.Lock()
	c.cliIDs[sessionID] = transcriptID
	c.mappingMu.Unlock()
}

func (c *Claude) ensureReady() bool {
	if c.resolveCommand() == "" {
		c.lastError = "CLI command not found on PATH: " + c.command
		c.lastState = "needs_manual"
		return false
	}
	return true
}

func (c *Claude) startCLIStream(sessionID string, transcriptID string, cwd string, resume bool, fork bool, opts StartOptions) error {
	if c.cliStream == nil {
		return errors.New("Claude stream-json backend is unavailable")
	}
	if c.cliStream.HasSession(sessionID) {
		return nil
	}
	bin := c.resolveCommand()
	if bin == "" {
		return errors.New("Claude CLI command not found")
	}
	targetCwd := expandUser(firstNonEmpty(cwd, c.cwd))
	if st, err := os.Stat(targetCwd); err != nil || !st.IsDir() {
		return fmt.Errorf("Claude session cwd is not a directory: %s", targetCwd)
	}
	args := []string{
		"-p", "--input-format", "stream-json", "--output-format", "stream-json",
		"--verbose", "--include-partial-messages", "--include-hook-events", "--replay-user-messages",
		"--permission-prompt-tool", "stdio",
	}
	if resume {
		args = append(args, "--resume", transcriptID)
		if fork {
			args = append(args, "--fork-session")
		}
	} else {
		args = append(args, "--session-id", transcriptID)
	}
	args = append(args, c.permissionArgs(opts.Mode)...)
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	if opts.Effort != "" {
		args = append(args, "--effort", opts.Effort)
	}
	return c.cliStream.Start(claudeStreamStart{
		LogicalID: sessionID, TranscriptID: transcriptID, Executable: bin,
		Args: args, Cwd: targetCwd, Env: claudeStreamEnv(),
	})
}

func (c *Claude) sendCLIStreamPrompt(sessionID string, content []map[string]any) SendResult {
	if c.cliStream == nil {
		msg := "Claude stream-json backend is unavailable"
		return SendResult{OK: false, State: "error", Error: &msg}
	}
	if running, ok := c.cliStream.SessionRunning(sessionID); ok && running {
		msg := "a turn is already in progress in Claude CLI"
		return SendResult{OK: false, State: "running", Error: &msg}
	}
	tid := c.transcriptID(sessionID)
	if !c.cliStream.HasSession(sessionID) {
		handedOff := false
		if tid != "" {
			var err error
			handedOff, err = c.prepareCLIResume(tid, false)
			if err != nil {
				msg := err.Error()
				c.lastError = msg
				c.lastState = "error"
				return SendResult{OK: false, State: "error", Error: &msg}
			}
		}
		if tid != "" && !handedOff && !turnstateIdle(tid, c.turnstateDir, c.staleAfter) {
			msg := "session turn is still running; wait for idle or attach to the existing Claude owner"
			c.lastError = msg
			c.lastState = "running"
			return SendResult{OK: false, State: "running", Error: &msg}
		}
		knownTranscript := c.sessions[sessionID] != "" || claudeTranscriptPath(tid, stringExtra(c.cfg.Extra, "claude_projects_dir", "")) != ""
		if knownTranscript && claudeUUIDRE.MatchString(tid) {
			cwd := firstNonEmpty(c.cliStream.Cwd(sessionID), c.cwdForTranscript(tid))
			if err := c.startCLIStream(sessionID, tid, cwd, true, false, StartOptions{}); err != nil {
				msg := err.Error()
				return SendResult{OK: false, State: "error", Error: &msg}
			}
		} else if _, err := c.OpenOrCreateSession(sessionID, StartOptions{}); err != nil {
			msg := err.Error()
			return SendResult{OK: false, State: "error", Error: &msg}
		}
		tid = c.transcriptID(sessionID)
	}
	payload, err := json.Marshal(map[string]any{
		"type": "user", "session_id": "", "parent_tool_use_id": nil,
		"message": map[string]any{
			"role": "user", "content": content,
		},
	})
	if err != nil {
		msg := err.Error()
		return SendResult{OK: false, State: "error", Error: &msg}
	}
	if err := c.cliStream.Send(sessionID, payload); err != nil {
		c.lastError = err.Error()
		c.lastState = "error"
		return SendResult{OK: false, State: "error", Error: &c.lastError}
	}
	c.lastError = ""
	c.lastState = "running"
	c.lastChange = time.Now()
	c.lastSessionID = sessionID
	c.sessions[sessionID] = tid
	return SendResult{OK: true, State: "running", Message: "prompt sent to Claude stream-json CLI", NativeTaskID: tid}
}

func (c *Claude) cwdForTranscript(transcriptID string) string {
	for _, row := range c.ListNativeSessions() {
		if stringAny(row["cli_session_id"]) == transcriptID {
			return firstNonEmpty(stringAny(row["cwd"]), c.cwd)
		}
	}
	return c.cwd
}

func (c *Claude) resolveCommand() string {
	if c.preferDesktop && !filepath.IsAbs(c.command) {
		if p := resolveDesktopClaude(""); p != "" {
			return p
		}
	}
	if filepath.IsAbs(c.command) {
		if _, err := os.Stat(c.command); err == nil {
			return c.command
		}
		return ""
	}
	if p, err := exec.LookPath(strings.Fields(c.command)[0]); err == nil {
		return p
	}
	// private-services deliberately runs with a small, predictable PATH. Both
	// Claude's native installer and common package managers place user CLIs in
	// locations that a login shell sees but the daemon may not. Keep a bare
	// "claude" config portable across devices without opting into Desktop's
	// signed internal bundle.
	name := strings.Fields(c.command)[0]
	if !strings.ContainsRune(name, filepath.Separator) {
		for _, candidate := range []string{
			filepath.Join(expandUser("~/.local/bin"), name),
			filepath.Join("/opt/homebrew/bin", name),
			filepath.Join("/usr/local/bin", name),
		} {
			if st, err := os.Stat(candidate); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
				return candidate
			}
		}
	}
	return ""
}

func (c *Claude) permissionArgs(mode string) []string {
	if mode == "" {
		mode = c.permissionMode
	}
	aliases := map[string]string{
		"auto": "auto", "edit": "acceptEdits", "acceptEdits": "acceptEdits",
		"plan": "plan", "ask": "default", "manual": "default", "default": "default",
		"bypass": "bypassPermissions", "bypassPermissions": "bypassPermissions",
	}
	perm := aliases[mode]
	if perm == "" {
		perm = mode
	}
	return []string{"--permission-mode", perm}
}

func resolveDesktopClaude(base string) string {
	if base == "" {
		base = expandUser("~/Library/Application Support/Claude/claude-code")
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		return ""
	}
	type candidate struct {
		path string
		key  []int
	}
	var candidates []candidate
	for _, e := range entries {
		m := desktopVersionRE.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		bin := filepath.Join(base, e.Name(), "claude.app", "Contents", "MacOS", "claude")
		if st, err := os.Stat(bin); err != nil || st.IsDir() {
			continue
		}
		parts := strings.Split(m[1], ".")
		key := make([]int, len(parts))
		for i, p := range parts {
			key[i], _ = strconv.Atoi(p)
		}
		candidates = append(candidates, candidate{path: bin, key: key})
	}
	sort.Slice(candidates, func(i, j int) bool {
		a, b := candidates[i].key, candidates[j].key
		for k := 0; k < len(a) || k < len(b); k++ {
			av, bv := 0, 0
			if k < len(a) {
				av = a[k]
			}
			if k < len(b) {
				bv = b[k]
			}
			if av != bv {
				return av > bv
			}
		}
		return false
	})
	if len(candidates) == 0 {
		return ""
	}
	return candidates[0].path
}

func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return newFallbackID()
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	hexed := hex.EncodeToString(b[:])
	return hexed[:8] + "-" + hexed[8:12] + "-" + hexed[12:16] + "-" + hexed[16:20] + "-" + hexed[20:]
}

func newFallbackID() string {
	return strconv.FormatInt(time.Now().UnixNano(), 16)
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

func looksLikeApproval(text string) bool {
	low := strings.ToLower(text)
	for _, h := range []string{
		"do you want to proceed", "do you want to allow", "allow command", "allow this tool", "permission required",
		"approve", "trust this folder", "1. yes", "❯ 1. yes", "是否允许", "需要授权", "允许此工具",
	} {
		if strings.Contains(low, h) {
			return true
		}
	}
	return false
}

func looksLikeNeedsManual(text string) bool {
	low := strings.ToLower(text)
	for _, h := range []string{"sign in again", "please log in", "please sign in", "session expired", "login expired", "update available", "please update"} {
		if strings.Contains(low, h) {
			return true
		}
	}
	return false
}

func looksBusy(text string) bool {
	low := strings.ToLower(text)
	for _, h := range []string{"esc to interrupt", "esc to stop", "working", "thinking", "generating", "running command", "tool use"} {
		if strings.Contains(low, h) {
			return true
		}
	}
	return false
}

func stringExtra(m map[string]any, key string, fallback string) string {
	if v, ok := m[key].(string); ok && v != "" {
		return v
	}
	return fallback
}

func intExtra(m map[string]any, key string, fallback int) int {
	switch v := m[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return fallback
}

func boolExtra(m map[string]any, key string, fallback bool) bool {
	if v, ok := m[key].(bool); ok {
		return v
	}
	return fallback
}

func durationExtra(m map[string]any, key string, fallback time.Duration) time.Duration {
	switch v := m[key].(type) {
	case float64:
		return time.Duration(v * float64(time.Second))
	case int:
		return time.Duration(v) * time.Second
	}
	return fallback
}

func stringSliceExtra(m map[string]any, key string, fallback []string) []string {
	raw, ok := m[key].([]any)
	if !ok {
		return fallback
	}
	out := []string{}
	for _, v := range raw {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return fallback
	}
	return out
}

func expandUser(p string) string {
	if p == "~" {
		if h, err := os.UserHomeDir(); err == nil {
			return h
		}
	}
	if strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, p[2:])
		}
	}
	return p
}
