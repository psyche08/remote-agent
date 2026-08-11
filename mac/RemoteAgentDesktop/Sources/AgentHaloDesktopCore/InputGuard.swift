import CoreGraphics
import Foundation

/// The decision made for one event observed while the privacy shield is up.
///
/// Classification is deliberately pure. The real event tap supplies the type
/// and marker, while tests can exercise every branch without installing a tap
/// or asking macOS for Accessibility permission.
enum InputEventDisposition: Equatable {
    case allowAgentEvent
    case suppressLocalInput
    case recoverDisabledTap
    case ignore
}

struct InputEventClassifier {
    /// The complete keyboard/pointer vocabulary represented by CGEvent taps.
    /// The production tap derives its mask from this same list so a classified
    /// event can never be accidentally omitted at the observation boundary.
    static let guardedTypes: [CGEventType] = [
        .leftMouseDown, .leftMouseUp,
        .rightMouseDown, .rightMouseUp,
        .mouseMoved, .leftMouseDragged, .rightMouseDragged,
        .keyDown, .keyUp, .flagsChanged,
        .scrollWheel, .tabletPointer, .tabletProximity,
        .otherMouseDown, .otherMouseUp, .otherMouseDragged,
    ]

    /// Only events carrying the marker minted by this helper *and* reported by
    /// Core Graphics with this helper's Unix pid may reach the temporarily
    /// unlocked desktop. This reliably separates hardware/unmarked input and
    /// ordinary synthetic input; it is defense in depth, not a cryptographic
    /// identity proof against a hostile same-user process with event-injection
    /// permission (CGEvent fields are not documented as unforgeable).
    static func disposition(
        type: CGEventType, eventMarker: Int64, agentMarker: Int64,
        eventSourcePID: Int64 = Int64(getpid()), agentPID: Int64 = Int64(getpid())
    ) -> InputEventDisposition {
        if type == .tapDisabledByTimeout || type == .tapDisabledByUserInput {
            return .recoverDisabledTap
        }
        guard guards(type) else { return .ignore }
        // Zero is what ordinary hardware input carries. Even a misconfigured
        // injected guard must not turn that default into an allow marker.
        // The marker is observable and therefore cannot authenticate an event
        // by itself. Require Core Graphics' reported source pid too, which
        // rejects ordinary events that copy only the marker. Do not treat that
        // reported field as code-signing or kernel-backed peer authentication.
        return agentMarker != 0 && eventMarker == agentMarker
            && agentPID > 0 && eventSourcePID == agentPID
            ? .allowAgentEvent
            : .suppressLocalInput
    }

    static func guards(_ type: CGEventType) -> Bool {
        guardedTypes.contains(type)
    }
}

/// Small boundary around a mutable CGEvent tap. Tests inject a fake so tap
/// creation, disable notifications, failed re-enables, and teardown are all
/// deterministic and require no TCC state.
protocol InputTapControlling: AnyObject {
    var isEnabled: Bool { get }
    func setEnabled(_ enabled: Bool)
    func invalidate()
}

typealias InputTapHandler = (CGEventType, Int64, Int64) -> Bool
typealias InputTapFactory = (@escaping InputTapHandler) -> InputTapControlling?

/// Suppresses physical keyboard and pointer input for the lifetime of a Locked
/// Use display shield while allowing events explicitly marked by this helper.
///
/// A modifying event tap is part of the shield, not an optional enhancement:
/// if it cannot be created, becomes disabled, or cannot be re-enabled, the
/// guard reports inactive. `DisplayShield` folds that into its live state, so
/// the controller's existing safeguard closes the window and relocks.
final class InputGuard: @unchecked Sendable {
    /// Per-process and deliberately non-secret. Randomizing it prevents an
    /// accidental collision with another event producer. The signed helper
    /// socket protects requests, but cannot authenticate independently posted
    /// CGEvents; that distinction is part of the documented threat boundary.
    static let agentEventMarker = Int64.random(in: 1...Int64.max)

    private let agentMarker: Int64
    private let agentPID: Int64
    private let tapFactory: InputTapFactory
    private let lock = NSLock()
    private var tap: InputTapControlling?
    private var healthy = false
    /// Sticky for one shield lifetime. The controller polls this independently
    /// of the HID idle clock so an event that we successfully suppress still
    /// ends Locked Use immediately instead of disappearing from observation.
    private var localInputObserved = false

    init(
        agentMarker: Int64 = InputGuard.agentEventMarker,
        agentPID: Int64 = Int64(getpid()),
        tapFactory: InputTapFactory? = nil
    ) {
        self.agentMarker = agentMarker
        self.agentPID = agentPID
        self.tapFactory = tapFactory ?? { SystemInputTap(handler: $0) }
    }

    deinit { stop() }

    /// Marks an event before it is posted at the HID tap. The marker survives
    /// delivery to the session event tap. It is combined with Core Graphics'
    /// reported source pid; the observable fields are never authority alone.
    static func markAgentEvent(_ event: CGEvent) {
        event.setIntegerValueField(.eventSourceUserData, value: agentEventMarker)
    }

    @discardableResult
    func start() -> Bool {
        lock.lock()
        let existing = tap
        let alreadyHealthy = healthy
        lock.unlock()
        if alreadyHealthy, existing?.isEnabled == true { return true }

        stop()
        lock.lock()
        localInputObserved = false
        lock.unlock()
        guard let created = tapFactory({ [weak self] type, marker, sourcePID in
            self?.handle(type: type, marker: marker, sourcePID: sourcePID) ?? false
        }) else {
            return false
        }

        // Publish the tap before enabling it so a disable notification delivered
        // immediately after enable can recover the same object.
        lock.lock()
        tap = created
        healthy = false
        lock.unlock()

        created.setEnabled(true)
        let enabled = created.isEnabled
        lock.lock()
        if tap === created { healthy = enabled }
        lock.unlock()
        if !enabled {
            stop()
        }
        return enabled
    }

    func stop() {
        lock.lock()
        let current = tap
        tap = nil
        healthy = false
        lock.unlock()

        current?.setEnabled(false)
        current?.invalidate()
    }

    var isActive: Bool {
        lock.lock()
        let current = tap
        let believedHealthy = healthy
        lock.unlock()
        guard believedHealthy, let current, current.isEnabled else {
            if believedHealthy {
                lock.lock()
                if tap === current { healthy = false }
                lock.unlock()
            }
            return false
        }
        return true
    }

    /// Whether any unmarked keyboard or pointer event was observed during the
    /// current guard lifetime. This stays true after `stop()` so the controller
    /// cannot miss an event racing with shield teardown; the next `start()` is
    /// the only operation that clears it.
    var hasObservedLocalInput: Bool {
        lock.lock()
        defer { lock.unlock() }
        return localInputObserved
    }

    /// Returns true only when the event should continue through the tap.
    private func handle(type: CGEventType, marker: Int64, sourcePID: Int64) -> Bool {
        switch InputEventClassifier.disposition(
            type: type, eventMarker: marker, agentMarker: agentMarker,
            eventSourcePID: sourcePID, agentPID: agentPID
        ) {
        case .allowAgentEvent:
            return isActive
        case .suppressLocalInput:
            lock.lock()
            localInputObserved = true
            lock.unlock()
            return false
        case .ignore:
            return true
        case .recoverDisabledTap:
            // A user-input disable is not proof that the input was delivered,
            // but it is enough evidence of local presence to fail closed.
            if type == .tapDisabledByUserInput {
                lock.lock()
                localInputObserved = true
                lock.unlock()
            }
            recoverDisabledTap()
            // Disable notifications are out-of-band sentinels, never input to
            // deliver to an application.
            return false
        }
    }

    private func recoverDisabledTap() {
        lock.lock()
        let current = tap
        healthy = false
        lock.unlock()
        guard let current else { return }

        current.setEnabled(true)
        let recovered = current.isEnabled
        lock.lock()
        if tap === current { healthy = recovered }
        lock.unlock()
    }
}

private final class InputTapCallbackBox {
    let handler: InputTapHandler
    init(handler: @escaping InputTapHandler) { self.handler = handler }
}

/// Production CGEvent tap. It runs at the head of the user's session stream:
/// agent events posted at the HID tap arrive here with their marker, while
/// physical keyboard and pointer events arrive unmarked and are dropped.
/// The helper is a user LaunchAgent, so this cannot move to `.cghidEventTap`:
/// Core Graphics permits active HID-entry taps only to root processes.
private final class SystemInputTap: InputTapControlling {
    private let callbackBox: InputTapCallbackBox
    private let port: CFMachPort
    private let runLoopSource: CFRunLoopSource

    init?(handler: @escaping InputTapHandler) {
        let box = InputTapCallbackBox(handler: handler)
        guard let port = CGEvent.tapCreate(
            tap: .cgSessionEventTap,
            place: .headInsertEventTap,
            options: .defaultTap,
            eventsOfInterest: Self.eventMask,
            callback: { _, type, event, userInfo in
                // No callback state means there is no basis to permit input.
                guard let userInfo else { return nil }
                let box = Unmanaged<InputTapCallbackBox>
                    .fromOpaque(userInfo).takeUnretainedValue()
                // Disabled-tap callbacks are out-of-band notifications, not
                // input events with a meaningful payload. Do not depend on
                // their CGEvent fields being readable.
                let guarded = InputEventClassifier.guards(type)
                let marker = guarded
                    ? event.getIntegerValueField(.eventSourceUserData) : 0
                let sourcePID = guarded
                    ? event.getIntegerValueField(.eventSourceUnixProcessID) : 0
                guard box.handler(type, marker, sourcePID) else { return nil }
                return Unmanaged.passUnretained(event)
            },
            userInfo: Unmanaged.passUnretained(box).toOpaque()
        ) else {
            return nil
        }
        guard let source = CFMachPortCreateRunLoopSource(nil, port, 0) else {
            CFMachPortInvalidate(port)
            return nil
        }
        self.callbackBox = box
        self.port = port
        self.runLoopSource = source
        CFRunLoopAddSource(CFRunLoopGetMain(), source, .commonModes)
    }

    var isEnabled: Bool { CGEvent.tapIsEnabled(tap: port) }

    func setEnabled(_ enabled: Bool) {
        CGEvent.tapEnable(tap: port, enable: enabled)
    }

    func invalidate() {
        CGEvent.tapEnable(tap: port, enable: false)
        CFRunLoopRemoveSource(CFRunLoopGetMain(), runLoopSource, .commonModes)
        CFMachPortInvalidate(port)
    }

    private static let eventMask = InputEventClassifier.guardedTypes.reduce(CGEventMask(0)) {
        mask, type in
        mask | (CGEventMask(1) << type.rawValue)
    }
}
