import CoreGraphics
import XCTest
@testable import AgentHaloDesktopCore

/// The shield is the privacy safeguard the controller refuses to unlock
/// without, so "covered" has to mean covered. These pin the matching that
/// decides it.
///
/// This is not hypothetical hardening. On a two-display Mac the secondary
/// shield window was created through NSWindow's `screen:` initializer, which
/// interprets the rect in that screen's own coordinates; the window landed at
/// an origin scaled by the backing factor, far outside every display. Pixel
/// sampling showed the external monitor 100% uncovered while the safeguard
/// reported full coverage — the desktop unlocked and visible to anyone standing
/// there. A size-only comparison could not see it.
final class DisplayShieldTests: XCTestCase {
    private let builtIn = CGRect(x: 0, y: 0, width: 1728, height: 1117)
    private let external = CGRect(x: -192, y: -1080, width: 1920, height: 1080)

    func testOrdinaryCaptureClientsRetainTheBlackShield() {
        XCTAssertEqual(DisplayShield.captureSharingType, .readOnly)
        XCTAssertNotEqual(DisplayShield.captureSharingType, .none)
    }

    func testExactCoverageIsAccepted() {
        XCTAssertTrue(DisplayShield.coverageSatisfied(
            windows: [builtIn, external], screens: [builtIn, external]))
    }

    /// The regression: right size, wrong place. This is what shipped.
    func testAWindowOfTheRightSizeInTheWrongPlaceIsNotCoverage() {
        let misplaced = CGRect(x: -384, y: -2197, width: 1920, height: 1080)
        XCTAssertFalse(DisplayShield.coverageSatisfied(
            windows: [builtIn, misplaced], screens: [builtIn, external]))
    }

    func testAMissingWindowIsNotCoverage() {
        XCTAssertFalse(DisplayShield.coverageSatisfied(
            windows: [builtIn], screens: [builtIn, external]))
    }

    /// Mirrored displays share a rect and show the same content, so one window
    /// covering it covers both. Demanding one window per display reported an
    /// actually-covered mirrored Mac as uncovered — the shield was up, the
    /// screen was black, and Locked Use refused to open a window.
    ///
    /// The window server is why: it does not list a fully occluded twin as on
    /// screen, so the second window can never be found.
    func testMirroredScreensAreCoveredByOneWindow() {
        XCTAssertTrue(DisplayShield.coverageSatisfied(
            windows: [builtIn], screens: [builtIn, builtIn]))
    }

    /// Deduplicating by rect must not excuse a genuinely missing display: two
    /// screens at different rects still need two windows.
    func testDistinctScreensStillNeedTheirOwnWindows() {
        XCTAssertFalse(DisplayShield.coverageSatisfied(
            windows: [builtIn], screens: [builtIn, external]))
    }

    /// A window that covers only part of a display leaves the rest readable.
    func testAPartiallySizedWindowIsNotCoverage() {
        let short = CGRect(x: 0, y: 0, width: 1728, height: 900)
        XCTAssertFalse(DisplayShield.coverageSatisfied(
            windows: [short], screens: [builtIn]))
    }

    /// Unrelated windows of this process must not be mistaken for shields, but
    /// their presence must not break a genuine match either.
    func testExtraWindowsDoNotDecideTheOutcome() {
        let unrelated = CGRect(x: 40, y: 60, width: 400, height: 300)
        XCTAssertTrue(DisplayShield.coverageSatisfied(
            windows: [unrelated, builtIn], screens: [builtIn]))
        XCTAssertFalse(DisplayShield.coverageSatisfied(
            windows: [unrelated], screens: [builtIn]))
    }

    /// No screens is not "nothing to cover, so covered". A shield that reported
    /// success with no displays would let a window open against a machine whose
    /// display state could not be read.
    func testNoScreensIsNotCoverage() {
        XCTAssertFalse(DisplayShield.coverageSatisfied(windows: [builtIn], screens: []))
    }
}
