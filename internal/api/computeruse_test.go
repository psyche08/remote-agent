package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/psyche08/remote-agent/internal/config"
	"github.com/psyche08/remote-agent/internal/provider"
	"github.com/psyche08/remote-agent/internal/state"
)

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
	srv := computerUseServer(t, func(cfg *config.Config) {
		cfg.ComputerUse.Enabled = true
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
// request may grant a Mac the ability to unlock itself.
func TestLockedUseCannotBeEnabledOverTheAPI(t *testing.T) {
	srv := computerUseServer(t, func(cfg *config.Config) {
		cfg.ComputerUse.Enabled = true
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
	srv := computerUseServer(t, func(cfg *config.Config) {
		cfg.ComputerUse.Enabled = true
		cfg.ComputerUse.LockedUse.Enabled = true
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

func TestComputerUseActionValidatesRequests(t *testing.T) {
	srv := computerUseServer(t, func(cfg *config.Config) {
		cfg.ComputerUse.Enabled = true
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

func TestComputerUseWindowRejectsBadInput(t *testing.T) {
	srv := computerUseServer(t, func(cfg *config.Config) {
		cfg.ComputerUse.Enabled = true
		cfg.ComputerUse.LockedUse.Enabled = true
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

	srv = computerUseServer(t, func(cfg *config.Config) {
		cfg.ComputerUse.Enabled = true
		cfg.ComputerUse.LockedUse.Enabled = true
	})
	rr = httptest.NewRecorder()
	if !srv.captureGate(rr) {
		t.Fatal("capture gate refused with no window open")
	}
}
