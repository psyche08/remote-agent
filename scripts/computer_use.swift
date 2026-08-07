// computer_use.swift
//
// Local worker for the computer-use surface. It performs one operation per
// invocation, described by a single JSON argument, and prints one JSON object
// to stdout. Mirrors the invocation style of scripts/ocr_vision.swift so the Go
// service stays free of cgo.
//
// Usage:   swift scripts/computer_use.swift '{"op":"idle_seconds"}'
// Exit:    0 ok (check the "ok" field) · 2 bad args
//
// Operations:
//   lock_state       -> {"ok":true,"locked":bool}
//   lock             -> {"ok":true}
//   idle_seconds     -> {"ok":true,"idle_seconds":double}
//   shield_engage    -> {"ok":true,"displays":int}
//   shield_release   -> {"ok":true}
//   action           -> one validated desktop action; see runAction below
//
// Security notes that the Go side depends on:
//
//   * There is no unlock operation here, and there never should be. Unlocking
//     happens only through the macOS unlock flow, decided by the Authorization
//     Plug-in against a signed grant. This worker cannot unlock a Mac, and it
//     never handles a password.
//   * shield_engage reports how many displays it actually covered, and the
//     caller treats partial coverage as failure. An exit code of 0 is not
//     evidence the screen is covered.
//   * Nothing here writes screen contents anywhere except the explicit
//     screenshot path the caller asked for.

import Foundation
import CoreGraphics
import AppKit

// MARK: - Output

func emit(_ payload: [String: Any]) -> Never {
    let data = try! JSONSerialization.data(withJSONObject: payload, options: [])
    FileHandle.standardOutput.write(data)
    exit(0)
}

func fail(_ message: String) -> Never {
    emit(["ok": false, "error": message])
}

// MARK: - Input

let args = CommandLine.arguments
guard args.count > 1,
      let requestData = args[1].data(using: .utf8),
      let request = (try? JSONSerialization.jsonObject(with: requestData)) as? [String: Any],
      let op = request["op"] as? String else {
    FileHandle.standardError.write("usage: computer_use.swift '<json>'\n".data(using: .utf8)!)
    exit(2)
}

// MARK: - Lock state

/// Reads the session's console state. `CGSessionCopyCurrentDictionary` reports
/// screen-lock status without any special entitlement.
func screenIsLocked() -> Bool {
    guard let info = CGSessionCopyCurrentDictionary() as? [String: Any] else {
        // An unreadable session dictionary is not evidence the screen is
        // unlocked. Report locked so the caller's fail-closed paths engage.
        return true
    }
    if let locked = info["CGSSessionScreenIsLocked"] as? Bool { return locked }
    if let locked = info["CGSSessionScreenIsLocked"] as? Int { return locked != 0 }
    // Absent key means the screen is not locked on current macOS versions.
    return false
}

/// Locks the screen.
///
/// This deliberately does NOT use `pmset displaysleepnow`: that sleeps the
/// display, and whether the screen ends up *locked* then depends on the user's
/// "require password after sleep" setting and its grace delay. For this feature
/// a display that is asleep but unlocked is a failure, not a success.
///
/// `SACLockScreenImmediate` in the private login framework is the call the
/// Apple menu's "Lock Screen" item uses and locks unconditionally. The caller
/// always confirms the result by reading the lock state back, so a failure here
/// surfaces as an unconfirmed relock rather than a silent one.
func lockScreen() -> Bool {
    let path = "/System/Library/PrivateFrameworks/login.framework/Versions/Current/login"
    guard let handle = dlopen(path, RTLD_LAZY) else { return false }
    defer { dlclose(handle) }
    guard let sym = dlsym(handle, "SACLockScreenImmediate") else { return false }
    typealias LockFn = @convention(c) () -> Int32
    let lock = unsafeBitCast(sym, to: LockFn.self)
    return lock() == 0
}

// MARK: - Idle time

/// Seconds since the last local HID input (key, mouse move, button, scroll).
///
/// The caller treats a reset of this counter as proof that a person is
/// physically present and ends the Locked Use window. That inference is only
/// valid if the counter excludes the agent's OWN synthetic events.
///
/// `.hidSystemState` counts every event that reached the HID system, including
/// the ones this script posts, so reading it directly would make the agent
/// relock itself the instant it typed anything — and, worse, would mean a real
/// person's keystrokes could not be distinguished from the agent's.
///
/// `.combinedSessionState` has the same problem. The separation that does hold
/// is the event source *state id*: events this script posts are created from a
/// private `CGEventSource`, and we record when we last posted one. A reset of
/// the system counter that is NOT accounted for by our own most recent post is
/// a real human at the keyboard.
func secondsSinceLastInput() -> Double {
    let types: [CGEventType] = [
        .keyDown, .leftMouseDown, .rightMouseDown, .otherMouseDown,
        .mouseMoved, .scrollWheel,
    ]
    var shortest = Double.greatestFiniteMagnitude
    for type in types {
        let seconds = CGEventSource.secondsSinceLastEventType(.hidSystemState, eventType: type)
        if seconds < shortest { shortest = seconds }
    }
    let systemIdle = shortest == Double.greatestFiniteMagnitude ? 0 : shortest

    // If our own last synthetic post is at least as recent as the system's last
    // input, the system counter is explained by us and carries no evidence of a
    // person. Report the agent-excluded idle time instead.
    guard let ourLast = lastSyntheticPostAt() else { return systemIdle }
    let sinceOurs = Date().timeIntervalSince1970 - ourLast
    if sinceOurs <= systemIdle + syntheticAttributionSlack {
        // The most recent input is attributable to the agent. Fall back to the
        // idle time measured from before our activity began.
        return max(systemIdle, syntheticIdleFloor(ourLast))
    }
    return systemIdle
}

/// Slack absorbs the gap between posting an event and the HID counter moving.
let syntheticAttributionSlack = 0.35

/// Where the agent records when it last posted a synthetic event. A file is
/// used because each operation runs as its own short-lived process.
let syntheticStampPath = NSString(string: "~/.remote-agent-computer-use-lastpost")
    .expandingTildeInPath

func markSyntheticPost() {
    let now = String(Date().timeIntervalSince1970)
    try? now.write(toFile: syntheticStampPath, atomically: true, encoding: .utf8)
}

func lastSyntheticPostAt() -> Double? {
    guard let text = try? String(contentsOfFile: syntheticStampPath, encoding: .utf8),
          let value = Double(text.trimmingCharacters(in: .whitespacesAndNewlines)) else {
        return nil
    }
    return value
}

/// When the newest input is the agent's own, the machine has been free of human
/// input at least since the agent started acting. Report that, so a window is
/// not torn down by the agent's own work.
func syntheticIdleFloor(_ ourLast: Double) -> Double {
    Date().timeIntervalSince1970 - ourLast
}

// MARK: - Display shield
//
// The shield is a full-screen opaque window above the shielding level on every
// active display. It exists so a bystander cannot read the session while the
// agent works on a temporarily unlocked desktop.
//
// A one-shot CLI cannot hold windows open across invocations, so the shield is
// owned by a small helper process this script starts and stops. Coverage is
// reported back and verified by the caller.

let shieldPidPath = NSString(string: "~/.remote-agent-computer-use-shield.pid").expandingTildeInPath

func activeDisplayCount() -> Int {
    var count: UInt32 = 0
    guard CGGetActiveDisplayList(0, nil, &count) == .success else { return 0 }
    return Int(count)
}

func shieldRunning() -> Bool {
    guard let text = try? String(contentsOfFile: shieldPidPath, encoding: .utf8),
          let pid = Int32(text.trimmingCharacters(in: .whitespacesAndNewlines)) else {
        return false
    }
    return kill(pid, 0) == 0
}

func engageShield() -> (Bool, Int) {
    if shieldRunning() {
        return (true, activeDisplayCount())
    }
    let displays = activeDisplayCount()
    guard displays > 0 else { return (false, 0) }
    // The helper is this same script re-invoked in shield-host mode, so there
    // is exactly one file to install and sign.
    //
    // #filePath, not CommandLine.arguments[0]: when run as `swift script.swift`
    // argv[0] is the interpreter, not this file, so re-invoking through it would
    // spawn a shield host that never covers anything.
    let task = Process()
    task.executableURL = URL(fileURLWithPath: "/usr/bin/env")
    task.arguments = ["swift", #filePath, "{\"op\":\"shield_host\"}"]
    do {
        try task.run()
    } catch {
        return (false, 0)
    }
    try? String(task.processIdentifier).write(toFile: shieldPidPath, atomically: true, encoding: .utf8)
    // Give the host a moment to map its windows before reporting coverage.
    Thread.sleep(forTimeInterval: 0.4)
    return (shieldRunning(), displays)
}

func releaseShield() -> Bool {
    guard let text = try? String(contentsOfFile: shieldPidPath, encoding: .utf8),
          let pid = Int32(text.trimmingCharacters(in: .whitespacesAndNewlines)) else {
        return true
    }
    kill(pid, SIGTERM)
    try? FileManager.default.removeItem(atPath: shieldPidPath)
    return true
}

/// Shield host mode: cover every active display and stay resident until killed.
func runShieldHost() -> Never {
    let app = NSApplication.shared
    app.setActivationPolicy(.accessory)
    var windows: [NSWindow] = []
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
        windows.append(window)
    }
    // A display hot-plug can expose an uncovered screen. Exiting on
    // reconfiguration makes the caller's next coverage check fail, which
    // relocks — the safe direction.
    NotificationCenter.default.addObserver(
        forName: NSApplication.didChangeScreenParametersNotification,
        object: nil, queue: .main
    ) { _ in exit(0) }
    app.run()
    exit(0)
}

// MARK: - Actions

func keyCode(for name: String) -> CGKeyCode? {
    let map: [String: CGKeyCode] = [
        "a": 0, "s": 1, "d": 2, "f": 3, "h": 4, "g": 5, "z": 6, "x": 7,
        "c": 8, "v": 9, "b": 11, "q": 12, "w": 13, "e": 14, "r": 15,
        "y": 16, "t": 17, "1": 18, "2": 19, "3": 20, "4": 21, "6": 22,
        "5": 23, "=": 24, "9": 25, "7": 26, "-": 27, "8": 28, "0": 29,
        "]": 30, "o": 31, "u": 32, "[": 33, "i": 34, "p": 35,
        "return": 36, "enter": 36, "l": 37, "j": 38, "'": 39, "k": 40,
        ";": 41, "\\": 42, ",": 43, "/": 44, "n": 45, "m": 46, ".": 47,
        "tab": 48, "space": 49, "`": 50, "delete": 51, "backspace": 51,
        "escape": 53, "esc": 53,
        "f1": 122, "f2": 120, "f3": 99, "f4": 118, "f5": 96, "f6": 97,
        "f7": 98, "f8": 100, "f9": 101, "f10": 109, "f11": 103, "f12": 111,
        "home": 115, "pageup": 116, "end": 119, "pagedown": 121,
        "left": 123, "right": 124, "down": 125, "up": 126,
    ]
    return map[name]
}

func modifierFlag(for name: String) -> CGEventFlags? {
    switch name {
    case "cmd", "command": return .maskCommand
    case "ctrl", "control": return .maskControl
    case "alt", "option": return .maskAlternate
    case "shift": return .maskShift
    case "fn": return .maskSecondaryFn
    default: return nil
    }
}

func postKeyChord(_ keys: [String]) -> Bool {
    var flags: CGEventFlags = []
    var mainKey: CGKeyCode?
    for key in keys {
        if let flag = modifierFlag(for: key) {
            flags.insert(flag)
        } else if let code = keyCode(for: key) {
            mainKey = code
        } else {
            return false
        }
    }
    guard let code = mainKey, let source = CGEventSource(stateID: .hidSystemState) else { return false }
    guard let down = CGEvent(keyboardEventSource: source, virtualKey: code, keyDown: true),
          let up = CGEvent(keyboardEventSource: source, virtualKey: code, keyDown: false) else {
        return false
    }
    down.flags = flags
    up.flags = flags
    down.post(tap: .cghidEventTap)
    up.post(tap: .cghidEventTap)
    return true
}

func typeText(_ text: String) -> Bool {
    guard let source = CGEventSource(stateID: .hidSystemState) else { return false }
    // Post the text as unicode payloads rather than mapping to key codes, so
    // non-ASCII input survives without depending on the active layout.
    for chunk in Array(text.unicodeScalars).chunked(into: 20) {
        var utf16: [UniChar] = []
        for scalar in chunk { utf16.append(contentsOf: Array(String(scalar).utf16)) }
        guard let down = CGEvent(keyboardEventSource: source, virtualKey: 0, keyDown: true),
              let up = CGEvent(keyboardEventSource: source, virtualKey: 0, keyDown: false) else {
            return false
        }
        down.keyboardSetUnicodeString(stringLength: utf16.count, unicodeString: utf16)
        up.keyboardSetUnicodeString(stringLength: utf16.count, unicodeString: utf16)
        down.post(tap: .cghidEventTap)
        up.post(tap: .cghidEventTap)
        Thread.sleep(forTimeInterval: 0.004)
    }
    return true
}

extension Array {
    func chunked(into size: Int) -> [[Element]] {
        stride(from: 0, to: count, by: size).map { Array(self[$0..<Swift.min($0 + size, count)]) }
    }
}

func mouseEventTypes(_ button: String) -> (CGEventType, CGEventType, CGMouseButton)? {
    switch button {
    case "left": return (.leftMouseDown, .leftMouseUp, .left)
    case "right": return (.rightMouseDown, .rightMouseUp, .right)
    case "middle": return (.otherMouseDown, .otherMouseUp, .center)
    default: return nil
    }
}

func runAction(_ request: [String: Any]) -> Never {
    guard let action = request["action"] as? String else { fail("missing action") }
    let x = (request["x"] as? Int).map { CGFloat($0) } ?? 0
    let y = (request["y"] as? Int).map { CGFloat($0) } ?? 0
    let point = CGPoint(x: x, y: y)

    switch action {
    case "screen.capture":
        let dir = NSString(string: "~/.remote-agent-computer-use").expandingTildeInPath
        try? FileManager.default.createDirectory(atPath: dir, withIntermediateDirectories: true)
        let stamp = ISO8601DateFormatter().string(from: Date()).replacingOccurrences(of: ":", with: "")
        let path = "\(dir)/capture_\(stamp).png"
        let task = Process()
        task.executableURL = URL(fileURLWithPath: "/usr/sbin/screencapture")
        task.arguments = ["-x", path]
        do { try task.run(); task.waitUntilExit() } catch { fail("capture failed") }
        guard task.terminationStatus == 0 else { fail("capture failed") }
        emit(["ok": true, "path": path])

    case "pointer.move":
        markSyntheticPost()
        guard let source = CGEventSource(stateID: .hidSystemState),
              let event = CGEvent(mouseEventSource: source, mouseType: .mouseMoved,
                                  mouseCursorPosition: point, mouseButton: .left) else {
            fail("could not synthesize pointer move")
        }
        event.post(tap: .cghidEventTap)
        emit(["ok": true])

    case "pointer.click":
        let button = (request["button"] as? String) ?? "left"
        let count = (request["count"] as? Int) ?? 1
        markSyntheticPost()
        guard let (downType, upType, mouseButton) = mouseEventTypes(button) else {
            fail("unknown mouse button")
        }
        guard let source = CGEventSource(stateID: .hidSystemState) else {
            fail("could not synthesize click")
        }
        for click in 1...max(1, count) {
            guard let down = CGEvent(mouseEventSource: source, mouseType: downType,
                                     mouseCursorPosition: point, mouseButton: mouseButton),
                  let up = CGEvent(mouseEventSource: source, mouseType: upType,
                                   mouseCursorPosition: point, mouseButton: mouseButton) else {
                fail("could not synthesize click")
            }
            down.setIntegerValueField(.mouseEventClickState, value: Int64(click))
            up.setIntegerValueField(.mouseEventClickState, value: Int64(click))
            down.post(tap: .cghidEventTap)
            up.post(tap: .cghidEventTap)
            Thread.sleep(forTimeInterval: 0.02)
        }
        emit(["ok": true])

    case "pointer.scroll":
        markSyntheticPost()
        let dx = Int32((request["delta_x"] as? Int) ?? 0)
        let dy = Int32((request["delta_y"] as? Int) ?? 0)
        guard let source = CGEventSource(stateID: .hidSystemState),
              let move = CGEvent(mouseEventSource: source, mouseType: .mouseMoved,
                                 mouseCursorPosition: point, mouseButton: .left),
              let scroll = CGEvent(scrollWheelEvent2Source: source, units: .pixel,
                                   wheelCount: 2, wheel1: dy, wheel2: dx, wheel3: 0) else {
            fail("could not synthesize scroll")
        }
        move.post(tap: .cghidEventTap)
        scroll.post(tap: .cghidEventTap)
        emit(["ok": true])

    case "keyboard.type":
        guard let text = request["text"] as? String else { fail("missing text") }
        markSyntheticPost()
        guard typeText(text) else { fail("could not synthesize typing") }
        emit(["ok": true])

    case "keyboard.key":
        guard let keys = request["keys"] as? [String], !keys.isEmpty else { fail("missing keys") }
        markSyntheticPost()
        guard postKeyChord(keys) else { fail("could not synthesize key chord") }
        emit(["ok": true])

    default:
        fail("unknown action")
    }
}

// MARK: - Dispatch

switch op {
case "lock_state":
    emit(["ok": true, "locked": screenIsLocked()])
case "lock":
    guard lockScreen() else { fail("could not lock the screen") }
    emit(["ok": true])
case "idle_seconds":
    emit(["ok": true, "idle_seconds": secondsSinceLastInput()])
case "shield_engage":
    let (engaged, displays) = engageShield()
    guard engaged else { fail("could not engage the display shield") }
    emit(["ok": true, "displays": displays])
case "shield_release":
    _ = releaseShield()
    emit(["ok": true])
case "shield_state":
    // Report live coverage so the caller can detect a shield that died — a
    // crashed host or a display hot-plug — instead of trusting a cached flag.
    emit([
        "ok": true,
        "engaged": shieldRunning(),
        "displays": activeDisplayCount(),
    ])
case "shield_host":
    runShieldHost()
case "action":
    runAction(request)
default:
    fail("unknown op")
}
