package computeruse

import (
	"errors"
	"time"
)

// ErrUnsupported is returned by the stub system layer on non-Darwin builds and
// by any capability the current device cannot provide. Callers treat it as a
// hard failure, never as a reason to proceed with a weaker safeguard.
var ErrUnsupported = errors.New("computer use is not supported on this platform")

// ScreenLocker observes and changes the screen lock state.
//
// Note the asymmetry, which is the whole point of Locked Use: this interface
// can *lock* the screen directly, but it cannot unlock it. Unlocking only ever
// happens through the macOS unlock flow, with the Authorization Plug-in
// deciding whether a signed grant permits it. Nothing in this process holds or
// supplies the user's password.
type ScreenLocker interface {
	// Locked reports whether the screen is currently locked.
	Locked() (bool, error)
	// Lock locks the screen immediately. Used to restore the locked state when
	// a window ends for any reason, including failure paths.
	Lock() error
}

// InputMonitor reports local human activity at the physical machine. Any local
// keystroke or pointer movement during an unlock window means a person is
// present, which ends the window immediately.
type InputMonitor interface {
	// SinceLastInput returns how long the machine has been idle of local
	// keyboard and pointer input. An error must be treated as "a person may be
	// present": the controller fails closed and relocks.
	SinceLastInput() (time.Duration, error)
}

// DisplayShield covers the screen while the desktop is temporarily unlocked so
// a bystander cannot read the session or watch the agent work.
type DisplayShield interface {
	// Engage raises the shield. The controller refuses to open a window when
	// this fails and the shield is required.
	Engage() error
	// Release drops the shield. Called on every window-close path.
	Release() error
	// Engaged reports the shield's current state.
	Engaged() bool
}

// ActionRunner executes a validated Action against the real desktop.
type ActionRunner interface {
	// Run performs the action. Result is action-specific (e.g. a screenshot
	// path); it is never used to carry secrets.
	Run(action Action) (map[string]any, error)
}

// System is the full device boundary the controller depends on. Tests supply a
// fake; Darwin supplies the real implementation; other platforms supply a stub
// that reports ErrUnsupported for everything.
type System interface {
	ScreenLocker
	InputMonitor
	DisplayShield
	ActionRunner
}

// SystemAvailability is an optional System capability. A System that does not
// implement it is assumed available, and any real problem surfaces as a failed
// operation rather than a silently degraded safeguard.
type SystemAvailability interface {
	Available() bool
}

// systemAvailable reports whether a System can be used at all.
func systemAvailable(sys System) bool {
	if sys == nil {
		return false
	}
	if a, ok := sys.(SystemAvailability); ok {
		return a.Available()
	}
	return true
}

// unsupportedSystem is the non-Darwin implementation. Every method fails, so a
// misconfigured non-Mac deployment can never believe it opened a window.
type unsupportedSystem struct{}

func (unsupportedSystem) Available() bool { return false }

func (unsupportedSystem) Locked() (bool, error)                  { return false, ErrUnsupported }
func (unsupportedSystem) Lock() error                            { return ErrUnsupported }
func (unsupportedSystem) SinceLastInput() (time.Duration, error) { return 0, ErrUnsupported }
func (unsupportedSystem) Engage() error                          { return ErrUnsupported }
func (unsupportedSystem) Release() error                         { return ErrUnsupported }
func (unsupportedSystem) Engaged() bool                          { return false }
func (unsupportedSystem) Run(Action) (map[string]any, error)     { return nil, ErrUnsupported }
