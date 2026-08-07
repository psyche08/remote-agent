// Package computeruse implements the device-scoped computer-use control
// surface and its Locked Use extension.
//
// Two boundaries matter here and are kept deliberately separate:
//
//   - Actions (this file) describe what the agent may do to the desktop. The
//     vocabulary is closed: an unknown action id is rejected rather than passed
//     through to a shell or a native helper.
//   - Locked Use (locked.go, grant.go) decides whether the desktop may be
//     unlocked at all while the screen is locked. It never handles the user's
//     password; it only mints short-lived signed grants that a separately
//     installed Apple Authorization Plug-in verifies during the unlock flow.
package computeruse

import (
	"errors"
	"fmt"
	"strings"
)

// ActionID is the closed set of desktop operations. Anything outside this list
// is refused at the API boundary, so a compromised or confused caller cannot
// reach an arbitrary command through the computer-use route.
type ActionID string

const (
	ActionScreenshot ActionID = "screen.capture"
	ActionMove       ActionID = "pointer.move"
	ActionClick      ActionID = "pointer.click"
	ActionScroll     ActionID = "pointer.scroll"
	ActionType       ActionID = "keyboard.type"
	ActionKey        ActionID = "keyboard.key"
)

// MouseButton is the closed set of pointer buttons.
type MouseButton string

const (
	ButtonLeft   MouseButton = "left"
	ButtonRight  MouseButton = "right"
	ButtonMiddle MouseButton = "middle"
)

const (
	// MaxTypeRunes bounds a single type action. Long text is the caller's job
	// to chunk; an unbounded synthetic keystroke stream is a denial-of-service
	// against the very desktop the agent is driving.
	MaxTypeRunes = 4096
	// MaxKeyChordKeys bounds a single key chord (e.g. cmd+shift+4).
	MaxKeyChordKeys = 5
	// MaxClickCount bounds a multi-click (single, double, triple).
	MaxClickCount = 3
	// MaxScrollMagnitude bounds one scroll action's wheel delta.
	MaxScrollMagnitude = 4096
)

// Action is one validated desktop operation. It is produced only by
// ParseAction, so every field a native helper consumes has already been
// range-checked.
type Action struct {
	ID     ActionID    `json:"id"`
	X      int         `json:"x,omitempty"`
	Y      int         `json:"y,omitempty"`
	Button MouseButton `json:"button,omitempty"`
	Count  int         `json:"count,omitempty"`
	Text   string      `json:"text,omitempty"`
	Keys   []string    `json:"keys,omitempty"`
	DeltaX int         `json:"delta_x,omitempty"`
	DeltaY int         `json:"delta_y,omitempty"`
}

// ActionRequest is the raw wire shape accepted by the API. It is intentionally
// distinct from Action: the API never hands an unvalidated struct to the
// system layer.
type ActionRequest struct {
	Action string   `json:"action"`
	X      *int     `json:"x"`
	Y      *int     `json:"y"`
	Button string   `json:"button"`
	Count  int      `json:"count"`
	Text   string   `json:"text"`
	Keys   []string `json:"keys"`
	DeltaX int      `json:"delta_x"`
	DeltaY int      `json:"delta_y"`
}

// keyNames is the closed set of non-character key names a chord may use.
// Character keys (a-z, 0-9, punctuation) are handled as single-rune names.
var keyNames = map[string]bool{
	"cmd": true, "command": true, "ctrl": true, "control": true,
	"alt": true, "option": true, "shift": true, "fn": true,
	"return": true, "enter": true, "tab": true, "space": true,
	"delete": true, "backspace": true, "escape": true, "esc": true,
	"up": true, "down": true, "left": true, "right": true,
	"home": true, "end": true, "pageup": true, "pagedown": true,
	"f1": true, "f2": true, "f3": true, "f4": true, "f5": true, "f6": true,
	"f7": true, "f8": true, "f9": true, "f10": true, "f11": true, "f12": true,
}

var errUnknownAction = errors.New("unknown computer-use action")

// ParseAction validates a wire request into an Action. It is the only way to
// construct an Action that the system layer will execute.
func ParseAction(in ActionRequest) (Action, error) {
	id := ActionID(strings.TrimSpace(in.Action))
	switch id {
	case ActionScreenshot:
		return Action{ID: id}, nil

	case ActionMove:
		x, y, err := requirePoint(in)
		if err != nil {
			return Action{}, err
		}
		return Action{ID: id, X: x, Y: y}, nil

	case ActionClick:
		x, y, err := requirePoint(in)
		if err != nil {
			return Action{}, err
		}
		button, err := parseButton(in.Button)
		if err != nil {
			return Action{}, err
		}
		count := in.Count
		if count == 0 {
			count = 1
		}
		if count < 1 || count > MaxClickCount {
			return Action{}, fmt.Errorf("click count must be 1..%d", MaxClickCount)
		}
		return Action{ID: id, X: x, Y: y, Button: button, Count: count}, nil

	case ActionScroll:
		x, y, err := requirePoint(in)
		if err != nil {
			return Action{}, err
		}
		if in.DeltaX == 0 && in.DeltaY == 0 {
			return Action{}, errors.New("scroll requires a non-zero delta_x or delta_y")
		}
		if abs(in.DeltaX) > MaxScrollMagnitude || abs(in.DeltaY) > MaxScrollMagnitude {
			return Action{}, fmt.Errorf("scroll delta must be within +/-%d", MaxScrollMagnitude)
		}
		return Action{ID: id, X: x, Y: y, DeltaX: in.DeltaX, DeltaY: in.DeltaY}, nil

	case ActionType:
		if in.Text == "" {
			return Action{}, errors.New("type requires text")
		}
		if n := len([]rune(in.Text)); n > MaxTypeRunes {
			return Action{}, fmt.Errorf("type text exceeds %d characters", MaxTypeRunes)
		}
		if strings.ContainsRune(in.Text, 0) {
			return Action{}, errors.New("type text contains a NUL byte")
		}
		return Action{ID: id, Text: in.Text}, nil

	case ActionKey:
		keys, err := parseKeys(in.Keys)
		if err != nil {
			return Action{}, err
		}
		return Action{ID: id, Keys: keys}, nil

	default:
		return Action{}, errUnknownAction
	}
}

func requirePoint(in ActionRequest) (int, int, error) {
	if in.X == nil || in.Y == nil {
		return 0, 0, errors.New("action requires x and y")
	}
	// Screen bounds are enforced by the native helper against the real display
	// geometry. Reject only absurd values here so a typo cannot become a
	// pathological synthetic event.
	if *in.X < -32768 || *in.X > 32767 || *in.Y < -32768 || *in.Y > 32767 {
		return 0, 0, errors.New("coordinates out of range")
	}
	return *in.X, *in.Y, nil
}

func parseButton(name string) (MouseButton, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "left":
		return ButtonLeft, nil
	case "right":
		return ButtonRight, nil
	case "middle":
		return ButtonMiddle, nil
	default:
		return "", errors.New("unknown mouse button: " + name)
	}
}

func parseKeys(in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, errors.New("key action requires keys")
	}
	if len(in) > MaxKeyChordKeys {
		return nil, fmt.Errorf("key chord exceeds %d keys", MaxKeyChordKeys)
	}
	out := make([]string, 0, len(in))
	for _, raw := range in {
		key := strings.ToLower(strings.TrimSpace(raw))
		if key == "" {
			return nil, errors.New("key chord contains an empty key")
		}
		if !keyNames[key] && len([]rune(key)) != 1 {
			return nil, errors.New("unknown key: " + raw)
		}
		out = append(out, key)
	}
	return out, nil
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
