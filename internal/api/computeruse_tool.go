package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/psyche08/remote-agent/internal/computeruse"
	"github.com/psyche08/remote-agent/internal/provider"
)

const maxComputerUseImageBytes = 25 << 20

var pngSignature = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}

// installComputerUseToolHandler binds the provider identity outside the model
// tool arguments. The provider also derives this identity from its protocol,
// but checking it again here keeps the desktop authority boundary independent
// of provider parsing bugs.
func (s *Server) installComputerUseToolHandler(providerID string, host provider.ComputerUseToolHost) {
	host.SetComputerUseToolHandler(func(
		ctx context.Context, req provider.ComputerUseToolRequest,
	) (provider.ComputerUseToolResult, error) {
		if req.ProviderID != "" && !sameProviderID(req.ProviderID, providerID) {
			return provider.ComputerUseToolResult{}, errors.New("computer-use tool provider identity mismatch")
		}
		req.ProviderID = providerID
		return s.handleComputerUseTool(ctx, req)
	})
}

func (s *Server) handleComputerUseTool(
	ctx context.Context, req provider.ComputerUseToolRequest,
) (provider.ComputerUseToolResult, error) {
	if err := ctx.Err(); err != nil {
		return provider.ComputerUseToolResult{}, err
	}
	ctl := s.computerUseCtl
	if ctl == nil {
		return provider.ComputerUseToolResult{}, computeruse.ErrNotEnabled
	}
	target, err := s.computerUseToolTarget(req.ProviderID, req.SessionID, req.ThreadID)
	if err != nil {
		return provider.ComputerUseToolResult{}, err
	}
	ref, err := s.computerUseLeaseReference(req.ProviderID, target, req.TurnID)
	if err != nil {
		return provider.ComputerUseToolResult{}, err
	}
	lease, release, err := s.acquireComputerUseLease(ref)
	if err != nil {
		return provider.ComputerUseToolResult{}, err
	}
	defer release()

	tool := strings.TrimSpace(req.Tool)
	switch tool {
	case "prepare_app":
		return s.computerUsePrepareApp(ctl, lease)
	case "get_app_state":
		return s.computerUseGetAppState(ctl, lease, req)
	case "press", "focus":
		if err := ctl.CheckTurnOwner(lease.OwnerID); err != nil {
			return provider.ComputerUseToolResult{}, err
		}
		op := map[string]string{
			"press": "ax_press",
			"focus": "ax_focus",
		}[tool]
		result, err := ctl.RunAX(computeruse.AXRequest{
			TurnID: lease.OwnerID, Op: op, App: req.App,
			BundleID: req.BundleID, Path: req.Path,
		})
		return computerUseToolJSONResult(tool, result, err)
	case "set_value":
		if err := ctl.CheckTurnOwner(lease.OwnerID); err != nil {
			return provider.ComputerUseToolResult{}, err
		}
		result, err := ctl.RunAX(computeruse.AXRequest{
			TurnID: lease.OwnerID, Op: "ax_setvalue", App: req.App,
			BundleID: req.BundleID, Path: req.Path, Value: req.Value,
		})
		return computerUseToolJSONResult(tool, result, err)
	case "click", "type_text", "press_key", "scroll":
		if err := ctl.CheckTurnOwner(lease.OwnerID); err != nil {
			return provider.ComputerUseToolResult{}, err
		}
		action := map[string]string{
			"click": "pointer.click", "type_text": "keyboard.type",
			"press_key": "keyboard.key", "scroll": "pointer.scroll",
		}[tool]
		coordinateSpace := ""
		if tool == "click" || tool == "scroll" {
			// Model coordinates refer to the top-left origin of the composite
			// image returned by get_app_state. Legacy HTTP actions intentionally
			// retain their historical Core Graphics global-coordinate contract.
			coordinateSpace = "screenshot"
		}
		result, err := ctl.RunAction(computeruse.ActionRequest{
			TurnID: lease.OwnerID, Action: action, CoordinateSpace: coordinateSpace,
			X: req.X, Y: req.Y, Button: req.Button, Count: req.Count,
			Text: req.Text, Keys: req.Keys, DeltaX: req.DeltaX, DeltaY: req.DeltaY,
		})
		return computerUseToolJSONResult(tool, result, err)
	default:
		return provider.ComputerUseToolResult{}, fmt.Errorf("unknown computer-use tool %q", tool)
	}
}

// computerUsePrepareApp acquires the operation's safe desktop window without
// reading or launching a target application. Desktop-backed providers use it
// before process launch so an app cannot observe protected Keychain state while
// the console is still locked. The lease cleanup remains responsible for the
// synchronous close/relock on every return path.
func (s *Server) computerUsePrepareApp(
	ctl *computeruse.Controller, lease computerUseTurnLease,
) (provider.ComputerUseToolResult, error) {
	window, err := ctl.OpenWindow(lease.OwnerID)
	mode := "locked_use"
	if errors.Is(err, computeruse.ErrLockedUseNotEnabled) {
		mode = "normal_unlocked"
		window.Phase = computeruse.WindowPhaseClosed
		err = nil
	}
	if err != nil {
		return provider.ComputerUseToolResult{}, err
	}
	payload, err := json.Marshal(map[string]any{
		"ok": true, "tool": "prepare_app",
		"window": map[string]any{
			"registered": window.Registered, "phase": window.Phase,
			"open": window.Open, "closing": window.Closing, "mode": mode,
		},
	})
	if err != nil {
		return provider.ComputerUseToolResult{}, err
	}
	return provider.ComputerUseToolResult{Text: string(payload)}, nil
}

func (s *Server) computerUseToolTarget(providerID, sessionID, threadID string) (string, error) {
	sessionID = strings.TrimSpace(sessionID)
	threadID = strings.TrimSpace(threadID)
	if sessionID == "" && threadID == "" {
		return "", errors.New("computer-use tool has no session identity")
	}
	if sessionID == "" {
		return threadID, nil
	}
	if threadID == "" {
		return sessionID, nil
	}
	if rejectUnsafeSessionID(sessionID) != nil || rejectUnsafeSessionID(threadID) != nil {
		return "", errors.New("computer-use tool has an invalid session identity")
	}
	if s.computerUseLeaseTarget(providerID, sessionID) != s.computerUseLeaseTarget(providerID, threadID) {
		return "", errors.New("computer-use tool session and thread identities do not match")
	}
	return sessionID, nil
}

func (s *Server) computerUseGetAppState(
	ctl *computeruse.Controller, lease computerUseTurnLease, req provider.ComputerUseToolRequest,
) (provider.ComputerUseToolResult, error) {
	window, err := ctl.OpenWindow(lease.OwnerID)
	windowOpened := err == nil
	normalUnlocked := errors.Is(err, computeruse.ErrLockedUseNotEnabled)
	if err != nil && !normalUnlocked {
		return provider.ComputerUseToolResult{}, err
	}
	windowMode := "locked_use"
	if normalUnlocked {
		windowMode = "normal_unlocked"
		window.Phase = computeruse.WindowPhaseClosed
	}
	failAndRelock := func(operationErr error) (provider.ComputerUseToolResult, error) {
		// ErrLockedUseNotEnabled selects the helper's ordinary-unlocked gate.
		// No unlock window was opened in that mode, so sending window_close would
		// manufacture a cleanup debt and obscure the real operation error.
		if !windowOpened {
			return provider.ComputerUseToolResult{}, operationErr
		}
		if _, closeErr := ctl.CloseWindowForTurn(lease.OwnerID, "get_app_state failed"); closeErr != nil {
			return provider.ComputerUseToolResult{}, errors.Join(
				operationErr, fmt.Errorf("get_app_state could not confirm relock: %w", closeErr),
			)
		}
		return provider.ComputerUseToolResult{}, operationErr
	}
	capture, err := ctl.RunAction(computeruse.ActionRequest{
		TurnID: lease.OwnerID, Action: "screen.capture",
	})
	if err != nil {
		return failAndRelock(err)
	}
	imageURL, imageBytes, err := computerUsePNGDataURL(capture, maxComputerUseImageBytes)
	if err != nil {
		return failAndRelock(err)
	}
	accessibility, err := ctl.RunAX(computeruse.AXRequest{
		TurnID: lease.OwnerID, Op: "ax_read", App: req.App, BundleID: req.BundleID,
	})
	if err != nil {
		return failAndRelock(err)
	}
	payload := map[string]any{
		"ok": true, "tool": "get_app_state",
		"window": map[string]any{
			"registered": window.Registered, "phase": window.Phase,
			"open": window.Open, "closing": window.Closing, "mode": windowMode,
		},
		"accessibility": accessibility,
		"image":         map[string]any{"media_type": "image/png", "bytes": imageBytes},
	}
	text, err := json.Marshal(payload)
	if err != nil {
		return provider.ComputerUseToolResult{}, err
	}
	return provider.ComputerUseToolResult{Text: string(text), ImageURL: imageURL}, nil
}

func computerUseToolJSONResult(
	tool string, result map[string]any, runErr error,
) (provider.ComputerUseToolResult, error) {
	if runErr != nil {
		return provider.ComputerUseToolResult{}, runErr
	}
	if result == nil {
		result = map[string]any{}
	}
	text, err := json.Marshal(map[string]any{
		"ok": true, "tool": tool, "result": result,
	})
	if err != nil {
		return provider.ComputerUseToolResult{}, err
	}
	return provider.ComputerUseToolResult{Text: string(text)}, nil
}

func computerUsePNGDataURL(capture map[string]any, maxBytes int) (string, int, error) {
	if stringAny(capture["media_type"]) != "image/png" {
		return "", 0, errors.New("computer-use helper returned an unsupported image type")
	}
	encoded, ok := capture["image_base64"].(string)
	if !ok || encoded == "" {
		return "", 0, errors.New("computer-use helper returned no in-memory image")
	}
	if maxBytes <= 0 || len(encoded) > base64.StdEncoding.EncodedLen(maxBytes+1) {
		return "", 0, errors.New("computer-use helper image is too large")
	}
	data, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil {
		return "", 0, errors.New("computer-use helper returned invalid image data")
	}
	if len(data) > maxBytes {
		return "", 0, errors.New("computer-use helper image is too large")
	}
	if !bytes.HasPrefix(data, pngSignature) {
		return "", 0, errors.New("computer-use helper image is not a PNG")
	}
	encoded = base64.StdEncoding.EncodeToString(data)
	return "data:image/png;base64," + encoded, len(data), nil
}
