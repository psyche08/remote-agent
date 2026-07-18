package provider

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/psyche08/remote-agent/internal/config"
)

const fakeCodexThreadID = "019ecffb-fd7d-7422-b05c-2e1c3ebec53d"

type fakeCodexClient struct {
	mu             sync.Mutex
	threadStatus   map[string]string
	threadTurn     map[string]string
	turns          [][2]string
	rollbacks      [][2]any
	resumes        []string
	responses      []map[string]any
	errorResponses []map[string]any
	respondedIDs   []any
	listResult     any
	resumeResult   any
	active         bool
	lastModel      string
	startErr       error
	startID        string
}

func newFakeCodexClient() *fakeCodexClient {
	return &fakeCodexClient{threadStatus: map[string]string{}, threadTurn: map[string]string{}}
}

func (f *fakeCodexClient) Start() error                                         { return nil }
func (f *fakeCodexClient) Initialize(name string) error                         { return nil }
func (f *fakeCodexClient) Close() error                                         { return nil }
func (f *fakeCodexClient) AccountRateLimits(timeout time.Duration) (any, error) { return nil, nil }
func (f *fakeCodexClient) AccountRead(timeout time.Duration) (any, error)       { return nil, nil }

func (f *fakeCodexClient) ThreadStart(params map[string]any) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		return "", f.startErr
	}
	id := firstNonEmpty(f.startID, "thread-1")
	f.threadStatus[id] = "idle"
	return id, nil
}

func (f *fakeCodexClient) ThreadResume(threadID string, params map[string]any, timeout time.Duration) (any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resumes = append(f.resumes, threadID)
	f.threadStatus[threadID] = "idle"
	if f.resumeResult != nil {
		return f.resumeResult, nil
	}
	return map[string]any{"thread": map[string]any{"id": threadID}}, nil
}

func (f *fakeCodexClient) ThreadFork(threadID string, params map[string]any) (any, error) {
	return map[string]any{"thread": map[string]any{"id": "fork-1"}}, nil
}

func (f *fakeCodexClient) ThreadRollback(threadID string, numTurns int, params map[string]any) (any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rollbacks = append(f.rollbacks, [2]any{threadID, numTurns})
	f.threadStatus[threadID] = "idle"
	return map[string]any{"thread": map[string]any{"id": threadID}}, nil
}

func (f *fakeCodexClient) ThreadList(timeout time.Duration, params map[string]any) (any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listResult != nil {
		return f.listResult, nil
	}
	return map[string]any{"data": []any{}}, nil
}

func (f *fakeCodexClient) TurnStart(threadID string, prompt string, extra map[string]any) (any, error) {
	if f.IsActive(threadID) {
		return nil, errors.New("thread " + threadID + " has a live turn in progress")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.turns = append(f.turns, [2]string{threadID, prompt})
	f.threadTurn[threadID] = "turn-1"
	return map[string]any{"turn": map[string]any{"id": "turn-1"}}, nil
}

func (f *fakeCodexClient) TurnSteer(threadID string, prompt string, extra map[string]any) (any, error) {
	return map[string]any{"turnId": "turn-1"}, nil
}

func (f *fakeCodexClient) TurnInterrupt(threadID string, extra map[string]any) (any, error) {
	return map[string]any{}, nil
}

func (f *fakeCodexClient) Respond(requestID any, result map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.responses = append(f.responses, result)
	f.respondedIDs = append(f.respondedIDs, requestID)
	return nil
}

func (f *fakeCodexClient) RespondError(requestID any, code int, message string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errorResponses = append(f.errorResponses, map[string]any{"id": requestID, "code": code, "message": message})
	return nil
}

func (f *fakeCodexClient) IsActive(threadID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.threadStatus[threadID] == "active"
}
func (f *fakeCodexClient) ThreadStatus(threadID string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.threadStatus[threadID]
	return v, ok
}
func (f *fakeCodexClient) SetThreadStatus(threadID string, status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.threadStatus[threadID] = status
}
func (f *fakeCodexClient) ThreadTurn(threadID string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.threadTurn[threadID]
	return v, ok
}
func (f *fakeCodexClient) SetThreadTurn(threadID string, turnID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.threadTurn[threadID] = turnID
}
func (f *fakeCodexClient) LastModel() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastModel
}

type fakeDesktopClient struct {
	starts         [][2]string
	targetStarts   [][3]string
	err            error
	missingTargets map[string]bool
	snapshots      []map[string]any
	decisions      []map[string]any
	decisionErr    error
}

func (f *fakeDesktopClient) StartTurn(conversationID string, prompt string, opts map[string]any, timeout time.Duration) (any, error) {
	return f.StartTurnOnClient(conversationID, prompt, opts, "", timeout)
}
func (f *fakeDesktopClient) StartTurnOnClient(conversationID string, prompt string, opts map[string]any, targetClientID string, timeout time.Duration) (any, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.targetStarts = append(f.targetStarts, [3]string{conversationID, prompt, targetClientID})
	if f.missingTargets != nil && f.missingTargets[targetClientID] {
		return nil, CodexDesktopIPCError{"thread-follower-start-turn failed: \"no-client-found\""}
	}
	f.starts = append(f.starts, [2]string{conversationID, prompt})
	return map[string]any{"result": map[string]any{"turn": map[string]any{"id": "desktop-turn"}}}, nil
}
func (f *fakeDesktopClient) SteerTurn(conversationID string, prompt string, timeout time.Duration) (any, error) {
	return map[string]any{}, nil
}
func (f *fakeDesktopClient) InterruptTurn(conversationID string, timeout time.Duration) (any, error) {
	return map[string]any{}, nil
}
func (f *fakeDesktopClient) SnapshotLiveThreads(timeout time.Duration) []map[string]any {
	return f.snapshots
}

func (f *fakeDesktopClient) recordDecision(method string, conversationID string, requestID any, payload any, target string) (any, error) {
	if f.decisionErr != nil {
		return nil, f.decisionErr
	}
	f.decisions = append(f.decisions, map[string]any{
		"method": method, "conversationId": conversationID, "requestId": requestID,
		"payload": payload, "target": target,
	})
	return map[string]any{"ok": true}, nil
}

func (f *fakeDesktopClient) CommandApprovalDecision(conversationID string, requestID any, decision any, target string, timeout time.Duration) (any, error) {
	return f.recordDecision("thread-follower-command-approval-decision", conversationID, requestID, decision, target)
}

func (f *fakeDesktopClient) FileApprovalDecision(conversationID string, requestID any, decision any, target string, timeout time.Duration) (any, error) {
	return f.recordDecision("thread-follower-file-approval-decision", conversationID, requestID, decision, target)
}

func (f *fakeDesktopClient) PermissionsApprovalResponse(conversationID string, requestID any, response map[string]any, target string, timeout time.Duration) (any, error) {
	return f.recordDecision("thread-follower-permissions-request-approval-response", conversationID, requestID, response, target)
}

func (f *fakeDesktopClient) SubmitUserInput(conversationID string, requestID any, response map[string]any, target string, timeout time.Duration) (any, error) {
	return f.recordDecision("thread-follower-submit-user-input", conversationID, requestID, response, target)
}

func (f *fakeDesktopClient) SubmitMcpElicitationResponse(conversationID string, requestID any, response map[string]any, target string, timeout time.Duration) (any, error) {
	return f.recordDecision("thread-follower-submit-mcp-server-elicitation-response", conversationID, requestID, response, target)
}

func testCodexWithClient(t *testing.T, fc *fakeCodexClient) *Codex {
	t.Helper()
	fixtures := t.TempDir()
	c := NewCodex("codex", config.ProviderConfig{Command: "codex", Cwd: "/tmp", Extra: map[string]any{
		"codex_session_index": filepath.Join(fixtures, "session_index.jsonl"),
		"codex_sessions_dirs": []any{fixtures},
	}})
	c.clientFactory = func(func(string, map[string]any), func(any, string, map[string]any)) codexAppClient {
		return fc
	}
	// Keep unit tests hermetic: never let a test reach the real Desktop IPC
	// socket on the host machine.
	c.desktopFactory = func() codexDesktopClient { return &fakeDesktopClient{} }
	return c
}

func TestCodexStatusIncludesNormalizedAccountLimits(t *testing.T) {
	fc := newFakeCodexClient()
	fc.lastModel = "gpt-5-codex"
	c := testCodexWithClient(t, fc)
	c.client = fc
	c.rateLimits = map[string]any{
		"primary":   map[string]any{"usedPercent": 31.0, "windowDurationMins": 300, "resetsAt": 12345},
		"secondary": map[string]any{"usedPercent": 12.0, "windowDurationMins": 10080, "resetsAt": 67890},
	}
	c.rateLimitsByID = map[string]any{
		"codex_bengalfox": map[string]any{"limitName": "GPT-Codex-Spark", "primary": map[string]any{"usedPercent": 8.0}},
	}
	c.planType = "pro"

	account := c.Status().Account
	if account["model"] != "gpt-5-codex" || account["plan_type"] != "pro" {
		t.Fatalf("bad account metadata: %#v", account)
	}
	primary := mapAny(account["primary"])
	secondary := mapAny(account["secondary"])
	if primary["used_percent"] != 31.0 || primary["window_mins"] != 300 || secondary["used_percent"] != 12.0 || secondary["window_mins"] != 10080 {
		t.Fatalf("bad normalized windows: %#v", account)
	}
	limits, _ := account["limits"].([]map[string]any)
	if len(limits) != 1 || !boolAny(limits[0]["fast"]) {
		t.Fatalf("spark bucket was not exposed: %#v", account)
	}
}

func desktopOwnerSnapshot(threadID string, owner string) []map[string]any {
	return []map[string]any{{
		"session_id": threadID, "native_session_id": threadID, "transcript_id": threadID, "codex_thread_id": threadID,
		"live": false, "status": "completed", "desktop_owner_client_id": owner,
	}}
}

func attachableDesktop(threadID string, owner string) (*fakeDesktopClient, func(string) error) {
	desktop := &fakeDesktopClient{}
	return desktop, func(opened string) error {
		desktop.snapshots = desktopOwnerSnapshot(firstNonEmpty(opened, threadID), owner)
		return nil
	}
}

func TestCodexOpenSessionThenSendTurnViaAppServer(t *testing.T) {
	fc := newFakeCodexClient()
	fc.startID = fakeCodexThreadID
	c := testCodexWithClient(t, fc)
	tid, err := c.OpenOrCreateSession("s1", StartOptions{Cwd: "/work", Mode: "plan"})
	if err != nil {
		t.Fatal(err)
	}
	if tid != fakeCodexThreadID || c.threads["s1"] != fakeCodexThreadID {
		t.Fatalf("bad thread mapping: %s %#v", tid, c.threads)
	}
	res := c.SendPrompt("s1", "hello codex")
	if !res.OK || res.NativeTaskID != fakeCodexThreadID || res.Message != "turn started via Codex app-server" {
		t.Fatalf("bad send: %#v", res)
	}
	if len(fc.turns) != 1 || fc.turns[0] != [2]string{fakeCodexThreadID, "hello codex"} {
		t.Fatalf("app-server TurnStart not used: %#v", fc.turns)
	}
}

func TestCodexSendPromptLazilyAttachesDesktopOwner(t *testing.T) {
	fc := newFakeCodexClient()
	c := testCodexWithClient(t, fc)
	c.BindDesktopTranscript("s1", fakeCodexThreadID)
	desktop, opener := attachableDesktop(fakeCodexThreadID, "owner-1")
	c.desktopFactory = func() codexDesktopClient { return desktop }
	c.desktopOpener = opener
	res := c.SendPrompt("s1", "owner unknown")
	if !res.OK || res.Message != "turn started via Codex Desktop" {
		t.Fatalf("expected lazy Desktop delivery: %#v", res)
	}
	if len(fc.turns) != 0 {
		t.Fatalf("native Desktop thread must not fall back to app-server: %#v", fc.turns)
	}
	if len(desktop.targetStarts) != 1 || desktop.targetStarts[0] != [3]string{fakeCodexThreadID, "owner unknown", "owner-1"} {
		t.Fatalf("Desktop owner was not targeted after lazy attach: %#v", desktop.targetStarts)
	}
}

func TestCodexSendPromptDoesNotFallbackWhenDesktopAttachFails(t *testing.T) {
	fc := newFakeCodexClient()
	c := testCodexWithClient(t, fc)
	c.BindDesktopTranscript("s1", fakeCodexThreadID)
	c.desktopFactory = func() codexDesktopClient { return &fakeDesktopClient{} }
	c.desktopOpener = func(string) error { return errors.New("desktop unavailable") }
	res := c.SendPrompt("s1", "must stay pending")
	if res.OK || res.Error == nil || !stringsContains(*res.Error, "desktop unavailable") {
		t.Fatalf("expected Desktop attach error: %#v", res)
	}
	if len(fc.turns) != 0 {
		t.Fatalf("failed Desktop attach must not fall back to app-server: %#v", fc.turns)
	}
}

func TestCodexSendClearsStaleDesktopOwnerOnNoClientFound(t *testing.T) {
	fc := newFakeCodexClient()
	c := testCodexWithClient(t, fc)
	c.BindDesktopTranscript("s1", fakeCodexThreadID)
	c.noteDesktopOwnerClients([]map[string]any{{
		"transcript_id": fakeCodexThreadID, "desktop_owner_client_id": "stale-owner",
	}})
	desktop := &fakeDesktopClient{
		missingTargets: map[string]bool{"stale-owner": true},
		snapshots:      desktopOwnerSnapshot(fakeCodexThreadID, "owner-2"),
	}
	c.desktopFactory = func() codexDesktopClient { return desktop }
	turnID, err := c.tryDesktopStartTurn("s1", fakeCodexThreadID, "continue")
	if err != nil || turnID != "desktop-turn" {
		t.Fatalf("send failed after stale owner retry: turn=%q err=%v", turnID, err)
	}
	if len(desktop.targetStarts) != 2 {
		t.Fatalf("expected targeted attempt plus retry: %#v", desktop.targetStarts)
	}
	if desktop.targetStarts[0] != [3]string{fakeCodexThreadID, "continue", "stale-owner"} ||
		desktop.targetStarts[1] != [3]string{fakeCodexThreadID, "continue", "owner-2"} {
		t.Fatalf("bad target retry sequence: %#v", desktop.targetStarts)
	}
	if got := c.desktopOwnerClient(fakeCodexThreadID); got != "owner-2" {
		t.Fatalf("owner should refresh to owner-2, got %q", got)
	}
}

func TestCodexSendRejectsWhenDesktopOwnerDisappears(t *testing.T) {
	fc := newFakeCodexClient()
	c := testCodexWithClient(t, fc)
	c.BindDesktopTranscript("s1", fakeCodexThreadID)
	c.noteDesktopOwnerClients([]map[string]any{{
		"transcript_id": fakeCodexThreadID, "desktop_owner_client_id": "stale-owner",
	}})
	desktop := &fakeDesktopClient{
		missingTargets: map[string]bool{"stale-owner": true},
		snapshots:      []map[string]any{},
	}
	c.desktopFactory = func() codexDesktopClient { return desktop }
	turnID, err := c.tryDesktopStartTurn("s1", fakeCodexThreadID, "continue")
	if err == nil || !stringsContains(err.Error(), "no active Desktop IPC owner") || turnID != "" {
		t.Fatalf("expected missing Desktop owner after refresh: turn=%q err=%v", turnID, err)
	}
	if len(desktop.targetStarts) != 1 || desktop.targetStarts[0] != [3]string{fakeCodexThreadID, "continue", "stale-owner"} {
		t.Fatalf("should only attempt the stale targeted owner: %#v", desktop.targetStarts)
	}
}

func TestCodexSendReattachesAfterDesktopOwnerDisappears(t *testing.T) {
	fc := newFakeCodexClient()
	c := testCodexWithClient(t, fc)
	c.BindDesktopTranscript("s1", fakeCodexThreadID)
	c.noteDesktopOwnerClients([]map[string]any{{
		"transcript_id": fakeCodexThreadID, "desktop_owner_client_id": "stale-owner",
	}})
	desktop := &fakeDesktopClient{
		missingTargets: map[string]bool{"stale-owner": true},
		snapshots:      []map[string]any{},
	}
	c.desktopFactory = func() codexDesktopClient { return desktop }
	c.desktopOpener = func(string) error {
		desktop.snapshots = desktopOwnerSnapshot(fakeCodexThreadID, "owner-2")
		return nil
	}
	res := c.SendPrompt("s1", "continue safely")
	if !res.OK || res.Message != "turn started via Codex Desktop" {
		t.Fatalf("expected Desktop reattach after no-client-found: %#v", res)
	}
	if len(desktop.targetStarts) != 2 || desktop.targetStarts[1][2] != "owner-2" || len(fc.turns) != 0 {
		t.Fatalf("bad Desktop/app-server sequence desktop=%#v app=%#v", desktop.targetStarts, fc.turns)
	}
}

func TestCodexResumeDoesNotSendWithoutDesktopOwner(t *testing.T) {
	fc := newFakeCodexClient()
	c := testCodexWithClient(t, fc)
	c.desktopAttachTimeout = time.Millisecond
	c.desktopOpener = func(string) error { return nil }
	c.desktopFactory = func() codexDesktopClient { return &fakeDesktopClient{} }
	tid, err := c.OpenResumeSession("s1", fakeCodexThreadID, "/repo", false)
	if err != nil || tid != fakeCodexThreadID {
		t.Fatalf("resume should remain usable through app-server: tid=%q err=%v", tid, err)
	}
	res := c.SendPrompt("s1", "from pwa")
	if res.OK || res.Error == nil || !stringsContains(*res.Error, "no active Desktop IPC owner") {
		t.Fatalf("resumed native thread must wait for a Desktop owner: %#v", res)
	}
	if len(fc.turns) != 0 {
		t.Fatalf("resumed native thread must not fall back to app-server: %#v", fc.turns)
	}
}

func TestCodexRewindUserMessageRollsBackTurnsAndStartsViaDesktop(t *testing.T) {
	fc := newFakeCodexClient()
	fc.resumeResult = map[string]any{"thread": map[string]any{
		"id": fakeCodexThreadID,
		"turns": []any{
			map[string]any{"id": "turn-a", "items": []any{map[string]any{"type": "userMessage", "content": []any{map[string]any{"type": "text", "text": "first"}}}}},
			map[string]any{"id": "turn-b", "items": []any{map[string]any{"type": "userMessage", "content": []any{map[string]any{"type": "text", "text": "second"}}}}},
			map[string]any{"id": "turn-c", "items": []any{map[string]any{"type": "userMessage", "content": []any{map[string]any{"type": "text", "text": "third"}}}}},
		},
	}}
	c := testCodexWithClient(t, fc)
	desktop := &fakeDesktopClient{snapshots: desktopOwnerSnapshot(fakeCodexThreadID, "owner-1")}
	c.desktopFactory = func() codexDesktopClient { return desktop }
	res, err := c.RewindUserMessage(RewindUserMessageOptions{
		SessionID: "logical-rewind",
		ThreadID:  fakeCodexThreadID,
		TurnID:    "turn-b",
		Prompt:    "edited second",
		Cwd:       "/repo",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ThreadID != fakeCodexThreadID || res.State != "running" || res.TurnID != "desktop-turn" {
		t.Fatalf("bad rewind result: %#v", res)
	}
	if len(fc.rollbacks) != 1 || fc.rollbacks[0] != [2]any{fakeCodexThreadID, 2} {
		t.Fatalf("bad rollback calls: %#v", fc.rollbacks)
	}
	if len(desktop.starts) != 1 || desktop.starts[0] != [2]string{fakeCodexThreadID, "edited second"} {
		t.Fatalf("bad desktop calls: %#v", desktop.starts)
	}
	if c.threads["logical-rewind"] != fakeCodexThreadID || !c.desktopSyncSessions["logical-rewind"] {
		t.Fatalf("rewind session not bound to Desktop IPC: threads=%#v sync=%#v", c.threads, c.desktopSyncSessions)
	}
}

func TestCodexLiveTurnGuard(t *testing.T) {
	fc := newFakeCodexClient()
	c := testCodexWithClient(t, fc)
	c.BindTranscript("s1", fakeCodexThreadID)
	c.desktopFactory = func() codexDesktopClient {
		return &fakeDesktopClient{snapshots: []map[string]any{{
			"transcript_id": fakeCodexThreadID, "live": true, "status": "active",
		}}}
	}
	res := c.SendPrompt("s1", "second")
	if res.OK || res.Error == nil || !stringsContains(*res.Error, "in progress") {
		t.Fatalf("expected live-turn rejection: %#v", res)
	}
}

func TestCodexRuntimeSessionsTrackActiveTurns(t *testing.T) {
	fc := newFakeCodexClient()
	c := testCodexWithClient(t, fc)
	c.BindTranscript("s1", fakeCodexThreadID)
	desktop := &fakeDesktopClient{snapshots: desktopOwnerSnapshot(fakeCodexThreadID, "owner-1")}
	c.desktopFactory = func() codexDesktopClient { return desktop }
	res := c.SendPrompt("s1", "work")
	if !res.OK {
		t.Fatalf("send failed: %#v", res)
	}
	desktop.snapshots = nil
	rows := c.RuntimeSessions()
	if len(rows) != 1 {
		t.Fatalf("runtime rows=%#v", rows)
	}
	row := rows[0]
	if row["session_id"] != "s1" || row["native_session_id"] != fakeCodexThreadID || row["transcript_id"] != fakeCodexThreadID {
		t.Fatalf("bad runtime row: %#v", row)
	}
	if row["live"] != true || row["state"] != "running" || row["status"] != "active" {
		t.Fatalf("bad runtime state: %#v", row)
	}
	c.onNotification("thread/status/changed", map[string]any{"threadId": fakeCodexThreadID, "status": map[string]any{"type": "idle"}})
	if rows := c.RuntimeSessions(); len(rows) != 0 {
		t.Fatalf("runtime rows after idle=%#v", rows)
	}
}

func TestCodexRuntimeSessionsIncludeDesktopSnapshots(t *testing.T) {
	fc := newFakeCodexClient()
	c := testCodexWithClient(t, fc)
	desktop := &fakeDesktopClient{snapshots: []map[string]any{{
		"session_id": "thread-2", "transcript_id": "thread-2", "title": "Desktop live",
		"live": true, "status": "inProgress", "updated_at": "2026-06-24T10:00:00Z",
		"desktop_owner_client_id": "owner-1",
	}}}
	c.desktopFactory = func() codexDesktopClient { return desktop }
	rows := c.RuntimeSessions()
	if len(rows) != 1 {
		t.Fatalf("runtime rows=%#v", rows)
	}
	if rows[0]["transcript_id"] != "thread-2" || rows[0]["title"] != "Desktop live" || rows[0]["provider_id"] != "codex" {
		t.Fatalf("bad desktop row: %#v", rows[0])
	}
	if got := c.desktopOwnerClient("thread-2"); got != "owner-1" {
		t.Fatalf("desktop owner not cached: %q", got)
	}
}

func TestCodexSendPromptTargetsDesktopOwnerFromSnapshot(t *testing.T) {
	fc := newFakeCodexClient()
	c := testCodexWithClient(t, fc)
	c.BindDesktopTranscript("s1", fakeCodexThreadID)
	desktop := &fakeDesktopClient{snapshots: []map[string]any{{
		"session_id": fakeCodexThreadID, "transcript_id": fakeCodexThreadID,
		"live": false, "status": "completed", "desktop_owner_client_id": "owner-1",
	}}}
	c.desktopFactory = func() codexDesktopClient { return desktop }
	res := c.SendPrompt("s1", "hello owner")
	if !res.OK {
		t.Fatalf("send failed: %#v", res)
	}
	if len(desktop.targetStarts) != 1 || desktop.targetStarts[0] != [3]string{fakeCodexThreadID, "hello owner", "owner-1"} {
		t.Fatalf("bad targeted start: %#v", desktop.targetStarts)
	}
}

func TestCodexSessionRunningClearsStaleDesktopActive(t *testing.T) {
	fc := newFakeCodexClient()
	c := testCodexWithClient(t, fc)
	tid := "019ecffb-fd7d-7422-b05c-2e1c3ebec53d"
	c.threads["s1"] = tid
	c.setThreadActive(tid, true)
	desktop := &fakeDesktopClient{snapshots: []map[string]any{}}
	c.desktopFactory = func() codexDesktopClient { return desktop }

	running := c.SessionRunning("s1")
	if running == nil || *running {
		t.Fatalf("expected desktop snapshot to clear stale active, got %#v", running)
	}
	if c.threadActive(tid) {
		t.Fatalf("stale active flag not cleared")
	}
}

func TestCodexResumeForkAndDesktopSync(t *testing.T) {
	fc := newFakeCodexClient()
	c := testCodexWithClient(t, fc)
	tid := "019ecffb-fd7d-7422-b05c-2e1c3ebec53d"
	desktop, opener := attachableDesktop(tid, "owner-1")
	c.desktopFactory = func() codexDesktopClient { return desktop }
	c.desktopOpener = opener
	got, err := c.OpenResumeSession("s1", tid, "/repo", false)
	if err != nil || got != tid {
		t.Fatalf("resume: %s %v", got, err)
	}
	if !c.desktopSyncSessions["s1"] {
		t.Fatalf("desktop sync not marked")
	}
	res := c.SendPrompt("s1", "from web")
	if !res.OK || res.Message != "turn started via Codex Desktop" {
		t.Fatalf("desktop send not used: %#v", res)
	}
	if len(fc.turns) != 0 || desktop.starts[0] != [2]string{tid, "from web"} {
		t.Fatalf("bad desktop/app-server calls desktop=%#v app=%#v", desktop.starts, fc.turns)
	}
	if rows := c.RuntimeSessions(); len(rows) != 1 || rows[0]["native_session_id"] != tid {
		t.Fatalf("desktop runtime not tracked: %#v", rows)
	}
	fork, err := c.OpenResumeSession("s2", tid, "", true)
	if err != nil || fork != "fork-1" {
		t.Fatalf("fork: %s %v", fork, err)
	}
}

func TestCodexBindDesktopTranscriptRestoresDesktopSyncAfterRestart(t *testing.T) {
	fc := newFakeCodexClient()
	c := testCodexWithClient(t, fc)
	tid := "019ecffb-fd7d-7422-b05c-2e1c3ebec53d"
	c.BindDesktopTranscript("s1", tid)
	if c.threads["s1"] != tid {
		t.Fatalf("thread binding not restored: %#v", c.threads)
	}
	if !c.desktopSyncSessions["s1"] {
		t.Fatalf("desktop sync not restored")
	}
	desktop := &fakeDesktopClient{snapshots: desktopOwnerSnapshot(tid, "owner-1")}
	c.desktopFactory = func() codexDesktopClient { return desktop }
	res := c.SendPrompt("s1", "after restart")
	if !res.OK || res.Message != "turn started via Codex Desktop" || res.NativeTaskID != tid {
		t.Fatalf("desktop send not used after bind: %#v", res)
	}
	if len(fc.turns) != 0 {
		t.Fatalf("app-server fallback should not start a new turn: %#v", fc.turns)
	}
	if len(desktop.starts) != 1 || desktop.starts[0] != [2]string{tid, "after restart"} {
		t.Fatalf("wrong desktop start: %#v", desktop.starts)
	}
}

func TestCodexSendPromptUsesAppServerForBoundThreadWithoutDesktopSync(t *testing.T) {
	fc := newFakeCodexClient()
	c := testCodexWithClient(t, fc)
	c.threads["s1"] = fakeCodexThreadID
	res := c.SendPrompt("s1", "fallback")
	if !res.OK || res.Message != "turn started via Codex app-server" {
		t.Fatalf("expected app-server send: %#v", res)
	}
	if len(fc.turns) != 1 || fc.turns[0] != [2]string{fakeCodexThreadID, "fallback"} {
		t.Fatalf("app-server TurnStart must be used: %#v", fc.turns)
	}
}

func TestCodexSendPromptDoesNotFallbackOnDesktopIPCError(t *testing.T) {
	fc := newFakeCodexClient()
	c := testCodexWithClient(t, fc)
	c.BindDesktopTranscript("s1", fakeCodexThreadID)
	c.noteDesktopOwnerClients([]map[string]any{{
		"transcript_id": fakeCodexThreadID, "desktop_owner_client_id": "owner-1",
	}})
	c.desktopFactory = func() codexDesktopClient {
		return &fakeDesktopClient{err: CodexDesktopIPCError{Message: "socket not found"}}
	}
	res := c.SendPrompt("s1", "must not fallback")
	if res.OK || res.Error == nil || !stringsContains(*res.Error, "socket not found") {
		t.Fatalf("expected desktop IPC error: %#v", res)
	}
	if len(fc.turns) != 0 {
		t.Fatalf("app-server TurnStart must not be used: %#v", fc.turns)
	}
}

func TestDesktopSnapshotLiveRows(t *testing.T) {
	msg := map[string]any{
		"type":           "broadcast",
		"method":         "thread-stream-state-changed",
		"sourceClientId": "owner-1",
		"params": map[string]any{
			"conversationId": "thread-1",
			"change": map[string]any{"conversationState": map[string]any{
				"id": "thread-1",
				"turns": []any{map[string]any{
					"status":          "inProgress",
					"turnStartedAtMs": float64(1782295200000),
					"params": map[string]any{
						"cwd":   "/repo",
						"input": []any{map[string]any{"type": "text", "text": "hello desktop"}},
					},
				}},
			}},
		},
	}
	rows := desktopSnapshotLiveRows(msg)
	if len(rows) != 1 {
		t.Fatalf("snapshot rows=%#v", rows)
	}
	row := rows[0]
	if row["transcript_id"] != "thread-1" || row["status"] != "inProgress" || row["cwd"] != "/repo" {
		t.Fatalf("bad snapshot row: %#v", row)
	}
	if row["live"] != true || row["state"] != "running" || row["desktop_owner_client_id"] != "owner-1" {
		t.Fatalf("bad snapshot owner/state: %#v", row)
	}
	if row["title"] != "hello desktop" || row["updated_at"] != "2026-06-24T10:00:00Z" {
		t.Fatalf("bad snapshot metadata: %#v", row)
	}
}

func TestDesktopIPCRequestRespondsToClientDiscovery(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()
	client := &CodexDesktopIPCClient{Timeout: time.Second, ClientType: "remote-agent", HostID: "local", clientID: "remote-client"}
	done := make(chan any, 1)
	go func() {
		res, err := client.requestOnConn(clientConn, "thread-follower-start-turn", map[string]any{
			"conversationId": fakeCodexThreadID,
		}, time.Second, "", "")
		if err != nil {
			done <- err
			return
		}
		done <- res
	}()
	req, err := readDesktopFrame(serverConn)
	if err != nil {
		t.Fatal(err)
	}
	rid := stringAny(req["requestId"])
	if req["method"] != "thread-follower-start-turn" || rid == "" {
		t.Fatalf("bad request frame: %#v", req)
	}
	if err := writeDesktopFrame(serverConn, map[string]any{
		"type":      "client-discovery-request",
		"requestId": "discover-1",
		"request":   map[string]any{"method": "ide-context", "version": 0},
	}); err != nil {
		t.Fatal(err)
	}
	disc, err := readDesktopFrame(serverConn)
	if err != nil {
		t.Fatal(err)
	}
	if disc["type"] != "client-discovery-response" || disc["requestId"] != "discover-1" || mapAny(disc["response"])["canHandle"] != false {
		t.Fatalf("bad discovery response: %#v", disc)
	}
	if err := writeDesktopFrame(serverConn, map[string]any{
		"type":       "response",
		"requestId":  rid,
		"resultType": "success",
		"result":     map[string]any{"ok": true},
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if err, ok := got.(error); ok {
			t.Fatal(err)
		}
		if mapAny(got)["ok"] != true {
			t.Fatalf("bad result: %#v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("request timed out")
	}
}

func TestDesktopIPCStartTurnAcceptsRunningBroadcast(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()
	client := &CodexDesktopIPCClient{Timeout: time.Second, ClientType: "remote-agent", HostID: "local", clientID: "remote-client"}
	done := make(chan any, 1)
	go func() {
		res, err := client.requestOnConn(clientConn, "thread-follower-start-turn", map[string]any{
			"conversationId": fakeCodexThreadID,
		}, time.Second, "", "owner-1")
		if err != nil {
			done <- err
			return
		}
		done <- res
	}()
	req, err := readDesktopFrame(serverConn)
	if err != nil {
		t.Fatal(err)
	}
	if req["method"] != "thread-follower-start-turn" || req["targetClientId"] != "owner-1" {
		t.Fatalf("bad request frame: %#v", req)
	}
	if err := writeDesktopFrame(serverConn, map[string]any{
		"type":           "broadcast",
		"method":         "thread-stream-state-changed",
		"sourceClientId": "owner-1",
		"params": map[string]any{
			"conversationId": fakeCodexThreadID,
			"change": map[string]any{"conversationState": map[string]any{
				"id": fakeCodexThreadID,
				"turns": []any{map[string]any{
					"status":          "inProgress",
					"turnStartedAtMs": float64(1782295200000),
				}},
			}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if err, ok := got.(error); ok {
			t.Fatal(err)
		}
		if mapAny(got)["accepted"] != true {
			t.Fatalf("bad result: %#v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("request timed out")
	}
}

func TestCodexThreadListAndMessages(t *testing.T) {
	fc := newFakeCodexClient()
	fc.listResult = map[string]any{"data": []any{map[string]any{
		"id": "thread-1", "name": "App thread", "cwd": "/repo", "updatedAt": float64(1782267600),
	}}}
	fc.resumeResult = map[string]any{"thread": map[string]any{
		"id": "thread-1", "cwd": "/repo", "turns": []any{map[string]any{"items": []any{
			map[string]any{"type": "userMessage", "content": []any{map[string]any{"type": "text", "text": "hello"}}},
			map[string]any{"type": "agentMessage", "text": "hi"},
			map[string]any{"type": "functionCall", "id": "call-1", "name": "exec_command", "arguments": map[string]any{"command": "pwd"}},
			map[string]any{"type": "functionCallOutput", "callId": "call-1", "output": map[string]any{"stdout": "/repo\n"}},
		}}}},
	}
	c := testCodexWithClient(t, fc)
	sessions := c.ListNativeSessions()
	if len(sessions) != 1 || sessions[0]["native_session_id"] != "thread-1" || sessions[0]["source"] != codexAppServerSource {
		t.Fatalf("bad sessions: %#v", sessions)
	}
	msgs, err := c.SessionMessages("thread-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 3 {
		t.Fatalf("bad msgs: %#v", msgs)
	}
	tool := findKind(msgs, "tool")
	if tool == nil || tool["result"] != "/repo\n" {
		t.Fatalf("bad tool: %#v all=%#v", tool, msgs)
	}
}

func TestCodexThreadListMergesLocalRollouts(t *testing.T) {
	fc := newFakeCodexClient()
	appID := "019ef111-1111-7111-8111-111111111111"
	rolloutOnlyID := "019ef222-2222-7222-8222-222222222222"
	fc.listResult = map[string]any{"data": []any{map[string]any{
		"id": appID, "name": "App thread", "cwd": "/repo/app", "updatedAt": float64(1782267600),
	}}}
	c := testCodexWithClient(t, fc)
	sessionDirs := stringSliceExtra(c.cfg.Extra, "codex_sessions_dirs", nil)
	writeJSONL(t, filepath.Join(sessionDirs[0], "rollout-2026-07-14T20-15-23-"+rolloutOnlyID+".jsonl"), []map[string]any{
		{"timestamp": "2026-07-14T12:15:23Z", "type": "session_meta", "payload": map[string]any{"cwd": "/repo/current"}},
	})

	sessions := c.ListNativeSessions()
	if len(sessions) != 2 {
		t.Fatalf("local rollout was dropped after successful thread/list: %#v", sessions)
	}
	byID := map[string]map[string]any{}
	for _, row := range sessions {
		byID[stringAny(row["native_session_id"])] = row
	}
	if byID[appID]["source"] != codexAppServerSource {
		t.Fatalf("app-server row lost precedence: %#v", byID[appID])
	}
	if byID[rolloutOnlyID]["cwd"] != "/repo/current" || byID[rolloutOnlyID]["source"] != "codex" {
		t.Fatalf("rollout-only session missing: %#v", byID[rolloutOnlyID])
	}
}

func TestCodexSessionMessagesPrefersBoundLocalRollout(t *testing.T) {
	dir := t.TempDir()
	threadID := "01800000-0000-7000-8000-000000000001"
	rollout := filepath.Join(dir, "rollout-2026-07-13T14-19-00-"+threadID+".jsonl")
	record := `{"timestamp":"2026-07-13T14:19:00Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ready"}]}}` + "\n"
	if err := os.WriteFile(rollout, []byte(record), 0o600); err != nil {
		t.Fatal(err)
	}
	fc := newFakeCodexClient()
	c := NewCodex("codex", config.ProviderConfig{
		Command: "codex", Cwd: "/tmp",
		Extra: map[string]any{"codex_sessions_dirs": []any{dir}},
	})
	c.clientFactory = func(func(string, map[string]any), func(any, string, map[string]any)) codexAppClient { return fc }
	c.desktopFactory = func() codexDesktopClient { return &fakeDesktopClient{} }
	c.BindTranscript("logical-session", threadID)

	msgs, err := c.SessionMessages("logical-session")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0]["text"] != "ready" {
		t.Fatalf("local rollout messages = %#v", msgs)
	}
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if len(fc.resumes) != 0 {
		t.Fatalf("local preview unexpectedly resumed app-server thread: %#v", fc.resumes)
	}
}

func TestCodexSessionMessagesReturnsFullHistoryForStablePreviewOffsets(t *testing.T) {
	dir := t.TempDir()
	threadID := "01800000-0000-7000-8000-000000000002"
	records := make([]map[string]any, nativePreviewMaxItems+3)
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
	writeJSONL(t, filepath.Join(dir, "rollout-2026-07-15T00-00-00-"+threadID+".jsonl"), records)
	c := NewCodex("codex", config.ProviderConfig{
		Command: "codex", Cwd: "/tmp",
		Extra: map[string]any{"codex_sessions_dirs": []any{dir}},
	})

	msgs, err := c.SessionMessages(threadID)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != len(records) {
		t.Fatalf("SessionMessages len=%d want=%d; capped history makes preview offsets slide", len(msgs), len(records))
	}
}

func TestCodexSessionMessagesFallbackReturnsFullAppServerHistory(t *testing.T) {
	items := make([]any, nativePreviewMaxItems+2)
	for i := range items {
		items[i] = map[string]any{"type": "agentMessage", "text": fmt.Sprintf("message-%d", i)}
	}
	fc := newFakeCodexClient()
	fc.resumeResult = map[string]any{"thread": map[string]any{
		"id": "thread-without-rollout", "turns": []any{map[string]any{"items": items}},
	}}
	c := testCodexWithClient(t, fc)

	msgs, err := c.SessionMessages("thread-without-rollout")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != len(items) {
		t.Fatalf("app-server fallback len=%d want=%d; capped history makes preview offsets slide", len(msgs), len(items))
	}
}

func TestCodexThreadListSortsByLastReplyAt(t *testing.T) {
	rows := codexThreadListToSessions(map[string]any{"data": []any{
		map[string]any{
			"id": "updated-newer", "name": "updated newer", "updatedAt": float64(300),
			"turns": []any{map[string]any{
				"completedAt": float64(100),
				"items":       []any{map[string]any{"type": "agentMessage", "text": "old reply"}},
			}},
		},
		map[string]any{
			"id": "reply-newer", "name": "reply newer", "updatedAt": float64(200),
			"turns": []any{map[string]any{
				"completedAt": float64(400),
				"items":       []any{map[string]any{"type": "agentMessage", "text": "new reply"}},
			}},
		},
	}})
	if len(rows) != 2 {
		t.Fatalf("bad rows: %#v", rows)
	}
	if rows[0]["native_session_id"] != "reply-newer" {
		t.Fatalf("expected latest reply first: %#v", rows)
	}
	if rows[0]["last_reply_at"] == "" {
		t.Fatalf("missing last_reply_at: %#v", rows[0])
	}
}

func TestCodexThreadListMarksActiveStatus(t *testing.T) {
	fc := newFakeCodexClient()
	fc.listResult = map[string]any{"data": []any{map[string]any{
		"id": "thread-1", "name": "Running", "cwd": "/repo", "status": map[string]any{"type": "active"},
	}}}
	c := testCodexWithClient(t, fc)
	sessions := c.ListNativeSessions()
	if len(sessions) != 1 {
		t.Fatalf("bad sessions: %#v", sessions)
	}
	if sessions[0]["status"] != "active" || sessions[0]["live"] != true || sessions[0]["state"] != "running" {
		t.Fatalf("active status not merged: %#v", sessions[0])
	}
	rows := c.RuntimeSessions()
	if len(rows) != 1 || rows[0]["session_id"] != "thread-1" || rows[0]["transcript_id"] != "thread-1" {
		t.Fatalf("bad runtime rows: %#v", rows)
	}
}
