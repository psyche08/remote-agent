package computeruse

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/psyche08/remote-agent/internal/config"
)

// Controller owns the computer-use surface and the Locked Use state machine.
//
// The invariant that governs every path below: uncertainty relocks. There is no
// "log it and continue" branch on a safeguard failure. If the shield cannot be
// confirmed, if the input monitor cannot answer, if the lock state cannot be
// read — the window closes and the screen goes back to locked. A safeguard
// whose failure is survivable is not a safeguard.
type Controller struct {
	cfg      config.ComputerUseConfig
	deviceID string
	sys      System
	signer   *Signer

	// grantDir holds the single published grant; ledgerDir is this process's
	// mirror of the plugin's consumed-nonce ledger.
	grantDir  string
	ledgerDir string

	mu     sync.Mutex
	window *window
	// armed is set once startup scrub has confirmed a clean baseline. Nothing
	// opens a window before that.
	armed    bool
	armError string
	// active is the runtime toggle the console drives. Config is the ceiling:
	// this can turn Locked Use off on a device that permits it, but it can
	// never turn it on where config says no. A security capability must be
	// granted on the device, not enabled over the network.
	active    bool
	auditRing []AuditEntry

	// stop ends the monitor goroutine when the controller shuts down.
	stopOnce sync.Once
	stopCh   chan struct{}
}

// window is one authorized per-turn unlock window.
type window struct {
	turnID    string
	openedAt  time.Time
	expiresAt time.Time
	cancel    context.CancelFunc
	closed    bool
	// done closes once this window's cleanup has finished. A second closer
	// waits on it rather than returning while the relock is still in flight:
	// "the window is closed" must mean "the screen is confirmed locked", or
	// shutdown becomes a way to leave a Mac unlocked.
	done chan struct{}
}

// AuditEntry is one Locked Use state transition. It deliberately carries no
// grant body, no nonce beyond a short prefix, and no key material: the agent's
// log is uploaded off-device, so anything recorded here leaves the machine.
type AuditEntry struct {
	At          string `json:"at"`
	Event       string `json:"event"`
	TurnID      string `json:"turn_id,omitempty"`
	NoncePrefix string `json:"nonce_prefix,omitempty"`
	Reason      string `json:"reason,omitempty"`
}

const (
	// inputPollInterval is the fixed monitor cadence. It is intentionally not
	// configurable: a human at the keyboard must be noticed in tens of
	// milliseconds, and no deployment should be able to widen that to seconds.
	inputPollInterval = 40 * time.Millisecond
	// relockDeadline bounds how long cleanup will keep retrying a relock before
	// escalating. The shield stays up for the whole attempt. Cleanup never runs
	// on an HTTP request's path, so this is bounded for safety rather than to
	// fit a response budget.
	relockDeadline = 20 * time.Second
	// relockRetryInterval paces relock retries within relockDeadline.
	relockRetryInterval = 500 * time.Millisecond
	// auditRingSize bounds the in-memory audit ring surfaced by /computer_use.
	auditRingSize = 64
)

// DefaultGrantDir is where the Authorization Plug-in looks for a grant. It must
// stay in step with RA_LOCKED_USE_DIR in
// mac/authorization-plugin/RemoteAgentLockedUse.m.
const DefaultGrantDir = "/Library/Application Support/remote-agent/locked-use"

// NewController builds the controller. It does not arm Locked Use; call Start
// so the startup scrub runs before any window can open.
func NewController(cfg config.ComputerUseConfig, deviceID string, dataDir string, sys System) *Controller {
	lu := cfg.LockedUse
	grantDir := lu.GrantDir
	if grantDir == "" {
		// Default to the directory the Authorization Plug-in actually reads.
		// The plugin's path is a compile-time constant, so defaulting to the
		// agent's own state dir would produce a controller that publishes
		// grants nowhere the verifier looks — Locked Use would appear armed and
		// simply never unlock. Tests and non-Darwin builds override this.
		grantDir = DefaultGrantDir
	}
	c := &Controller{
		cfg:       cfg,
		deviceID:  deviceID,
		sys:       sys,
		grantDir:  grantDir,
		ledgerDir: filepath.Join(grantDir, "consumed"),
		stopCh:    make(chan struct{}),
	}
	return c
}

// Start performs the startup scrub and arms Locked Use if it is configured.
//
// The scrub exists because a crash is not a clean stop: it can leave a valid
// grant on disk and a screen that is unlocked with nothing watching it. A
// restart must never inherit that state, so we delete every grant artifact and
// command a relock before serving anything.
func (c *Controller) Start() {
	if !c.cfg.LockedUse.Enabled {
		return
	}
	if err := c.scrub(); err != nil {
		c.setArmError("startup scrub failed: " + err.Error())
		return
	}
	keyPath := c.cfg.LockedUse.SigningKeyPath
	if keyPath == "" {
		keyPath = filepath.Join(c.grantDir, "signing.key")
	}
	signer, err := LoadOrCreateSigner(keyPath, c.deviceID)
	if err != nil {
		c.setArmError("signing key unavailable: " + err.Error())
		return
	}
	c.mu.Lock()
	c.signer = signer
	c.armed = true
	c.active = true
	c.armError = ""
	c.mu.Unlock()
	c.audit(AuditEntry{Event: "armed"})
	go c.monitorLoop()
}

// Stop closes any open window and ends the monitor.
//
// The window is closed and its relock awaited BEFORE stopCh is signalled.
// Signalling first would let the watcher win the race, begin cleanup, and clear
// the registration, so this call would find nothing to do and return while the
// relock was still running — and the process would then exit mid-relock.
func (c *Controller) Stop() {
	c.CloseWindow("shutdown")
	c.stopOnce.Do(func() { close(c.stopCh) })
}

// scrub clears grant state and forces a locked baseline.
func (c *Controller) scrub() error {
	if err := ScrubGrants(c.grantDir); err != nil {
		return err
	}
	_ = PruneNonces(c.ledgerDir, time.Now())
	// Command an unconditional relock. A machine that is already locked is the
	// normal case and the lock is a no-op; a machine left unlocked by a crash
	// is exactly what this call is for.
	if err := c.sys.Lock(); err != nil {
		// On a device where the system layer is unavailable Locked Use cannot
		// be operated safely at all, so refuse to arm rather than arming with
		// an unverifiable baseline.
		return fmt.Errorf("could not establish a locked baseline: %w", err)
	}
	return nil
}

func (c *Controller) setArmError(msg string) {
	c.mu.Lock()
	c.armed = false
	c.armError = msg
	c.mu.Unlock()
	c.audit(AuditEntry{Event: "arm_failed", Reason: msg})
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
	// ErrNoWindow means an action needed an open unlock window and none exists.
	ErrNoWindow = errors.New("no open locked-use window for this turn")
)

// Enabled reports whether the computer-use surface is on.
func (c *Controller) Enabled() bool { return c.cfg.Enabled }

// LockedUseEnabled reports whether Locked Use is configured on.
func (c *Controller) LockedUseEnabled() bool { return c.cfg.LockedUse.Enabled }

// SetLockedUseActive is the console's runtime toggle.
//
// It can only move within what config already permits. Turning Locked Use off
// takes effect immediately and closes any open window; turning it on is
// refused unless the device's own config enabled the capability, so no remote
// caller can grant this device the ability to unlock itself.
func (c *Controller) SetLockedUseActive(active bool) error {
	if !c.cfg.Enabled {
		return ErrNotEnabled
	}
	if !c.cfg.LockedUse.Enabled {
		return ErrLockedUseNotEnabled
	}
	c.mu.Lock()
	armed := c.armed
	c.active = active
	c.mu.Unlock()
	if !active {
		// Clear the flag first, then close. An open that already passed the
		// ready check still holds the window reservation, so CloseWindow waits
		// for its teardown; a new open now fails the ready check outright.
		c.CloseWindow("locked use switched off")
	}
	if active && !armed {
		return ErrNotArmed
	}
	event := "deactivated"
	if active {
		event = "activated"
	}
	c.audit(AuditEntry{Event: event})
	return nil
}

// lockedUseReady reports whether a window may open at all right now.
func (c *Controller) lockedUseReady() error {
	if !c.cfg.Enabled {
		return ErrNotEnabled
	}
	if !c.cfg.LockedUse.Enabled {
		return ErrLockedUseNotEnabled
	}
	c.mu.Lock()
	armed, active, armError := c.armed, c.active, c.armError
	c.mu.Unlock()
	if !armed {
		if armError != "" {
			return fmt.Errorf("%w: %s", ErrNotArmed, armError)
		}
		return ErrNotArmed
	}
	if !active {
		return ErrLockedUseNotEnabled
	}
	return nil
}

// OpenWindow opens a per-turn unlock window and unlocks the screen.
//
// It is idempotent per turn: a repeated call for a turn that already owns the
// open window extends nothing and returns success, so a client retry after a
// relay timeout cannot double-open or stack windows.
//
// The caller's request context is deliberately not threaded through. Unlocking,
// shielding and relocking must complete or roll back on the device's own
// schedule; a client disconnect or the relay's 30s cut must never abandon a
// half-open window.
func (c *Controller) OpenWindow(turnID string) error {
	if turnID == "" {
		return errors.New("turn_id is required")
	}
	if err := c.lockedUseReady(); err != nil {
		return err
	}

	now := time.Now()
	c.mu.Lock()
	// A window is registered before any of the slow work below and stays
	// registered until its cleanup finishes. Opening involves an unlock that
	// can take seconds, so without this reservation a client retry after the
	// relay's 30s cut would sail past the check and start a second concurrent
	// unlock against the same desktop.
	if existing := c.window; existing != nil {
		turn, closing := existing.turnID, existing.closed
		c.mu.Unlock()
		switch {
		case turn == turnID && !closing:
			return nil
		case closing:
			return fmt.Errorf("the locked-use window for turn %s is still closing", turn)
		default:
			return fmt.Errorf("another turn (%s) owns the locked-use window", turn)
		}
	}
	signer := c.signer
	w := &window{
		turnID:    turnID,
		openedAt:  now,
		expiresAt: now.Add(time.Duration(c.cfg.LockedUse.WindowTTLSeconds) * time.Second),
		done:      make(chan struct{}),
	}
	c.window = w
	c.mu.Unlock()

	// A person at the machine outranks a remote turn. Require the device to
	// already be idle before taking it over.
	if err := c.requireIdle(); err != nil {
		c.abortOpen(w, err.Error())
		return err
	}

	// The shield goes up before anything is unlocked, never after: the gap
	// between an unlock and a shield is a window where the desktop is visible
	// to whoever is standing there.
	if c.cfg.LockedUse.ShieldRequired() {
		if err := c.sys.Engage(); err != nil {
			c.abortOpen(w, "shield: "+err.Error())
			return fmt.Errorf("%w: %v", ErrShieldRequired, err)
		}
		if !c.sys.Engaged() {
			c.abortOpen(w, "shield not confirmed")
			return ErrShieldRequired
		}
	}

	// Mint and publish a grant only now, immediately before the unlock it
	// authorizes, and withdraw it as soon as the unlock resolves. The grant is
	// never left in place for the life of the window.
	ttl := time.Duration(c.cfg.LockedUse.GrantTTLSeconds) * time.Second
	grant, payload, err := signer.Mint(turnID, ttl, time.Now())
	if err != nil {
		c.abortOpen(w, "mint: "+err.Error())
		return err
	}
	if err := WriteGrant(c.grantDir, grant); err != nil {
		c.abortOpen(w, "publish: "+err.Error())
		return err
	}
	c.audit(AuditEntry{Event: "grant_published", TurnID: turnID, NoncePrefix: noncePrefix(payload.Nonce)})

	// The unlock itself is performed by macOS. This process does not supply a
	// credential; it has only asserted, verifiably, that an authorized turn is
	// asking. The Authorization Plug-in decides.
	unlockErr := c.awaitUnlock(payload)
	// Withdraw the grant regardless of outcome so nothing can ride it later.
	_ = RemoveGrant(c.grantDir)
	if unlockErr != nil {
		c.abortOpen(w, "unlock: "+unlockErr.Error())
		return unlockErr
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.mu.Lock()
	w.cancel = cancel
	c.mu.Unlock()
	go c.watchWindow(ctx, w)
	c.audit(AuditEntry{Event: "window_opened", TurnID: turnID})
	return nil
}

// abortOpen tears down a window that never finished opening.
//
// The security-critical part — withdrawing the grant so nothing can ride it —
// happens synchronously. The verified relock then runs in the background, because
// it retries on a deadline that can outlast the relay's 30s HTTP timeout and the
// caller must not be held that long for an error it can already act on. The
// window stays registered until that cleanup completes, so a retry cannot open a
// new window while the previous one is still being unwound.
func (c *Controller) abortOpen(w *window, reason string) {
	_ = RemoveGrant(c.grantDir)
	c.audit(AuditEntry{Event: "open_failed", TurnID: w.turnID, Reason: reason})
	c.mu.Lock()
	if w.closed {
		done := w.done
		c.mu.Unlock()
		<-done
		return
	}
	w.closed = true
	c.mu.Unlock()
	go func() {
		c.cleanup(w, reason)
		c.releaseWindow(w)
		close(w.done)
	}()
}

// releaseWindow clears a window once its cleanup has finished, letting the next
// open proceed.
func (c *Controller) releaseWindow(w *window) {
	c.mu.Lock()
	if c.window == w {
		c.window = nil
	}
	c.mu.Unlock()
}

// awaitUnlock waits for the screen to actually become unlocked after a grant is
// published, bounded by the grant's own lifetime. It reports an error if the
// unlock does not happen — a published grant that nothing consumed is not a
// success, and the caller must not proceed as though the desktop is reachable.
func (c *Controller) awaitUnlock(payload GrantPayload) error {
	deadline := time.Unix(payload.ExpiresAt, 0)
	for {
		locked, err := c.sys.Locked()
		if err != nil {
			return fmt.Errorf("could not read lock state: %w", err)
		}
		if !locked {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("unlock was not authorized before the grant expired")
		}
		select {
		case <-time.After(inputPollInterval):
		case <-c.stopCh:
			return errors.New("shutting down")
		}
	}
}

// watchWindow enforces the window's limits for its whole life: the hard TTL,
// local input, and continued shield coverage.
func (c *Controller) watchWindow(ctx context.Context, w *window) {
	ticker := time.NewTicker(inputPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.stopCh:
			c.closeWindowIf(w, "shutdown")
			return
		case <-ticker.C:
			if reason := c.windowViolation(w); reason != "" {
				c.closeWindowIf(w, reason)
				return
			}
		}
	}
}

// windowViolation returns a non-empty reason when the window must close now.
// Every error path returns a reason: an unreadable safeguard is a violation,
// not a pass.
func (c *Controller) windowViolation(w *window) string {
	if time.Now().After(w.expiresAt) {
		return "window ttl expired"
	}
	idle, err := c.sys.SinceLastInput()
	if err != nil {
		return "input monitor unavailable"
	}
	// Any local input since the window opened means a person is present. The
	// idle counter having reset below the window's age is that signal.
	if idle < time.Since(w.openedAt) {
		return "local input detected"
	}
	if c.cfg.LockedUse.ShieldRequired() && !c.sys.Engaged() {
		return "display shield dropped"
	}
	return ""
}

// requireIdle refuses to open a window on a machine someone is actively using.
func (c *Controller) requireIdle() error {
	idle, err := c.sys.SinceLastInput()
	if err != nil {
		return fmt.Errorf("could not read local input state: %w", err)
	}
	grace := time.Duration(c.cfg.LockedUse.InputRelockGraceMs) * time.Millisecond
	if idle < grace {
		return ErrLocalInput
	}
	return nil
}

// CloseWindow ends the current window for any reason.
func (c *Controller) CloseWindow(reason string) {
	c.mu.Lock()
	w := c.window
	c.mu.Unlock()
	if w == nil {
		return
	}
	c.closeWindowIf(w, reason)
}

// windowClosing reports whether a window is registered but already unwinding,
// so callers can tell "busy" from "open".
func (c *Controller) windowClosing() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.window != nil && c.window.closed
}

// CloseWindowForTurn ends the window only if the named turn owns it, so a
// finishing turn cannot relock a window another turn legitimately opened.
func (c *Controller) CloseWindowForTurn(turnID string, reason string) {
	c.mu.Lock()
	w := c.window
	c.mu.Unlock()
	if w == nil || w.turnID != turnID {
		return
	}
	c.closeWindowIf(w, reason)
}

func (c *Controller) closeWindowIf(w *window, reason string) {
	c.mu.Lock()
	if w.closed {
		done := w.done
		c.mu.Unlock()
		// Another goroutine already owns this teardown. Wait for it rather than
		// reporting success while its relock is still running.
		<-done
		return
	}
	w.closed = true
	cancel := w.cancel
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	c.cleanup(w, reason)
	// Clear the registration only after cleanup, so a new window cannot open
	// while this one is still being relocked.
	c.releaseWindow(w)
	close(w.done)
}

// cleanup restores the locked, unshielded baseline in a strict order:
// relock and confirm it first, then drop the shield, then withdraw the grant.
//
// The ordering is the point. Dropping the shield before the screen is confirmed
// locked would expose the live desktop at exactly the moment the agent believes
// it has finished cleaning up — the worst possible state.
func (c *Controller) cleanup(w *window, reason string) {
	// A window that is no longer the registered one has already been superseded.
	// Relocking here would tear down whatever window replaced it.
	c.mu.Lock()
	superseded := c.window != nil && c.window != w
	c.mu.Unlock()
	if superseded {
		c.audit(AuditEntry{Event: "cleanup_skipped", TurnID: w.turnID, Reason: "window superseded"})
		return
	}

	// Withdrawing the grant is always safe and always first-priority: it stops
	// any *new* unlock from being authorized while we work.
	_ = RemoveGrant(c.grantDir)

	relocked := c.relockVerified(w)
	if relocked {
		if c.cfg.LockedUse.ShieldRequired() || c.sys.Engaged() {
			_ = c.sys.Release()
		}
		c.audit(AuditEntry{Event: "window_closed", TurnID: w.turnID, Reason: reason})
		return
	}
	// Relock failed. Keep the shield up — an uncovered unlocked screen is worse
	// than a covered one — and record it loudly. The shield stays until a later
	// relock succeeds or an operator intervenes.
	c.audit(AuditEntry{
		Event:  "relock_failed",
		TurnID: w.turnID,
		Reason: reason + "; shield held up, screen may still be unlocked",
	})
}

// relockVerified locks the screen and confirms it by reading the state back,
// retrying within a bounded deadline. A lock command that returns success is
// not evidence the screen is locked.
func (c *Controller) relockVerified(w *window) bool {
	// A window that never unlocked anything still gets a confirmation pass:
	// the cheapest way to know the baseline holds is to check it.
	deadline := time.Now().Add(relockDeadline)
	for {
		if locked, err := c.sys.Locked(); err == nil && locked {
			return true
		}
		if err := c.sys.Lock(); err == nil {
			if locked, err := c.sys.Locked(); err == nil && locked {
				return true
			}
		}
		if time.Now().After(deadline) {
			return false
		}
		// Deliberately not selecting on stopCh: a relock in progress must run
		// to its deadline even while shutting down. Cutting it short is exactly
		// how a shutdown would leave the Mac unlocked.
		time.Sleep(relockRetryInterval)
	}
}

// monitorLoop prunes the nonce ledger. Window enforcement lives in
// watchWindow; this loop only handles slow background maintenance.
func (c *Controller) monitorLoop() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-c.stopCh:
			return
		case <-ticker.C:
			_ = PruneNonces(c.ledgerDir, time.Now())
		}
	}
}

// WindowOpen reports whether a window is currently open, and for which turn.
func (c *Controller) WindowOpen() (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.window == nil || c.window.closed {
		return "", false
	}
	return c.window.turnID, true
}

// CaptureAllowed reports whether a screen capture may proceed right now.
//
// Screen capture is not suppressed while the desktop is unlocked under Locked
// Use, so an unshielded capture could persist and then serve the lock screen or
// whatever was on it. Capture is therefore refused whenever a window is open
// and the shield is not confirmed up.
func (c *Controller) CaptureAllowed() (bool, string) {
	c.mu.Lock()
	open := c.window != nil && !c.window.closed
	c.mu.Unlock()
	if !open {
		return true, ""
	}
	// A device that explicitly opted out of the shield has accepted that its
	// desktop is visible while a window is open; refusing every capture there
	// would disable the feature rather than protect anything.
	if !c.cfg.LockedUse.ShieldRequired() {
		return true, ""
	}
	if !c.sys.Engaged() {
		return false, "a locked-use window is open without a confirmed display shield"
	}
	return true, ""
}

// RunAction executes a validated action.
//
// When a Locked Use window is open, the same capture gate applies: an action
// that produces a frame is refused unless the shield is confirmed.
func (c *Controller) RunAction(action Action) (map[string]any, error) {
	if !c.cfg.Enabled {
		return nil, ErrNotEnabled
	}
	if !systemAvailable(c.sys) {
		return nil, ErrUnsupported
	}
	if action.ID == ActionScreenshot {
		if ok, reason := c.CaptureAllowed(); !ok {
			return nil, errors.New(reason)
		}
	}
	return c.sys.Run(action)
}

// Status describes the feature for the console. It reports capability and
// state only — never a grant, a nonce, or key material.
func (c *Controller) Status() map[string]any {
	c.mu.Lock()
	armed, active, armError := c.armed, c.active, c.armError
	turnID, open := "", false
	if c.window != nil && !c.window.closed {
		turnID, open = c.window.turnID, true
	}
	audit := make([]AuditEntry, len(c.auditRing))
	copy(audit, c.auditRing)
	// The public key is the verifying half and is meant to be published: an
	// operator provisions the Authorization Plug-in with it. It cannot sign a
	// grant, so exposing it grants nothing.
	publicKey := c.signer.PublicKeyBase64()
	c.mu.Unlock()

	lu := c.cfg.LockedUse
	status := map[string]any{
		"enabled":   c.cfg.Enabled,
		"available": systemAvailable(c.sys),
		"actions":   ActionCatalog(),
		"locked_use": map[string]any{
			"enabled":                lu.Enabled,
			"armed":                  armed,
			"active":                 active,
			"window_open":            open,
			"window_turn_id":         turnID,
			"grant_ttl_seconds":      lu.GrantTTLSeconds,
			"window_ttl_seconds":     lu.WindowTTLSeconds,
			"require_display_shield": lu.ShieldRequired(),
			"shield_engaged":         c.sys.Engaged(),
		},
		"audit": audit,
	}
	if armError != "" {
		status["locked_use"].(map[string]any)["error"] = armError
	}
	if publicKey != "" {
		status["locked_use"].(map[string]any)["public_key"] = publicKey
	}
	return status
}

// PublicKeyBase64 exposes the verifying key so an operator can provision the
// Authorization Plug-in. Returns empty when Locked Use never armed.
func (c *Controller) PublicKeyBase64() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.signer.PublicKeyBase64()
}

// audit appends to the bounded in-memory ring.
func (c *Controller) audit(entry AuditEntry) {
	entry.At = time.Now().UTC().Format(time.RFC3339Nano)
	c.mu.Lock()
	c.auditRing = append(c.auditRing, entry)
	if len(c.auditRing) > auditRingSize {
		c.auditRing = c.auditRing[len(c.auditRing)-auditRingSize:]
	}
	c.mu.Unlock()
}

// noncePrefix shortens a nonce for audit. A prefix is enough to correlate two
// records; the full value is the single-use secret and never leaves the grant.
func noncePrefix(nonce string) string {
	if len(nonce) <= 8 {
		return ""
	}
	return nonce[:8]
}

// ActionCatalog lists the closed action vocabulary for clients.
func ActionCatalog() []string {
	return []string{
		string(ActionScreenshot), string(ActionMove), string(ActionClick),
		string(ActionScroll), string(ActionType), string(ActionKey),
	}
}
