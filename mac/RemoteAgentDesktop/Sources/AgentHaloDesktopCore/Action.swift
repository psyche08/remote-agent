import Foundation

/// The closed set of desktop operations. Anything outside this list is refused
/// before it reaches CoreGraphics, so a confused or compromised caller cannot
/// reach an arbitrary command through this surface.
///
/// This is a port of internal/computeruse/action.go and must stay behaviourally
/// identical to it while the Go side still validates: two validators that
/// disagree would mean the reachable vocabulary is the union of both, which is
/// larger than either was reviewed to be.
public enum ActionID: String, Codable, CaseIterable, Sendable {
    case screenCapture = "screen.capture"
    case pointerMove = "pointer.move"
    case pointerClick = "pointer.click"
    case pointerScroll = "pointer.scroll"
    case keyboardType = "keyboard.type"
    case keyboardKey = "keyboard.key"
}

public enum MouseButton: String, Codable, Sendable {
    case left, right, middle
}

/// Pointer coordinates coming from public/debug APIs retain the historical
/// Core Graphics global-coordinate contract. Model tools instead point into
/// the top-left-origin composite PNG returned by `get_app_state`; the desktop
/// layer translates those coordinates across negative-origin displays.
public enum ActionCoordinateSpace: String, Codable, Sendable {
    case global
    case screenshot
}

public enum ActionLimits {
    /// Bounds a single type action. Long text is the caller's job to chunk; an
    /// unbounded synthetic keystroke stream is a denial-of-service against the
    /// very desktop the agent is driving.
    public static let maxTypeCharacters = 4096
    /// Bounds a single chord (e.g. cmd+shift+4).
    public static let maxChordKeys = 5
    /// Bounds a multi-click (single, double, triple).
    public static let maxClickCount = 3
    /// Bounds one scroll action's wheel delta.
    public static let maxScrollMagnitude = 4096
    /// Screen bounds are enforced against real display geometry when the event
    /// is posted. This only rejects absurd values, so a typo cannot become a
    /// pathological synthetic event.
    public static let coordinateRange = (-32768)...32767
}

/// One validated desktop operation. It is produced only by `Action.parse`, so
/// every field the CoreGraphics layer consumes has already been range-checked.
public struct Action: Sendable, Equatable {
    public let id: ActionID
    public let coordinateSpace: ActionCoordinateSpace
    public let x: Int
    public let y: Int
    public let button: MouseButton
    public let count: Int
    public let text: String
    public let keys: [String]
    public let deltaX: Int
    public let deltaY: Int

    init(
        id: ActionID, coordinateSpace: ActionCoordinateSpace = .global,
        x: Int = 0, y: Int = 0, button: MouseButton = .left,
        count: Int = 1, text: String = "", keys: [String] = [],
        deltaX: Int = 0, deltaY: Int = 0
    ) {
        self.id = id
        self.coordinateSpace = coordinateSpace
        self.x = x
        self.y = y
        self.button = button
        self.count = count
        self.text = text
        self.keys = keys
        self.deltaX = deltaX
        self.deltaY = deltaY
    }
}

/// The raw wire shape. Deliberately distinct from `Action`: nothing hands an
/// unvalidated request to the system layer.
public struct ActionRequest: Codable, Sendable {
    public var action: String
    public var coordinateSpace: String?
    public var x: Int?
    public var y: Int?
    public var button: String?
    public var count: Int?
    public var text: String?
    public var keys: [String]?
    public var deltaX: Int?
    public var deltaY: Int?

    enum CodingKeys: String, CodingKey {
        case action, x, y, button, count, text, keys
        case coordinateSpace = "coordinate_space"
        case deltaX = "delta_x"
        case deltaY = "delta_y"
    }

    public init(
        action: String, coordinateSpace: String? = nil,
        x: Int? = nil, y: Int? = nil, button: String? = nil,
        count: Int? = nil, text: String? = nil, keys: [String]? = nil,
        deltaX: Int? = nil, deltaY: Int? = nil
    ) {
        self.action = action
        self.coordinateSpace = coordinateSpace
        self.x = x
        self.y = y
        self.button = button
        self.count = count
        self.text = text
        self.keys = keys
        self.deltaX = deltaX
        self.deltaY = deltaY
    }
}

public struct ActionError: Error, Equatable, CustomStringConvertible {
    public let message: String
    public var description: String { message }
    init(_ message: String) { self.message = message }
}

/// The closed set of non-character key names a chord may use. Character keys
/// (a-z, 0-9, punctuation) are accepted as single-character names.
private let namedKeys: Set<String> = [
    "cmd", "command", "ctrl", "control", "alt", "option", "shift", "fn",
    "return", "enter", "tab", "space", "delete", "backspace", "escape", "esc",
    "up", "down", "left", "right", "home", "end", "pageup", "pagedown",
    "f1", "f2", "f3", "f4", "f5", "f6", "f7", "f8", "f9", "f10", "f11", "f12",
]

extension Action {
    /// Validates a wire request into an Action. This is the only constructor
    /// the system layer will execute.
    public static func parse(_ request: ActionRequest) throws -> Action {
        guard let id = ActionID(rawValue: request.action.trimmingCharacters(in: .whitespaces)) else {
            throw ActionError("unknown computer-use action")
        }
        switch id {
        case .screenCapture:
            return Action(id: id)

        case .pointerMove:
            let (space, x, y) = try requirePoint(request)
            return Action(id: id, coordinateSpace: space, x: x, y: y)

        case .pointerClick:
            let (space, x, y) = try requirePoint(request)
            let button = try parseButton(request.button)
            let count = request.count ?? 1
            guard count >= 1, count <= ActionLimits.maxClickCount else {
                throw ActionError("click count must be 1..\(ActionLimits.maxClickCount)")
            }
            return Action(
                id: id, coordinateSpace: space, x: x, y: y,
                button: button, count: count)

        case .pointerScroll:
            let (space, x, y) = try requirePoint(request)
            let dx = request.deltaX ?? 0
            let dy = request.deltaY ?? 0
            guard dx != 0 || dy != 0 else {
                throw ActionError("scroll requires a non-zero delta_x or delta_y")
            }
            guard abs(dx) <= ActionLimits.maxScrollMagnitude,
                  abs(dy) <= ActionLimits.maxScrollMagnitude else {
                throw ActionError("scroll delta must be within +/-\(ActionLimits.maxScrollMagnitude)")
            }
            return Action(
                id: id, coordinateSpace: space, x: x, y: y,
                deltaX: dx, deltaY: dy)

        case .keyboardType:
            let text = request.text ?? ""
            guard !text.isEmpty else { throw ActionError("type requires text") }
            guard text.count <= ActionLimits.maxTypeCharacters else {
                throw ActionError("type text exceeds \(ActionLimits.maxTypeCharacters) characters")
            }
            guard !text.unicodeScalars.contains(where: { $0.value == 0 }) else {
                throw ActionError("type text contains a NUL byte")
            }
            return Action(id: id, text: text)

        case .keyboardKey:
            return Action(id: id, keys: try parseKeys(request.keys ?? []))
        }
    }

    private static func requirePoint(
        _ request: ActionRequest
    ) throws -> (ActionCoordinateSpace, Int, Int) {
        guard let x = request.x, let y = request.y else {
            throw ActionError("action requires x and y")
        }
        guard ActionLimits.coordinateRange.contains(x),
              ActionLimits.coordinateRange.contains(y) else {
            throw ActionError("coordinates out of range")
        }
        let rawSpace = (request.coordinateSpace ?? "global")
            .trimmingCharacters(in: .whitespaces).lowercased()
        guard let space = ActionCoordinateSpace(rawValue: rawSpace) else {
            throw ActionError("unknown coordinate space")
        }
        if space == .screenshot, (x < 0 || y < 0) {
            throw ActionError("screenshot coordinates cannot be negative")
        }
        return (space, x, y)
    }

    private static func parseButton(_ name: String?) throws -> MouseButton {
        let cleaned = (name ?? "").trimmingCharacters(in: .whitespaces).lowercased()
        if cleaned.isEmpty { return .left }
        guard let button = MouseButton(rawValue: cleaned) else {
            throw ActionError("unknown mouse button: \(name ?? "")")
        }
        return button
    }

    private static func parseKeys(_ keys: [String]) throws -> [String] {
        guard !keys.isEmpty else { throw ActionError("key action requires keys") }
        guard keys.count <= ActionLimits.maxChordKeys else {
            throw ActionError("key chord exceeds \(ActionLimits.maxChordKeys) keys")
        }
        return try keys.map { raw in
            let key = raw.trimmingCharacters(in: .whitespaces).lowercased()
            guard !key.isEmpty else { throw ActionError("key chord contains an empty key") }
            guard namedKeys.contains(key) || key.count == 1 else {
                throw ActionError("unknown key: \(raw)")
            }
            return key
        }
    }
}
