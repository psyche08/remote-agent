package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/psyche08/remote-agent/internal/computeruse"
	"github.com/psyche08/remote-agent/internal/config"
	"github.com/psyche08/remote-agent/internal/provider"
	"github.com/psyche08/remote-agent/internal/state"
)

type fakeComputerUseToolHost struct {
	fakePushProvider
	handler provider.ComputerUseToolHandler
}

func (f *fakeComputerUseToolHost) SetComputerUseToolHandler(handler provider.ComputerUseToolHandler) {
	f.handler = handler
}

func TestModelComputerUseToolUsesProviderLeaseAndInMemoryPNG(t *testing.T) {
	png := append(append([]byte(nil), pngSignature...), []byte("test image")...)
	owner := ""
	helper := startFakeHelper(t, func(req map[string]any) map[string]any {
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
		case "action":
			if req["action"] == "screen.capture" {
				return map[string]any{
					"ok": true, "action": "screen.capture", "media_type": "image/png",
					"image_base64": base64.StdEncoding.EncodeToString(png),
				}
			}
			return map[string]any{"ok": true, "action": req["action"]}
		case "ax_read":
			return map[string]any{"ok": true, "elements": []any{
				map[string]any{"role": "AXButton", "label": "Run", "path": []any{0}},
			}}
		case "window_close":
			owner = ""
			return map[string]any{
				"ok": true, "window_open": false,
				"window_turn_id": "", "window_closing": false,
			}
		default:
			return map[string]any{"ok": true}
		}
	})
	host := &fakeComputerUseToolHost{fakePushProvider: fakePushProvider{id: "codex"}}
	cfg := &config.Config{
		DeviceID: "device-a", DefaultProvider: "codex",
		Providers: map[string]config.ProviderConfig{"codex": {}},
	}
	cfg.ComputerUse.Enabled = true
	cfg.ComputerUse.LockedUse.Enabled = true
	cfg.ComputerUse.HelperSocket = helper.path
	config.ApplyDefaults(cfg)
	srv := NewServer(cfg, provider.Registry{"codex": host}, state.New(filepath.Join(t.TempDir(), "data")))
	if host.handler == nil {
		t.Fatal("NewServer did not inject the in-process computer-use handler")
	}
	if err := srv.store.UpsertSession(state.Record{
		"provider_id": "codex", "session_id": "session-1",
		"native_session_id": "thread-1", "transcript_id": "thread-1",
	}); err != nil {
		t.Fatal(err)
	}

	req := provider.ComputerUseToolRequest{
		SessionID: "session-1", ThreadID: "thread-1", TurnID: "turn-1",
		Tool: "get_app_state", App: "CatDesk",
	}
	if _, err := host.handler(context.Background(), req); !errors.Is(err, computeruse.ErrTurnNotActive) {
		t.Fatalf("self-asserted model turn err=%v, want ErrTurnNotActive", err)
	}
	if seen := helper.seen(); len(seen) != 0 {
		t.Fatalf("self-asserted model turn reached helper: %#v", seen)
	}
	if _, err := host.handler(context.Background(), provider.ComputerUseToolRequest{
		ProviderID: "claude", SessionID: "session-1", TurnID: "turn-1", Tool: "get_app_state",
	}); err == nil {
		t.Fatal("provider identity mismatch was accepted")
	}

	if err := srv.observeComputerUseProviderFrame("codex", "thread-1", map[string]any{
		"type": "turn", "status": "started", "turn_id": "turn-1",
	}); err != nil {
		t.Fatal(err)
	}
	result, err := host.handler(context.Background(), req)
	if err != nil {
		t.Fatalf("get_app_state: %v", err)
	}
	wantImage := "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
	if result.ImageURL != wantImage {
		t.Fatalf("image URL mismatch: got %q want %q", result.ImageURL, wantImage)
	}
	var text map[string]any
	if err := json.Unmarshal([]byte(result.Text), &text); err != nil {
		t.Fatalf("result text is not JSON: %q: %v", result.Text, err)
	}
	if text["tool"] != "get_app_state" || text["accessibility"] == nil {
		t.Fatalf("get_app_state JSON lost AX state: %#v", text)
	}

	seen := helper.seen()
	if len(seen) != 3 || seen[0]["op"] != "window_open" ||
		seen[1]["action"] != "screen.capture" || seen[2]["op"] != "ax_read" {
		t.Fatalf("get_app_state helper sequence=%#v", seen)
	}
	wantOwner := computerUseOwnerID("codex", "session-1", "turn-1")
	for _, index := range []int{0, 1, 2} {
		if seen[index]["turn_id"] != wantOwner {
			t.Fatalf("request %d owner=%v, want scoped lease owner", index, seen[index]["turn_id"])
		}
	}

	clickX, clickY := 17, 23
	if _, err := host.handler(context.Background(), provider.ComputerUseToolRequest{
		SessionID: "session-1", ThreadID: "thread-1", TurnID: "turn-1",
		Tool: "click", X: &clickX, Y: &clickY,
	}); err != nil {
		t.Fatalf("model click: %v", err)
	}
	seen = helper.seen()
	var click map[string]any
	for _, helperRequest := range seen {
		if helperRequest["op"] == "action" && helperRequest["action"] == "pointer.click" {
			click = helperRequest
		}
	}
	if click == nil || click["coordinate_space"] != "screenshot" ||
		click["x"] != float64(clickX) || click["y"] != float64(clickY) {
		t.Fatalf("model click lost screenshot-coordinate contract: %#v", seen)
	}
}

func TestGetAppStateFallsBackToOrdinaryUnlockedComputerUse(t *testing.T) {
	png := append(append([]byte(nil), pngSignature...), []byte("normal unlocked")...)
	helper := startFakeHelper(t, func(req map[string]any) map[string]any {
		switch req["op"] {
		case "window_open":
			return refuse("locked_use_not_enabled", "locked use is not enabled on this device")
		case "action":
			return map[string]any{
				"ok": true, "action": "screen.capture", "media_type": "image/png",
				"image_base64": base64.StdEncoding.EncodeToString(png),
			}
		case "ax_read":
			return map[string]any{"ok": true, "elements": []any{}}
		default:
			return map[string]any{"ok": true}
		}
	})
	srv := computerUseServer(t, func(cfg *config.Config) {
		cfg.ComputerUse.Enabled = true
		cfg.ComputerUse.LockedUse.Enabled = false
		cfg.ComputerUse.HelperSocket = helper.path
	})
	if err := srv.observeComputerUseProviderFrame("codex", "session-1", map[string]any{
		"type": "turn", "status": "started", "turn_id": "turn-1",
	}); err != nil {
		t.Fatal(err)
	}
	result, err := srv.handleComputerUseTool(context.Background(), provider.ComputerUseToolRequest{
		ProviderID: "codex", SessionID: "session-1", TurnID: "turn-1",
		Tool: "get_app_state", App: "TextEdit",
	})
	if err != nil {
		t.Fatalf("ordinary unlocked get_app_state: %v", err)
	}
	var text map[string]any
	if err := json.Unmarshal([]byte(result.Text), &text); err != nil {
		t.Fatal(err)
	}
	window, ok := text["window"].(map[string]any)
	if !ok || window["mode"] != "normal_unlocked" || window["open"] != false ||
		window["registered"] != false || window["phase"] != computeruse.WindowPhaseClosed {
		t.Fatalf("ordinary unlocked result has wrong window mode: %#v", text)
	}
	seen := helper.seen()
	if len(seen) != 3 || seen[0]["op"] != "window_open" ||
		seen[1]["action"] != "screen.capture" || seen[2]["op"] != "ax_read" {
		t.Fatalf("ordinary unlocked helper sequence=%#v", seen)
	}
	for _, request := range seen {
		if request["op"] == "window_close" {
			t.Fatalf("ordinary unlocked path incurred a false relock debt: %#v", seen)
		}
	}
}

func TestGetAppStateFailsClosedWhenScreenIsLockedWithoutLockedUse(t *testing.T) {
	helper := startFakeHelper(t, func(req map[string]any) map[string]any {
		switch req["op"] {
		case "window_open":
			return refuse("locked_use_not_enabled", "locked use is not enabled on this device")
		case "action":
			// The helper's ordinary-unlocked gate is authoritative for actual
			// lock state. It refuses capture instead of letting the fallback turn
			// a disabled Locked Use configuration into an unlock bypass.
			return refuse("no_window", "the screen is locked")
		default:
			return map[string]any{"ok": true}
		}
	})
	srv := computerUseServer(t, func(cfg *config.Config) {
		cfg.ComputerUse.Enabled = true
		cfg.ComputerUse.LockedUse.Enabled = false
		cfg.ComputerUse.HelperSocket = helper.path
	})
	if err := srv.observeComputerUseProviderFrame("codex", "session-1", map[string]any{
		"type": "turn", "status": "started", "turn_id": "turn-1",
	}); err != nil {
		t.Fatal(err)
	}
	_, err := srv.handleComputerUseTool(context.Background(), provider.ComputerUseToolRequest{
		ProviderID: "codex", SessionID: "session-1", TurnID: "turn-1",
		Tool: "get_app_state", App: "TextEdit",
	})
	if !errors.Is(err, computeruse.ErrNoWindow) {
		t.Fatalf("locked ordinary path err=%v, want ErrNoWindow", err)
	}
	seen := helper.seen()
	if len(seen) != 2 || seen[0]["op"] != "window_open" || seen[1]["action"] != "screen.capture" {
		t.Fatalf("locked ordinary path helper sequence=%#v", seen)
	}
	for _, request := range seen {
		if request["op"] == "ax_read" || request["op"] == "window_close" {
			t.Fatalf("locked ordinary path did work after refusal: %#v", seen)
		}
	}
}

func TestComputerUsePNGDataURLValidation(t *testing.T) {
	png := append(append([]byte(nil), pngSignature...), 'x')
	encoded := base64.StdEncoding.EncodeToString(png)
	if url, size, err := computerUsePNGDataURL(map[string]any{
		"media_type": "image/png", "image_base64": encoded,
	}, len(png)); err != nil || size != len(png) || url != "data:image/png;base64,"+encoded {
		t.Fatalf("valid PNG url=%q size=%d err=%v", url, size, err)
	}

	for name, capture := range map[string]map[string]any{
		"path-only":  {"media_type": "image/png", "path": "/tmp/capture.png"},
		"wrong-type": {"media_type": "image/jpeg", "image_base64": encoded},
		"bad-base64": {"media_type": "image/png", "image_base64": "%%%"},
		"bad-magic":  {"media_type": "image/png", "image_base64": base64.StdEncoding.EncodeToString([]byte("not png"))},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := computerUsePNGDataURL(capture, len(png)); err == nil {
				t.Fatal("invalid helper image was accepted")
			}
		})
	}
	if _, _, err := computerUsePNGDataURL(map[string]any{
		"media_type": "image/png", "image_base64": encoded,
	}, len(png)-1); err == nil {
		t.Fatal("oversized helper image was accepted")
	}
}

func TestGetAppStateRelocksWhenInMemoryCaptureIsInvalid(t *testing.T) {
	owner := ""
	helper := startFakeHelper(t, func(req map[string]any) map[string]any {
		switch req["op"] {
		case "window_open":
			owner, _ = req["turn_id"].(string)
			return map[string]any{
				"ok": true, "window_open": true,
				"window_turn_id": owner, "window_closing": false,
			}
		case "action":
			return map[string]any{
				"ok": true, "action": "screen.capture", "media_type": "image/png",
				"path": "/tmp/legacy-capture.png",
			}
		case "window_close":
			owner = ""
			return map[string]any{
				"ok": true, "window_open": false,
				"window_turn_id": "", "window_closing": false,
			}
		default:
			return map[string]any{"ok": true}
		}
	})
	srv := computerUseServer(t, func(cfg *config.Config) {
		cfg.ComputerUse.Enabled = true
		cfg.ComputerUse.LockedUse.Enabled = true
		cfg.ComputerUse.HelperSocket = helper.path
	})
	if err := srv.observeComputerUseProviderFrame("codex", "session-1", map[string]any{
		"type": "turn", "status": "started", "turn_id": "turn-1",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.handleComputerUseTool(context.Background(), provider.ComputerUseToolRequest{
		ProviderID: "codex", SessionID: "session-1", TurnID: "turn-1",
		Tool: "get_app_state", App: "CatDesk",
	}); err == nil {
		t.Fatal("legacy path-only capture was accepted")
	}
	seen := helper.seen()
	if len(seen) != 3 || seen[0]["op"] != "window_open" ||
		seen[1]["action"] != "screen.capture" || seen[2]["op"] != "window_close" {
		t.Fatalf("failed get_app_state did not synchronously relock: %#v", seen)
	}
}
