import Foundation

/// Owns the computer-use surface and the Locked Use state machine.
///
/// The invariant that governs every path below: **uncertainty relocks**. There
/// is no "log it and continue" branch on a safeguard failure. If the shield
/// cannot be confirmed, if the input monitor cannot answer, if the lock state
/// cannot be read — the window closes and the screen goes back to locked. A
/// safeguard whose failure is survivable is not a safeguard.
public final class LockedUseController: @unchecked Sendable {
    private let config: ComputerUseConfig
    private let deviceID: String
    private let system: LockedUseSystem
    private let grantDirectory: String
    private let ledgerDirectory: String

    private let lock = NSLock()
    private var signer: GrantSigner?
    private var window: Window?
    /// Set once the startup scrub has confirmed a clean baseline. Nothing opens
    /// a window before that.
    private var armed = false
    private var armError = ""
    /// The runtime toggle the console drives. Config is the ceiling: this can
    /// turn Locked Use off on a device that permits it, but never on where
    /// config says no. A security capability must be granted on the device, not
    /// enabled over the network.
    private var active = false
    private var auditRing: [AuditEntry] = []

    private let stopLatch = Latch()

    /// The fixed monitor cadence. Intentionally not configurable: a human at
    /// the keyboard must be noticed in tens of milliseconds, and no deployment
    /// should be able to widen that to seconds.
    private static let inputPollInterval: TimeInterval = 0.040
    /// Bounds how long cleanup retries a relock before escalating. The shield
    /// stays up for the whole attempt. Cleanup never runs on a request's path,
    /// so this is bounded for safety rather than to fit a response budget.
    private static let relockDeadline: TimeInterval = 20
    private static let relockRetryInterval: TimeInterval = 0.5
    /// How long to wait for the window server to confirm the shield. Short: on
    /// the post-unlock side this is time the desktop is live, so it is a bound
    /// on exposure, not a convenience.
    private static let shieldConfirmTimeout: TimeInterval = 2.0
    private static let auditRingSize = 64

    /// One authorized per-turn unlock window.
    private final class Window {
        let turnID: String
        let openedAt: Date
        let expiresAt: Date
        var closed = false
        /// Closes once this window's cleanup has finished. A second closer
        /// waits on it rather than returning while the relock is still in
        /// flight: "the window is closed" must mean "the screen is confirmed
        /// locked", or shutdown becomes a way to leave a Mac unlocked.
        let done = Latch()
        /// Ends the watcher for this window.
        let cancelled = Latch()

        init(turnID: String, openedAt: Date, expiresAt: Date) {
            self.turnID = turnID
            self.openedAt = openedAt
            self.expiresAt = expiresAt
        }
    }

    /// One Locked Use state transition. It deliberately carries no grant body,
    /// no nonce beyond a short prefix, and no key material: the agent's log is
    /// uploaded off-device, so anything recorded here leaves the machine.
    public struct AuditEntry: Codable, Sendable, Equatable {
        public var at: String
        public var event: String
        public var turnID: String?
        public var noncePrefix: String?
        public var reason: String?

        enum CodingKeys: String, CodingKey {
            case at, event, reason
            case turnID = "turn_id"
            case noncePrefix = "nonce_prefix"
        }

        /// The entry as plain JSON types.
        ///
        /// The status response is assembled as `[String: Any]` and serialized
        /// with JSONSerialization, which cannot encode a Swift struct: handing
        /// it one throws an Objective-C exception that Swift cannot catch, so
        /// the helper does not fail the request — it dies. That is what
        /// happened the first time Locked Use was enabled on a real device,
        /// because until an audit entry exists the array is empty and encodes
        /// fine.
        var jsonObject: [String: Any] {
            var out: [String: Any] = ["at": at, "event": event]
            if let turnID { out["turn_id"] = turnID }
            if let noncePrefix { out["nonce_prefix"] = noncePrefix }
            if let reason { out["reason"] = reason }
            return out
        }
    }

    public init(
        config: ComputerUseConfig, deviceID: String, system: LockedUseSystem
    ) {
        let normalized = config.normalized()
        self.config = normalized
        self.deviceID = deviceID
        self.system = system
        let directory = normalized.lockedUse.grantDirectory.isEmpty
            ? defaultGrantDirectory
            : normalized.lockedUse.grantDirectory
        self.grantDirectory = directory
        self.ledgerDirectory = (directory as NSString).appendingPathComponent("consumed")
    }

    // MARK: - Lifecycle

    /// Performs the startup scrub and arms Locked Use if it is configured.
    ///
    /// The scrub exists because a crash is not a clean stop: it can leave a
    /// valid grant on disk and a screen that is unlocked with nothing watching
    /// it. A restart must never inherit that state, so every grant artifact is
    /// deleted and a relock commanded before serving anything.
    public func start() {
        guard config.lockedUse.enabled else { return }
        do {
            try scrub()
        } catch {
            setArmError("startup scrub failed: \(error)")
            return
        }
        var keyPath = config.lockedUse.signingKeyPath
        if keyPath.isEmpty {
            keyPath = defaultSigningKeyPath
        }
        let loaded: GrantSigner
        do {
            loaded = try GrantSigner.loadOrCreate(path: keyPath, deviceID: deviceID)
        } catch {
            setArmError("signing key unavailable: \(error)")
            return
        }
        lock.lock()
        signer = loaded
        armed = true
        active = true
        armError = ""
        lock.unlock()
        audit(event: "armed")
        DispatchQueue.global(qos: .utility).async { [weak self] in self?.monitorLoop() }
    }

    /// Closes any open window and ends the monitor.
    ///
    /// The window is closed and its relock awaited BEFORE the stop latch is
    /// signalled. Signalling first would let the watcher win the race, begin
    /// cleanup, and clear the registration, so this call would find nothing to
    /// do and return while the relock was still running — and the process would
    /// then exit mid-relock.
    public func stop() {
        closeWindow(reason: "shutdown")
        stopLatch.close()
    }

    private func scrub() throws {
        GrantStore.scrub(directory: grantDirectory)
        NonceLedger.prune(directory: ledgerDirectory, now: Date())
        // Command an unconditional relock. An already-locked machine is the
        // normal case and this is a no-op; a machine left unlocked by a crash
        // is exactly what this call is for.
        do {
            try system.lock()
        } catch {
            // Where the system layer is unavailable, Locked Use cannot be
            // operated safely at all: refuse to arm rather than arming with an
            // unverifiable baseline.
            throw LockedUseError.systemFailure("could not establish a locked baseline: \(error)")
        }
    }

    private func setArmError(_ message: String) {
        lock.lock()
        armed = false
        armError = message
        lock.unlock()
        audit(event: "arm_failed", reason: message)
    }

    // MARK: - Gates

    public var isEnabled: Bool { config.enabled }
    public var isLockedUseEnabled: Bool { config.lockedUse.enabled }

    /// The console's runtime toggle.
    ///
    /// It can only move within what config already permits. Turning Locked Use
    /// off takes effect immediately and closes any open window; turning it on
    /// is refused unless the device's own config enabled the capability, so no
    /// remote caller can grant this device the ability to unlock itself.
    public func setLockedUseActive(_ wanted: Bool) throws {
        guard config.enabled else { throw LockedUseError.notEnabled }
        guard config.lockedUse.enabled else { throw LockedUseError.lockedUseNotEnabled }
        lock.lock()
        let wasArmed = armed
        active = wanted
        lock.unlock()
        if !wanted {
            // Clear the flag first, then close. An open that already passed the
            // ready check still holds the window reservation, so closing waits
            // for its teardown; a new open now fails the ready check outright.
            closeWindow(reason: "locked use switched off")
        }
        if wanted && !wasArmed { throw LockedUseError.notArmed("") }
        audit(event: wanted ? "activated" : "deactivated")
    }

    private func lockedUseReady() throws {
        guard config.enabled else { throw LockedUseError.notEnabled }
        guard config.lockedUse.enabled else { throw LockedUseError.lockedUseNotEnabled }
        lock.lock()
        let (isArmed, isActive, failure) = (armed, active, armError)
        lock.unlock()
        guard isArmed else { throw LockedUseError.notArmed(failure) }
        guard isActive else { throw LockedUseError.lockedUseNotEnabled }
    }

    // MARK: - Window

    /// Opens a per-turn unlock window and unlocks the screen.
    ///
    /// Idempotent per turn: a repeated call for the turn that already owns the
    /// open window extends nothing and returns success, so a client retry after
    /// a relay timeout cannot double-open or stack windows.
    ///
    /// Unlocking, shielding and relocking complete or roll back on the device's
    /// own schedule; a client disconnect must never abandon a half-open window.
    public func openWindow(turnID: String) throws {
        guard !turnID.isEmpty else {
            throw LockedUseError.systemFailure("turn_id is required")
        }
        try lockedUseReady()

        let now = Date()
        lock.lock()
        // A window is registered before any of the slow work below and stays
        // registered until its cleanup finishes. Opening involves an unlock
        // that can take seconds, so without this reservation a client retry
        // after the relay's 30s cut would sail past the check and start a
        // second concurrent unlock against the same desktop.
        if let existing = window {
            let (turn, closing) = (existing.turnID, existing.closed)
            lock.unlock()
            if turn == turnID && !closing { return }
            if closing {
                throw LockedUseError.windowBusy(
                    "the locked-use window for turn \(turn) is still closing")
            }
            throw LockedUseError.windowBusy("another turn (\(turn)) owns the locked-use window")
        }
        let minter = signer
        let opened = Window(
            turnID: turnID, openedAt: now,
            expiresAt: now.addingTimeInterval(TimeInterval(config.lockedUse.windowTTLSeconds)))
        window = opened
        lock.unlock()

        // A person at the machine outranks a remote turn. Require the device to
        // already be idle before taking it over.
        do {
            try requireIdle()
        } catch {
            abortOpen(opened, reason: "\(error)")
            throw error
        }

        // The shield goes up before anything is unlocked, never after: the gap
        // between an unlock and a shield is a window where the desktop is
        // visible to whoever is standing there.
        //
        // *Confirming* it, though, can only happen on the side of the unlock
        // where the session is actually being displayed. While the screen is
        // locked the user's session is not on screen at all, so the window
        // server reports no coverage no matter how correct the shield is —
        // requiring confirmation first made the feature unusable from exactly
        // the state it exists for: locked, nobody present, refusing with
        // "display shield could not be engaged".
        //
        // Nothing is exposed by waiting. While locked, the lock screen is
        // itself covering the session; the shield only has to be in place by
        // the moment the unlock lands, and it is raised before that here. When
        // the screen is already unlocked the desktop is visible right now, so
        // confirmation stays where it was: before anything else happens.
        let lockedAtOpen = (try? system.isLocked()) ?? true
        if config.lockedUse.shieldRequired {
            do {
                try system.engageShield()
            } catch {
                abortOpen(opened, reason: "shield: \(error)")
                throw LockedUseError.shieldRequired("\(error)")
            }
            if !lockedAtOpen, !system.confirmShieldCoverage(timeout: Self.shieldConfirmTimeout) {
                abortOpen(opened, reason: "shield not confirmed")
                throw LockedUseError.shieldRequired("")
            }
        }

        // Mint and publish a grant only now, immediately before the unlock it
        // authorizes, and withdraw it as soon as the unlock resolves. The grant
        // is never left in place for the life of the window.
        guard let minter else {
            abortOpen(opened, reason: "mint: no signing key")
            throw GrantError.noSigningKey
        }
        let minted: (Grant, GrantPayload)
        do {
            minted = try minter.mint(
                turnID: turnID,
                ttl: TimeInterval(config.lockedUse.grantTTLSeconds), now: Date())
        } catch {
            abortOpen(opened, reason: "mint: \(error)")
            throw error
        }
        do {
            try GrantStore.write(minted.0, to: grantDirectory)
        } catch {
            abortOpen(opened, reason: "publish: \(error)")
            throw error
        }
        audit(
            event: "grant_published", turnID: turnID,
            noncePrefix: Self.noncePrefix(minted.1.nonce))

        // The unlock itself is performed by macOS. This process does not supply
        // a credential; it has only asserted, verifiably, that an authorized
        // turn is asking. The Authorization Plug-in decides.
        let unlockError = awaitUnlock(payload: minted.1)
        // Withdraw the grant regardless of outcome so nothing can ride it later.
        GrantStore.remove(from: grantDirectory)
        if let unlockError {
            abortOpen(opened, reason: "unlock: \(unlockError)")
            throw unlockError
        }

        // The session is on screen now, so this is the first moment the shield
        // can be confirmed after starting from a locked screen — and the last
        // moment it is safe not to have been. A shield that is not covering
        // here means the desktop is live and readable, so the window is torn
        // down and the screen relocked at once rather than handed to the turn.
        if config.lockedUse.shieldRequired && lockedAtOpen {
            guard system.confirmShieldCoverage(timeout: Self.shieldConfirmTimeout) else {
                abortOpen(opened, reason: "shield not confirmed after unlock")
                throw LockedUseError.shieldRequired("the shield was not covering after the unlock")
            }
        }
        DispatchQueue.global(qos: .userInitiated).async { [weak self] in
            self?.watchWindow(opened)
        }
        audit(event: "window_opened", turnID: turnID)
    }

    /// Waits for the screen to actually become unlocked after a grant is
    /// published, bounded by the grant's own lifetime. A published grant that
    /// nothing consumed is not a success, and the caller must not proceed as
    /// though the desktop is reachable.
    private func awaitUnlock(payload: GrantPayload) -> Error? {
        let deadline = Date(timeIntervalSince1970: TimeInterval(payload.expiresAt))
        while true {
            do {
                if try !system.isLocked() { return nil }
            } catch {
                return LockedUseError.systemFailure("could not read lock state: \(error)")
            }
            if Date() > deadline {
                return LockedUseError.systemFailure(
                    "unlock was not authorized before the grant expired")
            }
            if stopLatch.wait(timeout: Self.inputPollInterval) {
                return LockedUseError.systemFailure("shutting down")
            }
        }
    }

    /// Tears down a window that never finished opening.
    ///
    /// The security-critical part — withdrawing the grant so nothing can ride
    /// it — happens synchronously. The verified relock then runs in the
    /// background, because it retries on a deadline that can outlast the
    /// relay's 30s HTTP timeout and the caller must not be held that long for
    /// an error it can already act on. The window stays registered until that
    /// cleanup completes, so a retry cannot open a new window while the
    /// previous one is still being unwound.
    private func abortOpen(_ w: Window, reason: String) {
        GrantStore.remove(from: grantDirectory)
        audit(event: "open_failed", turnID: w.turnID, reason: reason)
        lock.lock()
        if w.closed {
            lock.unlock()
            w.done.wait()
            return
        }
        w.closed = true
        lock.unlock()
        DispatchQueue.global(qos: .userInitiated).async { [weak self] in
            guard let self else { return }
            self.cleanup(w, reason: reason)
            self.releaseWindow(w)
            w.done.close()
        }
    }

    /// Clears a window once its cleanup has finished, letting the next open
    /// proceed.
    private func releaseWindow(_ w: Window) {
        lock.lock()
        if window === w { window = nil }
        lock.unlock()
    }

    /// Enforces the window's limits for its whole life: the hard TTL, local
    /// input, and continued shield coverage.
    private func watchWindow(_ w: Window) {
        while true {
            if w.cancelled.isClosed { return }
            if stopLatch.isClosed {
                closeWindowIf(w, reason: "shutdown")
                return
            }
            if let reason = windowViolation(w) {
                closeWindowIf(w, reason: reason)
                return
            }
            if w.cancelled.wait(timeout: Self.inputPollInterval) { return }
        }
    }

    /// Returns a reason when the window must close now. Every error path
    /// returns one: an unreadable safeguard is a violation, not a pass.
    private func windowViolation(_ w: Window) -> String? {
        if Date() > w.expiresAt { return "window ttl expired" }
        let idle: TimeInterval
        do {
            idle = try system.sinceLastInput()
        } catch {
            return "input monitor unavailable"
        }
        // Any local input since the window opened means a person is present.
        // The idle counter having reset below the window's age is that signal.
        if idle < Date().timeIntervalSince(w.openedAt) { return "local input detected" }
        if config.lockedUse.shieldRequired && !system.shieldEngaged() {
            return "display shield dropped"
        }
        return nil
    }

    /// Refuses to open a window on a machine someone is actively using.
    private func requireIdle() throws {
        let idle: TimeInterval
        do {
            idle = try system.sinceLastInput()
        } catch {
            throw LockedUseError.systemFailure("could not read local input state: \(error)")
        }
        if idle < TimeInterval(config.lockedUse.inputRelockGraceMs) / 1000 {
            throw LockedUseError.localInput
        }
    }

    /// Ends the current window for any reason.
    public func closeWindow(reason: String) {
        lock.lock()
        let current = window
        lock.unlock()
        guard let current else { return }
        closeWindowIf(current, reason: reason)
    }

    /// Ends the window only if the named turn owns it, so a finishing turn
    /// cannot relock a window another turn legitimately opened.
    public func closeWindow(forTurn turnID: String, reason: String) {
        lock.lock()
        let current = window
        lock.unlock()
        guard let current, current.turnID == turnID else { return }
        closeWindowIf(current, reason: reason)
    }

    private func closeWindowIf(_ w: Window, reason: String) {
        lock.lock()
        if w.closed {
            lock.unlock()
            // Another thread already owns this teardown. Wait for it rather
            // than reporting success while its relock is still running.
            w.done.wait()
            return
        }
        w.closed = true
        lock.unlock()
        w.cancelled.close()
        cleanup(w, reason: reason)
        // Clear the registration only after cleanup, so a new window cannot
        // open while this one is still being relocked.
        releaseWindow(w)
        w.done.close()
    }

    /// Restores the locked, unshielded baseline in a strict order: relock and
    /// confirm it first, then drop the shield, then withdraw the grant.
    ///
    /// The ordering is the point. Dropping the shield before the screen is
    /// confirmed locked would expose the live desktop at exactly the moment the
    /// agent believes it has finished cleaning up — the worst possible state.
    private func cleanup(_ w: Window, reason: String) {
        // A window that is no longer the registered one has already been
        // superseded. Relocking here would tear down whatever replaced it.
        lock.lock()
        let superseded = window != nil && window !== w
        lock.unlock()
        if superseded {
            audit(event: "cleanup_skipped", turnID: w.turnID, reason: "window superseded")
            return
        }

        // Withdrawing the grant is always safe and always first priority: it
        // stops any *new* unlock from being authorized while we work.
        GrantStore.remove(from: grantDirectory)

        if relockVerified() {
            if config.lockedUse.shieldRequired || system.shieldEngaged() {
                try? system.releaseShield()
            }
            audit(event: "window_closed", turnID: w.turnID, reason: reason)
            return
        }
        // Relock failed. Keep the shield up — an uncovered unlocked screen is
        // worse than a covered one — and record it loudly. The shield stays
        // until a later relock succeeds or an operator intervenes.
        audit(
            event: "relock_failed", turnID: w.turnID,
            reason: reason + "; shield held up, screen may still be unlocked")
    }

    /// Locks the screen and confirms it by reading the state back, retrying
    /// within a bounded deadline. A lock command that returns success is not
    /// evidence the screen is locked.
    private func relockVerified() -> Bool {
        let deadline = Date().addingTimeInterval(Self.relockDeadline)
        while true {
            if let locked = try? system.isLocked(), locked { return true }
            if (try? system.lock()) != nil {
                if let locked = try? system.isLocked(), locked { return true }
            }
            if Date() > deadline { return false }
            // Deliberately not waiting on the stop latch: a relock in progress
            // must run to its deadline even while shutting down. Cutting it
            // short is exactly how a shutdown would leave the Mac unlocked.
            Thread.sleep(forTimeInterval: Self.relockRetryInterval)
        }
    }

    /// Prunes the nonce ledger. Window enforcement lives in `watchWindow`; this
    /// loop only handles slow background maintenance.
    private func monitorLoop() {
        while !stopLatch.wait(timeout: 60) {
            NonceLedger.prune(directory: ledgerDirectory, now: Date())
        }
    }

    // MARK: - Queries

    /// Whether a window is currently open, and for which turn.
    public func openWindowTurn() -> String? {
        lock.lock()
        defer { lock.unlock() }
        guard let window, !window.closed else { return nil }
        return window.turnID
    }

    /// Whether a window is registered but already unwinding, so callers can
    /// tell "busy" from "open".
    ///
    /// The distinction is not cosmetic: a window stays registered until its
    /// relock finishes, so between `openWindowTurn() == nil` and the next open
    /// succeeding there is a real interval during which the honest answer is
    /// "still closing", not "free".
    public func isWindowClosing() -> Bool {
        lock.lock()
        defer { lock.unlock() }
        return window?.closed ?? false
    }

    /// Whether a screen capture may proceed right now.
    ///
    /// Screen capture is not suppressed while the desktop is unlocked under
    /// Locked Use, so an unshielded capture could persist and then serve the
    /// lock screen or whatever was on it. Capture is refused whenever a window
    /// is open and the shield is not confirmed up.
    public func captureAllowed() -> (allowed: Bool, reason: String) {
        lock.lock()
        let open = window != nil && !(window?.closed ?? true)
        lock.unlock()
        if !open { return (true, "") }
        // A device that explicitly opted out of the shield has accepted that
        // its desktop is visible while a window is open; refusing every capture
        // there would disable the feature rather than protect anything.
        if !config.lockedUse.shieldRequired { return (true, "") }
        if !system.shieldEngaged() {
            return (false, "a locked-use window is open without a confirmed display shield")
        }
        return (true, "")
    }

    /// Executes a validated action.
    ///
    /// When a Locked Use window is open, the same capture gate applies: an
    /// action that produces a frame is refused unless the shield is confirmed.
    public func run(_ action: Action) throws -> DesktopService.ActionResult {
        guard config.enabled else { throw LockedUseError.notEnabled }
        guard system.isAvailable else { throw LockedUseError.unsupported }
        if action.id == .screenCapture {
            let gate = captureAllowed()
            guard gate.allowed else { throw LockedUseError.systemFailure(gate.reason) }
        }
        return try system.run(action)
    }

    /// Describes the feature for the console. Capability and state only — never
    /// a grant, a nonce, or key material.
    public func status() -> [String: Any] {
        lock.lock()
        let (isArmed, isActive, failure) = (armed, active, armError)
        var turnID = ""
        var open = false
        if let window, !window.closed {
            turnID = window.turnID
            open = true
        }
        let entries = auditRing
        // The public key is the verifying half and is meant to be published: an
        // operator provisions the plug-in with it. It cannot sign a grant, so
        // exposing it grants nothing.
        let publicKey = signer?.publicKeyBase64 ?? ""
        lock.unlock()

        let lu = config.lockedUse
        var lockedUse: [String: Any] = [
            "enabled": lu.enabled,
            "armed": isArmed,
            "active": isActive,
            "window_open": open,
            "window_turn_id": turnID,
            "grant_ttl_seconds": lu.grantTTLSeconds,
            "window_ttl_seconds": lu.windowTTLSeconds,
            "require_display_shield": lu.shieldRequired,
            "shield_engaged": system.shieldEngaged(),
        ]
        if !failure.isEmpty { lockedUse["error"] = failure }
        if !publicKey.isEmpty { lockedUse["public_key"] = publicKey }
        return [
            "enabled": config.enabled,
            "available": system.isAvailable,
            "actions": ActionID.allCases.map(\.rawValue),
            "locked_use": lockedUse,
            "audit": entries.map(\.jsonObject),
        ]
    }

    /// The verifying key, so an operator can provision the plug-in. Empty when
    /// Locked Use never armed.
    public var publicKeyBase64: String {
        lock.lock()
        defer { lock.unlock() }
        return signer?.publicKeyBase64 ?? ""
    }

    public func auditEntries() -> [AuditEntry] {
        lock.lock()
        defer { lock.unlock() }
        return auditRing
    }

    private func audit(
        event: String, turnID: String? = nil, noncePrefix: String? = nil, reason: String? = nil
    ) {
        let formatter = ISO8601DateFormatter()
        formatter.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        let entry = AuditEntry(
            at: formatter.string(from: Date()), event: event, turnID: turnID,
            noncePrefix: noncePrefix, reason: reason)
        lock.lock()
        auditRing.append(entry)
        if auditRing.count > Self.auditRingSize {
            auditRing.removeFirst(auditRing.count - Self.auditRingSize)
        }
        lock.unlock()
    }

    /// Shortens a nonce for audit. A prefix is enough to correlate two records;
    /// the full value is the single-use secret and never leaves the grant.
    private static func noncePrefix(_ nonce: String) -> String? {
        guard nonce.count > 8 else { return nil }
        return String(nonce.prefix(8))
    }
}
