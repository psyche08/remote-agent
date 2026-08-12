import CoreGraphics
import XCTest
@testable import AgentHaloDesktopCore

final class WakeProbeTests: XCTestCase {
    func testMovesOnePointRightInsidePrimaryDisplay() throws {
        let point = try DesktopService.wakeProbePoint(
            current: CGPoint(x: 100, y: 200),
            displayFrames: [CGRect(x: 0, y: 0, width: 1920, height: 1080)])

        XCTAssertEqual(point, CGPoint(x: 101, y: 200))
    }

    func testMovesLeftAtRightBoundary() throws {
        let point = try DesktopService.wakeProbePoint(
            current: CGPoint(x: 1919.5, y: 500),
            displayFrames: [CGRect(x: 0, y: 0, width: 1920, height: 1080)])

        XCTAssertEqual(point, CGPoint(x: 1918.5, y: 500))
    }

    func testMovesVerticallyWhenHorizontalSpanCannotAdmitOnePoint() throws {
        let point = try DesktopService.wakeProbePoint(
            current: CGPoint(x: 0.25, y: 4),
            displayFrames: [CGRect(x: 0, y: 0, width: 0.5, height: 10)])

        XCTAssertEqual(point, CGPoint(x: 0.25, y: 5))
    }

    func testMovesInsideNegativeOriginDisplay() throws {
        let frame = CGRect(x: -1920, y: -200, width: 1920, height: 1080)
        let point = try DesktopService.wakeProbePoint(
            current: CGPoint(x: -1920, y: -200), displayFrames: [frame])

        XCTAssertEqual(point, CGPoint(x: -1919, y: -200))
        XCTAssertTrue(point.x >= frame.minX && point.x < frame.maxX)
        XCTAssertTrue(point.y >= frame.minY && point.y < frame.maxY)
    }

    func testMovesWithinTheDisplayContainingCursor() throws {
        let primary = CGRect(x: 0, y: 0, width: 1512, height: 982)
        let left = CGRect(x: -1920, y: 80, width: 1920, height: 1080)
        let current = CGPoint(x: -1, y: 500)
        let point = try DesktopService.wakeProbePoint(
            current: current, displayFrames: [primary, left])

        XCTAssertEqual(point, CGPoint(x: -2, y: 500))
        XCTAssertTrue(point.x >= left.minX && point.x < left.maxX)
        XCTAssertTrue(point.y >= left.minY && point.y < left.maxY)
    }

    func testRejectsCursorInDisplayLayoutGap() {
        let first = CGRect(x: -200, y: 0, width: 100, height: 80)
        let second = CGRect(x: 100, y: 0, width: 100, height: 80)
        XCTAssertThrowsError(try DesktopService.wakeProbePoint(
            current: CGPoint(x: 0, y: 20), displayFrames: [first, second]))
    }

    func testRejectsCursorOutsideEveryDisplay() {
        XCTAssertThrowsError(try DesktopService.wakeProbePoint(
            current: CGPoint(x: 500, y: 500),
            displayFrames: [CGRect(x: 0, y: 0, width: 100, height: 100)]))
    }

    func testRejectsUnavailableOrInvalidGeometry() {
        XCTAssertThrowsError(try DesktopService.wakeProbePoint(
            current: CGPoint(x: 10, y: 10), displayFrames: []))
        XCTAssertThrowsError(try DesktopService.wakeProbePoint(
            current: CGPoint(x: CGFloat.nan, y: 10),
            displayFrames: [CGRect(x: 0, y: 0, width: 100, height: 100)]))
        XCTAssertThrowsError(try DesktopService.wakeProbePoint(
            current: CGPoint(x: 10, y: 10),
            displayFrames: [CGRect(x: 0, y: 0, width: 0, height: 100)]))
        XCTAssertThrowsError(try DesktopService.wakeProbePoint(
            current: CGPoint(x: 10, y: 10),
            displayFrames: [
                CGRect(x: 0, y: 0, width: 100, height: 100),
                CGRect(x: 200, y: 0, width: CGFloat.infinity, height: 100),
            ]))
    }

    func testRejectsDisplayThatCannotAdmitOnePointMove() {
        XCTAssertThrowsError(try DesktopService.wakeProbePoint(
            current: CGPoint(x: 0.25, y: 0.25),
            displayFrames: [CGRect(x: 0, y: 0, width: 0.5, height: 0.5)]))
    }
}
