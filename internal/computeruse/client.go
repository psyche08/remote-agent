// Package computeruse is the agent's client for the macOS desktop helper.
//
// The desktop surface and the Locked Use state machine live in
// mac/RemoteAgentDesktop, a resident Swift process that owns the display shield
// as windows, mints grants, and enforces every safeguard. This package only
// forwards: it holds no policy, no key, and no window state, so there is
// exactly one implementation of each and no way for the two to disagree.
//
// Two consequences are worth stating plainly, because they are easy to
// misread:
//
//   - The helper reads the device's config file itself. Nothing here can
//     configure it. Locked Use lets a machine unlock itself, so the capability
//     has to be granted on the device; a client that could enable it over a
//     socket would defeat the point.
//   - The action vocabulary is validated in the helper, not here. That is not a
//     weakening: the socket is reachable by any process running as this user,
//     so the helper's validator was always the one guarding the desktop. A copy
//     here would only guard the HTTP path, and a copy that drifted would be
//     worse than none.
package computeruse

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

// Controller is the agent-side handle on the desktop helper. The name is kept
// from the in-process implementation it replaces so the API layer reads the
// same either way.
type Controller struct {
	socketPath string
	enabled    bool
	// lockedUseEnabled mirrors the device's config so the capture gate can tell
	// "the helper is unreachable and a window might be open" from "no window
	// can exist here at all".
	lockedUseEnabled bool

	// One connection at a time, so a slow action cannot interleave its reply
	// with a status poll on the same socket. The helper serves each connection
	// on its own queue, so this costs nothing but ordering.
	mu   sync.Mutex
	conn net.Conn
	rd   *bufio.Reader
}

const (
	// dialTimeout bounds reconnecting to a helper that is not listening.
	dialTimeout = 2 * time.Second
	// callTimeout bounds an ordinary request. Opening a window is the slow one:
	// it waits for macOS to complete an unlock, bounded by the grant's own
	// lifetime, which keeps it well inside the relay's 30s budget.
	callTimeout = 25 * time.Second
	// quickTimeout bounds the status and gate calls that sit on request paths
	// which must stay responsive.
	quickTimeout = 5 * time.Second
)

// NewController returns a handle on the helper at socketPath.
func NewController(socketPath string, enabled, lockedUseEnabled bool) *Controller {
	return &Controller{
		socketPath: socketPath, enabled: enabled, lockedUseEnabled: lockedUseEnabled,
	}
}

// Start and Stop exist for symmetry with the server's lifecycle. The helper is
// a separate long-lived process with its own startup scrub and its own
// shutdown relock, so neither owns the feature's lifetime.
func (c *Controller) Start() {}

func (c *Controller) Stop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closeLocked()
}

// Error is a refusal from the helper. Code is the stable machine-readable
// reason; Detail is the helper's message and never carries grant or key
// material.
type Error struct {
	Code   string
	Detail string
}

func (e *Error) Error() string {
	if e.Detail == "" {
		return e.Code
	}
	return e.Detail
}

// Is maps helper codes onto this package's sentinels so callers keep using
// errors.Is rather than string-matching a message that may be reworded.
func (e *Error) Is(target error) bool {
	switch target {
	case ErrNotEnabled:
		return e.Code == "not_enabled"
	case ErrLockedUseNotEnabled:
		return e.Code == "locked_use_not_enabled"
	case ErrNotArmed:
		return e.Code == "not_armed"
	case ErrShieldRequired:
		return e.Code == "shield_required"
	case ErrLocalInput:
		return e.Code == "local_input"
	case ErrNoWindow:
		return e.Code == "no_window" || e.Code == "window_busy"
	case ErrUnsupported:
		return e.Code == "unsupported"
	case ErrBadRequest:
		return e.Code == "bad_request"
	}
	return false
}

var (
	// ErrNotEnabled means the feature is off in config. It is distinct from a
	// runtime failure so the console can tell "not turned on" from "broken".
	ErrNotEnabled = errors.New("computer use is not enabled on this device")
	// ErrLockedUseNotEnabled means computer use is on but Locked Use is not.
	ErrLockedUseNotEnabled = errors.New("locked use is not enabled on this device")
	// ErrNotArmed means Locked Use is configured but could not establish a safe
	// baseline, so it refuses to open windows.
	ErrNotArmed = errors.New("locked use is not armed")
	// ErrShieldRequired means the privacy shield could not be confirmed.
	ErrShieldRequired = errors.New("display shield could not be engaged")
	// ErrLocalInput means a person is using the machine.
	ErrLocalInput = errors.New("local input detected at the device")
	// ErrNoWindow means an action needed an open unlock window and none exists,
	// or another turn owns it.
	ErrNoWindow = errors.New("no open locked-use window for this turn")
	// ErrUnsupported means the device cannot provide the capability at all.
	ErrUnsupported = errors.New("computer use is not supported on this platform")
	// ErrBadRequest means the helper refused the request as malformed.
	ErrBadRequest = errors.New("invalid computer-use request")
	// ErrHelperUnavailable means the helper is not reachable. It is deliberately
	// not folded into ErrNotEnabled: a device where the helper died has computer
	// use configured on and broken, and reporting that as "off" would hide it.
	ErrHelperUnavailable = errors.New("the computer-use helper is not running")
)

// ActionRequest is the raw wire shape accepted by the API and forwarded
// verbatim. Validation happens in the helper, which is the only process that
// can act on it.
type ActionRequest struct {
	Action string   `json:"action"`
	X      *int     `json:"x,omitempty"`
	Y      *int     `json:"y,omitempty"`
	Button string   `json:"button,omitempty"`
	Count  int      `json:"count,omitempty"`
	Text   string   `json:"text,omitempty"`
	Keys   []string `json:"keys,omitempty"`
	DeltaX int      `json:"delta_x,omitempty"`
	DeltaY int      `json:"delta_y,omitempty"`
}

// call sends one request and reads one reply, reconnecting once when the
// request never reached the helper.
//
// What may be retried is decided by delivery, not by whether an error came
// back. A refusal is a valid answer and resending it would ask twice; a read
// that failed after the request was written may have been acted on already, and
// resending an action that half-executed would run it again. Only a request the
// helper never received is safe to send a second time.
func (c *Controller) call(timeout time.Duration, req map[string]any) (map[string]any, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	body = append(body, '\n')

	c.mu.Lock()
	defer c.mu.Unlock()
	res, delivered, err := c.attemptLocked(timeout, body)
	if err == nil || delivered || errors.Is(err, ErrHelperUnavailable) {
		return res, err
	}
	// The bytes never went out, which is what a cached connection looks like
	// after the helper restarted for an update, a crash, or a re-login. One
	// reconnect keeps a routine restart from surfacing as a failed action.
	c.closeLocked()
	res, _, err = c.attemptLocked(timeout, body)
	return res, err
}

// attemptLocked sends one request. delivered reports whether it reached the
// helper, so the caller can tell a lost request from one that may have run.
func (c *Controller) attemptLocked(
	timeout time.Duration, body []byte,
) (result map[string]any, delivered bool, err error) {
	if c.conn == nil {
		conn, dialErr := net.DialTimeout("unix", c.socketPath, dialTimeout)
		if dialErr != nil {
			return nil, false, fmt.Errorf("%w: %v", ErrHelperUnavailable, dialErr)
		}
		c.conn = conn
		c.rd = bufio.NewReader(conn)
	}
	if err := c.conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		c.closeLocked()
		return nil, false, err
	}
	if _, err := c.conn.Write(body); err != nil {
		c.closeLocked()
		return nil, false, err
	}
	line, err := c.rd.ReadBytes('\n')
	if err != nil {
		c.closeLocked()
		// Written, so the helper may have acted on it. Not retryable.
		return nil, true, err
	}
	var res map[string]any
	if err := json.Unmarshal(line, &res); err != nil {
		c.closeLocked()
		return nil, true, errors.New("the computer-use helper returned malformed output")
	}
	if ok, _ := res["ok"].(bool); !ok {
		detail, _ := res["error"].(string)
		code, _ := res["code"].(string)
		if code == "" {
			code = "failed"
		}
		return nil, true, &Error{Code: code, Detail: detail}
	}
	return res, true, nil
}

func (c *Controller) closeLocked() {
	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
		c.rd = nil
	}
}

// Status describes the feature for the console. It reports capability and
// state only — never a grant, a nonce, or key material.
//
// A helper that cannot be reached is reported as configured-but-unavailable
// rather than as disabled, so an operator sees a broken device instead of an
// apparently switched-off one.
func (c *Controller) Status() map[string]any {
	res, err := c.call(quickTimeout, map[string]any{"op": "status"})
	if err != nil {
		return map[string]any{
			"enabled":   c.enabled,
			"available": false,
			"detail":    err.Error(),
		}
	}
	status, _ := res["status"].(map[string]any)
	if status == nil {
		status = map[string]any{}
	}
	return status
}

// SetLockedUseActive is the console's runtime toggle. It can only move within
// what the device's own config already permits; the helper enforces that.
func (c *Controller) SetLockedUseActive(active bool) error {
	_, err := c.call(quickTimeout, map[string]any{
		"op": "locked_use_active", "active": active,
	})
	return err
}

// OpenWindow opens a per-turn unlock window.
func (c *Controller) OpenWindow(turnID string) error {
	_, err := c.call(callTimeout, map[string]any{
		"op": "window_open", "turn_id": turnID,
	})
	return err
}

// CloseWindowForTurn ends the window only if the named turn owns it, so a
// finishing turn cannot relock a window another turn legitimately opened.
func (c *Controller) CloseWindowForTurn(turnID string, reason string) {
	_, _ = c.call(callTimeout, map[string]any{
		"op": "window_close", "turn_id": turnID, "reason": reason,
	})
}

// WindowOpen reports whether a window is currently open, and for which turn.
func (c *Controller) WindowOpen() (string, bool) {
	res, err := c.call(quickTimeout, map[string]any{"op": "window_state"})
	if err != nil {
		return "", false
	}
	open, _ := res["window_open"].(bool)
	turn, _ := res["window_turn_id"].(string)
	return turn, open
}

// CaptureAllowed reports whether a screen capture may proceed right now.
//
// When Locked Use is configured on and the helper cannot answer, this refuses.
// The helper owns the shield as windows in its own process, so a helper that
// died took the shield with it and may have left the desktop unlocked and
// uncovered — and capture is the one gate whose failure writes what is on
// screen to a file that is then served over the relay. "Cannot tell" has to
// mean "no".
//
// Where Locked Use is not configured on, no window can ever exist, so an
// unreachable helper is not a reason to refuse: doing so would disable ordinary
// screenshots on every device that has not installed the helper yet.
func (c *Controller) CaptureAllowed() (bool, string) {
	if !c.lockedUseEnabled {
		return true, ""
	}
	res, err := c.call(quickTimeout, map[string]any{"op": "capture_allowed"})
	if err != nil {
		if errors.Is(err, ErrNotEnabled) || errors.Is(err, ErrLockedUseNotEnabled) {
			return true, ""
		}
		return false, "the computer-use helper could not confirm the display shield"
	}
	allowed, _ := res["allowed"].(bool)
	reason, _ := res["reason"].(string)
	return allowed, reason
}

// RunAction forwards a validated-by-the-helper action and returns its result.
func (c *Controller) RunAction(req ActionRequest) (map[string]any, error) {
	payload := map[string]any{"op": "action", "action": req.Action}
	if req.X != nil {
		payload["x"] = *req.X
	}
	if req.Y != nil {
		payload["y"] = *req.Y
	}
	if req.Button != "" {
		payload["button"] = req.Button
	}
	if req.Count != 0 {
		payload["count"] = req.Count
	}
	if req.Text != "" {
		payload["text"] = req.Text
	}
	if len(req.Keys) > 0 {
		payload["keys"] = req.Keys
	}
	if req.DeltaX != 0 {
		payload["delta_x"] = req.DeltaX
	}
	if req.DeltaY != 0 {
		payload["delta_y"] = req.DeltaY
	}
	res, err := c.call(callTimeout, payload)
	if err != nil {
		return nil, err
	}
	delete(res, "ok")
	return res, nil
}
