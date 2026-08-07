import Foundation

/// One request from the agent. The wire shape is newline-delimited JSON, one
/// object per line, and action fields are flattened alongside `op` — the same
/// shape the one-shot worker accepted on argv, so the Go side changes only its
/// transport, not its vocabulary.
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

    enum CodingKeys: String, CodingKey {
        case op, action, x, y, button, count, text, keys
        case deltaX = "delta_x"
        case deltaY = "delta_y"
    }

    var actionRequest: ActionRequest {
        ActionRequest(
            action: action ?? "", x: x, y: y, button: button, count: count,
            text: text, keys: keys, deltaX: deltaX, deltaY: deltaY)
    }
}

/// One response. Absent fields are omitted rather than sent as null, so a
/// caller cannot mistake "not applicable" for "false".
public struct Response: Encodable, Sendable {
    public var ok: Bool
    public var error: String?
    public var locked: Bool?
    public var idleSeconds: Double?
    public var engaged: Bool?
    public var displays: Int?
    public var path: String?

    enum CodingKeys: String, CodingKey {
        case ok, error, locked, engaged, displays, path
        case idleSeconds = "idle_seconds"
    }

    public static func failure(_ message: String) -> Response {
        Response(ok: false, error: message)
    }

    public static var success: Response { Response(ok: true) }
}

/// Routes a request to the desktop service.
///
/// The op set is closed for the same reason the action set is: an unknown op
/// must be refused here, not forwarded to something that might interpret it.
public struct RequestRouter: Sendable {
    private let desktop: DesktopService

    public init(desktop: DesktopService) {
        self.desktop = desktop
    }

    public func handle(_ request: Request) -> Response {
        switch request.op {
        case "ping":
            return .success

        case "lock_state":
            return Response(ok: true, locked: desktop.screenIsLocked())

        case "lock":
            guard desktop.lockScreen() else {
                return .failure("could not lock the screen")
            }
            return .success

        case "idle_seconds":
            return Response(ok: true, idleSeconds: desktop.secondsSinceLastInput())

        case "shield_engage":
            let result = desktop.engageShield()
            guard result.engaged else {
                return .failure("could not engage the display shield")
            }
            return Response(ok: true, displays: result.displays)

        case "shield_release":
            desktop.releaseShield()
            return .success

        case "shield_state":
            let state = desktop.shieldState()
            return Response(ok: true, engaged: state.engaged, displays: state.displays)

        case "action":
            do {
                let action = try Action.parse(request.actionRequest)
                switch try desktop.perform(action) {
                case .done:
                    return .success
                case .captured(let path):
                    return Response(ok: true, path: path)
                }
            } catch let error as ActionError {
                return .failure(error.message)
            } catch {
                return .failure("action failed")
            }

        default:
            return .failure("unknown op")
        }
    }

    /// Decodes, routes, and encodes one line. Malformed input is answered, not
    /// dropped: a caller that gets no reply cannot tell a rejected request from
    /// a hung helper, and the safeguard above it would wait instead of failing
    /// closed.
    public func handle(line: Data) -> Data {
        let response: Response
        do {
            response = handle(try JSONDecoder().decode(Request.self, from: line))
        } catch {
            response = .failure("malformed request")
        }
        // A response that cannot be encoded still has to be answerable.
        let encoded = (try? JSONEncoder().encode(response))
            ?? Data(#"{"ok":false,"error":"response could not be encoded"}"#.utf8)
        return encoded + Data("\n".utf8)
    }
}
