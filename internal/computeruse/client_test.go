package computeruse

import (
	"bufio"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// helper is a scripted stand-in for remote-agent-desktop.
type helper struct {
	listener net.Listener
	path     string

	mu       sync.Mutex
	requests []map[string]any
	reply    func(map[string]any) map[string]any
	// hangUp closes the connection without answering, standing in for a helper
	// that died mid-request.
	hangUp bool
}

func startHelper(t *testing.T, reply func(map[string]any) map[string]any) *helper {
	t.Helper()
	// sockaddr_un allows 104 bytes, and macOS TempDir paths do not fit.
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
	h := &helper{listener: listener, path: path, reply: reply}
	t.Cleanup(func() { listener.Close() })
	go h.serve()
	return h
}

func (h *helper) serve() {
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
				reply, hangUp := h.reply, h.hangUp
				h.mu.Unlock()
				if hangUp {
					return
				}
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

func (h *helper) seen() []map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]map[string]any(nil), h.requests...)
}

func (h *helper) setHangUp(v bool) {
	h.mu.Lock()
	h.hangUp = v
	h.mu.Unlock()
}

// A refusal is an answer, not a transport failure. Resending it would ask the
// helper twice — and for an action that half-executed before failing, the
// second attempt would run it again.
func TestARefusedRequestIsNotResent(t *testing.T) {
	h := startHelper(t, func(map[string]any) map[string]any {
		return map[string]any{
			"ok": false, "code": "local_input", "error": "local input detected at the device",
		}
	})
	c := NewController(h.path, true, true)
	defer c.Stop()

	err := c.OpenWindow("turn-1")
	if !errors.Is(err, ErrLocalInput) {
		t.Fatalf("err = %v, want ErrLocalInput", err)
	}
	if got := len(h.seen()); got != 1 {
		t.Fatalf("helper saw %d requests, want 1 — a refusal was retried", got)
	}
}

// A request that was written but never answered may have been acted on. It must
// not be sent again.
func TestARequestThatMayHaveRunIsNotResent(t *testing.T) {
	h := startHelper(t, nil)
	h.setHangUp(true)
	c := NewController(h.path, true, true)
	defer c.Stop()

	if _, err := c.RunAction(ActionRequest{Action: "keyboard.type", Text: "hello"}); err == nil {
		t.Fatal("a hung-up request reported success")
	}
	if got := len(h.seen()); got != 1 {
		t.Fatalf("helper saw %d requests, want 1 — an action that may have run was retried", got)
	}
}

// A cached connection the helper closed on restart fails at the write, before
// anything is delivered. That one is safe to retry, and must be, or a routine
// helper restart would surface as a failed action.
func TestAStaleConnectionIsReconnectedAndTheRequestSucceeds(t *testing.T) {
	h := startHelper(t, func(req map[string]any) map[string]any {
		return map[string]any{"ok": true, "action": req["action"]}
	})
	c := NewController(h.path, true, true)
	defer c.Stop()

	// Establish and cache a connection.
	if _, err := c.RunAction(ActionRequest{Action: "pointer.move", X: intPtr(1), Y: intPtr(2)}); err != nil {
		t.Fatalf("first action: %v", err)
	}
	// Break it the way a restarted helper does.
	c.mu.Lock()
	if c.conn != nil {
		c.conn.Close()
	}
	c.mu.Unlock()

	res, err := c.RunAction(ActionRequest{Action: "pointer.move", X: intPtr(3), Y: intPtr(4)})
	if err != nil {
		t.Fatalf("after a stale connection: %v", err)
	}
	if res["action"] != "pointer.move" {
		t.Fatalf("result = %#v", res)
	}
}

// An absent helper is a fault, not a disabled feature: reporting it as "off"
// would hide a broken device behind a setting.
func TestAnAbsentHelperReportsUnavailable(t *testing.T) {
	c := NewController("/tmp/ra-no-such-helper.sock", true, true)
	defer c.Stop()

	err := c.OpenWindow("turn-1")
	if !errors.Is(err, ErrHelperUnavailable) {
		t.Fatalf("err = %v, want ErrHelperUnavailable", err)
	}
	status := c.Status()
	if status["available"] != false {
		t.Errorf("status reported available with no helper: %#v", status)
	}
	if status["enabled"] != true {
		t.Errorf("status hid the configured feature: %#v", status)
	}
}

// Capture is the gate whose failure writes what is on screen to a file that is
// then served over the relay, so an unanswerable shield must mean "no". Where
// Locked Use is off no window can exist, and refusing there would disable
// ordinary screenshots on every device without the helper.
func TestCaptureGateFailsClosedOnlyWhenLockedUseIsConfigured(t *testing.T) {
	withLockedUse := NewController("/tmp/ra-no-such-helper.sock", true, true)
	defer withLockedUse.Stop()
	if allowed, _ := withLockedUse.CaptureAllowed(); allowed {
		t.Error("capture allowed while Locked Use was configured and unverifiable")
	}

	withoutLockedUse := NewController("/tmp/ra-no-such-helper.sock", true, false)
	defer withoutLockedUse.Stop()
	if allowed, _ := withoutLockedUse.CaptureAllowed(); !allowed {
		t.Error("capture refused on a device without Locked Use")
	}
}

// Zero is the coordinate most easily lost: a click at (0,0) must not arrive as
// a click with no coordinates, which the helper refuses as malformed.
func TestZeroCoordinatesSurviveForwarding(t *testing.T) {
	h := startHelper(t, nil)
	c := NewController(h.path, true, true)
	defer c.Stop()

	if _, err := c.RunAction(ActionRequest{
		Action: "pointer.click", X: intPtr(0), Y: intPtr(0),
	}); err != nil {
		t.Fatalf("action: %v", err)
	}
	seen := h.seen()
	if len(seen) != 1 {
		t.Fatalf("helper saw %d requests", len(seen))
	}
	for _, key := range []string{"x", "y"} {
		if _, ok := seen[0][key]; !ok {
			t.Errorf("%s was dropped from the forwarded action: %#v", key, seen[0])
		}
	}
}

func intPtr(v int) *int { return &v }
