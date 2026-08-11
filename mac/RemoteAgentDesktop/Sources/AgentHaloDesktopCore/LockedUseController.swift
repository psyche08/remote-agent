import Darwin
import Foundation
import SystemConfiguration

/// A signed grant is scoped to the primary console session, not merely to a
/// machine. `SCDynamicStoreCopyConsoleUser` is Apple's public API for that
/// identity and explicitly excludes sessions that were fast-user-switched out.
struct ConsoleUserIdentity: Equatable, Sendable {
    let uid: uid_t
    let username: String

    static func activeForCurrentProcess() throws -> ConsoleUserIdentity {
        var consoleUID: uid_t = 0
        var consoleGID: gid_t = 0
        guard let copiedName = SCDynamicStoreCopyConsoleUser(
            nil, &consoleUID, &consoleGID) else {
            throw GrantError("no primary console user is logged in")
        }
        let username = copiedName as String
        guard consoleUID > 0, consoleUID == getuid() else {
            throw GrantError(
                "this helper is not running as the primary console user")
        }
        guard !username.isEmpty, username.utf8.count <= 256,
              !username.utf8.contains(0) else {
            throw GrantError("the primary console username is invalid")
        }

        // Authorization's username context is the account's short name. Bind
        // the dynamic-store name to the uid's canonical passwd record before
        // signing it so an inconsistent directory-service result fails closed.
        var record = passwd()
        var result: UnsafeMutablePointer<passwd>?
        var buffer = [CChar](repeating: 0, count: 16 * 1024)
        let lookup = buffer.withUnsafeMutableBufferPointer { bytes in
            getpwuid_r(consoleUID, &record, bytes.baseAddress, bytes.count, &result)
        }
        guard lookup == 0, result != nil, let rawName = record.pw_name,
              String(cString: rawName) == username else {
            throw GrantError("the primary console identity could not be resolved")
        }
        return ConsoleUserIdentity(uid: consoleUID, username: username)
    }
}

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
    private let receiptVerifier: @Sendable (String, String) throws -> Bool
    private let pendingReceiptVerifier: @Sendable (String, String) throws -> Bool
    private let completionReceiptVerifier: @Sendable (String, String) throws -> Bool
    private let signerLoader: @Sendable (String) throws -> GrantSigner
    private let consoleUserProvider: @Sendable () throws -> ConsoleUserIdentity
    private let authorizationSettleTimeout: TimeInterval
    private let relockTimeout: TimeInterval
    private let relockRetryInterval: TimeInterval

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
    /// Set after physical input is observed during opening or an open window.
    /// Automatic unlock remains disabled until cleanup has first established a
    /// locked baseline and a later probe observes the user unlock it manually.
    private var suppressedUntilManualUnlock = false
    private var suppressionSawLockedBaseline = false
    /// Operations admitted while the ordinary, unlocked desktop has no Locked
    /// Use window. Opening reserves a window only when this is zero, closing a
    /// race where an unowned action could overlap a newly opening window.
    private var unwindowedOperations = 0
    /// Once graceful shutdown or a supervised restart begins, the socket may
    /// remain alive while cleanup/quarantine finishes. Refuse every new
    /// operation during that interval rather than reopening the surface the
    /// caller is waiting to replace.
    private var stopping = false
    private let unwindowedOperationsDone = Latch()
    /// A turn whose Locked Use window ended cannot silently fall through to
    /// ordinary unlocked-desktop authority after a lock -> unlock transition.
    /// It must explicitly open a new window (and, if locked, mint a new grant).
    /// Keep exact tombstones for the lifetime of this helper process. Dropping
    /// old entries to match the bounded audit ring would let an ended Locked
    /// Use owner eventually fall through to ordinary unlocked authority. Only
    /// the signed peer can create these owners, and each entry requires a real
    /// window lifecycle, so preserving the security invariant is preferable to
    /// a rolling cache with false negatives.
    private var retiredWindowTurns: Set<String> = []
    /// A consumed authorization whose final lock transition is uncertain is a
    /// quarantine, not a cleanly closed window. The controller stays alive,
    /// disarmed, and shielded while a background loop repeatedly withdraws the
    /// grant and relocks. Graceful termination may exit only after it resolves.
    private var quarantineActive = false
    private var quarantineRequiresUnlockObservation = false
    private var quarantineSawUnlock = false
    /// The public Authorization/AX APIs cannot causally identify a visible
    /// unlock after the exact mechanism lifecycle became ambiguous. Once a
    /// nonce proof exists but the interactor failed its ordered field/lock
    /// acknowledgement, no later unlocked sample can repair that ambiguity:
    /// it may belong to a person while this grant's own effect is still late.
    /// Keep relocking and retain the shield for the life of this process.
    private var quarantineRequiresManualRecovery = false
    private var terminationRequested = false
    private var terminationSafe = false
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
    /// A receipt means loginwindow has already been told to allow the
    /// transition. Its effect is no longer bounded by the grant's expiry, so
    /// observe it on an independent deadline before declaring it uncertain.
    private static let authorizationSettleDeadline: TimeInterval = 20
    /// How long to wait for the window server to confirm the shield. Short: on
    /// the post-unlock side this is time the desktop is live, so it is a bound
    /// on exposure, not a convenience.
    private static let shieldConfirmTimeout: TimeInterval = 2.0
    private static let auditRingSize = 64

    /// One authorized per-turn unlock window.
    private enum WindowPhase: String {
        case opening
        case open
        case closing
    }

    private final class Window {
        let turnID: String
        let openedAt: Date
        let expiresAt: Date
        /// Guarded by the controller lock.
        var phase: WindowPhase = .opening
        var openingError: Error?
        var cleanupStarted = false
        var cleanupSafe = false
        var shieldConfirmed = false
        var authorizationRequested = false
        var authorizationNonce: String?
        var authorizationPendingReceiptObserved = false
        var authorizationReceiptObserved = false
        var authorizationCompletionReceiptObserved = false
        var authorizationUICompleted = false
        var authorizationSettled = false
        /// Operations acquire a lease only while this window is `.open`.
        /// Closing changes the phase first, preventing new leases, and then
        /// waits for these already-authorized operations before relocking.
        var inFlightOperations = 0
        let operationsDone = Latch()
        /// Closes only after the opening thread has stopped issuing side
        /// effects. A closer must wait for this before relocking, otherwise a
        /// late authorization request can undo its relock.
        let openingDone = Latch()
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

    /// Thread-safe handoff between the controller's grant-preparation callback
    /// and the receipt callback owned by the system interactor. Both callbacks
    /// are independently `@Sendable`, so the payload must not live in an
    /// unsynchronized local capture. The one-way preparation flag also makes a
    /// buggy second callback fail before it can mint or rewrite a grant.
    private final class AuthorizationAttempt: @unchecked Sendable {
        private let mutex = NSLock()
        private var preparationBegan = false
        private var preparedPayload: GrantPayload?

        func beginPreparation() throws {
            mutex.lock()
            defer { mutex.unlock() }
            guard !preparationBegan else {
                throw LockedUseError.systemFailure(
                    "lock-screen grant preparation was requested more than once")
            }
            preparationBegan = true
        }

        func publish(_ payload: GrantPayload) {
            mutex.lock()
            preparedPayload = payload
            mutex.unlock()
        }

        var began: Bool {
            mutex.lock()
            defer { mutex.unlock() }
            return preparationBegan
        }

        var payload: GrantPayload? {
            mutex.lock()
            defer { mutex.unlock() }
            return preparedPayload
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

    public convenience init(
        config: ComputerUseConfig, deviceID: String, system: LockedUseSystem
    ) {
        self.init(
            config: config, deviceID: deviceID, system: system,
            receiptVerifier: { nonce, directory in
                try GrantStore.receiptMatches(nonce: nonce, directory: directory)
            },
            pendingReceiptVerifier: { nonce, directory in
                try GrantStore.pendingReceiptMatches(
                    nonce: nonce, directory: directory)
            },
            completionReceiptVerifier: { nonce, directory in
                try GrantStore.completionReceiptMatches(
                    nonce: nonce, directory: directory)
            },
            signerLoader: { deviceID in
                try GrantSigner.loadSecure(deviceID: deviceID)
            },
            consoleUserProvider: { try ConsoleUserIdentity.activeForCurrentProcess() })
    }

    /// Test seam for the root-owned receipt boundary. Production always uses
    /// `GrantStore.receiptMatches`; unit tests cannot create root-owned files
    /// and inject a verifier over the fake system's consumed nonce instead.
    init(
        config: ComputerUseConfig, deviceID: String, system: LockedUseSystem,
        receiptVerifier: @escaping @Sendable (String, String) throws -> Bool,
        pendingReceiptVerifier: @escaping @Sendable (String, String) throws -> Bool = {
            _, _ in false
        },
        completionReceiptVerifier: @escaping @Sendable (String, String) throws -> Bool = {
            _, _ in false
        },
        signerLoader: (@Sendable (String) throws -> GrantSigner)? = nil,
        consoleUserProvider: @escaping @Sendable () throws -> ConsoleUserIdentity = {
            ConsoleUserIdentity(uid: getuid(), username: NSUserName())
        },
        authorizationSettleTimeout: TimeInterval =
            LockedUseController.authorizationSettleDeadline,
        relockTimeout: TimeInterval = LockedUseController.relockDeadline,
        relockRetryInterval: TimeInterval = LockedUseController.relockRetryInterval
    ) {
        let normalized = config.normalized()
        self.config = normalized
        self.deviceID = deviceID
        self.system = system
        self.receiptVerifier = receiptVerifier
        self.pendingReceiptVerifier = pendingReceiptVerifier
        self.completionReceiptVerifier = completionReceiptVerifier
        self.consoleUserProvider = consoleUserProvider
        self.authorizationSettleTimeout = authorizationSettleTimeout
        self.relockTimeout = relockTimeout
        self.relockRetryInterval = relockRetryInterval
        let directory = normalized.lockedUse.grantDirectory.isEmpty
            ? defaultGrantDirectory
            : normalized.lockedUse.grantDirectory
        self.grantDirectory = directory
        self.ledgerDirectory = (directory as NSString).appendingPathComponent("consumed")
        if let signerLoader {
            self.signerLoader = signerLoader
        } else {
            // Unit-test default only. Production enters through the public
            // convenience initializer above and always uses the AgentHalo
            // file-based login Keychain; no plaintext import path is accepted.
            let testKeyPath = (directory as NSString).appendingPathComponent(".test-signing.key")
            self.signerLoader = { deviceID in
                try GrantSigner.loadOrCreate(path: testKeyPath, deviceID: deviceID)
            }
        }
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
        guard !deviceID.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty else {
            setArmError("device_id is required for locked use")
            return
        }
        let loaded: GrantSigner
        do {
            loaded = try signerLoader(deviceID)
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
    @discardableResult
    public func stop() -> Bool {
        lock.lock()
        stopping = true
        active = false
        terminationRequested = true
        let waitForOrdinaryOperations = unwindowedOperations > 0
        lock.unlock()

        if waitForOrdinaryOperations { unwindowedOperationsDone.wait() }
        _ = closeWindow(reason: "shutdown")
        stopLatch.close()

        // A disabled controller never published a grant or requested an
        // unlock. It exists in configured mode solely so the router can refuse
        // computer use instead of falling back to an unguarded desktop path.
        guard config.lockedUse.enabled else {
            lock.lock()
            terminationSafe = true
            lock.unlock()
            return true
        }

        lock.lock()
        let alreadyQuarantined = quarantineActive
        lock.unlock()
        if alreadyQuarantined { return false }

        // Even with no registered window, graceful exit establishes the same
        // fail-closed boundary: no grant remains and the screen is confirmed
        // locked. Otherwise process exit would silently tear down AppKit's
        // shield and defeat cleanup's "held up" guarantee.
        var grantWithdrawn = true
        do {
            try GrantStore.remove(from: grantDirectory)
        } catch {
            grantWithdrawn = false
            disarmAfterGrantFailure(error, turnID: nil)
        }
        let relocked = relockVerified()
        if grantWithdrawn && relocked {
            markTerminationSafe()
            return true
        }

        enterQuarantine(
            turnID: nil,
            reason: "graceful termination could not establish a safe locked baseline",
            requiresUnlockObservation: false,
            requiresManualRecovery: !grantWithdrawn)
        return false
    }

    /// Polled by the executable after `stop()` returned false. The main AppKit
    /// run loop and shield remain alive until quarantine proves it is safe for
    /// the process to exit.
    public var isSafeToExit: Bool {
        lock.lock()
        defer { lock.unlock() }
        return terminationSafe
    }

    private func scrub() throws {
        try GrantStore.scrub(directory: grantDirectory)
        NonceLedger.prune(directory: ledgerDirectory, now: Date())
        // Command an unconditional relock. An already-locked machine is the
        // normal case and this is a no-op; a machine left unlocked by a crash
        // is exactly what this call is for. macOS applies
        // `SACLockScreenImmediate` asynchronously: a successful return means
        // the request was accepted, not that CGSession already exposes the
        // locked bit. Use the same bounded, read-back-verified relock loop as
        // cleanup instead of turning that normal transition delay into a
        // permanent startup disarm.
        guard relockVerified() else {
            // Where the system layer is unavailable, Locked Use cannot be
            // operated safely at all: refuse to arm rather than arming with an
            // unverifiable baseline.
            throw LockedUseError.systemFailure(
                "could not establish a locked baseline before the relock deadline")
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
        if stopping {
            lock.unlock()
            throw LockedUseError.notArmed("the computer-use controller is stopping")
        }
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
        refreshManualUnlockSuppression()
        guard config.enabled else { throw LockedUseError.notEnabled }
        guard config.lockedUse.enabled else { throw LockedUseError.lockedUseNotEnabled }
        lock.lock()
        let (isArmed, isActive, failure, suppressed, isStopping) = (
            armed, active, armError, suppressedUntilManualUnlock, stopping)
        lock.unlock()
        guard !isStopping else {
            throw LockedUseError.notArmed("the computer-use controller is stopping")
        }
        guard isArmed else { throw LockedUseError.notArmed(failure) }
        guard isActive else { throw LockedUseError.lockedUseNotEnabled }
        guard !suppressed else { throw LockedUseError.localInput }
    }

    // MARK: - Window

    /// Opens one per-turn window. A same-turn retry that arrives during
    /// `.opening` waits for the original result; it never reports success merely
    /// because a reservation exists.
    public func openWindow(turnID: String) throws {
        guard !turnID.isEmpty else {
            throw LockedUseError.systemFailure("turn_id is required")
        }
        try lockedUseReady()

        let now = Date()
        lock.lock()
        if stopping {
            lock.unlock()
            throw LockedUseError.notArmed("the computer-use controller is stopping")
        }
        if let existing = window {
            let phase = existing.phase
            let owner = existing.turnID
            lock.unlock()
            guard owner == turnID else {
                throw LockedUseError.windowBusy(
                    phase == .closing
                        ? "the locked-use window for turn \(owner) is still closing"
                        : "another turn (\(owner)) owns the locked-use window")
            }
            switch phase {
            case .open:
                return
            case .opening:
                existing.openingDone.wait()
                try completedOpeningResult(existing)
                return
            case .closing:
                throw LockedUseError.windowBusy(
                    "the locked-use window for turn \(owner) is still closing")
            }
        }

        guard unwindowedOperations == 0 else {
            lock.unlock()
            throw LockedUseError.windowBusy(
                "an ordinary desktop operation is still in flight")
        }

        let minter = signer
        let opened = Window(
            turnID: turnID, openedAt: now,
            expiresAt: now.addingTimeInterval(
                TimeInterval(config.lockedUse.windowTTLSeconds)))
        retiredWindowTurns.remove(turnID)
        window = opened
        lock.unlock()

        do {
            try requireIdle(opened)
            try ensureOpening(opened)

            let lockedAtOpen: Bool
            do {
                lockedAtOpen = try system.isLocked()
            } catch {
                throw LockedUseError.systemFailure(
                    "could not read lock state before opening: \(error)")
            }
            try ensureOpening(opened)

            if config.lockedUse.shieldRequired {
                do {
                    try system.engageShield()
                } catch {
                    throw LockedUseError.shieldRequired("\(error)")
                }
                if !lockedAtOpen,
                   !system.confirmShieldCoverage(timeout: Self.shieldConfirmTimeout) {
                    throw LockedUseError.shieldRequired("")
                }
            }

            // Start enforcing local input as soon as the safeguard is in place,
            // including while loginwindow authorization is still in flight.
            DispatchQueue.global(qos: .userInitiated).async { [weak self] in
                self?.watchWindow(opened)
            }
            try ensureOpening(opened)

            if lockedAtOpen {
                guard let minter else { throw GrantError.noSigningKey }
                let attempt = AuthorizationAttempt()
                var requestFailure: Error?
                do {
                    try system.requestUnlockAuthorization(
                        authorizationFieldReady: {
                            self.audit(
                                event: "authorization_field_ready", turnID: turnID)
                        },
                        prepareGrant: {
                            try attempt.beginPreparation()
                            // Wake/discovery may have taken seconds. Revalidate
                            // every owner, presence, and console-session
                            // boundary at the last pre-submission point, then
                            // mint from now so that wait consumes no grant TTL.
                            try self.ensureOpening(opened)
                            try self.ensureNoLocalInput(opened)
                            guard try self.system.isLocked() else {
                                throw LockedUseError.systemFailure(
                                    "the screen unlocked before grant publication")
                            }
                            let consoleUser: ConsoleUserIdentity
                            do {
                                consoleUser = try self.consoleUserProvider()
                            } catch {
                                throw LockedUseError.systemFailure(
                                    "could not bind authorization to the active console user: \(error)")
                            }
                            guard consoleUser.uid == getuid(), consoleUser.uid > 0 else {
                                throw LockedUseError.systemFailure(
                                    "this helper is not the active console user's process")
                            }
                            let minted = try minter.mint(
                                turnID: turnID,
                                ttl: TimeInterval(self.config.lockedUse.grantTTLSeconds),
                                now: Date(), consoleUID: consoleUser.uid,
                                consoleUsername: consoleUser.username)
                            try self.ensureOpening(opened)
                            try self.ensureNoLocalInput(opened)
                            self.lock.lock()
                            opened.authorizationNonce = minted.1.nonce
                            self.lock.unlock()
                            try GrantStore.write(minted.0, to: self.grantDirectory)
                            attempt.publish(minted.1)
                            self.lock.lock()
                            opened.authorizationRequested = true
                            self.lock.unlock()
                            self.audit(
                                event: "grant_published", turnID: turnID,
                                noncePrefix: Self.noncePrefix(minted.1.nonce))
                        },
                        completionReceiptObserved: {
                            guard let payload = attempt.payload else {
                                throw LockedUseError.systemFailure(
                                    "authorization receipt was requested before grant publication")
                            }
                            return try self.completionReceiptVerifier(
                                payload.nonce, self.grantDirectory)
                        })
                    lock.lock()
                    opened.authorizationUICompleted = true
                    lock.unlock()
                    audit(event: "authorization_ui_completed", turnID: turnID)
                } catch {
                    requestFailure = LockedUseError.systemFailure(
                        "could not request lock-screen authorization: \(error)")
                }
                audit(
                    event: "authorization_request_returned", turnID: turnID,
                    reason: requestFailure.map {
                        Self.sanitizedAuthorizationFailureReason(
                            $0, nonce: attempt.payload?.nonce)
                    })

                guard let payload = attempt.payload else {
                    // Discovery/focus failure never published a grant and may
                    // return directly. If preparation began, scrub anyway: a
                    // failed write can be partially visible even though no
                    // payload reached the receipt observer.
                    if attempt.began { try withdrawGrant(turnID: turnID) }
                    throw requestFailure ?? LockedUseError.systemFailure(
                        "lock-screen authorization returned without preparing a grant")
                }

                // Even a throwing interactor may have crossed the AX boundary
                // before learning it could not finish. Observe the exact
                // receipt/lock result before cleanup so that transition cannot
                // arrive after our relock.
                try awaitUnlock(window: opened, payload: payload)
                try withdrawGrant(turnID: turnID)
                if let requestFailure { throw requestFailure }
                audit(
                    event: "grant_consumed", turnID: turnID,
                    noncePrefix: Self.noncePrefix(payload.nonce))

                if config.lockedUse.shieldRequired,
                   !system.confirmShieldCoverage(timeout: Self.shieldConfirmTimeout) {
                    throw LockedUseError.shieldRequired(
                        "the shield was not covering after the unlock")
                }
            }

            try ensureNoLocalInput(opened)
            try finishOpening(opened)
        } catch {
            failOpening(opened, error: error)
            throw error
        }

        audit(event: "window_opened", turnID: turnID)
    }

    /// A retry waiting on the first opener receives that opener's actual
    /// outcome. The stored error contains no grant material.
    private func completedOpeningResult(_ w: Window) throws {
        lock.lock()
        let phase = w.phase
        let failure = w.openingError
        lock.unlock()
        if phase == .open { return }
        if let failure { throw failure }
        throw LockedUseError.windowBusy(
            "the locked-use window for turn \(w.turnID) did not finish opening")
    }

    private func ensureOpening(_ w: Window) throws {
        lock.lock()
        let valid = window === w && w.phase == .opening && !w.cancelled.isClosed
        lock.unlock()
        guard valid else { throw openingCancelled(w) }
    }

    private func openingCancelled(_ w: Window) -> LockedUseError {
        .windowBusy("the locked-use window for turn \(w.turnID) was cancelled while opening")
    }

    /// Waits for both halves of the authorization result: the screen retracted
    /// its lock and the privileged plug-in acknowledged this exact nonce.
    ///
    /// Once authorization has been requested, cancellation withdraws the grant
    /// but continues observing the transaction to a terminal boundary. Returning
    /// early and relocking would allow a late unlock to land after cleanup.
    private func awaitUnlock(window w: Window, payload: GrantPayload) throws {
        let grantDeadline = Date(timeIntervalSince1970: TimeInterval(payload.expiresAt))
        var privilegedProofObservedAt: Date?
        var finalReceiptObserved = false
        var completionReceiptObserved = false
        var cancellationFailure: Error?
        var cancelledGrantWithdrawn = false

        while true {
            if cancellationFailure == nil {
                if let inputError = localInputViolation(w) {
                    if inputError as? LockedUseError == .localInput {
                        recordLocalInputSuppression(turnID: w.turnID)
                    }
                    w.cancelled.close()
                    cancellationFailure = inputError
                } else {
                    lock.lock()
                    let cancelled = window !== w || w.phase != .opening
                        || w.cancelled.isClosed
                    lock.unlock()
                    if cancelled { cancellationFailure = openingCancelled(w) }
                }
            }

            if cancellationFailure != nil, !cancelledGrantWithdrawn {
                try withdrawGrant(turnID: w.turnID)
                cancelledGrantWithdrawn = true
            }

            let pendingNow: Bool
            let finalNow: Bool
            let completionNow: Bool
            do {
                pendingNow = try pendingReceiptVerifier(payload.nonce, grantDirectory)
                finalNow = try receiptVerifier(payload.nonce, grantDirectory)
                completionNow = try completionReceiptVerifier(
                    payload.nonce, grantDirectory)
            } catch {
                throw LockedUseError.systemFailure(
                    "could not verify authorization proof: \(error)")
            }
            if pendingNow {
                lock.lock()
                let firstPending = !w.authorizationPendingReceiptObserved
                w.authorizationPendingReceiptObserved = true
                lock.unlock()
                if firstPending {
                    audit(
                        event: "authorization_pending_observed", turnID: w.turnID,
                        noncePrefix: Self.noncePrefix(payload.nonce))
                }
            }
            if finalNow {
                finalReceiptObserved = true
                lock.lock()
                let firstFinal = !w.authorizationReceiptObserved
                w.authorizationReceiptObserved = true
                lock.unlock()
                if firstFinal {
                    audit(
                        event: "authorization_receipt_observed", turnID: w.turnID,
                        noncePrefix: Self.noncePrefix(payload.nonce))
                }
            }
            if completionNow {
                completionReceiptObserved = true
                lock.lock()
                let firstCompletion = !w.authorizationCompletionReceiptObserved
                w.authorizationCompletionReceiptObserved = true
                lock.unlock()
                if firstCompletion {
                    audit(
                        event: "authorization_completion_observed", turnID: w.turnID,
                        noncePrefix: Self.noncePrefix(payload.nonce))
                }
            }
            if (pendingNow || finalNow || completionNow), privilegedProofObservedAt == nil {
                privilegedProofObservedAt = Date()
            }
            let stillLocked: Bool
            do {
                stillLocked = try system.isLocked()
            } catch {
                throw LockedUseError.systemFailure("could not read lock state: \(error)")
            }

            lock.lock()
            let uiCompleted = w.authorizationUICompleted
            lock.unlock()
            if uiCompleted && finalReceiptObserved
                && completionReceiptObserved && !stillLocked
            {
                lock.lock()
                w.authorizationSettled = true
                lock.unlock()
            }
            if uiCompleted && finalReceiptObserved
                && completionReceiptObserved && !stillLocked
            {
                if let cancellationFailure { throw cancellationFailure }
                return
            }
            let now = Date()
            if let privilegedProofObservedAt,
               now.timeIntervalSince(privilegedProofObservedAt)
                    > authorizationSettleTimeout {
                if let cancellationFailure { throw cancellationFailure }
                throw LockedUseError.systemFailure(
                    finalReceiptObserved
                        ? "authorization proof arrived but its UI transaction did not safely settle"
                        : "authorization became pending but no final receipt arrived")
            }
            if privilegedProofObservedAt == nil, now > grantDeadline {
                audit(event: "authorization_proof_expired", turnID: w.turnID)
                if let cancellationFailure { throw cancellationFailure }
                throw LockedUseError.systemFailure(
                    "no exact authorization receipt arrived before the grant expired")
            }
            _ = stopLatch.wait(timeout: Self.inputPollInterval)
        }
    }

    private func finishOpening(_ w: Window) throws {
        lock.lock()
        guard window === w, w.phase == .opening, !w.cancelled.isClosed else {
            lock.unlock()
            throw openingCancelled(w)
        }
        w.phase = .open
        // Both shield-required paths synchronously confirmed coverage before
        // reaching here. Do not call into AppKit while holding this lock.
        w.shieldConfirmed = true
        w.openingDone.close()
        lock.unlock()
    }

    /// Publishes the failed result before starting cleanup. A concurrent closer
    /// owns cleanup if it already moved the phase to `.closing`; otherwise this
    /// opener schedules it. Exactly one thread relocks and releases the window.
    private func failOpening(_ w: Window, error: Error) {
        let reason = "\(error)"
        var ownsCleanup = false
        var operationsAreDone = false
        lock.lock()
        w.openingError = error
        if window === w, w.phase == .opening {
            w.phase = .closing
            operationsAreDone = w.inFlightOperations == 0
            if !w.cleanupStarted {
                w.cleanupStarted = true
                ownsCleanup = true
            }
        }
        w.cancelled.close()
        w.openingDone.close()
        lock.unlock()
        if operationsAreDone { w.operationsDone.close() }
        audit(event: "open_failed", turnID: w.turnID, reason: reason)

        if ownsCleanup {
            DispatchQueue.global(qos: .userInitiated).async { [self] in
                cleanupAndRelease(w, reason: reason)
            }
        }
    }

    private func releaseWindow(_ w: Window) {
        lock.lock()
        if window === w {
            window = nil
            retiredWindowTurns.insert(w.turnID)
        }
        lock.unlock()
    }

    private enum WindowViolation {
        case localInput
        case safeguard(String)

        var reason: String {
            switch self {
            case .localInput: return "local input detected"
            case .safeguard(let reason): return reason
            }
        }
    }

    /// Enforces safeguards during both `.opening` and `.open`.
    private func watchWindow(_ w: Window) {
        while true {
            if w.cancelled.isClosed { return }
            if stopLatch.isClosed {
                closeWindowIf(w, reason: "shutdown")
                return
            }
            if let violation = windowViolation(w) {
                if case .localInput = violation {
                    recordLocalInputSuppression(turnID: w.turnID)
                }
                closeWindowIf(w, reason: violation.reason)
                return
            }
            if w.cancelled.wait(timeout: Self.inputPollInterval) { return }
        }
    }

    private func windowViolation(_ w: Window) -> WindowViolation? {
        if Date() > w.expiresAt { return .safeguard("window ttl expired") }
        if let error = localInputViolation(w) {
            if error as? LockedUseError == .localInput { return .localInput }
            return .safeguard("input monitor unavailable")
        }
        lock.lock()
        let phase = w.phase
        lock.unlock()
        if phase == .open {
            do {
                if try system.isLocked() {
                    return .safeguard("screen locked during the computer-use window")
                }
            } catch {
                return .safeguard("lock state became unavailable")
            }
        }
        if config.lockedUse.shieldRequired && !system.shieldEngaged() {
            return .safeguard("display shield dropped")
        }
        return nil
    }

    /// Returns `.localInput` for a reset idle counter, or a system failure when
    /// the monitor cannot answer. Every caller fails closed on either result.
    private func localInputViolation(_ w: Window) -> Error? {
        // The event tap is authoritative for input it suppressed. Relying only
        // on the idle clock can misattribute a person's event that arrives in
        // the short wake produced by an immediately preceding agent event.
        if system.physicalInputObserved() { return LockedUseError.localInput }
        let idle: TimeInterval
        do {
            idle = try system.sinceLastInput()
        } catch {
            return LockedUseError.systemFailure(
                "could not read local input state: \(error)")
        }
        if idle < Date().timeIntervalSince(w.openedAt) {
            return LockedUseError.localInput
        }
        return nil
    }

    private func ensureNoLocalInput(_ w: Window) throws {
        if let error = localInputViolation(w) {
            if error as? LockedUseError == .localInput {
                recordLocalInputSuppression(turnID: w.turnID)
            }
            throw error
        }
    }

    private func requireIdle(_ w: Window) throws {
        let idle: TimeInterval
        do {
            idle = try system.sinceLastInput()
        } catch {
            throw LockedUseError.systemFailure(
                "could not read local input state: \(error)")
        }
        if idle < TimeInterval(config.lockedUse.inputRelockGraceMs) / 1000 {
            recordLocalInputSuppression(turnID: w.turnID)
            throw LockedUseError.localInput
        }
    }

    private func recordLocalInputSuppression(turnID: String) {
        lock.lock()
        let newlySuppressed = !suppressedUntilManualUnlock
        suppressedUntilManualUnlock = true
        suppressionSawLockedBaseline = false
        lock.unlock()
        if newlySuppressed {
            audit(
                event: "automatic_unlock_suppressed", turnID: turnID,
                reason: "local input; waiting for manual unlock")
        }
    }

    /// Suppression cannot clear on the automatic unlock from the turn that
    /// caused it. Cleanup first records a verified locked baseline; only a later
    /// unlocked observation with no registered window counts as manual recovery.
    private func refreshManualUnlockSuppression() {
        lock.lock()
        let mayRecover = suppressedUntilManualUnlock
            && suppressionSawLockedBaseline && window == nil
        lock.unlock()
        guard mayRecover, let locked = try? system.isLocked(), !locked else { return }

        lock.lock()
        let recovered = suppressedUntilManualUnlock
            && suppressionSawLockedBaseline && window == nil
        if recovered {
            suppressedUntilManualUnlock = false
            suppressionSawLockedBaseline = false
        }
        lock.unlock()
        if recovered { audit(event: "manual_unlock_observed") }
    }

    @discardableResult
    public func closeWindow(reason: String) -> Bool {
        lock.lock()
        let current = window
        lock.unlock()
        guard let current else { return true }
        return closeWindowIf(current, reason: reason)
    }

    public func closeWindow(forTurn turnID: String, reason: String) throws {
        lock.lock()
        let current = window
        lock.unlock()
        guard let current else { throw LockedUseError.noWindow }
        guard current.turnID == turnID else {
            throw LockedUseError.windowBusy(
                "another turn (\(current.turnID)) owns the locked-use window")
        }
        guard closeWindowIf(current, reason: reason) else {
            throw LockedUseError.systemFailure(
                "locked-use cleanup entered quarantine; the shield remains engaged")
        }
    }

    @discardableResult
    private func closeWindowIf(_ w: Window, reason: String) -> Bool {
        var ownsCleanup = false
        var operationsAreDone = false
        lock.lock()
        if window === w, w.phase != .closing {
            w.phase = .closing
            operationsAreDone = w.inFlightOperations == 0
            if !w.cleanupStarted {
                w.cleanupStarted = true
                ownsCleanup = true
            }
        }
        lock.unlock()
        if operationsAreDone { w.operationsDone.close() }
        w.cancelled.close()

        guard ownsCleanup else {
            w.done.wait()
            lock.lock()
            let safe = w.cleanupSafe
            lock.unlock()
            return safe
        }
        return cleanupAndRelease(w, reason: reason)
    }

    @discardableResult
    private func cleanupAndRelease(_ w: Window, reason: String) -> Bool {
        // These two waits are the lifetime barrier: no authorization side
        // effect and no operation admitted by this window may land after the
        // relock below.
        w.openingDone.wait()
        w.operationsDone.wait()
        let safe = cleanup(w, reason: reason)
        lock.lock()
        w.cleanupSafe = safe
        lock.unlock()
        releaseWindow(w)
        w.done.close()
        return safe
    }

    /// Restores the locked baseline. A grant withdrawal failure disarms Locked
    /// Use and keeps the shield up even if relock succeeds, because a surviving
    /// grant could authorize another transition after cleanup.
    private func cleanup(_ w: Window, reason: String) -> Bool {
        var grantWithdrawn = true
        var proofReadSafe = true
        do {
            try GrantStore.remove(from: grantDirectory)
        } catch {
            grantWithdrawn = false
            disarmAfterGrantFailure(error, turnID: w.turnID)
        }
        if grantWithdrawn {
            do {
                try refreshAuthorizationProofs(w)
            } catch {
                proofReadSafe = false
                let message = "authorization proof could not be verified after withdrawal: \(error)"
                lock.lock()
                armed = false
                armError = message
                lock.unlock()
                audit(event: "authorization_proof_failed", turnID: w.turnID, reason: message)
            }
        }

        if !proofReadSafe {
            enterQuarantine(
                turnID: w.turnID,
                reason: reason + "; authorization proof state is unknown",
                requiresUnlockObservation: true,
                requiresManualRecovery: true)
            return false
        }

        lock.lock()
        let proofObserved = w.authorizationPendingReceiptObserved
            || w.authorizationReceiptObserved
            || w.authorizationCompletionReceiptObserved
        let exactLifecycleCompleted = w.authorizationUICompleted
            && w.authorizationReceiptObserved
            && w.authorizationCompletionReceiptObserved
        lock.unlock()
        if proofObserved && !exactLifecycleCompleted {
            enterQuarantine(
                turnID: w.turnID,
                reason: reason
                    + "; authorization proof exists without a trusted UI lifecycle completion",
                requiresUnlockObservation: true,
                requiresManualRecovery: true)
            return false
        }

        let relocked = relockVerified()
        if relocked {
            lock.lock()
            if suppressedUntilManualUnlock { suppressionSawLockedBaseline = true }
            lock.unlock()
        }

        guard grantWithdrawn, relocked else {
            if !relocked {
                let message = "relock could not be verified; shield held up"
                lock.lock()
                armed = false
                armError = message
                lock.unlock()
                audit(
                    event: "relock_failed", turnID: w.turnID,
                    reason: reason + "; shield held up, screen may still be unlocked")
            }
            lock.lock()
            let authorizationWasRequested = w.authorizationRequested
            lock.unlock()
            enterQuarantine(
                turnID: w.turnID, reason: reason,
                requiresUnlockObservation: !grantWithdrawn && authorizationWasRequested,
                requiresManualRecovery: !grantWithdrawn && authorizationWasRequested)
            return false
        }

        if config.lockedUse.shieldRequired || system.shieldEngaged() {
            do {
                try system.releaseShield()
            } catch {
                audit(event: "shield_release_failed", turnID: w.turnID, reason: "\(error)")
            }
        }
        audit(event: "window_closed", turnID: w.turnID, reason: reason)
        return true
    }

    /// Re-read every privileged phase after grant withdrawal acquired the
    /// exclusive fd lock. This catches a verifier that had already opened the
    /// grant just before the ordinary polling deadline: it must finish pending
    /// and final publication before `GrantStore.remove` can return.
    private func refreshAuthorizationProofs(_ w: Window) throws {
        lock.lock()
        let nonce = w.authorizationNonce
        lock.unlock()
        guard let nonce else { return }

        let pending = try pendingReceiptVerifier(nonce, grantDirectory)
        let final = try receiptVerifier(nonce, grantDirectory)
        let completion = try completionReceiptVerifier(nonce, grantDirectory)
        lock.lock()
        let firstPending = pending && !w.authorizationPendingReceiptObserved
        let firstFinal = final && !w.authorizationReceiptObserved
        let firstCompletion = completion
            && !w.authorizationCompletionReceiptObserved
        w.authorizationPendingReceiptObserved =
            w.authorizationPendingReceiptObserved || pending
        w.authorizationReceiptObserved = w.authorizationReceiptObserved || final
        w.authorizationCompletionReceiptObserved =
            w.authorizationCompletionReceiptObserved || completion
        lock.unlock()
        if firstPending {
            audit(
                event: "authorization_pending_observed", turnID: w.turnID,
                noncePrefix: Self.noncePrefix(nonce))
        }
        if firstFinal {
            audit(
                event: "authorization_receipt_observed", turnID: w.turnID,
                noncePrefix: Self.noncePrefix(nonce))
        }
        if firstCompletion {
            audit(
                event: "authorization_completion_observed", turnID: w.turnID,
                noncePrefix: Self.noncePrefix(nonce))
        }
    }

    private func withdrawGrant(turnID: String) throws {
        do {
            try GrantStore.remove(from: grantDirectory)
        } catch {
            disarmAfterGrantFailure(error, turnID: turnID)
            throw LockedUseError.systemFailure("could not withdraw unlock grant: \(error)")
        }
    }

    private func disarmAfterGrantFailure(_ error: Error, turnID: String?) {
        let message = "grant withdrawal failed: \(error)"
        lock.lock()
        armed = false
        armError = message
        lock.unlock()
        audit(event: "grant_revoke_failed", turnID: turnID, reason: message)
    }

    /// Keeps the helper and its shield alive while an unsafe cleanup is
    /// repaired. When the plug-in receipt was seen, a currently locked screen
    /// is not enough: loginwindow may still apply that allowed transition, so
    /// quarantine must first observe the late unlock and then relock it.
    private func enterQuarantine(
        turnID: String?, reason: String, requiresUnlockObservation: Bool,
        unlockAlreadyObserved: Bool = false,
        requiresManualRecovery: Bool = false
    ) {
        if !system.shieldEngaged() {
            do {
                try system.engageShield()
            } catch {
                audit(
                    event: "quarantine_shield_failed", turnID: turnID,
                    reason: "\(error)")
            }
        }

        var startLoop = false
        lock.lock()
        if !quarantineActive {
            quarantineActive = true
            quarantineSawUnlock = unlockAlreadyObserved
            startLoop = true
        } else if unlockAlreadyObserved {
            quarantineSawUnlock = true
        }
        quarantineRequiresUnlockObservation =
            quarantineRequiresUnlockObservation || requiresUnlockObservation
        quarantineRequiresManualRecovery =
            quarantineRequiresManualRecovery || requiresManualRecovery
        armed = false
        armError = "locked-use cleanup is quarantined: \(reason)"
        if quarantineRequiresManualRecovery {
            armError += "; automatic recovery is disabled; a controlled reboot or operator recovery is required"
        }
        terminationSafe = false
        lock.unlock()
        audit(event: "quarantine_entered", turnID: turnID, reason: reason)

        if startLoop {
            DispatchQueue.global(qos: .userInitiated).async { [weak self] in
                self?.quarantineLoop(turnID: turnID)
            }
        }
    }

    private func quarantineLoop(turnID: String?) {
        while true {
            let grantWithdrawn = (try? GrantStore.remove(from: grantDirectory)) != nil

            let lockedBefore = try? system.isLocked()
            if lockedBefore == false {
                lock.lock()
                quarantineSawUnlock = true
                lock.unlock()
            }

            try? system.lock()
            let lockedAfter = try? system.isLocked()

            lock.lock()
            let resolved = quarantineActive
                && grantWithdrawn
                && lockedAfter == true
                && !quarantineRequiresManualRecovery
                && (!quarantineRequiresUnlockObservation || quarantineSawUnlock)
            if resolved {
                quarantineActive = false
                if suppressedUntilManualUnlock { suppressionSawLockedBaseline = true }
                if terminationRequested { terminationSafe = true }
            }
            lock.unlock()

            if resolved {
                if system.shieldEngaged() {
                    do {
                        try system.releaseShield()
                    } catch {
                        audit(
                            event: "shield_release_failed", turnID: turnID,
                            reason: "quarantine resolved but shield release failed: \(error)")
                    }
                }
                audit(event: "quarantine_resolved", turnID: turnID)
                return
            }
            Thread.sleep(forTimeInterval: relockRetryInterval)
        }
    }

    private func markTerminationSafe() {
        lock.lock()
        terminationSafe = true
        lock.unlock()
        if system.shieldEngaged() { try? system.releaseShield() }
    }

    private func relockVerified() -> Bool {
        let deadline = Date().addingTimeInterval(relockTimeout)
        while true {
            if let locked = try? system.isLocked(), locked { return true }
            if (try? system.lock()) != nil,
               let locked = try? system.isLocked(), locked { return true }
            if Date() > deadline { return false }
            Thread.sleep(forTimeInterval: relockRetryInterval)
        }
    }

    /// Poll suppression recovery frequently, while retaining the slow ledger
    /// maintenance cadence.
    private func monitorLoop() {
        var lastPrune = Date()
        while !stopLatch.wait(timeout: 1) {
            refreshManualUnlockSuppression()
            let now = Date()
            if now.timeIntervalSince(lastPrune) >= 60 {
                NonceLedger.prune(directory: ledgerDirectory, now: now)
                lastPrune = now
            }
        }
    }

    // MARK: - Queries

    /// Whether a window is currently open, and for which turn.
    public func openWindowTurn() -> String? {
        lock.lock()
        defer { lock.unlock() }
        guard let window, window.phase == .open else { return nil }
        return window.turnID
    }

    /// Full registration state for lifecycle clients. `openWindowTurn()` is an
    /// operation gate and intentionally hides opening/closing; shutdown needs
    /// the opposite view so it can cancel an authorization that has not opened
    /// yet instead of leaving it to unlock after the agent exits.
    public func windowRegistration() -> (
        registered: Bool, phase: String, turnID: String
    ) {
        lock.lock()
        defer { lock.unlock() }
        guard let window else {
            return (registered: false, phase: "closed", turnID: "")
        }
        return (registered: true, phase: window.phase.rawValue, turnID: window.turnID)
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
        return window?.phase == .closing
    }

    /// Whether a legacy capture path that has no turn ownership may proceed.
    ///
    /// Owned `screen.capture` actions do not use this gate: they are admitted
    /// by `withAuthorizedTurn` and return bytes in-process. Legacy `/screenshot`
    /// and `/ocr` flows have no turn id and historically wrote a file, so they
    /// are refused for every registered Locked Use phase, even with a healthy
    /// shield. This prevents them from borrowing another turn's live desktop.
    public func captureAllowed() -> (allowed: Bool, reason: String) {
        lock.lock()
        let phase = window?.phase
        let isStopping = stopping
        let quarantined = quarantineActive
        lock.unlock()
        if isStopping {
            return (false, "the computer-use controller is stopping")
        }
        if quarantined {
            return (false, "locked-use cleanup is quarantined")
        }
        if let phase {
            return (
                false,
                "legacy capture is unavailable while a locked-use window is \(phase.rawValue)")
        }
        return (true, "")
    }

    /// Executes a validated action.
    ///
    /// When a Locked Use window is open, its owned in-memory capture is refused
    /// unless the shield is still confirmed. This is deliberately distinct
    /// from the unowned legacy-capture gate above.
    public func run(
        _ action: Action, forTurn turnID: String
    ) throws -> DesktopService.ActionResult {
        guard config.enabled else { throw LockedUseError.notEnabled }
        guard system.isAvailable else { throw LockedUseError.unsupported }
        return try withAuthorizedTurn(forTurn: turnID) {
            if action.id == .screenCapture {
                let gate = authorizedCaptureAllowed()
                guard gate.allowed else {
                    throw LockedUseError.systemFailure(gate.reason)
                }
            }
            return try system.run(action)
        }
    }

    /// Runs one operation under an atomic ownership lease. Validation and lease
    /// acquisition share the controller lock with close's phase transition, so
    /// there is no `window_state`-then-act TOCTOU. The operation itself runs
    /// outside the lock; close waits on `operationsDone` before relocking.
    func withAuthorizedTurn<T>(
        forTurn turnID: String, _ operation: () throws -> T
    ) throws -> T {
        guard !turnID.isEmpty else {
            throw LockedUseError.systemFailure("turn_id is required")
        }
        guard config.enabled else { throw LockedUseError.notEnabled }
        guard system.isAvailable else { throw LockedUseError.unsupported }

        lock.lock()
        if stopping {
            lock.unlock()
            throw LockedUseError.notArmed("the computer-use controller is stopping")
        }
        if quarantineActive {
            let failure = armError
            lock.unlock()
            throw LockedUseError.notArmed(failure)
        }

        if let current = window {
            guard current.turnID == turnID else {
                let owner = current.turnID
                lock.unlock()
                throw LockedUseError.windowBusy(
                    "another turn (\(owner)) owns the locked-use window")
            }
            guard current.phase == .open else {
                let phase = current.phase.rawValue
                lock.unlock()
                throw LockedUseError.windowBusy(
                    "the locked-use window for turn \(turnID) is \(phase)")
            }
            current.inFlightOperations += 1
            lock.unlock()

            let entryLockState: LockStateSnapshot
            do {
                entryLockState = try system.lockStateSnapshot()
            } catch {
                finishOperation(in: current)
                _ = closeWindowIf(
                    current, reason: "screen lock state became unknown")
                throw LockedUseError.systemFailure(
                    "could not verify the screen stayed unlocked: \(error)")
            }
            if entryLockState.isLocked {
                finishOperation(in: current)
                _ = closeWindowIf(
                    current, reason: "screen locked or lock state became unknown")
                throw LockedUseError.noWindow
            }

            let operationResult: Result<T, Error>
            do {
                operationResult = .success(try operation())
            } catch {
                operationResult = .failure(error)
            }
            let postOperationFailure = lockStateFailureAfterOperation(
                since: entryLockState)
            // Finish our lease before any synchronous close; cleanup waits for
            // every lease and would deadlock if this request tried to close
            // while still counting itself. The lock postcheck runs for both a
            // result and an operation error so an error cannot bypass the
            // lock-boundary retirement path.
            let leaseStillAuthorized = finishOperationAndValidateLease(in: current)
            if let postOperationFailure = postOperationFailure
                ?? (leaseStillAuthorized ? nil : LockedUseError.noWindow)
            {
                _ = closeWindowIf(
                    current,
                    reason: leaseStillAuthorized
                        ? "screen crossed a lock boundary or became unknown during an operation"
                        : "the locked-use window ended during an operation")
                throw postOperationFailure
            }
            switch operationResult {
            case .success(let result):
                return result
            case .failure(let error):
                // An AX transport timeout is an availability/safety failure,
                // not a bad selector: end the window so a hung app cannot
                // leave the shield up forever.
                if error is AccessibilityIPCError {
                    _ = closeWindowIf(
                        current, reason: "Accessibility IPC became unresponsive")
                }
                throw error
            }
        }

        if retiredWindowTurns.contains(turnID) {
            lock.unlock()
            throw LockedUseError.noWindow
        }

        // No Locked Use window exists. This is the ordinary unlocked-desktop
        // computer-use path; the signed socket peer and non-empty turn lease
        // are its authority. Reserve it before checking lock state so a window
        // cannot begin concurrently and turn it into an unowned operation.
        unwindowedOperations += 1
        lock.unlock()
        defer { finishUnwindowedOperation() }

        let entryLockState: LockStateSnapshot
        do {
            entryLockState = try system.lockStateSnapshot()
        } catch {
            throw LockedUseError.systemFailure("could not read lock state: \(error)")
        }
        guard !entryLockState.isLocked else { throw LockedUseError.noWindow }
        let operationResult: Result<T, Error>
        do {
            operationResult = .success(try operation())
        } catch {
            operationResult = .failure(error)
        }
        if let postOperationFailure = lockStateFailureAfterOperation(
            since: entryLockState)
        {
            // An ordinary unlocked turn that crossed a lock boundary must not
            // regain authority merely because the user later unlocks again.
            // The already-posted CGEvent cannot be recalled, but no result is
            // returned and the device is driven back to a known locked state.
            lock.lock()
            retiredWindowTurns.insert(turnID)
            lock.unlock()
            try? system.lock()
            throw postOperationFailure
        }
        return try operationResult.get()
    }

    private func lockStateFailureAfterOperation(
        since entry: LockStateSnapshot
    ) -> Error? {
        do {
            let exit = try system.lockStateSnapshot()
            return exit.isLocked || exit.generation != entry.generation
                ? LockedUseError.noWindow
                : nil
        } catch {
            return LockedUseError.systemFailure(
                "could not verify the screen stayed unlocked through the operation: \(error)")
        }
    }

    private func finishUnwindowedOperation() {
        var signalShutdown = false
        lock.lock()
        precondition(unwindowedOperations > 0)
        unwindowedOperations -= 1
        if stopping && unwindowedOperations == 0 { signalShutdown = true }
        lock.unlock()
        if signalShutdown { unwindowedOperationsDone.close() }
    }

    /// The owned in-memory capture path still verifies the shield immediately
    /// before entering ScreenCaptureKit. It is separate from `captureAllowed`,
    /// whose answer is intentionally always false for an unowned legacy caller
    /// while any Locked Use registration exists.
    private func authorizedCaptureAllowed() -> (allowed: Bool, reason: String) {
        lock.lock()
        let current = window
        let phase = current?.phase
        let confirmed = current?.shieldConfirmed ?? false
        lock.unlock()
        guard current != nil else { return (true, "") }
        guard phase == .open, confirmed, system.shieldEngaged() else {
            return (
                false,
                "the owned capture does not have a confirmed locked-use display shield")
        }
        return (true, "")
    }

    private func finishOperation(in w: Window) {
        var operationsAreDone = false
        lock.lock()
        precondition(w.inFlightOperations > 0)
        w.inFlightOperations -= 1
        if w.phase == .closing && w.inFlightOperations == 0 {
            operationsAreDone = true
        }
        lock.unlock()
        if operationsAreDone { w.operationsDone.close() }
    }

    /// Ends an operation lease and atomically validates that its authority did
    /// not end while the operation ran. A watcher/closer changes phase or sets
    /// `cancelled` under this same controller lock before waiting for leases,
    /// so a result cannot survive an already-observed local-input, shield, TTL,
    /// relock, shutdown, or explicit-close violation.
    private func finishOperationAndValidateLease(in w: Window) -> Bool {
        var operationsAreDone = false
        lock.lock()
        precondition(w.inFlightOperations > 0)
        let stillAuthorized = window === w
            && w.phase == .open
            && !w.cancelled.isClosed
        w.inFlightOperations -= 1
        if w.phase == .closing && w.inFlightOperations == 0 {
            operationsAreDone = true
        }
        lock.unlock()
        if operationsAreDone { w.operationsDone.close() }
        return stillAuthorized
    }

    /// Describes the feature for the console. Capability and state only — never
    /// a grant, a nonce, or key material.
    public func status() -> [String: Any] {
        refreshManualUnlockSuppression()
        lock.lock()
        let (
            isArmed, isActive, failure, suppressed, quarantined,
            requiresManualRecovery, isStopping
        ) = (
            armed, active, armError, suppressedUntilManualUnlock, quarantineActive,
            quarantineRequiresManualRecovery, stopping)
        var turnID = ""
        var open = false
        var windowState = "closed"
        if let window {
            turnID = window.turnID
            windowState = window.phase.rawValue
            open = window.phase == .open
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
            "window_state": windowState,
            "suppressed_until_manual_unlock": suppressed,
            "quarantined": quarantined,
            "requires_manual_recovery": requiresManualRecovery,
            "stopping": isStopping,
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

    /// Authorization-return reasons are useful remote diagnostics, but audit
    /// entries leave the machine. Strip control characters, the exact nonce,
    /// and any long token-shaped value before bounding the record size.
    static func sanitizedAuthorizationFailureReason(
        _ error: Error, nonce: String?
    ) -> String {
        var reason = String(describing: error)
        if let nonce, !nonce.isEmpty {
            reason = reason.replacingOccurrences(of: nonce, with: "[redacted]")
        }
        reason = reason.unicodeScalars.map { scalar in
            CharacterSet.controlCharacters.contains(scalar) ? " " : String(scalar)
        }.joined()
        if let expression = try? NSRegularExpression(
            pattern: #"(?i)\b[0-9a-f]{16,}\b|[A-Za-z0-9+_=/\-]{32,}"#)
        {
            let range = NSRange(reason.startIndex..<reason.endIndex, in: reason)
            reason = expression.stringByReplacingMatches(
                in: reason, range: range, withTemplate: "[redacted]")
        }
        return String(reason.prefix(512))
    }
}
