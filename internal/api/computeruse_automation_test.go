package api

import (
	"context"
	"encoding/base64"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/psyche08/remote-agent/internal/computeruse"
	"github.com/psyche08/remote-agent/internal/config"
	"github.com/psyche08/remote-agent/internal/provider"
	"github.com/psyche08/remote-agent/internal/state"
)

type fakeComputerUseAutomationHost struct {
	fakePushProvider
	handler provider.ComputerUseAutomationHandler
}

func (f *fakeComputerUseAutomationHost) SetComputerUseAutomationHandler(
	handler provider.ComputerUseAutomationHandler,
) {
	f.handler = handler
}

func newComputerUseAutomationServer(
	t *testing.T, helperPath string,
) (*Server, *fakeComputerUseAutomationHost) {
	t.Helper()
	host := &fakeComputerUseAutomationHost{fakePushProvider: fakePushProvider{id: "claude"}}
	cfg := &config.Config{
		DeviceID:        "device-a",
		DefaultProvider: "claude",
		ComputerUse: config.ComputerUseConfig{
			Enabled: true, HelperSocket: helperPath,
			LockedUse: config.LockedUseConfig{Enabled: true},
		},
		Providers: map[string]config.ProviderConfig{"claude": {}},
	}
	config.ApplyDefaults(cfg)
	srv := NewServer(
		cfg, provider.Registry{"claude": host},
		state.New(filepath.Join(t.TempDir(), "data")),
	)
	if host.handler == nil {
		t.Fatal("NewServer did not install the trusted computer-use automation handler")
	}
	return srv, host
}

func storeComputerUseAutomationSession(t *testing.T, srv *Server) {
	t.Helper()
	if err := srv.store.UpsertSession(state.Record{
		"provider_id": "claude", "session_id": "logical-1",
		"native_session_id": "native-1", "transcript_id": "native-1",
	}); err != nil {
		t.Fatal(err)
	}
}

func startComputerUseAutomationHelper(t *testing.T) *fakeHelper {
	t.Helper()
	owner := ""
	png := append(append([]byte(nil), pngSignature...), []byte("trusted UI")...)
	return startFakeHelper(t, func(req map[string]any) map[string]any {
		switch req["op"] {
		case "window_open":
			owner, _ = req["turn_id"].(string)
			return map[string]any{
				"ok": true, "window_open": true,
				"window_turn_id": owner, "window_closing": false,
			}
		case "window_state":
			return map[string]any{
				"ok": true, "window_open": owner != "",
				"window_turn_id": owner, "window_closing": false,
			}
		case "window_close":
			if req["turn_id"] == owner {
				owner = ""
			}
			return map[string]any{
				"ok": true, "window_open": owner != "",
				"window_turn_id": owner, "window_closing": false,
			}
		case "action":
			if req["action"] == "screen.capture" {
				return map[string]any{
					"ok": true, "media_type": "image/png",
					"image_base64": base64.StdEncoding.EncodeToString(png),
				}
			}
			return map[string]any{"ok": true, "action": req["action"]}
		case "ax_read":
			return map[string]any{"ok": true, "elements": []any{
				map[string]any{"role": "AXTextArea", "label": "Message", "path": []any{0}},
			}}
		default:
			return map[string]any{"ok": true}
		}
	})
}

func TestComputerUseAutomationPreflightDoesNotInvokeCallback(t *testing.T) {
	helper := startComputerUseAutomationHelper(t)
	srv, host := newComputerUseAutomationServer(t, helper.path)
	storeComputerUseAutomationSession(t, srv)

	called := false
	callback := func(context.Context, provider.ComputerUseToolHandler) error {
		called = true
		return nil
	}
	for _, sessionID := range []string{"missing", "native-1"} {
		err := host.handler(context.Background(), sessionID, callback)
		if err == nil || !strings.Contains(err.Error(), "stored logical session") {
			t.Fatalf("session %q preflight err=%v", sessionID, err)
		}
	}
	if called {
		t.Fatal("failed automation preflight invoked the provider callback")
	}
	if seen := helper.seen(); len(seen) != 0 {
		t.Fatalf("failed automation preflight reached helper: %#v", seen)
	}
}

func TestComputerUseAutomationSuccessBindsIdentityAndRelocks(t *testing.T) {
	helper := startComputerUseAutomationHelper(t)
	srv, host := newComputerUseAutomationServer(t, helper.path)
	storeComputerUseAutomationSession(t, srv)

	err := host.handler(context.Background(), "logical-1", func(
		ctx context.Context, tool provider.ComputerUseToolHandler,
	) error {
		srv.computerUseMu.Lock()
		lease := srv.computerUseTurns[computerUseLeaseKey("claude", "logical-1")]
		srv.computerUseMu.Unlock()
		if !lease.Active || lease.ProviderID != "claude" || lease.Target != "logical-1" ||
			!strings.HasPrefix(lease.TurnID, computerUseAutomationTurnPrefix) {
			t.Fatalf("bad callback-scoped automation lease: %#v", lease)
		}

		result, err := tool(ctx, provider.ComputerUseToolRequest{
			ProviderID: "forged", SessionID: "forged", ThreadID: "forged",
			TurnID: "forged", Tool: "get_app_state",
			BundleID: "com.anthropic.claudefordesktop",
		})
		if err != nil {
			return err
		}
		if result.ImageURL == "" || !strings.Contains(result.Text, `"get_app_state"`) {
			t.Fatalf("automation inspection result=%#v", result)
		}
		if _, err = tool(ctx, provider.ComputerUseToolRequest{
			Tool: "focus", BundleID: "com.anthropic.claudefordesktop", Path: []int{0},
		}); err != nil {
			return err
		}
		_, err = tool(ctx, provider.ComputerUseToolRequest{
			ProviderID: "forged", SessionID: "forged", TurnID: "forged",
			Tool: "type_text", Text: "hello",
		})
		return err
	})
	if err != nil {
		t.Fatalf("trusted UI automation: %v", err)
	}

	srv.computerUseMu.Lock()
	lease := srv.computerUseTurns[computerUseLeaseKey("claude", "logical-1")]
	srv.computerUseMu.Unlock()
	if lease.Active {
		t.Fatalf("automation lease remained active after callback: %#v", lease)
	}

	seen := helper.seen()
	if len(seen) < 6 || seen[0]["op"] != "window_open" ||
		seen[len(seen)-1]["op"] != "window_close" {
		t.Fatalf("automation helper sequence did not end in relock: %#v", seen)
	}
	wantOwner := computerUseOwnerID("claude", "logical-1", lease.TurnID)
	for _, request := range seen {
		if op := request["op"]; op == "window_open" || op == "window_state" ||
			op == "window_close" || op == "action" || op == "ax_read" || op == "ax_focus" {
			if request["turn_id"] != nil && request["turn_id"] != wantOwner {
				t.Fatalf("automation request escaped scoped owner: %#v want=%s", request, wantOwner)
			}
		}
	}
	focused := false
	for _, request := range seen {
		if request["op"] == "ax_focus" && request["bundle_id"] == "com.anthropic.claudefordesktop" {
			focused = true
		}
	}
	if !focused {
		t.Fatalf("automation focus did not reach the exact Claude bundle: %#v", seen)
	}
}

func TestComputerUseAutomationCallbackErrorStillRelocks(t *testing.T) {
	helper := startComputerUseAutomationHelper(t)
	srv, host := newComputerUseAutomationServer(t, helper.path)
	storeComputerUseAutomationSession(t, srv)
	wantErr := errors.New("provider UI failed")

	err := host.handler(context.Background(), "logical-1", func(
		ctx context.Context, tool provider.ComputerUseToolHandler,
	) error {
		if _, err := tool(ctx, provider.ComputerUseToolRequest{
			Tool: "get_app_state", App: "Claude",
		}); err != nil {
			return err
		}
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("callback error was not preserved: %v", err)
	}
	seen := helper.seen()
	if len(seen) == 0 || seen[len(seen)-1]["op"] != "window_close" {
		t.Fatalf("callback error did not synchronously relock: %#v", seen)
	}
}

func TestComputerUseAutomationCallbackPanicStillRelocks(t *testing.T) {
	helper := startComputerUseAutomationHelper(t)
	srv, host := newComputerUseAutomationServer(t, helper.path)
	storeComputerUseAutomationSession(t, srv)

	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("automation callback panic was not propagated to the API boundary")
			}
		}()
		_ = host.handler(context.Background(), "logical-1", func(
			ctx context.Context, tool provider.ComputerUseToolHandler,
		) error {
			if _, err := tool(ctx, provider.ComputerUseToolRequest{
				Tool: "get_app_state", App: "Claude",
			}); err != nil {
				return err
			}
			panic("provider UI panic")
		})
	}()
	seen := helper.seen()
	if len(seen) == 0 || seen[len(seen)-1]["op"] != "window_close" {
		t.Fatalf("callback panic did not synchronously relock: %#v", seen)
	}
}

func TestComputerUseAutomationPostUseCleanupWithoutOpening(t *testing.T) {
	helper := startComputerUseAutomationHelper(t)
	srv, host := newComputerUseAutomationServer(t, helper.path)
	storeComputerUseAutomationSession(t, srv)

	if err := host.handler(context.Background(), "logical-1", func(
		context.Context, provider.ComputerUseToolHandler,
	) error {
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	srv.computerUseMu.Lock()
	lease := srv.computerUseTurns[computerUseLeaseKey("claude", "logical-1")]
	_, ended := srv.computerUseEnded[computerUseIdentity("claude", "logical-1", lease.TurnID)]
	srv.computerUseMu.Unlock()
	if lease.Active || !ended {
		t.Fatalf("no-op automation transaction was not retired: lease=%#v ended=%v", lease, ended)
	}
	if seen := helper.seen(); len(seen) != 1 || seen[0]["op"] != "window_close" {
		t.Fatalf("no-op transaction did not run the close/relock boundary: %#v", seen)
	}
}

func TestComputerUseAutomationRetainedHandlerIsStale(t *testing.T) {
	helper := startComputerUseAutomationHelper(t)
	srv, host := newComputerUseAutomationServer(t, helper.path)
	storeComputerUseAutomationSession(t, srv)

	var retained provider.ComputerUseToolHandler
	if err := host.handler(context.Background(), "logical-1", func(
		_ context.Context, tool provider.ComputerUseToolHandler,
	) error {
		retained = tool
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before := len(helper.seen())
	_, err := retained(context.Background(), provider.ComputerUseToolRequest{
		Tool: "get_app_state", App: "Claude",
	})
	if !errors.Is(err, computeruse.ErrTurnNotActive) {
		t.Fatalf("retained automation handler err=%v, want ErrTurnNotActive", err)
	}
	if after := len(helper.seen()); after != before {
		t.Fatalf("stale automation handler reached helper: before=%d after=%d", before, after)
	}
}

func TestComputerUseAutomationRevokesHandlerBeforeCleanupBarrier(t *testing.T) {
	helper := startComputerUseAutomationHelper(t)
	srv, host := newComputerUseAutomationServer(t, helper.path)
	storeComputerUseAutomationSession(t, srv)

	revoked := make(chan struct{})
	continueCleanup := make(chan struct{})
	srv.computerUseAutomationRevokedHook = func() {
		close(revoked)
		<-continueCleanup
	}
	var retained provider.ComputerUseToolHandler
	done := make(chan error, 1)
	go func() {
		done <- host.handler(context.Background(), "logical-1", func(
			_ context.Context, tool provider.ComputerUseToolHandler,
		) error {
			retained = tool
			return nil
		})
	}()
	<-revoked

	// Cleanup is deliberately paused after the callback's liveness bit was
	// revoked. A retained handler must already fail, without relying on the
	// later lease retirement or helper-side close.
	before := len(helper.seen())
	_, err := retained(context.Background(), provider.ComputerUseToolRequest{
		Tool: "get_app_state", App: "Claude",
	})
	if !errors.Is(err, computeruse.ErrTurnNotActive) {
		t.Fatalf("handler during revoke/cleanup barrier err=%v", err)
	}
	if after := len(helper.seen()); after != before {
		t.Fatalf("revoked handler reached helper before cleanup: before=%d after=%d", before, after)
	}
	close(continueCleanup)
	if err := <-done; err != nil {
		t.Fatalf("automation cleanup after barrier: %v", err)
	}
}

func TestComputerUseAutomationCleanupFailureHasNonFallbackSentinel(t *testing.T) {
	srv, host := newComputerUseAutomationServer(
		t, filepath.Join(t.TempDir(), "missing-helper.sock"),
	)
	storeComputerUseAutomationSession(t, srv)

	err := host.handler(context.Background(), "logical-1", func(
		context.Context, provider.ComputerUseToolHandler,
	) error {
		return nil
	})
	if !errors.Is(err, provider.ErrComputerUseAutomationCleanup) {
		t.Fatalf("cleanup error=%v, want ErrComputerUseAutomationCleanup", err)
	}
	if !errors.Is(err, computeruse.ErrHelperUnavailable) {
		t.Fatalf("cleanup error lost helper cause: %v", err)
	}
}
