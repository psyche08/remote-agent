package provider

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/psyche08/remote-agent/internal/config"
)

func TestCodexComputerUseToolsAreAdvertisedOnlyOnNewThreads(t *testing.T) {
	client := newFakeCodexClient()
	c := testCodexWithClient(t, client)
	if tools := c.threadStartOptions("/repo", "", "", "auto")["dynamicTools"]; tools != nil {
		t.Fatalf("computer-use tools advertised without an in-process broker: %#v", tools)
	}

	c.SetComputerUseToolHandler(func(context.Context, ComputerUseToolRequest) (ComputerUseToolResult, error) {
		return ComputerUseToolResult{}, nil
	})
	opts := c.threadStartOptions("/repo", "", "", "auto")
	names := codexComputerUseToolNames(opts["dynamicTools"])
	want := []string{"get_app_state", "press", "set_value", "click", "type_text", "press_key", "scroll"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("dynamic tool names=%v want=%v", names, want)
	}

	threadID, err := c.OpenOrCreateSession("logical-session", StartOptions{Cwd: "/repo", Mode: "auto"})
	if err != nil {
		t.Fatal(err)
	}
	c.computerUseMu.Lock()
	enabledGeneration := c.computerUseThreads[threadID]
	c.computerUseMu.Unlock()
	if enabledGeneration != 1 {
		t.Fatalf("new thread generation=%d want=1", enabledGeneration)
	}
	if stored := c.startOptionsFor("logical-session"); stored["dynamicTools"] != nil {
		t.Fatalf("thread-only dynamicTools leaked into turn/start params: %#v", stored)
	}
}

func TestCodexComputerUseRequiresInspectionAndUsesAuthoritativeIdentity(t *testing.T) {
	client := newFakeCodexClient()
	c := testCodexWithClient(t, client)
	calls := make(chan ComputerUseToolRequest, 4)
	c.SetComputerUseToolHandler(func(_ context.Context, request ComputerUseToolRequest) (ComputerUseToolResult, error) {
		calls <- request
		if request.Tool == "get_app_state" {
			return ComputerUseToolResult{
				Text:     `{"accessibility":{"role":"AXApplication"}}`,
				ImageURL: "data:image/png;base64,iVBORw0KGgo=",
			}, nil
		}
		return ComputerUseToolResult{Text: `{"ok":true}`}, nil
	})
	threadID, err := c.OpenOrCreateSession("logical-session", StartOptions{Cwd: "/repo"})
	if err != nil {
		t.Fatal(err)
	}
	activateCodexComputerUseTurn(c, client, threadID, "turn-real")

	// Arguments are model-controlled and may contain identity-looking keys.
	// They must not override the app-server envelope.
	c.onServerRequestFromClient(1, float64(10), "item/tool/call", map[string]any{
		"namespace": codexComputerUseNamespace,
		"tool":      "press",
		"threadId":  threadID,
		"turnId":    "turn-real",
		"callId":    "call-before-inspect",
		"arguments": map[string]any{
			"app": "TextEdit", "path": []any{float64(0)},
		},
	})
	beforeInspect := waitForFakeCodexResponse(t, client, 1)
	if boolAny(beforeInspect["success"]) || !strings.Contains(dynamicToolResponseText(beforeInspect), "get_app_state") {
		t.Fatalf("mutation before inspection was not refused: %#v", beforeInspect)
	}
	select {
	case call := <-calls:
		t.Fatalf("refused mutation reached broker: %#v", call)
	default:
	}

	c.onServerRequestFromClient(1, float64(11), "item/tool/call", map[string]any{
		"namespace": codexComputerUseNamespace,
		"tool":      "get_app_state",
		"threadId":  threadID,
		"turnId":    "turn-real",
		"callId":    "call-inspect",
		"arguments": map[string]any{
			"bundle_id": "com.apple.TextEdit", "provider_id": "forged-provider",
			"session_id": "forged-session", "turn_id": "forged-turn",
		},
	})
	inspectResponse := waitForFakeCodexResponse(t, client, 2)
	if !boolAny(inspectResponse["success"]) {
		t.Fatalf("get_app_state failed: %#v", inspectResponse)
	}
	items := dynamicToolResponseItems(inspectResponse)
	if len(items) != 2 || stringAny(items[0]["type"]) != "inputText" ||
		stringAny(items[1]["type"]) != "inputImage" {
		t.Fatalf("get_app_state did not return text plus image: %#v", inspectResponse)
	}
	inspectCall := <-calls
	if inspectCall.ProviderID != "codex" || inspectCall.SessionID != "logical-session" ||
		inspectCall.ThreadID != threadID || inspectCall.TurnID != "turn-real" ||
		inspectCall.CallID != "call-inspect" || inspectCall.BundleID != "com.apple.TextEdit" {
		t.Fatalf("model arguments overrode authoritative identity: %#v", inspectCall)
	}

	c.onServerRequestFromClient(1, float64(12), "item/tool/call", map[string]any{
		"namespace": codexComputerUseNamespace,
		"tool":      "press",
		"threadId":  threadID,
		"turnId":    "turn-real",
		"callId":    "call-press",
		"arguments": map[string]any{
			"bundle_id": "com.apple.TextEdit", "path": []any{float64(1), float64(2)},
		},
	})
	mutationResponse := waitForFakeCodexResponse(t, client, 3)
	if !boolAny(mutationResponse["success"]) {
		t.Fatalf("mutation after inspection failed: %#v", mutationResponse)
	}
	mutationCall := <-calls
	if mutationCall.Tool != "press" || mutationCall.BundleID != "com.apple.TextEdit" ||
		len(mutationCall.Path) != 2 || mutationCall.Path[0] != 1 || mutationCall.Path[1] != 2 {
		t.Fatalf("bad structured mutation request: %#v", mutationCall)
	}
}

func TestCodexComputerUseInspectionIsBoundToAXApplication(t *testing.T) {
	client := newFakeCodexClient()
	c := testCodexWithClient(t, client)
	var calls atomic.Int32
	c.SetComputerUseToolHandler(func(_ context.Context, request ComputerUseToolRequest) (ComputerUseToolResult, error) {
		calls.Add(1)
		if request.Tool == "get_app_state" {
			return ComputerUseToolResult{
				Text: `{"ok":true}`, ImageURL: "data:image/png;base64,iVBORw0KGgo=",
			}, nil
		}
		return ComputerUseToolResult{Text: `{"ok":true}`}, nil
	})
	threadID, err := c.OpenOrCreateSession("session", StartOptions{Cwd: "/repo"})
	if err != nil {
		t.Fatal(err)
	}
	activateCodexComputerUseTurn(c, client, threadID, "turn-a")

	if err := c.answerDynamicTool(1, float64(40), map[string]any{
		"namespace": codexComputerUseNamespace, "tool": "get_app_state",
		"threadId": threadID, "turnId": "turn-a", "callId": "inspect-textedit",
		"arguments": map[string]any{"bundle_id": "com.apple.TextEdit"},
	}); err != nil {
		t.Fatal(err)
	}
	if response := waitForFakeCodexResponse(t, client, 1); !boolAny(response["success"]) {
		t.Fatalf("inspection failed: %#v", response)
	}

	if err := c.answerDynamicTool(1, float64(41), map[string]any{
		"namespace": codexComputerUseNamespace, "tool": "press",
		"threadId": threadID, "turnId": "turn-a", "callId": "cross-app",
		"arguments": map[string]any{
			"bundle_id": "com.apple.Notes", "path": []any{float64(0)},
		},
	}); err != nil {
		t.Fatal(err)
	}
	response := waitForFakeCodexResponse(t, client, 2)
	if boolAny(response["success"]) || !strings.Contains(
		dynamicToolResponseText(response), "same application",
	) {
		t.Fatalf("cross-app AX mutation was not refused: %#v", response)
	}
	if calls.Load() != 1 {
		t.Fatalf("cross-app mutation reached broker; calls=%d", calls.Load())
	}
}

func TestCodexComputerUseInspectionIsSingleUse(t *testing.T) {
	client := newFakeCodexClient()
	c := testCodexWithClient(t, client)
	var calls atomic.Int32
	c.SetComputerUseToolHandler(func(_ context.Context, request ComputerUseToolRequest) (ComputerUseToolResult, error) {
		calls.Add(1)
		if request.Tool == "get_app_state" {
			return ComputerUseToolResult{
				Text: `{"ok":true}`, ImageURL: "data:image/png;base64,iVBORw0KGgo=",
			}, nil
		}
		return ComputerUseToolResult{Text: `{"ok":true}`}, nil
	})
	threadID, err := c.OpenOrCreateSession("session", StartOptions{Cwd: "/repo"})
	if err != nil {
		t.Fatal(err)
	}
	activateCodexComputerUseTurn(c, client, threadID, "turn-a")

	call := func(id float64, tool, callID string) map[string]any {
		args := map[string]any{"bundle_id": "com.apple.TextEdit"}
		if tool == "press" {
			args["path"] = []any{float64(0)}
		}
		if err := c.answerDynamicTool(1, id, map[string]any{
			"namespace": codexComputerUseNamespace, "tool": tool,
			"threadId": threadID, "turnId": "turn-a", "callId": callID,
			"arguments": args,
		}); err != nil {
			t.Fatal(err)
		}
		return waitForFakeCodexResponse(t, client, int(id-49))
	}

	if response := call(50, "get_app_state", "inspect-1"); !boolAny(response["success"]) {
		t.Fatalf("first inspection failed: %#v", response)
	}
	if response := call(51, "press", "mutate-1"); !boolAny(response["success"]) {
		t.Fatalf("first mutation failed: %#v", response)
	}
	if response := call(52, "press", "mutate-stale"); boolAny(response["success"]) ||
		!strings.Contains(dynamicToolResponseText(response), "get_app_state") {
		t.Fatalf("second mutation reused a stale observation: %#v", response)
	}
	if response := call(53, "get_app_state", "inspect-2"); !boolAny(response["success"]) {
		t.Fatalf("re-inspection failed: %#v", response)
	}
	if response := call(54, "press", "mutate-2"); !boolAny(response["success"]) {
		t.Fatalf("mutation after re-inspection failed: %#v", response)
	}
	if got := calls.Load(); got != 4 {
		t.Fatalf("broker calls=%d want=4 (stale mutation must be refused)", got)
	}
}

func TestCodexComputerUseRejectsWrongTurnGenerationAndLegacyResume(t *testing.T) {
	client := newFakeCodexClient()
	c := testCodexWithClient(t, client)
	var calls atomic.Int32
	c.SetComputerUseToolHandler(func(context.Context, ComputerUseToolRequest) (ComputerUseToolResult, error) {
		calls.Add(1)
		return ComputerUseToolResult{
			Text: `{"ok":true}`, ImageURL: "data:image/png;base64,iVBORw0KGgo=",
		}, nil
	})
	threadID, err := c.OpenOrCreateSession("session", StartOptions{Cwd: "/repo"})
	if err != nil {
		t.Fatal(err)
	}
	activateCodexComputerUseTurn(c, client, threadID, "turn-a")

	for i, params := range []map[string]any{
		{
			"namespace": codexComputerUseNamespace, "tool": "get_app_state",
			"threadId": threadID, "turnId": "turn-wrong", "callId": "wrong-turn",
			"arguments": map[string]any{"app": "TextEdit"},
		},
		{
			"namespace": codexComputerUseNamespace, "tool": "get_app_state",
			"threadId": "thread-wrong", "turnId": "turn-a", "callId": "wrong-thread",
			"arguments": map[string]any{"app": "TextEdit"},
		},
	} {
		if err := c.answerDynamicTool(1, float64(20+i), params); err != nil {
			t.Fatal(err)
		}
		response := waitForFakeCodexResponse(t, client, i+1)
		if boolAny(response["success"]) {
			t.Fatalf("wrong identity accepted: %#v", response)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("wrong identity reached broker %d times", calls.Load())
	}

	// A replacement app-server generation may resume the persisted thread, but
	// thread/resume cannot safely inject dynamicTools. Do not inherit generation
	// 1's advertisement into generation 2.
	c.clientMu.Lock()
	c.clientGeneration = 2
	c.clientSequence = 2
	c.clientMu.Unlock()
	activateCodexComputerUseTurn(c, client, threadID, "turn-b")
	if err := c.answerDynamicTool(2, float64(22), map[string]any{
		"namespace": codexComputerUseNamespace, "tool": "get_app_state",
		"threadId": threadID, "turnId": "turn-b", "callId": "resumed-generation",
		"arguments": map[string]any{"app": "TextEdit"},
	}); err != nil {
		t.Fatal(err)
	}
	response := waitForFakeCodexResponse(t, client, 3)
	if boolAny(response["success"]) || calls.Load() != 0 {
		t.Fatalf("resumed generation inherited computer-use authority: response=%#v calls=%d", response, calls.Load())
	}
}

func TestCodexComputerUseTerminalNotificationClearsInspectionAndPublishesLeaseFrames(t *testing.T) {
	client := newFakeCodexClient()
	c := testCodexWithClient(t, client)
	c.SetComputerUseToolHandler(func(_ context.Context, request ComputerUseToolRequest) (ComputerUseToolResult, error) {
		if request.Tool == "get_app_state" {
			return ComputerUseToolResult{
				Text: `{"ok":true}`, ImageURL: "data:image/png;base64,iVBORw0KGgo=",
			}, nil
		}
		return ComputerUseToolResult{Text: `{"ok":true}`}, nil
	})
	frames := make(chan map[string]any, 8)
	c.SetStreamPublisher(func(target string, frame map[string]any) {
		if target == "session" {
			frames <- frame
		}
	})
	threadID, err := c.OpenOrCreateSession("session", StartOptions{Cwd: "/repo"})
	if err != nil {
		t.Fatal(err)
	}
	c.onNotificationFromClient(1, "turn/started", map[string]any{
		"threadId": threadID, "turn": map[string]any{"id": "turn-a"},
	})
	started := waitForProviderFrame(t, frames)
	if started["type"] != "turn" || started["status"] != "started" || started["turn_id"] != "turn-a" {
		t.Fatalf("bad authoritative start frame: %#v", started)
	}
	if err := c.answerDynamicTool(1, float64(30), map[string]any{
		"namespace": codexComputerUseNamespace, "tool": "get_app_state",
		"threadId": threadID, "turnId": "turn-a", "callId": "inspect",
		"arguments": map[string]any{"app": "TextEdit"},
	}); err != nil {
		t.Fatal(err)
	}
	if response := waitForFakeCodexResponse(t, client, 1); !boolAny(response["success"]) {
		t.Fatalf("inspection failed: %#v", response)
	}

	c.onNotificationFromClient(1, "turn/completed", map[string]any{
		"threadId": threadID,
		"turn":     map[string]any{"id": "turn-a", "status": "completed"},
	})
	completed := waitForProviderFrame(t, frames)
	if completed["type"] != "turn" || completed["status"] != "completed" {
		t.Fatalf("bad terminal frame: %#v", completed)
	}
	if err := c.answerDynamicTool(1, float64(31), map[string]any{
		"namespace": codexComputerUseNamespace, "tool": "press",
		"threadId": threadID, "turnId": "turn-a", "callId": "after-terminal",
		"arguments": map[string]any{"app": "TextEdit", "path": []any{float64(0)}},
	}); err != nil {
		t.Fatal(err)
	}
	if response := waitForFakeCodexResponse(t, client, 2); boolAny(response["success"]) {
		t.Fatalf("terminal turn retained computer-use authority: %#v", response)
	}
}

func TestCodexComputerUseStatusWithoutTurnIDCannotRevokeAuthoritativeStart(t *testing.T) {
	c := testCodexWithClient(t, newFakeCodexClient())
	frames := c.framesForNotification("turn/started", map[string]any{
		"turn": map[string]any{"id": "turn-a"},
	}, "thread-a")
	if len(frames) != 1 || frames[0]["turn_id"] != "turn-a" {
		t.Fatalf("authoritative turn/started frame missing: %#v", frames)
	}
	if frames := c.framesForNotification("thread/status/changed", map[string]any{
		"status": map[string]any{"type": "active"},
	}, "thread-a"); len(frames) != 0 {
		t.Fatalf("identity-free active status published a destructive start frame: %#v", frames)
	}
	frames = c.framesForNotification("turn/completed", map[string]any{
		"turn": map[string]any{"id": "turn-a", "status": "completed"},
	}, "thread-a")
	if len(frames) != 1 || frames[0]["turn_id"] != "turn-a" {
		t.Fatalf("nested terminal turn id was lost: %#v", frames)
	}
}

func TestParseCodexComputerUseToolVocabulary(t *testing.T) {
	empty := ""
	tests := []struct {
		tool string
		args map[string]any
		want ComputerUseToolRequest
	}{
		{
			tool: "get_app_state", args: map[string]any{"app": " TextEdit "},
			want: ComputerUseToolRequest{Tool: "get_app_state", App: "TextEdit"},
		},
		{
			tool: "press", args: map[string]any{
				"bundle_id": "com.apple.TextEdit", "path": []any{float64(1), float64(2)},
			},
			want: ComputerUseToolRequest{
				Tool: "press", BundleID: "com.apple.TextEdit", Path: []int{1, 2},
			},
		},
		{
			tool: "set_value", args: map[string]any{
				"app": "TextEdit", "path": []any{float64(3)}, "value": "",
			},
			want: ComputerUseToolRequest{Tool: "set_value", App: "TextEdit", Path: []int{3}, Value: &empty},
		},
		{
			tool: "click", args: map[string]any{
				"x": float64(120), "y": float64(80), "button": "right", "count": float64(2),
			},
			want: ComputerUseToolRequest{
				Tool: "click", X: testComputerUseIntPtr(120), Y: testComputerUseIntPtr(80),
				Button: "right", Count: 2,
			},
		},
		{
			tool: "type_text", args: map[string]any{"text": "hello"},
			want: ComputerUseToolRequest{Tool: "type_text", Text: "hello"},
		},
		{
			tool: "press_key", args: map[string]any{"keys": []any{"cmd", "s"}},
			want: ComputerUseToolRequest{Tool: "press_key", Keys: []string{"cmd", "s"}},
		},
		{
			tool: "scroll", args: map[string]any{
				"x": float64(500), "y": float64(400), "delta_y": float64(-300),
			},
			want: ComputerUseToolRequest{
				Tool: "scroll", X: testComputerUseIntPtr(500), Y: testComputerUseIntPtr(400), DeltaY: -300,
			},
		},
	}
	for _, test := range tests {
		t.Run(test.tool, func(t *testing.T) {
			got, err := parseCodexComputerUseRequest(test.tool, test.args)
			if err != nil {
				t.Fatal(err)
			}
			if !equalComputerUseRequest(got, test.want) {
				t.Fatalf("request=%#v want=%#v", got, test.want)
			}
		})
	}

	for name, test := range map[string]struct {
		tool string
		args map[string]any
	}{
		"non-string app": {"get_app_state", map[string]any{"app": float64(1)}},
		"zero scroll": {"scroll", map[string]any{
			"x": float64(1), "y": float64(2), "delta_y": float64(0),
		}},
		"negative path": {"press", map[string]any{
			"app": "TextEdit", "path": []any{float64(-1)},
		}},
		"negative screenshot point": {"click", map[string]any{
			"x": float64(-1), "y": float64(2),
		}},
		"oversized screenshot point": {"scroll", map[string]any{
			"x": float64(32768), "y": float64(2), "delta_y": float64(1),
		}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseCodexComputerUseRequest(test.tool, test.args); err == nil {
				t.Fatal("invalid tool arguments were accepted")
			}
		})
	}
}

func TestCodexComputerUseFakeAppServerProtocolRoundTrip(t *testing.T) {
	transport := newFakeCodexAppServerJSONTransport()
	c := NewCodex("codex", config.ProviderConfig{
		Command: "/unused/codex", Cwd: t.TempDir(),
		Extra: map[string]any{"prefer_desktop_codex": false},
	})
	c.desktopFactory = func() codexDesktopClient { return &fakeDesktopClient{} }
	created := make(chan *CodexAppServerClient, 1)
	c.clientFactory = func(
		onNotification func(string, map[string]any),
		onServerRequest func(any, string, map[string]any),
	) codexAppClient {
		client := newFakeSharedCodexClient(t, transport, onNotification, onServerRequest)
		created <- client
		return client
	}
	c.SetComputerUseToolHandler(func(_ context.Context, request ComputerUseToolRequest) (ComputerUseToolResult, error) {
		if request.ThreadID != "thread-json" || request.TurnID != "turn-json" {
			t.Errorf("bad app-server identity: %#v", request)
		}
		return ComputerUseToolResult{
			Text:     `{"accessibility":{"role":"AXApplication"}}`,
			ImageURL: "data:image/png;base64,iVBORw0KGgo=",
		}, nil
	})
	defer c.Shutdown()

	opened := make(chan error, 1)
	go func() {
		_, err := c.OpenOrCreateSession("session-json", StartOptions{Cwd: "/repo"})
		opened <- err
	}()
	<-created
	initialize := transport.nextWrite(t)
	if method := stringAny(initialize["method"]); method != "initialize" {
		t.Fatalf("first method=%q want=initialize", method)
	}
	transport.send(t, map[string]any{
		"id": initialize["id"],
		"result": map[string]any{
			"codexHome": "/tmp/codex-home", "platformFamily": "unix",
		},
	})
	initialized := transport.nextWrite(t)
	if method := stringAny(initialized["method"]); method != "initialized" {
		t.Fatalf("second method=%q want=initialized", method)
	}
	threadStart := nextFakeAppServerMethod(t, transport, "thread/start")
	threadParams := mapAny(threadStart["params"])
	if names := codexComputerUseToolNames(threadParams["dynamicTools"]); len(names) != 7 {
		t.Fatalf("thread/start missing dynamic tools: %#v", threadParams)
	}
	transport.send(t, map[string]any{
		"id":     threadStart["id"],
		"result": map[string]any{"thread": map[string]any{"id": "thread-json"}},
	})
	if err := <-opened; err != nil {
		t.Fatal(err)
	}

	sent := make(chan SendResult, 1)
	go func() { sent <- c.SendPrompt("session-json", "inspect TextEdit") }()
	turnStart := nextFakeAppServerMethod(t, transport, "turn/start")
	turnParams := mapAny(turnStart["params"])
	if turnParams["dynamicTools"] != nil {
		t.Fatalf("thread dynamic tools leaked into turn/start: %#v", turnParams)
	}
	transport.send(t, map[string]any{
		"id":     turnStart["id"],
		"result": map[string]any{"turn": map[string]any{"id": "turn-json"}},
	})
	if result := <-sent; !result.OK {
		t.Fatalf("turn/start failed: %#v", result)
	}
	transport.send(t, map[string]any{
		"method": "turn/started",
		"params": map[string]any{
			"threadId": "thread-json", "turn": map[string]any{"id": "turn-json"},
		},
	})
	transport.send(t, map[string]any{
		"id": float64(91), "method": "item/tool/call",
		"params": map[string]any{
			"namespace": codexComputerUseNamespace, "tool": "get_app_state",
			"threadId": "thread-json", "turnId": "turn-json", "callId": "call-json",
			"arguments": map[string]any{"bundle_id": "com.apple.TextEdit"},
		},
	})
	response := nextFakeAppServerResponse(t, transport, 91)
	result := mapAny(response["result"])
	if !boolAny(result["success"]) || len(dynamicToolResponseItems(result)) != 2 {
		t.Fatalf("bad dynamic tool response: %#v", response)
	}
}

func activateCodexComputerUseTurn(c *Codex, client *fakeCodexClient, threadID, turnID string) {
	client.SetThreadStatus(threadID, "active")
	c.markAppServerThread(threadID)
	c.setThreadActive(threadID, true)
	c.bindTurnThread(threadID, turnID)
}

func codexComputerUseToolNames(raw any) []string {
	specs := computerUseTestList(raw)
	if len(specs) != 1 {
		return nil
	}
	namespace := mapAny(specs[0])
	if namespace["type"] != "namespace" || namespace["name"] != codexComputerUseNamespace {
		return nil
	}
	out := []string{}
	for _, rawTool := range computerUseTestList(namespace["tools"]) {
		out = append(out, stringAny(mapAny(rawTool)["name"]))
	}
	return out
}

func waitForFakeCodexResponse(t *testing.T, client *fakeCodexClient, count int) map[string]any {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		client.mu.Lock()
		if len(client.responses) >= count {
			response := client.responses[count-1]
			client.mu.Unlock()
			return response
		}
		client.mu.Unlock()
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for response %d", count)
	return nil
}

func dynamicToolResponseItems(response map[string]any) []map[string]any {
	out := []map[string]any{}
	for _, raw := range computerUseTestList(response["contentItems"]) {
		out = append(out, mapAny(raw))
	}
	return out
}

func computerUseTestList(raw any) []any {
	switch values := raw.(type) {
	case []any:
		return values
	case []map[string]any:
		out := make([]any, len(values))
		for index, value := range values {
			out[index] = value
		}
		return out
	default:
		return nil
	}
}

func dynamicToolResponseText(response map[string]any) string {
	items := dynamicToolResponseItems(response)
	if len(items) == 0 {
		return ""
	}
	return stringAny(items[0]["text"])
}

func waitForProviderFrame(t *testing.T, frames <-chan map[string]any) map[string]any {
	t.Helper()
	select {
	case frame := <-frames:
		return frame
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for provider frame")
		return nil
	}
}

func testComputerUseIntPtr(value int) *int { return &value }

func equalComputerUseRequest(left, right ComputerUseToolRequest) bool {
	if left.Tool != right.Tool || left.App != right.App || left.BundleID != right.BundleID ||
		left.Button != right.Button || left.Count != right.Count || left.Text != right.Text ||
		left.DeltaX != right.DeltaX || left.DeltaY != right.DeltaY ||
		!equalComputerUseIntPtr(left.X, right.X) || !equalComputerUseIntPtr(left.Y, right.Y) ||
		!equalComputerUseStringPtr(left.Value, right.Value) ||
		strings.Join(left.Keys, "\x00") != strings.Join(right.Keys, "\x00") {
		return false
	}
	if len(left.Path) != len(right.Path) {
		return false
	}
	for index := range left.Path {
		if left.Path[index] != right.Path[index] {
			return false
		}
	}
	return true
}

func equalComputerUseIntPtr(left, right *int) bool {
	return (left == nil && right == nil) || (left != nil && right != nil && *left == *right)
}

func equalComputerUseStringPtr(left, right *string) bool {
	return (left == nil && right == nil) || (left != nil && right != nil && *left == *right)
}

func nextFakeAppServerMethod(
	t *testing.T, transport *fakeCodexAppServerJSONTransport, wanted string,
) map[string]any {
	t.Helper()
	for {
		message := transport.nextWrite(t)
		method := stringAny(message["method"])
		if method == wanted {
			return message
		}
		switch method {
		case "account/rateLimits/read", "account/read":
			transport.send(t, map[string]any{"id": message["id"], "result": map[string]any{}})
		case "initialized":
		default:
			t.Fatalf("unexpected app-server method %q while waiting for %q: %#v", method, wanted, message)
		}
	}
}

func nextFakeAppServerResponse(
	t *testing.T, transport *fakeCodexAppServerJSONTransport, wantedID int64,
) map[string]any {
	t.Helper()
	for {
		message := transport.nextWrite(t)
		if id, ok := numericID(message["id"]); ok && id == wantedID && message["result"] != nil {
			return message
		}
		method := stringAny(message["method"])
		switch method {
		case "account/rateLimits/read", "account/read":
			transport.send(t, map[string]any{"id": message["id"], "result": map[string]any{}})
		case "initialized":
		default:
			t.Fatalf("unexpected app-server message while waiting for response %d: %#v", wantedID, message)
		}
	}
}
