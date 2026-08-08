import AppKit
import CoreGraphics
import Foundation

/// The desktop boundary: lock state, human-presence measurement, the display
/// shield, and the closed action vocabulary.
///
/// There is no unlock operation here, and there never should be. Unlocking
/// happens only through the macOS unlock flow, decided by the Authorization
/// Plug-in against a signed grant. This service cannot unlock a Mac and never
/// handles a password.
///
/// `@unchecked Sendable` is a claim this type has to earn, and it does so in
/// two ways: the synthetic-post timestamp is the only mutable state here and is
/// guarded by `lock`, and the shield's state is confined to the main thread by
/// `DisplayShield` itself. Anything added later that is neither is a bug this
/// annotation would hide.
public final class DesktopService: @unchecked Sendable {
    private let lock = NSLock()
    /// When this process last posted a synthetic event. Held in memory, not in
    /// a file: the one-shot script had to persist it, and a file in $HOME is
    /// writable by every process running as this user.
    private var lastSyntheticPost: TimeInterval?
    /// The human-idle clock: at `humanIdleAnchorAt`, input not caused by this
    /// process was `humanIdleAnchorValue` seconds old. It is carried forward
    /// across the agent's own events so its activity does not read as a person
    /// arriving at the keyboard.
    private var humanIdleAnchorAt: TimeInterval?
    private var humanIdleAnchorValue: TimeInterval = 0
    private let shield: DisplayShield

    public init() {
        self.shield = DisplayShield()
    }

    // MARK: - Lock state

    /// Reads the session's console state. `CGSessionCopyCurrentDictionary`
    /// reports screen-lock status without any special entitlement.
    ///
    /// An unreadable session dictionary reports *locked*, because it is not
    /// evidence the screen is unlocked and every caller's fail-closed path
    /// should engage.
    public func screenIsLocked() -> Bool {
        guard let info = CGSessionCopyCurrentDictionary() as? [String: Any] else {
            return true
        }
        if let locked = info["CGSSessionScreenIsLocked"] as? Bool { return locked }
        if let locked = info["CGSSessionScreenIsLocked"] as? Int { return locked != 0 }
        // An absent key means the screen is not locked on current macOS.
        return false
    }

    /// Locks the screen.
    ///
    /// Deliberately not `pmset displaysleepnow`: that sleeps the display, and
    /// whether the screen ends up *locked* then depends on the user's "require
    /// password after sleep" setting and its grace delay. A display that is
    /// asleep but unlocked is a failure here, not a success.
    ///
    /// `SACLockScreenImmediate` is what the Apple menu's Lock Screen item uses
    /// and locks unconditionally. Callers confirm by reading the state back, so
    /// a failure surfaces as an unconfirmed relock rather than a silent one.
    public func lockScreen() -> Bool {
        let path = "/System/Library/PrivateFrameworks/login.framework/Versions/Current/login"
        guard let handle = dlopen(path, RTLD_LAZY) else { return false }
        defer { dlclose(handle) }
        guard let symbol = dlsym(handle, "SACLockScreenImmediate") else { return false }
        typealias LockFunction = @convention(c) () -> Int32
        return unsafeBitCast(symbol, to: LockFunction.self)() == 0
    }

    // MARK: - Human presence

    /// Slack absorbing the gap between posting an event and the HID counter
    /// moving.
    static let syntheticAttributionSlack = 0.35

    /// Seconds since the last local HID input that is *not* attributable to
    /// this process.
    ///
    /// Callers treat a reset of this counter as proof a person is physically
    /// present and end the Locked Use window. That inference only holds if the
    /// counter excludes the agent's own synthetic events: `.hidSystemState`
    /// counts everything that reached the HID system, including what this
    /// process posts, so reading it directly would relock the moment the agent
    /// typed — and would make a real person indistinguishable from the agent.
    public func secondsSinceLastInput() -> Double {
        let systemIdle = Self.systemIdleSeconds()
        let now = Date().timeIntervalSince1970

        lock.lock()
        let ourLast = lastSyntheticPost
        let anchorAt = humanIdleAnchorAt
        let anchorValue = humanIdleAnchorValue
        lock.unlock()

        guard let ourLast, let anchorAt else { return systemIdle }
        let decision = Self.humanIdle(
            systemIdle: systemIdle, now: now, lastSyntheticPost: ourLast,
            anchorAt: anchorAt, anchorValue: anchorValue)
        if decision.humanPresent {
            lock.lock()
            humanIdleAnchorAt = now
            humanIdleAnchorValue = systemIdle
            lock.unlock()
        }
        return decision.idle
    }

    /// Decides how long the machine has been free of input *this process did not
    /// cause*, and whether a person has acted since the agent's last event.
    ///
    /// Pure, so both directions can be tested: no synthetic event can stand in
    /// for a human one, because every event this process posts is attributed to
    /// it by construction.
    ///
    /// The system counter cannot say who caused the newest event, but the timing
    /// can. An input newer than our own last post — by more than the slack
    /// between posting and the counter moving — is not ours.
    ///
    /// When the newest input *is* ours it is no evidence of a person, so the
    /// human clock is carried forward across the agent's activity. Reporting the
    /// system counter here — or the time since our own post, which amounts to
    /// the same number — was the original defect: the agent's first mouse move
    /// looked like someone arriving at the keyboard, so the window closed and
    /// the screen relocked on the first action it ever took. The feature could
    /// not work at all.
    static func humanIdle(
        systemIdle: Double, now: TimeInterval, lastSyntheticPost: TimeInterval,
        anchorAt: TimeInterval, anchorValue: TimeInterval
    ) -> (idle: Double, humanPresent: Bool) {
        if systemIdle + syntheticAttributionSlack < now - lastSyntheticPost {
            return (systemIdle, true)
        }
        return (anchorValue + (now - anchorAt), false)
    }

    private static func systemIdleSeconds() -> Double {
        let types: [CGEventType] = [
            .keyDown, .leftMouseDown, .rightMouseDown, .otherMouseDown,
            .mouseMoved, .scrollWheel,
        ]
        var shortest = Double.greatestFiniteMagnitude
        for type in types {
            let seconds = CGEventSource.secondsSinceLastEventType(.hidSystemState, eventType: type)
            if seconds < shortest { shortest = seconds }
        }
        return shortest == Double.greatestFiniteMagnitude ? 0 : shortest
    }

    /// Records that this process is about to post a synthetic event.
    ///
    /// The anchor takes the *human* idle estimate rather than the raw system
    /// counter, because by the second event in a burst the raw counter is
    /// already explained by our own first one — anchoring to it would collapse
    /// the human clock to zero and reintroduce the defect one action later.
    private func markSyntheticPost() {
        let humanIdle = secondsSinceLastInput()
        let now = Date().timeIntervalSince1970
        lock.lock()
        humanIdleAnchorAt = now
        humanIdleAnchorValue = humanIdle
        lastSyntheticPost = now
        lock.unlock()
    }

    // MARK: - Display shield

    public func engageShield() -> (engaged: Bool, displays: Int) {
        shield.engage()
    }

    public func releaseShield() {
        shield.release()
    }

    /// Live coverage, re-probed on every call. A cached flag could not detect a
    /// shield that died — a crashed host, or a display hot-plugged in beside an
    /// uncovered screen — and the safeguard built on it would never fire.
    public func shieldState() -> (engaged: Bool, displays: Int) {
        shield.state()
    }

    // MARK: - Actions

    public enum ActionResult: Sendable {
        case done
        case captured(path: String)
    }

    public func perform(_ action: Action) throws -> ActionResult {
        switch action.id {
        case .screenCapture:
            return .captured(path: try capture())
        case .pointerMove:
            markSyntheticPost()
            try movePointer(to: point(action))
            return .done
        case .pointerClick:
            markSyntheticPost()
            try click(action)
            return .done
        case .pointerScroll:
            markSyntheticPost()
            try scroll(action)
            return .done
        case .keyboardType:
            markSyntheticPost()
            try type(action.text)
            return .done
        case .keyboardKey:
            markSyntheticPost()
            try chord(action.keys)
            return .done
        }
    }

    private func point(_ action: Action) -> CGPoint {
        CGPoint(x: CGFloat(action.x), y: CGFloat(action.y))
    }

    private func eventSource() throws -> CGEventSource {
        guard let source = CGEventSource(stateID: .hidSystemState) else {
            throw ActionError("could not create an event source")
        }
        return source
    }

    private func capture() throws -> String {
        let directory = NSString(string: "~/.remote-agent-computer-use").expandingTildeInPath
        try? FileManager.default.createDirectory(
            atPath: directory, withIntermediateDirectories: true,
            attributes: [.posixPermissions: 0o700])
        let stamp = ISO8601DateFormatter().string(from: Date())
            .replacingOccurrences(of: ":", with: "")
        let path = "\(directory)/capture_\(stamp).png"
        let task = Process()
        task.executableURL = URL(fileURLWithPath: "/usr/sbin/screencapture")
        task.arguments = ["-x", path]
        do {
            try task.run()
            task.waitUntilExit()
        } catch {
            throw ActionError("capture failed")
        }
        guard task.terminationStatus == 0 else { throw ActionError("capture failed") }
        return path
    }

    private func movePointer(to position: CGPoint) throws {
        let source = try eventSource()
        guard let event = CGEvent(
            mouseEventSource: source, mouseType: .mouseMoved,
            mouseCursorPosition: position, mouseButton: .left) else {
            throw ActionError("could not synthesize pointer move")
        }
        event.post(tap: .cghidEventTap)
    }

    private func click(_ action: Action) throws {
        let types: (CGEventType, CGEventType, CGMouseButton)
        switch action.button {
        case .left: types = (.leftMouseDown, .leftMouseUp, .left)
        case .right: types = (.rightMouseDown, .rightMouseUp, .right)
        case .middle: types = (.otherMouseDown, .otherMouseUp, .center)
        }
        let source = try eventSource()
        let position = point(action)
        for index in 1...max(1, action.count) {
            guard let down = CGEvent(
                    mouseEventSource: source, mouseType: types.0,
                    mouseCursorPosition: position, mouseButton: types.2),
                  let up = CGEvent(
                    mouseEventSource: source, mouseType: types.1,
                    mouseCursorPosition: position, mouseButton: types.2) else {
                throw ActionError("could not synthesize click")
            }
            down.setIntegerValueField(.mouseEventClickState, value: Int64(index))
            up.setIntegerValueField(.mouseEventClickState, value: Int64(index))
            down.post(tap: .cghidEventTap)
            up.post(tap: .cghidEventTap)
            Thread.sleep(forTimeInterval: 0.02)
        }
    }

    private func scroll(_ action: Action) throws {
        let source = try eventSource()
        guard let move = CGEvent(
                mouseEventSource: source, mouseType: .mouseMoved,
                mouseCursorPosition: point(action), mouseButton: .left),
              let wheel = CGEvent(
                scrollWheelEvent2Source: source, units: .pixel, wheelCount: 2,
                wheel1: Int32(action.deltaY), wheel2: Int32(action.deltaX), wheel3: 0) else {
            throw ActionError("could not synthesize scroll")
        }
        move.post(tap: .cghidEventTap)
        wheel.post(tap: .cghidEventTap)
    }

    /// Posts text as unicode payloads rather than mapping to key codes, so
    /// non-ASCII input survives without depending on the active layout.
    private func type(_ text: String) throws {
        let source = try eventSource()
        for chunk in Array(text.unicodeScalars).chunked(into: 20) {
            var utf16: [UniChar] = []
            for scalar in chunk { utf16.append(contentsOf: Array(String(scalar).utf16)) }
            guard let down = CGEvent(keyboardEventSource: source, virtualKey: 0, keyDown: true),
                  let up = CGEvent(keyboardEventSource: source, virtualKey: 0, keyDown: false) else {
                throw ActionError("could not synthesize typing")
            }
            down.keyboardSetUnicodeString(stringLength: utf16.count, unicodeString: utf16)
            up.keyboardSetUnicodeString(stringLength: utf16.count, unicodeString: utf16)
            down.post(tap: .cghidEventTap)
            up.post(tap: .cghidEventTap)
            Thread.sleep(forTimeInterval: 0.004)
        }
    }

    private func chord(_ keys: [String]) throws {
        var flags: CGEventFlags = []
        var mainKey: CGKeyCode?
        for key in keys {
            if let flag = KeyMap.modifier(for: key) {
                flags.insert(flag)
            } else if let code = KeyMap.code(for: key) {
                mainKey = code
            } else {
                throw ActionError("unknown key: \(key)")
            }
        }
        guard let code = mainKey else { throw ActionError("key chord has no non-modifier key") }
        let source = try eventSource()
        guard let down = CGEvent(keyboardEventSource: source, virtualKey: code, keyDown: true),
              let up = CGEvent(keyboardEventSource: source, virtualKey: code, keyDown: false) else {
            throw ActionError("could not synthesize key chord")
        }
        down.flags = flags
        up.flags = flags
        down.post(tap: .cghidEventTap)
        up.post(tap: .cghidEventTap)
    }
}

extension Array {
    func chunked(into size: Int) -> [[Element]] {
        stride(from: 0, to: count, by: size).map {
            Array(self[$0..<Swift.min($0 + size, count)])
        }
    }
}
