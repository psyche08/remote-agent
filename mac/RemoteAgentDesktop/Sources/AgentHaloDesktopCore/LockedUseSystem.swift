import Foundation

/// One atomic observation of the screen-lock state. `generation` is sticky for
/// every lock-state edge this system boundary has observed, so an operation
/// cannot accept a final `unlocked` sample after crossing locked in between.
///
/// Production currently derives edges from `CGSessionCopyCurrentDictionary`
/// probes; macOS exposes no documented, lossless lock-transition notification
/// to this user-session helper. The open-window watcher supplies concurrent
/// probes, but an edge entirely between probes remains a platform boundary.
public struct LockStateSnapshot: Equatable, Sendable {
    public let isLocked: Bool
    public let generation: UInt64

    public init(isLocked: Bool, generation: UInt64) {
        self.isLocked = isLocked
        self.generation = generation
    }
}

final class ObservedLockStateGeneration: @unchecked Sendable {
    private let lock = NSLock()
    private var previous: Bool?
    private var generation: UInt64 = 0

    func observe(_ isLocked: Bool) -> LockStateSnapshot {
        lock.lock()
        if let previous, previous != isLocked { generation &+= 1 }
        self.previous = isLocked
        let snapshot = LockStateSnapshot(
            isLocked: isLocked, generation: generation)
        lock.unlock()
        return snapshot
    }
}

/// The device boundary the controller depends on. Tests supply a fake; the real
/// implementation is `DesktopService`.
///
/// Note the asymmetry, which is the whole point of Locked Use: this can *lock*
/// the screen directly, but it cannot unlock it. Unlocking only ever happens
/// through the macOS unlock flow, with the Authorization Plug-in deciding
/// whether a signed grant permits it. Nothing in this process holds or supplies
/// the user's password.
public protocol LockedUseSystem: AnyObject, Sendable {
    /// Whether the screen is currently locked. Throwing must be treated as "the
    /// state is unknown", never as unlocked.
    func isLocked() throws -> Bool
    /// Atomically returns the current state and the sticky generation for every
    /// state edge observed through this system boundary.
    func lockStateSnapshot() throws -> LockStateSnapshot
    /// Locks the screen immediately. Used to restore the locked state when a
    /// window ends for any reason, including failure paths.
    func lock() throws
    /// How long the machine has been idle of local keyboard and pointer input.
    /// Throwing must be treated as "a person may be present": the controller
    /// fails closed and relocks.
    func sinceLastInput() throws -> TimeInterval
    /// Raises the shield windows. Coverage is confirmed separately.
    func engageShield() throws
    /// Waits for confirmation that the shield is actually on screen.
    ///
    /// This cannot succeed while the screen is locked — the user's session is
    /// not displayed, so nothing in it is on screen — which is why the
    /// controller requires it before the unlock only when the screen was
    /// already unlocked, and immediately after otherwise.
    func confirmShieldCoverage(timeout: TimeInterval) -> Bool
    /// Drops the shield. Called on every window-close path.
    func releaseShield() throws
    /// The shield's current state, re-probed rather than cached.
    func shieldEngaged() -> Bool
    /// Whether the shield's event tap observed and suppressed physical input
    /// during this window. This is distinct from the HID idle clock because a
    /// successfully suppressed event must still terminate Locked Use.
    func physicalInputObserved() -> Bool
    /// Begins the real loginwindow authorization transaction. The production
    /// implementation wakes the lock screen, locates its password field through
    /// Accessibility, and writes one empty value without supplying a credential.
    /// The Authorization Plug-in is the branch that decides whether this exact
    /// transaction may retract the lock. `prepareGrant` runs exactly once only
    /// after the exact field is ready, immediately before empty-value
    /// submission, so lock-screen wake/discovery consumes no grant lifetime.
    func requestUnlockAuthorization(
        authorizationFieldReady: @Sendable () -> Void,
        prepareGrant: @Sendable () throws -> Void,
        emptyValueWritten: @Sendable () -> Void,
        completionReceiptObserved: @Sendable () throws -> Bool
    ) throws
    /// Executes a validated action. The result is action-specific (e.g. PNG
    /// bytes held in memory); it never exposes a helper filesystem path.
    func run(_ action: Action) throws -> DesktopService.ActionResult
    /// Whether this system can be used at all. A device that cannot answer must
    /// not arm rather than arming with an unverifiable baseline.
    var isAvailable: Bool { get }
}

/// The real system: `DesktopService` behind the controller's boundary.
public final class DesktopSystem: LockedUseSystem {
    private let desktop: DesktopService
    private let lockScreenAuthorization: LockScreenAuthorizationRequesting
    private let observedLockState = ObservedLockStateGeneration()

    public init(
        desktop: DesktopService,
        lockScreenAuthorization: LockScreenAuthorizationRequesting =
            SystemLockScreenAuthorizationInteractor()
    ) {
        self.desktop = desktop
        self.lockScreenAuthorization = lockScreenAuthorization
    }

    public func isLocked() throws -> Bool { try lockStateSnapshot().isLocked }

    public func lockStateSnapshot() throws -> LockStateSnapshot {
        observedLockState.observe(desktop.screenIsLocked())
    }

    public func lock() throws {
        guard desktop.lockScreen() else {
            throw LockedUseError.systemFailure("could not lock the screen")
        }
        _ = try? lockStateSnapshot()
    }

    public func sinceLastInput() throws -> TimeInterval { desktop.secondsSinceLastInput() }

    public func engageShield() throws {
        guard desktop.engageShield().engaged else {
            throw LockedUseError.systemFailure("could not engage the display shield")
        }
    }

    public func confirmShieldCoverage(timeout: TimeInterval) -> Bool {
        desktop.confirmShieldCoverage(timeout: timeout)
    }

    public func requestUnlockAuthorization(
        authorizationFieldReady: @Sendable () -> Void,
        prepareGrant: @Sendable () throws -> Void,
        emptyValueWritten: @Sendable () -> Void,
        completionReceiptObserved: @Sendable () throws -> Bool
    ) throws {
        // A grant on disk does not itself make loginwindow evaluate the unlock
        // right. Wake the lock screen first, then drive its own authorization
        // control. No password is read, stored, or injected on this path.
        desktop.provokeUnlockAttempt()
        try lockScreenAuthorization.requestAuthorization(
            authorizationFieldReady: authorizationFieldReady,
            prepareGrant: prepareGrant,
            emptyValueWritten: emptyValueWritten,
            completionReceiptObserved: completionReceiptObserved,
            isLocked: { [self] in try isLocked() })
    }

    public func releaseShield() throws { desktop.releaseShield() }

    public func shieldEngaged() -> Bool { desktop.shieldState().engaged }

    public func physicalInputObserved() -> Bool {
        desktop.physicalInputObservedWhileShielded()
    }

    public func run(_ action: Action) throws -> DesktopService.ActionResult {
        try desktop.perform(action)
    }

    public var isAvailable: Bool { true }
}

public enum LockedUseError: Error, Equatable, CustomStringConvertible {
    /// The feature is off in config. Distinct from a runtime failure so the
    /// console can tell "not turned on" from "broken".
    case notEnabled
    /// Computer use is on but Locked Use is not.
    case lockedUseNotEnabled
    /// Locked Use is configured but could not establish a safe baseline, so it
    /// refuses to open windows.
    case notArmed(String)
    /// The privacy shield could not be confirmed.
    case shieldRequired(String)
    /// A person is using the machine.
    case localInput
    /// An action needed an open unlock window and none exists.
    case noWindow
    /// Another turn owns the window, or the previous one is still unwinding.
    case windowBusy(String)
    case systemFailure(String)
    case unsupported

    public var description: String {
        switch self {
        case .notEnabled: return "computer use is not enabled on this device"
        case .lockedUseNotEnabled: return "locked use is not enabled on this device"
        case .notArmed(let detail):
            return detail.isEmpty ? "locked use is not armed" : "locked use is not armed: \(detail)"
        case .shieldRequired(let detail):
            return detail.isEmpty
                ? "display shield could not be engaged"
                : "display shield could not be engaged: \(detail)"
        case .localInput: return "local input detected at the device"
        case .noWindow: return "no open locked-use window for this turn"
        case .windowBusy(let detail): return detail
        case .systemFailure(let detail): return detail
        case .unsupported: return "computer use is not supported on this platform"
        }
    }
}
