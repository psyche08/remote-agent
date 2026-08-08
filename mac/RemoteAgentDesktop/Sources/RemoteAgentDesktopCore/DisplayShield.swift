import AppKit
import CoreGraphics
import Foundation

/// A window that occupies exactly the rect it is given.
///
/// AppKit constrains ordinary windows to a screen's usable area — under the
/// menu bar, inside the safe area. That is right for application windows and
/// wrong for a shield: measured against the window server, the constraint left
/// a border of live desktop around every edge, which is precisely the content
/// the shield exists to hide.
private final class ShieldWindow: NSWindow {
    override func constrainFrameRect(_ frameRect: NSRect, to screen: NSScreen?) -> NSRect {
        frameRect
    }

    // The shield covers the screen; it must never take focus or become the
    // key window, or it would start receiving the keystrokes it is standing
    // in front of.
    override var canBecomeKey: Bool { false }
    override var canBecomeMain: Bool { false }
}

/// One full-screen opaque window per attached screen, above the shielding
/// level, so a bystander cannot read the session while the agent works on a
/// temporarily unlocked desktop.
///
/// The windows are owned by this process. The one-shot script could not hold
/// windows across invocations, so it spawned a host process and tracked it by
/// pid file in $HOME — which meant any process running as this user could write
/// a live pid there and make "the shield is up" true to the controller while
/// the desktop sat uncovered. Ownership removes that forgeable surface: the
/// only thing that can claim coverage is the object holding the windows.
///
/// Every method is main-thread confined because AppKit windows are. Callers
/// arrive from the socket queue, so each entry point hops.
final class DisplayShield {
    private var windows: [NSWindow] = []
    /// How many screens were covered at engage time. A later probe finding
    /// more attached screens means a display was plugged in that the shield
    /// does not cover.
    private var coveredScreens = 0
    /// Set when the screen layout changed under us. The shield is not trusted
    /// again after that: the caller's next state probe reports uncovered, which
    /// relocks — the safe direction.
    private var invalidated = false
    private var observer: NSObjectProtocol?

    /// How long to wait for the window server to report the shield on screen.
    ///
    /// `orderFrontRegardless()` hands the window to the server; it does not
    /// make it visible by the time the call returns. Confirming synchronously
    /// would always fail — and a shield that works, reported as failed, means
    /// Locked Use never opens a window.
    private static let coverageDeadline: TimeInterval = 1.5

    /// Raises the shield windows. Coverage is confirmed separately, because
    /// when the two can happen is decided by whether the screen is locked.
    func engage() -> (engaged: Bool, displays: Int) {
        create()
    }

    /// Waits for the window server to report the shield actually on screen.
    ///
    /// `orderFrontRegardless()` hands a window to the server; it does not make
    /// it visible by the time the call returns, so this polls rather than
    /// asking once.
    ///
    /// It cannot succeed while the screen is locked: the user's session is not
    /// being displayed, so nothing in it is on screen, and the window server
    /// says so. That is not a failure to cover — the lock screen is covering
    /// the session — which is why the controller only requires this before an
    /// unlock when the screen was already unlocked, and immediately after
    /// otherwise.
    @discardableResult
    func confirmCoverage(timeout: TimeInterval) -> Bool {
        let deadline = Date().addingTimeInterval(timeout)
        while Date() < deadline {
            if onMain({ Self.windowServerShowsCoverage(pid: getpid(), screens: NSScreen.screens) }) {
                return true
            }
            // On the main thread the run loop is what commits the windows, so
            // sleeping here would block the very thing being waited for.
            if Thread.isMainThread {
                RunLoop.current.run(until: Date().addingTimeInterval(0.05))
            } else {
                Thread.sleep(forTimeInterval: 0.05)
            }
        }
        return onMain { Self.windowServerShowsCoverage(pid: getpid(), screens: NSScreen.screens) }
    }

    static let defaultCoverageTimeout: TimeInterval = coverageDeadline

    private func create() -> (engaged: Bool, displays: Int) {
        onMain {
            if !self.windows.isEmpty && !self.invalidated {
                return (true, NSScreen.screens.count)
            }
            self.teardown()
            // One shield per NSScreen, and NSScreen is what decides how many
            // there are. CGGetActiveDisplayList disagrees in two ways that both
            // matter: it reports 1 when displays are mirrored, and 0 when they
            // are asleep. Gating on it meant a Mac with sleeping displays
            // refused to raise a shield at all — and "nobody is here, the
            // screens have gone to sleep" is exactly the situation Locked Use
            // exists for, so that closed the main use case.
            let screens = NSScreen.screens
            guard !screens.isEmpty else { return (false, 0) }
            for screen in screens {
                // Both halves of this are load-bearing, and each was wrong on
                // its own when measured against the window server:
                //
                //   * The `screen:` initializer interprets contentRect in that
                //     screen's coordinates, so a secondary display's shield
                //     landed at an origin scaled by its backing factor — far
                //     outside any display. The external monitor showed the live
                //     desktop while the safeguard reported full coverage.
                //   * Plain setFrame is then constrained by AppKit, which insets
                //     the window into the "usable" area. Measured here that left
                //     a 17pt border of live desktop on every edge.
                //
                // So: place it in global coordinates, and refuse the constraint.
                let window = ShieldWindow(
                    contentRect: screen.frame, styleMask: .borderless,
                    backing: .buffered, defer: false)
                window.setFrame(screen.frame, display: true)
                window.level = NSWindow.Level(rawValue: Int(CGShieldingWindowLevel()))
                window.backgroundColor = .black
                window.isOpaque = true
                window.ignoresMouseEvents = false
                window.collectionBehavior = [.canJoinAllSpaces, .stationary, .fullScreenAuxiliary]
                window.orderFrontRegardless()
                self.windows.append(window)
            }
            // Cover every screen or none: partial coverage is not a shield,
            // and reporting it as one would leave a readable display behind a
            // safeguard that believes it is satisfied.
            guard self.windows.count == screens.count else {
                self.teardown()
                return (false, 0)
            }
            // Coverage is confirmed against the window server by the caller,
            // once the windows have had a chance to reach the screen.
            self.coveredScreens = screens.count
            self.invalidated = false
            self.observer = NotificationCenter.default.addObserver(
                forName: NSApplication.didChangeScreenParametersNotification,
                object: nil, queue: .main
            ) { [weak self] _ in
                self?.invalidated = true
            }
            return (true, screens.count)
        }
    }

    func release() {
        onMain { self.teardown() }
    }

    func state() -> (engaged: Bool, displays: Int) {
        onMain {
            // A screen appearing since engage time is a display the shield does
            // not cover — plugging a monitor in must end the window, not be
            // absorbed silently.
            let screens = NSScreen.screens.count
            let live = !self.windows.isEmpty
                && !self.invalidated
                && self.windows.allSatisfy { $0.isVisible }
                && screens == self.coveredScreens
            return (live, screens)
        }
    }

    private func teardown() {
        if let observer {
            NotificationCenter.default.removeObserver(observer)
            self.observer = nil
        }
        for window in windows { window.orderOut(nil) }
        windows.removeAll()
        coveredScreens = 0
        invalidated = false
    }

    /// Whether the window server reports this process owning an on-screen
    /// window the size of every attached display.
    ///
    /// This reads window geometry only — bounds, owner, layer — which is
    /// metadata the window list gives without Screen Recording permission.
    /// Nothing here reads pixels, and it must stay that way: a shield that had
    /// to capture the screen to prove it was covering the screen would be
    /// reading exactly the content it exists to hide.
    ///
    /// Position is compared, not just size. A size-only check passed a shield
    /// whose secondary window sat far outside every display — the external
    /// monitor showed the live desktop while the safeguard reported full
    /// coverage. A window of the right size in the wrong place covers nothing.
    static func windowServerShowsCoverage(pid: pid_t, screens: [NSScreen]) -> Bool {
        guard !screens.isEmpty else { return false }
        // CGWindowList reports a top-left origin measured from the primary
        // display; NSScreen uses a bottom-left origin. The primary is the
        // screen at (0, 0), and its height is the pivot between the two.
        guard let primary = screens.first(where: { $0.frame.origin == .zero }) else { return false }
        let pivot = primary.frame.height
        guard let listed = CGWindowListCopyWindowInfo(.optionOnScreenOnly, kCGNullWindowID)
                as? [[String: Any]] else {
            return false
        }
        let shieldLevel = Int(CGShieldingWindowLevel())
        var covered: [CGRect] = []
        for entry in listed {
            guard let owner = entry[kCGWindowOwnerPID as String] as? pid_t, owner == pid,
                  let layer = entry[kCGWindowLayer as String] as? Int, layer >= shieldLevel,
                  let bounds = entry[kCGWindowBounds as String] as? [String: Any],
                  // Double, not CGFloat: these arrive as NSNumber, and CGFloat
                  // is not a bridged number type, so `as? CGFloat` yields nil
                  // for every window and the check silently finds no coverage
                  // at all — a shield that works, reported as failed.
                  let x = bounds["X"] as? Double,
                  let y = bounds["Y"] as? Double,
                  let width = bounds["Width"] as? Double,
                  let height = bounds["Height"] as? Double else {
                continue
            }
            covered.append(CGRect(x: x, y: y, width: width, height: height))
        }
        let wanted = screens.map { screen -> CGRect in
            let frame = screen.frame
            return CGRect(
                x: frame.minX, y: pivot - frame.maxY,
                width: frame.width, height: frame.height)
        }
        return coverageSatisfied(windows: covered, screens: wanted)
    }

    /// Whether every screen rect is matched by its own window rect.
    ///
    /// Pure and coordinate-agnostic so the matching itself can be tested: this
    /// is the logic that once accepted a right-sized window sitting far outside
    /// every display, and reported an uncovered monitor as shielded.
    ///
    /// Screens are matched by distinct *rect*, not one window per display.
    /// Mirrored displays share a rect and show the same content, so a single
    /// window covering it covers both — and the window server does not list a
    /// fully occluded twin as on screen, so demanding one window per display
    /// would report an actually-covered mirrored setup as uncovered and refuse
    /// to ever open a window.
    static func coverageSatisfied(windows: [CGRect], screens: [CGRect]) -> Bool {
        guard !screens.isEmpty else { return false }
        var distinct: [CGRect] = []
        for screen in screens where !distinct.contains(where: { sameRect($0, screen) }) {
            distinct.append(screen)
        }
        var available = windows
        for want in distinct {
            guard let index = available.firstIndex(where: { sameRect($0, want) }) else {
                return false
            }
            available.remove(at: index)
        }
        return true
    }

    /// Rects are compared with a small tolerance: the window server rounds to
    /// device pixels, and an exact-equality check would reject a shield that is
    /// covering the screen.
    private static func sameRect(_ a: CGRect, _ b: CGRect) -> Bool {
        abs(a.minX - b.minX) < 2 && abs(a.minY - b.minY) < 2
            && abs(a.width - b.width) < 2 && abs(a.height - b.height) < 2
    }

    /// Runs `body` on the main thread and returns its value. The socket queue
    /// is never the main thread, and the main run loop is always running, so
    /// the sync hop cannot deadlock.
    private func onMain<T>(_ body: @escaping () -> T) -> T {
        if Thread.isMainThread { return body() }
        return DispatchQueue.main.sync(execute: body)
    }
}
