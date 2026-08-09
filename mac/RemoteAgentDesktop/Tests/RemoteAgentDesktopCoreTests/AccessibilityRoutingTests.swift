import XCTest
@testable import RemoteAgentDesktopCore

/// The Accessibility ops are the channel that operates an app while the screen
/// is locked — reaching the element tree in-process, which the lock screen does
/// not partition. These cover the routing and argument handling; the AX calls
/// themselves need a real trusted process and a running app, exercised on
/// device rather than here.
///
/// The op set is closed like the action vocabulary: an unknown op is refused,
/// and a missing required argument is refused before anything touches an app.
final class AccessibilityRoutingTests: XCTestCase {
    private func router() -> RequestRouter {
        // No controller: the AX ops do not need Locked Use, only Accessibility
        // trust, which is exactly the point — they operate without unlocking.
        RequestRouter(desktop: DesktopService())
    }

    private func reply(_ json: String) -> [String: Any] {
        let data = router().handle(line: Data(json.utf8))
        return (try? JSONSerialization.jsonObject(with: data)) as? [String: Any] ?? [:]
    }

    func testUnknownOpIsRefused() {
        let r = reply(#"{"op":"ax_teleport"}"#)
        XCTAssertEqual(r["ok"] as? Bool, false)
        XCTAssertEqual(r["code"] as? String, "bad_request")
    }

    /// Without Accessibility trust the ops must report a permission gap, not a
    /// generic failure — an operator needs to know to grant it, not to debug a
    /// crash. When the test process *is* trusted the call proceeds and fails
    /// later for want of the target app; either way it must not 500.
    func testAXOpsReportTheTrustGapRatherThanCrashing() {
        for op in ["ax_read", "ax_press", "ax_setvalue"] {
            let r = reply(#"{"op":"\#(op)","app":"NoSuchApp","path":[0],"value":"x"}"#)
            XCTAssertEqual(r["ok"] as? Bool, false, op)
            if !Accessibility.isTrusted() {
                XCTAssertEqual(
                    r["code"] as? String, "unsupported",
                    "\(op) should surface the Accessibility trust gap")
            }
        }
    }

    func testPressRequiresAPath() {
        // With trust, a missing path is the first thing refused. Without trust,
        // the trust gap is refused first; both are correct refusals, neither is
        // a 500.
        let r = reply(#"{"op":"ax_press","app":"CatDesk"}"#)
        XCTAssertEqual(r["ok"] as? Bool, false)
        if Accessibility.isTrusted() {
            XCTAssertEqual(r["code"] as? String, "bad_request")
            XCTAssertEqual(r["error"] as? String, "path is required")
        }
    }

    func testSetValueRequiresPathAndValue() {
        let missingValue = reply(#"{"op":"ax_setvalue","app":"CatDesk","path":[0]}"#)
        XCTAssertEqual(missingValue["ok"] as? Bool, false)
        if Accessibility.isTrusted() {
            XCTAssertEqual(missingValue["code"] as? String, "bad_request")
            XCTAssertEqual(missingValue["error"] as? String, "value is required")
        }
    }

    /// The wire request must carry the AX addressing fields through decoding —
    /// a dropped field would reach the app layer as a nil target.
    func testRequestDecodesAXFields() throws {
        let json = #"{"op":"ax_setvalue","app":"CatDesk","bundle_id":"com.x.y","path":[1,4,2],"value":"你好"}"#
        let request = try JSONDecoder().decode(Request.self, from: Data(json.utf8))
        XCTAssertEqual(request.op, "ax_setvalue")
        XCTAssertEqual(request.app, "CatDesk")
        XCTAssertEqual(request.bundleID, "com.x.y")
        XCTAssertEqual(request.path, [1, 4, 2])
        XCTAssertEqual(request.value, "你好")
    }
}
