import Darwin
import XCTest
@testable import AgentHaloDesktopCore

/// Stands in for the device.
///
/// The important part is `grantDirectory`: the fake consumes a grant from it
/// exactly once, the way the real plug-in does. A test that observes an unlock
/// has therefore also proven the controller really published a grant to
/// authorize it, rather than a test double simply agreeing to be unlocked.
final class FakeSystem: LockedUseSystem, @unchecked Sendable {
    private let mutex = NSLock()

    private var locked = true
    private var lockStateGeneration: UInt64 = 0
    private var idle: TimeInterval = 3600
    private var shieldUp = false
    var shieldFails = false
    var lockFails = false
    var lockStateFails = false
    private var lockRequestsBeforeConfirmation = 0
    var idleFails = false
    var coverageUnconfirmable = false
    var grantDirectory = ""
    private var authorizationGate: Latch?
    private var verificationGate: Latch?
    private var pendingReceiptNonce: String?
    private var receiptNonce: String?
    private var completionReceiptNonce: String?
    private var pendingReceiptVisible = true
    private var receiptVisible = true
    private var publishFinalReceipt = true
    private var authorizationRequests = 0
    private var grantPreparationCallbacks = 0
    private var grantPreparationCallsPerRequest = 1
    private var authorizationFailureBeforePreparation: Error?
    private var authorizationFailureAfterPreparation: Error?
    private var mostRecentGrantPayload: GrantPayload?
    private var delayedUnlockGate: Latch?
    private var transactionDestroyGate: Latch?
    private var authorizationTransactionTimeout: TimeInterval = 5
    private var authorizationFieldValid = true
    private var physicalInput = false
    let authorizationStarted = Latch()
    let grantVerificationStarted = Latch()
    let pendingReceiptPublished = Latch()
    let receiptPublished = Latch()
    let completionReceiptPublished = Latch()
    let authorizationTransitionApplied = Latch()
    private var actionGate: Latch?
    private var actionError: Error?
    let actionStarted = Latch()
    private var actions: [Action] = []

    func isLocked() throws -> Bool {
        try lockStateSnapshot().isLocked
    }

    func lockStateSnapshot() throws -> LockStateSnapshot {
        mutex.lock()
        defer { mutex.unlock() }
        if lockStateFails { throw LockedUseError.systemFailure("lock state unavailable") }
        return LockStateSnapshot(
            isLocked: locked, generation: lockStateGeneration)
    }

    func lock() throws {
        mutex.lock()
        defer { mutex.unlock() }
        if lockFails { throw LockedUseError.systemFailure("lock failed") }
        if lockRequestsBeforeConfirmation > 0 {
            lockRequestsBeforeConfirmation -= 1
            return
        }
        setLockedWhileHoldingMutex(true)
    }

    func sinceLastInput() throws -> TimeInterval {
        mutex.lock()
        defer { mutex.unlock() }
        if idleFails { throw LockedUseError.systemFailure("input monitor unavailable") }
        return idle
    }

    func engageShield() throws {
        mutex.lock()
        defer { mutex.unlock() }
        if shieldFails { throw LockedUseError.systemFailure("shield failed") }
        shieldUp = true
        physicalInput = false
    }

    func releaseShield() throws {
        mutex.lock()
        defer { mutex.unlock() }
        shieldUp = false
    }

    func shieldEngaged() -> Bool {
        mutex.lock()
        defer { mutex.unlock() }
        return shieldUp
    }

    func physicalInputObserved() -> Bool {
        mutex.lock()
        defer { mutex.unlock() }
        return physicalInput
    }

    /// Confirmation mirrors the real one: it answers for whatever the shield is
    /// actually doing, and a test can make it fail independently of engaging to
    /// stand in for a shield that never reached the screen.
    func confirmShieldCoverage(timeout: TimeInterval) -> Bool {
        mutex.lock()
        defer { mutex.unlock() }
        return shieldUp && !coverageUnconfirmable
    }

    /// Stands in for loginwindow plus the Authorization Plug-in: consume the
    /// published grant, persist its exact nonce receipt, then retract the lock.
    /// `isLocked()` remains a pure observation so polling cannot manufacture a
    /// successful unlock.
    func requestUnlockAuthorization(
        authorizationFieldReady: @Sendable () -> Void,
        prepareGrant: @Sendable () throws -> Void,
        completionReceiptObserved: @Sendable () throws -> Bool
    ) throws {
        mutex.lock()
        authorizationRequests += 1
        authorizationFieldValid = true
        let gate = authorizationGate
        let directory = grantDirectory
        let failureBeforePreparation = authorizationFailureBeforePreparation
        let preparationCalls = grantPreparationCallsPerRequest
        mutex.unlock()
        authorizationStarted.close()
        gate?.wait()
        if let failureBeforePreparation { throw failureBeforePreparation }
        authorizationFieldReady()

        // This gate represents the production wake + exact field discovery
        // phase. It must finish before the callback publishes short-lived
        // authority. Tests can deliberately violate the one-call contract to
        // prove the controller rejects a second callback before reminting.
        for _ in 0..<preparationCalls {
            mutex.lock()
            grantPreparationCallbacks += 1
            mutex.unlock()
            try prepareGrant()
        }
        mutex.lock()
        let failureAfterPreparation = authorizationFailureAfterPreparation
        mutex.unlock()
        if let failureAfterPreparation { throw failureAfterPreparation }

        let grantPath = (directory as NSString)
            .appendingPathComponent(GrantContract.fileName)
        let grantFD = open(grantPath, O_RDONLY | O_NOFOLLOW | O_CLOEXEC)
        guard grantFD >= 0, flock(grantFD, LOCK_SH) == 0 else {
            if grantFD >= 0 { close(grantFD) }
            throw GrantError("fake could not lock the grant")
        }
        var transferredGrantLock = false
        defer {
            if !transferredGrantLock {
                flock(grantFD, LOCK_UN)
                close(grantFD)
            }
        }
        mutex.lock()
        let heldVerificationGate = verificationGate
        mutex.unlock()
        grantVerificationStarted.close()

        let grant = try JSONDecoder().decode(
            Grant.self, from: Data(contentsOf: URL(fileURLWithPath: grantPath)))
        guard let payloadData = Data(base64Encoded: grant.payload) else {
            throw GrantError("fake received a malformed grant")
        }
        let payload = try JSONDecoder().decode(GrantPayload.self, from: payloadData)
        mutex.lock()
        mostRecentGrantPayload = payload
        mutex.unlock()

        // AX submission and the authorization engine advance independently.
        // Start the fake engine only after the interactor has sampled the exact
        // field + locked baseline, and do not apply the unlock until it has
        // observed the terminal while that baseline still holds.
        let monitorStarted = Latch()
        let terminalSamplingFinished = Latch()
        transferredGrantLock = true
        DispatchQueue.global(qos: .userInitiated).async { [weak self] in
            monitorStarted.wait()
            heldVerificationGate?.wait()
            let finalPublished = (try? self?.publishPendingAndFinal(
                payload, directory: directory)) ?? false
            // Match the production plug-in boundary exactly: the shared grant
            // lock ends after pending/Allow/final publication. Destroy's
            // terminal proof and loginwindow's eventual visible transition
            // are independent and may occur after controller withdrawal.
            flock(grantFD, LOCK_UN)
            close(grantFD)
            guard finalPublished else {
                self?.applyAuthorizationTransitionAfterDelay()
                return
            }
            self?.publishTerminalAndApplyTransition(
                payload, terminalSamplingFinished: terminalSamplingFinished)
        }
        try waitForAuthorizationTransaction(
            completionReceiptObserved: completionReceiptObserved,
            monitorStarted: monitorStarted,
            terminalSamplingFinished: terminalSamplingFinished)
    }

    private func publishPendingAndFinal(
        _ payload: GrantPayload, directory: String
    ) throws -> Bool {
        let pendingPath = (directory as NSString)
            .appendingPathComponent(GrantContract.pendingReceiptFileName)
        try Data(payload.nonce.utf8).write(to: URL(fileURLWithPath: pendingPath))

        mutex.lock()
        pendingReceiptNonce = payload.nonce
        let shouldPublishFinal = publishFinalReceipt
        mutex.unlock()
        pendingReceiptPublished.close()

        if shouldPublishFinal {
            let receiptPath = (directory as NSString)
                .appendingPathComponent(GrantContract.receiptFileName)
            try Data(payload.nonce.utf8).write(to: URL(fileURLWithPath: receiptPath))
        }

        mutex.lock()
        if shouldPublishFinal { receiptNonce = payload.nonce }
        mutex.unlock()
        receiptPublished.close()
        return shouldPublishFinal
    }

    private func publishTerminalAndApplyTransition(
        _ payload: GrantPayload, terminalSamplingFinished: Latch
    ) {
        mutex.lock()
        let destroyGate = transactionDestroyGate
        mutex.unlock()
        destroyGate?.wait()
        mutex.lock()
        completionReceiptNonce = payload.nonce
        mutex.unlock()
        completionReceiptPublished.close()

        // Let the fake interactor either observe the terminal while its exact
        // field/locked baseline still holds or reject that lifecycle. A failed
        // observer must not cancel the engine's already-allowed side effect.
        terminalSamplingFinished.wait()
        applyAuthorizationTransitionAfterDelay()
    }

    private func applyAuthorizationTransitionAfterDelay() {
        mutex.lock()
        let delayedGate = delayedUnlockGate
        mutex.unlock()
        delayedGate?.wait()
        mutex.lock()
        setLockedWhileHoldingMutex(false)
        authorizationFieldValid = false
        mutex.unlock()
        authorizationTransitionApplied.close()
    }

    private func waitForAuthorizationTransaction(
        completionReceiptObserved: @Sendable () throws -> Bool,
        monitorStarted: Latch,
        terminalSamplingFinished: Latch
    ) throws {
        mutex.lock()
        let timeout = authorizationTransactionTimeout
        mutex.unlock()
        let deadline = Date().addingTimeInterval(timeout)
        var terminalObserved = false
        while Date() <= deadline {
            mutex.lock()
            let sameField = authorizationFieldValid
            let isLocked = locked
            mutex.unlock()
            if !terminalObserved {
                guard sameField, isLocked else {
                    terminalSamplingFinished.close()
                    throw LockScreenAuthorizationError(
                        "fake authorization UI changed before terminal")
                }
                monitorStarted.close()
                let completionObserved: Bool
                do {
                    completionObserved = try completionReceiptObserved()
                } catch {
                    terminalSamplingFinished.close()
                    throw error
                }
                if completionObserved {
                    mutex.lock()
                    let stillSame = authorizationFieldValid
                    let stillLocked = locked
                    mutex.unlock()
                    guard stillSame, stillLocked else {
                        terminalSamplingFinished.close()
                        throw LockScreenAuthorizationError(
                            "fake authorization UI changed while observing terminal")
                    }
                    terminalObserved = true
                    terminalSamplingFinished.close()
                }
            } else if !sameField && !isLocked {
                return
            } else if sameField && !isLocked {
                terminalSamplingFinished.close()
                throw LockScreenAuthorizationError(
                    "fake screen unlocked before its field transaction completed")
            }
            Thread.sleep(forTimeInterval: 0.005)
        }
        terminalSamplingFinished.close()
        throw LockScreenAuthorizationError("fake authorization transaction timed out")
    }

    func run(_ action: Action) throws -> DesktopService.ActionResult {
        mutex.lock()
        let gate = actionGate
        mutex.unlock()
        actionStarted.close()
        gate?.wait()
        mutex.lock()
        actions.append(action)
        let error = actionError
        mutex.unlock()
        if let error { throw error }
        return .done
    }

    var isAvailable: Bool { true }

    // Test-side accessors.
    var isScreenLocked: Bool {
        mutex.lock()
        defer { mutex.unlock() }
        return locked
    }
    var isShieldUp: Bool {
        mutex.lock()
        defer { mutex.unlock() }
        return shieldUp
    }
    /// The actions that reached the system layer, so a test can assert a
    /// refused action never got there.
    var ranActions: [Action] {
        mutex.lock()
        defer { mutex.unlock() }
        return actions
    }
    var authorizationRequestCount: Int {
        mutex.lock()
        defer { mutex.unlock() }
        return authorizationRequests
    }
    var grantPreparationCallbackCount: Int {
        mutex.lock()
        defer { mutex.unlock() }
        return grantPreparationCallbacks
    }
    var lastGrantPayload: GrantPayload? {
        mutex.lock()
        defer { mutex.unlock() }
        return mostRecentGrantPayload
    }
    func setAuthorizationGate(_ gate: Latch?) {
        mutex.lock()
        authorizationGate = gate
        mutex.unlock()
    }
    func setAuthorizationFailureAfterPreparation(_ error: Error?) {
        mutex.lock()
        authorizationFailureAfterPreparation = error
        mutex.unlock()
    }
    func setAuthorizationFailureBeforePreparation(_ error: Error?) {
        mutex.lock()
        authorizationFailureBeforePreparation = error
        mutex.unlock()
    }
    func setGrantPreparationCallsPerRequest(_ count: Int) {
        mutex.lock()
        grantPreparationCallsPerRequest = max(0, count)
        mutex.unlock()
    }
    func setVerificationGate(_ gate: Latch?) {
        mutex.lock()
        verificationGate = gate
        mutex.unlock()
    }
    func setDelayedUnlockGate(_ gate: Latch?) {
        mutex.lock()
        delayedUnlockGate = gate
        mutex.unlock()
    }
    func setTransactionDestroyGate(_ gate: Latch?) {
        mutex.lock()
        transactionDestroyGate = gate
        mutex.unlock()
    }
    func setAuthorizationTransactionTimeout(_ timeout: TimeInterval) {
        mutex.lock()
        authorizationTransactionTimeout = timeout
        mutex.unlock()
    }
    func setAuthorizationFieldValid(_ valid: Bool) {
        mutex.lock()
        authorizationFieldValid = valid
        mutex.unlock()
    }
    func setAuthorizationUIState(fieldValid: Bool, locked: Bool) {
        mutex.lock()
        authorizationFieldValid = fieldValid
        setLockedWhileHoldingMutex(locked)
        mutex.unlock()
    }
    func receiptMatches(_ nonce: String) -> Bool {
        mutex.lock()
        defer { mutex.unlock() }
        return receiptVisible && receiptNonce == nonce
    }
    func pendingReceiptMatches(_ nonce: String) -> Bool {
        mutex.lock()
        defer { mutex.unlock() }
        return pendingReceiptVisible && pendingReceiptNonce == nonce
    }
    func completionReceiptMatches(_ nonce: String) -> Bool {
        mutex.lock()
        defer { mutex.unlock() }
        return receiptVisible && completionReceiptNonce == nonce
    }
    func setReceiptVisible(_ visible: Bool) {
        mutex.lock()
        receiptVisible = visible
        pendingReceiptVisible = visible
        mutex.unlock()
    }
    func setPublishFinalReceipt(_ publish: Bool) {
        mutex.lock()
        publishFinalReceipt = publish
        mutex.unlock()
    }
    func setActionGate(_ gate: Latch?) {
        mutex.lock()
        actionGate = gate
        mutex.unlock()
    }
    func setActionError(_ error: Error?) {
        mutex.lock()
        actionError = error
        mutex.unlock()
    }
    func set(physicalInput value: Bool) {
        mutex.lock()
        physicalInput = value
        mutex.unlock()
    }
    func set(idle value: TimeInterval) {
        mutex.lock()
        idle = value
        mutex.unlock()
    }
    func set(shieldUp value: Bool) {
        mutex.lock()
        shieldUp = value
        mutex.unlock()
    }
    func set(locked value: Bool) {
        mutex.lock()
        setLockedWhileHoldingMutex(value)
        mutex.unlock()
    }

    func delayLockConfirmation(forRequests count: Int) {
        mutex.lock()
        lockRequestsBeforeConfirmation = max(0, count)
        mutex.unlock()
    }

    private func setLockedWhileHoldingMutex(_ value: Bool) {
        if locked != value {
            locked = value
            lockStateGeneration &+= 1
        }
    }
}

/// A system that cannot do anything, for the paths that must refuse to arm
/// rather than arm on an unverifiable baseline.
final class UnavailableSystem: LockedUseSystem, @unchecked Sendable {
    func isLocked() throws -> Bool { throw LockedUseError.unsupported }
    func lockStateSnapshot() throws -> LockStateSnapshot {
        throw LockedUseError.unsupported
    }
    func lock() throws { throw LockedUseError.unsupported }
    func sinceLastInput() throws -> TimeInterval { throw LockedUseError.unsupported }
    func engageShield() throws { throw LockedUseError.unsupported }
    func releaseShield() throws { throw LockedUseError.unsupported }
    func shieldEngaged() -> Bool { false }
    func physicalInputObserved() -> Bool { false }
    func confirmShieldCoverage(timeout: TimeInterval) -> Bool { false }
    func requestUnlockAuthorization(
        authorizationFieldReady: @Sendable () -> Void,
        prepareGrant: @Sendable () throws -> Void,
        completionReceiptObserved: @Sendable () throws -> Bool
    ) throws { throw LockedUseError.unsupported }
    func run(_ action: Action) throws -> DesktopService.ActionResult {
        throw LockedUseError.unsupported
    }
    var isAvailable: Bool { false }
}

final class LockedUseControllerTests: XCTestCase {
    private func makeController(
        system: FakeSystem,
        authorizationSettleTimeout: TimeInterval = 20,
        relockTimeout: TimeInterval = 20,
        relockRetryInterval: TimeInterval = 0.5,
        consoleUserIdentity: ConsoleUserIdentity? = nil,
        mutate: ((inout ComputerUseConfig) -> Void)? = nil
    ) -> LockedUseController {
        let directory = NSTemporaryDirectory() + "ra-ctl-\(UUID().uuidString)"
        addTeardownBlock { try? FileManager.default.removeItem(atPath: directory) }
        var config = ComputerUseConfig(
            enabled: true,
            lockedUse: LockedUseConfig(
                enabled: true,
                // Production defaults to the plug-in's root-owned directory; a
                // test must stay inside its own temp dir.
                grantDirectory: (directory as NSString).appendingPathComponent("locked-use")))
        mutate?(&config)
        system.grantDirectory = config.lockedUse.grantDirectory
        let identity = consoleUserIdentity
            ?? ConsoleUserIdentity(uid: getuid(), username: NSUserName())
        let controller = LockedUseController(
            config: config, deviceID: "mac-test", system: system,
            receiptVerifier: { nonce, _ in system.receiptMatches(nonce) },
            pendingReceiptVerifier: { nonce, _ in
                system.pendingReceiptMatches(nonce)
            },
            completionReceiptVerifier: { nonce, _ in
                system.completionReceiptMatches(nonce)
            },
            consoleUserProvider: { identity },
            authorizationSettleTimeout: authorizationSettleTimeout,
            relockTimeout: relockTimeout,
            relockRetryInterval: relockRetryInterval)
        controller.start()
        addTeardownBlock { controller.stop() }
        return controller
    }

    /// Polls until `condition` holds, so a test never races the 40ms monitor.
    private func eventually(
        _ description: String, timeout: TimeInterval = 5,
        _ condition: () -> Bool
    ) {
        let deadline = Date().addingTimeInterval(timeout)
        while Date() < deadline {
            if condition() { return }
            Thread.sleep(forTimeInterval: 0.01)
        }
        XCTFail("timed out waiting for \(description)")
    }

    private func routed(
        _ json: String, controller: LockedUseController
    ) -> [String: Any] {
        let router = RequestRouter(desktop: DesktopService(), controller: controller)
        let data = router.handle(line: Data(json.utf8))
        return (try? JSONSerialization.jsonObject(with: data)) as? [String: Any] ?? [:]
    }

    func testProductionLockStateGenerationIsStickyAcrossObservedEdges() {
        let observed = ObservedLockStateGeneration()
        let entry = observed.observe(false)
        XCTAssertEqual(observed.observe(false).generation, entry.generation)
        XCTAssertNotEqual(observed.observe(true).generation, entry.generation)
        let unlockedAgain = observed.observe(false)
        XCTAssertFalse(unlockedAgain.isLocked)
        XCTAssertNotEqual(unlockedAgain.generation, entry.generation)
    }

    func testArmsAndOpensWindow() throws {
        let system = FakeSystem()
        let controller = makeController(system: system)

        try controller.openWindow(turnID: "turn-1")
        XCTAssertEqual(controller.openWindowTurn(), "turn-1")
        XCTAssertFalse(system.isScreenLocked, "the window should have unlocked the screen")
        XCTAssertTrue(system.isShieldUp, "the shield must be up while a window is open")

        controller.closeWindow(reason: "done")
        XCTAssertNil(controller.openWindowTurn())
        XCTAssertTrue(system.isScreenLocked, "closing must relock")
        XCTAssertFalse(system.isShieldUp, "closing must drop the shield after relocking")
    }

    /// The grant must not outlive the unlock it authorized. One resting on disk
    /// is ambient authority any local process could ride.
    func testGrantDoesNotOutliveTheUnlock() throws {
        let system = FakeSystem()
        let controller = makeController(system: system)
        try controller.openWindow(turnID: "turn-1")

        let path = (system.grantDirectory as NSString)
            .appendingPathComponent(GrantContract.fileName)
        XCTAssertFalse(
            FileManager.default.fileExists(atPath: path),
            "a grant was still published after the unlock resolved")
    }

    func testNonConsoleHelperRefusesBeforePublishingAnAuthorizationGrant() {
        let system = FakeSystem()
        let controller = makeController(
            system: system,
            consoleUserIdentity: ConsoleUserIdentity(
                uid: getuid() &+ 1, username: NSUserName() + "-switched-out"))

        XCTAssertThrowsError(try controller.openWindow(turnID: "turn-other-session"))
        eventually("non-console refusal cleanup") { !controller.isWindowClosing() }
        XCTAssertEqual(
            system.authorizationRequestCount, 1,
            "field readiness must precede the final console-identity revalidation")
        XCTAssertEqual(system.grantPreparationCallbackCount, 1)
        XCTAssertTrue(system.isScreenLocked)
        XCTAssertFalse(
            controller.auditEntries().contains { $0.event == "grant_published" },
            "a switched-out user's helper must never publish ambient unlock authority")
    }

    func testOpenIsIdempotentPerTurn() throws {
        let system = FakeSystem()
        let controller = makeController(system: system)
        try controller.openWindow(turnID: "turn-1")
        // A client retry after a relay timeout must not stack windows.
        try controller.openWindow(turnID: "turn-1")
        XCTAssertEqual(controller.openWindowTurn(), "turn-1")

        XCTAssertThrowsError(try controller.openWindow(turnID: "turn-2")) { error in
            guard case LockedUseError.windowBusy = error else {
                return XCTFail("expected windowBusy, got \(error)")
            }
        }
    }

    func testRefusedWhenLocalInputIsRecent() {
        let system = FakeSystem()
        system.set(idle: 0.01)
        let controller = makeController(system: system)

        XCTAssertThrowsError(try controller.openWindow(turnID: "turn-1")) { error in
            XCTAssertEqual(error as? LockedUseError, .localInput)
        }
        // The window stays registered until its relock finishes, so waiting on
        // openWindowTurn() alone would resume while it is still unwinding.
        eventually("the reservation to clear") { !controller.isWindowClosing() }
        XCTAssertTrue(system.isScreenLocked)
    }

    /// An input monitor that cannot answer is not permission to proceed.
    func testRefusedWhenInputMonitorFails() {
        let system = FakeSystem()
        system.idleFails = true
        let controller = makeController(system: system)

        XCTAssertThrowsError(try controller.openWindow(turnID: "turn-1"))
        XCTAssertTrue(system.isScreenLocked)
    }

    func testRefusedWhenShieldFails() {
        let system = FakeSystem()
        system.shieldFails = true
        let controller = makeController(system: system)

        XCTAssertThrowsError(try controller.openWindow(turnID: "turn-1")) { error in
            guard case LockedUseError.shieldRequired = error else {
                return XCTFail("expected shieldRequired, got \(error)")
            }
        }
        XCTAssertTrue(system.isScreenLocked, "no unlock may happen without a confirmed shield")
    }

    func testLocalInputDuringWindowRelocks() throws {
        let system = FakeSystem()
        let controller = makeController(system: system)
        try controller.openWindow(turnID: "turn-1")

        // A person touches the machine: the idle counter resets below the
        // window's age, which is the presence signal.
        system.set(idle: 0)
        eventually("the window to close on local input") { controller.openWindowTurn() == nil }
        eventually("the screen to relock") { system.isScreenLocked }
        eventually("the local-input cleanup to finish") { !controller.isWindowClosing() }
        let lockedUse = controller.status()["locked_use"] as? [String: Any]
        XCTAssertEqual(lockedUse?["suppressed_until_manual_unlock"] as? Bool, true)
        XCTAssertThrowsError(try controller.openWindow(turnID: "turn-2")) { error in
            XCTAssertEqual(error as? LockedUseError, .localInput)
        }
    }

    func testSuppressedPhysicalInputRelocksEvenWhenIdleClockDoesNotMove() throws {
        let system = FakeSystem()
        let controller = makeController(system: system)
        try controller.openWindow(turnID: "turn-1")

        // The event tap suppressed this physical event, so the HID idle clock
        // can remain high. The sticky guard signal must still end the window.
        system.set(physicalInput: true)
        eventually("physical-input window close") { controller.openWindowTurn() == nil }
        eventually("physical-input relock") { system.isScreenLocked }
        eventually("physical-input cleanup") { !controller.isWindowClosing() }
        let state = controller.status()["locked_use"] as? [String: Any]
        XCTAssertEqual(state?["suppressed_until_manual_unlock"] as? Bool, true)
    }

    func testWindowTTLExpiryCloses() throws {
        let system = FakeSystem()
        let controller = makeController(system: system) { config in
            config.lockedUse.windowTTLSeconds = 15  // the clamped minimum
        }
        try controller.openWindow(turnID: "turn-1")
        XCTAssertNotNil(controller.openWindowTurn())
        // The TTL floor is 15s, too slow to wait out here; closing explicitly
        // exercises the same teardown the watcher would run.
        controller.closeWindow(reason: "ttl")
        XCTAssertTrue(system.isScreenLocked)
    }

    func testShieldDropDuringWindowRelocks() throws {
        let system = FakeSystem()
        let controller = makeController(system: system)
        try controller.openWindow(turnID: "turn-1")

        // The shield dying is indistinguishable from a bystander gaining a view
        // of the desktop, so it must end the window.
        system.set(shieldUp: false)
        eventually("the window to close on a dropped shield") {
            controller.openWindowTurn() == nil
        }
        eventually("the screen to relock") { system.isScreenLocked }
    }

    /// An uncovered unlocked screen is worse than a covered one: when the
    /// relock cannot be confirmed the shield stays up and the failure is
    /// recorded loudly.
    func testFailedRelockKeepsShieldUp() throws {
        let system = FakeSystem()
        let controller = makeController(
            system: system, relockTimeout: 0.05, relockRetryInterval: 0.01
        ) { config in
            config.lockedUse.windowTTLSeconds = 900
        }
        try controller.openWindow(turnID: "turn-1")

        system.lockFails = true
        system.lockStateFails = true
        controller.closeWindow(reason: "test")

        XCTAssertTrue(system.isShieldUp, "the shield must be held up when the relock failed")
        XCTAssertTrue(
            controller.auditEntries().contains { $0.event == "relock_failed" },
            "a failed relock must be recorded")

        system.lockFails = false
        system.lockStateFails = false
        eventually("relock quarantine recovery") { !system.isShieldUp }
    }

    func testUnsafeTerminationDoesNotPermitExitOrReleaseShield() throws {
        let system = FakeSystem()
        let controller = makeController(
            system: system, relockTimeout: 0.05, relockRetryInterval: 0.01)
        try controller.openWindow(turnID: "turn-1")

        system.lockFails = true
        system.lockStateFails = true
        XCTAssertFalse(controller.stop())
        XCTAssertFalse(controller.isSafeToExit)
        XCTAssertTrue(system.isShieldUp)
        XCTAssertFalse(
            controller.captureAllowed().allowed,
            "legacy capture must remain closed while cleanup is quarantined")

        // Once quarantine can prove grant withdrawal plus a relock, the
        // executable's termination waiter may safely exit.
        system.lockFails = false
        system.lockStateFails = false
        eventually("termination quarantine recovery") { controller.isSafeToExit }
        XCTAssertTrue(system.isScreenLocked)
    }

    func testPrepareRestartSynchronouslyRelocksAndRefusesNewWork() throws {
        let system = FakeSystem()
        let controller = makeController(system: system)
        try controller.openWindow(turnID: "turn-restart")

        let response = routed(#"{"op":"prepare_restart"}"#, controller: controller)
        XCTAssertEqual(response["ok"] as? Bool, true)
        XCTAssertEqual(response["safe_to_restart"] as? Bool, true)
        XCTAssertTrue(system.isScreenLocked)
        XCTAssertFalse(system.isShieldUp)
        XCTAssertFalse(controller.windowRegistration().registered)

        let lateAction = routed(
            #"{"op":"action","turn_id":"new-turn","action":"pointer.move","x":1,"y":1}"#,
            controller: controller)
        XCTAssertEqual(lateAction["code"] as? String, "not_armed")
        XCTAssertTrue(system.ranActions.isEmpty)
    }

    func testPrepareRestartFailureKeepsShieldAndCanBeRetriedAfterQuarantine() throws {
        let system = FakeSystem()
        let controller = makeController(
            system: system, relockTimeout: 0.05, relockRetryInterval: 0.01)
        try controller.openWindow(turnID: "turn-restart")
        system.lockFails = true
        system.lockStateFails = true

        let refused = routed(#"{"op":"prepare_restart"}"#, controller: controller)
        XCTAssertEqual(refused["ok"] as? Bool, false)
        XCTAssertEqual(refused["code"] as? String, "failed")
        XCTAssertFalse(controller.isSafeToExit)
        XCTAssertTrue(system.isShieldUp)

        system.lockFails = false
        system.lockStateFails = false
        eventually("restart quarantine recovery") { controller.isSafeToExit }
        let retried = routed(#"{"op":"prepare_restart"}"#, controller: controller)
        XCTAssertEqual(retried["ok"] as? Bool, true)
        XCTAssertEqual(retried["safe_to_restart"] as? Bool, true)
    }

    /// A crash is not a clean stop: it can leave a valid grant on disk and an
    /// unlocked screen. A restart must never inherit that.
    func testStartupScrubRemovesOrphanedGrantAndRelocks() throws {
        let system = FakeSystem()
        system.set(locked: false)
        let directory = NSTemporaryDirectory() + "ra-scrub-\(UUID().uuidString)"
        defer { try? FileManager.default.removeItem(atPath: directory) }
        let grantDirectory = (directory as NSString).appendingPathComponent("locked-use")
        try FileManager.default.createDirectory(
            atPath: grantDirectory, withIntermediateDirectories: true)
        let orphan = (grantDirectory as NSString)
            .appendingPathComponent(GrantContract.fileName)
        try Data(#"{"payload":"x","signature":"y"}"#.utf8)
            .write(to: URL(fileURLWithPath: orphan))

        let config = ComputerUseConfig(
            enabled: true,
            lockedUse: LockedUseConfig(enabled: true, grantDirectory: grantDirectory))
        let controller = LockedUseController(
            config: config, deviceID: "mac-test", system: system,
            receiptVerifier: { nonce, _ in system.receiptMatches(nonce) })
        controller.start()
        defer { controller.stop() }

        XCTAssertFalse(
            FileManager.default.fileExists(atPath: orphan),
            "a grant that survived a crash must not survive a restart")
        XCTAssertTrue(system.isScreenLocked, "startup must establish a locked baseline")
    }

    func testStartupScrubWaitsForAsynchronousLockConfirmation() throws {
        let system = FakeSystem()
        system.set(locked: false)
        // A real SACLockScreenImmediate request returns before CGSession flips
        // its locked bit. Model two accepted-but-not-yet-visible requests so a
        // one-shot readback would disarm even though the transition is healthy.
        system.delayLockConfirmation(forRequests: 2)
        let directory = NSTemporaryDirectory() + "ra-async-lock-\(UUID().uuidString)"
        defer { try? FileManager.default.removeItem(atPath: directory) }
        let grantDirectory = (directory as NSString).appendingPathComponent("locked-use")
        let config = ComputerUseConfig(
            enabled: true,
            lockedUse: LockedUseConfig(enabled: true, grantDirectory: grantDirectory))
        let controller = LockedUseController(
            config: config, deviceID: "mac-test", system: system,
            receiptVerifier: { nonce, _ in system.receiptMatches(nonce) },
            relockTimeout: 0.25, relockRetryInterval: 0.01)
        controller.start()
        defer { controller.stop() }

        XCTAssertTrue(system.isScreenLocked)
        XCTAssertEqual(
            (controller.status()["locked_use"] as? [String: Any])?["armed"] as? Bool,
            true,
            "an asynchronous but verified lock transition must arm Locked Use")
    }

    func testRefusesToArmWithoutAWorkingSystem() {
        let config = ComputerUseConfig(
            enabled: true,
            lockedUse: LockedUseConfig(
                enabled: true,
                grantDirectory: NSTemporaryDirectory() + "ra-unavail-\(UUID().uuidString)"))
        let controller = LockedUseController(
            config: config, deviceID: "mac-test", system: UnavailableSystem())
        controller.start()

        XCTAssertThrowsError(try controller.openWindow(turnID: "turn-1")) { error in
            guard case LockedUseError.notArmed = error else {
                return XCTFail("expected notArmed, got \(error)")
            }
        }
    }

    func testRefusesToArmWithEmptyDeviceIDAfterStartupScrub() throws {
        let system = FakeSystem()
        system.set(locked: false)
        let directory = NSTemporaryDirectory() + "ra-empty-device-\(UUID().uuidString)"
        defer { try? FileManager.default.removeItem(atPath: directory) }
        let grantDirectory = (directory as NSString).appendingPathComponent("locked-use")
        try FileManager.default.createDirectory(
            atPath: grantDirectory, withIntermediateDirectories: true)
        let orphan = (grantDirectory as NSString)
            .appendingPathComponent(GrantContract.fileName)
        try Data(#"{"payload":"x","signature":"y"}"#.utf8)
            .write(to: URL(fileURLWithPath: orphan))

        let config = ComputerUseConfig(
            enabled: true,
            lockedUse: LockedUseConfig(enabled: true, grantDirectory: grantDirectory))
        let signerLoadAttempted = Latch()
        let controller = LockedUseController(
            config: config, deviceID: "  \n", system: system,
            receiptVerifier: { nonce, _ in system.receiptMatches(nonce) },
            signerLoader: { _ in
                signerLoadAttempted.close()
                throw LockedUseError.systemFailure("must not load a key")
            })
        controller.start()
        defer { controller.stop() }

        XCTAssertFalse(FileManager.default.fileExists(atPath: orphan))
        XCTAssertTrue(system.isScreenLocked)
        XCTAssertFalse(signerLoadAttempted.isClosed)
        XCTAssertThrowsError(try controller.openWindow(turnID: "turn-1")) { error in
            guard case let LockedUseError.notArmed(reason) = error else {
                return XCTFail("expected notArmed, got \(error)")
            }
            XCTAssertEqual(reason, "device_id is required for locked use")
        }
        let state = try XCTUnwrap(controller.status()["locked_use"] as? [String: Any])
        XCTAssertEqual(state["armed"] as? Bool, false)
        XCTAssertEqual(state["error"] as? String, "device_id is required for locked use")
    }

    /// No remote caller can grant a device the ability to unlock itself.
    func testRuntimeToggleCannotEnableWhatConfigDisabled() {
        let system = FakeSystem()
        let controller = makeController(system: system) { config in
            config.lockedUse.enabled = false
        }
        XCTAssertThrowsError(try controller.setLockedUseActive(true)) { error in
            XCTAssertEqual(error as? LockedUseError, .lockedUseNotEnabled)
        }
    }

    func testRuntimeToggleOffClosesWindowAndRelocks() throws {
        let system = FakeSystem()
        let controller = makeController(system: system)
        try controller.openWindow(turnID: "turn-1")

        try controller.setLockedUseActive(false)
        XCTAssertNil(controller.openWindowTurn())
        XCTAssertTrue(system.isScreenLocked)

        XCTAssertThrowsError(try controller.openWindow(turnID: "turn-2"))
    }

    /// Legacy capture has no turn ownership and is refused for any registered
    /// window. The owned in-memory action independently requires the shield.
    func testCaptureRefusedWhenWindowOpenWithoutShield() throws {
        let system = FakeSystem()
        let controller = makeController(system: system)
        try controller.openWindow(turnID: "turn-1")

        system.set(shieldUp: false)
        XCTAssertFalse(controller.captureAllowed().allowed)
        XCTAssertThrowsError(try controller.run(Action.parse(
            ActionRequest(action: "screen.capture")), forTurn: "turn-1"))
        XCTAssertTrue(
            system.ranActions.isEmpty,
            "a refused capture must never reach the system layer")
    }

    func testLockedUseNarrowsAnExplicitShieldOptOut() throws {
        let system = FakeSystem()
        let controller = makeController(system: system) { config in
            config.lockedUse.requireDisplayShield = false
        }
        try controller.openWindow(turnID: "turn-1")
        XCTAssertTrue(system.isShieldUp)
        XCTAssertFalse(
            controller.captureAllowed().allowed,
            "legacy capture must not borrow an open turn even with a healthy shield")
        XCTAssertNoThrow(try controller.run(
            Action.parse(ActionRequest(action: "screen.capture")),
            forTurn: "turn-1"))
    }

    func testRunActionRequiresComputerUseEnabled() throws {
        let system = FakeSystem()
        let controller = makeController(system: system) { config in
            config.enabled = false
        }
        XCTAssertThrowsError(try controller.run(Action.parse(
            ActionRequest(action: "pointer.move", x: 1, y: 1)),
            forTurn: "turn-1")) { error in
            XCTAssertEqual(error as? LockedUseError, .notEnabled)
        }
        XCTAssertTrue(system.ranActions.isEmpty)
    }

    func testOrdinaryUnlockedComputerUseDoesNotRequireALockedUseWindow() throws {
        let system = FakeSystem()
        system.set(locked: false)
        let controller = makeController(system: system) { config in
            config.lockedUse.enabled = false
        }

        _ = try controller.run(
            Action.parse(ActionRequest(action: "pointer.move", x: 1, y: 2)),
            forTurn: "ordinary-turn")
        XCTAssertEqual(system.ranActions.count, 1)
    }

    func testOrdinaryComputerUseRefusesALockedScreenWithoutAWindow() throws {
        let system = FakeSystem()
        let controller = makeController(system: system) { config in
            config.lockedUse.enabled = false
        }

        XCTAssertThrowsError(try controller.run(
            Action.parse(ActionRequest(action: "pointer.move", x: 1, y: 2)),
            forTurn: "ordinary-turn")) { error in
                XCTAssertEqual(error as? LockedUseError, .noWindow)
            }
        XCTAssertTrue(system.ranActions.isEmpty)
    }

    func testAnOpenWindowCannotActAfterTheScreenRelocks() throws {
        let system = FakeSystem()
        let controller = makeController(system: system)
        try controller.openWindow(turnID: "turn-1")
        XCTAssertEqual(system.authorizationRequestCount, 1)

        system.set(locked: true)
        eventually("watcher to retire the relocked window") {
            controller.openWindowTurn() == nil
        }
        eventually("relocked window cleanup") { !controller.isWindowClosing() }

        // Unlocking again does not resurrect the old owner. The transition
        // permanently retired that window, even though the admission-time lock
        // check now observes an ordinary unlocked desktop.
        system.set(locked: false)
        XCTAssertThrowsError(try controller.run(
            Action.parse(ActionRequest(action: "pointer.move", x: 1, y: 2)),
            forTurn: "turn-1"))
        XCTAssertTrue(system.ranActions.isEmpty)
        XCTAssertFalse(system.isShieldUp)

        // The old owner/window cannot be reused. A new open performs a fresh
        // grant-backed authorization transaction.
        system.set(locked: true)
        try controller.openWindow(turnID: "turn-2")
        XCTAssertEqual(system.authorizationRequestCount, 2)
        XCTAssertEqual(controller.openWindowTurn(), "turn-2")
    }

    func testRetiredWindowOwnerNeverFallsBackAfterManyLaterWindows() throws {
        let system = FakeSystem()
        let controller = makeController(system: system)

        // This deliberately exceeds the audit ring's bounded history. Security
        // tombstones are not audit entries: evicting the oldest owner would let
        // it silently become an ordinary unlocked-desktop turn again.
        for index in 0..<80 {
            system.set(locked: false)
            try controller.openWindow(turnID: "retired-turn-\(index)")
            XCTAssertTrue(controller.closeWindow(reason: "retire test owner"))
        }

        system.set(locked: false)
        XCTAssertThrowsError(try controller.run(
            Action.parse(ActionRequest(action: "pointer.move", x: 1, y: 2)),
            forTurn: "retired-turn-0")) { error in
                XCTAssertEqual(error as? LockedUseError, .noWindow)
            }
        XCTAssertTrue(system.ranActions.isEmpty)
    }

    func testDisabledComputerUseRefusesInsteadOfUsingDesktopFallback() throws {
        let system = FakeSystem()
        system.set(locked: false)
        let controller = makeController(system: system) { config in
            config.enabled = false
        }

        let response = routed(
            #"{"op":"action","turn_id":"turn-1","action":"pointer.move","x":1,"y":2}"#,
            controller: controller)
        XCTAssertEqual(response["code"] as? String, "not_enabled")
        XCTAssertTrue(system.ranActions.isEmpty)
    }

    /// The audit ring is uploaded off-device, so anything recorded leaves the
    /// machine. It must carry no grant body, no full nonce, no key material.
    func testAuditRingCarriesNoSecrets() throws {
        let system = FakeSystem()
        let controller = makeController(system: system)
        try controller.openWindow(turnID: "turn-1")
        controller.closeWindow(reason: "done")

        let entries = controller.auditEntries()
        XCTAssertTrue(entries.contains { $0.event == "grant_published" })
        XCTAssertTrue(entries.contains { $0.event == "window_opened" })
        XCTAssertTrue(entries.contains { $0.event == "window_closed" })

        let encoded = String(
            decoding: try JSONEncoder().encode(entries), as: UTF8.self)
        XCTAssertFalse(encoded.contains("payload"))
        XCTAssertFalse(encoded.contains("signature"))
        XCTAssertFalse(encoded.contains("BEGIN"))
        for entry in entries {
            if let prefix = entry.noncePrefix {
                XCTAssertEqual(
                    prefix.count, 8,
                    "only a short nonce prefix may be recorded, never the full value")
            }
        }
    }

    func testStatusReportsCapabilityWithoutSecrets() throws {
        let system = FakeSystem()
        let controller = makeController(system: system)
        let status = controller.status()

        XCTAssertEqual(status["enabled"] as? Bool, true)
        XCTAssertEqual(status["available"] as? Bool, true)
        let lockedUse = try XCTUnwrap(status["locked_use"] as? [String: Any])
        XCTAssertEqual(lockedUse["armed"] as? Bool, true)
        XCTAssertEqual(lockedUse["window_open"] as? Bool, false)
        XCTAssertEqual(lockedUse["window_state"] as? String, "closed")
        XCTAssertEqual(lockedUse["suppressed_until_manual_unlock"] as? Bool, false)
        XCTAssertNil(lockedUse["signing_key"])
        XCTAssertNil(lockedUse["private_key"])

        // The published key is the verifying half and must actually verify this
        // controller's grants, or an operator provisions the plug-in with
        // something that can never accept them.
        let published = try XCTUnwrap(lockedUse["public_key"] as? String)
        let publicKey = try XCTUnwrap(Data(base64Encoded: published))
        XCTAssertEqual(publicKey.count, GrantContract.publicKeyBytes)
    }

    /// Only one window may exist no matter how many callers race for it.
    func testConcurrentOpensYieldOneWindow() {
        let system = FakeSystem()
        let controller = makeController(system: system)

        let succeeded = NSMutableArray()
        let guardLock = NSLock()
        DispatchQueue.concurrentPerform(iterations: 8) { index in
            if (try? controller.openWindow(turnID: "turn-\(index)")) != nil {
                guardLock.lock()
                succeeded.add(index)
                guardLock.unlock()
            }
        }
        XCTAssertEqual(succeeded.count, 1, "concurrent opens must not stack windows")
    }

    /// A relay retry for the same turn must wait for the first opener's real
    /// result. A reservation is not evidence that authorization succeeded.
    func testSameTurnRetryWaitsForOpeningAndSharesItsResult() {
        let system = FakeSystem()
        let gate = Latch()
        system.setAuthorizationGate(gate)
        let controller = makeController(system: system)
        let firstDone = Latch()
        let secondDone = Latch()
        let failures = NSMutableArray()
        let failuresLock = NSLock()

        DispatchQueue.global().async {
            do { try controller.openWindow(turnID: "turn-1") }
            catch {
                failuresLock.lock()
                failures.add("first: \(error)")
                failuresLock.unlock()
            }
            firstDone.close()
        }
        XCTAssertTrue(system.authorizationStarted.wait(timeout: 2))

        DispatchQueue.global().async {
            do { try controller.openWindow(turnID: "turn-1") }
            catch {
                failuresLock.lock()
                failures.add("second: \(error)")
                failuresLock.unlock()
            }
            secondDone.close()
        }
        XCTAssertFalse(
            secondDone.wait(timeout: 0.1),
            "same-turn retry returned success while the first open was blocked")
        XCTAssertFalse(controller.captureAllowed().allowed)
        let openingAction = routed(
            #"{"op":"action","turn_id":"turn-1","action":"pointer.move","x":1,"y":1}"#,
            controller: controller)
        XCTAssertEqual(openingAction["code"] as? String, "window_busy")
        let openingAX = routed(
            #"{"op":"ax_read","turn_id":"turn-1","app":"NoSuchApp"}"#,
            controller: controller)
        XCTAssertEqual(
            openingAX["code"] as? String, "window_busy",
            "opening ownership must be checked before Accessibility trust")

        gate.close()
        XCTAssertTrue(firstDone.wait(timeout: 5))
        XCTAssertTrue(secondDone.wait(timeout: 5))
        XCTAssertEqual(failures.count, 0, "same-turn callers diverged: \(failures)")
        XCTAssertEqual(system.authorizationRequestCount, 1)
        XCTAssertEqual(controller.openWindowTurn(), "turn-1")
    }

    /// Closing while loginwindow authorization is blocked must wait for that
    /// attempt, observe its late unlock, and relock after it — never before it.
    func testCloseDuringOpeningCannotLeaveALateUnlockBehind() {
        let system = FakeSystem()
        let gate = Latch()
        system.setAuthorizationGate(gate)
        let controller = makeController(system: system)
        let openDone = Latch()
        let closeDone = Latch()

        DispatchQueue.global().async {
            try? controller.openWindow(turnID: "turn-1")
            openDone.close()
        }
        XCTAssertTrue(system.authorizationStarted.wait(timeout: 2))
        XCTAssertFalse(
            controller.auditEntries().contains { $0.event == "grant_published" },
            "field discovery published the grant before it was ready")
        DispatchQueue.global().async {
            controller.closeWindow(reason: "cancel opening")
            closeDone.close()
        }
        XCTAssertFalse(
            closeDone.wait(timeout: 0.1),
            "close returned before the in-flight authorization became terminal")

        gate.close()
        XCTAssertTrue(openDone.wait(timeout: 5))
        XCTAssertTrue(closeDone.wait(timeout: 5))
        XCTAssertTrue(system.isScreenLocked)
        XCTAssertNil(controller.openWindowTurn())
        XCTAssertFalse(controller.isWindowClosing())
        XCTAssertFalse(
            controller.auditEntries().contains { $0.event == "window_opened" },
            "a cancelled opener was published as open")
        XCTAssertFalse(
            controller.auditEntries().contains { $0.event == "grant_published" },
            "cancellation before field readiness still published a grant")
        XCTAssertEqual(system.grantPreparationCallbackCount, 1)
    }

    func testWindowStateExposesAndCloseCancelsAnOpeningOwner() throws {
        let system = FakeSystem()
        let gate = Latch()
        system.setAuthorizationGate(gate)
        let controller = makeController(system: system)
        let openDone = Latch()
        let closeDone = Latch()
        let closeResponses = NSMutableArray()

        DispatchQueue.global().async {
            try? controller.openWindow(turnID: "turn-opening")
            openDone.close()
        }
        XCTAssertTrue(system.authorizationStarted.wait(timeout: 2))

        let state = routed(#"{"op":"window_state"}"#, controller: controller)
        XCTAssertEqual(state["window_registered"] as? Bool, true)
        XCTAssertEqual(state["window_phase"] as? String, "opening")
        XCTAssertEqual(state["window_open"] as? Bool, false)
        XCTAssertEqual(state["window_turn_id"] as? String, "turn-opening")

        let router = RequestRouter(desktop: DesktopService(), controller: controller)
        DispatchQueue.global().async {
            closeResponses.add(router.handle(line: Data(
                #"{"op":"window_close","turn_id":"turn-opening","reason":"agent shutdown"}"#.utf8)))
            closeDone.close()
        }
        XCTAssertFalse(
            closeDone.wait(timeout: 0.1),
            "opening close returned before the authorization boundary settled")

        gate.close()
        XCTAssertTrue(openDone.wait(timeout: 5))
        XCTAssertTrue(closeDone.wait(timeout: 5))
        let response = try XCTUnwrap(closeResponses.firstObject as? Data)
        XCTAssertEqual(
            (try JSONSerialization.jsonObject(with: response) as? [String: Any])?["ok"]
                as? Bool,
            true)
        XCTAssertTrue(system.isScreenLocked)
        XCTAssertFalse(controller.windowRegistration().registered)
    }

    func testReceiptExtendsSettlementPastGrantExpiryBeforeCloseReleasesShield() {
        let system = FakeSystem()
        let lateUnlock = Latch()
        system.setDelayedUnlockGate(lateUnlock)
        let controller = makeController(
            system: system, authorizationSettleTimeout: 5,
            relockRetryInterval: 0.01
        ) { config in
            config.lockedUse.grantTTLSeconds = 2
        }
        let openDone = Latch()
        let closeDone = Latch()

        DispatchQueue.global().async {
            try? controller.openWindow(turnID: "turn-late")
            openDone.close()
        }
        XCTAssertTrue(system.receiptPublished.wait(timeout: 2))
        DispatchQueue.global().async {
            _ = controller.closeWindow(reason: "cancel after receipt")
            closeDone.close()
        }

        Thread.sleep(forTimeInterval: 2.2)
        XCTAssertFalse(openDone.isClosed, "the opener stopped at grant expiry after receipt")
        XCTAssertFalse(closeDone.isClosed, "close returned before authorization settled")
        XCTAssertTrue(system.isShieldUp, "the shield dropped before the late transition")

        lateUnlock.close()
        XCTAssertTrue(openDone.wait(timeout: 5))
        XCTAssertTrue(closeDone.wait(timeout: 5))
        XCTAssertTrue(system.isScreenLocked)
        XCTAssertFalse(system.isShieldUp)
    }

    func testExternalUnlockBeforeTerminalCannotSettleDelayedGrant() throws {
        let system = FakeSystem()
        let destroyGate = Latch()
        let delayedGrantTransition = Latch()
        system.setTransactionDestroyGate(destroyGate)
        system.setDelayedUnlockGate(delayedGrantTransition)
        let controller = makeController(
            system: system, authorizationSettleTimeout: 0.15,
            relockTimeout: 0.05, relockRetryInterval: 0.01)
        let openDone = Latch()
        let closeDone = Latch()
        let openOutcomes = NSMutableArray()
        let closeOutcomes = NSMutableArray()

        DispatchQueue.global().async {
            do {
                try controller.openWindow(turnID: "turn-unrelated-unlock")
                openOutcomes.add("opened")
            } catch {
                openOutcomes.add(error)
            }
            openDone.close()
        }
        XCTAssertTrue(system.receiptPublished.wait(timeout: 2))

        // Apple Watch/a person wins before this mechanism instance reaches its
        // exact terminal. The exact field disappears with that unrelated
        // unlock, while this grant's own visible transition remains delayed.
        system.setAuthorizationUIState(fieldValid: false, locked: false)
        DispatchQueue.global().async {
            closeOutcomes.add(
                controller.closeWindow(reason: "cancel ambiguous authorization"))
            closeDone.close()
        }

        XCTAssertTrue(openDone.wait(timeout: 3))
        XCTAssertTrue(closeDone.wait(timeout: 3))
        XCTAssertFalse(openOutcomes.contains("opened"))
        XCTAssertEqual(closeOutcomes.firstObject as? Bool, false)
        eventually("ambiguous authorization quarantine") {
            !controller.windowRegistration().registered
                && (controller.status()["locked_use"] as? [String: Any])?["quarantined"]
                    as? Bool == true
        }
        XCTAssertTrue(system.isScreenLocked)
        XCTAssertTrue(system.isShieldUp)

        // Destroy and the OS effect remain independent of grant withdrawal.
        // Releasing both after cleanup must produce a relock, never an exposed
        // unlocked desktop or a repaired/open window.
        destroyGate.close()
        XCTAssertTrue(system.completionReceiptPublished.wait(timeout: 2))
        delayedGrantTransition.close()
        XCTAssertTrue(system.authorizationTransitionApplied.wait(timeout: 2))
        eventually("late grant transition to be relocked") { system.isScreenLocked }
        XCTAssertTrue(system.isShieldUp)
        XCTAssertFalse(
            controller.auditEntries().contains { $0.event == "quarantine_resolved" })
        XCTAssertFalse(
            controller.auditEntries().contains { $0.event == "window_opened" })
    }

    func testWithdrawalWaitsForVerifierAndRechecksProofAfterGrantDeadline() {
        let system = FakeSystem()
        let verifierGate = Latch()
        system.setVerificationGate(verifierGate)
        system.setAuthorizationTransactionTimeout(0.15)
        let controller = makeController(
            system: system, authorizationSettleTimeout: 0.15,
            relockTimeout: 0.05, relockRetryInterval: 0.01
        ) { config in
            config.lockedUse.grantTTLSeconds = 2
        }
        let openDone = Latch()

        DispatchQueue.global().async {
            try? controller.openWindow(turnID: "turn-verifier-race")
            openDone.close()
        }
        XCTAssertTrue(system.grantVerificationStarted.wait(timeout: 2))

        XCTAssertTrue(
            openDone.wait(timeout: 3),
            "the bounded interactor plus grant deadline did not end the request")
        eventually("deadline cleanup to wait on the verifier") {
            controller.isWindowClosing()
        }
        XCTAssertTrue(
            system.isShieldUp,
            "cleanup released the shield while the verifier held the shared grant lock")

        verifierGate.close()
        eventually("late verifier transition quarantine", timeout: 5) {
            !controller.isWindowClosing() && system.isScreenLocked
        }
        let state = controller.status()["locked_use"] as? [String: Any]
        XCTAssertEqual(state?["requires_manual_recovery"] as? Bool, true)
        XCTAssertTrue(system.isShieldUp)
        XCTAssertFalse(
            controller.auditEntries().contains { $0.event == "quarantine_resolved" })
    }

    func testPendingProofWithoutAllowDisarmsAndQuarantinesLateUnlock() {
        let system = FakeSystem()
        let lateUnlock = Latch()
        system.setDelayedUnlockGate(lateUnlock)
        system.setPublishFinalReceipt(false)
        let controller = makeController(
            system: system, authorizationSettleTimeout: 0.15,
            relockTimeout: 0.05, relockRetryInterval: 0.01)

        XCTAssertThrowsError(try controller.openWindow(turnID: "turn-pending"))
        eventually("authorization quarantine") {
            controller.auditEntries().contains { $0.event == "quarantine_entered" }
        }
        XCTAssertTrue(system.isShieldUp)
        let state = controller.status()["locked_use"] as? [String: Any]
        XCTAssertEqual(state?["armed"] as? Bool, false)

        lateUnlock.close()
        XCTAssertTrue(system.authorizationTransitionApplied.wait(timeout: 2))
        eventually("late transition relock") {
            system.isScreenLocked
        }
        XCTAssertTrue(system.isShieldUp)
        XCTAssertEqual(
            (controller.status()["locked_use"] as? [String: Any])?["requires_manual_recovery"]
                as? Bool,
            true)
        XCTAssertFalse(
            controller.auditEntries().contains { $0.event == "quarantine_resolved" })
    }

    func testCrashBetweenAllowAndFinalReceiptStillPassesThroughQuarantine() {
        let system = FakeSystem()
        system.setPublishFinalReceipt(false)
        let controller = makeController(
            system: system, authorizationSettleTimeout: 0.15,
            relockTimeout: 0.05, relockRetryInterval: 0.01)

        // The fake publishes pending, applies the unlock, and omits final — the
        // exact SetResult(Allow) -> final-receipt crash window.
        XCTAssertThrowsError(try controller.openWindow(turnID: "turn-final-crash"))
        eventually("final-receipt crash quarantine") {
            let events = controller.auditEntries().map(\.event)
            return events.contains("quarantine_entered")
        }
        XCTAssertTrue(system.isScreenLocked)
        XCTAssertTrue(system.isShieldUp)
        XCTAssertEqual(
            (controller.status()["locked_use"] as? [String: Any])?["requires_manual_recovery"]
                as? Bool,
            true)
        XCTAssertFalse(
            controller.auditEntries().contains { $0.event == "quarantine_resolved" })
        XCTAssertFalse(
            controller.auditEntries().contains { $0.event == "window_opened" })
    }

    /// An unlocked screen is insufficient without an exact receipt from the
    /// plug-in. This catches stale/manual unlocks being mistaken for this turn.
    func testUnlockWithoutExactReceiptNeverOpensAWindow() {
        let system = FakeSystem()
        system.setReceiptVisible(false)
        let controller = makeController(system: system) { config in
            config.lockedUse.grantTTLSeconds = 2
        }

        XCTAssertThrowsError(try controller.openWindow(turnID: "turn-1"))
        eventually("receipt-less open cleanup") { !controller.isWindowClosing() }
        XCTAssertTrue(system.isScreenLocked)
        XCTAssertNil(controller.openWindowTurn())
        XCTAssertFalse(controller.auditEntries().contains { $0.event == "window_opened" })
        XCTAssertFalse(
            controller.auditEntries().contains { $0.event == "quarantine_entered" },
            "a failure before privileged pending proof is a safe abort")
    }

    /// Physical input while authorization is opening both cancels/relocks this
    /// turn and suppresses future automatic unlocks. Only a later manual unlock
    /// after the verified locked baseline clears suppression.
    func testOpeningInputSuppressesUntilAManualUnlockBaseline() throws {
        let system = FakeSystem()
        let gate = Latch()
        system.setAuthorizationGate(gate)
        let controller = makeController(system: system)
        let openDone = Latch()

        DispatchQueue.global().async {
            try? controller.openWindow(turnID: "turn-1")
            openDone.close()
        }
        XCTAssertTrue(system.authorizationStarted.wait(timeout: 2))
        XCTAssertFalse(
            controller.auditEntries().contains { $0.event == "grant_published" })
        system.set(idle: 0)
        eventually("opening input suppression") {
            let status = controller.status()["locked_use"] as? [String: Any]
            return status?["suppressed_until_manual_unlock"] as? Bool == true
        }

        gate.close()
        XCTAssertTrue(openDone.wait(timeout: 5))
        eventually("opening-input cleanup") { !controller.isWindowClosing() }
        XCTAssertFalse(
            controller.auditEntries().contains { $0.event == "grant_published" },
            "human presence before field readiness still published a grant")
        XCTAssertEqual(system.grantPreparationCallbackCount, 1)
        XCTAssertTrue(system.isScreenLocked)
        XCTAssertThrowsError(try controller.openWindow(turnID: "turn-2")) { error in
            XCTAssertEqual(error as? LockedUseError, .localInput)
        }

        // The auto-unlock from turn-1 did not clear suppression. Only this
        // later unlocked observation, after cleanup proved locked, may do so.
        system.set(idle: 3600)
        system.set(locked: false)
        let recovered = try XCTUnwrap(controller.status()["locked_use"] as? [String: Any])
        XCTAssertEqual(recovered["suppressed_until_manual_unlock"] as? Bool, false)
        XCTAssertTrue(
            controller.auditEntries().contains { $0.event == "manual_unlock_observed" })

        try controller.openWindow(turnID: "turn-3")
        XCTAssertEqual(controller.openWindowTurn(), "turn-3")
    }

    func testRouterRequiresAndEnforcesTurnOwnershipAcrossActionsAXAndClose() throws {
        let system = FakeSystem()
        let controller = makeController(system: system)
        try controller.openWindow(turnID: "turn-owner")

        let missingAction = routed(
            #"{"op":"action","action":"pointer.move","x":1,"y":1}"#,
            controller: controller)
        XCTAssertEqual(missingAction["code"] as? String, "bad_request")

        let foreignAction = routed(
            #"{"op":"action","turn_id":"turn-other","action":"pointer.move","x":1,"y":1}"#,
            controller: controller)
        XCTAssertEqual(foreignAction["code"] as? String, "window_busy")
        XCTAssertTrue(system.ranActions.isEmpty)

        let ownedAction = routed(
            #"{"op":"action","turn_id":"turn-owner","action":"pointer.move","x":1,"y":1}"#,
            controller: controller)
        XCTAssertEqual(ownedAction["ok"] as? Bool, true)
        XCTAssertEqual(system.ranActions.count, 1)

        let missingAX = routed(
            #"{"op":"ax_read","app":"NoSuchApp"}"#, controller: controller)
        XCTAssertEqual(missingAX["code"] as? String, "bad_request")
        let foreignAX = routed(
            #"{"op":"ax_read","turn_id":"turn-other","app":"NoSuchApp"}"#,
            controller: controller)
        XCTAssertEqual(
            foreignAX["code"] as? String, "window_busy",
            "ownership must be refused before Accessibility trust is consulted")

        let foreignClose = routed(
            #"{"op":"window_close","turn_id":"turn-other"}"#, controller: controller)
        XCTAssertEqual(foreignClose["code"] as? String, "window_busy")
        XCTAssertEqual(controller.openWindowTurn(), "turn-owner")

        let ownedClose = routed(
            #"{"op":"window_close","turn_id":"turn-owner"}"#, controller: controller)
        XCTAssertEqual(ownedClose["ok"] as? Bool, true)
        XCTAssertTrue(system.isScreenLocked)
        let absentClose = routed(
            #"{"op":"window_close","turn_id":"turn-owner"}"#, controller: controller)
        XCTAssertEqual(absentClose["code"] as? String, "no_window")
    }

    func testAccessibilityTransportTimeoutClosesAndRelocksWithoutDeadlock() throws {
        let system = FakeSystem()
        let controller = makeController(system: system)
        try controller.openWindow(turnID: "turn-ax-timeout")

        XCTAssertThrowsError(try controller.withAuthorizedTurn(
            forTurn: "turn-ax-timeout"
        ) {
            throw AccessibilityIPCError(message: "target hung")
        }) { error in
            XCTAssertTrue(error is AccessibilityIPCError)
        }
        XCTAssertFalse(controller.windowRegistration().registered)
        XCTAssertTrue(system.isScreenLocked)
        XCTAssertFalse(system.isShieldUp)
    }

    /// The controller lease closes the window-state TOCTOU: close blocks until
    /// an admitted action finishes, but once close flips the phase even that
    /// action's result is discarded; no later action is admitted at all.
    func testRouterCloseWaitsForAnAtomicallyAdmittedAction() throws {
        let system = FakeSystem()
        let actionGate = Latch()
        system.setActionGate(actionGate)
        let controller = makeController(system: system)
        try controller.openWindow(turnID: "turn-owner")
        let router = RequestRouter(desktop: DesktopService(), controller: controller)
        let actionDone = Latch()
        let closeDone = Latch()
        let actionResponses = NSMutableArray()
        let closeResponses = NSMutableArray()

        DispatchQueue.global().async {
            let data = router.handle(line: Data(
                #"{"op":"action","turn_id":"turn-owner","action":"pointer.move","x":1,"y":1}"#.utf8))
            actionResponses.add(data)
            actionDone.close()
        }
        XCTAssertTrue(system.actionStarted.wait(timeout: 2))

        DispatchQueue.global().async {
            let data = router.handle(line: Data(
                #"{"op":"window_close","turn_id":"turn-owner"}"#.utf8))
            closeResponses.add(data)
            closeDone.close()
        }
        XCTAssertFalse(
            closeDone.wait(timeout: 0.1),
            "close returned before an action already admitted for this window finished")
        eventually("the window to enter closing") { controller.isWindowClosing() }
        let lateAction = routed(
            #"{"op":"action","turn_id":"turn-owner","action":"pointer.move","x":2,"y":2}"#,
            controller: controller)
        XCTAssertEqual(lateAction["code"] as? String, "window_busy")

        actionGate.close()
        XCTAssertTrue(actionDone.wait(timeout: 5))
        XCTAssertTrue(closeDone.wait(timeout: 5))
        let actionReply = try XCTUnwrap(actionResponses.firstObject as? Data)
        let closeReply = try XCTUnwrap(closeResponses.firstObject as? Data)
        let actionObject = try XCTUnwrap(
            try JSONSerialization.jsonObject(with: actionReply) as? [String: Any])
        XCTAssertEqual(actionObject["ok"] as? Bool, false)
        XCTAssertEqual(actionObject["code"] as? String, "no_window")
        XCTAssertEqual(
            (try JSONSerialization.jsonObject(with: closeReply) as? [String: Any])?["ok"]
                as? Bool,
            true)
        XCTAssertTrue(system.isScreenLocked)
        XCTAssertNil(controller.openWindowTurn())
    }

    func testOperationResultIsDiscardedWhenScreenRelocksDuringExecution() throws {
        let system = FakeSystem()
        let controller = makeController(system: system)
        try controller.openWindow(turnID: "turn-relock-race")
        let gate = Latch()
        system.setActionGate(gate)
        let responses = NSMutableArray()
        let done = Latch()

        DispatchQueue.global().async {
            responses.add(self.routed(
                #"{"op":"action","turn_id":"turn-relock-race","action":"pointer.move","x":1,"y":2}"#,
                controller: controller))
            done.close()
        }
        XCTAssertTrue(system.actionStarted.wait(timeout: 2))
        system.set(locked: true)
        gate.close()
        XCTAssertTrue(done.wait(timeout: 5))

        let result = try XCTUnwrap(responses.firstObject as? [String: Any])
        XCTAssertEqual(result["ok"] as? Bool, false)
        XCTAssertEqual(result["code"] as? String, "no_window")
        eventually("post-operation relock cleanup") {
            !controller.windowRegistration().registered && system.isScreenLocked
        }
        XCTAssertEqual(system.ranActions.count, 1, "a posted event cannot be recalled")

        // Even after a later manual unlock, this owner cannot use the ordinary
        // no-window fallback to continue the stale turn.
        system.set(locked: false)
        XCTAssertThrowsError(try controller.run(
            Action.parse(ActionRequest(action: "pointer.move", x: 1, y: 2)),
            forTurn: "turn-relock-race"))
    }

    func testOwnedOperationCannotReturnAfterWatcherClosedThenScreenUnlocked() throws {
        let system = FakeSystem()
        let controller = makeController(system: system)
        try controller.openWindow(turnID: "turn-watcher-relock-race")
        let gate = Latch()
        system.setActionGate(gate)
        let responses = NSMutableArray()
        let done = Latch()

        DispatchQueue.global().async {
            responses.add(self.routed(
                #"{"op":"action","turn_id":"turn-watcher-relock-race","action":"pointer.move","x":1,"y":2}"#,
                controller: controller))
            done.close()
        }
        XCTAssertTrue(system.actionStarted.wait(timeout: 2))

        system.set(locked: true)
        eventually("watcher to revoke the operation lease") {
            controller.isWindowClosing()
        }
        // The final snapshot is unlocked again. Phase/cancelled validation and
        // the sticky generation must still discard the already-run action.
        system.set(locked: false)
        gate.close()
        XCTAssertTrue(done.wait(timeout: 5))

        let result = try XCTUnwrap(responses.firstObject as? [String: Any])
        XCTAssertEqual(result["ok"] as? Bool, false)
        eventually("revoked owned operation cleanup") {
            !controller.windowRegistration().registered && system.isScreenLocked
        }
        XCTAssertTrue(system.ranActions.count == 1)
    }

    func testOrdinaryOperationCannotReturnAfterTransientLockUnlockEdge() throws {
        let system = FakeSystem()
        let controller = makeController(system: system)
        system.set(locked: false)
        let gate = Latch()
        system.setActionGate(gate)
        let responses = NSMutableArray()
        let done = Latch()

        DispatchQueue.global().async {
            responses.add(self.routed(
                #"{"op":"action","turn_id":"turn-ordinary-edge","action":"pointer.move","x":4,"y":5}"#,
                controller: controller))
            done.close()
        }
        XCTAssertTrue(system.actionStarted.wait(timeout: 2))
        system.set(locked: true)
        system.set(locked: false)
        gate.close()
        XCTAssertTrue(done.wait(timeout: 5))

        let result = try XCTUnwrap(responses.firstObject as? [String: Any])
        XCTAssertEqual(result["ok"] as? Bool, false)
        XCTAssertEqual(result["code"] as? String, "no_window")
        XCTAssertTrue(system.isScreenLocked)
        XCTAssertThrowsError(try controller.run(
            Action.parse(ActionRequest(action: "pointer.move", x: 1, y: 2)),
            forTurn: "turn-ordinary-edge"))
    }

    func testOperationErrorCannotBypassPostOperationRelockCheck() throws {
        let system = FakeSystem()
        let controller = makeController(system: system)
        try controller.openWindow(turnID: "turn-error-relock-race")
        let gate = Latch()
        system.setActionGate(gate)
        system.setActionError(GrantError("operation failed"))
        let responses = NSMutableArray()
        let done = Latch()

        DispatchQueue.global().async {
            responses.add(self.routed(
                #"{"op":"action","turn_id":"turn-error-relock-race","action":"pointer.move","x":1,"y":2}"#,
                controller: controller))
            done.close()
        }
        XCTAssertTrue(system.actionStarted.wait(timeout: 2))
        system.set(locked: true)
        gate.close()
        XCTAssertTrue(done.wait(timeout: 5))

        let result = try XCTUnwrap(responses.firstObject as? [String: Any])
        XCTAssertEqual(result["ok"] as? Bool, false)
        XCTAssertEqual(
            result["code"] as? String, "no_window",
            "the lock-boundary failure must take precedence over the operation error")
        eventually("error-path post-operation cleanup") {
            !controller.windowRegistration().registered && system.isScreenLocked
        }
        XCTAssertTrue(system.ranActions.count == 1)
    }

    /// A refused open must release its reservation, or the next legitimate open
    /// is blocked forever by a window that never existed.
    func testWindowReservationIsReleasedAfterARefusedOpen() throws {
        let system = FakeSystem()
        system.shieldFails = true
        let controller = makeController(system: system)

        XCTAssertThrowsError(try controller.openWindow(turnID: "turn-1"))
        eventually("the reservation to clear") { !controller.isWindowClosing() }

        system.shieldFails = false
        system.set(idle: 3600)
        try controller.openWindow(turnID: "turn-2")
        XCTAssertEqual(controller.openWindowTurn(), "turn-2")
    }

    /// "The window is closed" has to mean "the screen is confirmed locked".
    /// Stop must not return while a relock is still in flight, or shutdown
    /// becomes a way to leave a Mac unlocked.
    func testStopWaitsForAnInFlightRelock() throws {
        let system = FakeSystem()
        let controller = makeController(system: system)
        try controller.openWindow(turnID: "turn-1")

        controller.stop()
        XCTAssertTrue(
            system.isScreenLocked,
            "stop returned before the screen was confirmed locked")
        XCTAssertNil(controller.openWindowTurn())
    }
}

/// Which side of the unlock the shield is confirmed on is decided by whether
/// the screen was locked, and getting it wrong made the feature unusable from
/// the only state it exists for.
extension LockedUseControllerTests {
    /// Starting from a locked screen, the shield cannot be confirmed first: the
    /// user's session is not being displayed, so the window server reports no
    /// coverage however correct the shield is. Requiring it up front refused
    /// every window with "display shield could not be engaged".
    func testAWindowOpensFromALockedScreen() throws {
        let system = FakeSystem()
        // The fake starts locked, which is the real starting state.
        let controller = makeController(system: system)
        try controller.openWindow(turnID: "turn-1")
        XCTAssertEqual(controller.openWindowTurn(), "turn-1")
        XCTAssertTrue(system.isShieldUp)
    }

    /// The confirmation still has to happen — just after the unlock, which is
    /// the first moment it can succeed and the last moment it is safe to be
    /// missing. A shield that is not covering there means a live desktop, so
    /// the window must not be handed to the turn.
    func testAnUnconfirmableShieldStillRefusesTheWindow() {
        let system = FakeSystem()
        system.coverageUnconfirmable = true
        let controller = makeController(system: system)

        XCTAssertThrowsError(try controller.openWindow(turnID: "turn-1")) { error in
            guard case LockedUseError.shieldRequired = error else {
                return XCTFail("expected shieldRequired, got \(error)")
            }
        }
        eventually("the reservation to clear") { !controller.isWindowClosing() }
        XCTAssertTrue(system.isScreenLocked, "an unconfirmable shield must leave the screen locked")
    }

    /// When the screen is already unlocked the desktop is visible right now, so
    /// confirmation keeps its original place: before anything else happens.
    func testAnUnlockedScreenStillConfirmsTheShieldFirst() {
        let system = FakeSystem()
        system.coverageUnconfirmable = true
        let controller = makeController(system: system)
        // The startup scrub locks; the unlocked state has to be established
        // after it or this would silently exercise the locked path.
        system.set(locked: false)

        XCTAssertThrowsError(try controller.openWindow(turnID: "turn-1")) { error in
            guard case LockedUseError.shieldRequired = error else {
                return XCTFail("expected shieldRequired, got \(error)")
            }
        }
    }
}

/// Publishing a grant alone starts no authorization transaction. The controller
/// must drive loginwindow's own authorization control after publishing it.
extension LockedUseControllerTests {
    func testSlowFieldReadinessConsumesNoGrantLifetime() {
        let system = FakeSystem()
        let fieldReady = Latch()
        system.setAuthorizationGate(fieldReady)
        let controller = makeController(system: system) { config in
            config.lockedUse.grantTTLSeconds = 2
        }
        let openDone = Latch()
        let failures = NSMutableArray()
        let failureLock = NSLock()

        DispatchQueue.global().async {
            do { try controller.openWindow(turnID: "turn-slow-field") }
            catch {
                failureLock.lock()
                failures.add(String(describing: error))
                failureLock.unlock()
            }
            openDone.close()
        }
        XCTAssertTrue(system.authorizationStarted.wait(timeout: 2))

        // Longer than the configured grant TTL: a grant minted before this
        // simulated wake/discovery delay would be expired when the field became
        // ready. No grant or callback may exist yet.
        Thread.sleep(forTimeInterval: 2.2)
        XCTAssertEqual(system.grantPreparationCallbackCount, 0)
        XCTAssertFalse(
            controller.auditEntries().contains { $0.event == "grant_published" })
        let grantPath = (system.grantDirectory as NSString)
            .appendingPathComponent(GrantContract.fileName)
        XCTAssertFalse(FileManager.default.fileExists(atPath: grantPath))

        let fieldReadyReleasedAt = Int64(Date().timeIntervalSince1970)
        fieldReady.close()
        XCTAssertTrue(openDone.wait(timeout: 5))
        XCTAssertEqual(failures.count, 0, "slow discovery consumed grant TTL: \(failures)")
        XCTAssertEqual(system.grantPreparationCallbackCount, 1)
        XCTAssertEqual(
            controller.auditEntries().filter { $0.event == "grant_published" }.count, 1)
        let payload = try? XCTUnwrap(system.lastGrantPayload)
        XCTAssertGreaterThanOrEqual(payload?.issuedAt ?? 0, fieldReadyReleasedAt)
        XCTAssertEqual(
            (payload?.expiresAt ?? 0) - (payload?.issuedAt ?? 0), 2,
            "the fresh payload did not retain its full configured TTL")
        XCTAssertGreaterThanOrEqual(
            payload?.expiresAt ?? 0, Int64(Date().timeIntervalSince1970),
            "the grant was already expired when the prepared transaction completed")
        XCTAssertEqual(controller.openWindowTurn(), "turn-slow-field")
    }

    func testDiscoveryOrReadinessFailurePublishesNoGrant() {
        let system = FakeSystem()
        system.setAuthorizationFailureBeforePreparation(
            LockScreenAuthorizationError(
                "lock-screen value/action readiness did not complete"))
        let controller = makeController(system: system)

        XCTAssertThrowsError(
            try controller.openWindow(turnID: "turn-readiness-failed"))
        eventually("readiness failure cleanup") { !controller.isWindowClosing() }

        XCTAssertEqual(system.authorizationRequestCount, 1)
        XCTAssertEqual(system.grantPreparationCallbackCount, 0)
        XCTAssertFalse(
            controller.auditEntries().contains { $0.event == "authorization_field_ready" })
        XCTAssertFalse(
            controller.auditEntries().contains { $0.event == "grant_published" })
        let returned = controller.auditEntries().last {
            $0.event == "authorization_request_returned"
        }
        XCTAssertTrue(returned?.reason?.contains("value/action readiness") == true)
        let grantPath = (system.grantDirectory as NSString)
            .appendingPathComponent(GrantContract.fileName)
        XCTAssertFalse(FileManager.default.fileExists(atPath: grantPath))
    }

    func testAdversarialSecondPreparationCallbackIsRejectedBeforeRemint() {
        let system = FakeSystem()
        system.setGrantPreparationCallsPerRequest(2)
        let controller = makeController(system: system) { config in
            config.lockedUse.grantTTLSeconds = 2
        }

        XCTAssertThrowsError(
            try controller.openWindow(turnID: "turn-double-preparation"))
        eventually("double callback cleanup") { !controller.isWindowClosing() }

        XCTAssertEqual(system.grantPreparationCallbackCount, 2)
        XCTAssertEqual(
            controller.auditEntries().filter { $0.event == "authorization_field_ready" }.count,
            1)
        XCTAssertEqual(
            controller.auditEntries().filter { $0.event == "grant_published" }.count, 1,
            "the second callback reminted or rewrote a grant")
        let returned = controller.auditEntries().last {
            $0.event == "authorization_request_returned"
        }
        XCTAssertTrue(returned?.reason?.contains("more than once") == true)
        let grantPath = (system.grantDirectory as NSString)
            .appendingPathComponent(GrantContract.fileName)
        XCTAssertFalse(
            FileManager.default.fileExists(atPath: grantPath),
            "the first grant survived rejection of a second callback")
    }

    func testFailureAfterGrantPreparationNeverRemintsOrRewrites() {
        let system = FakeSystem()
        let secretLikeToken = String(repeating: "A", count: 64)
        system.setAuthorizationFailureAfterPreparation(
            LockScreenAuthorizationError(
                "empty-value assignment failed\nsecret \(secretLikeToken)"))
        let controller = makeController(system: system) { config in
            config.lockedUse.grantTTLSeconds = 2
        }

        XCTAssertThrowsError(
            try controller.openWindow(turnID: "turn-single-preparation"))
        eventually("post-preparation failure cleanup") { !controller.isWindowClosing() }

        XCTAssertEqual(system.authorizationRequestCount, 1)
        XCTAssertEqual(system.grantPreparationCallbackCount, 1)
        XCTAssertEqual(
            controller.auditEntries().filter { $0.event == "grant_published" }.count, 1,
            "an ambiguous AX failure reminted or rewrote grant authority")
        let returned = controller.auditEntries().last {
            $0.event == "authorization_request_returned"
        }
        let reason = returned?.reason ?? ""
        XCTAssertTrue(reason.contains("empty-value assignment failed"))
        XCTAssertFalse(reason.contains(secretLikeToken))
        XCTAssertFalse(reason.contains("\n"))
        XCTAssertTrue(reason.contains("[redacted]"))
        let grantPath = (system.grantDirectory as NSString)
            .appendingPathComponent(GrantContract.fileName)
        XCTAssertFalse(
            FileManager.default.fileExists(atPath: grantPath),
            "the single published grant survived ambiguous AX failure cleanup")
    }

    func testAuthorizationFailureReasonRedactsExactNonceControlsAndLength() {
        let nonce = String(repeating: "ab", count: 16)
        let reason = LockedUseController.sanitizedAuthorizationFailureReason(
            LockScreenAuthorizationError(
                "confirm failed\nnonce=\(nonce)\u{0000}"
                    + String(repeating: " noisy", count: 200)),
            nonce: nonce)

        XCTAssertFalse(reason.contains(nonce))
        XCTAssertFalse(reason.unicodeScalars.contains {
            CharacterSet.controlCharacters.contains($0)
        })
        XCTAssertEqual(reason.count, 512)
        XCTAssertTrue(reason.contains("[redacted]"))
    }

    func testOpeningFromALockedScreenRequestsAuthorization() throws {
        let system = FakeSystem()
        let controller = makeController(system: system)
        try controller.openWindow(turnID: "turn-1")
        XCTAssertGreaterThan(
            system.authorizationRequestCount, 0,
            "the controller published a grant without starting loginwindow authorization")
    }

    /// Nothing needs provoking when the screen is already unlocked, and doing
    /// it anyway would post an event into whatever has focus.
    func testAnUnlockedScreenDoesNotRequestAuthorization() throws {
        let system = FakeSystem()
        let controller = makeController(system: system)
        // After the controller, not before: the startup scrub locks the screen
        // on purpose, so setting this first would test the locked path while
        // claiming to test the unlocked one.
        system.set(locked: false)
        try controller.openWindow(turnID: "turn-1")
        XCTAssertEqual(
            system.authorizationRequestCount, 0,
            "an already-unlocked screen started an unnecessary authorization transaction")
    }
}
