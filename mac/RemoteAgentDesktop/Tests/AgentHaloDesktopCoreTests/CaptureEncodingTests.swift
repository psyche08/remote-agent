import CoreGraphics
import Foundation
import XCTest
@testable import AgentHaloDesktopCore

final class CaptureEncodingTests: XCTestCase {
    func testCompositeCaptureIsAnInMemoryPNG() throws {
        let image = try makeImage(width: 2, height: 2)
        let data = try DesktopService.encodeCompositePNG([
            (CGRect(x: 0, y: 0, width: 2, height: 2), image),
        ])

        XCTAssertTrue(data.starts(with: Data([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a])))
        XCTAssertGreaterThan(data.count, 8)
    }

    func testCompositeCaptureRejectsMissingAndUnsafeFrames() throws {
        XCTAssertThrowsError(try DesktopService.encodeCompositePNG([]))

        let image = try makeImage(width: 1, height: 1)
        XCTAssertThrowsError(try DesktopService.encodeCompositePNG([
            (CGRect(x: 0, y: 0, width: 100_000_001, height: 1), image),
        ]))
    }

    func testProtocolSerializesCaptureBytesWithoutAPath() throws {
        let png = Data([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 1, 2, 3])
        let response = RequestRouter.capturedResponse(
            action: .screenCapture, data: png, mediaType: "image/png")
        let object = try XCTUnwrap(
            try JSONSerialization.jsonObject(with: response.encoded()) as? [String: Any])

        XCTAssertEqual(object["ok"] as? Bool, true)
        XCTAssertEqual(object["action"] as? String, "screen.capture")
        XCTAssertEqual(object["media_type"] as? String, "image/png")
        XCTAssertEqual(
            Data(base64Encoded: try XCTUnwrap(object["image_base64"] as? String)), png)
        XCTAssertNil(object["path"])
    }

    func testScreenshotPointsMapAcrossNegativeOriginDisplays() throws {
        let frames = [
            CGRect(x: 0, y: 0, width: 100, height: 100),
            CGRect(x: -80, y: 20, width: 80, height: 60),
            CGRect(x: 10, y: -50, width: 50, height: 50),
            CGRect(x: 20, y: 100, width: 30, height: 40),
        ]

        XCTAssertEqual(try DesktopService.globalPoint(
            forScreenshotPoint: CGPoint(x: 80, y: 50), displayFrames: frames),
            CGPoint(x: 0, y: 0))
        XCTAssertEqual(try DesktopService.globalPoint(
            forScreenshotPoint: CGPoint(x: 0, y: 70), displayFrames: frames),
            CGPoint(x: -80, y: 20))
        XCTAssertEqual(try DesktopService.globalPoint(
            forScreenshotPoint: CGPoint(x: 90, y: 0), displayFrames: frames),
            CGPoint(x: 10, y: -50))
        XCTAssertEqual(try DesktopService.globalPoint(
            forScreenshotPoint: CGPoint(x: 100, y: 150), displayFrames: frames),
            CGPoint(x: 20, y: 100))

        // This is inside the rectangular union but in empty space between the
        // left and above displays, so it must not become a stray global click.
        XCTAssertThrowsError(try DesktopService.globalPoint(
            forScreenshotPoint: CGPoint(x: 30, y: 10), displayFrames: frames))
        XCTAssertThrowsError(try DesktopService.globalPoint(
            forScreenshotPoint: CGPoint(x: 180, y: 0), displayFrames: frames))
    }

    func testCompositeDestinationFlipsGlobalDisplayYIntoBitmapY() {
        let bounds = CGRect(x: 0, y: -50, width: 100, height: 150)
        let above = CGRect(x: 10, y: -50, width: 50, height: 50)
        let primary = CGRect(x: 0, y: 0, width: 100, height: 100)

        XCTAssertEqual(
            DesktopService.compositeDestination(for: above, in: bounds),
            CGRect(x: 10, y: 100, width: 50, height: 50))
        XCTAssertEqual(
            DesktopService.compositeDestination(for: primary, in: bounds),
            CGRect(x: 0, y: 0, width: 100, height: 100))
    }

    private func makeImage(width: Int, height: Int) throws -> CGImage {
        let colorSpace = try XCTUnwrap(CGColorSpace(name: CGColorSpace.sRGB))
        let context = try XCTUnwrap(CGContext(
            data: nil, width: width, height: height, bitsPerComponent: 8,
            bytesPerRow: 0, space: colorSpace,
            bitmapInfo: CGImageAlphaInfo.premultipliedLast.rawValue))
        context.setFillColor(CGColor(red: 0.2, green: 0.4, blue: 0.8, alpha: 1))
        context.fill(CGRect(x: 0, y: 0, width: width, height: height))
        return try XCTUnwrap(context.makeImage())
    }
}
