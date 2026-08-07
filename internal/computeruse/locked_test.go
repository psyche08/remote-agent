package computeruse

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/psyche08/remote-agent/internal/config"
)

// fakeSystem stands in for the Mac. It lets a test drive every safeguard into
// both its working and its failing state, which is the only way to prove the
// "uncertainty relocks" invariant actually holds.
type fakeSystem struct {
	mu sync.Mutex

	locked       bool
	idle         time.Duration
	shieldUp     bool
	shieldFails  bool
	lockFails    bool
	lockStateErr bool
	idleErr      bool
	// grantDir mirrors the directory the Authorization Plug-in watches. The
	// fake consumes a grant from it exactly once, the way the real plugin does,
	// so a test that observes an unlock has also proven the controller actually
	// published a grant to authorize it.
	grantDir string
	actions  []Action
}

func newFakeSystem() *fakeSystem {
	return &fakeSystem{locked: true, idle: time.Hour}
}

func (f *fakeSystem) Locked() (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lockStateErr {
		return false, errors.New("lock state unavailable")
	}
	if f.locked && f.grantDir != "" {
		if _, err := os.Stat(f.grantDir + "/" + GrantFileName); err == nil {
			// Stand in for the plugin: consume the grant and allow the unlock.
			// Consumption is single-use, so a stale grant cannot unlock twice.
			_ = os.Remove(f.grantDir + "/" + GrantFileName)
			f.locked = false
		}
	}
	return f.locked, nil
}

func (f *fakeSystem) Lock() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lockFails {
		return errors.New("lock failed")
	}
	f.locked = true
	return nil
}

func (f *fakeSystem) SinceLastInput() (time.Duration, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.idleErr {
		return 0, errors.New("input monitor unavailable")
	}
	return f.idle, nil
}

func (f *fakeSystem) Engage() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.shieldFails {
		return errors.New("shield failed")
	}
	f.shieldUp = true
	return nil
}

func (f *fakeSystem) Release() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.shieldUp = false
	return nil
}

func (f *fakeSystem) Engaged() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.shieldUp
}

func (f *fakeSystem) Run(a Action) (map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.actions = append(f.actions, a)
	return map[string]any{"ran": string(a.ID)}, nil
}

// ranActions returns the actions that reached the system layer, so a test can
// assert that a refused action never got there.
func (f *fakeSystem) ranActions() []Action {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Action(nil), f.actions...)
}

func (f *fakeSystem) set(mutate func(*fakeSystem)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	mutate(f)
}

func (f *fakeSystem) isLocked() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.locked
}

func testConfig(mutate func(*config.ComputerUseConfig)) config.ComputerUseConfig {
	cfg := config.ComputerUseConfig{
		Enabled:   true,
		LockedUse: config.LockedUseConfig{Enabled: true},
	}
	if mutate != nil {
		mutate(&cfg)
	}
	full := config.Config{ComputerUse: cfg}
	config.ApplyDefaults(&full)
	return full.ComputerUse
}

func newTestController(t *testing.T, sys *fakeSystem, mutate func(*config.ComputerUseConfig)) *Controller {
	t.Helper()
	dir := t.TempDir()
	cfg := testConfig(func(cu *config.ComputerUseConfig) {
		// Production defaults to the plugin's root-owned directory; a test must
		// stay inside its own temp dir.
		cu.LockedUse.GrantDir = filepath.Join(dir, "locked-use")
		if mutate != nil {
			mutate(cu)
		}
	})
	c := NewController(cfg, "mac-test", dir, sys)
	// Point the fake at the same directory the plugin would watch, so an
	// unlock only happens when the controller really publishes a grant.
	sys.set(func(f *fakeSystem) { f.grantDir = c.grantDir })
	c.Start()
	t.Cleanup(c.Stop)
	return c
}

func TestControllerArmsAndOpensWindow(t *testing.T) {
	sys := newFakeSystem()
	c := newTestController(t, sys, nil)

	if err := c.OpenWindow("turn-1"); err != nil {
		t.Fatalf("OpenWindow: %v", err)
	}
	turnID, open := c.WindowOpen()
	if !open || turnID != "turn-1" {
		t.Fatalf("WindowOpen() = %q,%v; want turn-1,true", turnID, open)
	}
	if !sys.Engaged() {
		t.Error("display shield is not up while a window is open")
	}

	c.CloseWindowForTurn("turn-1", "turn ended")
	if _, open := c.WindowOpen(); open {
		t.Error("window still open after close")
	}
	if !sys.isLocked() {
		t.Error("screen was not relocked when the window closed")
	}
	if sys.Engaged() {
		t.Error("shield still up after a confirmed relock")
	}
}

// The grant is the unlock authorization. It must not outlive the moment it
// authorizes — a grant resting on disk is ambient authority for anything local.
func TestGrantDoesNotOutliveTheUnlock(t *testing.T) {
	sys := newFakeSystem()
	c := newTestController(t, sys, nil)

	if err := c.OpenWindow("turn-1"); err != nil {
		t.Fatalf("OpenWindow: %v", err)
	}
	if grantFileExists(t, c) {
		t.Error("a grant is still published after the unlock completed")
	}
	c.CloseWindowForTurn("turn-1", "done")
	if grantFileExists(t, c) {
		t.Error("a grant is still published after the window closed")
	}
}

func TestOpenWindowIsIdempotentPerTurn(t *testing.T) {
	sys := newFakeSystem()
	c := newTestController(t, sys, nil)

	if err := c.OpenWindow("turn-1"); err != nil {
		t.Fatalf("first open: %v", err)
	}
	// A client retry after a relay timeout must not stack windows.
	if err := c.OpenWindow("turn-1"); err != nil {
		t.Fatalf("repeat open for the same turn: %v", err)
	}
	// A different turn must not silently take over the open window.
	if err := c.OpenWindow("turn-2"); err == nil {
		t.Fatal("a second turn opened a window while turn-1 owned one")
	}
}

func TestOpenWindowRefusedWhenLocalInputIsRecent(t *testing.T) {
	sys := newFakeSystem()
	sys.set(func(f *fakeSystem) { f.idle = 10 * time.Millisecond })
	c := newTestController(t, sys, nil)

	err := c.OpenWindow("turn-1")
	if !errors.Is(err, ErrLocalInput) {
		t.Fatalf("err = %v, want ErrLocalInput", err)
	}
	if _, open := c.WindowOpen(); open {
		t.Error("a window opened despite a person at the machine")
	}
}

// A safeguard that cannot answer is a safeguard that failed. An unreadable
// input monitor must refuse the window, not skip the check.
func TestOpenWindowRefusedWhenInputMonitorFails(t *testing.T) {
	sys := newFakeSystem()
	sys.set(func(f *fakeSystem) { f.idleErr = true })
	c := newTestController(t, sys, nil)

	if err := c.OpenWindow("turn-1"); err == nil {
		t.Fatal("window opened while the input monitor was unavailable")
	}
	if _, open := c.WindowOpen(); open {
		t.Error("window is open after a refused request")
	}
}

func TestOpenWindowRefusedWhenShieldFails(t *testing.T) {
	sys := newFakeSystem()
	sys.set(func(f *fakeSystem) { f.shieldFails = true })
	c := newTestController(t, sys, nil)

	err := c.OpenWindow("turn-1")
	if !errors.Is(err, ErrShieldRequired) {
		t.Fatalf("err = %v, want ErrShieldRequired", err)
	}
	if !sys.isLocked() {
		t.Error("screen was left unlocked after a refused open")
	}
	if grantFileExists(t, c) {
		t.Error("a grant was left behind after a refused open")
	}
}

func TestLocalInputDuringWindowTriggersRelock(t *testing.T) {
	sys := newFakeSystem()
	c := newTestController(t, sys, nil)

	if err := c.OpenWindow("turn-1"); err != nil {
		t.Fatalf("OpenWindow: %v", err)
	}
	// A person touches the keyboard: the idle counter resets.
	sys.set(func(f *fakeSystem) { f.idle = 0 })

	// Wait on the invariant that matters — the screen ends up locked — rather
	// than on the window flag, which is cleared before cleanup finishes.
	waitForRelock(t, c, sys, "local input did not relock the screen")
}

// The hard TTL is a ceiling independent of turn activity: a turn that keeps
// working must still lose the window when its time is up.
func TestWindowTTLExpiryClosesWindow(t *testing.T) {
	sys := newFakeSystem()
	// Build the config directly rather than through ApplyDefaults, so the
	// window is one second instead of the clamp floor. A window's deadline is
	// fixed when it opens and never mutated afterwards, so shortening it here
	// is the only way to exercise the real watcher quickly.
	dir := t.TempDir()
	cfg := config.ComputerUseConfig{
		Enabled: true,
		LockedUse: config.LockedUseConfig{
			Enabled:          true,
			WindowTTLSeconds: 1,
			GrantTTLSeconds:  int(MaxGrantTTL / time.Second),
			GrantDir:         filepath.Join(dir, "locked-use"),
		},
	}
	c := NewController(cfg, "mac-test", dir, sys)
	sys.set(func(f *fakeSystem) { f.grantDir = c.grantDir })
	c.Start()
	t.Cleanup(c.Stop)

	if err := c.OpenWindow("turn-1"); err != nil {
		t.Fatalf("OpenWindow: %v", err)
	}
	waitForRelock(t, c, sys, "window outlived its hard TTL without relocking")
}

// The config layer must not let a deployment widen these windows past their
// clamps, since every safeguard timing depends on them.
func TestConfigClampsBoundWindowAndGrantLifetimes(t *testing.T) {
	cfg := testConfig(func(cu *config.ComputerUseConfig) {
		cu.LockedUse.GrantTTLSeconds = 3600
		cu.LockedUse.WindowTTLSeconds = 86400
		cu.LockedUse.InputRelockGraceMs = 600000
	})
	if cfg.LockedUse.GrantTTLSeconds > config.MaxGrantTTLSeconds {
		t.Errorf("grant ttl = %ds, want <= %ds", cfg.LockedUse.GrantTTLSeconds, config.MaxGrantTTLSeconds)
	}
	if cfg.LockedUse.WindowTTLSeconds > 900 {
		t.Errorf("window ttl = %ds, want <= 900s", cfg.LockedUse.WindowTTLSeconds)
	}
	if cfg.LockedUse.InputRelockGraceMs > 5000 {
		t.Errorf("input grace = %dms, want <= 5000ms", cfg.LockedUse.InputRelockGraceMs)
	}
	// A grant must never be able to outlive the window it was minted for.
	if cfg.LockedUse.GrantTTLSeconds > cfg.LockedUse.WindowTTLSeconds {
		t.Error("grant ttl exceeds the window ttl")
	}
}

func TestShieldDropDuringWindowTriggersRelock(t *testing.T) {
	sys := newFakeSystem()
	c := newTestController(t, sys, nil)
	if err := c.OpenWindow("turn-1"); err != nil {
		t.Fatalf("OpenWindow: %v", err)
	}
	// A display hot-plug or a crashed shield host drops coverage.
	sys.set(func(f *fakeSystem) { f.shieldUp = false })

	waitForRelock(t, c, sys, "a dropped shield did not relock the screen")
}

// Dropping the shield while the screen is still unlocked is the worst outcome:
// the desktop is live and uncovered, and the agent believes it cleaned up.
func TestFailedRelockKeepsShieldUp(t *testing.T) {
	sys := newFakeSystem()
	c := newTestController(t, sys, nil)
	if err := c.OpenWindow("turn-1"); err != nil {
		t.Fatalf("OpenWindow: %v", err)
	}
	sys.set(func(f *fakeSystem) {
		f.lockFails = true
		f.lockStateErr = true
	})

	done := make(chan struct{})
	go func() {
		c.CloseWindowForTurn("turn-1", "turn ended")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(relockDeadline + 10*time.Second):
		t.Fatal("close did not return within the relock deadline")
	}

	if !sys.Engaged() {
		t.Error("shield was dropped even though the relock could not be confirmed")
	}
	if !hasAuditEvent(c, "relock_failed") {
		t.Error("a failed relock was not recorded in the audit ring")
	}
}

func TestStartupScrubRemovesOrphanedGrantAndRelocks(t *testing.T) {
	sys := newFakeSystem()
	dataDir := t.TempDir()

	// Simulate a crash: a valid grant left on disk and an unlocked screen.
	scrubCfg := testConfig(func(cu *config.ComputerUseConfig) {
		cu.LockedUse.GrantDir = filepath.Join(dataDir, "locked-use")
	})
	pre := NewController(scrubCfg, "mac-test", dataDir, sys)
	signer, err := LoadOrCreateSigner(pre.grantDir+"/signing.key", "mac-test")
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	grant, _, _ := signer.Mint("crashed-turn", 10*time.Second, time.Now())
	if err := WriteGrant(pre.grantDir, grant); err != nil {
		t.Fatalf("WriteGrant: %v", err)
	}
	sys.set(func(f *fakeSystem) { f.locked = false })

	c := NewController(scrubCfg, "mac-test", dataDir, sys)
	sys.set(func(f *fakeSystem) { f.grantDir = c.grantDir })
	c.Start()
	t.Cleanup(c.Stop)

	if grantFileExists(t, c) {
		t.Error("a grant survived startup and could still authorize an unlock")
	}
	if !sys.isLocked() {
		t.Error("startup did not restore a locked baseline")
	}
}

// A device where the system layer cannot answer must not arm at all: arming on
// an unverifiable baseline would mean every later safeguard is guesswork.
func TestControllerRefusesToArmWithoutASystem(t *testing.T) {
	dir := t.TempDir()
	c := NewController(testConfig(func(cu *config.ComputerUseConfig) {
		cu.LockedUse.GrantDir = filepath.Join(dir, "locked-use")
	}), "mac-test", dir, unsupportedSystem{})
	c.Start()
	t.Cleanup(c.Stop)

	if err := c.OpenWindow("turn-1"); !errors.Is(err, ErrNotArmed) {
		t.Fatalf("err = %v, want ErrNotArmed", err)
	}
}

// Config is the ceiling. A network caller must never be able to grant a Mac
// the ability to unlock itself.
func TestRuntimeToggleCannotEnableWhatConfigDisabled(t *testing.T) {
	sys := newFakeSystem()
	c := newTestController(t, sys, func(cu *config.ComputerUseConfig) {
		cu.LockedUse.Enabled = false
	})
	if err := c.SetLockedUseActive(true); !errors.Is(err, ErrLockedUseNotEnabled) {
		t.Fatalf("err = %v, want ErrLockedUseNotEnabled", err)
	}
	if err := c.OpenWindow("turn-1"); !errors.Is(err, ErrLockedUseNotEnabled) {
		t.Fatalf("OpenWindow err = %v, want ErrLockedUseNotEnabled", err)
	}
}

func TestRuntimeToggleOffClosesWindowAndRelocks(t *testing.T) {
	sys := newFakeSystem()
	c := newTestController(t, sys, nil)
	if err := c.OpenWindow("turn-1"); err != nil {
		t.Fatalf("OpenWindow: %v", err)
	}
	if err := c.SetLockedUseActive(false); err != nil {
		t.Fatalf("SetLockedUseActive(false): %v", err)
	}
	if _, open := c.WindowOpen(); open {
		t.Error("window survived the runtime toggle")
	}
	if !sys.isLocked() {
		t.Error("screen was not relocked when Locked Use was switched off")
	}
	if err := c.OpenWindow("turn-2"); !errors.Is(err, ErrLockedUseNotEnabled) {
		t.Fatalf("err = %v, want ErrLockedUseNotEnabled after toggle off", err)
	}
}

// Screen capture while the desktop is unlocked and uncovered would persist and
// then serve whatever is on screen.
func TestCaptureRefusedWhenWindowOpenWithoutShield(t *testing.T) {
	sys := newFakeSystem()
	c := newTestController(t, sys, nil)

	if ok, _ := c.CaptureAllowed(); !ok {
		t.Fatal("capture refused with no window open")
	}
	if err := c.OpenWindow("turn-1"); err != nil {
		t.Fatalf("OpenWindow: %v", err)
	}
	if ok, _ := c.CaptureAllowed(); !ok {
		t.Error("capture refused while the shield is confirmed up")
	}

	sys.set(func(f *fakeSystem) { f.shieldUp = false })
	ok, reason := c.CaptureAllowed()
	if ok {
		t.Error("capture allowed while a window was open without a shield")
	}
	if reason == "" {
		t.Error("refusal carried no reason")
	}
	if _, err := c.RunAction(Action{ID: ActionScreenshot}); err == nil {
		t.Error("RunAction ran a capture that CaptureAllowed refused")
	}
	// The refusal must happen before the system layer, not after it captured.
	for _, a := range sys.ranActions() {
		if a.ID == ActionScreenshot {
			t.Fatal("a refused capture still reached the system layer")
		}
	}
}

func TestRunActionRequiresComputerUseEnabled(t *testing.T) {
	sys := newFakeSystem()
	dir := t.TempDir()
	c := NewController(testConfig(func(cu *config.ComputerUseConfig) {
		cu.Enabled = false
		cu.LockedUse.GrantDir = filepath.Join(dir, "locked-use")
	}), "mac-test", dir, sys)

	if _, err := c.RunAction(Action{ID: ActionMove, X: 1, Y: 1}); !errors.Is(err, ErrNotEnabled) {
		t.Fatalf("err = %v, want ErrNotEnabled", err)
	}
}

// The audit ring is surfaced over the API and the agent's log is uploaded
// off-device, so it must never carry a full nonce or key material.
func TestAuditRingCarriesNoSecrets(t *testing.T) {
	sys := newFakeSystem()
	c := newTestController(t, sys, nil)
	if err := c.OpenWindow("turn-1"); err != nil {
		t.Fatalf("OpenWindow: %v", err)
	}
	c.CloseWindowForTurn("turn-1", "done")

	c.mu.Lock()
	entries := append([]AuditEntry(nil), c.auditRing...)
	c.mu.Unlock()

	if len(entries) == 0 {
		t.Fatal("no audit entries recorded")
	}
	sawGrant := false
	for _, e := range entries {
		if e.Event == "grant_published" {
			sawGrant = true
			if len(e.NoncePrefix) > 8 {
				t.Errorf("audit recorded %d nonce characters, want a short prefix", len(e.NoncePrefix))
			}
			if e.NoncePrefix == "" {
				t.Error("grant_published carried no nonce prefix to correlate with")
			}
		}
	}
	if !sawGrant {
		t.Error("no grant_published entry recorded")
	}
	pub := c.PublicKeyBase64()
	for _, e := range entries {
		if e.Reason != "" && pub != "" && e.Reason == pub {
			t.Error("audit reason leaked key material")
		}
	}
}

func TestStatusReportsCapabilityWithoutSecrets(t *testing.T) {
	sys := newFakeSystem()
	c := newTestController(t, sys, nil)

	status := c.Status()
	lu, ok := status["locked_use"].(map[string]any)
	if !ok {
		t.Fatal("status has no locked_use block")
	}
	if lu["enabled"] != true || lu["armed"] != true {
		t.Fatalf("locked_use = %+v; want enabled and armed", lu)
	}
	for _, banned := range []string{"signing_key", "private_key", "grant", "nonce", "password"} {
		if _, present := lu[banned]; present {
			t.Errorf("status exposes %q", banned)
		}
	}
}

func grantFileExists(t *testing.T, c *Controller) bool {
	t.Helper()
	_, err := osStat(c.grantDir + "/" + GrantFileName)
	return err == nil
}

// waitForRelock waits for a window to close AND the screen to end up locked.
// The window flag clears before cleanup finishes, so asserting on it alone
// would race the relock it is supposed to guarantee.
func waitForRelock(t *testing.T, c *Controller, sys *fakeSystem, msg string) {
	t.Helper()
	waitFor(t, 10*time.Second, func() bool {
		_, open := c.WindowOpen()
		return !open && sys.isLocked()
	}, msg)
}

func waitFor(t *testing.T, limit time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(msg)
}

// osStat is a thin indirection so the helper above reads clearly.
func osStat(path string) (os.FileInfo, error) { return os.Stat(path) }

// hasAuditEvent reports whether the ring recorded the named event.
func hasAuditEvent(c *Controller, event string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.auditRing {
		if e.Event == event {
			return true
		}
	}
	return false
}

// The public key must be published so an operator can provision the plug-in,
// and it must be the key that actually verifies this controller's grants.
func TestStatusPublishesUsableVerifyingKey(t *testing.T) {
	sys := newFakeSystem()
	c := newTestController(t, sys, nil)

	lu := c.Status()["locked_use"].(map[string]any)
	encoded, _ := lu["public_key"].(string)
	if encoded == "" {
		t.Fatal("status did not publish a public key to provision the plug-in")
	}
	pub, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(pub) != PublicKeyBytes {
		t.Fatalf("published key is not a usable P-256 verifying key: %v", err)
	}

	c.mu.Lock()
	signer := c.signer
	c.mu.Unlock()
	grant, _, err := signer.Mint("turn-1", 10*time.Second, time.Now())
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if _, err := VerifyGrant(grant, pub, "mac-test", time.Now()); err != nil {
		t.Fatalf("published key does not verify this controller's grants: %v", err)
	}
}

// Opening involves an unlock that takes seconds. A second caller arriving in
// that gap — a client retry after the relay's 30s cut is the realistic case —
// must not start a second concurrent unlock against the same desktop.
func TestConcurrentOpensYieldOneWindow(t *testing.T) {
	sys := newFakeSystem()
	c := newTestController(t, sys, nil)

	const callers = 8
	results := make(chan error, callers)
	start := make(chan struct{})
	for i := 0; i < callers; i++ {
		turn := "turn-" + string(rune('a'+i))
		go func() {
			<-start
			results <- c.OpenWindow(turn)
		}()
	}
	close(start)
	opened := 0
	for i := 0; i < callers; i++ {
		if err := <-results; err == nil {
			opened++
		}
	}
	if opened != 1 {
		t.Fatalf("%d concurrent callers opened a window, want exactly 1", opened)
	}
	if _, open := c.WindowOpen(); !open {
		t.Error("no window is open after a successful concurrent race")
	}
}

// A refused open must not strand the reservation: once its cleanup finishes,
// the next turn can open normally.
func TestWindowReservationIsReleasedAfterARefusedOpen(t *testing.T) {
	sys := newFakeSystem()
	sys.set(func(f *fakeSystem) { f.shieldFails = true })
	c := newTestController(t, sys, nil)

	if err := c.OpenWindow("turn-1"); !errors.Is(err, ErrShieldRequired) {
		t.Fatalf("err = %v, want ErrShieldRequired", err)
	}
	// Cleanup runs in the background; the reservation clears when it finishes.
	waitFor(t, 10*time.Second, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.window == nil
	}, "the window reservation was never released after a refused open")

	sys.set(func(f *fakeSystem) { f.shieldFails = false })
	if err := c.OpenWindow("turn-2"); err != nil {
		t.Fatalf("a later turn could not open after a refused open: %v", err)
	}
}

// Stop must not return while a relock is still running: the process exits
// immediately afterwards, so an early return leaves the Mac unlocked.
func TestStopWaitsForAnInFlightRelock(t *testing.T) {
	sys := newFakeSystem()
	c := newTestController(t, sys, nil)
	if err := c.OpenWindow("turn-1"); err != nil {
		t.Fatalf("OpenWindow: %v", err)
	}
	// Make the watcher start its own teardown, then race Stop against it.
	sys.set(func(f *fakeSystem) { f.idle = 0 })
	c.Stop()
	if !sys.isLocked() {
		t.Fatal("Stop returned while the screen was still unlocked")
	}
	if _, open := c.WindowOpen(); open {
		t.Error("a window is still open after Stop")
	}
}

// A device that explicitly opted out of the shield must still be able to
// capture; otherwise the opt-out silently disables the whole feature.
func TestCaptureAllowedWhenShieldExplicitlyDisabled(t *testing.T) {
	sys := newFakeSystem()
	off := false
	c := newTestController(t, sys, func(cu *config.ComputerUseConfig) {
		cu.LockedUse.RequireDisplayShield = &off
	})
	if err := c.OpenWindow("turn-1"); err != nil {
		t.Fatalf("OpenWindow: %v", err)
	}
	sys.set(func(f *fakeSystem) { f.shieldUp = false })
	if ok, reason := c.CaptureAllowed(); !ok {
		t.Fatalf("capture refused on a device with the shield opted out: %s", reason)
	}
}

// An unset TTL must fall back to the shortest useful grant life, never the
// ceiling: a caller that forgot to specify one must not get the most
// permissive grant this code can mint.
func TestMintFallsBackToTheShortestTTL(t *testing.T) {
	signer, _ := newTestSigner(t, "mac-1")
	now := time.Now()
	for _, ttl := range []time.Duration{0, -time.Minute} {
		_, payload, err := signer.Mint("turn-1", ttl, now)
		if err != nil {
			t.Fatalf("Mint(%v): %v", ttl, err)
		}
		got := time.Unix(payload.ExpiresAt, 0).Sub(time.Unix(payload.IssuedAt, 0))
		if got != MinGrantTTL {
			t.Errorf("Mint(%v) life = %v, want %v", ttl, got, MinGrantTTL)
		}
	}
}
