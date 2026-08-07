//go:build darwin

package computeruse

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"time"
)

// darwinSystem drives the desktop through a small Swift worker, mirroring how
// this repo already shells out to scripts/ocr_vision.swift. Synthetic events
// and idle-time queries need CoreGraphics, which is reachable from Swift
// without adding cgo to the service binary.
type darwinSystem struct {
	script string

	mu       sync.Mutex
	shieldOn bool
	// shieldDisplays is how many displays the host reported covering when it
	// engaged. A later probe that finds more attached displays means a monitor
	// was plugged in that the shield does not cover.
	shieldDisplays int
}

// NewSystem returns the macOS system boundary. scriptPath is the resolved path
// to scripts/computer_use.swift; when empty, every operation fails closed.
func NewSystem(scriptPath string) System {
	return &darwinSystem{script: scriptPath}
}

const (
	helperTimeout      = 15 * time.Second
	helperShortTimeout = 5 * time.Second
)

// runHelper invokes the Swift worker with a JSON request on argv and parses a
// single JSON object from stdout. Requests are passed as one argument and are
// never assembled into a shell string, so no input reaches a shell.
func (d *darwinSystem) runHelper(timeout time.Duration, req map[string]any) (map[string]any, error) {
	if d.script == "" {
		return nil, errors.New("computer-use helper script is not available")
	}
	swift, err := exec.LookPath("swift")
	if err != nil {
		return nil, errors.New("swift runtime is not available for the computer-use helper")
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, swift, d.script, string(payload)).Output()
	if ctx.Err() != nil {
		return nil, fmt.Errorf("computer-use helper timed out")
	}
	if err != nil {
		// stderr is deliberately dropped: the helper's failure text is not
		// needed downstream and must never become a channel for screen or
		// credential content to reach a log.
		return nil, errors.New("computer-use helper failed")
	}
	var res map[string]any
	if err := json.Unmarshal(out, &res); err != nil {
		return nil, errors.New("computer-use helper returned malformed output")
	}
	if ok, _ := res["ok"].(bool); !ok {
		detail, _ := res["error"].(string)
		if detail == "" {
			detail = "computer-use helper reported failure"
		}
		return nil, errors.New(detail)
	}
	return res, nil
}

func (d *darwinSystem) Locked() (bool, error) {
	res, err := d.runHelper(helperShortTimeout, map[string]any{"op": "lock_state"})
	if err != nil {
		return false, err
	}
	locked, ok := res["locked"].(bool)
	if !ok {
		return false, errors.New("computer-use helper did not report lock state")
	}
	return locked, nil
}

func (d *darwinSystem) Lock() error {
	_, err := d.runHelper(helperShortTimeout, map[string]any{"op": "lock"})
	return err
}

func (d *darwinSystem) SinceLastInput() (time.Duration, error) {
	res, err := d.runHelper(helperShortTimeout, map[string]any{"op": "idle_seconds"})
	if err != nil {
		return 0, err
	}
	seconds, ok := res["idle_seconds"].(float64)
	if !ok || seconds < 0 {
		return 0, errors.New("computer-use helper did not report idle time")
	}
	return time.Duration(seconds * float64(time.Second)), nil
}

func (d *darwinSystem) Engage() error {
	res, err := d.runHelper(helperShortTimeout, map[string]any{"op": "shield_engage"})
	if err != nil {
		return err
	}
	displays, ok := res["displays"].(float64)
	if !ok || displays <= 0 {
		return errors.New("display shield reported no covered displays")
	}
	d.mu.Lock()
	d.shieldOn = true
	d.shieldDisplays = int(displays)
	d.mu.Unlock()
	return nil
}

func (d *darwinSystem) Release() error {
	_, err := d.runHelper(helperShortTimeout, map[string]any{"op": "shield_release"})
	// Clear the local flag either way. A shield believed to be up when it is
	// not would let a later window skip its own engage step.
	d.mu.Lock()
	d.shieldOn = false
	d.shieldDisplays = 0
	d.mu.Unlock()
	return err
}

// Engaged probes the device for live shield coverage rather than reporting a
// cached flag.
//
// A cached bool would only ever change when this process called Engage or
// Release, which would make the controller's "shield dropped" safeguard dead
// code: the shield host can die on its own (a crash, or a display hot-plug it
// deliberately exits on) and nothing in this process would notice.
//
// A probe that cannot answer reports false, so the caller relocks rather than
// trusting an unverifiable shield.
func (d *darwinSystem) Engaged() bool {
	res, err := d.runHelper(helperShortTimeout, map[string]any{"op": "shield_state"})
	if err != nil {
		return false
	}
	engaged, _ := res["engaged"].(bool)
	if !engaged {
		d.mu.Lock()
		d.shieldOn = false
		d.mu.Unlock()
		return false
	}
	// Coverage must span every attached display; a newly plugged-in monitor
	// that the host does not cover exposes the live desktop.
	displays, ok := res["displays"].(float64)
	if !ok || displays <= 0 {
		return false
	}
	d.mu.Lock()
	covered := d.shieldDisplays
	d.mu.Unlock()
	return covered > 0 && int(displays) <= covered
}

func (d *darwinSystem) Run(action Action) (map[string]any, error) {
	req := map[string]any{"op": "action", "action": string(action.ID)}
	switch action.ID {
	case ActionScreenshot:
	case ActionMove:
		req["x"], req["y"] = action.X, action.Y
	case ActionClick:
		req["x"], req["y"] = action.X, action.Y
		req["button"], req["count"] = string(action.Button), action.Count
	case ActionScroll:
		req["x"], req["y"] = action.X, action.Y
		req["delta_x"], req["delta_y"] = action.DeltaX, action.DeltaY
	case ActionType:
		req["text"] = action.Text
	case ActionKey:
		req["keys"] = action.Keys
	default:
		return nil, errUnknownAction
	}
	res, err := d.runHelper(helperTimeout, req)
	if err != nil {
		return nil, err
	}
	delete(res, "ok")
	return res, nil
}

// Available reports whether the Swift worker can run at all, so the status
// endpoint can explain an unavailable feature instead of failing at use time.
func (d *darwinSystem) Available() bool {
	if d.script == "" {
		return false
	}
	_, err := exec.LookPath("swift")
	return err == nil
}
