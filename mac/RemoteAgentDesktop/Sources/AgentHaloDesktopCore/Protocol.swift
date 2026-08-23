import Foundation

/// One request from the agent. The wire shape is newline-delimited JSON, one
/// object per line, and action fields are flattened alongside `op`.
///
/// Note what is *not* here: there is no way to configure this process over the
/// socket. Config is read from the device's own file at startup, because a
/// capability that lets the machine unlock itself must be granted on the device
/// — a `configure` op would hand every local process a way to turn Locked Use
/// on, which is the one thing the design refuses to allow over a wire.
public struct Request: Decodable, Sendable {
    public let op: String
    public let action: String?
    public let coordinateSpace: String?
    public let x: Int?
    public let y: Int?
    public let button: String?
    public let count: Int?
    public let text: String?
    public let keys: [String]?
    public let deltaX: Int?
    public let deltaY: Int?
    public let turnID: String?
    public let reason: String?
    public let active: Bool?
    // Accessibility addressing: which app, which element, what value.
    public let app: String?
    public let bundleID: String?
    public let path: [Int]?
    public let value: String?

    enum CodingKeys: String, CodingKey {
        case op, action, x, y, button, count, text, keys, reason, active
        case app, path, value
        case deltaX = "delta_x"
        case deltaY = "delta_y"
        case turnID = "turn_id"
        case bundleID = "bundle_id"
        case coordinateSpace = "coordinate_space"
    }

    var actionRequest: ActionRequest {
        ActionRequest(
            action: action ?? "", coordinateSpace: coordinateSpace,
            x: x, y: y, button: button, count: count,
            text: text, keys: keys, deltaX: deltaX, deltaY: deltaY)
    }
}

/// Why a request was refused.
///
/// The agent maps these onto HTTP status codes, so they exist to keep that
/// mapping off string matching: a caller must be able to tell "not turned on
/// here" from "refused right now" from "this actually failed", and a reworded
/// message must never silently change a status code.
public enum ErrorCode: String, Sendable {
    case badRequest = "bad_request"
    case notEnabled = "not_enabled"
    case lockedUseNotEnabled = "locked_use_not_enabled"
    case notArmed = "not_armed"
    case shieldRequired = "shield_required"
    case localInput = "local_input"
    case noWindow = "no_window"
    case windowBusy = "window_busy"
    case unsupported = "unsupported"
    case failed = "failed"
}

/// One response. Absent fields are omitted rather than sent as null, so a
/// caller cannot mistake "not applicable" for "false".
public struct Response {
    public private(set) var fields: [String: Any]

    public static func ok(_ extra: [String: Any] = [:]) -> Response {
        var fields: [String: Any] = ["ok": true]
        fields.merge(extra) { _, new in new }
        return Response(fields: fields)
    }

    public static func failure(_ message: String, code: ErrorCode) -> Response {
        Response(fields: ["ok": false, "error": message, "code": code.rawValue])
    }

    public func encoded() -> Data {
        guard let data = try? JSONSerialization.data(withJSONObject: fields) else {
            return Data(#"{"ok":false,"error":"response could not be encoded","code":"failed"}"#.utf8)
        }
        return data
    }
}

/// Helper-owned cross-request state binding one protected AX tree observation
/// to later mutations in the same authoritative turn. The process pin itself
/// never crosses the socket. A single operation lock also prevents two client
/// connections from replacing/using a pin concurrently.
final class TurnScopedTargetPinStore<Value>: @unchecked Sendable {
    private struct Key: Hashable {
        let turnID: String
        let bundleIdentifier: String
    }

    private struct Entry {
        let value: Value
        let boundAt: Date
    }

    private let operationLock = NSLock()
    private let stateLock = NSLock()
    private var entries: [Key: Entry] = [:]
    private let maximumEntries = 128
    private let maximumAge: TimeInterval = 15 * 60

    func withExclusiveOperation<T>(_ body: () throws -> T) rethrows -> T {
        operationLock.lock()
        defer { operationLock.unlock() }
        return try body()
    }

    func bind(_ value: Value, turnID: String, bundleIdentifier: String) {
        stateLock.lock()
        defer { stateLock.unlock() }
        pruneLocked(now: Date())
        let key = Key(turnID: turnID, bundleIdentifier: bundleIdentifier)
        entries[key] = Entry(value: value, boundAt: Date())
        if entries.count > maximumEntries,
           let oldest = entries.min(by: { $0.value.boundAt < $1.value.boundAt })?.key {
            entries.removeValue(forKey: oldest)
        }
    }

    func value(turnID: String, bundleIdentifier: String) -> Value? {
        stateLock.lock()
        defer { stateLock.unlock() }
        pruneLocked(now: Date())
        return entries[Key(
            turnID: turnID, bundleIdentifier: bundleIdentifier)]?.value
    }

    func release(turnID: String) {
        stateLock.lock()
        entries = entries.filter { $0.key.turnID != turnID }
        stateLock.unlock()
    }

    func releaseAll() {
        stateLock.lock()
        entries.removeAll()
        stateLock.unlock()
    }

    var count: Int {
        stateLock.lock()
        defer { stateLock.unlock() }
        return entries.count
    }

    private func pruneLocked(now: Date) {
        entries = entries.filter {
            now.timeIntervalSince($0.value.boundAt) <= maximumAge
        }
    }
}

/// Routes a request to the desktop and the Locked Use controller.
///
/// The op set is closed for the same reason the action set is: an unknown op is
/// refused here, never forwarded to something that might interpret it.
public struct RequestRouter: @unchecked Sendable {
    private let desktop: DesktopService
    private let controller: LockedUseController?
    private let accessibilityTargetPolicies: [Accessibility.TargetPolicy]
    private let accessibilityTargetPins =
        TurnScopedTargetPinStore<Accessibility.TargetPin>()

    public init(
        desktop: DesktopService, controller: LockedUseController? = nil,
        accessibilityTargetPolicies: [Accessibility.TargetPolicy] = []
    ) {
        self.desktop = desktop
        self.controller = controller
        self.accessibilityTargetPolicies = accessibilityTargetPolicies
    }

    public func handle(_ request: Request) -> Response {
        switch request.op {
        case "ping":
            return .ok()

        case "lock_state":
            return .ok(["locked": desktop.screenIsLocked()])

        case "idle_seconds":
            return .ok(["idle_seconds": desktop.secondsSinceLastInput()])

        case "shield_state":
            let state = desktop.shieldState()
            return .ok(["engaged": state.engaged, "displays": state.displays])

        case "status":
            guard let controller else { return unconfigured() }
            return .ok(["status": controller.status()])

        case "locked_use_active":
            guard let controller else { return unconfigured() }
            guard let active = request.active else {
                return .failure("active is required", code: .badRequest)
            }
            do {
                try controller.setLockedUseActive(active)
                if !active { accessibilityTargetPins.releaseAll() }
                return .ok(["status": controller.status()])
            } catch {
                return Self.failure(for: error)
            }

        case "window_open":
            guard let controller else { return unconfigured() }
            guard let turnID = request.turnID, !turnID.isEmpty else {
                return .failure("turn_id is required", code: .badRequest)
            }
            // A turn id denotes one controller lifecycle. Clear any bounded
            // residue before admitting a new lifecycle with that identifier.
            accessibilityTargetPins.release(turnID: turnID)
            do {
                try controller.openWindow(turnID: turnID)
                return .ok(windowState(controller))
            } catch {
                return Self.failure(for: error)
            }

        case "window_close":
            guard let controller else { return unconfigured() }
            guard let turnID = request.turnID, !turnID.isEmpty else {
                return .failure("turn_id is required", code: .badRequest)
            }
            defer { accessibilityTargetPins.release(turnID: turnID) }
            do {
                try controller.closeWindow(
                    forTurn: turnID, reason: request.reason ?? "turn ended")
                return .ok(windowState(controller))
            } catch {
                return Self.failure(for: error)
            }

        case "window_state":
            guard let controller else { return unconfigured() }
            return .ok(windowState(controller))

        case "prepare_restart":
            guard let controller else { return unconfigured() }
            // Keep this helper alive after the reply. The caller may replace
            // it only after synchronous withdrawal + relock succeeds; on
            // failure the existing shield and quarantine loop must survive.
            guard controller.stop(), controller.isSafeToExit else {
                return .failure(
                    "computer-use cleanup is not yet safe for restart; the current helper remains active",
                    code: .failed)
            }
            accessibilityTargetPins.releaseAll()
            return .ok(["safe_to_restart": true])

        case "capture_allowed":
            guard let controller else { return .ok(["allowed": true]) }
            let gate = controller.captureAllowed()
            return .ok(["allowed": gate.allowed, "reason": gate.reason])

        case "action":
            guard let turnID = request.turnID, !turnID.isEmpty else {
                return .failure("turn_id is required", code: .badRequest)
            }
            do {
                let action = try Action.parse(request.actionRequest)
                let result: DesktopService.ActionResult
                if let controller {
                    result = try controller.run(action, forTurn: turnID)
                } else {
                    result = try desktop.perform(action)
                }
                switch result {
                case .done:
                    return .ok(["action": action.id.rawValue])
                case .captured(let data, let mediaType):
                    return Self.capturedResponse(
                        action: action.id, data: data, mediaType: mediaType)
                }
            } catch {
                return Self.failure(for: error)
            }

        // Accessibility ops. Unlike the pointer/keyboard actions, these reach an
        // app's element tree in-process, so they work after an authorized
        // Locked Use transition without relying on pointer coordinates.
        // They require the helper to be trusted for Accessibility; without it
        // every call fails, reported as such rather than as a broken feature.
        case "ax_read", "ax_press", "ax_setvalue", "ax_focus":
            guard let turnID = request.turnID, !turnID.isEmpty else {
                return .failure("turn_id is required", code: .badRequest)
            }
            if let controller {
                do {
                    return try controller.withAuthorizedTurn(forTurn: turnID) {
                        try accessibilityResponse(request)
                    }
                } catch {
                    return Self.failure(for: error)
                }
            }
            do {
                return try accessibilityResponse(request)
            } catch {
                return Self.failure(for: error)
            }

        default:
            return .failure("unknown op", code: .badRequest)
        }
    }

    private func windowState(_ controller: LockedUseController) -> [String: Any] {
        let state = controller.windowRegistration()
        return [
            "window_registered": state.registered,
            "window_phase": state.phase,
            "window_open": state.phase == "open",
            "window_turn_id": state.turnID,
            "window_closing": state.phase == "closing",
        ]
    }

    /// Screen bytes stay in-process from capture through serialization. A
    /// pathname would reintroduce a swap/read/delete TOCTOU between the helper
    /// and agent, and could leave screenshots behind after a crash.
    static func capturedResponse(
        action: ActionID, data: Data, mediaType: String
    ) -> Response {
        .ok([
            "action": action.rawValue,
            "image_base64": data.base64EncodedString(),
            "media_type": mediaType,
        ])
    }

    private func accessibilityResponse(_ request: Request) throws -> Response {
        guard Accessibility.isTrusted() else {
            return .failure(
                "the helper is not trusted for Accessibility; grant it in System Settings",
                code: .unsupported)
        }
        let targetPolicy = accessibilityTargetPolicy(for: request)
        guard let turnID = request.turnID, !turnID.isEmpty else {
            throw AccessibilityTargetError(
                message: "a protected Accessibility operation requires a turn id")
        }
        return try accessibilityTargetPins.withExclusiveOperation {
            do {
                switch request.op {
                case "ax_read":
                    let observation = try Accessibility.readPinned(
                        bundleID: request.bundleID, name: request.app,
                        policy: targetPolicy)
                    if let targetPolicy {
                        guard let pin = observation.targetPin else {
                            throw AccessibilityTargetError(
                                message: "the protected Accessibility read produced no process pin")
                        }
                        accessibilityTargetPins.bind(
                            pin, turnID: turnID,
                            bundleIdentifier: targetPolicy.bundleIdentifier)
                    }
                    return .ok([
                        "elements": observation.nodes.map(
                            Self.serializedAccessibilityNode)
                    ])
                case "ax_press":
                    guard let path = request.path else {
                        return .failure("path is required", code: .badRequest)
                    }
                    try Accessibility.pressPinned(
                        bundleID: request.bundleID, name: request.app, path: path,
                        policy: targetPolicy,
                        targetPin: try requiredTargetPin(
                            policy: targetPolicy, turnID: turnID))
                    return .ok()
                case "ax_setvalue":
                    guard let path = request.path else {
                        return .failure("path is required", code: .badRequest)
                    }
                    guard let value = request.value else {
                        return .failure("value is required", code: .badRequest)
                    }
                    try Accessibility.setValuePinned(
                        bundleID: request.bundleID, name: request.app,
                        path: path, value: value, policy: targetPolicy,
                        targetPin: try requiredTargetPin(
                            policy: targetPolicy, turnID: turnID))
                    return .ok()
                case "ax_focus":
                    guard let path = request.path else {
                        return .failure("path is required", code: .badRequest)
                    }
                    try Accessibility.focusPinned(
                        bundleID: request.bundleID, name: request.app, path: path,
                        policy: targetPolicy,
                        targetPin: try requiredTargetPin(
                            policy: targetPolicy, turnID: turnID))
                    return .ok()
                default:
                    return .failure("unknown Accessibility op", code: .badRequest)
                }
            } catch {
                if error is AccessibilityTargetError {
                    accessibilityTargetPins.release(turnID: turnID)
                }
                throw error
            }
        }
    }

    private func requiredTargetPin(
        policy: Accessibility.TargetPolicy?, turnID: String
    ) throws -> Accessibility.TargetPin? {
        guard let policy else { return nil }
        guard let pin = accessibilityTargetPins.value(
            turnID: turnID, bundleIdentifier: policy.bundleIdentifier)
        else {
            throw AccessibilityTargetError(
                message: "a protected Accessibility mutation requires a fresh read in the same turn")
        }
        return pin
    }

    /// A request can select a policy by its exact bundle id, or by the app name
    /// only when no bundle id was supplied. Expected team/path values never
    /// come from the socket and therefore cannot be weakened by the caller.
    func accessibilityTargetPolicy(
        for request: Request
    ) -> Accessibility.TargetPolicy? {
        if let bundleID = request.bundleID, !bundleID.isEmpty {
            return accessibilityTargetPolicies.first {
                $0.bundleIdentifier == bundleID
            }
        }
        guard let app = request.app, !app.isEmpty else { return nil }
        return accessibilityTargetPolicies.first { $0.appName == app }
    }

    static func serializedAccessibilityNode(
        _ node: Accessibility.Node
    ) -> [String: Any] {
        [
            "role": node.role,
            "identifier": node.identifier.map { $0 as Any } ?? NSNull(),
            "subrole": node.subrole.map { $0 as Any } ?? NSNull(),
            "label": node.label,
            "value": node.value.map { $0 as Any } ?? NSNull(),
            "enabled": node.enabled.map { $0 as Any } ?? NSNull(),
            "selected": node.selected.map { $0 as Any } ?? NSNull(),
            "focused": node.focused.map { $0 as Any } ?? NSNull(),
            "current": node.current.map { $0 as Any } ?? NSNull(),
            "checked": node.checked.map { $0 as Any } ?? NSNull(),
            "pressed": node.pressed.map { $0 as Any } ?? NSNull(),
            "actionable": node.actionable,
            "path": node.path,
        ]
    }

    /// A helper running without a controller can still drive the desktop, but
    /// it cannot answer for Locked Use. Saying so is not the same as saying the
    /// feature is off, and the agent must not report it as such.
    private func unconfigured() -> Response {
        .failure("computer use is not configured on this device", code: .notEnabled)
    }

    private static func failure(for error: Error) -> Response {
        if let error = error as? LockedUseError {
            let code: ErrorCode
            switch error {
            case .notEnabled: code = .notEnabled
            case .lockedUseNotEnabled: code = .lockedUseNotEnabled
            case .notArmed: code = .notArmed
            case .shieldRequired: code = .shieldRequired
            case .localInput: code = .localInput
            case .noWindow: code = .noWindow
            case .windowBusy: code = .windowBusy
            case .unsupported: code = .unsupported
            case .systemFailure: code = .failed
            }
            return .failure(error.description, code: code)
        }
        if let error = error as? ActionError {
            return .failure(error.message, code: .badRequest)
        }
        if let error = error as? AccessibilityIPCError {
            return .failure(error.message, code: .failed)
        }
        if let error = error as? AccessibilityTargetError {
            return .failure(error.message, code: .failed)
        }
        if let error = error as? GrantError {
            return .failure(error.message, code: .failed)
        }
        return .failure("\(error)", code: .failed)
    }

    /// Decodes, routes, and encodes one line. Malformed input is answered, not
    /// dropped: a caller that gets no reply cannot tell a rejected request from
    /// a hung helper, and the safeguard above it would wait instead of failing
    /// closed.
    public func handle(line: Data) -> Data {
        let response: Response
        if let request = try? JSONDecoder().decode(Request.self, from: line) {
            response = handle(request)
        } else {
            response = .failure("malformed request", code: .badRequest)
        }
        return response.encoded() + Data("\n".utf8)
    }
}
