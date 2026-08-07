import AppKit
import CoreGraphics
import Foundation

/// A full-screen opaque window above the shielding level on every active
/// display, so a bystander cannot read the session while the agent works on a
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
    /// How many displays were covered at engage time. A later probe finding
    /// more attached displays means a monitor was plugged in that the shield
    /// does not cover.
    private var coveredDisplays = 0
    /// Set when the screen layout changed under us. The shield is not trusted
    /// again after that: the caller's next state probe reports uncovered, which
    /// relocks — the safe direction.
    private var invalidated = false
    private var observer: NSObjectProtocol?

    func engage() -> (engaged: Bool, displays: Int) {
        onMain {
            if !self.windows.isEmpty && !self.invalidated {
                return (true, Self.activeDisplayCount())
            }
            self.teardown()
            let displays = Self.activeDisplayCount()
            guard displays > 0 else { return (false, 0) }
            for screen in NSScreen.screens {
                let window = NSWindow(
                    contentRect: screen.frame, styleMask: .borderless,
                    backing: .buffered, defer: false, screen: screen)
                window.level = NSWindow.Level(rawValue: Int(CGShieldingWindowLevel()))
                window.backgroundColor = .black
                window.isOpaque = true
                window.ignoresMouseEvents = false
                window.collectionBehavior = [.canJoinAllSpaces, .stationary, .fullScreenAuxiliary]
                window.orderFrontRegardless()
                self.windows.append(window)
            }
            // Cover every display or none: partial coverage is not a shield,
            // and reporting it as one would leave a readable screen behind a
            // safeguard that believes it is satisfied.
            guard self.windows.count >= displays else {
                self.teardown()
                return (false, 0)
            }
            self.coveredDisplays = displays
            self.invalidated = false
            self.observer = NotificationCenter.default.addObserver(
                forName: NSApplication.didChangeScreenParametersNotification,
                object: nil, queue: .main
            ) { [weak self] _ in
                self?.invalidated = true
            }
            return (true, displays)
        }
    }

    func release() {
        onMain { self.teardown() }
    }

    func state() -> (engaged: Bool, displays: Int) {
        onMain {
            let displays = Self.activeDisplayCount()
            let live = !self.windows.isEmpty
                && !self.invalidated
                && self.windows.allSatisfy { $0.isVisible }
                && displays == self.coveredDisplays
            return (live, displays)
        }
    }

    private func teardown() {
        if let observer {
            NotificationCenter.default.removeObserver(observer)
            self.observer = nil
        }
        for window in windows { window.orderOut(nil) }
        windows.removeAll()
        coveredDisplays = 0
        invalidated = false
    }

    private static func activeDisplayCount() -> Int {
        var count: UInt32 = 0
        guard CGGetActiveDisplayList(0, nil, &count) == .success else { return 0 }
        return Int(count)
    }

    /// Runs `body` on the main thread and returns its value. The socket queue
    /// is never the main thread, and the main run loop is always running, so
    /// the sync hop cannot deadlock.
    private func onMain<T>(_ body: @escaping () -> T) -> T {
        if Thread.isMainThread { return body() }
        return DispatchQueue.main.sync(execute: body)
    }
}
