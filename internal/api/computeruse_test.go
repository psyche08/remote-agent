package api

import (
	"bufio"
	"encoding/json"
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

// fakeHelper is a scripted stand-in for remote-agent-desktop.
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
	} {
		code, out := doJSON(t, srv, http.MethodPost, route.path, route.body)
		if code != http.StatusConflict {
			t.Errorf("%s status=%d body=%#v, want 409", route.path, code, out)
		}
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

	code, body := doJSON(t, srv, http.MethodPost, "/computer_use/locked_use", `{"active":true}`)
	if code != http.StatusConflict {
		t.Fatalf("status=%d body=%#v, want 409", code, body)
	}
	if body["status"] != "not_enabled" {
		t.Fatalf("status field = %v, want not_enabled", body["status"])
	}

	code, body = doJSON(t, srv, http.MethodPost, "/computer_use/window", `{"turn_id":"t1","action":"open"}`)
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
		return refuse("bad_request", "unknown computer-use action")
	})
	srv := computerUseServer(t, func(cfg *config.Config) {
		cfg.ComputerUse.Enabled = true
		cfg.ComputerUse.HelperSocket = helper.path
	})
	for _, body := range []string{
		`{"action":"shell.exec"}`,
		`{"action":"pointer.click"}`,
		`{"action":"keyboard.key","keys":[]}`,
		`{"action":""}`,
	} {
		code, out := doJSON(t, srv, http.MethodPost, "/computer_use/action", body)
		if code != http.StatusBadRequest {
			t.Errorf("action %s status=%d body=%#v, want 400", body, code, out)
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

	code, body := doJSON(t, srv, http.MethodPost, "/computer_use/action",
		`{"action":"pointer.click","x":0,"y":0,"button":"right","count":2}`)
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
		"op": "action", "action": "pointer.click",
		"x": float64(0), "y": float64(0), "button": "right", "count": float64(2),
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

	code, body := doJSON(t, srv, http.MethodPost, "/computer_use/action",
		`{"action":"pointer.move","x":1,"y":1}`)
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
		code, out := doJSON(t, srv, http.MethodPost, "/computer_use/window", body)
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
	for _, path := range []string{"/computer_use/locked_use", "/computer_use/window", "/computer_use/action"} {
		code, _ := doJSON(t, srv, http.MethodGet, path, "")
		if code != http.StatusMethodNotAllowed {
			t.Errorf("GET %s status=%d, want 405", path, code)
		}
	}
	if code, _ := doJSON(t, srv, http.MethodPost, "/computer_use", `{}`); code != http.StatusMethodNotAllowed {
		t.Errorf("POST /computer_use status=%d, want 405", code)
	}
}

// The capture gate only restricts capture while a Locked Use window is open.
// A device without the feature must keep its existing screenshot behavior.
func TestCaptureGateAllowsWhenNoLockedUseWindow(t *testing.T) {
	srv := computerUseServer(t, nil)
	rr := httptest.NewRecorder()
	if !srv.captureGate(rr) {
		t.Fatal("capture gate refused on a device without computer use")
	}

	helper := startFakeHelper(t, func(req map[string]any) map[string]any {
		return map[string]any{"ok": true, "allowed": true, "reason": ""}
	})
	srv = computerUseServer(t, func(cfg *config.Config) {
		cfg.ComputerUse.Enabled = true
		cfg.ComputerUse.LockedUse.Enabled = true
		cfg.ComputerUse.HelperSocket = helper.path
	})
	rr = httptest.NewRecorder()
	if !srv.captureGate(rr) {
		t.Fatal("capture gate refused with no window open")
	}
}

// The helper owns the shield as windows in its own process, so a helper that
// died took the shield with it and may have left the desktop unlocked and
// uncovered. Capture is the gate whose failure writes what is on screen to a
// file that is then served over the relay, so "cannot tell" must mean "no".
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

// The Accessibility route forwards element-tree operations that work while the
// screen is locked. Like the action route, the vocabulary is enforced in the
// helper; what this layer owns is the op allow-list and faithful forwarding.
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

	// A good read forwards and returns the elements.
	code, body := doJSON(t, srv, http.MethodPost, "/computer_use/ax",
		`{"op":"ax_read","app":"CatDesk"}`)
	if code != http.StatusOK {
		t.Fatalf("ax_read status=%d body=%#v", code, body)
	}
	if _, ok := body["elements"].([]any); !ok {
		t.Fatalf("ax_read did not return elements: %#v", body)
	}

	// The op allow-list is this layer's own check and must be refused before
	// reaching the helper.
	code, _ = doJSON(t, srv, http.MethodPost, "/computer_use/ax", `{"op":"ax_teleport"}`)
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
		`{"op":"ax_setvalue","app":"CatDesk","path":[1,4],"value":"你好"}`)
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
