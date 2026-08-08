import XCTest
@testable import RemoteAgentDesktopCore

/// Human presence outranks a remote turn: any local input during a window ends
/// it. The whole safeguard rests on telling the agent's own synthetic events
/// apart from a person's, and the system idle counter cannot make that
/// distinction — it counts everything that reached the HID system.
///
/// Both directions are failure modes, and the codebase has had each:
///
///   * Counting our own events as a person made the agent's first mouse move
///     close its own window and relock the screen. Measured live: one
///     pointer.move took the reported idle from 83722s to 0.34s, which the
///     controller reads as someone arriving at the keyboard.
///   * Counting a person's input as ours would leave the desktop unlocked with
///     someone standing at it — the failure this feature exists to prevent.
final class HumanPresenceTests: XCTestCase {
    private let slack = DesktopService.syntheticAttributionSlack

    /// The agent acting must not look like a person arriving. This is the case
    /// that was broken.
    func testTheAgentsOwnEventIsNotAPerson() {
        // Nobody has touched the machine for an hour; the agent posted 0.1s ago
        // and the system counter has moved because of that post.
        let result = DesktopService.humanIdle(
            systemIdle: 0.1, now: 1000, lastSyntheticPost: 999.9,
            anchorAt: 999.9, anchorValue: 3600)
        XCTAssertFalse(result.humanPresent)
        XCTAssertEqual(result.idle, 3600.1, accuracy: 0.001,
                       "the agent's own event reset the human clock")
    }

    /// A burst of agent activity must keep carrying the clock forward, not
    /// collapse it on the second event.
    func testTheClockCarriesForwardAcrossABurst() {
        var anchorAt: TimeInterval = 1000
        var anchorValue: TimeInterval = 3600
        var now: TimeInterval = 1000
        for _ in 0..<20 {
            now += 0.25
            let result = DesktopService.humanIdle(
                systemIdle: 0.05, now: now, lastSyntheticPost: now - 0.05,
                anchorAt: anchorAt, anchorValue: anchorValue)
            XCTAssertFalse(result.humanPresent)
            // Each post re-anchors to the human estimate, exactly as
            // markSyntheticPost does.
            anchorValue = result.idle
            anchorAt = now
        }
        XCTAssertGreaterThan(anchorValue, 3604,
                             "the human clock stopped advancing during agent activity")
    }

    /// An input newer than our last post is a person. This is the direction no
    /// live test can produce, because every event this process posts is
    /// attributed to it by construction.
    func testAnInputNewerThanOurLastPostIsAPerson() {
        // The agent posted 5s ago; something touched the machine 0.1s ago.
        let result = DesktopService.humanIdle(
            systemIdle: 0.1, now: 1000, lastSyntheticPost: 995,
            anchorAt: 995, anchorValue: 3600)
        XCTAssertTrue(result.humanPresent)
        XCTAssertEqual(result.idle, 0.1, accuracy: 0.001,
                       "a person's input must be reported as the system sees it")
    }

    /// The slack absorbs the gap between posting an event and the counter
    /// moving. Just inside it is still ours; clearly beyond it is a person.
    func testSlackBoundsAttributionWithoutSwallowingAPerson() {
        let ours = DesktopService.humanIdle(
            systemIdle: 0.5 - slack + 0.01, now: 1000, lastSyntheticPost: 999.5,
            anchorAt: 999.5, anchorValue: 100)
        XCTAssertFalse(ours.humanPresent, "an event inside the slack is still ours")

        let person = DesktopService.humanIdle(
            systemIdle: 0.5 - slack - 0.01, now: 1000, lastSyntheticPost: 999.5,
            anchorAt: 999.5, anchorValue: 100)
        XCTAssertTrue(person.humanPresent, "an event beyond the slack is a person")
    }

    /// A person acting is reported at the system's value, not the carried
    /// clock — otherwise the controller would compare a large number against
    /// the window's age and keep the window open with someone at the keyboard.
    func testAPersonEndsTheWindowEvenAfterLongAgentActivity() {
        let result = DesktopService.humanIdle(
            systemIdle: 0.05, now: 5000, lastSyntheticPost: 4990,
            anchorAt: 4990, anchorValue: 7200)
        XCTAssertTrue(result.humanPresent)
        // The controller's violation test is `idle < window age`. A window open
        // for even a second must see this as input.
        XCTAssertLessThan(result.idle, 1.0)
    }
}
