import Foundation

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
    /// Executes a validated action. The result is action-specific (e.g. a
    /// screenshot path); it never carries secrets.
    func run(_ action: Action) throws -> DesktopService.ActionResult
    /// Whether this system can be used at all. A device that cannot answer must
    /// not arm rather than arming with an unverifiable baseline.
    var isAvailable: Bool { get }
}

/// The real system: `DesktopService` behind the controller's boundary.
public final class DesktopSystem: LockedUseSystem {
    private let desktop: DesktopService

    public init(desktop: DesktopService) {
        self.desktop = desktop
    }

    public func isLocked() throws -> Bool { desktop.screenIsLocked() }

    public func lock() throws {
        guard desktop.lockScreen() else {
            throw LockedUseError.systemFailure("could not lock the screen")
        }
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

    public func releaseShield() throws { desktop.releaseShield() }

    public func shieldEngaged() -> Bool { desktop.shieldState().engaged }

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
