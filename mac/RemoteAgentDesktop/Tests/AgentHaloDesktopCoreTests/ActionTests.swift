import XCTest
@testable import AgentHaloDesktopCore

/// These mirror internal/computeruse/action_test.go. The two validators guard
/// the same boundary, and while both are live the reachable vocabulary is the
/// union of what each accepts — so a gap on this side is a gap in the surface,
/// not merely an untested Swift file.
final class ActionTests: XCTestCase {
    func testAcceptsValidRequests() throws {
        let capture = try Action.parse(ActionRequest(action: "screen.capture"))
        XCTAssertEqual(capture.id, .screenCapture)

        let move = try Action.parse(ActionRequest(action: "pointer.move", x: 10, y: 20))
        XCTAssertEqual(move.x, 10)
        XCTAssertEqual(move.y, 20)
        XCTAssertEqual(move.coordinateSpace, .global)

        let screenshotMove = try Action.parse(ActionRequest(
            action: "pointer.move", coordinateSpace: "screenshot", x: 10, y: 20))
        XCTAssertEqual(screenshotMove.coordinateSpace, .screenshot)

        // An omitted button and count take the documented defaults rather than
        // failing: the API has always accepted a bare click.
        let click = try Action.parse(ActionRequest(action: "pointer.click", x: 1, y: 2))
        XCTAssertEqual(click.button, .left)
        XCTAssertEqual(click.count, 1)

        let chord = try Action.parse(
            ActionRequest(action: "keyboard.key", keys: [" CMD ", "Shift", "4"]))
        XCTAssertEqual(chord.keys, ["cmd", "shift", "4"])

        let scroll = try Action.parse(
            ActionRequest(action: "pointer.scroll", x: 0, y: 0, deltaY: -120))
        XCTAssertEqual(scroll.deltaY, -120)
    }

    func testRejectsUnknownActions() {
        for name in ["", "shell.exec", "screen.captures", "unlock", "pointer"] {
            XCTAssertThrowsError(try Action.parse(ActionRequest(action: name)), name)
        }
    }

    func testRequiresCoordinates() {
        for name in ["pointer.move", "pointer.click", "pointer.scroll"] {
            XCTAssertThrowsError(try Action.parse(ActionRequest(action: name, deltaY: 1)), name)
            XCTAssertThrowsError(
                try Action.parse(ActionRequest(action: name, x: 1, deltaY: 1)), "\(name) x only")
        }
    }

    func testScreenshotCoordinateSpaceIsExplicitAndNonNegative() {
        XCTAssertThrowsError(try Action.parse(ActionRequest(
            action: "pointer.click", coordinateSpace: "screen", x: 1, y: 2)))
        XCTAssertThrowsError(try Action.parse(ActionRequest(
            action: "pointer.click", coordinateSpace: "screenshot", x: -1, y: 2)))
        // Negative global coordinates remain valid for legacy callers and for
        // displays arranged left of or above the primary display.
        XCTAssertNoThrow(try Action.parse(ActionRequest(
            action: "pointer.click", coordinateSpace: "global", x: -1, y: -2)))
    }

    func testWireRequestPreservesScreenshotCoordinateSpace() throws {
        let request = try JSONDecoder().decode(Request.self, from: Data(
            #"{"op":"action","turn_id":"turn-1","action":"pointer.click","coordinate_space":"screenshot","x":7,"y":9}"#.utf8))
        let action = try Action.parse(request.actionRequest)
        XCTAssertEqual(action.coordinateSpace, .screenshot)
        XCTAssertEqual(action.x, 7)
        XCTAssertEqual(action.y, 9)
    }

    func testBoundsInputs() {
        XCTAssertThrowsError(try Action.parse(
            ActionRequest(action: "pointer.move", x: 40000, y: 0)))
        XCTAssertThrowsError(try Action.parse(
            ActionRequest(action: "pointer.move", x: 0, y: -40000)))
        XCTAssertThrowsError(try Action.parse(
            ActionRequest(action: "pointer.click", x: 0, y: 0, count: 4)))
        XCTAssertThrowsError(try Action.parse(
            ActionRequest(action: "pointer.click", x: 0, y: 0, count: -1)))
        XCTAssertThrowsError(try Action.parse(ActionRequest(
            action: "pointer.scroll", x: 0, y: 0,
            deltaY: ActionLimits.maxScrollMagnitude + 1)))
        XCTAssertThrowsError(try Action.parse(ActionRequest(
            action: "keyboard.type",
            text: String(repeating: "a", count: ActionLimits.maxTypeCharacters + 1))))
        XCTAssertThrowsError(try Action.parse(ActionRequest(
            action: "keyboard.key", keys: ["cmd", "shift", "alt", "ctrl", "fn", "a"])))
    }

    func testRejectsNulInTypedText() {
        XCTAssertThrowsError(try Action.parse(
            ActionRequest(action: "keyboard.type", text: "ok\u{0}injected")))
    }

    func testRejectsUnknownKeysAndButtons() {
        XCTAssertThrowsError(try Action.parse(
            ActionRequest(action: "keyboard.key", keys: ["hyper"])))
        XCTAssertThrowsError(try Action.parse(
            ActionRequest(action: "keyboard.key", keys: ["cmd", " "])))
        XCTAssertThrowsError(try Action.parse(
            ActionRequest(action: "pointer.click", x: 0, y: 0, button: "back")))
    }

    func testRejectsEmptyScrollAndKeys() {
        XCTAssertThrowsError(try Action.parse(
            ActionRequest(action: "pointer.scroll", x: 0, y: 0)))
        XCTAssertThrowsError(try Action.parse(
            ActionRequest(action: "keyboard.key", keys: [])))
        XCTAssertThrowsError(try Action.parse(ActionRequest(action: "keyboard.type", text: "")))
    }

    /// The vocabulary must never gain a way to unlock. Unlocking belongs to
    /// macOS and the Authorization Plug-in; an action that did it directly
    /// would make every controller safeguard bypassable.
    func testCatalogHasNoUnlockOperation() {
        for id in ActionID.allCases {
            XCTAssertFalse(id.rawValue.lowercased().contains("unlock"), id.rawValue)
            XCTAssertFalse(id.rawValue.lowercased().contains("password"), id.rawValue)
        }
    }

    /// Every name the validator accepts must have a meaning in KeyMap. A name
    /// that passed validation with no mapping would fail at post time, or worse
    /// post a different key than the caller named.
    func testEveryAcceptedKeyNameMaps() throws {
        let names = [
            "cmd", "command", "ctrl", "control", "alt", "option", "shift", "fn",
            "return", "enter", "tab", "space", "delete", "backspace", "escape", "esc",
            "up", "down", "left", "right", "home", "end", "pageup", "pagedown",
            "f1", "f2", "f3", "f4", "f5", "f6", "f7", "f8", "f9", "f10", "f11", "f12",
            "a", "z", "0", "9", "-", "=", "[", "]", ";", "'", ",", ".", "/", "\\", "`",
        ]
        for name in names {
            let parsed = try Action.parse(ActionRequest(action: "keyboard.key", keys: [name]))
            let key = parsed.keys[0]
            XCTAssertTrue(
                KeyMap.modifier(for: key) != nil || KeyMap.code(for: key) != nil,
                "accepted key \(key) has no mapping")
        }
    }
}
