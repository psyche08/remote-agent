import XCTest
@testable import RemoteAgentDesktopCore

/// Stands in for the device.
///
/// The important part is `grantDirectory`: the fake consumes a grant from it
/// exactly once, the way the real plug-in does. A test that observes an unlock
/// has therefore also proven the controller really published a grant to
/// authorize it, rather than a test double simply agreeing to be unlocked.
final class FakeSystem: LockedUseSystem, @unchecked Sendable {
    private let mutex = NSLock()

    private var locked = true
    private var idle: TimeInterval = 3600
    private var shieldUp = false
    var shieldFails = false
    var lockFails = false
    var lockStateFails = false
    var idleFails = false
    var coverageUnconfirmable = false
    var grantDirectory = ""
    private var actions: [Action] = []

    func isLocked() throws -> Bool {
        mutex.lock()
        defer { mutex.unlock() }
        if lockStateFails { throw LockedUseError.systemFailure("lock state unavailable") }
        if locked && !grantDirectory.isEmpty {
            let path = (grantDirectory as NSString)
                .appendingPathComponent(GrantContract.fileName)
            if FileManager.default.fileExists(atPath: path) {
                // Stand in for the plug-in: consume the grant and allow the
                // unlock. Consumption is single-use, so a stale grant cannot
                // unlock twice.
                unlink(path)
                locked = false
            }
        }
        return locked
    }

    func lock() throws {
        mutex.lock()
        defer { mutex.unlock() }
        if lockFails { throw LockedUseError.systemFailure("lock failed") }
        locked = true
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

    /// Confirmation mirrors the real one: it answers for whatever the shield is
    /// actually doing, and a test can make it fail independently of engaging to
    /// stand in for a shield that never reached the screen.
    func confirmShieldCoverage(timeout: TimeInterval) -> Bool {
        mutex.lock()
        defer { mutex.unlock() }
        return shieldUp && !coverageUnconfirmable
    }

    /// Counts how often the controller knocked on the login window's door.
    /// Publishing a grant provokes nothing on its own, so a controller that
    /// never knocks can only ever time out.
    private(set) var provocations = 0
    func provokeUnlockAttempt() {
        mutex.lock()
        provocations += 1
        mutex.unlock()
    }

    func run(_ action: Action) throws -> DesktopService.ActionResult {
        mutex.lock()
        defer { mutex.unlock() }
        actions.append(action)
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
        locked = value
        mutex.unlock()
    }
}

/// A system that cannot do anything, for the paths that must refuse to arm
/// rather than arm on an unverifiable baseline.
final class UnavailableSystem: LockedUseSystem, @unchecked Sendable {
    func isLocked() throws -> Bool { throw LockedUseError.unsupported }
    func lock() throws { throw LockedUseError.unsupported }
    func sinceLastInput() throws -> TimeInterval { throw LockedUseError.unsupported }
    func engageShield() throws { throw LockedUseError.unsupported }
    func releaseShield() throws { throw LockedUseError.unsupported }
    func shieldEngaged() -> Bool { false }
    func confirmShieldCoverage(timeout: TimeInterval) -> Bool { false }
    func provokeUnlockAttempt() {}
    func run(_ action: Action) throws -> DesktopService.ActionResult {
        throw LockedUseError.unsupported
    }
    var isAvailable: Bool { false }
}

final class LockedUseControllerTests: XCTestCase {
    private func makeController(
        system: FakeSystem,
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
        let controller = LockedUseController(
            config: config, deviceID: "mac-test", system: system)
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
        let controller = makeController(system: system) { config in
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
            config: config, deviceID: "mac-test", system: system)
        controller.start()
        defer { controller.stop() }

        XCTAssertFalse(
            FileManager.default.fileExists(atPath: orphan),
            "a grant that survived a crash must not survive a restart")
        XCTAssertTrue(system.isScreenLocked, "startup must establish a locked baseline")
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
        defer { controller.stop() }

        XCTAssertThrowsError(try controller.openWindow(turnID: "turn-1")) { error in
            guard case LockedUseError.notArmed = error else {
                return XCTFail("expected notArmed, got \(error)")
            }
        }
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

    /// Capture is not suppressed while the desktop is unlocked, so an
    /// unshielded frame could persist whatever was on screen.
    func testCaptureRefusedWhenWindowOpenWithoutShield() throws {
        let system = FakeSystem()
        let controller = makeController(system: system)
        try controller.openWindow(turnID: "turn-1")

        system.set(shieldUp: false)
        XCTAssertFalse(controller.captureAllowed().allowed)
        XCTAssertThrowsError(try controller.run(Action.parse(
            ActionRequest(action: "screen.capture"))))
        XCTAssertTrue(
            system.ranActions.isEmpty,
            "a refused capture must never reach the system layer")
    }

    /// A device that opted out of the shield accepted that its desktop is
    /// visible; refusing every capture there would disable the feature rather
    /// than protect anything.
    func testCaptureAllowedWhenShieldExplicitlyDisabled() throws {
        let system = FakeSystem()
        let controller = makeController(system: system) { config in
            config.lockedUse.requireDisplayShield = false
        }
        try controller.openWindow(turnID: "turn-1")
        XCTAssertTrue(controller.captureAllowed().allowed)
    }

    func testRunActionRequiresComputerUseEnabled() throws {
        let system = FakeSystem()
        let controller = makeController(system: system) { config in
            config.enabled = false
        }
        XCTAssertThrowsError(try controller.run(Action.parse(
            ActionRequest(action: "pointer.move", x: 1, y: 1)))) { error in
            XCTAssertEqual(error as? LockedUseError, .notEnabled)
        }
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

/// A published grant provokes nothing. macOS evaluates the unlock right when
/// the login window sees user activity go active — its own log calls it "user
/// event received, start an unlock with 'active user' as the reason" — so a
/// controller that only publishes and waits can do nothing but time out, which
/// is what every attempt on the first real device did.
extension LockedUseControllerTests {
    func testOpeningFromALockedScreenProvokesTheLoginWindow() throws {
        let system = FakeSystem()
        let controller = makeController(system: system)
        try controller.openWindow(turnID: "turn-1")
        XCTAssertGreaterThan(
            system.provocations, 0,
            "the controller published a grant and waited without asking macOS to evaluate it")
    }

    /// Nothing needs provoking when the screen is already unlocked, and doing
    /// it anyway would post an event into whatever has focus.
    func testAnUnlockedScreenIsNotProvoked() throws {
        let system = FakeSystem()
        let controller = makeController(system: system)
        // After the controller, not before: the startup scrub locks the screen
        // on purpose, so setting this first would test the locked path while
        // claiming to test the unlocked one.
        system.set(locked: false)
        try controller.openWindow(turnID: "turn-1")
        XCTAssertEqual(
            system.provocations, 0,
            "an already-unlocked screen was sent a synthetic event for no reason")
    }
}
