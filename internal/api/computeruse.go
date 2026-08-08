package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/psyche08/remote-agent/internal/computeruse"
)

// The computer-use routes are device-scoped, not provider- or session-scoped:
// there is one desktop per Mac, and driving it is a property of the device.
// They are still gated the same way as everything else here — the agent binds
// a 0700 Unix socket behind the relay's mTLS.
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

type computerUseWindowIn struct {
	TurnID string `json:"turn_id"`
	Action string `json:"action"`
	Reason string `json:"reason"`
}

// computerUseWindow opens or closes a Locked Use unlock window.
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
	turnID := strings.TrimSpace(body.TurnID)
	if turnID == "" {
		writeError(w, http.StatusBadRequest, "turn_id is required")
		return
	}
	// turn_id names a window and is echoed into the audit ring, so it gets the
	// same shape check as every other externally supplied identifier here.
	if err := rejectUnsafeSessionID(turnID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid turn_id")
		return
	}
	// turn_id is signed into the grant, and the plugin refuses a grant larger
	// than a few KB. Bound it here so a long id cannot make every grant
	// unverifiable instead of failing one obvious request.
	if len(turnID) > maxTurnIDLen {
		writeError(w, http.StatusBadRequest, "turn_id is too long")
		return
	}
	switch strings.ToLower(strings.TrimSpace(body.Action)) {
	case "open":
		if err := ctl.OpenWindow(turnID); err != nil {
			writeComputerUseError(w, err)
			return
		}
	case "close":
		reason := strings.TrimSpace(body.Reason)
		if reason == "" {
			reason = "turn ended"
		}
		owner, open := ctl.WindowOpen()
		if open && owner != turnID {
			// Another turn owns the window. Answering ok would tell this caller
			// the desktop is relocked when it is not, and echoing the owner
			// back would disclose it.
			writeJSON(w, http.StatusConflict, map[string]any{
				"ok": false, "status": "refused", "turn_id": turnID,
				"detail": "this turn does not own the locked-use window",
			})
			return
		}
		ctl.CloseWindowForTurn(turnID, reason)
	default:
		writeError(w, http.StatusBadRequest, `action must be "open" or "close"`)
		return
	}
	openTurn, open := ctl.WindowOpen()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "turn_id": turnID,
		"window_open": open, "window_turn_id": openTurn,
	})
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
	var body computeruse.ActionRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	// The action vocabulary is validated in the helper, which is the only
	// process that can act on it. Validating here as well would put a second
	// copy of the vocabulary in a process that cannot enforce it — the socket
	// is reachable without going through this route — and a copy that drifted
	// would be worse than none.
	result, err := ctl.RunAction(body)
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

// writeComputerUseError maps controller errors onto status codes so a client
// can distinguish "not turned on here" from "refused right now" without
// string-matching. Nothing in these messages carries grant or key material.
func writeComputerUseError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, computeruse.ErrBadRequest):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, computeruse.ErrHelperUnavailable):
		// A device whose helper died has computer use configured on and broken.
		// Reporting that as "not enabled" would hide a fault behind a setting.
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"ok": false, "status": "unavailable", "detail": err.Error(),
		})
	case errors.Is(err, computeruse.ErrNotEnabled),
		errors.Is(err, computeruse.ErrLockedUseNotEnabled),
		errors.Is(err, computeruse.ErrUnsupported):
		writeJSON(w, http.StatusConflict, map[string]any{
			"ok": false, "status": "not_enabled", "detail": err.Error(),
		})
	case errors.Is(err, computeruse.ErrNotArmed),
		errors.Is(err, computeruse.ErrShieldRequired),
		errors.Is(err, computeruse.ErrLocalInput),
		errors.Is(err, computeruse.ErrNoWindow):
		writeJSON(w, http.StatusConflict, map[string]any{
			"ok": false, "status": "refused", "detail": err.Error(),
		})
	default:
		// A real failure answers 5xx. Returning 200 here would let a client that
		// checks the status code — rather than the body's ok field — treat a
		// failed unlock as a successful one.
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"ok": false, "status": "error", "detail": err.Error(),
		})
	}
}

// captureGate refuses screen capture while a Locked Use window is open without
// a confirmed display shield.
//
// This matters because screen capture is not suppressed by the unlock state:
// with the shield down for even a moment, a capture would write whatever is on
// screen to a file that is then served over the relay and can be OCR'd to text.
// The gate fails closed — an unavailable controller means no restriction only
// because there is also no Locked Use.
func (s *Server) captureGate(w http.ResponseWriter) bool {
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
