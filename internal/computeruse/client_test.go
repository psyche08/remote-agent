package computeruse

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// helper is a scripted stand-in for agenthalo-desktop.
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

	_, err := c.OpenWindow("turn-1")
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

	_, err := c.OpenWindow("turn-1")
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

func TestStatusContextDoesNotQueueBehindMutableControllerCall(t *testing.T) {
	h := startHelper(t, func(req map[string]any) map[string]any {
		if req["op"] == "status" {
			return map[string]any{"ok": true, "status": map[string]any{
				"enabled": true, "available": true,
			}}
		}
		return nil
	})
	c := NewController(h.path, true, true)

	// Model an in-flight prompt holding the persistent transport. A status
	// probe must use its own read-only connection instead of waiting for this
	// mutex and inheriting the prompt's 25-second latency.
	c.mu.Lock()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	status := c.StatusContext(ctx)
	cancel()
	c.mu.Unlock()
	c.Stop()
	if status["available"] != true {
		t.Fatalf("status queued behind mutable call or lost helper state: %#v", status)
	}
}

func TestStatusContextHonorsCallerCancellation(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	h := startHelper(t, func(req map[string]any) map[string]any {
		if req["op"] == "status" {
			close(started)
			<-release
		}
		return map[string]any{"ok": true, "status": map[string]any{"available": true}}
	})
	c := NewController(h.path, true, false)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan map[string]any, 1)
	go func() { result <- c.StatusContext(ctx) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("helper never received status probe")
	}
	start := time.Now()
	cancel()
	var status map[string]any
	select {
	case status = <-result:
	case <-time.After(time.Second):
		t.Fatal("status did not stop after caller cancellation")
	}
	c.Stop()
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("status ignored caller cancellation for %s", elapsed)
	}
	if status["available"] != false {
		t.Fatalf("cancelled status reported helper available: %#v", status)
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
		Action: "pointer.click", CoordinateSpace: "screenshot",
		X: intPtr(0), Y: intPtr(0),
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
	if seen[0]["coordinate_space"] != "screenshot" {
		t.Fatalf("coordinate space was not forwarded: %#v", seen[0])
	}
}

func TestRunActionPreservesInMemoryCaptureFields(t *testing.T) {
	h := startHelper(t, func(req map[string]any) map[string]any {
		if req["op"] == "action" && req["action"] == "screen.capture" {
			return map[string]any{
				"ok": true, "action": "screen.capture",
				"media_type": "image/png", "image_base64": "iVBORw0KGgo=",
			}
		}
		return nil
	})
	c := NewController(h.path, true, false)
	defer c.Stop()

	result, err := c.RunAction(ActionRequest{TurnID: "turn-1", Action: "screen.capture"})
	if err != nil {
		t.Fatal(err)
	}
	if result["media_type"] != "image/png" || result["image_base64"] != "iVBORw0KGgo=" {
		t.Fatalf("in-memory capture fields were not preserved: %#v", result)
	}
	if _, exists := result["ok"]; exists {
		t.Fatalf("RunAction leaked the transport ok field: %#v", result)
	}
}

func TestOpenAndCloseUseTheOperationResponse(t *testing.T) {
	windowOpen := false
	h := startHelper(t, func(req map[string]any) map[string]any {
		switch req["op"] {
		case "window_open":
			windowOpen = true
			return map[string]any{
				"ok": true, "window_open": true,
				"window_turn_id": req["turn_id"], "window_closing": false,
			}
		case "window_close":
			windowOpen = false
			return map[string]any{
				"ok": true, "window_open": false,
				"window_turn_id": "", "window_closing": false,
			}
		case "window_state":
			return map[string]any{
				"ok": true, "window_open": windowOpen,
				"window_turn_id": "turn-1", "window_closing": false,
			}
		default:
			return nil
		}
	})
	c := NewController(h.path, true, true)
	defer c.Stop()

	opened, err := c.OpenWindow("turn-1")
	if err != nil || !opened.Open || opened.TurnID != "turn-1" {
		t.Fatalf("open state=%#v err=%v", opened, err)
	}
	closed, err := c.CloseWindowForTurn("turn-1", "done")
	if err != nil || closed.Open || closed.Closing {
		t.Fatalf("close state=%#v err=%v", closed, err)
	}
	seen := h.seen()
	if len(seen) != 2 {
		t.Fatalf("helper saw %d requests, want exactly open and close", len(seen))
	}
}

func TestUnconfirmedOpenClosesTheSameOwner(t *testing.T) {
	h := startHelper(t, func(req map[string]any) map[string]any {
		switch req["op"] {
		case "window_open":
			// The helper claims the window is open but omits part of the
			// authoritative state. The caller cannot treat this as success or
			// leave the possibly open window until its TTL.
			return map[string]any{
				"ok": true, "window_open": true, "window_turn_id": req["turn_id"],
			}
		case "window_close":
			return map[string]any{
				"ok": true, "window_open": false,
				"window_turn_id": "", "window_closing": false,
			}
		default:
			return nil
		}
	})
	c := NewController(h.path, true, true)
	defer c.Stop()

	if state, err := c.OpenWindow("turn-1"); err == nil {
		t.Fatalf("incomplete open state=%#v reported success", state)
	}
	seen := h.seen()
	if len(seen) != 2 || seen[0]["op"] != "window_open" || seen[1]["op"] != "window_close" {
		t.Fatalf("unconfirmed open requests=%#v, want open then scoped close", seen)
	}
	if seen[1]["turn_id"] != "turn-1" || seen[1]["reason"] != "window open was not confirmed" {
		t.Fatalf("unconfirmed open used the wrong cleanup owner: %#v", seen[1])
	}
}

func TestClosePreservesTransportAndOwnerErrors(t *testing.T) {
	c := NewController("/tmp/ra-no-such-helper.sock", true, true)
	if _, err := c.CloseWindowForTurn("turn-1", "done"); !errors.Is(err, ErrHelperUnavailable) {
		t.Fatalf("unavailable close err=%v, want ErrHelperUnavailable", err)
	}

	h := startHelper(t, func(req map[string]any) map[string]any {
		if req["op"] == "window_close" {
			return map[string]any{
				"ok": true, "window_open": true,
				"window_turn_id": "turn-2", "window_closing": false,
			}
		}
		return nil
	})
	c = NewController(h.path, true, true)
	defer c.Stop()
	if _, err := c.CloseWindowForTurn("turn-1", "done"); !errors.Is(err, ErrWindowBusy) {
		t.Fatalf("wrong-owner close err=%v, want ErrWindowBusy", err)
	}
}

func TestCloseRequiresTheHelperToClearItsOwner(t *testing.T) {
	h := startHelper(t, func(req map[string]any) map[string]any {
		if req["op"] == "window_close" {
			return map[string]any{
				"ok": true, "window_open": false,
				"window_turn_id": "turn-1", "window_closing": false,
			}
		}
		return nil
	})
	c := NewController(h.path, true, true)
	defer c.Stop()
	if state, err := c.CloseWindowForTurn("turn-1", "done"); err == nil {
		t.Fatalf("stale owner state=%#v reported a confirmed relock", state)
	}
}

func TestWindowStatePreservesTransportAndProtocolErrors(t *testing.T) {
	c := NewController("/tmp/ra-no-such-helper.sock", true, true)
	if _, err := c.WindowState(); !errors.Is(err, ErrHelperUnavailable) {
		t.Fatalf("unavailable state err=%v, want ErrHelperUnavailable", err)
	}

	h := startHelper(t, nil)
	c = NewController(h.path, true, false)
	defer c.Stop()
	if state, err := c.WindowState(); err == nil {
		t.Fatalf("incomplete helper state=%#v reported no protocol error", state)
	}
}

func TestStopClosesAnOpenWindowBeforeDisconnecting(t *testing.T) {
	windowOpen := true
	h := startHelper(t, func(req map[string]any) map[string]any {
		switch req["op"] {
		case "window_state":
			return map[string]any{
				"ok": true, "window_open": windowOpen,
				"window_turn_id": "turn-1", "window_closing": false,
			}
		case "window_close":
			windowOpen = false
			return map[string]any{
				"ok": true, "window_open": false,
				"window_turn_id": "", "window_closing": false,
			}
		default:
			return nil
		}
	})
	c := NewController(h.path, true, true)
	c.Stop()

	seen := h.seen()
	if len(seen) != 2 || seen[0]["op"] != "window_state" || seen[1]["op"] != "window_close" {
		t.Fatalf("shutdown requests=%#v, want window_state then window_close", seen)
	}
	if seen[1]["reason"] != "AgentHalo shutdown" {
		t.Fatalf("shutdown close reason=%v", seen[1]["reason"])
	}
}

func TestStopCancelsAndWaitsForAnOpeningWindow(t *testing.T) {
	phase := WindowPhaseOpening
	h := startHelper(t, func(req map[string]any) map[string]any {
		switch req["op"] {
		case "window_state":
			return map[string]any{
				"ok": true, "window_registered": phase != WindowPhaseClosed,
				"window_phase": phase, "window_open": phase == WindowPhaseOpen,
				"window_turn_id": "turn-opening", "window_closing": phase == WindowPhaseClosing,
			}
		case "window_close":
			phase = WindowPhaseClosed
			return map[string]any{
				"ok": true, "window_registered": false,
				"window_phase": WindowPhaseClosed, "window_open": false,
				"window_turn_id": "", "window_closing": false,
			}
		default:
			return nil
		}
	})
	c := NewController(h.path, true, true)
	c.Stop()

	seen := h.seen()
	if len(seen) != 2 || seen[0]["op"] != "window_state" || seen[1]["op"] != "window_close" {
		t.Fatalf("opening shutdown requests=%#v, want state then synchronous close", seen)
	}
	if seen[1]["turn_id"] != "turn-opening" {
		t.Fatalf("opening shutdown lost the registered owner: %#v", seen[1])
	}
}

func TestStopWaitsForClosingWindowConfirmation(t *testing.T) {
	closeStarted := make(chan struct{})
	allowClose := make(chan struct{})
	h := startHelper(t, func(req map[string]any) map[string]any {
		switch req["op"] {
		case "window_state":
			return map[string]any{
				"ok": true, "window_registered": true,
				"window_phase": WindowPhaseClosing, "window_open": false,
				"window_turn_id": "turn-closing", "window_closing": true,
			}
		case "window_close":
			close(closeStarted)
			<-allowClose
			return map[string]any{
				"ok": true, "window_registered": false,
				"window_phase": WindowPhaseClosed, "window_open": false,
				"window_turn_id": "", "window_closing": false,
			}
		default:
			return nil
		}
	})
	c := NewController(h.path, true, true)
	done := make(chan struct{})
	go func() {
		c.Stop()
		close(done)
	}()
	<-closeStarted
	select {
	case <-done:
		t.Fatal("Stop returned before the helper confirmed closing-window cleanup")
	default:
	}
	close(allowClose)
	<-done
}

func TestPrepareForRestartRequiresAtomicHelperConfirmation(t *testing.T) {
	h := startHelper(t, func(req map[string]any) map[string]any {
		if req["op"] != "prepare_restart" {
			t.Fatalf("restart preflight used non-atomic request: %#v", req)
		}
		return map[string]any{"ok": true, "safe_to_restart": true}
	})
	c := NewController(h.path, true, true)
	if err := c.PrepareForRestart(); err != nil {
		t.Fatalf("restart preflight: %v", err)
	}
	seen := h.seen()
	if len(seen) != 1 || seen[0]["op"] != "prepare_restart" {
		t.Fatalf("restart preflight requests=%#v, want one atomic prepare_restart", seen)
	}
}

func TestPrepareForRestartFailsClosedWithoutStrictSafeConfirmation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reply map[string]any
	}{
		{name: "missing", reply: map[string]any{"ok": true}},
		{name: "false", reply: map[string]any{"ok": true, "safe_to_restart": false}},
		{name: "wrong-type", reply: map[string]any{"ok": true, "safe_to_restart": "true"}},
		{name: "legacy-unknown-op", reply: map[string]any{
			"ok": false, "code": "bad_request", "error": "unknown op",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := startHelper(t, func(map[string]any) map[string]any { return tc.reply })
			c := NewController(h.path, true, true)
			if err := c.PrepareForRestart(); err == nil {
				t.Fatal("helper without strict safe_to_restart=true was accepted")
			}
			seen := h.seen()
			if len(seen) != 1 || seen[0]["op"] != "prepare_restart" {
				t.Fatalf("unsafe preflight requests=%#v", seen)
			}
		})
	}
}

func TestAXForwardsEmptyValueAndRejectsUnsafePaths(t *testing.T) {
	h := startHelper(t, nil)
	c := NewController(h.path, true, false)
	defer c.Stop()
	empty := ""
	if _, err := c.RunAX(AXRequest{
		TurnID: "turn-1", Op: "ax_setvalue", App: "Notes", Path: []int{0}, Value: &empty,
	}); err != nil {
		t.Fatalf("empty setvalue: %v", err)
	}
	seen := h.seen()
	if got, ok := seen[0]["value"]; !ok || got != "" {
		t.Fatalf("empty value was not forwarded: %#v", seen[0])
	}
	if seen[0]["turn_id"] != "turn-1" {
		t.Fatalf("turn_id was not forwarded: %#v", seen[0])
	}
	if _, err := c.RunAX(AXRequest{
		TurnID: "turn-focus", Op: "ax_focus", BundleID: "com.example.App", Path: []int{2, 1},
	}); err != nil {
		t.Fatalf("focus: %v", err)
	}
	seen = h.seen()
	focus := seen[len(seen)-1]
	if focus["op"] != "ax_focus" || focus["turn_id"] != "turn-focus" ||
		focus["bundle_id"] != "com.example.App" {
		t.Fatalf("focus fields were not forwarded: %#v", focus)
	}
	path, ok := focus["path"].([]any)
	if !ok || len(path) != 2 || path[0] != float64(2) || path[1] != float64(1) {
		t.Fatalf("focus path was not forwarded: %#v", focus)
	}
	if _, present := focus["value"]; present {
		t.Fatalf("focus unexpectedly forwarded a value: %#v", focus)
	}
	if _, err := c.RunAX(AXRequest{Op: "ax_press", Path: []int{-1}}); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("negative path err=%v, want ErrBadRequest", err)
	}
	tooDeep := make([]int, MaxAXPathDepth+1)
	if _, err := c.RunAX(AXRequest{Op: "ax_press", Path: tooDeep}); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("deep path err=%v, want ErrBadRequest", err)
	}
}

func intPtr(v int) *int { return &v }
