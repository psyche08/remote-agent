import AppKit
import CoreGraphics
import Foundation
import ImageIO
import ScreenCaptureKit

/// Thread-safe bridge from ScreenCaptureKit's async API to the synchronous
/// action protocol served by the helper socket.
private final class CaptureOutcome: @unchecked Sendable {
    private let lock = NSLock()
    private var result: Result<Data, Error>?

    func store(_ value: Result<Data, Error>) {
        lock.lock()
        result = value
        lock.unlock()
    }

    func take() -> Result<Data, Error>? {
        lock.lock()
        defer { lock.unlock() }
        return result
    }
}

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

    /// Posts a single synthetic event to make the login window notice user
    /// activity.
    ///
    /// Publishing a grant does not make macOS evaluate anything. The unlock
    /// begins when loginwindow sees the user-activity state go active — its own
    /// log calls it "user event received, start an unlock with 'active user' as
    /// the reason" — and only then is the authorization right evaluated, which
    /// is where a grant can authorize anything at all.
    ///
    /// What matters is the *transition*, not the event: a machine already
    /// considered active produces no transition and no unlock. The controller
    /// requires the device to be idle before opening a window, so by the time
    /// this runs the state is inactive and this one event flips it.
    ///
    /// It is marked as ours like any other synthetic post, so the presence
    /// safeguard does not read the agent knocking on the door as a person
    /// arriving at it.
    public func provokeUnlockAttempt() throws {
        guard let cursorEvent = CGEvent(source: nil) else {
            throw ActionError("could not read the current pointer position")
        }
        let current = cursorEvent.location
        // Active displays may be reported as an empty list while the screens
        // are asleep — precisely the unattended Locked Use case. Online
        // display geometry remains available and is enough to constrain this
        // one wake move without changing capture/pointer active-display rules.
        let displayFrames = try onlineDisplayFrames()
        let position = try Self.wakeProbePoint(
            current: current, displayFrames: displayFrames)
        let source = try eventSource()
        // A move rather than a click or a keystroke: at the login window a
        // click lands somewhere and a keystroke lands in the password field.
        // A move is the least the system can be asked to notice.
        guard let event = CGEvent(
            mouseEventSource: source, mouseType: .mouseMoved,
            mouseCursorPosition: position, mouseButton: .left) else {
            throw ActionError("could not create the lock-screen wake event")
        }
        // Core Graphics owns the final event coordinate. Re-read it before
        // attributing or posting anything: a rounded/no-op coordinate or a
        // move that escaped the cursor's exact display is not a valid wake
        // probe and must expose no grant window.
        let encodedPosition = event.location
        guard encodedPosition == position, encodedPosition != current,
              let currentDisplay = displayFrames.first(where: {
                  Self.displayFrame($0, contains: current)
              }),
              Self.displayFrame(currentDisplay, contains: encodedPosition) else {
            throw ActionError("lock-screen wake event did not preserve its display")
        }
        // Attribute the post only once an event is ready. On every successful
        // path this remains ordered before InputGuard's marker and the HID
        // post; failures do not leave behind a synthetic-input timestamp for
        // an event that never existed.
        markSyntheticPost()
        postAgentEvent(event)
    }

    /// Selects a one-point move that cannot collapse into the current cursor
    /// position. Reposting the old fixed `(1, 1)` coordinate becomes a spatial
    /// no-op after the first attempt, and loginwindow then sees no inactive to
    /// active transition even though TCC accepts the event.
    ///
    /// The cursor and destination must be on the same exact display. A cursor
    /// reported in a layout gap, invalid geometry, or a display too narrow to
    /// admit a one-point move fails closed rather than warping across screens.
    static func wakeProbePoint(
        current: CGPoint, displayFrames: [CGRect]
    ) throws -> CGPoint {
        guard current.x.isFinite, current.y.isFinite,
              validDisplayBounds(displayFrames) != nil else {
            throw ActionError("could not choose a lock-screen wake position")
        }

        guard let currentDisplay = displayFrames.first(where: {
            Self.displayFrame($0, contains: current)
        }) else {
            throw ActionError("could not choose a lock-screen wake position")
        }
        let candidates = [
            CGPoint(x: current.x + 1, y: current.y),
            CGPoint(x: current.x - 1, y: current.y),
            CGPoint(x: current.x, y: current.y + 1),
            CGPoint(x: current.x, y: current.y - 1),
        ]
        guard let point = candidates.first(where: {
            $0 != current && Self.displayFrame(currentDisplay, contains: $0)
        }) else {
            throw ActionError("online display cannot admit a one-point wake move")
        }
        return point
    }

    // MARK: - Display shield

    public func engageShield() -> (engaged: Bool, displays: Int) {
        shield.engage()
    }

    /// Waits for the window server to confirm the shield is actually on screen.
    ///
    /// Separate from engaging because the two cannot always happen together:
    /// while the screen is locked the user's session is not displayed, so
    /// nothing in it can be confirmed on screen — see the controller for which
    /// side of the unlock this is required on.
    public func confirmShieldCoverage(timeout: TimeInterval) -> Bool {
        shield.confirmCoverage(timeout: timeout)
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

    public func physicalInputObservedWhileShielded() -> Bool {
        shield.physicalInputObserved()
    }

    // MARK: - Actions

    public enum ActionResult: Sendable {
        case done
        case captured(data: Data, mediaType: String)
    }

    public func perform(_ action: Action) throws -> ActionResult {
        switch action.id {
        case .screenCapture:
            return .captured(data: try capturePNG(), mediaType: "image/png")
        case .pointerMove:
            markSyntheticPost()
            try movePointer(to: try point(action))
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

    private func point(_ action: Action) throws -> CGPoint {
        let raw = CGPoint(x: CGFloat(action.x), y: CGFloat(action.y))
        switch action.coordinateSpace {
        case .global:
            return raw
        case .screenshot:
            return try Self.globalPoint(
                forScreenshotPoint: raw, displayFrames: try activeDisplayFrames())
        }
    }

    /// Converts the top-left-origin point in the composite PNG into the same
    /// global display coordinate space Core Graphics mouse events consume.
    /// The union may start at a negative X or Y when a display is left of or
    /// above the primary display. Points in a rectangular gap between displays
    /// are refused instead of becoming a click on an unrelated screen.
    static func globalPoint(
        forScreenshotPoint point: CGPoint, displayFrames: [CGRect]
    ) throws -> CGPoint {
        guard point.x.isFinite, point.y.isFinite, point.x >= 0, point.y >= 0,
              let bounds = validDisplayBounds(displayFrames),
              point.x < bounds.width, point.y < bounds.height else {
            throw ActionError("screenshot coordinates are outside the display composite")
        }
        let global = CGPoint(x: bounds.minX + point.x, y: bounds.minY + point.y)
        guard displayFrames.contains(where: { frame in
            frame.width > 0 && frame.height > 0 &&
                global.x >= frame.minX && global.x < frame.maxX &&
                global.y >= frame.minY && global.y < frame.maxY
        }) else {
            throw ActionError("screenshot coordinates fall between displays")
        }
        return global
    }

    private static func validDisplayBounds(_ frames: [CGRect]) -> CGRect? {
        guard let first = frames.first,
              first.minX.isFinite, first.minY.isFinite,
              first.width.isFinite, first.height.isFinite,
              first.maxX.isFinite, first.maxY.isFinite,
              first.width > 0, first.height > 0 else { return nil }
        var bounds = first
        for frame in frames.dropFirst() {
            guard frame.minX.isFinite, frame.minY.isFinite,
                  frame.width.isFinite, frame.height.isFinite,
                  frame.maxX.isFinite, frame.maxY.isFinite,
                  frame.width > 0, frame.height > 0 else { return nil }
            bounds = bounds.union(frame)
        }
        guard bounds.minX.isFinite, bounds.minY.isFinite,
              bounds.width.isFinite, bounds.height.isFinite,
              bounds.maxX.isFinite, bounds.maxY.isFinite else { return nil }
        return bounds
    }

    private static func displayFrame(_ frame: CGRect, contains point: CGPoint) -> Bool {
        point.x.isFinite && point.y.isFinite &&
            point.x >= frame.minX && point.x < frame.maxX &&
            point.y >= frame.minY && point.y < frame.maxY
    }

    private func activeDisplayFrames() throws -> [CGRect] {
        var count: UInt32 = 0
        guard CGGetActiveDisplayList(0, nil, &count) == .success, count > 0 else {
            throw ActionError("could not read active display geometry")
        }
        var displayIDs = [CGDirectDisplayID](repeating: 0, count: Int(count))
        var filled: UInt32 = 0
        let result = displayIDs.withUnsafeMutableBufferPointer { buffer in
            CGGetActiveDisplayList(count, buffer.baseAddress, &filled)
        }
        guard result == .success, filled > 0, filled <= count else {
            throw ActionError("could not read active display geometry")
        }
        let frames = displayIDs.prefix(Int(filled)).map(CGDisplayBounds)
        guard Self.validDisplayBounds(frames) != nil else {
            throw ActionError("active display geometry is invalid")
        }
        return frames
    }

    private func onlineDisplayFrames() throws -> [CGRect] {
        var count: UInt32 = 0
        guard CGGetOnlineDisplayList(0, nil, &count) == .success, count > 0 else {
            throw ActionError("could not read online display geometry")
        }
        var displayIDs = [CGDirectDisplayID](repeating: 0, count: Int(count))
        var filled: UInt32 = 0
        let result = displayIDs.withUnsafeMutableBufferPointer { buffer in
            CGGetOnlineDisplayList(count, buffer.baseAddress, &filled)
        }
        guard result == .success, filled > 0, filled <= count else {
            throw ActionError("could not read online display geometry")
        }
        // Mirrored outputs can report identical bounds. They are one pointer
        // coordinate space, so retain each exact frame only once.
        var frames: [CGRect] = []
        for displayID in displayIDs.prefix(Int(filled)) {
            let frame = CGDisplayBounds(displayID)
            if !frames.contains(frame) { frames.append(frame) }
        }
        guard Self.validDisplayBounds(frames) != nil else {
            throw ActionError("online display geometry is invalid")
        }
        return frames
    }

    private func eventSource() throws -> CGEventSource {
        guard let source = CGEventSource(stateID: .hidSystemState) else {
            throw ActionError("could not create an event source")
        }
        return source
    }

    /// Every synthetic keyboard and pointer event takes this single path. The
    /// input guard drops unmarked events while the Locked Use shield is up, so
    /// posting directly would make the agent indistinguishable from a person at
    /// the device and would suppress its own action.
    private func postAgentEvent(_ event: CGEvent) {
        InputGuard.markAgentEvent(event)
        event.post(tap: .cghidEventTap)
    }

    /// Captures through ScreenCaptureKit and returns the PNG bytes directly.
    ///
    /// Returning a filesystem path created a same-uid TOCTOU boundary between
    /// this signed helper and its Go client: another process could replace a
    /// predictable capture before the client read it. Keeping the frame in
    /// memory means the only bytes that cross the signed UDS are the bytes this
    /// process encoded. The content filter explicitly excludes this helper, so
    /// the physical-display shield remains black locally but is absent from the
    /// model's frame.
    private func capturePNG() throws -> Data {
        guard #available(macOS 14.0, *) else {
            throw ActionError("screen capture requires macOS 14 or newer")
        }

        let outcome = CaptureOutcome()
        let completed = DispatchSemaphore(value: 0)
        Task.detached(priority: .userInitiated) {
            do {
                outcome.store(.success(try await Self.capturePNGAsync()))
            } catch {
                outcome.store(.failure(error))
            }
            completed.signal()
        }
        guard completed.wait(timeout: .now() + 15) == .success else {
            throw ActionError("screen capture timed out")
        }
        switch outcome.take() {
        case .success(let data): return data
        case .failure(let error as ActionError): throw error
        case .failure(let error): throw ActionError("screen capture failed: \(error)")
        case nil: throw ActionError("screen capture produced no result")
        }
    }

    @available(macOS 14.0, *)
    private static func capturePNGAsync() async throws -> Data {
        let content = try await SCShareableContent.excludingDesktopWindows(
            false, onScreenWindowsOnly: true)
        guard !content.displays.isEmpty else {
            throw ActionError("screen capture found no displays")
        }

        let ownApplication = content.applications.first { $0.processID == getpid() }
        let ownWindows = content.windows.filter { $0.owningApplication?.processID == getpid() }
        var frames: [(CGRect, CGImage)] = []
        for display in content.displays {
            let filter: SCContentFilter
            if let ownApplication {
                filter = SCContentFilter(
                    display: display,
                    excludingApplications: [ownApplication],
                    exceptingWindows: [])
            } else {
                // A command-line LaunchAgent may not be represented as a
                // shareable application. Exclude every shareable window
                // attributed to our pid in that case, including the shield.
                filter = SCContentFilter(display: display, excludingWindows: ownWindows)
            }
            let configuration = SCStreamConfiguration()
            configuration.width = max(1, display.width)
            configuration.height = max(1, display.height)
            configuration.showsCursor = true
            let image = try await SCScreenshotManager.captureImage(
                contentFilter: filter, configuration: configuration)
            // CGDisplayBounds is the coordinate space used by CGEvent. Using
            // it here keeps the composite and later screenshot-coordinate
            // pointer actions on one geometry contract, including Retina and
            // negative-origin secondary displays.
            frames.append((CGDisplayBounds(display.displayID), image))
        }
        return try encodeCompositePNG(frames)
    }

    private static let maxCapturePixels = 100_000_000
    private static let maxCaptureBytes = 25 * 1024 * 1024

    static func encodeCompositePNG(_ frames: [(CGRect, CGImage)]) throws -> Data {
        guard let bounds = validDisplayBounds(frames.map(\.0)) else {
            throw ActionError("screen capture produced no frames")
        }
        let width = Int(ceil(bounds.width))
        let height = Int(ceil(bounds.height))
        guard width > 0, height > 0,
              width <= Self.maxCapturePixels / height else {
            throw ActionError("screen capture dimensions are unsafe")
        }
        guard let colorSpace = CGColorSpace(name: CGColorSpace.sRGB),
              let context = CGContext(
                data: nil, width: width, height: height, bitsPerComponent: 8,
                bytesPerRow: 0, space: colorSpace,
                bitmapInfo: CGImageAlphaInfo.premultipliedLast.rawValue)
        else { throw ActionError("screen capture could not allocate an image") }

        context.setFillColor(CGColor(gray: 0, alpha: 1))
        context.fill(CGRect(x: 0, y: 0, width: width, height: height))
        context.interpolationQuality = .high
        for (frame, image) in frames {
            context.draw(image, in: compositeDestination(for: frame, in: bounds))
        }
        guard let image = context.makeImage() else {
            throw ActionError("screen capture could not compose the displays")
        }
        let encoded = NSMutableData()
        guard let destination = CGImageDestinationCreateWithData(
                encoded, "public.png" as CFString, 1, nil) else {
            throw ActionError("screen capture could not create a PNG encoder")
        }
        CGImageDestinationAddImage(destination, image, nil)
        guard CGImageDestinationFinalize(destination) else {
            throw ActionError("screen capture could not encode PNG")
        }
        let data = encoded as Data
        let pngMagic = Data([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a])
        guard data.starts(with: pngMagic), data.count <= Self.maxCaptureBytes else {
            throw ActionError("screen capture PNG is invalid or too large")
        }
        return data
    }

    static func compositeDestination(for frame: CGRect, in bounds: CGRect) -> CGRect {
        CGRect(
            x: frame.minX - bounds.minX,
            // Display/global Y grows down from the primary display's top-left;
            // bitmap-context Y grows up. This places a display above the
            // primary at the top of the encoded PNG.
            y: bounds.maxY - frame.maxY,
            width: frame.width,
            height: frame.height)
    }

    private func movePointer(to position: CGPoint) throws {
        let source = try eventSource()
        guard let event = CGEvent(
            mouseEventSource: source, mouseType: .mouseMoved,
            mouseCursorPosition: position, mouseButton: .left) else {
            throw ActionError("could not synthesize pointer move")
        }
        postAgentEvent(event)
    }

    private func click(_ action: Action) throws {
        let types: (CGEventType, CGEventType, CGMouseButton)
        switch action.button {
        case .left: types = (.leftMouseDown, .leftMouseUp, .left)
        case .right: types = (.rightMouseDown, .rightMouseUp, .right)
        case .middle: types = (.otherMouseDown, .otherMouseUp, .center)
        }
        let source = try eventSource()
        let position = try point(action)
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
            postAgentEvent(down)
            postAgentEvent(up)
            Thread.sleep(forTimeInterval: 0.02)
        }
    }

    private func scroll(_ action: Action) throws {
        let source = try eventSource()
        guard let move = CGEvent(
                mouseEventSource: source, mouseType: .mouseMoved,
                mouseCursorPosition: try point(action), mouseButton: .left),
              let wheel = CGEvent(
                scrollWheelEvent2Source: source, units: .pixel, wheelCount: 2,
                wheel1: Int32(action.deltaY), wheel2: Int32(action.deltaX), wheel3: 0) else {
            throw ActionError("could not synthesize scroll")
        }
        postAgentEvent(move)
        postAgentEvent(wheel)
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
            postAgentEvent(down)
            postAgentEvent(up)
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
        postAgentEvent(down)
        postAgentEvent(up)
    }
}

extension Array {
    func chunked(into size: Int) -> [[Element]] {
        stride(from: 0, to: count, by: size).map {
            Array(self[$0..<Swift.min($0 + size, count)])
        }
    }
}
