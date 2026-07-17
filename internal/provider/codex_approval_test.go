package provider

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/psyche08/remote-agent/internal/config"
)

const (
	approvalThreadA = "019f0000-0000-7000-8000-00000000000a"
	approvalThreadB = "019f0000-0000-7000-8000-00000000000b"
)

func commandApprovalParams(threadID string, extra map[string]any) map[string]any {
	params := map[string]any{
		"threadId": threadID, "turnId": "turn-a", "itemId": "exec-1",
		"command": "touch x", "cwd": "/tmp", "startedAtMs": float64(1700000000000),
	}
	for k, v := range extra {
		params[k] = v
	}
	return params
}

// Requirement: thread A's pending approval must survive thread B going idle.
func TestCodexApprovalSurvivesOtherThreadIdle(t *testing.T) {
	fc := newFakeCodexClient()
	c := testCodexWithClient(t, fc)
	c.BindTranscript("sess-a", approvalThreadA)
	c.BindTranscript("sess-b", approvalThreadB)

	c.onServerRequest(float64(1), "item/commandExecution/requestApproval", commandApprovalParams(approvalThreadA, nil))
	c.onServerRequest(float64(2), "item/commandExecution/requestApproval", commandApprovalParams(approvalThreadB, map[string]any{"itemId": "exec-2"}))

	// Thread B goes idle: only B's approval may be dropped.
	c.onNotification("thread/status/changed", map[string]any{
		"threadId": approvalThreadB, "status": map[string]any{"type": "idle"},
	})

	if got := c.DetectState("sess-a"); got != "waiting_approval" {
		t.Fatalf("thread A approval was lost: state=%q", got)
	}
	if got := c.DetectState("sess-b"); got == "waiting_approval" {
		t.Fatalf("thread B approval should be cleared after idle")
	}
	ar := c.ApprovalRequest("sess-a")
	if ar == nil || ar["request_id"] != "1" || ar["thread_id"] != approvalThreadA {
		t.Fatalf("bad approval view for thread A: %#v", ar)
	}
}

// Requirement: legacy execCommandApproval/applyPatchApproval use
// conversationId and the legacy ReviewDecision response shape.
func TestCodexLegacyConversationIDRoutingAndResponse(t *testing.T) {
	fc := newFakeCodexClient()
	c := testCodexWithClient(t, fc)
	c.BindTranscript("sess-a", approvalThreadA)

	c.onServerRequest(float64(7), "execCommandApproval", map[string]any{
		"conversationId": approvalThreadA, "callId": "call-1",
		"command": []any{"touch", "x"}, "cwd": "/tmp",
	})
	ar := c.ApprovalRequest("sess-a")
	if ar == nil || ar["request_id"] != "7" || ar["thread_id"] != approvalThreadA || ar["type"] != "command" {
		t.Fatalf("legacy approval not routed by conversationId: %#v", ar)
	}
	res := c.RelayApprovalRequest("sess-a", "7", "allow")
	if !boolAny(res["ok"]) {
		t.Fatalf("legacy allow failed: %#v", res)
	}
	if len(fc.responses) != 1 || fc.responses[0]["decision"] != "approved" {
		t.Fatalf("legacy allow must send decision=approved: %#v", fc.responses)
	}

	c.onServerRequest(float64(8), "applyPatchApproval", map[string]any{
		"conversation_id": approvalThreadA, "callId": "call-2", "fileChanges": map[string]any{},
	})
	res = c.RelayApprovalRequest("sess-a", "8", "deny")
	if !boolAny(res["ok"]) {
		t.Fatalf("legacy deny failed: %#v", res)
	}
	if len(fc.responses) != 2 || fc.responses[1]["decision"] != "denied" {
		t.Fatalf("legacy deny must send decision=denied: %#v", fc.responses)
	}
}

// Requirement: request_id selects the exact approval; duplicates/stale ids
// are rejected without side effects.
func TestCodexRequestScopedApprovalAndStale(t *testing.T) {
	fc := newFakeCodexClient()
	c := testCodexWithClient(t, fc)
	c.BindTranscript("sess-a", approvalThreadA)

	c.onServerRequest(float64(11), "item/commandExecution/requestApproval", commandApprovalParams(approvalThreadA, map[string]any{"itemId": "exec-11"}))
	c.onServerRequest(float64(12), "item/commandExecution/requestApproval", commandApprovalParams(approvalThreadA, map[string]any{"itemId": "exec-12"}))

	res := c.RelayApprovalRequest("sess-a", "12", "allow")
	if !boolAny(res["ok"]) || res["request_id"] != "12" {
		t.Fatalf("targeted approval failed: %#v", res)
	}
	if len(fc.respondedIDs) != 1 || canonicalRequestKey(fc.respondedIDs[0]) != "12" {
		t.Fatalf("responded to wrong request id: %#v", fc.respondedIDs)
	}
	// The other approval must still be pending (serial/parallel callbacks on
	// one thread are supported).
	if ar := c.ApprovalRequest("sess-a"); ar == nil || ar["request_id"] != "11" {
		t.Fatalf("first approval lost after answering second: %#v", ar)
	}
	// Duplicate response to the already-answered request: explicit stale.
	res = c.RelayApprovalRequest("sess-a", "12", "deny")
	if boolAny(res["ok"]) || res["status"] != "stale" {
		t.Fatalf("duplicate response must be stale: %#v", res)
	}
	if len(fc.responses) != 1 {
		t.Fatalf("stale response must not reach the app-server: %#v", fc.responses)
	}
}

// Requirement: allow/deny wire shapes for the new protocol, including
// availableDecisions constraints.
func TestCodexApprovalResponseShapes(t *testing.T) {
	fc := newFakeCodexClient()
	c := testCodexWithClient(t, fc)
	c.BindTranscript("sess-a", approvalThreadA)

	c.onServerRequest(float64(21), "item/commandExecution/requestApproval", commandApprovalParams(approvalThreadA, nil))
	if res := c.RelayApprovalRequest("sess-a", "21", "allow"); !boolAny(res["ok"]) {
		t.Fatalf("allow failed: %#v", res)
	}
	if fc.responses[0]["decision"] != "accept" {
		t.Fatalf("command allow must be accept: %#v", fc.responses[0])
	}

	c.onServerRequest(float64(22), "item/fileChange/requestApproval", map[string]any{
		"threadId": approvalThreadA, "turnId": "turn-a", "itemId": "patch-1", "startedAtMs": float64(1),
	})
	if res := c.RelayApprovalRequest("sess-a", "22", "deny"); !boolAny(res["ok"]) {
		t.Fatalf("deny failed: %#v", res)
	}
	if fc.responses[1]["decision"] != "decline" {
		t.Fatalf("file deny must be decline: %#v", fc.responses[1])
	}

	// availableDecisions without "decline": deny falls back to cancel.
	c.onServerRequest(float64(23), "item/commandExecution/requestApproval", commandApprovalParams(approvalThreadA, map[string]any{
		"availableDecisions": []any{"accept", map[string]any{"acceptWithExecpolicyAmendment": map[string]any{}}, "cancel"},
	}))
	if res := c.RelayApprovalRequest("sess-a", "23", "deny"); !boolAny(res["ok"]) {
		t.Fatalf("constrained deny failed: %#v", res)
	}
	if fc.responses[2]["decision"] != "cancel" {
		t.Fatalf("deny without decline must cancel: %#v", fc.responses[2])
	}
}

// Requirement: permissions allow and deny must produce different responses.
func TestCodexPermissionsAllowDenyDiffer(t *testing.T) {
	requested := map[string]any{
		"fileSystem": map[string]any{"write": []any{"/tmp/x"}},
		"network":    map[string]any{"enabled": true},
	}
	allowBody, err := codexApprovalResponseBody("item/permissions/requestApproval", true, map[string]any{"permissions": requested})
	if err != nil {
		t.Fatal(err)
	}
	denyBody, err := codexApprovalResponseBody("item/permissions/requestApproval", false, map[string]any{"permissions": requested})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprintf("%v", allowBody) == fmt.Sprintf("%v", denyBody) {
		t.Fatalf("permissions allow and deny must differ: %#v", allowBody)
	}
	if granted := mapAny(allowBody["permissions"]); len(granted) == 0 || mapAny(granted["network"])["enabled"] != true {
		t.Fatalf("allow must grant the requested profile: %#v", allowBody)
	}
	if granted := mapAny(denyBody["permissions"]); len(granted) != 0 {
		t.Fatalf("deny must grant an empty profile: %#v", denyBody)
	}
}

// Requirement: machine requests (token refresh, attestation, clock) must not
// surface as approvals; they get an immediate JSON-RPC error instead.
func TestCodexMachineRequestsAreNotApprovals(t *testing.T) {
	fc := newFakeCodexClient()
	c := testCodexWithClient(t, fc)
	if _, err := c.ensureClient(); err != nil {
		t.Fatal(err)
	}
	c.BindTranscript("sess-a", approvalThreadA)
	for i, method := range []string{"account/chatgptAuthTokens/refresh", "attestation/generate", "currentTime/read", "fuzzyFileSearch/whatever"} {
		c.onServerRequest(float64(100+i), method, map[string]any{"threadId": approvalThreadA})
	}
	if got := c.DetectState("sess-a"); got == "waiting_approval" {
		t.Fatalf("machine requests misreported as approvals")
	}
	if len(fc.errorResponses) != 4 {
		t.Fatalf("machine requests must be answered with errors: %#v", fc.errorResponses)
	}
}

func bridgeSnapshotFrame(threadID string, owner string, rev int, state map[string]any) map[string]any {
	return map[string]any{
		"type": "broadcast", "method": "thread-stream-state-changed", "sourceClientId": owner,
		"params": map[string]any{
			"hostId": "local", "conversationId": threadID,
			"change": map[string]any{"type": "snapshot", "revision": float64(rev), "conversationState": state},
		},
	}
}

func bridgePatchesFrame(threadID string, owner string, baseRev int, rev int, patches []any) map[string]any {
	return map[string]any{
		"type": "broadcast", "method": "thread-stream-state-changed", "sourceClientId": owner,
		"params": map[string]any{
			"hostId": "local", "conversationId": threadID,
			"change": map[string]any{"type": "patches", "baseRevision": float64(baseRev), "revision": float64(rev), "patches": patches},
		},
	}
}

func TestCodexDesktopBridgePreservesSnapshotBeforeInitializeResponse(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	owner := "desktop-owner-before-init"
	done := make(chan error, 1)
	go func() {
		initFrame, err := readDesktopFrame(server)
		if err != nil {
			done <- err
			return
		}
		if err := writeDesktopFrame(server, bridgeSnapshotFrame(approvalThreadA, owner, 1, map[string]any{
			"id": approvalThreadA, "requests": []any{},
		})); err != nil {
			done <- err
			return
		}
		done <- writeDesktopFrame(server, map[string]any{
			"type": "response", "requestId": stringAny(initFrame["requestId"]), "resultType": "success",
			"method": "initialize", "result": map[string]any{"clientId": "remote-coding-client"},
		})
	}()

	clientID, pending, err := codexBridgeInitialize(client)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if clientID != "remote-coding-client" || len(pending) != 1 {
		t.Fatalf("client=%q pending=%d", clientID, len(pending))
	}
	b := newCodexDesktopBridge("", "local")
	b.handleFrame(client, pending[0])
	if got := b.OwnerClient(approvalThreadA); got != owner {
		t.Fatalf("owner=%q want=%q", got, owner)
	}
}

// Requirement: Desktop IPC broadcasts (snapshot + immer patches) carry the
// pending approval requests; the bridge mirrors their full lifecycle.
func TestCodexDesktopBridgeApprovalLifecycle(t *testing.T) {
	b := newCodexDesktopBridge("", "local")
	owner := "owner-1"
	changes := []string{}
	b.onHumanRequestsChanged = func(threadID string) { changes = append(changes, threadID) }
	b.HandleBroadcast(bridgeSnapshotFrame(approvalThreadA, owner, 1, map[string]any{
		"id": approvalThreadA, "requests": []any{},
		"threadRuntimeStatus": map[string]any{"type": "active"},
		"latestThreadSettings": map[string]any{
			"approvalPolicy": "on-request", "approvalsReviewer": "auto_review",
			"sandboxPolicy": map[string]any{"type": "workspaceWrite"},
			"model":         "gpt-5.2", "effort": "high",
		},
	}))
	if got := b.PendingHumanRequests(approvalThreadA); len(got) != 0 {
		t.Fatalf("no requests expected yet: %#v", got)
	}
	// Owner broadcasts an immer patch adding the approval request.
	b.HandleBroadcast(bridgePatchesFrame(approvalThreadA, owner, 1, 2, []any{
		map[string]any{"op": "add", "path": []any{"requests", float64(0)}, "value": map[string]any{
			"id": float64(47), "method": "item/commandExecution/requestApproval",
			"params": commandApprovalParams(approvalThreadA, nil),
		}},
	}))
	got := b.PendingHumanRequests(approvalThreadA)
	if len(got) != 1 || canonicalRequestKey(got[0].ID) != "47" || got[0].Method != "item/commandExecution/requestApproval" {
		t.Fatalf("approval request not mirrored from patches: %#v", got)
	}
	if b.OwnerClient(approvalThreadA) != owner {
		t.Fatalf("owner not tracked")
	}
	st := b.ThreadSettings(approvalThreadA)
	if stringAny(st["approval_policy"]) != "on-request" || stringAny(st["approvals_reviewer"]) != "auto_review" || stringAny(st["sandbox"]) != "workspace-write" {
		t.Fatalf("real thread settings not mirrored: %#v", st)
	}
	if running := b.ThreadRunning(approvalThreadA); running == nil || !*running {
		t.Fatalf("threadRuntimeStatus active must report running")
	}
	// Desktop (or auto_review) resolves the request: patch removes it.
	b.HandleBroadcast(bridgePatchesFrame(approvalThreadA, owner, 2, 3, []any{
		map[string]any{"op": "remove", "path": []any{"requests", float64(0)}},
	}))
	if got := b.PendingHumanRequests(approvalThreadA); len(got) != 0 {
		t.Fatalf("resolved approval must disappear: %#v", got)
	}
	if len(changes) != 2 || changes[0] != approvalThreadA || changes[1] != approvalThreadA {
		t.Fatalf("pending/resolved approval changes not published: %#v", changes)
	}
	// A patch on a lost revision marks the thread stale (no phantom state).
	b.HandleBroadcast(bridgePatchesFrame(approvalThreadA, owner, 99, 100, []any{
		map[string]any{"op": "replace", "path": []any{"requests"}, "value": []any{map[string]any{"id": float64(1), "method": "item/commandExecution/requestApproval", "params": map[string]any{}}}},
	}))
	if got := b.PendingHumanRequests(approvalThreadA); len(got) != 0 {
		t.Fatalf("stale thread must not report approvals: %#v", got)
	}
}

func TestCodexPreviewBindRefreshesPreexistingDesktopApproval(t *testing.T) {
	fc := newFakeCodexClient()
	c := testCodexWithClient(t, fc)
	b := newCodexDesktopBridge("", "local")
	c.bridge = b
	c.noteDesktopOwnerClients([]map[string]any{{
		"transcript_id": approvalThreadA, "desktop_owner_client_id": "owner-1",
	}})
	refreshed := make(chan struct{}, 1)
	b.resync = func(threadID string, owner string) {
		b.HandleBroadcast(bridgeSnapshotFrame(threadID, owner, 1, map[string]any{
			"id": threadID,
			"requests": []any{map[string]any{
				"id": float64(58), "method": "item/commandExecution/requestApproval",
				"params": commandApprovalParams(threadID, nil),
			}},
		}))
		refreshed <- struct{}{}
	}
	c.BindTranscript("sess-preview", approvalThreadA)
	select {
	case <-refreshed:
	case <-time.After(time.Second):
		t.Fatal("native-session preview bind did not refresh Desktop pending state")
	}
	if state := c.DetectState("sess-preview"); state != "waiting_approval" {
		t.Fatalf("pre-existing Desktop approval not restored: state=%q", state)
	}
	if ar := c.ApprovalRequest("sess-preview"); ar == nil || ar["request_id"] != "58" {
		t.Fatalf("bad restored Desktop approval: %#v", ar)
	}
}

// Requirement: Web allow/deny on a Desktop-owned approval goes through the
// thread-follower decision methods with the exact request id.
func TestCodexDesktopApprovalDecisionRouting(t *testing.T) {
	fc := newFakeCodexClient()
	c := testCodexWithClient(t, fc)
	desktop := &fakeDesktopClient{}
	c.desktopFactory = func() codexDesktopClient { return desktop }
	c.BindTranscript("sess-a", approvalThreadA)

	b := newCodexDesktopBridge("", "local")
	c.bridge = b
	owner := "owner-9"
	b.HandleBroadcast(bridgeSnapshotFrame(approvalThreadA, owner, 5, map[string]any{
		"id": approvalThreadA,
		"requests": []any{
			map[string]any{"id": float64(47), "method": "item/commandExecution/requestApproval",
				"params": commandApprovalParams(approvalThreadA, nil)},
			map[string]any{"id": float64(48), "method": "item/permissions/requestApproval",
				"params": map[string]any{"threadId": approvalThreadA, "turnId": "turn-a", "itemId": "perm-1",
					"permissions": map[string]any{"network": map[string]any{"enabled": true}}, "startedAtMs": float64(1700000000001)}},
		},
		"threadRuntimeStatus": map[string]any{"type": "active"},
	}))

	if got := c.DetectState("sess-a"); got != "waiting_approval" {
		t.Fatalf("desktop-owned approval not surfaced: state=%q", got)
	}
	ar := c.ApprovalRequest("sess-a")
	if ar == nil || ar["request_id"] != "47" || ar["source"] != codexApprovalSourceDesktop || ar["pending_count"] != 2 {
		t.Fatalf("bad desktop approval view: %#v", ar)
	}

	res := c.RelayApprovalRequest("sess-a", "47", "allow")
	if !boolAny(res["ok"]) {
		t.Fatalf("desktop allow failed: %#v", res)
	}
	res = c.RelayApprovalRequest("sess-a", "48", "deny")
	if !boolAny(res["ok"]) {
		t.Fatalf("desktop permissions deny failed: %#v", res)
	}
	if len(desktop.decisions) != 2 {
		t.Fatalf("expected 2 IPC decisions: %#v", desktop.decisions)
	}
	first := desktop.decisions[0]
	if first["method"] != "thread-follower-command-approval-decision" || canonicalRequestKey(first["requestId"]) != "47" ||
		first["payload"] != "accept" || first["target"] != owner || first["conversationId"] != approvalThreadA {
		t.Fatalf("bad command decision frame: %#v", first)
	}
	second := desktop.decisions[1]
	if second["method"] != "thread-follower-permissions-request-approval-response" || canonicalRequestKey(second["requestId"]) != "48" {
		t.Fatalf("bad permissions frame: %#v", second)
	}
	if granted := mapAny(mapAny(second["payload"])["permissions"]); len(granted) != 0 {
		t.Fatalf("permissions deny must grant empty profile: %#v", second["payload"])
	}
	// Unknown/stale id after Desktop resolution: safe stale, no extra IPC.
	res = c.RelayApprovalRequest("sess-a", "49", "allow")
	if boolAny(res["ok"]) || res["status"] != "stale" {
		t.Fatalf("unknown request must be stale: %#v", res)
	}
	if len(desktop.decisions) != 2 {
		t.Fatalf("stale must not emit IPC decisions: %#v", desktop.decisions)
	}
}

func TestCodexAttachRefreshesQuestionCreatedBeforeBridge(t *testing.T) {
	fc := newFakeCodexClient()
	c := testCodexWithClient(t, fc)
	desktop := &fakeDesktopClient{}
	c.desktopFactory = func() codexDesktopClient { return desktop }
	c.desktopOpener = func(string) error { return nil }
	b := newCodexDesktopBridge("", "local")
	c.bridge = b
	owner := "owner-before-attach"
	b.HandleBroadcast(bridgeSnapshotFrame(approvalThreadA, owner, 1, map[string]any{
		"id": approvalThreadA, "requests": []any{},
		"threadRuntimeStatus": map[string]any{"type": "active"},
	}))
	resynced := make(chan struct{}, 1)
	b.resync = func(threadID string, targetOwner string) {
		if threadID != approvalThreadA || targetOwner != owner {
			t.Errorf("bad resync target thread=%q owner=%q", threadID, targetOwner)
			return
		}
		b.HandleBroadcast(bridgeSnapshotFrame(approvalThreadA, owner, 2, map[string]any{
			"id": approvalThreadA,
			"requests": []any{map[string]any{
				"id": float64(57), "method": "item/tool/requestUserInput",
				"params": map[string]any{
					"threadId": approvalThreadA, "turnId": "turn-existing", "itemId": "input-existing",
					"startedAtMs": float64(1700000000000), "questions": []any{map[string]any{
						"id": "q-existing", "header": "Mode", "question": "Choose mode",
						"options": []any{map[string]any{"label": "Safe"}},
					}},
				},
			}},
			"threadRuntimeStatus": map[string]any{"type": "active"},
		}))
		resynced <- struct{}{}
	}

	got, err := c.OpenResumeSession("sess-a", approvalThreadA, "/repo", false)
	if err != nil || got != approvalThreadA {
		t.Fatalf("attach failed: thread=%q err=%v", got, err)
	}
	select {
	case <-resynced:
	default:
		t.Fatal("attach did not request a complete Desktop snapshot")
	}
	if state := c.DetectState("sess-a"); state != "waiting_approval" {
		t.Fatalf("pre-existing question not restored: state=%q", state)
	}
	ar := c.ApprovalRequest("sess-a")
	if ar == nil || ar["type"] != "question" || ar["request_id"] != "57" {
		t.Fatalf("bad restored question: %#v", ar)
	}
}

// Requirement: requestUserInput surfaces as a question and answers map back
// by question id / text / index.
func TestCodexUserInputQuestionAnswer(t *testing.T) {
	fc := newFakeCodexClient()
	c := testCodexWithClient(t, fc)
	c.BindTranscript("sess-a", approvalThreadA)
	c.onServerRequest(float64(31), "item/tool/requestUserInput", map[string]any{
		"threadId": approvalThreadA, "turnId": "turn-a", "itemId": "input-1",
		"questions": []any{map[string]any{
			"id": "q-color", "header": "Color", "question": "Pick a color",
			"options": []any{map[string]any{"label": "red", "description": "warm"}},
		}},
	})
	ar := c.ApprovalRequest("sess-a")
	if ar == nil || ar["type"] != "question" || ar["request_id"] != "31" {
		t.Fatalf("question not surfaced: %#v", ar)
	}
	qs := ar["questions"].([]map[string]any)
	if len(qs) != 1 || qs[0]["question"] != "Pick a color" {
		t.Fatalf("bad question payload: %#v", qs)
	}
	// Web answers keyed by question text.
	res := c.AnswerQuestion("sess-a", "31", map[string]string{"Pick a color": "red"})
	if !boolAny(res["ok"]) {
		t.Fatalf("answer failed: %#v", res)
	}
	body := fc.responses[len(fc.responses)-1]
	answer := mapAny(mapAny(body["answers"])["q-color"])
	if got := listAny(answer["answers"]); len(got) != 1 || got[0] != "red" {
		t.Fatalf("bad user input response body: %#v", body)
	}
	if ar := c.ApprovalRequest("sess-a"); ar != nil {
		t.Fatalf("answered question still pending: %#v", ar)
	}
}

// Requirement: serverRequest/resolved clears exactly that request (answered
// on another client / auto-reviewed), and turn/completed clears the thread.
func TestCodexResolvedAndTurnCompletedCleanup(t *testing.T) {
	fc := newFakeCodexClient()
	c := testCodexWithClient(t, fc)
	c.BindTranscript("sess-a", approvalThreadA)
	c.BindTranscript("sess-b", approvalThreadB)
	c.onServerRequest(float64(41), "item/commandExecution/requestApproval", commandApprovalParams(approvalThreadA, nil))
	c.onServerRequest(float64(42), "item/commandExecution/requestApproval", commandApprovalParams(approvalThreadA, map[string]any{"itemId": "exec-2"}))
	c.onServerRequest(float64(43), "item/commandExecution/requestApproval", commandApprovalParams(approvalThreadB, nil))

	c.onNotification("serverRequest/resolved", map[string]any{"threadId": approvalThreadA, "requestId": float64(41)})
	if ar := c.ApprovalRequest("sess-a"); ar == nil || ar["request_id"] != "42" {
		t.Fatalf("resolved cleanup wrong: %#v", ar)
	}
	c.onNotification("turn/completed", map[string]any{"threadId": approvalThreadA, "turn": map[string]any{"id": "turn-a"}})
	if ar := c.ApprovalRequest("sess-a"); ar != nil {
		t.Fatalf("turn completion must clear its thread approvals: %#v", ar)
	}
	if ar := c.ApprovalRequest("sess-b"); ar == nil || ar["request_id"] != "43" {
		t.Fatalf("other thread approvals must survive: %#v", ar)
	}
}

// Requirement: attach/resume sessions report the real Desktop-owned mode
// (on-request + auto_review), not provider defaults.
func TestCodexSessionSettingsFromBridge(t *testing.T) {
	fc := newFakeCodexClient()
	c := testCodexWithClient(t, fc)
	c.BindTranscript("sess-a", approvalThreadA)
	b := newCodexDesktopBridge("", "local")
	c.bridge = b
	b.HandleBroadcast(bridgeSnapshotFrame(approvalThreadA, "owner-1", 1, map[string]any{
		"id": approvalThreadA, "requests": []any{},
		"latestThreadSettings": map[string]any{
			"approvalPolicy": "on-request", "approvalsReviewer": "auto_review",
			"sandboxPolicy": map[string]any{"type": "workspaceWrite"},
			"model":         "gpt-5.2", "effort": "xhigh",
		},
		"currentPermissions": map[string]any{
			"approvalPolicy": "on-request", "approvalsReviewer": "auto_review",
			"sandboxPolicy": map[string]any{"type": "workspaceWrite"},
		},
	}))
	st := c.SessionSettings("sess-a")
	if st == nil || st["mode"] != "on-request" || st["approvals_reviewer"] != "auto_review" || st["sandbox"] != "workspace-write" {
		t.Fatalf("session settings must reflect the real rollout: %#v", st)
	}
	model := c.SessionModel("sess-a")
	if model["model"] != "gpt-5.2" || model["effort"] != "xhigh" {
		t.Fatalf("session model must come from the bridge: %#v", model)
	}
	// Provider-global ModelSelect still reports defaults; the /status layer
	// overlays SessionSettings for the viewed session.
	if c.ModelSelect().Mode != "auto" {
		t.Fatalf("provider default mode changed unexpectedly")
	}
}

// Requirement: concurrent server requests, notifications, reads and
// responses must be race-free (run with -race).
func TestCodexApprovalConcurrency(t *testing.T) {
	fc := newFakeCodexClient()
	c := testCodexWithClient(t, fc)
	c.BindTranscript("sess-a", approvalThreadA)
	c.BindTranscript("sess-b", approvalThreadB)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				id := float64(n*1000 + j)
				threadID := approvalThreadA
				session := "sess-a"
				if j%2 == 1 {
					threadID = approvalThreadB
					session = "sess-b"
				}
				switch n % 4 {
				case 0:
					c.onServerRequest(id, "item/commandExecution/requestApproval", commandApprovalParams(threadID, nil))
				case 1:
					c.onNotification("thread/status/changed", map[string]any{"threadId": threadID, "status": map[string]any{"type": "idle"}})
					c.onNotification("turn/completed", map[string]any{"threadId": threadID})
				case 2:
					_ = c.DetectState(session)
					_ = c.ApprovalRequest(session)
					_ = c.LatestOutput(session)
				case 3:
					if ar := c.ApprovalRequest(session); ar != nil {
						_ = c.RelayApprovalRequest(session, stringAny(ar["request_id"]), "deny")
					}
					_ = c.Status()
				}
			}
		}(i)
	}
	wg.Wait()
}

// The old failure mode: an approval response racing the read loop could
// requeue into a nil slice. Now double-responses must stay idempotent.
func TestCodexRelayApprovalNewestWithoutRequestID(t *testing.T) {
	fc := newFakeCodexClient()
	c := testCodexWithClient(t, fc)
	c.BindTranscript("sess-a", approvalThreadA)
	c.onServerRequest(float64(51), "item/commandExecution/requestApproval", commandApprovalParams(approvalThreadA, nil))
	time.Sleep(2 * time.Millisecond)
	c.onServerRequest(float64(52), "item/commandExecution/requestApproval", commandApprovalParams(approvalThreadA, map[string]any{"itemId": "exec-2"}))
	res := c.RelayApproval("sess-a", "deny")
	if !boolAny(res["ok"]) || res["request_id"] != "52" {
		t.Fatalf("RelayApproval must answer the newest approval: %#v", res)
	}
	if ar := c.ApprovalRequest("sess-a"); ar == nil || ar["request_id"] != "51" {
		t.Fatalf("older approval must remain: %#v", ar)
	}
}

// Requirement: a device without the codex binary hides the provider.
func TestCodexInstalledDetection(t *testing.T) {
	missing := NewCodex("codex", config.ProviderConfig{
		Command: "/nonexistent/codex-e2e-missing", Cwd: "/tmp",
		Extra: map[string]any{"prefer_desktop_codex": false},
	})
	if missing.Installed() {
		t.Fatal("codex must report not installed for a missing binary")
	}
	bin := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	present := NewCodex("codex", config.ProviderConfig{
		Command: bin, Cwd: "/tmp",
		Extra: map[string]any{"prefer_desktop_codex": false},
	})
	if !present.Installed() || !present.Status().Installed {
		t.Fatal("codex must report installed when the binary exists")
	}
}

func TestCodexServerRequestRecoversThreadFromTurnID(t *testing.T) {
	c := testCodexWithClient(t, newFakeCodexClient())
	c.BindTranscript("logical-a", approvalThreadA)
	if got := c.threadIDForNotification(map[string]any{"threadId": approvalThreadA, "turnId": "turn-a"}); got != approvalThreadA {
		t.Fatalf("notification mapping=%q", got)
	}
	c.onServerRequest(float64(71), "item/commandExecution/requestApproval", map[string]any{
		"turnId": "turn-a", "command": []any{"pwd"},
	})
	request := c.ApprovalRequest("logical-a")
	if request == nil || request["request_id"] != "71" || request["thread_id"] != approvalThreadA {
		t.Fatalf("turn-only approval was not scoped to its thread: %#v", request)
	}
}

func TestCodexFutureApprovalMethodsStayActionable(t *testing.T) {
	method := "item/networkAccess/requestApproval"
	if !codexHumanRequestMethod(method) {
		t.Fatalf("future approval method was dropped")
	}
	allow, err := codexApprovalResponseBody(method, true, nil)
	if err != nil || allow["decision"] != "accept" {
		t.Fatalf("future allow response=%#v err=%v", allow, err)
	}
	deny, err := codexApprovalResponseBody(method, false, nil)
	if err != nil || deny["decision"] != "decline" {
		t.Fatalf("future deny response=%#v err=%v", deny, err)
	}
	for _, machine := range []string{"account/chatgptAuthTokens/refresh", "attestation/generate", "currentTime/read"} {
		if codexHumanRequestMethod(machine) {
			t.Fatalf("machine request became approval: %s", machine)
		}
	}
}

func TestCodexFutureDesktopApprovalStaysVisibleButNonActionable(t *testing.T) {
	c := testCodexWithClient(t, newFakeCodexClient())
	c.BindTranscript("sess-a", approvalThreadA)
	b := newCodexDesktopBridge("", "local")
	c.bridge = b
	b.HandleBroadcast(bridgeSnapshotFrame(approvalThreadA, "owner-future", 1, map[string]any{
		"id": approvalThreadA,
		"requests": []any{map[string]any{
			"id": float64(88), "method": "item/networkAccess/requestApproval",
			"params": map[string]any{"threadId": approvalThreadA, "startedAtMs": float64(1700000000000)},
		}},
		"threadRuntimeStatus": map[string]any{"type": "active"},
	}))
	request := c.ApprovalRequest("sess-a")
	if request == nil || request["request_id"] != "88" || boolAny(request["actionable"]) {
		t.Fatalf("future Desktop request should remain visible but non-actionable: %#v", request)
	}
}

func TestCodexScopedStateDoesNotLeakFromAnotherThread(t *testing.T) {
	c := testCodexWithClient(t, newFakeCodexClient())
	c.setLastState("running")
	c.BindTranscript("idle-session", approvalThreadA)
	if got := c.DetectState("idle-session"); got != "idle" {
		t.Fatalf("global state leaked into scoped session: %q", got)
	}
}

// Requirement: a device without any Claude Desktop CLI / PATH claude hides
// the claude providers.
func TestClaudeInstalledDetection(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	missing := NewClaudeCLI("claude", config.ProviderConfig{Command: "/nonexistent/claude-e2e-missing"})
	if missing.Installed() {
		t.Fatal("claude must report not installed for a missing binary")
	}
	bin := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	present := NewClaudeCLI("claude", config.ProviderConfig{Command: bin})
	if !present.Installed() || !present.Status().Installed {
		t.Fatal("claude must report installed when the binary exists")
	}
	userBin := filepath.Join(home, ".local", "bin", "claude")
	if err := os.MkdirAll(filepath.Dir(userBin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(userBin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir())
	userInstalled := NewClaudeCLI("claude", config.ProviderConfig{Command: "claude"})
	if got := userInstalled.resolveCommand(); got != userBin {
		t.Fatalf("bare claude command resolved to %q, want standard user install %q", got, userBin)
	}
}
