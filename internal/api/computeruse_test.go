package api

import (
	"bufio"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/psyche08/remote-agent/internal/config"
	"github.com/psyche08/remote-agent/internal/provider"
	"github.com/psyche08/remote-agent/internal/state"
)

// The desktop surface and every Locked Use safeguard live in the helper
// process, and are tested there (mac/RemoteAgentDesktop). What is left to test
// on this side is the boundary: that requests are forwarded faithfully, that a
// refusal's code becomes the right status, and that an absent helper is
// reported as a fault rather than as a switched-off feature.

// fakeHelper is a scripted stand-in for agenthalo-desktop.
type fakeHelper struct {
	path     string
	listener net.Listener

	mu       sync.Mutex
	requests []map[string]any
	reply    func(req map[string]any) map[string]any
}

func startFakeHelper(t *testing.T, reply func(map[string]any) map[string]any) *fakeHelper {
	t.Helper()
	// A unix socket path must fit in sockaddr_un's 104 bytes. t.TempDir() on
	// macOS resolves under /private/var/folders/... and, with the test name in
	// it, blows that budget — so this takes a deliberately short root.
	dir, err := os.MkdirTemp("/tmp", "ra")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "h.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	h := &fakeHelper{path: path, listener: listener, reply: reply}
	go h.serve()
	t.Cleanup(func() { listener.Close() })
	return h
}

func startWindowTrackingHelper(t *testing.T) *fakeHelper {
	t.Helper()
	owner := ""
	return startFakeHelper(t, func(req map[string]any) map[string]any {
		switch req["op"] {
		case "window_open":
			owner, _ = req["turn_id"].(string)
			return map[string]any{
				"ok": true, "window_open": true,
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
		case "window_state":
			return map[string]any{
				"ok": true, "window_open": owner != "",
				"window_turn_id": owner, "window_closing": false,
			}
		case "action":
			return map[string]any{"ok": true, "action": req["action"]}
		default:
			return map[string]any{"ok": true}
		}
	})
}

func (h *fakeHelper) serve() {
	for {
		conn, err := h.listener.Accept()
		if err != nil {
			return
		}
		go func() {
			defer conn.Close()
			rd := bufio.NewReader(conn)
			for {
				line, err := rd.ReadBytes('\n')
				if err != nil {
					return
				}
				var req map[string]any
				if json.Unmarshal(line, &req) != nil {
					return
				}
				h.mu.Lock()
				h.requests = append(h.requests, req)
				reply := h.reply
				h.mu.Unlock()
				out := map[string]any{"ok": true}
				if reply != nil {
					if res := reply(req); res != nil {
						out = res
					}
				}
				// Most boundary tests do not care about window ownership. Model the
				// real helper's complete closed-state response unless a test
				// explicitly scripts a different owner.
				if req["op"] == "window_state" {
					if _, ok := out["window_open"]; !ok {
						out["window_open"] = false
						out["window_turn_id"] = ""
						out["window_closing"] = false
					}
				}
				body, _ := json.Marshal(out)
				if _, err := conn.Write(append(body, '\n')); err != nil {
					return
				}
			}
		}()
	}
}

func (h *fakeHelper) seen() []map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]map[string]any(nil), h.requests...)
}

func computerUseServer(t *testing.T, mutate func(*config.Config)) *Server {
	t.Helper()
	cfg := &config.Config{
		DeviceID:        "device-a",
		DefaultProvider: "claude",
		// Most tests in this file exercise the legacy HTTP boundary itself. Opt
		// that test fixture into the explicit debug route; production defaults
		// stay off and are covered by the model-tool-required test below.
		ComputerUse: config.ComputerUseConfig{DebugHTTPActions: true},
		Providers: map[string]config.ProviderConfig{
			"claude": {AppName: "Claude Code CLI", Command: "claude"},
		},
	}
	if mutate != nil {
		mutate(cfg)
	}
	config.ApplyDefaults(cfg)
	return NewServer(cfg, provider.BuildRegistry(cfg), state.New(filepath.Join(t.TempDir(), "data")))
}

// refuse builds the shape the helper returns for a refusal.
func refuse(code, detail string) map[string]any {
	return map[string]any{"ok": false, "code": code, "error": detail}
}

func doJSON(t *testing.T, srv *Server, method, path, body string) (int, map[string]any) {
	t.Helper()
	rr := httptest.NewRecorder()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	srv.Handler().ServeHTTP(rr, req)
	out := map[string]any{}
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	return rr.Code, out
}

const (
	computerUseTestProvider = "codex"
	computerUseTestSession  = "computer-use-session"
)

func computerUseBody(t *testing.T, raw string) string {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["provider_id"]; !ok {
		body["provider_id"] = computerUseTestProvider
	}
	if _, ok := body["session_id"]; !ok {
		body["session_id"] = computerUseTestSession
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func startComputerUseTurn(srv *Server, providerID, sessionID, turnID string) {
	srv.publishProviderStream(providerID, sessionID, map[string]any{
		"type": "turn", "status": "started", "turn_id": turnID,
	})
}

// With computer use unconfigured the routes must exist and answer plainly,
// rather than 404ing or implying the feature is present.
func TestComputerUseDisabledByDefault(t *testing.T) {
	srv := computerUseServer(t, nil)

	code, body := doJSON(t, srv, http.MethodGet, "/computer_use", "")
	if code != http.StatusOK {
		t.Fatalf("status=%d body=%#v", code, body)
	}
	if body["enabled"] != false {
		t.Fatalf("computer use is enabled without configuration: %#v", body)
	}
	if srv.computerUseCtl != nil {
		t.Fatal("a controller was built for an unconfigured device")
	}

	// Every mutating route must refuse rather than silently no-op.
	for _, route := range []struct{ path, body string }{
		{"/computer_use/locked_use", `{"active":true}`},
		{"/computer_use/window", `{"turn_id":"t1","action":"open"}`},
		{"/computer_use/action", `{"action":"screen.capture"}`},
		{"/computer_use/ax", `{"turn_id":"t1","op":"ax_read","app":"CatDesk"}`},
	} {
		code, out := doJSON(t, srv, http.MethodPost, route.path, route.body)
		if code != http.StatusConflict {
			t.Errorf("%s status=%d body=%#v, want 409", route.path, code, out)
		}
	}
}

func TestLockedUseHTTPMutationsRequireModelToolByDefault(t *testing.T) {
	helper := startWindowTrackingHelper(t)
	srv := computerUseServer(t, func(cfg *config.Config) {
		cfg.ComputerUse.Enabled = true
		cfg.ComputerUse.LockedUse.Enabled = true
		cfg.ComputerUse.DebugHTTPActions = false
		cfg.ComputerUse.HelperSocket = helper.path
	})
	startComputerUseTurn(srv, computerUseTestProvider, computerUseTestSession, "turn-1")

	for _, route := range []struct{ path, body string }{
		{"/computer_use/window", computerUseBody(t, `{"turn_id":"turn-1","action":"open"}`)},
		{"/computer_use/action", computerUseBody(t, `{"turn_id":"turn-1","action":"pointer.move","x":1,"y":2}`)},
		{"/computer_use/ax", computerUseBody(t, `{"turn_id":"turn-1","op":"ax_read","app":"CatDesk"}`)},
	} {
		code, body := doJSON(t, srv, http.MethodPost, route.path, route.body)
		if code != http.StatusForbidden || body["code"] != "model_tool_required" {
			t.Errorf("%s status=%d body=%#v, want 403/model_tool_required", route.path, code, body)
		}
	}
	if seen := helper.seen(); len(seen) != 0 {
		t.Fatalf("model-tool-only HTTP request reached helper: %#v", seen)
	}

	// Closing and runtime deactivation can only remove authority, so they stay
	// available even when mutation routes are model-tool-only.
	code, body := doJSON(t, srv, http.MethodPost, "/computer_use/window",
		computerUseBody(t, `{"turn_id":"turn-1","action":"close"}`))
	if code != http.StatusOK || body["ok"] != true {
		t.Fatalf("HTTP close status=%d body=%#v", code, body)
	}
	code, body = doJSON(t, srv, http.MethodPost, "/computer_use/locked_use", `{"active":false}`)
	if code != http.StatusOK || body["ok"] != true {
		t.Fatalf("runtime deactivate status=%d body=%#v", code, body)
	}
}

// Locked Use must not be reachable when only computer use is enabled: the two
// are separate opt-ins and the unlock capability is the more serious one.
func TestLockedUseRequiresItsOwnOptIn(t *testing.T) {
	helper := startFakeHelper(t, func(req map[string]any) map[string]any {
		switch req["op"] {
		case "locked_use_active", "window_open":
			return refuse("locked_use_not_enabled", "locked use is not enabled on this device")
		}
		return nil
	})
	srv := computerUseServer(t, func(cfg *config.Config) {
		cfg.ComputerUse.Enabled = true
		cfg.ComputerUse.HelperSocket = helper.path
	})
	if srv.computerUseCtl == nil {
		t.Fatal("no controller built for an enabled device")
	}
	startComputerUseTurn(srv, computerUseTestProvider, computerUseTestSession, "t1")

	code, body := doJSON(t, srv, http.MethodPost, "/computer_use/locked_use", `{"active":true}`)
	if code != http.StatusConflict {
		t.Fatalf("status=%d body=%#v, want 409", code, body)
	}
	if body["status"] != "not_enabled" {
		t.Fatalf("status field = %v, want not_enabled", body["status"])
	}

	code, body = doJSON(t, srv, http.MethodPost, "/computer_use/window",
		computerUseBody(t, `{"turn_id":"t1","action":"open"}`))
	if code != http.StatusConflict {
		t.Fatalf("window status=%d body=%#v, want 409", code, body)
	}
}

// Config is the ceiling. Enabling Locked Use is an on-device decision, and no
// request may grant a Mac the ability to unlock itself. The helper enforces
// that; this checks the agent neither claims otherwise nor hides the refusal.
func TestLockedUseCannotBeEnabledOverTheAPI(t *testing.T) {
	helper := startFakeHelper(t, func(req map[string]any) map[string]any {
		if req["op"] == "locked_use_active" {
			return refuse("locked_use_not_enabled", "locked use is not enabled on this device")
		}
		return map[string]any{"ok": true, "status": map[string]any{
			"enabled":   true,
			"available": true,
			"locked_use": map[string]any{
				"enabled": false, "armed": false, "active": false,
			},
		}}
	})
	srv := computerUseServer(t, func(cfg *config.Config) {
		cfg.ComputerUse.Enabled = true
		cfg.ComputerUse.HelperSocket = helper.path
	})
	if _, body := doJSON(t, srv, http.MethodPost, "/computer_use/locked_use", `{"active":true}`); body["ok"] == true {
		t.Fatal("the API enabled Locked Use on a device whose config disabled it")
	}
	_, status := doJSON(t, srv, http.MethodGet, "/computer_use", "")
	lu, ok := status["locked_use"].(map[string]any)
	if !ok {
		t.Fatalf("status has no locked_use block: %#v", status)
	}
	if lu["enabled"] == true || lu["armed"] == true {
		t.Fatalf("locked use reports enabled/armed after a refused request: %#v", lu)
	}
}

func TestComputerUseStatusExposesNoSecrets(t *testing.T) {
	helper := startFakeHelper(t, func(req map[string]any) map[string]any {
		return map[string]any{"ok": true, "status": map[string]any{
			"enabled":   true,
			"available": true,
			"locked_use": map[string]any{
				"enabled": true, "armed": true,
				// The verifying half is meant to be published; it cannot sign.
				"public_key": "BPXUe9ozoBk1CvdFUmCDFHN0",
			},
		}}
	})
	srv := computerUseServer(t, func(cfg *config.Config) {
		cfg.ComputerUse.Enabled = true
		cfg.ComputerUse.LockedUse.Enabled = true
		cfg.ComputerUse.HelperSocket = helper.path
	})
	code, body := doJSON(t, srv, http.MethodGet, "/computer_use", "")
	if code != http.StatusOK {
		t.Fatalf("status=%d", code)
	}
	raw, _ := json.Marshal(body)
	text := strings.ToLower(string(raw))
	for _, banned := range []string{"signing.key", "private", "password", "seed"} {
		if strings.Contains(text, banned) {
			t.Errorf("computer-use status exposes %q: %s", banned, raw)
		}
	}
}

// The vocabulary is enforced in the helper, which is the only process that can
// act on it. What must hold here is that its refusal keeps its meaning: a
// malformed action stays a 400 and does not become a generic 5xx.
func TestRefusedActionKeepsItsStatusCode(t *testing.T) {
	helper := startFakeHelper(t, func(req map[string]any) map[string]any {
		if req["op"] == "window_state" {
			return nil
		}
		return refuse("bad_request", "unknown computer-use action")
	})
	srv := computerUseServer(t, func(cfg *config.Config) {
		cfg.ComputerUse.Enabled = true
		cfg.ComputerUse.HelperSocket = helper.path
	})
	startComputerUseTurn(srv, computerUseTestProvider, computerUseTestSession, "t1")
	for _, body := range []string{
		`{"turn_id":"t1","action":"shell.exec"}`,
		`{"turn_id":"t1","action":"pointer.click"}`,
		`{"turn_id":"t1","action":"keyboard.key","keys":[]}`,
		`{"turn_id":"t1","action":""}`,
	} {
		code, out := doJSON(t, srv, http.MethodPost, "/computer_use/action", computerUseBody(t, body))
		if code != http.StatusBadRequest {
			t.Errorf("action %s status=%d body=%#v, want 400", body, code, out)
		}
		if out["code"] != "bad_request" {
			t.Errorf("action %s code=%v, want bad_request", body, out["code"])
		}
	}
}

// Forwarding has to be faithful in both directions, and zero is the value most
// easily lost: a click at (0,0) must not arrive as a click with no coordinates,
// which the helper would refuse as malformed.
func TestActionFieldsAreForwardedFaithfully(t *testing.T) {
	helper := startFakeHelper(t, func(req map[string]any) map[string]any {
		return map[string]any{"ok": true, "action": req["action"]}
	})
	srv := computerUseServer(t, func(cfg *config.Config) {
		cfg.ComputerUse.Enabled = true
		cfg.ComputerUse.HelperSocket = helper.path
	})
	startComputerUseTurn(srv, computerUseTestProvider, computerUseTestSession, "t1")

	code, body := doJSON(t, srv, http.MethodPost, "/computer_use/action",
		computerUseBody(t, `{"turn_id":"t1","action":"pointer.click","x":0,"y":0,"button":"right","count":2}`))
	if code != http.StatusOK {
		t.Fatalf("status=%d body=%#v", code, body)
	}
	if body["action"] != "pointer.click" {
		t.Fatalf("action not echoed back: %#v", body)
	}

	seen := helper.seen()
	if len(seen) == 0 {
		t.Fatal("nothing reached the helper")
	}
	last := seen[len(seen)-1]
	for key, want := range map[string]any{
		"op":      "action",
		"turn_id": computerUseOwnerID(computerUseTestProvider, computerUseTestSession, "t1"),
		"action":  "pointer.click",
		"x":       float64(0), "y": float64(0), "button": "right", "count": float64(2),
	} {
		if got, ok := last[key]; !ok || got != want {
			t.Errorf("forwarded %s = %#v (present=%v), want %#v", key, got, ok, want)
		}
	}
}

// A device whose helper died has computer use configured on and broken.
// Reporting that as "not enabled" would hide a fault behind a setting.
func TestUnreachableHelperIsReportedAsAFault(t *testing.T) {
	srv := computerUseServer(t, func(cfg *config.Config) {
		cfg.ComputerUse.Enabled = true
		cfg.ComputerUse.HelperSocket = "/tmp/ra-absent-helper.sock"
	})
	startComputerUseTurn(srv, computerUseTestProvider, computerUseTestSession, "t1")

	code, body := doJSON(t, srv, http.MethodPost, "/computer_use/action",
		computerUseBody(t, `{"turn_id":"t1","action":"pointer.move","x":1,"y":1}`))
	if code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%#v, want 503", code, body)
	}

	_, status := doJSON(t, srv, http.MethodGet, "/computer_use", "")
	if status["enabled"] != true {
		t.Errorf("an unreachable helper made the feature look disabled: %#v", status)
	}
	if status["available"] != false {
		t.Errorf("an unreachable helper still reported available: %#v", status)
	}
}

func TestComputerUseWindowRejectsBadInput(t *testing.T) {
	helper := startFakeHelper(t, nil)
	srv := computerUseServer(t, func(cfg *config.Config) {
		cfg.ComputerUse.Enabled = true
		cfg.ComputerUse.LockedUse.Enabled = true
		cfg.ComputerUse.HelperSocket = helper.path
	})
	for _, body := range []string{
		`{"action":"open"}`,
		`{"turn_id":"../etc","action":"open"}`,
		`{"turn_id":"t1","action":"sudo"}`,
	} {
		code, out := doJSON(t, srv, http.MethodPost, "/computer_use/window", computerUseBody(t, body))
		if code != http.StatusBadRequest {
			t.Errorf("window %s status=%d body=%#v, want 400", body, code, out)
		}
	}
	// None of those may have reached the helper: turn_id shape and the open/
	// close verb are this layer's own checks.
	for _, req := range helper.seen() {
		if req["op"] == "window_open" {
			t.Errorf("a malformed window request was forwarded: %#v", req)
		}
	}
}

func TestComputerUseRoutesRejectWrongMethods(t *testing.T) {
	srv := computerUseServer(t, func(cfg *config.Config) {
		cfg.ComputerUse.Enabled = true
	})
	for _, path := range []string{
		"/computer_use/locked_use", "/computer_use/window", "/computer_use/action", "/computer_use/ax",
	} {
		code, _ := doJSON(t, srv, http.MethodGet, path, "")
		if code != http.StatusMethodNotAllowed {
			t.Errorf("GET %s status=%d, want 405", path, code)
		}
	}
	if code, _ := doJSON(t, srv, http.MethodPost, "/computer_use", `{}`); code != http.StatusMethodNotAllowed {
		t.Errorf("POST /computer_use status=%d, want 405", code)
	}
}

// A device without Locked Use keeps its legacy screenshot behavior. Once
// Locked Use is configured, legacy file capture is permanently model-only:
// checking the current phase and then invoking screencapture would race a new
// window opening between those two operations.
func TestCaptureGateIsPermanentlyModelOnlyWhenLockedUseIsConfigured(t *testing.T) {
	srv := computerUseServer(t, nil)
	rr := httptest.NewRecorder()
	if !srv.captureGate(rr) {
		t.Fatal("capture gate refused on a device without computer use")
	}

	helper := startFakeHelper(t, func(req map[string]any) map[string]any {
		t.Fatalf("permanent Locked Use legacy gate queried helper state: %#v", req)
		return nil
	})
	srv = computerUseServer(t, func(cfg *config.Config) {
		cfg.ComputerUse.Enabled = true
		cfg.ComputerUse.LockedUse.Enabled = true
		cfg.ComputerUse.HelperSocket = helper.path
	})
	rr = httptest.NewRecorder()
	if srv.captureGate(rr) || rr.Code != http.StatusForbidden {
		t.Fatalf("Locked Use legacy capture gate status=%d, want 403", rr.Code)
	}

	// A failed true -> false helper reload has a disabled new config but may
	// still have an old registered window. The process-local refresh marker
	// retains the same capture boundary until a safe restart succeeds.
	srv = computerUseServer(t, func(cfg *config.Config) {
		cfg.ComputerUse.HelperRefreshFailed = true
	})
	rr = httptest.NewRecorder()
	if srv.captureGate(rr) || rr.Code != http.StatusForbidden {
		t.Fatalf("refresh-failed legacy capture gate status=%d, want 403", rr.Code)
	}
}

// The permanent config gate also fails safely when the helper is unreachable;
// a missing helper cannot turn legacy on-disk capture back on.
func TestCaptureGateRefusesWhenLockedUseHelperIsUnreachable(t *testing.T) {
	srv := computerUseServer(t, func(cfg *config.Config) {
		cfg.ComputerUse.Enabled = true
		cfg.ComputerUse.LockedUse.Enabled = true
		cfg.ComputerUse.HelperSocket = "/tmp/ra-absent-helper.sock"
	})
	rr := httptest.NewRecorder()
	if srv.captureGate(rr) {
		t.Fatal("capture proceeded while Locked Use was configured and unverifiable")
	}

	// But a device that never enabled Locked Use has no window to protect, and
	// must keep taking screenshots even with no helper installed.
	srv = computerUseServer(t, func(cfg *config.Config) {
		cfg.ComputerUse.Enabled = true
		cfg.ComputerUse.HelperSocket = "/tmp/ra-absent-helper.sock"
	})
	rr = httptest.NewRecorder()
	if !srv.captureGate(rr) {
		t.Fatal("capture gate refused on a device without Locked Use")
	}
}

func TestLockedUseWindowRefusesLegacyCaptureOCRAndStoredScreenshot(t *testing.T) {
	helper := startWindowTrackingHelper(t)
	srv := computerUseServer(t, func(cfg *config.Config) {
		cfg.ComputerUse.Enabled = true
		cfg.ComputerUse.LockedUse.Enabled = true
		cfg.ComputerUse.HelperSocket = helper.path
	})
	startComputerUseTurn(srv, computerUseTestProvider, computerUseTestSession, "turn-legacy-gate")
	code, body := doJSON(t, srv, http.MethodPost, "/computer_use/window",
		computerUseBody(t, `{"turn_id":"turn-legacy-gate","action":"open"}`))
	if code != http.StatusOK || body["window_open"] != true {
		t.Fatalf("open status=%d body=%#v", code, body)
	}

	handler := srv.Handler()
	const parallelCaptures = 12
	codes := make(chan int, parallelCaptures)
	for range parallelCaptures {
		go func() {
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/screenshot", nil))
			codes <- rr.Code
		}()
	}
	for range parallelCaptures {
		if got := <-codes; got != http.StatusForbidden {
			t.Errorf("concurrent legacy screenshot status=%d, want 403", got)
		}
	}
	screenshotDir := filepath.Join(filepath.Dir(srv.store.DataDir()), "screenshots")
	if entries, err := os.ReadDir(screenshotDir); err == nil {
		if len(entries) != 0 {
			t.Fatalf("legacy screenshot wrote files under Locked Use: %#v", entries)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("inspect screenshot directory: %v", err)
	}

	oldPath := filepath.Join(t.TempDir(), "old-screenshot.png")
	if err := os.WriteFile(oldPath, []byte("must not be served or OCRed"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv.lastScreenshot = oldPath
	for _, route := range []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/ocr"},
		{http.MethodGet, "/last_screenshot"},
	} {
		code, body = doJSON(t, srv, route.method, route.path, "")
		if code != http.StatusForbidden || body["code"] != "model_tool_required" {
			t.Errorf("%s status=%d body=%#v, want 403/model_tool_required", route.path, code, body)
		}
	}
	data, err := os.ReadFile(oldPath)
	if err != nil || string(data) != "must not be served or OCRed" {
		t.Fatalf("legacy OCR mutated stored screenshot: data=%q err=%v", data, err)
	}
	if seen := helper.seen(); len(seen) != 1 || seen[0]["op"] != "window_open" {
		t.Fatalf("legacy capture gates reached helper after open: %#v", seen)
	}
}

// The Accessibility route forwards element-tree operations admitted by the
// helper after its Locked Use transition. Like the action route, the vocabulary
// is enforced there; this layer owns the op allow-list and faithful forwarding.
func TestComputerUseAXForwardsAndGatesOps(t *testing.T) {
	helper := startFakeHelper(t, func(req map[string]any) map[string]any {
		return map[string]any{"ok": true, "elements": []any{
			map[string]any{"role": "AXButton", "label": "新任务", "path": []any{0, 2}},
		}}
	})
	srv := computerUseServer(t, func(cfg *config.Config) {
		cfg.ComputerUse.Enabled = true
		cfg.ComputerUse.HelperSocket = helper.path
	})
	startComputerUseTurn(srv, computerUseTestProvider, computerUseTestSession, "t1")

	// A good read forwards and returns the elements.
	code, body := doJSON(t, srv, http.MethodPost, "/computer_use/ax",
		computerUseBody(t, `{"turn_id":"t1","op":"ax_read","app":"CatDesk"}`))
	if code != http.StatusOK {
		t.Fatalf("ax_read status=%d body=%#v", code, body)
	}
	if _, ok := body["elements"].([]any); !ok {
		t.Fatalf("ax_read did not return elements: %#v", body)
	}

	// The op allow-list is this layer's own check and must be refused before
	// reaching the helper.
	code, _ = doJSON(t, srv, http.MethodPost, "/computer_use/ax",
		computerUseBody(t, `{"turn_id":"t1","op":"ax_teleport"}`))
	if code != http.StatusBadRequest {
		t.Errorf("unknown ax op status=%d, want 400", code)
	}
	for _, req := range helper.seen() {
		if req["op"] == "ax_teleport" {
			t.Error("an unknown ax op was forwarded to the helper")
		}
	}

	// Set-value fields forward faithfully, including a unicode value.
	code, _ = doJSON(t, srv, http.MethodPost, "/computer_use/ax",
		computerUseBody(t, `{"turn_id":"t1","op":"ax_setvalue","app":"CatDesk","path":[1,4],"value":"你好"}`))
	if code != http.StatusOK {
		t.Fatalf("ax_setvalue status=%d", code)
	}
	seen := helper.seen()
	last := seen[len(seen)-1]
	if last["op"] != "ax_setvalue" || last["value"] != "你好" {
		t.Errorf("ax_setvalue forwarded wrong: %#v", last)
	}
	if p, ok := last["path"].([]any); !ok || len(p) != 2 {
		t.Errorf("ax_setvalue path forwarded wrong: %#v", last["path"])
	}
}

func TestComputerUseRequiresAndEnforcesTurnOwnership(t *testing.T) {
	ownerID := computerUseOwnerID(computerUseTestProvider, computerUseTestSession, "owner-turn")
	helper := startFakeHelper(t, func(req map[string]any) map[string]any {
		if req["op"] == "window_state" {
			return map[string]any{
				"ok": true, "window_open": true,
				"window_turn_id": ownerID, "window_closing": false,
			}
		}
		return map[string]any{"ok": true, "action": req["action"]}
	})
	srv := computerUseServer(t, func(cfg *config.Config) {
		cfg.ComputerUse.Enabled = true
		cfg.ComputerUse.LockedUse.Enabled = true
		cfg.ComputerUse.HelperSocket = helper.path
	})
	startComputerUseTurn(srv, computerUseTestProvider, computerUseTestSession, "owner-turn")
	startComputerUseTurn(srv, computerUseTestProvider, "other-session", "other-turn")

	for _, route := range []struct{ path, body string }{
		{"/computer_use/action", `{"provider_id":"codex","session_id":"computer-use-session","action":"pointer.move","x":1,"y":2}`},
		{"/computer_use/ax", `{"provider_id":"codex","session_id":"computer-use-session","op":"ax_read","app":"CatDesk"}`},
		{"/computer_use/action", `{"session_id":"computer-use-session","turn_id":"owner-turn","action":"pointer.move","x":1,"y":2}`},
		{"/computer_use/ax", `{"provider_id":"codex","turn_id":"owner-turn","op":"ax_read","app":"CatDesk"}`},
	} {
		code, body := doJSON(t, srv, http.MethodPost, route.path, route.body)
		if code != http.StatusBadRequest || body["code"] != "bad_request" {
			t.Errorf("missing turn %s status=%d body=%#v", route.path, code, body)
		}
	}

	for _, route := range []struct{ path, body string }{
		{"/computer_use/action", `{"provider_id":"codex","session_id":"other-session","turn_id":"other-turn","action":"pointer.move","x":1,"y":2}`},
		{"/computer_use/ax", `{"provider_id":"codex","session_id":"other-session","turn_id":"other-turn","op":"ax_read","app":"CatDesk"}`},
	} {
		code, body := doJSON(t, srv, http.MethodPost, route.path, route.body)
		if code != http.StatusConflict || body["code"] != "window_busy" {
			t.Errorf("wrong owner %s status=%d body=%#v", route.path, code, body)
		}
		if detail, _ := body["detail"].(string); strings.Contains(detail, "owner-turn") {
			t.Errorf("wrong-owner response exposed the owning turn: %#v", body)
		}
	}

	code, body := doJSON(t, srv, http.MethodPost, "/computer_use/action",
		computerUseBody(t, `{"turn_id":"owner-turn","action":"pointer.move","x":1,"y":2}`))
	if code != http.StatusOK || body["ok"] != true {
		t.Fatalf("owner action status=%d body=%#v", code, body)
	}
}

func TestComputerUseRejectsSelfAssertedOrWrongScopeTurn(t *testing.T) {
	helper := startFakeHelper(t, func(req map[string]any) map[string]any {
		return map[string]any{"ok": true, "action": req["action"]}
	})
	srv := computerUseServer(t, func(cfg *config.Config) {
		cfg.ComputerUse.Enabled = true
		cfg.ComputerUse.HelperSocket = helper.path
	})

	request := `{"provider_id":"codex","session_id":"session-a","turn_id":"turn-real","action":"pointer.move","x":1,"y":2}`
	code, body := doJSON(t, srv, http.MethodPost, "/computer_use/action", request)
	if code != http.StatusConflict || body["code"] != "turn_not_active" {
		t.Fatalf("self-asserted turn status=%d body=%#v", code, body)
	}
	if seen := helper.seen(); len(seen) != 0 {
		t.Fatalf("self-asserted turn reached helper: %#v", seen)
	}

	startComputerUseTurn(srv, "codex", "session-a", "turn-real")
	for _, wrong := range []string{
		`{"provider_id":"codex","session_id":"session-b","turn_id":"turn-real","action":"pointer.move","x":1,"y":2}`,
		`{"provider_id":"claude","session_id":"session-a","turn_id":"turn-real","action":"pointer.move","x":1,"y":2}`,
		`{"provider_id":"codex","session_id":"session-a","turn_id":"turn-made-up","action":"pointer.move","x":1,"y":2}`,
	} {
		code, body = doJSON(t, srv, http.MethodPost, "/computer_use/action", wrong)
		if code != http.StatusConflict || body["code"] != "turn_not_active" {
			t.Errorf("wrong lease scope status=%d body=%#v", code, body)
		}
	}
	if seen := helper.seen(); len(seen) != 0 {
		t.Fatalf("wrong lease scope reached helper: %#v", seen)
	}

	code, body = doJSON(t, srv, http.MethodPost, "/computer_use/action", request)
	if code != http.StatusOK || body["ok"] != true {
		t.Fatalf("provider-established turn status=%d body=%#v", code, body)
	}
}

func TestProviderTerminalFramesRevokeLeaseAndRelock(t *testing.T) {
	cases := []struct {
		name  string
		frame map[string]any
	}{
		{"completed", map[string]any{"type": "turn", "status": "completed", "turn_id": "turn-1"}},
		{"error", map[string]any{"type": "error", "turn_id": "turn-1"}},
		{"interrupt", map[string]any{"type": "turn", "status": "interrupted", "turn_id": "turn-1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			helper := startWindowTrackingHelper(t)
			srv := computerUseServer(t, func(cfg *config.Config) {
				cfg.ComputerUse.Enabled = true
				cfg.ComputerUse.LockedUse.Enabled = true
				cfg.ComputerUse.HelperSocket = helper.path
			})
			startComputerUseTurn(srv, computerUseTestProvider, computerUseTestSession, "turn-1")

			code, body := doJSON(t, srv, http.MethodPost, "/computer_use/window",
				computerUseBody(t, `{"turn_id":"turn-1","action":"open"}`))
			if code != http.StatusOK || body["window_open"] != true {
				t.Fatalf("open status=%d body=%#v", code, body)
			}
			if err := srv.observeComputerUseProviderFrame(
				computerUseTestProvider, computerUseTestSession, tc.frame,
			); err != nil {
				t.Fatalf("terminal cleanup: %v", err)
			}

			seen := helper.seen()
			if len(seen) != 2 || seen[1]["op"] != "window_close" {
				t.Fatalf("terminal requests=%#v, want open then close", seen)
			}
			wantOwner := computerUseOwnerID(computerUseTestProvider, computerUseTestSession, "turn-1")
			if seen[1]["turn_id"] != wantOwner {
				t.Fatalf("terminal close owner=%v, want %s", seen[1]["turn_id"], wantOwner)
			}

			code, body = doJSON(t, srv, http.MethodPost, "/computer_use/action",
				computerUseBody(t, `{"turn_id":"turn-1","action":"pointer.move","x":1,"y":2}`))
			if code != http.StatusConflict || body["code"] != "turn_not_active" {
				t.Fatalf("completed turn status=%d body=%#v", code, body)
			}
			if got := len(helper.seen()); got != 2 {
				t.Fatalf("completed turn reached helper; requests=%#v", helper.seen())
			}

			startComputerUseTurn(srv, computerUseTestProvider, computerUseTestSession, "turn-1")
			code, body = doJSON(t, srv, http.MethodPost, "/computer_use/action",
				computerUseBody(t, `{"turn_id":"turn-1","action":"pointer.move","x":1,"y":2}`))
			if code != http.StatusConflict || body["code"] != "turn_not_active" {
				t.Fatalf("replayed start resurrected terminal turn: status=%d body=%#v", code, body)
			}
		})
	}
}

func TestStaleTerminalAndStartedFramesCannotReplaceANewerLease(t *testing.T) {
	helper := startWindowTrackingHelper(t)
	srv := computerUseServer(t, func(cfg *config.Config) {
		cfg.ComputerUse.Enabled = true
		cfg.ComputerUse.HelperSocket = helper.path
	})

	startComputerUseTurn(srv, computerUseTestProvider, computerUseTestSession, "turn-old")
	startComputerUseTurn(srv, computerUseTestProvider, computerUseTestSession, "turn-new")
	if err := srv.observeComputerUseProviderFrame(
		computerUseTestProvider, computerUseTestSession,
		map[string]any{"type": "turn", "status": "completed", "turn_id": "turn-old"},
	); err != nil {
		t.Fatalf("stale terminal cleanup: %v", err)
	}
	// A replayed start for the completed id is also stale. Neither frame may
	// revoke or replace the real current lease.
	startComputerUseTurn(srv, computerUseTestProvider, computerUseTestSession, "turn-old")

	code, body := doJSON(t, srv, http.MethodPost, "/computer_use/action",
		computerUseBody(t, `{"turn_id":"turn-new","action":"pointer.move","x":1,"y":2}`))
	if code != http.StatusOK || body["ok"] != true {
		t.Fatalf("stale frames replaced newer lease: status=%d body=%#v", code, body)
	}
	code, body = doJSON(t, srv, http.MethodPost, "/computer_use/action",
		computerUseBody(t, `{"turn_id":"turn-old","action":"pointer.move","x":1,"y":2}`))
	if code != http.StatusConflict || body["code"] != "turn_not_active" {
		t.Fatalf("completed old lease revived: status=%d body=%#v", code, body)
	}
}

func TestProviderStartWithoutTurnIDFailsClosed(t *testing.T) {
	helper := startWindowTrackingHelper(t)
	srv := computerUseServer(t, func(cfg *config.Config) {
		cfg.ComputerUse.Enabled = true
		cfg.ComputerUse.LockedUse.Enabled = true
		cfg.ComputerUse.HelperSocket = helper.path
	})
	startComputerUseTurn(srv, "claude", "claude-session", "real-turn")

	code, body := doJSON(t, srv, http.MethodPost, "/computer_use/window",
		`{"provider_id":"claude","session_id":"claude-session","turn_id":"real-turn","action":"open"}`)
	if code != http.StatusOK || body["window_open"] != true {
		t.Fatalf("open status=%d body=%#v", code, body)
	}
	if err := srv.observeComputerUseProviderFrame("claude", "claude-session", map[string]any{
		"type": "turn", "status": "started", "turn_id": nil,
	}); err != nil {
		t.Fatalf("missing-id cleanup: %v", err)
	}

	seen := helper.seen()
	if len(seen) != 2 || seen[1]["op"] != "window_close" {
		t.Fatalf("missing-id frame did not close the old lease: %#v", seen)
	}
	code, body = doJSON(t, srv, http.MethodPost, "/computer_use/action",
		`{"provider_id":"claude","session_id":"claude-session","turn_id":"real-turn","action":"pointer.move","x":1,"y":2}`)
	if code != http.StatusConflict || body["code"] != "turn_not_active" {
		t.Fatalf("missing-id frame left authority active: status=%d body=%#v", code, body)
	}
}

func TestProviderTerminalWaitsForRelockConfirmation(t *testing.T) {
	helper := startFakeHelper(t, func(req map[string]any) map[string]any {
		switch req["op"] {
		case "window_open":
			return map[string]any{
				"ok": true, "window_open": true,
				"window_turn_id": req["turn_id"], "window_closing": false,
			}
		case "window_close":
			return refuse("failed", "relock could not be confirmed")
		default:
			return nil
		}
	})
	srv := computerUseServer(t, func(cfg *config.Config) {
		cfg.ComputerUse.Enabled = true
		cfg.ComputerUse.LockedUse.Enabled = true
		cfg.ComputerUse.HelperSocket = helper.path
	})
	startComputerUseTurn(srv, computerUseTestProvider, computerUseTestSession, "turn-1")
	code, body := doJSON(t, srv, http.MethodPost, "/computer_use/window",
		computerUseBody(t, `{"turn_id":"turn-1","action":"open"}`))
	if code != http.StatusOK {
		t.Fatalf("open status=%d body=%#v", code, body)
	}

	err := srv.observeComputerUseProviderFrame(computerUseTestProvider, computerUseTestSession,
		map[string]any{"type": "turn", "status": "completed", "turn_id": "turn-1"})
	if err == nil || computerUseErrorCode(err) != "failed" {
		t.Fatalf("terminal relock err=%v, want preserved helper failure", err)
	}
	code, body = doJSON(t, srv, http.MethodPost, "/computer_use/action",
		computerUseBody(t, `{"turn_id":"turn-1","action":"pointer.move","x":1,"y":2}`))
	if code != http.StatusConflict || body["code"] != "turn_not_active" {
		t.Fatalf("failed relock left lease active: status=%d body=%#v", code, body)
	}
}

func TestInterruptRevokesLeaseAndRelocksImmediately(t *testing.T) {
	helper := startWindowTrackingHelper(t)
	srv := computerUseServer(t, func(cfg *config.Config) {
		cfg.ComputerUse.Enabled = true
		cfg.ComputerUse.LockedUse.Enabled = true
		cfg.ComputerUse.HelperSocket = helper.path
	})
	srv.registry[computerUseTestProvider] = &fakePushProvider{id: computerUseTestProvider}
	if err := srv.store.UpsertSession(state.Record{
		"session_id": computerUseTestSession, "provider_id": computerUseTestProvider,
	}); err != nil {
		t.Fatal(err)
	}
	startComputerUseTurn(srv, computerUseTestProvider, computerUseTestSession, "turn-1")
	code, body := doJSON(t, srv, http.MethodPost, "/computer_use/window",
		computerUseBody(t, `{"turn_id":"turn-1","action":"open"}`))
	if code != http.StatusOK {
		t.Fatalf("open status=%d body=%#v", code, body)
	}

	code, body = doJSON(t, srv, http.MethodPost, "/interrupt",
		`{"provider_id":"codex","session_id":"computer-use-session"}`)
	if code != http.StatusOK || body["ok"] != true {
		t.Fatalf("interrupt status=%d body=%#v", code, body)
	}
	seen := helper.seen()
	if len(seen) != 2 || seen[1]["op"] != "window_close" {
		t.Fatalf("interrupt requests=%#v, want open then close", seen)
	}
}

func TestComputerUseWindowBusyPreservesCodeButHidesOwner(t *testing.T) {
	helper := startFakeHelper(t, func(req map[string]any) map[string]any {
		if req["op"] == "window_open" {
			return refuse("window_busy", "another turn (secret-owner) owns the locked-use window")
		}
		return nil
	})
	srv := computerUseServer(t, func(cfg *config.Config) {
		cfg.ComputerUse.Enabled = true
		cfg.ComputerUse.LockedUse.Enabled = true
		cfg.ComputerUse.HelperSocket = helper.path
	})
	startComputerUseTurn(srv, computerUseTestProvider, computerUseTestSession, "other-turn")

	code, body := doJSON(t, srv, http.MethodPost, "/computer_use/window",
		computerUseBody(t, `{"turn_id":"other-turn","action":"open"}`))
	if code != http.StatusConflict || body["code"] != "window_busy" {
		t.Fatalf("window busy status=%d body=%#v", code, body)
	}
	if detail, _ := body["detail"].(string); strings.Contains(detail, "secret-owner") {
		t.Fatalf("window busy response exposed the owner: %#v", body)
	}
}

func TestComputerUseWindowUsesOperationStateAndPropagatesCloseFailure(t *testing.T) {
	helper := startFakeHelper(t, func(req map[string]any) map[string]any {
		switch req["op"] {
		case "window_open":
			return map[string]any{
				"ok": true, "window_open": true,
				"window_turn_id": req["turn_id"], "window_closing": false,
			}
		case "window_close":
			return refuse("failed", "relock could not be confirmed")
		default:
			return nil
		}
	})
	srv := computerUseServer(t, func(cfg *config.Config) {
		cfg.ComputerUse.Enabled = true
		cfg.ComputerUse.LockedUse.Enabled = true
		cfg.ComputerUse.HelperSocket = helper.path
	})
	startComputerUseTurn(srv, computerUseTestProvider, computerUseTestSession, "turn-1")

	code, body := doJSON(t, srv, http.MethodPost, "/computer_use/window",
		computerUseBody(t, `{"turn_id":"turn-1","action":"open"}`))
	if code != http.StatusOK || body["window_open"] != true ||
		body["window_registered"] != true || body["window_phase"] != "open" ||
		body["window_turn_id"] != "turn-1" {
		t.Fatalf("open status=%d body=%#v", code, body)
	}
	seen := helper.seen()
	if len(seen) != 1 || seen[0]["op"] != "window_open" {
		t.Fatalf("open performed a second state query: %#v", seen)
	}

	code, body = doJSON(t, srv, http.MethodPost, "/computer_use/window",
		computerUseBody(t, `{"turn_id":"turn-1","action":"close"}`))
	if code != http.StatusInternalServerError || body["code"] != "failed" {
		t.Fatalf("close status=%d body=%#v, want 500/failed", code, body)
	}
}

func TestComputerUseCloseDoesNotClaimSuccessWhenHelperIsUnavailable(t *testing.T) {
	srv := computerUseServer(t, func(cfg *config.Config) {
		cfg.ComputerUse.Enabled = true
		cfg.ComputerUse.LockedUse.Enabled = true
		cfg.ComputerUse.HelperSocket = "/tmp/ra-absent-helper.sock"
	})
	code, body := doJSON(t, srv, http.MethodPost, "/computer_use/window",
		computerUseBody(t, `{"turn_id":"turn-1","action":"close"}`))
	if code != http.StatusServiceUnavailable || body["code"] != "helper_unavailable" {
		t.Fatalf("close status=%d body=%#v, want 503/helper_unavailable", code, body)
	}
}

func TestComputerUseAXRejectsUnsafePathsAndForwardsEmptyValue(t *testing.T) {
	helper := startFakeHelper(t, func(req map[string]any) map[string]any {
		return map[string]any{"ok": true}
	})
	srv := computerUseServer(t, func(cfg *config.Config) {
		cfg.ComputerUse.Enabled = true
		cfg.ComputerUse.HelperSocket = helper.path
	})

	for _, body := range []string{
		`{"turn_id":"t1","op":"ax_press","app":"CatDesk","path":[-1]}`,
		`{"turn_id":"t1","op":"ax_press","app":"CatDesk","path":[0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0]}`,
	} {
		code, out := doJSON(t, srv, http.MethodPost, "/computer_use/ax", computerUseBody(t, body))
		if code != http.StatusBadRequest || out["code"] != "bad_request" {
			t.Errorf("unsafe path status=%d body=%#v", code, out)
		}
	}

	startComputerUseTurn(srv, computerUseTestProvider, computerUseTestSession, "t1")
	code, body := doJSON(t, srv, http.MethodPost, "/computer_use/ax",
		computerUseBody(t, `{"turn_id":"t1","op":"ax_setvalue","app":"CatDesk","path":[0],"value":""}`))
	if code != http.StatusOK {
		t.Fatalf("empty setvalue status=%d body=%#v", code, body)
	}
	seen := helper.seen()
	last := seen[len(seen)-1]
	if value, present := last["value"]; !present || value != "" {
		t.Fatalf("empty value not forwarded: %#v", last)
	}
}
