import CoreGraphics
import XCTest
@testable import AgentHaloDesktopCore

private final class FakeInputTap: InputTapControlling {
    var isEnabled = false
    var enableSucceeds = true
    private(set) var enableCalls: [Bool] = []
    private(set) var invalidated = false

    func setEnabled(_ enabled: Bool) {
        enableCalls.append(enabled)
        isEnabled = enabled && enableSucceeds
    }

    func invalidate() {
        invalidated = true
        isEnabled = false
    }
}

final class InputGuardTests: XCTestCase {
    private let marker: Int64 = 0x5241_4C4F_434B_4544
    private let agentPID: Int64 = 4242

    func testClassifierAllowsMarkedAgentKeyboardAndPointerEvents() {
        let guarded: [CGEventType] = [
            .leftMouseDown, .leftMouseUp,
            .rightMouseDown, .rightMouseUp,
            .mouseMoved, .leftMouseDragged, .rightMouseDragged,
            .keyDown, .keyUp, .flagsChanged,
            .scrollWheel, .tabletPointer, .tabletProximity,
            .otherMouseDown, .otherMouseUp, .otherMouseDragged,
        ]
        for type in guarded {
            XCTAssertEqual(
                InputEventClassifier.disposition(
                    type: type, eventMarker: marker, agentMarker: marker),
                .allowAgentEvent,
                "marked \(type) should pass")
        }
    }

    func testClassifierSuppressesUnmarkedPhysicalKeyboardAndPointerEvents() {
        let guarded: [CGEventType] = [
            .leftMouseDown, .leftMouseUp,
            .rightMouseDown, .rightMouseUp,
            .mouseMoved, .leftMouseDragged, .rightMouseDragged,
            .keyDown, .keyUp, .flagsChanged,
            .scrollWheel, .tabletPointer, .tabletProximity,
            .otherMouseDown, .otherMouseUp, .otherMouseDragged,
        ]
        for type in guarded {
            XCTAssertEqual(
                InputEventClassifier.disposition(
                    type: type, eventMarker: 0, agentMarker: marker),
                .suppressLocalInput,
                "unmarked \(type) should be suppressed")
        }
    }

    func testZeroCannotBeConfiguredAsAnAgentMarker() {
        XCTAssertEqual(
            InputEventClassifier.disposition(
                type: .keyDown, eventMarker: 0, agentMarker: 0),
            .suppressLocalInput)
    }

    func testCopiedMarkerWithMismatchedReportedPIDIsSuppressed() {
        XCTAssertEqual(
            InputEventClassifier.disposition(
                type: .keyDown, eventMarker: marker, agentMarker: marker,
                eventSourcePID: agentPID + 1, agentPID: agentPID),
            .suppressLocalInput)
        XCTAssertEqual(
            InputEventClassifier.disposition(
                type: .keyDown, eventMarker: marker, agentMarker: marker,
                eventSourcePID: agentPID, agentPID: agentPID),
            .allowAgentEvent)
    }

    func testClassifierRoutesBothDisableNotificationsToRecovery() {
        XCTAssertEqual(
            InputEventClassifier.disposition(
                type: .tapDisabledByTimeout, eventMarker: 0, agentMarker: marker),
            .recoverDisabledTap)
        XCTAssertEqual(
            InputEventClassifier.disposition(
                type: .tapDisabledByUserInput, eventMarker: marker, agentMarker: marker),
            .recoverDisabledTap)
    }

    func testClassifierIgnoresEventsOutsideTheInputVocabulary() {
        XCTAssertEqual(
            InputEventClassifier.disposition(
                type: .null, eventMarker: 0, agentMarker: marker),
            .ignore)
    }

    func testStartFailsClosedWhenTapCannotBeCreatedOrEnabled() {
        let missing = InputGuard(agentMarker: marker, tapFactory: { _ in nil })
        XCTAssertFalse(missing.start())
        XCTAssertFalse(missing.isActive)

        let disabledTap = FakeInputTap()
        disabledTap.enableSucceeds = false
        let disabled = InputGuard(agentMarker: marker, tapFactory: { _ in disabledTap })
        XCTAssertFalse(disabled.start())
        XCTAssertFalse(disabled.isActive)
        XCTAssertTrue(disabledTap.invalidated)
    }

    func testRunningGuardPassesOnlyMarkedInput() throws {
        let tap = FakeInputTap()
        var callback: InputTapHandler?
        let guardUnderTest = InputGuard(agentMarker: marker, agentPID: agentPID, tapFactory: { handler in
            callback = handler
            return tap
        })
        XCTAssertTrue(guardUnderTest.start())
        let handle = try XCTUnwrap(callback)

        XCTAssertTrue(handle(.keyDown, marker, agentPID))
        XCTAssertTrue(handle(.leftMouseDown, marker, agentPID))
        XCTAssertFalse(guardUnderTest.hasObservedLocalInput)
        XCTAssertFalse(handle(.keyDown, 0, 0))
        XCTAssertTrue(guardUnderTest.hasObservedLocalInput)
        XCTAssertFalse(handle(.leftMouseDown, 0, 0))
        XCTAssertTrue(handle(.null, 0, 0))
    }

    func testLocalInputLatchSurvivesStopAndResetsForNextGuardLifetime() throws {
        let firstTap = FakeInputTap()
        let secondTap = FakeInputTap()
        var callbacks: [InputTapHandler] = []
        var taps = [firstTap, secondTap]
        let guardUnderTest = InputGuard(agentMarker: marker, agentPID: agentPID, tapFactory: { handler in
            callbacks.append(handler)
            return taps.removeFirst()
        })

        XCTAssertTrue(guardUnderTest.start())
        XCTAssertFalse(callbacks[0](.mouseMoved, 0, 0))
        XCTAssertTrue(guardUnderTest.hasObservedLocalInput)
        guardUnderTest.stop()
        XCTAssertTrue(guardUnderTest.hasObservedLocalInput)

        XCTAssertTrue(guardUnderTest.start())
        XCTAssertFalse(guardUnderTest.hasObservedLocalInput)
        XCTAssertTrue(callbacks[1](.keyDown, marker, agentPID))
        XCTAssertFalse(guardUnderTest.hasObservedLocalInput)
    }

    func testUserInputDisableLatchesLocalPresence() throws {
        let tap = FakeInputTap()
        var callback: InputTapHandler?
        let guardUnderTest = InputGuard(agentMarker: marker, agentPID: agentPID, tapFactory: { handler in
            callback = handler
            return tap
        })
        XCTAssertTrue(guardUnderTest.start())

        XCTAssertFalse(try XCTUnwrap(callback)(.tapDisabledByUserInput, 0, 0))
        XCTAssertTrue(guardUnderTest.hasObservedLocalInput)
    }

    func testTimeoutDisableIsReenabledImmediately() throws {
        let tap = FakeInputTap()
        var callback: InputTapHandler?
        let guardUnderTest = InputGuard(agentMarker: marker, tapFactory: { handler in
            callback = handler
            return tap
        })
        XCTAssertTrue(guardUnderTest.start())
        let handle = try XCTUnwrap(callback)

        tap.isEnabled = false
        XCTAssertFalse(handle(.tapDisabledByTimeout, 0, 0))
        XCTAssertTrue(guardUnderTest.isActive)
        XCTAssertEqual(tap.enableCalls.filter { $0 }.count, 2)
    }

    func testUserInputDisableFailsClosedWhenReenableFails() throws {
        let tap = FakeInputTap()
        var callback: InputTapHandler?
        let guardUnderTest = InputGuard(agentMarker: marker, agentPID: agentPID, tapFactory: { handler in
            callback = handler
            return tap
        })
        XCTAssertTrue(guardUnderTest.start())
        let handle = try XCTUnwrap(callback)

        tap.isEnabled = false
        tap.enableSucceeds = false
        XCTAssertFalse(handle(.tapDisabledByUserInput, 0, 0))
        XCTAssertFalse(guardUnderTest.isActive)
        // Even an otherwise valid agent event is refused after the safeguard
        // has lost its tap; DisplayShield observes the same inactive state and
        // forces the existing controller path to relock.
        XCTAssertFalse(handle(.keyDown, marker, agentPID))
    }

    func testAQuietlyDisabledTapIsDetectedAndFailsClosed() throws {
        let tap = FakeInputTap()
        var callback: InputTapHandler?
        let guardUnderTest = InputGuard(agentMarker: marker, agentPID: agentPID, tapFactory: { handler in
            callback = handler
            return tap
        })
        XCTAssertTrue(guardUnderTest.start())
        let handle = try XCTUnwrap(callback)

        // The normal callbacks cover explicit disable notifications. This
        // models the additional safeguard: a port that simply starts reporting
        // disabled is detected by the shield's live-state probe.
        tap.isEnabled = false
        XCTAssertFalse(guardUnderTest.isActive)
        XCTAssertFalse(handle(.keyDown, marker, agentPID))
    }

    func testStopDisablesAndInvalidatesTheTap() {
        let tap = FakeInputTap()
        let guardUnderTest = InputGuard(agentMarker: marker, tapFactory: { _ in tap })
        XCTAssertTrue(guardUnderTest.start())

        guardUnderTest.stop()
        XCTAssertFalse(guardUnderTest.isActive)
        XCTAssertEqual(tap.enableCalls.last, false)
        XCTAssertTrue(tap.invalidated)
    }

    func testAgentMarkerIsWrittenToCoreGraphicsEvent() throws {
        let event = try XCTUnwrap(CGEvent(source: nil))
        InputGuard.markAgentEvent(event)
        XCTAssertEqual(
            event.getIntegerValueField(.eventSourceUserData),
            InputGuard.agentEventMarker)
    }
}
