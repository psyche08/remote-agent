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

    enum CodingKeys: String, CodingKey {
        case op, action, x, y, button, count, text, keys, reason, active
        case deltaX = "delta_x"
        case deltaY = "delta_y"
        case turnID = "turn_id"
    }

    var actionRequest: ActionRequest {
        ActionRequest(
            action: action ?? "", x: x, y: y, button: button, count: count,
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

/// Routes a request to the desktop and the Locked Use controller.
///
/// The op set is closed for the same reason the action set is: an unknown op is
/// refused here, never forwarded to something that might interpret it.
public struct RequestRouter: @unchecked Sendable {
    private let desktop: DesktopService
    private let controller: LockedUseController?

    public init(desktop: DesktopService, controller: LockedUseController? = nil) {
        self.desktop = desktop
        self.controller = controller
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
                return .ok(["status": controller.status()])
            } catch {
                return Self.failure(for: error)
            }

        case "window_open":
            guard let controller else { return unconfigured() }
            guard let turnID = request.turnID, !turnID.isEmpty else {
                return .failure("turn_id is required", code: .badRequest)
            }
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
            controller.closeWindow(
                forTurn: turnID, reason: request.reason ?? "turn ended")
            return .ok(windowState(controller))

        case "window_state":
            guard let controller else { return unconfigured() }
            return .ok(windowState(controller))

        case "capture_allowed":
            guard let controller else { return .ok(["allowed": true]) }
            let gate = controller.captureAllowed()
            return .ok(["allowed": gate.allowed, "reason": gate.reason])

        case "action":
            do {
                let action = try Action.parse(request.actionRequest)
                let result: DesktopService.ActionResult
                if let controller {
                    result = try controller.run(action)
                } else {
                    result = try desktop.perform(action)
                }
                switch result {
                case .done:
                    return .ok(["action": action.id.rawValue])
                case .captured(let path):
                    return .ok(["action": action.id.rawValue, "path": path])
                }
            } catch {
                return Self.failure(for: error)
            }

        default:
            return .failure("unknown op", code: .badRequest)
        }
    }

    private func windowState(_ controller: LockedUseController) -> [String: Any] {
        let turn = controller.openWindowTurn()
        return [
            "window_open": turn != nil,
            "window_turn_id": turn ?? "",
            "window_closing": controller.isWindowClosing(),
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
