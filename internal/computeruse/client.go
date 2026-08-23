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
	"context"
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
	// restartTimeout covers cancellation of an opening authorization boundary
	// plus verified relock. It is a startup-only safety barrier, not an HTTP
	// request path.
	restartTimeout = 55 * time.Second
)

// NewController returns a handle on the helper at socketPath.
func NewController(socketPath string, enabled, lockedUseEnabled bool) *Controller {
	return &Controller{
		socketPath: socketPath, enabled: enabled, lockedUseEnabled: lockedUseEnabled,
	}
}

// Start and Stop exist for symmetry with the server's lifecycle. The helper is
// a separate long-lived process, but Stop still asks it to unwind any open
// window before this controller disconnects.
func (c *Controller) Start() {}

func (c *Controller) Stop() {
	// The helper outlives the Go process. If this controller opened a window,
	// merely closing the transport would leave the helper to wait for its TTL
	// before relocking. Reconcile the current owner and ask the helper to close
	// synchronously first. This is best-effort because shutdown must still be
	// able to finish when the helper has already gone away.
	if c.lockedUseEnabled {
		if state, err := c.WindowState(); err == nil && state.Registered && state.TurnID != "" {
			_, _ = c.CloseWindowForTurn(state.TurnID, "AgentHalo shutdown")
		}
	}
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
		return e.Code == "no_window"
	case ErrWindowBusy:
		return e.Code == "window_busy"
	case ErrTurnNotActive:
		return e.Code == "turn_not_active"
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
	// for the requested turn.
	ErrNoWindow = errors.New("no open locked-use window for this turn")
	// ErrWindowBusy means a different turn owns the open (or closing) window.
	ErrWindowBusy = errors.New("another turn owns the locked-use window")
	// ErrTurnNotActive means the requested turn was not established by a live
	// provider stream, or has already reached a terminal state.
	ErrTurnNotActive = errors.New("the provider turn is not active")
	// ErrUnsupported means the device cannot provide the capability at all.
	ErrUnsupported = errors.New("computer use is not supported on this platform")
	// ErrBadRequest means the helper refused the request as malformed.
	ErrBadRequest = errors.New("invalid computer-use request")
	// ErrHelperUnavailable means the helper is not reachable. It is deliberately
	// not folded into ErrNotEnabled: a device where the helper died has computer
	// use configured on and broken, and reporting that as "off" would hide it.
	ErrHelperUnavailable = errors.New("the computer-use helper is not running")
)

// ActionRequest is the wire shape accepted by the API. Provider/session fields
// establish the server-side lease and are never forwarded; the server replaces
// TurnID with its scoped trusted owner before this controller sees the request.
// Action vocabulary validation happens in the helper, which is the only
// process that can act on it.
type ActionRequest struct {
	ProviderID      string   `json:"provider_id"`
	SessionID       string   `json:"session_id"`
	TurnID          string   `json:"turn_id"`
	Action          string   `json:"action"`
	CoordinateSpace string   `json:"coordinate_space,omitempty"`
	X               *int     `json:"x,omitempty"`
	Y               *int     `json:"y,omitempty"`
	Button          string   `json:"button,omitempty"`
	Count           int      `json:"count,omitempty"`
	Text            string   `json:"text,omitempty"`
	Keys            []string `json:"keys,omitempty"`
	DeltaX          int      `json:"delta_x,omitempty"`
	DeltaY          int      `json:"delta_y,omitempty"`
}

// WindowState is the helper's authoritative view of the per-turn unlock
// window. Closing is separate from Open because the helper keeps a reservation
// until it has verified the relock; callers must not mistake that interval for
// a free desktop.
type WindowState struct {
	Registered bool
	Phase      string
	Open       bool
	TurnID     string
	Closing    bool
}

const (
	WindowPhaseClosed  = "closed"
	WindowPhaseOpening = "opening"
	WindowPhaseOpen    = "open"
	WindowPhaseClosing = "closing"
)

func windowStateFrom(res map[string]any) (WindowState, error) {
	open, openOK := res["window_open"].(bool)
	turn, turnOK := res["window_turn_id"].(string)
	closing, closingOK := res["window_closing"].(bool)
	if !openOK || !turnOK || !closingOK {
		return WindowState{}, errors.New("the computer-use helper returned an incomplete window state")
	}

	phase, phasePresent := res["window_phase"].(string)
	if _, exists := res["window_phase"]; exists && !phasePresent {
		return WindowState{}, errors.New("the computer-use helper returned an invalid window phase")
	}
	if !phasePresent {
		// Compatibility with helpers predating window_phase. Treat a retained
		// owner with neither legacy boolean set as opening, which is the only
		// safe interpretation for admission and shutdown.
		switch {
		case open:
			phase = WindowPhaseOpen
		case closing:
			phase = WindowPhaseClosing
		case turn != "":
			phase = WindowPhaseOpening
		default:
			phase = WindowPhaseClosed
		}
	}
	registered := phase != WindowPhaseClosed
	if raw, exists := res["window_registered"]; exists {
		value, ok := raw.(bool)
		if !ok || value != registered {
			return WindowState{}, errors.New("the computer-use helper returned an inconsistent window registration")
		}
	}
	switch phase {
	case WindowPhaseClosed:
		if open || closing || (phasePresent && turn != "") {
			return WindowState{}, errors.New("the computer-use helper returned an inconsistent closed window")
		}
	case WindowPhaseOpening:
		if open || closing || turn == "" {
			return WindowState{}, errors.New("the computer-use helper returned an inconsistent opening window")
		}
	case WindowPhaseOpen:
		if !open || closing || turn == "" {
			return WindowState{}, errors.New("the computer-use helper returned an inconsistent open window")
		}
	case WindowPhaseClosing:
		// A legacy response could not retain the closing owner. New phase-aware
		// helpers must, so Stop can wait for that exact window's cleanup.
		if open || !closing || (phasePresent && turn == "") {
			return WindowState{}, errors.New("the computer-use helper returned an inconsistent closing window")
		}
	default:
		return WindowState{}, errors.New("the computer-use helper returned an unknown window phase")
	}
	return WindowState{
		Registered: registered, Phase: phase, Open: open, TurnID: turn, Closing: closing,
	}, nil
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
	return c.StatusContext(context.Background())
}

// StatusContext probes the helper on a dedicated read-only connection. A
// prompt or unlock operation can legitimately hold the controller's shared
// request mutex for up to callTimeout; status presentation must not queue
// behind that mutation or outlive the caller's deadline. The helper already
// isolates connections, and this probe carries no grant or mutation authority.
func (c *Controller) StatusContext(ctx context.Context) map[string]any {
	res, err := c.statusContext(ctx)
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

func (c *Controller) statusContext(ctx context.Context) (map[string]any, error) {
	if ctx == nil {
		return nil, errors.New("computer-use status requires a context")
	}
	callCtx, cancel := context.WithTimeout(ctx, quickTimeout)
	defer cancel()
	if err := callCtx.Err(); err != nil {
		return nil, err
	}
	dialer := net.Dialer{Timeout: dialTimeout}
	conn, err := dialer.DialContext(callCtx, "unix", c.socketPath)
	if err != nil {
		if callCtx.Err() != nil {
			return nil, callCtx.Err()
		}
		return nil, fmt.Errorf("%w: %v", ErrHelperUnavailable, err)
	}
	defer conn.Close()
	if deadline, ok := callCtx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return nil, err
		}
	}
	stopCancellation := context.AfterFunc(callCtx, func() {
		_ = conn.SetDeadline(time.Now())
	})
	defer stopCancellation()
	if _, err := conn.Write([]byte("{\"op\":\"status\"}\n")); err != nil {
		if callCtx.Err() != nil {
			return nil, callCtx.Err()
		}
		return nil, err
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		if callCtx.Err() != nil {
			return nil, callCtx.Err()
		}
		return nil, err
	}
	var res map[string]any
	if err := json.Unmarshal(line, &res); err != nil {
		return nil, errors.New("the computer-use helper returned malformed output")
	}
	if ok, _ := res["ok"].(bool); !ok {
		detail, _ := res["error"].(string)
		code, _ := res["code"].(string)
		if code == "" {
			code = "failed"
		}
		return nil, &Error{Code: code, Detail: detail}
	}
	if err := callCtx.Err(); err != nil {
		return nil, err
	}
	return res, nil
}

// SetLockedUseActive is the console's runtime toggle. It can only move within
// what the device's own config already permits; the helper enforces that.
func (c *Controller) SetLockedUseActive(active bool) error {
	_, err := c.call(quickTimeout, map[string]any{
		"op": "locked_use_active", "active": active,
	})
	return err
}

// PrepareForRestart asks the helper to atomically stop admitting work, wait for
// every opening/windowed/unwindowed operation, withdraw grants, and confirm a
// locked baseline. A separate status -> close -> status sequence is not safe:
// another signed agent could begin opening after the final query and before
// launchd kills the process. Helpers predating prepare_restart fail closed and
// require a deliberate/manual upgrade.
func (c *Controller) PrepareForRestart() error {
	defer func() {
		c.mu.Lock()
		c.closeLocked()
		c.mu.Unlock()
	}()
	res, err := c.call(restartTimeout, map[string]any{"op": "prepare_restart"})
	if err != nil {
		return fmt.Errorf("desktop helper restart preflight failed: %w", err)
	}
	safe, ok := res["safe_to_restart"].(bool)
	if !ok || !safe {
		return errors.New("desktop helper did not confirm safe_to_restart")
	}
	return nil
}

// OpenWindow opens a per-turn unlock window and returns the state from that
// same operation. A second status query could fail after the unlock succeeded
// and falsely report the window as closed.
func (c *Controller) OpenWindow(turnID string) (WindowState, error) {
	res, err := c.call(callTimeout, map[string]any{
		"op": "window_open", "turn_id": turnID,
	})
	if err != nil {
		return WindowState{}, c.reconcileUnconfirmedOpen(turnID, err)
	}
	state, err := windowStateFrom(res)
	if err != nil {
		return state, c.reconcileUnconfirmedOpen(turnID, err)
	}
	if !state.Registered || state.Phase != WindowPhaseOpen ||
		!state.Open || state.TurnID != turnID || state.Closing {
		err := errors.New("the computer-use helper did not confirm the requested window owner")
		return state, c.reconcileUnconfirmedOpen(turnID, err)
	}
	return state, nil
}

// reconcileUnconfirmedOpen removes authority when an open request may have
// taken effect but its response did not prove the resulting owner. The close
// is scoped to the same turn, so it cannot relock a newer/different owner's
// window. A well-formed helper refusal is authoritative and an initial dial
// failure delivered no request, so neither creates cleanup work.
func (c *Controller) reconcileUnconfirmedOpen(turnID string, openErr error) error {
	var refusal *Error
	if errors.As(openErr, &refusal) || errors.Is(openErr, ErrHelperUnavailable) {
		return openErr
	}
	_, closeErr := c.CloseWindowForTurn(turnID, "window open was not confirmed")
	if closeErr == nil || errors.Is(closeErr, ErrNoWindow) || errors.Is(closeErr, ErrWindowBusy) {
		return openErr
	}
	// Keep the open failure as the error identity. A cleanup refusal is useful
	// diagnostic context, but must not turn an uncertain-open protocol failure
	// into (for example) a harmless-looking not_enabled API response.
	return fmt.Errorf(
		"%w; the computer-use helper could not confirm cleanup after window open: %v",
		openErr, closeErr,
	)
}

// CloseWindowForTurn ends the window only if the named turn owns it, so a
// finishing turn cannot relock a window another turn legitimately opened.
func (c *Controller) CloseWindowForTurn(turnID string, reason string) (WindowState, error) {
	res, err := c.call(callTimeout, map[string]any{
		"op": "window_close", "turn_id": turnID, "reason": reason,
	})
	if err != nil {
		return WindowState{}, err
	}
	state, err := windowStateFrom(res)
	if err != nil {
		return WindowState{}, err
	}
	if state.Registered && state.TurnID != turnID {
		return state, &Error{
			Code: "window_busy", Detail: "another turn owns the locked-use window",
		}
	}
	if state.Registered || state.Phase != WindowPhaseClosed ||
		state.Open || state.Closing || state.TurnID != "" {
		return state, errors.New("the computer-use helper did not confirm the relock")
	}
	return state, nil
}

// WindowState reports the authoritative window state. Transport and protocol
// errors are preserved; an unknown state must never be collapsed into closed.
func (c *Controller) WindowState() (WindowState, error) {
	res, err := c.call(quickTimeout, map[string]any{"op": "window_state"})
	if err != nil {
		return WindowState{}, err
	}
	return windowStateFrom(res)
}

// CheckTurnOwner prevents one remote turn from riding another turn's open
// unlock window. With no window open, ordinary unlocked computer use and the
// lock-preserving AX channel remain available; the helper still validates the
// action itself.
func (c *Controller) CheckTurnOwner(turnID string) error {
	state, err := c.WindowState()
	if err != nil {
		return err
	}
	if state.Registered && (state.Phase != WindowPhaseOpen || state.TurnID != turnID) {
		return &Error{
			Code: "window_busy", Detail: "another turn owns the locked-use window",
		}
	}
	return nil
}

// CaptureAllowed reports the helper's legacy-capture gate. The API permanently
// disables its on-disk screenshot/OCR routes whenever Locked Use is configured;
// this remains a defense-in-depth query for other callers and older flows.
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
	payload := map[string]any{
		"op": "action", "turn_id": req.TurnID, "action": req.Action,
	}
	if req.CoordinateSpace != "" {
		payload["coordinate_space"] = req.CoordinateSpace
	}
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

// AXRequest is the wire shape for an Accessibility operation. In Locked Use it
// runs only after the helper's temporary unlock has completed behind the
// display shield and input guard. Validation lives in the helper, which is the
// only process that can act on it.
type AXRequest struct {
	ProviderID string  `json:"provider_id"`
	SessionID  string  `json:"session_id"`
	TurnID     string  `json:"turn_id"`
	Op         string  `json:"op"`
	App        string  `json:"app"`
	BundleID   string  `json:"bundle_id"`
	Path       []int   `json:"path"`
	Value      *string `json:"value"`
}

// MaxAXPathDepth mirrors the helper's bounded Accessibility traversal. Keeping
// invalid paths away from the Swift array subscript is also important because a
// negative index would otherwise trap the resident helper process.
const MaxAXPathDepth = 40

func validateAXPath(path []int) error {
	if len(path) > MaxAXPathDepth {
		return &Error{Code: "bad_request", Detail: "accessibility path is too deep"}
	}
	for _, index := range path {
		if index < 0 {
			return &Error{Code: "bad_request", Detail: "accessibility path contains a negative index"}
		}
	}
	return nil
}

// RunAX forwards an Accessibility operation. op is one of ax_read, ax_press,
// ax_setvalue, or the provider-internal ax_focus; the helper refuses anything
// else. ax_focus is intentionally not advertised as a model-facing tool.
func (c *Controller) RunAX(req AXRequest) (map[string]any, error) {
	if err := validateAXPath(req.Path); err != nil {
		return nil, err
	}
	payload := map[string]any{"op": req.Op, "turn_id": req.TurnID}
	if req.App != "" {
		payload["app"] = req.App
	}
	if req.BundleID != "" {
		payload["bundle_id"] = req.BundleID
	}
	if req.Path != nil {
		payload["path"] = req.Path
	}
	if req.Value != nil {
		payload["value"] = *req.Value
	}
	res, err := c.call(callTimeout, payload)
	if err != nil {
		return nil, err
	}
	delete(res, "ok")
	return res, nil
}
