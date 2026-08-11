package api

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"

	"github.com/psyche08/remote-agent/internal/computeruse"
)

// There is one physical desktop per Mac, but authority to drive it is scoped to
// a real provider/session/turn lease established by the provider stream. The
// routes are also gated the same way as everything else here — the agent binds
// a 0700 Unix socket behind the relay's mTLS.
//
// Locked Use adds a stronger origin boundary: HTTP open/action/AX are disabled
// unless the device explicitly enables debug_http_actions. The normal path is
// the in-process provider tool handler, whose identity cannot come from model
// arguments. HTTP close and runtime deactivation remain available because they
// can only remove authority and relock the device.
//
// Locked Use is deliberately not something a client can switch on. Config on
// the device is the ceiling; POST /computer_use/locked_use can only move within
// it. A remote caller that could grant a Mac the ability to unlock itself would
// defeat the purpose of the feature.

func (s *Server) computerUse(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.computerUseCtl == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"enabled": false, "available": false,
			"detail": "computer use is not configured on this device",
		})
		return
	}
	status := s.computerUseCtl.Status()
	status["device_id"] = s.cfg.DeviceID
	writeJSON(w, http.StatusOK, status)
}

type lockedUseIn struct {
	Active *bool `json:"active"`
}

func (s *Server) computerUseLockedUse(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	ctl := s.computerUseCtl
	if ctl == nil {
		writeError(w, http.StatusConflict, "computer use is not configured on this device")
		return
	}
	var body lockedUseIn
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Active == nil {
		writeError(w, http.StatusBadRequest, "active is required")
		return
	}
	if err := ctl.SetLockedUseActive(*body.Active); err != nil {
		writeComputerUseError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "locked_use": ctl.Status()["locked_use"]})
}

// maxTurnIDLen keeps a signed grant comfortably inside the size the
// Authorization Plug-in accepts.
const maxTurnIDLen = 128

type computerUseTurnLease struct {
	ProviderID string
	Target     string
	TurnID     string
	OwnerID    string
	Active     bool
}

type computerUseLeaseRef struct {
	ProviderID string
	Target     string
	TurnID     string
	OwnerID    string
}

type computerUseTurnKey struct {
	ProviderID string
	Target     string
}

type computerUseTurnIdentity struct {
	ProviderID string
	Target     string
	TurnID     string
}

func computerUseLeaseKey(providerID, target string) computerUseTurnKey {
	return computerUseTurnKey{ProviderID: providerID, Target: target}
}

func computerUseIdentity(providerID, target, turnID string) computerUseTurnIdentity {
	return computerUseTurnIdentity{ProviderID: providerID, Target: target, TurnID: turnID}
}

func computerUseOwnerID(providerID, target, turnID string) string {
	// The helper has one flat owner namespace. Hash the full trusted lease
	// identity so equal provider turn ids in two sessions cannot ride each
	// other's window, while keeping bounded/safe data in grants and audit logs.
	sum := sha256.Sum256([]byte(providerID + "\x00" + target + "\x00" + turnID))
	return "lease-" + hex.EncodeToString(sum[:])
}

// computerUseLeaseTarget collapses a logical session and its native
// transcript aliases to the stored logical id. If the provider publishes
// before storage catches up, retaining its exact target fails closed for other
// aliases without confusing two different sessions.
func (s *Server) computerUseLeaseTarget(providerID, target string) string {
	if rec, ok, err := s.findSessionForProviderAny(providerID, target); err == nil && ok {
		if logical := recordString(rec, "session_id"); logical != "" {
			return logical
		}
	}
	return target
}

func (s *Server) computerUseLeaseReference(
	providerID, target, turnID string,
) (computerUseLeaseRef, error) {
	providerID = strings.TrimSpace(providerID)
	if providerID == "" {
		return computerUseLeaseRef{}, errors.New("provider_id is required")
	}
	if _, resolved, ok := s.getProvider(providerID); ok {
		providerID = resolved
	} else {
		return computerUseLeaseRef{}, errors.New("unknown provider_id")
	}
	target = strings.TrimSpace(target)
	if target == "" {
		return computerUseLeaseRef{}, errors.New("session_id is required")
	}
	if err := rejectUnsafeSessionID(target); err != nil {
		return computerUseLeaseRef{}, errors.New("invalid session_id")
	}
	turnID, err := validateComputerUseTurnID(turnID)
	if err != nil {
		return computerUseLeaseRef{}, err
	}
	target = s.computerUseLeaseTarget(providerID, target)
	return computerUseLeaseRef{
		ProviderID: providerID,
		Target:     target,
		TurnID:     turnID,
		OwnerID:    computerUseOwnerID(providerID, target, turnID),
	}, nil
}

// acquireComputerUseLease holds the lifecycle mutex until release is called.
// Keeping it across the helper operation prevents a completed/error frame from
// revoking and relocking the turn between validation and execution.
func (s *Server) acquireComputerUseLease(
	ref computerUseLeaseRef,
) (computerUseTurnLease, func(), error) {
	s.computerUseMu.Lock()
	lease, ok := s.computerUseTurns[computerUseLeaseKey(ref.ProviderID, ref.Target)]
	if !ok || !lease.Active || lease.TurnID != ref.TurnID || lease.OwnerID != ref.OwnerID {
		s.computerUseMu.Unlock()
		return computerUseTurnLease{}, nil, computeruse.ErrTurnNotActive
	}
	return lease, s.computerUseMu.Unlock, nil
}

func (s *Server) revokeComputerUseLeaseLocked(key computerUseTurnKey, reason string) error {
	lease, ok := s.computerUseTurns[key]
	if !ok {
		return nil
	}
	lease.Active = false
	s.computerUseTurns[key] = lease
	s.computerUseEnded[computerUseIdentity(lease.ProviderID, lease.Target, lease.TurnID)] = struct{}{}
	return s.closeComputerUseOwnerLocked(lease.OwnerID, reason)
}

func (s *Server) closeComputerUseOwnerLocked(ownerID, reason string) error {
	if s.computerUseCtl == nil {
		return nil
	}
	_, err := s.computerUseCtl.CloseWindowForTurn(ownerID, reason)
	// A different owner means this lease never had the one global window; no
	// relock is owed for it. Every other error is retained because the caller
	// cannot claim cleanup was confirmed.
	if errors.Is(err, computeruse.ErrWindowBusy) || errors.Is(err, computeruse.ErrNoWindow) {
		return nil
	}
	return err
}

// observeComputerUseProviderFrame is the only way an active lease is created.
// Request parameters can select an existing lease, never mint one.
func (s *Server) observeComputerUseProviderFrame(
	providerID, target string, frame map[string]any,
) error {
	frameType := strings.ToLower(strings.TrimSpace(stringAny(frame["type"])))
	status := strings.ToLower(strings.TrimSpace(stringAny(frame["status"])))
	started := frameType == "turn" && status == "started"
	terminal := frameType == "error" || frameType == "interrupt" || frameType == "interrupted" ||
		(frameType == "turn" && status != "started")
	if !started && !terminal {
		return nil
	}

	providerID = canonicalProviderID(providerID)
	target = s.computerUseLeaseTarget(providerID, target)
	key := computerUseLeaseKey(providerID, target)

	s.computerUseMu.Lock()
	defer s.computerUseMu.Unlock()
	if terminal {
		reason := "provider turn " + status
		if status == "" {
			reason = "provider " + frameType
		}
		if turnID, err := validateComputerUseTurnID(stringAny(frame["turn_id"])); err == nil {
			// Terminal notifications can be delayed behind a newer turn. End and
			// clean up the turn named by the provider without revoking that newer
			// lease. A missing/invalid id below deliberately revokes the current
			// lease instead: ambiguity at a security boundary must fail closed.
			s.computerUseEnded[computerUseIdentity(providerID, target, turnID)] = struct{}{}
			if lease, ok := s.computerUseTurns[key]; ok && lease.TurnID == turnID {
				lease.Active = false
				s.computerUseTurns[key] = lease
			}
			return s.closeComputerUseOwnerLocked(
				computerUseOwnerID(providerID, target, turnID), reason,
			)
		}
		return s.revokeComputerUseLeaseLocked(key, reason)
	}

	turnID, err := validateComputerUseTurnID(stringAny(frame["turn_id"]))
	if err != nil {
		// A provider start without an authoritative id must not leave a previous
		// lease usable. Claude currently emits this shape and is intentionally
		// fail-closed until it supplies a real turn id.
		return s.revokeComputerUseLeaseLocked(key, "provider turn missing turn_id")
	}
	ownerID := computerUseOwnerID(providerID, target, turnID)
	if _, ended := s.computerUseEnded[computerUseIdentity(providerID, target, turnID)]; ended {
		// Provider callbacks can fan out through aliases and can be replayed by
		// reconnect logic. Once this process observed a terminal frame for an id,
		// a later duplicate started frame cannot restore its authority.
		return nil
	}
	if old, ok := s.computerUseTurns[key]; ok {
		if !old.Active && old.TurnID == turnID {
			// A delayed/replayed started frame cannot resurrect a turn that this
			// process has already observed reaching a terminal state.
			return nil
		}
		if old.TurnID != turnID {
			if err := s.revokeComputerUseLeaseLocked(key, "provider started a new turn"); err != nil {
				return err
			}
		}
	}
	s.computerUseTurns[key] = computerUseTurnLease{
		ProviderID: providerID, Target: target, TurnID: turnID, OwnerID: ownerID, Active: true,
	}
	return nil
}

func (s *Server) terminateComputerUseTarget(providerID, target, reason string) error {
	providerID = canonicalProviderID(providerID)
	target = s.computerUseLeaseTarget(providerID, target)
	s.computerUseMu.Lock()
	defer s.computerUseMu.Unlock()
	return s.revokeComputerUseLeaseLocked(computerUseLeaseKey(providerID, target), reason)
}

type computerUseWindowIn struct {
	ProviderID string `json:"provider_id"`
	SessionID  string `json:"session_id"`
	TurnID     string `json:"turn_id"`
	Action     string `json:"action"`
	Reason     string `json:"reason"`
}

func validateComputerUseTurnID(raw string) (string, error) {
	turnID := strings.TrimSpace(raw)
	if turnID == "" {
		return "", errors.New("turn_id is required")
	}
	// turn_id names a window and is echoed into the audit ring, so it gets the
	// same shape check as every other externally supplied identifier here.
	if err := rejectUnsafeSessionID(turnID); err != nil {
		return "", errors.New("invalid turn_id")
	}
	// turn_id is signed into the grant, and the plugin refuses a grant larger
	// than a few KB. Bound it here so a long id cannot make every grant
	// unverifiable instead of failing one obvious request.
	if len(turnID) > maxTurnIDLen {
		return "", errors.New("turn_id is too long")
	}
	return turnID, nil
}

// computerUseWindow opens or closes a Locked Use unlock window.
//
// Open is a debug HTTP surface when Locked Use is configured; production model
// calls use computerUseGetAppState in-process. Close remains generally
// available because it cannot grant authority.
//
// Opening is intentionally allowed to take longer than a click: it waits for
// macOS to actually complete the unlock, bounded by the grant's own lifetime
// (single-digit seconds), which keeps the whole call well inside the relay's
// 30s HTTP timeout. The controller does not use this request's context, so a
// client disconnect can never abandon a half-open window.
func (s *Server) computerUseWindow(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	ctl := s.computerUseCtl
	if ctl == nil {
		writeError(w, http.StatusConflict, "computer use is not configured on this device")
		return
	}
	var body computerUseWindowIn
	if !decodeJSON(w, r, &body) {
		return
	}
	action := strings.ToLower(strings.TrimSpace(body.Action))
	if action == "open" && s.computerUseHTTPModelToolRequired() {
		writeComputerUseModelToolRequired(w)
		return
	}
	ref, err := s.computerUseLeaseReference(body.ProviderID, body.SessionID, body.TurnID)
	if err != nil {
		writeComputerUseInputError(w, err.Error())
		return
	}
	switch action {
	case "open":
		lease, release, err := s.acquireComputerUseLease(ref)
		if err != nil {
			writeComputerUseError(w, err)
			return
		}
		state, err := ctl.OpenWindow(lease.OwnerID)
		release()
		if err != nil {
			writeComputerUseError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "turn_id": ref.TurnID,
			"window_registered": state.Registered, "window_phase": state.Phase,
			"window_open": state.Open, "window_turn_id": ref.TurnID,
			"window_closing": state.Closing,
		})
		return
	case "close":
		reason := strings.TrimSpace(body.Reason)
		if reason == "" {
			reason = "turn ended"
		}
		// Closing is always allowed for a well-scoped owner token, even just
		// after the provider terminal frame removed the active lease. It can
		// only relock, never authorize a desktop operation.
		s.computerUseMu.Lock()
		state, err := ctl.CloseWindowForTurn(ref.OwnerID, reason)
		s.computerUseMu.Unlock()
		if err != nil {
			writeComputerUseError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "turn_id": ref.TurnID,
			"window_registered": state.Registered, "window_phase": state.Phase,
			"window_open": state.Open, "window_turn_id": "",
			"window_closing": state.Closing,
		})
		return
	default:
		writeComputerUseInputError(w, `action must be "open" or "close"`)
		return
	}
}

func (s *Server) computerUseAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	ctl := s.computerUseCtl
	if ctl == nil {
		writeError(w, http.StatusConflict, "computer use is not configured on this device")
		return
	}
	if s.computerUseHTTPModelToolRequired() {
		writeComputerUseModelToolRequired(w)
		return
	}
	var body computeruse.ActionRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	ref, err := s.computerUseLeaseReference(body.ProviderID, body.SessionID, body.TurnID)
	if err != nil {
		writeComputerUseInputError(w, err.Error())
		return
	}
	lease, release, err := s.acquireComputerUseLease(ref)
	if err != nil {
		writeComputerUseError(w, err)
		return
	}
	body.TurnID = lease.OwnerID
	if err := ctl.CheckTurnOwner(lease.OwnerID); err != nil {
		release()
		writeComputerUseError(w, err)
		return
	}
	// The action vocabulary is validated in the helper, which is the only
	// process that can act on it. Validating here as well would put a second
	// copy of the vocabulary in a process that cannot enforce it — the socket
	// is reachable without going through this route — and a copy that drifted
	// would be worse than none.
	result, err := ctl.RunAction(body)
	release()
	if err != nil {
		writeComputerUseError(w, err)
		return
	}
	if result == nil {
		result = map[string]any{}
	}
	result["ok"] = true
	writeJSON(w, http.StatusOK, result)
}

// computerUseAX drives an application through its Accessibility element tree.
//
// Locked Use reaches this channel only after the Authorization Plug-in has
// completed the temporary unlock and the helper has confirmed its display
// shield and input guard. AX itself is ordinary trusted-process automation; it
// is not an alternate way to address a still-locked login session.
func (s *Server) computerUseAX(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	ctl := s.computerUseCtl
	if ctl == nil {
		writeError(w, http.StatusConflict, "computer use is not configured on this device")
		return
	}
	if s.computerUseHTTPModelToolRequired() {
		writeComputerUseModelToolRequired(w)
		return
	}
	var body computeruse.AXRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	ref, err := s.computerUseLeaseReference(body.ProviderID, body.SessionID, body.TurnID)
	if err != nil {
		writeComputerUseInputError(w, err.Error())
		return
	}
	switch body.Op {
	case "ax_read", "ax_press", "ax_setvalue":
	default:
		writeComputerUseInputError(w, `op must be ax_read, ax_press, or ax_setvalue`)
		return
	}
	if len(body.Path) > computeruse.MaxAXPathDepth {
		writeComputerUseInputError(w, "accessibility path is too deep")
		return
	}
	for _, index := range body.Path {
		if index < 0 {
			writeComputerUseInputError(w, "accessibility path contains a negative index")
			return
		}
	}
	if body.Op == "ax_setvalue" && body.Value == nil {
		writeComputerUseInputError(w, "value is required")
		return
	}
	lease, release, err := s.acquireComputerUseLease(ref)
	if err != nil {
		writeComputerUseError(w, err)
		return
	}
	body.TurnID = lease.OwnerID
	if err := ctl.CheckTurnOwner(lease.OwnerID); err != nil {
		release()
		writeComputerUseError(w, err)
		return
	}
	result, err := ctl.RunAX(body)
	release()
	if err != nil {
		writeComputerUseError(w, err)
		return
	}
	if result == nil {
		result = map[string]any{}
	}
	result["ok"] = true
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) computerUseHTTPModelToolRequired() bool {
	return s.cfg.ComputerUse.LockedUse.Enabled && !s.cfg.ComputerUse.DebugHTTPActions
}

func writeComputerUseModelToolRequired(w http.ResponseWriter) {
	writeJSON(w, http.StatusForbidden, map[string]any{
		"ok": false, "status": "refused", "code": "model_tool_required",
		"detail": "Locked Use desktop operations require the in-process model tool",
	})
}

// writeComputerUseError maps controller errors onto status codes so a client
// can distinguish "not turned on here" from "refused right now" without
// string-matching. Nothing in these messages carries grant or key material.
func writeComputerUseError(w http.ResponseWriter, err error) {
	code := computerUseErrorCode(err)
	detail := err.Error()
	if errors.Is(err, computeruse.ErrWindowBusy) {
		// The helper's audit detail may name the owning turn. A competing turn
		// needs the stable refusal code, not another turn's identifier.
		detail = computeruse.ErrWindowBusy.Error()
	}
	switch {
	case errors.Is(err, computeruse.ErrBadRequest):
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok": false, "status": "invalid", "code": code, "detail": detail,
		})
	case errors.Is(err, computeruse.ErrHelperUnavailable):
		// A device whose helper died has computer use configured on and broken.
		// Reporting that as "not enabled" would hide a fault behind a setting.
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"ok": false, "status": "unavailable", "code": code, "detail": detail,
		})
	case errors.Is(err, computeruse.ErrNotEnabled),
		errors.Is(err, computeruse.ErrLockedUseNotEnabled),
		errors.Is(err, computeruse.ErrUnsupported):
		writeJSON(w, http.StatusConflict, map[string]any{
			"ok": false, "status": "not_enabled", "code": code, "detail": detail,
		})
	case errors.Is(err, computeruse.ErrNotArmed),
		errors.Is(err, computeruse.ErrShieldRequired),
		errors.Is(err, computeruse.ErrLocalInput),
		errors.Is(err, computeruse.ErrNoWindow),
		errors.Is(err, computeruse.ErrWindowBusy),
		errors.Is(err, computeruse.ErrTurnNotActive):
		writeJSON(w, http.StatusConflict, map[string]any{
			"ok": false, "status": "refused", "code": code, "detail": detail,
		})
	default:
		// A real failure answers 5xx. Returning 200 here would let a client that
		// checks the status code — rather than the body's ok field — treat a
		// failed unlock as a successful one.
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"ok": false, "status": "error", "code": code, "detail": detail,
		})
	}
}

func writeComputerUseInputError(w http.ResponseWriter, detail string) {
	writeJSON(w, http.StatusBadRequest, map[string]any{
		"ok": false, "status": "invalid", "code": "bad_request", "detail": detail,
	})
}

func computerUseErrorCode(err error) string {
	var helperError *computeruse.Error
	if errors.As(err, &helperError) && helperError.Code != "" {
		return helperError.Code
	}
	switch {
	case errors.Is(err, computeruse.ErrHelperUnavailable):
		return "helper_unavailable"
	case errors.Is(err, computeruse.ErrBadRequest):
		return "bad_request"
	case errors.Is(err, computeruse.ErrNotEnabled):
		return "not_enabled"
	case errors.Is(err, computeruse.ErrLockedUseNotEnabled):
		return "locked_use_not_enabled"
	case errors.Is(err, computeruse.ErrNotArmed):
		return "not_armed"
	case errors.Is(err, computeruse.ErrShieldRequired):
		return "shield_required"
	case errors.Is(err, computeruse.ErrLocalInput):
		return "local_input"
	case errors.Is(err, computeruse.ErrNoWindow):
		return "no_window"
	case errors.Is(err, computeruse.ErrWindowBusy):
		return "window_busy"
	case errors.Is(err, computeruse.ErrTurnNotActive):
		return "turn_not_active"
	case errors.Is(err, computeruse.ErrUnsupported):
		return "unsupported"
	default:
		return "failed"
	}
}

// captureGate is the legacy on-disk capture boundary. Once Locked Use is a
// device capability, screenshot/OCR may only run through the in-process model
// broker, which binds an in-memory capture to the authoritative owner turn.
// A phase query followed by an external screencapture would have a TOCTOU where
// a window opens between the two, so the legacy path stays off for the whole
// configured lifetime, not only while a window happens to be registered.
func (s *Server) captureGate(w http.ResponseWriter) bool {
	if s.cfg.ComputerUse.LockedUse.Enabled || s.cfg.ComputerUse.HelperRefreshFailed {
		writeComputerUseModelToolRequired(w)
		return false
	}
	if s.computerUseCtl == nil {
		return true
	}
	ok, reason := s.computerUseCtl.CaptureAllowed()
	if !ok {
		writeJSON(w, http.StatusConflict, map[string]any{
			"ok": false, "status": "refused", "detail": reason,
		})
		return false
	}
	return true
}
