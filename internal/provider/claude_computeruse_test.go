package provider

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/psyche08/remote-agent/internal/computeruse"
	"github.com/psyche08/remote-agent/internal/config"
	"github.com/psyche08/remote-agent/internal/turnstatehook"
)

const claudeComputerUseTestTranscript = "11111111-2222-4333-8444-555555555555"

type fakeClaudeComputerUse struct {
	mu        sync.Mutex
	opened    bool
	stage     int
	requests  []ComputerUseToolRequest
	prompt    string
	readErr   error
	mutateErr error
}

type fakeClaudeNewComputerUse struct {
	mu                 sync.Mutex
	opened             bool
	modeOpen           bool
	modeAuto           bool
	requireAutoConfirm bool
	autoConfirmOpen    bool
	composerFocused    bool
	prompt             string
	sent               bool
	callbackReturned   bool
	postSendPolls      int
	polledBeforeReturn bool
	requests           []ComputerUseToolRequest
}

func (f *fakeClaudeNewComputerUse) handler(
	ctx context.Context, sessionID string, callback ComputerUseAutomationCallback,
) error {
	if sessionID != "logical-new" {
		return errors.New("wrong new logical session")
	}
	err := callback(ctx, f.tool)
	f.mu.Lock()
	f.callbackReturned = true
	f.mu.Unlock()
	return err
}

func (f *fakeClaudeNewComputerUse) openURL(_ context.Context, appPath, rawURL string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if appPath != claudeDesktopDefaultAppPath || rawURL != "claude://code/new" {
		return errors.New("wrong new-session deep link")
	}
	f.opened = true
	f.composerFocused = true
	return nil
}

func (f *fakeClaudeNewComputerUse) tool(
	_ context.Context, request ComputerUseToolRequest,
) (ComputerUseToolResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, request)
	if request.BundleID != claudeDesktopDefaultBundleID {
		return ComputerUseToolResult{}, errors.New("wrong bundle")
	}
	if request.Tool == "get_app_state" {
		return claudeAXResult(f.snapshotLocked())
	}
	switch request.Tool {
	case "focus":
		if len(request.Path) != 3 || request.Path[2] != 1 {
			return ComputerUseToolResult{}, errors.New("focused wrong new-session element")
		}
		f.composerFocused = true
	case "set_value":
		if request.Value == nil || len(request.Path) != 3 || request.Path[2] != 1 || !f.composerFocused {
			return ComputerUseToolResult{}, errors.New("invalid new-session composer input")
		}
		f.prompt = *request.Value
	case "press":
		if len(request.Path) != 3 {
			return ComputerUseToolResult{}, errors.New("invalid new-session press path")
		}
		switch request.Path[1] {
		case 1:
			switch request.Path[2] {
			case 2:
				if f.prompt == "" || !f.modeAuto {
					return ComputerUseToolResult{}, errors.New("sent before exact mode and prompt")
				}
				f.sent = true
			case 3:
				f.modeOpen = !f.modeOpen
				f.composerFocused = false
			default:
				return ComputerUseToolResult{}, errors.New("wrong composer-footer press")
			}
		case 4:
			if request.Path[2] != 1 || !f.modeOpen {
				return ComputerUseToolResult{}, errors.New("wrong mode option press")
			}
			f.modeOpen = false
			if f.requireAutoConfirm {
				f.autoConfirmOpen = true
			} else {
				f.modeAuto = true
			}
		case 5:
			if request.Path[2] != 2 || !f.autoConfirmOpen {
				return ComputerUseToolResult{}, errors.New("wrong Auto confirmation press")
			}
			f.autoConfirmOpen = false
			f.modeAuto = true
		default:
			return ComputerUseToolResult{}, errors.New("wrong new-session press subtree")
		}
	default:
		return ComputerUseToolResult{}, errors.New("unexpected new-session tool")
	}
	return ComputerUseToolResult{Text: `{"ok":true}`}, nil
}

func (f *fakeClaudeNewComputerUse) snapshotLocked() []claudeAXElement {
	if !f.opened {
		return []claudeAXElement{{Role: "AXWindow", Label: "Claude", Path: []int{0}}}
	}
	value := f.prompt
	if f.sent {
		value = ""
		return []claudeAXElement{{
			Role: "AXTextArea", Label: "Message Claude", Value: value,
			Focused: claudeTestBool(true), Path: []int{0, 1, 1},
		}}
	}
	if f.autoConfirmOpen {
		return []claudeAXElement{
			{Role: "AXHeading", Label: "Enable auto mode?", Path: []int{0, 5, 0}},
			{Role: "AXButton", Label: "Enable auto mode", Actionable: true, Path: []int{0, 5, 2}},
		}
	}
	modeLabel := "Manual"
	if f.modeAuto {
		modeLabel = "Auto"
	}
	elements := []claudeAXElement{
		{Role: "AXHeading", Label: "New task", Current: claudeTestBool(true), Path: []int{0, 0}},
		{Role: "AXTextArea", Label: "Message Claude", Value: value,
			Focused: claudeTestBool(f.composerFocused), Path: []int{0, 1, 1}},
		{Role: "AXButton", Label: "Send message", Actionable: true, Path: []int{0, 1, 2}},
		{Role: "AXButton", Label: modeLabel, Actionable: true, Path: []int{0, 1, 3}},
	}
	if f.modeOpen {
		elements = append(elements,
			claudeAXElement{Role: "AXHeading", Label: "Mode", Path: []int{0, 4, 0}},
			claudeAXElement{Role: "AXMenuItem", Label: "Auto", Actionable: true,
				Checked: claudeTestBool(false), Path: []int{0, 4, 1}},
		)
	}
	return elements
}

func (f *fakeClaudeNewComputerUse) sessions() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.sent {
		return nil
	}
	f.postSendPolls++
	if !f.callbackReturned {
		f.polledBeforeReturn = true
	}
	if f.postSendPolls < 3 {
		return nil
	}
	return []map[string]any{{
		"native_session_id": "desktop-new", "cli_session_id": claudeComputerUseTestTranscript,
		"title": "New exact session", "permission_mode": "auto", "updated_at": "2026-08-11T02:00:02Z",
	}}
}

func claudeTestBool(value bool) *bool { return &value }

func (f *fakeClaudeComputerUse) openURL(_ context.Context, appPath, rawURL string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if appPath != claudeDesktopDefaultAppPath || rawURL == "" {
		return errors.New("wrong deep-link target")
	}
	f.opened = true
	return nil
}

func (f *fakeClaudeComputerUse) handler(
	ctx context.Context, sessionID string, callback ComputerUseAutomationCallback,
) error {
	if sessionID != "logical-1" {
		return errors.New("wrong logical session")
	}
	return callback(ctx, f.tool)
}

func (f *fakeClaudeComputerUse) tool(
	_ context.Context, request ComputerUseToolRequest,
) (ComputerUseToolResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, request)
	if request.BundleID != claudeDesktopDefaultBundleID {
		return ComputerUseToolResult{}, errors.New("wrong bundle")
	}
	if request.Tool == "get_app_state" {
		if f.readErr != nil {
			return ComputerUseToolResult{}, f.readErr
		}
		return claudeAXResult(f.snapshotLocked())
	}
	if f.mutateErr != nil {
		return ComputerUseToolResult{}, f.mutateErr
	}
	switch request.Tool {
	case "set_value":
		if request.Value == nil {
			return ComputerUseToolResult{}, errors.New("missing value")
		}
		f.prompt = *request.Value
		f.stage = 1
	case "press":
		if f.stage != 1 {
			return ComputerUseToolResult{}, errors.New("pressed before input")
		}
		f.stage = 2
	default:
		return ComputerUseToolResult{}, errors.New("unexpected tool")
	}
	return ComputerUseToolResult{Text: `{"ok":true}`}, nil
}

func (f *fakeClaudeComputerUse) snapshotLocked() []claudeAXElement {
	if !f.opened {
		return []claudeAXElement{{Role: "AXWindow", Label: "Claude", Path: []int{0}}}
	}
	value := ""
	if f.stage == 1 {
		value = f.prompt
	}
	return []claudeAXElement{
		{Role: "AXHeading", Label: "Exact session", Current: claudeTestBool(true), Path: []int{0, 0}},
		{Role: "AXTextArea", Label: "Message Claude", Value: value, Focused: claudeTestBool(true), Path: []int{0, 1}},
		{Role: "AXButton", Label: "Send message", Actionable: true, Path: []int{0, 2}},
	}
}

func (f *fakeClaudeComputerUse) calls() []ComputerUseToolRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ComputerUseToolRequest(nil), f.requests...)
}

func claudeAXResult(elements []claudeAXElement) (ComputerUseToolResult, error) {
	text, err := json.Marshal(map[string]any{
		"ok":            true,
		"accessibility": map[string]any{"elements": elements},
	})
	return ComputerUseToolResult{Text: string(text)}, err
}

func testClaudeComputerUse(t *testing.T, fake *fakeClaudeComputerUse) *Claude {
	t.Helper()
	c := NewClaude("claude", config.ProviderConfig{Extra: map[string]any{
		"desktop_bundle_id": claudeDesktopDefaultBundleID,
	}})
	c.SetComputerUseAutomationHandler(fake.handler)
	c.SetClaudeControlRouteCommitHandler(func(context.Context, string, string) error { return nil })
	c.claudeComputerUseSetDependencies(claudeComputerUseDependencies{
		verifyApp: func(context.Context, string, string, string) error { return nil },
		launchApp: func(context.Context, string) error { return nil },
		waitApp:   func(context.Context, string) error { return nil },
		openURL:   fake.openURL,
		sessions: func() []map[string]any {
			return []map[string]any{{
				"native_session_id": "desktop-1", "cli_session_id": claudeComputerUseTestTranscript,
				"title": "Exact session", "updated_at": "2026-08-11T01:00:00Z",
			}}
		},
		records: func(string) []map[string]any {
			if fake.stage != 2 {
				return nil
			}
			return []map[string]any{{
				"type": "user", "uuid": "message-1", "timestamp": "2026-08-11T02:00:01Z",
				"message": map[string]any{"content": fake.prompt},
			}}
		},
		sleep: func(context.Context, time.Duration) error { return nil },
		now:   func() time.Time { return time.Date(2026, 8, 11, 2, 0, 0, 0, time.UTC) },
	})
	c.BindTranscript("logical-1", claudeComputerUseTestTranscript)
	return c
}

func TestClaudeComputerUsePromptUsesExactBundleAndFreshInspection(t *testing.T) {
	fake := &fakeClaudeComputerUse{}
	c := testClaudeComputerUse(t, fake)
	outcome := c.claudeComputerUseSendPrompt(context.Background(), "logical-1", "private prompt")
	if outcome.Err != nil || outcome.Disposition != claudeComputerUseConfirmed {
		t.Fatalf("outcome=%#v", outcome)
	}
	if outcome.TranscriptID != claudeComputerUseTestTranscript || outcome.DesktopSessionID != "desktop-1" {
		t.Fatalf("lost exact session binding: %#v", outcome)
	}
	if route := c.claudeComputerUseRoute("logical-1"); route != claudeRouteDesktopComputerUse {
		t.Fatalf("route=%q", route)
	}
	calls := fake.calls()
	mutations := 0
	for i, call := range calls {
		if call.Tool != "set_value" && call.Tool != "press" {
			continue
		}
		mutations++
		if i == 0 || calls[i-1].Tool != "get_app_state" {
			t.Fatalf("mutation %d lacked a fresh inspection: %#v", i, calls)
		}
		if call.BundleID != claudeDesktopDefaultBundleID || call.App != "" {
			t.Fatalf("mutation did not use exact bundle routing: %#v", call)
		}
	}
	if mutations != 2 {
		t.Fatalf("mutations=%d calls=%#v", mutations, calls)
	}
}

func TestClaudeNewSessionAutoModeBindsAfterRelockAndConfirmsTranscript(t *testing.T) {
	fake := &fakeClaudeNewComputerUse{requireAutoConfirm: true}
	c := NewClaude("claude", config.ProviderConfig{Extra: map[string]any{
		"desktop_bundle_id": claudeDesktopDefaultBundleID,
	}})
	c.SetComputerUseAutomationHandler(fake.handler)
	c.SetClaudeControlRouteCommitHandler(func(context.Context, string, string) error { return nil })
	c.claudeComputerUseSetDependencies(claudeComputerUseDependencies{
		verifyApp: func(context.Context, string, string, string) error { return nil },
		launchApp: func(context.Context, string) error { return nil },
		waitApp:   func(context.Context, string) error { return nil },
		openURL:   fake.openURL,
		sessions:  fake.sessions,
		records: func(string) []map[string]any {
			fake.mu.Lock()
			defer fake.mu.Unlock()
			if fake.postSendPolls < 3 {
				return nil
			}
			return []map[string]any{{
				"type": "user", "uuid": "new-message-1", "timestamp": "2026-08-11T02:00:03Z",
				"message": map[string]any{"content": fake.prompt},
			}}
		},
		sleep: func(context.Context, time.Duration) error { return nil },
		now:   func() time.Time { return time.Date(2026, 8, 11, 2, 0, 0, 0, time.UTC) },
	})
	if _, err := c.OpenOrCreateSession("logical-new", StartOptions{Mode: "auto"}); err != nil {
		t.Fatal(err)
	}
	outcome := c.claudeComputerUseSendPrompt(context.Background(), "logical-new", "first Desktop prompt")
	if outcome.Err != nil || outcome.Disposition != claudeComputerUseConfirmed ||
		outcome.TranscriptID != claudeComputerUseTestTranscript || outcome.DesktopSessionID != "desktop-new" {
		t.Fatalf("outcome=%#v", outcome)
	}
	fake.mu.Lock()
	polledBeforeReturn := fake.polledBeforeReturn
	polls := fake.postSendPolls
	fake.mu.Unlock()
	if polledBeforeReturn || polls < 3 {
		t.Fatalf("metadata polling held the Locked Use window: before_return=%v polls=%d", polledBeforeReturn, polls)
	}
	calls := fake.requests
	foundAutoPress, foundAutoConfirm, foundComposerFocus := false, false, false
	for _, call := range calls {
		if call.Tool == "press" && len(call.Path) == 3 && call.Path[1] == 4 && call.Path[2] == 1 {
			foundAutoPress = true
		}
		if call.Tool == "focus" && len(call.Path) == 3 && call.Path[2] == 1 {
			foundComposerFocus = true
		}
		if call.Tool == "press" && len(call.Path) == 3 && call.Path[1] == 5 && call.Path[2] == 2 {
			foundAutoConfirm = true
		}
	}
	if !foundAutoPress || !foundAutoConfirm || !foundComposerFocus {
		t.Fatalf("Auto mode was not selected and refocused exactly: %#v", calls)
	}
}

func TestClaudeComputerUseFailureClassificationPreAndPostMutation(t *testing.T) {
	t.Run("preflight may fall back", func(t *testing.T) {
		fake := &fakeClaudeComputerUse{readErr: computeruse.ErrHelperUnavailable}
		c := testClaudeComputerUse(t, fake)
		outcome := c.claudeComputerUseSendPrompt(context.Background(), "logical-1", "do not log me")
		if outcome.Disposition != claudeComputerUseNotAttempted || !outcome.canFallback() {
			t.Fatalf("outcome=%#v", outcome)
		}
		if c.claudeComputerUseRoute("logical-1") != "" {
			t.Fatal("preflight failure bound a route")
		}
	})

	t.Run("generic broker failure is fail closed", func(t *testing.T) {
		fake := &fakeClaudeComputerUse{readErr: errors.New("unexpected broker failure")}
		c := testClaudeComputerUse(t, fake)
		outcome := c.claudeComputerUseSendPrompt(context.Background(), "logical-1", "do not retry")
		if outcome.Disposition != claudeComputerUseDeliveryUnknown || outcome.canFallback() {
			t.Fatalf("outcome=%#v", outcome)
		}
		if calls := fake.calls(); len(calls) != 1 || calls[0].Tool != "get_app_state" {
			t.Fatalf("generic failure retried or mutated: %#v", calls)
		}
	})

	t.Run("deep link uncertainty forbids fallback", func(t *testing.T) {
		fake := &fakeClaudeComputerUse{}
		c := testClaudeComputerUse(t, fake)
		c.claudeComputerUseSetDependencies(claudeComputerUseDependencies{
			openURL: func(context.Context, string, string) error { return errors.New("maybe delivered") },
		})
		outcome := c.claudeComputerUseSendPrompt(context.Background(), "logical-1", "sensitive")
		if outcome.Disposition != claudeComputerUseDeliveryUnknown || outcome.canFallback() {
			t.Fatalf("outcome=%#v", outcome)
		}
		if strings.Contains(outcome.Err.Error(), "sensitive") {
			t.Fatal("error disclosed prompt text")
		}
		if c.claudeComputerUseRoute("logical-1") != claudeRouteDesktopComputerUse {
			t.Fatal("uncertain mutation did not pin the Desktop route")
		}
	})

	t.Run("input uncertainty forbids fallback", func(t *testing.T) {
		fake := &fakeClaudeComputerUse{mutateErr: errors.New("unknown result")}
		c := testClaudeComputerUse(t, fake)
		outcome := c.claudeComputerUseSendPrompt(context.Background(), "logical-1", "sensitive")
		if outcome.Disposition != claudeComputerUseDeliveryUnknown || outcome.canFallback() {
			t.Fatalf("outcome=%#v", outcome)
		}
	})
}

func TestClaudeRouteCommitBarrierPrecedesDesktopAndCLISideEffects(t *testing.T) {
	t.Run("Desktop commit blocks deep link and AX mutation", func(t *testing.T) {
		fake := &fakeClaudeComputerUse{}
		c := testClaudeComputerUse(t, fake)
		root, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		c.cfg.Extra["interaction_dir"] = filepath.Join(root, "interactions")
		entered := make(chan struct{})
		release := make(chan struct{})
		c.SetClaudeControlRouteCommitHandler(func(_ context.Context, sessionID, route string) error {
			if sessionID != "logical-1" || route != claudeRouteDesktopComputerUse {
				return errors.New("wrong Desktop route commit identity")
			}
			close(entered)
			<-release
			return errors.New("durable store unavailable")
		})
		result := make(chan claudeComputerUseOutcome, 1)
		go func() {
			result <- c.claudeComputerUseSendPrompt(context.Background(), "logical-1", "must stay unsent",
				&claudePromptAttemptSpec{operationID: "commit-desktop-1", prompt: "must stay unsent"})
		}()
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatal("Desktop route commit barrier was not reached")
		}
		fake.mu.Lock()
		opened := fake.opened
		fake.mu.Unlock()
		calls := fake.calls()
		if opened || len(calls) != 1 || calls[0].Tool != "get_app_state" {
			t.Fatalf("Desktop side effect escaped blocked commit: opened=%v calls=%#v", opened, calls)
		}
		if _, err := turnstatehook.ReadInteractionAttempt(
			filepath.Join(root, "interactions"), "prompt:commit-desktop-1",
		); !errors.Is(err, turnstatehook.ErrInteractionNotFound) {
			t.Fatalf("prompt tombstone preceded failed route commit: %v", err)
		}
		close(release)
		outcome := <-result
		if outcome.Disposition != claudeComputerUseDeliveryUnknown || outcome.canFallback() {
			t.Fatalf("Desktop commit failure outcome=%#v", outcome)
		}
	})

	t.Run("CLI commit blocks process start and prompt attempt", func(t *testing.T) {
		root, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		interactionDir := filepath.Join(root, "interactions")
		c := NewClaude("claude", config.ProviderConfig{Command: "/bin/false", Extra: map[string]any{
			"primary_route": claudeRouteDesktopComputerUse, "fallback_route": claudeRouteStreamJSONCLI,
			"interaction_dir": interactionDir,
		}})
		entered := make(chan struct{})
		release := make(chan struct{})
		c.SetClaudeControlRouteCommitHandler(func(_ context.Context, sessionID, route string) error {
			if sessionID != "logical-cli-fallback" || route != claudeRouteStreamJSONCLI {
				return errors.New("wrong CLI route commit identity")
			}
			close(entered)
			<-release
			return errors.New("durable store unavailable")
		})
		result := make(chan SendResult, 1)
		go func() {
			result <- c.SendPromptOperation("logical-cli-fallback", "must not reach CLI", "commit-cli-1")
		}()
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatal("CLI route commit barrier was not reached")
		}
		if sessions := c.cliStream.Sessions(); len(sessions) != 0 {
			t.Fatalf("CLI process started before durable route commit: %#v", sessions)
		}
		if _, err := turnstatehook.ReadInteractionAttempt(
			interactionDir, "prompt:commit-cli-1",
		); !errors.Is(err, turnstatehook.ErrInteractionNotFound) {
			t.Fatalf("CLI prompt tombstone preceded failed route commit: %v", err)
		}
		close(release)
		sendResult := <-result
		if sendResult.OK || sendResult.State != "needs_manual" {
			t.Fatalf("CLI commit failure result=%#v", sendResult)
		}
		if sessions := c.cliStream.Sessions(); len(sessions) != 0 {
			t.Fatalf("CLI process started after failed durable commit: %#v", sessions)
		}
	})
}

func TestClaudeCloseSessionClearsEphemeralOwnershipButKeepsDurableAttempts(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	interactionDir := filepath.Join(root, "interactions")
	c := NewClaude("claude", config.ProviderConfig{Extra: map[string]any{
		"interaction_dir": interactionDir,
	}})
	logicalID := "logical-close"
	transcriptID := claudeComputerUseTestTranscript
	c.BindClaudeControlStartOptions(logicalID, StartOptions{Cwd: root, Mode: "auto"})
	c.BindClaudeControlRoute(logicalID, transcriptID, claudeRouteDesktopComputerUse, root)
	c.sessions[logicalID] = transcriptID
	runtime := claudeComputerUseRuntimeFor(c)
	runtime.mu.Lock()
	runtime.active[logicalID] = true
	runtime.mu.Unlock()
	c.streamMu.Lock()
	c.streamTargets[transcriptID] = map[string]bool{logicalID: true}
	c.streamTextDelta[logicalID] = true
	c.streamTextDelta[transcriptID] = true
	c.streamTools[logicalID] = map[string]map[string]any{"tool": {"name": "Bash"}}
	c.streamTools[transcriptID] = map[string]map[string]any{"tool": {"name": "Bash"}}
	c.streamControls[logicalID] = map[string]*claudeControlRequest{"request": {RequestID: "request"}}
	c.streamControls[transcriptID] = map[string]*claudeControlRequest{"request": {RequestID: "request"}}
	c.streamControlOrder[logicalID] = []string{"request"}
	c.streamControlOrder[transcriptID] = []string{"request"}
	c.recoveredQuestions[transcriptID] = map[string]any{"request_id": "request"}
	c.recoveredQuestionSeen[transcriptID] = true
	c.streamMu.Unlock()
	if _, err := turnstatehook.BeginInteractionAttempt(
		interactionDir, "prompt:kept-after-close", strings.Repeat("a", 64), logicalID, "prompt",
		strings.Repeat("b", 64),
	); err != nil {
		t.Fatal(err)
	}

	c.CloseSession(logicalID)
	runtime.mu.Lock()
	_, hasRoute := runtime.routes[logicalID]
	_, active := runtime.active[logicalID]
	_, hasOptions := runtime.startOptions[logicalID]
	runtime.mu.Unlock()
	if hasRoute || active || hasOptions || c.sessions[logicalID] != "" || c.transcriptID(logicalID) != logicalID {
		t.Fatalf("CloseSession retained ephemeral ownership: route=%v active=%v options=%v sessions=%q transcript=%q",
			hasRoute, active, hasOptions, c.sessions[logicalID], c.transcriptID(logicalID))
	}
	c.streamMu.RLock()
	_, target := c.streamTargets[transcriptID]
	_, logicalControl := c.streamControls[logicalID]
	_, transcriptControl := c.streamControls[transcriptID]
	_, question := c.recoveredQuestions[transcriptID]
	_, questionSeen := c.recoveredQuestionSeen[transcriptID]
	c.streamMu.RUnlock()
	if target || logicalControl || transcriptControl || question || questionSeen {
		t.Fatalf("CloseSession retained pending in-memory state: target=%v controls=%v/%v question=%v/%v",
			target, logicalControl, transcriptControl, question, questionSeen)
	}
	attempt, err := turnstatehook.ReadInteractionAttempt(interactionDir, "prompt:kept-after-close")
	if err != nil || attempt.State != "attempted" {
		t.Fatalf("CloseSession removed durable exact-once tombstone: attempt=%#v err=%v", attempt, err)
	}
}

func TestClaudeAXParserAndSelectorFailClosed(t *testing.T) {
	result, err := claudeAXResult([]claudeAXElement{
		{Role: "AXTextArea", Label: "Message Claude", Path: []int{0, 1}},
		{Role: "AXTextArea", Label: "Message Claude", Path: []int{0, 2}},
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := parseClaudeAXSnapshot(result)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := claudeComposer(snapshot); err == nil {
		t.Fatal("ambiguous composers were accepted")
	}

	duplicate, err := claudeAXResult([]claudeAXElement{
		{Role: "AXButton", Label: "Send", Actionable: true, Path: []int{0, 1}},
		{Role: "AXButton", Label: "Deny", Actionable: true, Path: []int{0, 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseClaudeAXSnapshot(duplicate); err == nil {
		t.Fatal("duplicate AX paths were accepted")
	}
}

func TestClaudeTargetRejectsSidebarTitleWhenAnotherSessionIsSelected(t *testing.T) {
	target := claudeDesktopTarget{
		cliID: claudeComputerUseTestTranscript, desktopID: "desktop-1", title: "Exact session", titleUnique: true,
	}
	tx := claudeDesktopTransaction{target: &target}
	snapshot := claudeAXSnapshot{elements: []claudeAXElement{
		{Role: "AXStaticText", Label: "Exact session", Selected: claudeTestBool(false), Path: []int{0, 1}},
		{Role: "AXStaticText", Label: "Different session", Selected: claudeTestBool(true), Path: []int{0, 2}},
		{Role: "AXTextArea", Label: "Message Claude", Focused: claudeTestBool(true), Path: []int{0, 3}},
	}}
	if err := tx.verifyTarget(snapshot); err == nil {
		t.Fatal("a matching sidebar title overrode the selected current session")
	}
}

func TestClaudeSecurityRefusalNeverAllowsCLIFallback(t *testing.T) {
	for _, refusal := range []error{computeruse.ErrLocalInput, computeruse.ErrWindowBusy} {
		t.Run(refusal.Error(), func(t *testing.T) {
			fake := &fakeClaudeComputerUse{readErr: refusal}
			c := testClaudeComputerUse(t, fake)
			outcome := c.claudeComputerUseSendPrompt(context.Background(), "logical-1", "must not reach CLI")
			if outcome.canFallback() || outcome.Disposition != claudeComputerUseDeliveryUnknown {
				t.Fatalf("security refusal outcome=%#v", outcome)
			}
			if calls := fake.calls(); len(calls) != 1 || calls[0].Tool != "get_app_state" {
				t.Fatalf("security refusal was retried or mutated: %#v", calls)
			}
		})
	}
}

func TestClaudeDesktopDeepLinksAreExact(t *testing.T) {
	resume, err := claudeDesktopDeepLink(claudeDesktopTarget{cliID: claudeComputerUseTestTranscript})
	if err != nil || resume != "claude://resume?session=11111111-2222-4333-8444-555555555555" {
		t.Fatalf("resume=%q err=%v", resume, err)
	}
	created, err := claudeDesktopDeepLink(claudeDesktopTarget{new: true})
	if err != nil || created != "claude://code/new" {
		t.Fatalf("new=%q err=%v", created, err)
	}
}

func TestClaudePromptOperationLedgerBlocksDuplicateAfterProviderRestart(t *testing.T) {
	fake := &fakeClaudeComputerUse{}
	c := testClaudeComputerUse(t, fake)
	tempRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	interactionDir := filepath.Join(tempRoot, "interactions")
	c.cfg.Extra["interaction_dir"] = interactionDir
	if _, err := turnstatehook.ReadInteractionAttempt(interactionDir, "prompt:task-1"); !errors.Is(err, turnstatehook.ErrInteractionNotFound) {
		t.Fatalf("fresh prompt ledger err=%v", err)
	}
	first := c.claudeComputerUseSendPrompt(context.Background(), "logical-1", "exact once", &claudePromptAttemptSpec{
		operationID: "task-1", prompt: "exact once",
	})
	if first.Disposition != claudeComputerUseConfirmed {
		t.Fatalf("first=%#v err=%v", first, first.Err)
	}
	before := len(fake.calls())

	restarted := testClaudeComputerUse(t, fake)
	restarted.cfg.Extra["interaction_dir"] = interactionDir
	second := restarted.SendPromptOperation("logical-1", "exact once", "task-1")
	if second.OK || second.State != "needs_manual" {
		t.Fatalf("duplicate=%#v", second)
	}
	if after := len(fake.calls()); after != before {
		t.Fatalf("duplicate operation opened or mutated UI: before=%d after=%d calls=%#v", before, after, fake.calls())
	}
	attempt, err := turnstatehook.ReadInteractionAttempt(interactionDir, "prompt:task-1")
	if err != nil || attempt.State != "resolved" || attempt.DecisionDigest == "" ||
		strings.Contains(mustJSON(attempt), "exact once") {
		t.Fatalf("prompt tombstone=%#v err=%v", attempt, err)
	}
}

func TestClaudePromptAttemptedAfterSendUncertaintyBlocksRestartRetry(t *testing.T) {
	fake := &fakeClaudeComputerUse{}
	c := testClaudeComputerUse(t, fake)
	tempRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	interactionDir := filepath.Join(tempRoot, "interactions")
	c.cfg.Extra["interaction_dir"] = interactionDir
	c.claudeComputerUseSetDependencies(claudeComputerUseDependencies{
		records: func(string) []map[string]any { return nil },
		sleep:   func(context.Context, time.Duration) error { return context.DeadlineExceeded },
	})
	first := c.SendPromptOperation("logical-1", "uncertain once", "task-uncertain")
	if first.OK || first.State != "needs_manual" {
		t.Fatalf("first uncertain result=%#v", first)
	}
	before := len(fake.calls())
	sendPresses := 0
	for _, call := range fake.calls() {
		if call.Tool == "press" {
			sendPresses++
		}
	}
	if sendPresses != 1 {
		t.Fatalf("first operation side effects=%#v", fake.calls())
	}
	attempt, err := turnstatehook.ReadInteractionAttempt(interactionDir, "prompt:task-uncertain")
	if err != nil || attempt.State != "attempted" {
		t.Fatalf("attempt=%#v err=%v", attempt, err)
	}

	restarted := testClaudeComputerUse(t, fake)
	restarted.cfg.Extra["interaction_dir"] = interactionDir
	second := restarted.SendPromptOperation("logical-1", "uncertain once", "task-uncertain")
	if second.OK || len(fake.calls()) != before {
		t.Fatalf("restart retried uncertain Desktop prompt: second=%#v calls=%#v", second, fake.calls())
	}
}

func TestClaudeCLIPromptAttemptBlocksSecondSendAfterAcceptedError(t *testing.T) {
	tempRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c := NewClaudeCLI("claude", config.ProviderConfig{Extra: map[string]any{
		"interaction_dir": filepath.Join(tempRoot, "interactions"),
	}})
	sends := 0
	fakeSend := func(string, []map[string]any) SendResult {
		sends++
		msg := "transport lost after accepting prompt"
		return SendResult{OK: false, State: "error", Error: &msg}
	}
	spec := &claudePromptAttemptSpec{operationID: "task-cli-uncertain", prompt: "one CLI send"}
	content := []map[string]any{{"type": "text", "text": "one CLI send"}}
	first := c.deliverClaudeCLIPromptExactOnce("logical-cli", content, spec, fakeSend)
	second := c.deliverClaudeCLIPromptExactOnce("logical-cli", content, spec, fakeSend)
	if first.OK || second.OK || sends != 1 || second.State != "needs_manual" {
		t.Fatalf("first=%#v second=%#v sends=%d", first, second, sends)
	}
	attempt, err := turnstatehook.ReadInteractionAttempt(
		filepath.Join(tempRoot, "interactions"), "prompt:task-cli-uncertain",
	)
	if err != nil || attempt.State != "attempted" {
		t.Fatalf("CLI attempt=%#v err=%v", attempt, err)
	}
}

func TestClaudeComputerUseCleanupFailureNeverFallsBack(t *testing.T) {
	fake := &fakeClaudeComputerUse{readErr: errors.New("AX capability unavailable")}
	c := testClaudeComputerUse(t, fake)
	c.SetComputerUseAutomationHandler(func(
		ctx context.Context, sessionID string, callback ComputerUseAutomationCallback,
	) error {
		callbackErr := callback(ctx, fake.tool)
		return errors.Join(callbackErr, ErrComputerUseAutomationCleanup)
	})
	outcome := c.claudeComputerUseSendPrompt(context.Background(), "logical-1", "never fallback")
	if outcome.Disposition != claudeComputerUseDeliveryUnknown || outcome.canFallback() {
		t.Fatalf("cleanup outcome=%#v", outcome)
	}
	if calls := fake.calls(); len(calls) != 1 || calls[0].Tool != "get_app_state" {
		t.Fatalf("cleanup failure retried or mutated: %#v", calls)
	}

	control := c.claudeComputerUseControl(
		context.Background(), "logical-1", nil, nil,
		func(context.Context, *claudeDesktopTransaction, claudeComputerUseDependencies) error {
			return errors.New("must not operate")
		},
	)
	if control.Disposition != claudeComputerUseDeliveryUnknown {
		t.Fatalf("control cleanup outcome=%#v", control)
	}
}

func TestClaudeInteractionRejectsParallelNativeCardsWithoutRequestIDs(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	projectDir := filepath.Join(root, "project")
	if err := os.MkdirAll(projectDir, 0o700); err != nil {
		t.Fatal(err)
	}
	transcriptPath := filepath.Join(projectDir, claudeComputerUseTestTranscript+".jsonl")
	lines := strings.Join([]string{
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tool-1","name":"Bash","input":{"command":"one"}}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tool-2","name":"Bash","input":{"command":"two"}}]}}`,
	}, "\n")
	if err := os.WriteFile(transcriptPath, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	c := NewClaude("claude", config.ProviderConfig{Extra: map[string]any{"claude_projects_dir": root}})
	c.BindTranscript("logical-1", claudeComputerUseTestTranscript)
	c.bindClaudeComputerUseRoute("logical-1", claudeRouteDesktopComputerUse)
	c.claudeComputerUseSetDependencies(claudeComputerUseDependencies{
		interactions: func() ([]turnstatehook.InteractionRecord, error) {
			return []turnstatehook.InteractionRecord{
				{RequestID: "tool-1", ToolUseID: "tool-1", SessionID: claudeComputerUseTestTranscript,
					TranscriptPath: transcriptPath, HookEventName: "PermissionRequest", ToolName: "Bash",
					ToolInput: map[string]any{"command": "one"}},
				{RequestID: "tool-2", ToolUseID: "tool-2", SessionID: claudeComputerUseTestTranscript,
					TranscriptPath: transcriptPath, HookEventName: "PermissionRequest", ToolName: "Bash",
					ToolInput: map[string]any{"command": "two"}},
			}, nil
		},
	})
	if _, err := c.claudeDesktopInteraction("logical-1", "tool-1", "Bash"); err == nil ||
		!strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("parallel cards err=%v", err)
	}
}

func TestClaudeSingleSelectUsesExactTextButtonAndScopedNavigation(t *testing.T) {
	snapshot := claudeAXSnapshot{elements: []claudeAXElement{
		{Role: "AXStaticText", Label: "Choose one", Path: []int{0, 3, 0}},
		{Role: "AXButton", Label: "1 First option Detailed explanation", Actionable: true, Path: []int{0, 3, 1}},
		{Role: "AXStaticText", Label: "First option", Path: []int{0, 3, 1, 0}},
		{Role: "AXStaticText", Label: "Detailed explanation", Path: []int{0, 3, 1, 1}},
		{Role: "AXButton", Label: "Submit", Actionable: true, Path: []int{0, 3, 9}},
		{Role: "AXButton", Label: "Submit", Actionable: true, Path: []int{0, 8, 1}},
	}}
	button, err := claudeQuestionOption(snapshot, "Choose one", "First option", false, false)
	if err != nil || len(button.Path) != 3 || button.Path[2] != 1 {
		t.Fatalf("single option=%#v err=%v", button, err)
	}
	if _, err := claudeQuestionOption(snapshot, "Choose one", "First option", false, true); err == nil {
		t.Fatal("single-select falsely inferred an unknown checked state")
	}
	submit, err := claudeSubmitAnswersButton(snapshot, "Choose one")
	if err != nil || submit.Path[1] != 3 {
		t.Fatalf("scoped submit=%#v err=%v", submit, err)
	}
}

func TestClaudeQuestionStateMachineHandlesSingleOtherMultiNextAndScopedSubmit(t *testing.T) {
	questionIndex := 0
	otherVisible := false
	otherValue := ""
	multiSelected := map[string]bool{}
	colorSelected := false
	submitted := false
	calls := make([]ComputerUseToolRequest, 0, 32)
	snapshot := func() []claudeAXElement {
		elements := []claudeAXElement{{
			Role: "AXHeading", Label: "Exact session", Current: claudeTestBool(true), Path: []int{0, 0},
		}}
		if submitted {
			return elements
		}
		switch questionIndex {
		case 0:
			elements = append(elements,
				claudeAXElement{Role: "AXStaticText", Label: "Describe the goal", Path: []int{0, 3, 0}},
				claudeAXElement{Role: "AXButton", Label: "3 Other answer", Actionable: true, Path: []int{0, 3, 1}},
				claudeAXElement{Role: "AXStaticText", Label: "Other", Path: []int{0, 3, 1, 0}},
				claudeAXElement{Role: "AXButton", Label: "Next", Actionable: true,
					Enabled: claudeTestBool(otherValue != ""), Path: []int{0, 3, 9}},
			)
			if otherVisible {
				elements = append(elements, claudeAXElement{
					Role: "AXTextField", Label: "Other option", Value: otherValue, Path: []int{0, 3, 2},
				})
			}
		case 1:
			elements = append(elements,
				claudeAXElement{Role: "AXStaticText", Label: "Pick features", Path: []int{0, 4, 0}},
				claudeAXElement{Role: "AXButton", Label: "1 Fast", Actionable: true,
					Pressed: claudeTestBool(multiSelected["Fast"]), Path: []int{0, 4, 1}},
				claudeAXElement{Role: "AXStaticText", Label: "Fast", Path: []int{0, 4, 1, 0}},
				claudeAXElement{Role: "AXButton", Label: "2 Safe", Actionable: true,
					Pressed: claudeTestBool(multiSelected["Safe"]), Path: []int{0, 4, 2}},
				claudeAXElement{Role: "AXStaticText", Label: "Safe", Path: []int{0, 4, 2, 0}},
				claudeAXElement{Role: "AXButton", Label: "Next", Actionable: true,
					Enabled: claudeTestBool(multiSelected["Fast"] && multiSelected["Safe"]), Path: []int{0, 4, 9}},
			)
		case 2:
			elements = append(elements,
				claudeAXElement{Role: "AXStaticText", Label: "Pick a color", Path: []int{0, 5, 0}},
				claudeAXElement{Role: "AXButton", Label: "1 Red Primary color", Actionable: true,
					Path: []int{0, 5, 1}},
				claudeAXElement{Role: "AXStaticText", Label: "Red", Path: []int{0, 5, 1, 0}},
				claudeAXElement{Role: "AXButton", Label: "Submit", Actionable: true,
					Enabled: claudeTestBool(colorSelected), Path: []int{0, 5, 9}},
				// An unrelated Submit exists elsewhere in the app. The selector must
				// choose the button sharing the current question card's deeper path.
				claudeAXElement{Role: "AXButton", Label: "Submit", Actionable: true, Path: []int{0, 8, 1}},
			)
		}
		return elements
	}
	tool := func(_ context.Context, request ComputerUseToolRequest) (ComputerUseToolResult, error) {
		calls = append(calls, request)
		if request.Tool == "get_app_state" {
			return claudeAXResult(snapshot())
		}
		switch request.Tool {
		case "press":
			switch {
			case questionIndex == 0 && sameInts(request.Path, []int{0, 3, 1}):
				otherVisible = true
			case questionIndex == 0 && sameInts(request.Path, []int{0, 3, 9}) && otherValue != "":
				questionIndex = 1
			case questionIndex == 1 && sameInts(request.Path, []int{0, 4, 1}):
				multiSelected["Fast"] = true
			case questionIndex == 1 && sameInts(request.Path, []int{0, 4, 2}):
				multiSelected["Safe"] = true
			case questionIndex == 1 && sameInts(request.Path, []int{0, 4, 9}) &&
				multiSelected["Fast"] && multiSelected["Safe"]:
				questionIndex = 2
			case questionIndex == 2 && sameInts(request.Path, []int{0, 5, 1}):
				colorSelected = true
			case questionIndex == 2 && sameInts(request.Path, []int{0, 5, 9}) && colorSelected:
				submitted = true
			default:
				return ComputerUseToolResult{}, errors.New("pressed an inexact question control")
			}
		case "set_value":
			if questionIndex != 0 || !otherVisible || request.Value == nil ||
				!sameInts(request.Path, []int{0, 3, 2}) {
				return ComputerUseToolResult{}, errors.New("mutated an inexact Other field")
			}
			otherValue = *request.Value
		default:
			return ComputerUseToolResult{}, errors.New("unexpected question tool")
		}
		return ComputerUseToolResult{Text: `{"ok":true}`}, nil
	}
	target := claudeDesktopTarget{title: "Exact session", titleUnique: true}
	tx := &claudeDesktopTransaction{tool: tool, bundleID: claudeDesktopDefaultBundleID, target: &target}
	plans := []claudeQuestionPlan{
		{question: "Describe the goal", selected: []string{"Other"}, other: "Keep it quiet"},
		{question: "Pick features", multi: true, selected: []string{"Fast", "Safe"}},
		{question: "Pick a color", selected: []string{"Red"}},
	}
	if err := claudeOperateQuestionAnswers(context.Background(), tx, plans); err != nil {
		t.Fatal(err)
	}
	if !submitted || !tx.confirmed || otherValue != "Keep it quiet" {
		t.Fatalf("state submitted=%v confirmed=%v other=%q", submitted, tx.confirmed, otherValue)
	}
	for index, call := range calls {
		if call.Tool != "press" && call.Tool != "set_value" {
			continue
		}
		if index == 0 || calls[index-1].Tool != "get_app_state" {
			t.Fatalf("question mutation lacked fresh inspection: index=%d calls=%#v", index, calls)
		}
	}
}

func sameInts(left, right []int) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func TestClaudeQuestionToolResultRequiresExactQuestionAnswerMapping(t *testing.T) {
	input := map[string]any{"questions": []any{
		map[string]any{
			"question": "Pick a color", "multiSelect": false,
			"options": []any{map[string]any{"label": "Red"}, map[string]any{"label": "Blue"}},
		},
		map[string]any{
			"question": "Pick features", "multiSelect": true,
			"options": []any{map[string]any{"label": "Fast"}, map[string]any{"label": "Safe"}},
		},
	}}
	answers := map[string]QuestionAnswer{
		"Pick a color":  {Selected: []string{"Red"}},
		"Pick features": {Selected: []string{"Fast", "Safe"}, Other: "Quiet"},
	}
	spec := &claudeInteractionAttemptSpec{
		record: turnstatehook.InteractionRecord{ToolUseID: "question-tool-1", ToolInput: input},
		kind:   "question", decision: answers,
	}
	result := func(content string) []map[string]any {
		return []map[string]any{{
			"type": "user",
			"message": map[string]any{"content": []any{map[string]any{
				"type": "tool_result", "tool_use_id": "question-tool-1", "content": content,
			}}},
		}}
	}
	pair := func(question, answer string) string {
		questionJSON, err := json.Marshal(question)
		if err != nil {
			t.Fatal(err)
		}
		answerJSON, err := json.Marshal(answer)
		if err != nil {
			t.Fatal(err)
		}
		return string(questionJSON) + "=" + string(answerJSON)
	}
	valid := claudeQuestionResultPrefix + pair("Pick a color", "Red") + ", " +
		pair("Pick features", "Fast, Safe, Quiet")
	if !claudeTranscriptHasInteractionResult(result(valid), spec) {
		t.Fatal("exact native AskUserQuestion result was rejected")
	}
	validOtherLabel := claudeQuestionResultPrefix + pair("Pick a color", "Red") + ", " +
		pair("Pick features", "Fast, Safe, Other: Quiet")
	if !claudeTranscriptHasInteractionResult(result(validOtherLabel), spec) {
		t.Fatal("exact Other-labelled native AskUserQuestion result was rejected")
	}

	for name, content := range map[string]string{
		"swapped": claudeQuestionResultPrefix + pair("Pick a color", "Fast, Safe, Quiet") + ", " +
			pair("Pick features", "Red"),
		"extra":              valid + ", " + pair("Unexpected", "answer"),
		"duplicate question": valid + ", " + pair("Pick a color", "Red"),
		"plain string":       "Your questions have been answered",
		"malformed separator": claudeQuestionResultPrefix + pair("Pick a color", "Red") + "; " +
			pair("Pick features", "Fast, Safe, Quiet"),
	} {
		t.Run(name, func(t *testing.T) {
			if claudeTranscriptHasInteractionResult(result(content), spec) {
				t.Fatalf("non-exact AskUserQuestion result was accepted: %q", content)
			}
		})
	}

	duplicateRecords := append(result(valid), result(valid)[0])
	if claudeTranscriptHasInteractionResult(duplicateRecords, spec) {
		t.Fatal("duplicate tool_result records were accepted")
	}
}

func TestClaudeWorkingDirectoryTooltipRejectsAnotherAppSubtree(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	trigger := claudeAXElement{
		Role: "AXButton", Label: filepath.Base(root), Focused: claudeTestBool(true), Path: []int{0, 3, 2},
	}
	snapshot := claudeAXSnapshot{elements: []claudeAXElement{
		trigger,
		{Role: "AXStaticText", Label: "Working directory", Path: []int{0, 8, 1}},
		{Role: "AXStaticText", Label: root, Path: []int{0, 8, 2}},
	}}
	if err := claudeWorkingDirectoryTooltip(snapshot, trigger, root); err == nil {
		t.Fatal("a tooltip in another app subtree was accepted as cwd proof")
	}
	snapshot.elements[1].Path = []int{0, 3, 8, 1}
	snapshot.elements[2].Path = []int{0, 3, 8, 2}
	if err := claudeWorkingDirectoryTooltip(snapshot, trigger, root); err != nil {
		t.Fatalf("scoped cwd tooltip rejected: %v", err)
	}
}

func TestClaudeAutoModeConfirmationRejectsWrongWorkspaceAndExternalDuplicate(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	snapshot := claudeAXSnapshot{elements: []claudeAXElement{
		{Role: "AXHeading", Label: "Enable auto mode?", Path: []int{0, 5, 0}},
		{Role: "AXStaticText", Label: filepath.Join(root, "wrong"), Path: []int{0, 5, 1}},
		{Role: "AXButton", Label: "Enable auto mode", Actionable: true, Path: []int{0, 5, 2}},
	}}
	if _, found, err := claudeAutoModeConfirmation(snapshot, root); !found || err == nil {
		t.Fatalf("wrong workspace found=%v err=%v", found, err)
	}
	snapshot.elements[1].Label = root
	snapshot.elements = append(snapshot.elements,
		claudeAXElement{Role: "AXButton", Label: "Enable auto mode", Actionable: true, Path: []int{0, 9, 2}},
	)
	if _, found, err := claudeAutoModeConfirmation(snapshot, root); !found || err == nil {
		t.Fatalf("external duplicate confirm found=%v err=%v", found, err)
	}
}

func TestClaudeAutoModeTriggerIsScopedAwayFromPopupItem(t *testing.T) {
	snapshot := claudeAXSnapshot{elements: []claudeAXElement{
		{Role: "AXTextArea", Label: "Message Claude", Focused: claudeTestBool(true), Path: []int{0, 2, 1}},
		{Role: "AXButton", Label: "Send message", Actionable: true, Path: []int{0, 2, 2}},
		{Role: "AXButton", Label: "Auto", Actionable: true, Path: []int{0, 2, 3}},
		{Role: "AXMenuItem", Label: "Auto", Actionable: true, Checked: claudeTestBool(true), Path: []int{0, 7, 1}},
	}}
	trigger, err := claudeModeTrigger(snapshot)
	if err != nil || trigger.Path[1] != 2 {
		t.Fatalf("Auto trigger=%#v err=%v", trigger, err)
	}
}
