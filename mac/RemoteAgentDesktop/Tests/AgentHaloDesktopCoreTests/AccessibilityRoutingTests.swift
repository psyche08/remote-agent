import ApplicationServices
import XCTest
@testable import AgentHaloDesktopCore

/// These cover Accessibility routing, bounds, and timeout classification. The
/// AX calls themselves need a real trusted process and running target and are
/// exercised on-device after the controller authorizes the owning turn.
///
/// The op set is closed like the action vocabulary: an unknown op is refused,
/// and a missing required argument is refused before anything touches an app.
final class AccessibilityRoutingTests: XCTestCase {
    private func router() -> RequestRouter {
        // No controller in this parser/transport fixture. Production routes use
        // the controller's ordinary-unlocked or owning-window operation lease;
        // this test seam is not evidence that AX can cross the macOS lock.
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
        for op in ["ax_read", "ax_press", "ax_setvalue", "ax_focus"] {
            let r = reply(#"{"op":"\#(op)","turn_id":"turn-test","app":"NoSuchApp","path":[0],"value":"x"}"#)
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
        let r = reply(#"{"op":"ax_press","turn_id":"turn-test","app":"CatDesk"}"#)
        XCTAssertEqual(r["ok"] as? Bool, false)
        if Accessibility.isTrusted() {
            XCTAssertEqual(r["code"] as? String, "bad_request")
            XCTAssertEqual(r["error"] as? String, "path is required")
        }
    }

    func testFocusRequiresAPath() {
        let r = reply(#"{"op":"ax_focus","turn_id":"turn-test","app":"CatDesk"}"#)
        XCTAssertEqual(r["ok"] as? Bool, false)
        if Accessibility.isTrusted() {
            XCTAssertEqual(r["code"] as? String, "bad_request")
            XCTAssertEqual(r["error"] as? String, "path is required")
        }
    }

    func testSetValueRequiresPathAndValue() {
        let missingValue = reply(#"{"op":"ax_setvalue","turn_id":"turn-test","app":"CatDesk","path":[0]}"#)
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

    func testAccessibilityNodeSerializesSafeStateFields() throws {
        let node = Accessibility.Node(
            role: "AXTextArea", identifier: "prompt-input", subrole: "AXSecureTextField",
            label: "Prompt", value: "", enabled: true, selected: false, focused: true,
            current: true, checked: false, pressed: true,
            actionable: false, path: [1, 2])
        let fields = RequestRouter.serializedAccessibilityNode(node)
        XCTAssertEqual(fields["role"] as? String, "AXTextArea")
        XCTAssertEqual(fields["identifier"] as? String, "prompt-input")
        XCTAssertEqual(fields["subrole"] as? String, "AXSecureTextField")
        XCTAssertEqual(fields["enabled"] as? Bool, true)
        XCTAssertEqual(fields["selected"] as? Bool, false)
        XCTAssertEqual(fields["focused"] as? Bool, true)
        XCTAssertEqual(fields["current"] as? Bool, true)
        XCTAssertEqual(fields["checked"] as? Bool, false)
        XCTAssertEqual(fields["pressed"] as? Bool, true)
        XCTAssertEqual(fields["path"] as? [Int], [1, 2])
        XCTAssertNoThrow(try JSONSerialization.data(withJSONObject: fields))

        let unknown = Accessibility.Node(
            role: "AXGroup", identifier: nil, subrole: nil,
            label: "", value: nil, enabled: nil, selected: nil, focused: nil,
            current: nil, checked: nil, pressed: nil,
            actionable: false, path: [])
        let absent = RequestRouter.serializedAccessibilityNode(unknown)
        XCTAssertTrue(absent["identifier"] is NSNull)
        XCTAssertTrue(absent["subrole"] is NSNull)
        XCTAssertTrue(absent["value"] is NSNull)
        XCTAssertTrue(absent["enabled"] is NSNull)
        XCTAssertTrue(absent["selected"] is NSNull)
        XCTAssertTrue(absent["focused"] is NSNull)
        XCTAssertTrue(absent["current"] is NSNull)
        XCTAssertTrue(absent["checked"] is NSNull)
        XCTAssertTrue(absent["pressed"] is NSNull)
    }

    func testARIACurrentPreservesNilFalseAndTrue() {
        XCTAssertNil(Accessibility.ariaCurrent(nil))
        XCTAssertEqual(Accessibility.ariaCurrent(false), false)
        XCTAssertEqual(Accessibility.ariaCurrent("false"), false)
        XCTAssertEqual(Accessibility.ariaCurrent("page"), true)
        XCTAssertEqual(Accessibility.ariaCurrent(true), true)
    }

    func testCheckedAndPressedStatesAreBinaryAndRoleGated() {
        XCTAssertNil(Accessibility.binaryControlState(nil))
        XCTAssertEqual(Accessibility.binaryControlState(false), false)
        XCTAssertEqual(Accessibility.binaryControlState(NSNumber(value: 0)), false)
        XCTAssertEqual(Accessibility.binaryControlState(NSNumber(value: 1)), true)
        XCTAssertNil(Accessibility.binaryControlState(NSNumber(value: 2)))
        XCTAssertNil(Accessibility.binaryControlState("mixed"))

        let checkbox = Accessibility.controlStates(
            role: kAXCheckBoxRole as String, subrole: nil,
            value: NSNumber(value: 0), ariaChecked: nil, ariaPressed: nil)
        XCTAssertEqual(checkbox.checked, false)
        XCTAssertNil(checkbox.pressed)

        let radio = Accessibility.controlStates(
            role: kAXRadioButtonRole as String, subrole: nil,
            value: NSNumber(value: 1), ariaChecked: nil, ariaPressed: nil)
        XCTAssertEqual(radio.checked, true)
        XCTAssertNil(radio.pressed)

        let toggle = Accessibility.controlStates(
            role: kAXButtonRole as String, subrole: kAXToggleSubrole as String,
            value: NSNumber(value: 0), ariaChecked: nil, ariaPressed: nil)
        XCTAssertNil(toggle.checked)
        XCTAssertEqual(toggle.pressed, false)

        let aria = Accessibility.controlStates(
            role: kAXButtonRole as String, subrole: nil, value: NSNumber(value: 1),
            ariaChecked: nil, ariaPressed: "true")
        XCTAssertNil(aria.checked)
        XCTAssertEqual(aria.pressed, true)

        let ordinaryButton = Accessibility.controlStates(
            role: kAXButtonRole as String, subrole: nil,
            value: NSNumber(value: 1), ariaChecked: nil, ariaPressed: nil)
        XCTAssertNil(ordinaryButton.checked)
        XCTAssertNil(ordinaryButton.pressed)

        let mixedARIA = Accessibility.controlStates(
            role: kAXCheckBoxRole as String, subrole: nil,
            value: NSNumber(value: 1), ariaChecked: "mixed", ariaPressed: nil)
        XCTAssertNil(mixedARIA.checked)
    }

    /// A negative path component used to pass `index < children.count` and then
    /// trap in Array.subscript, letting one socket request kill the helper (and
    /// any display shield it owned).
    func testNegativePathIsRefusedBeforeResolvingAnApp() {
        XCTAssertThrowsError(try Accessibility.validatePath([0, -1, 2])) { error in
            XCTAssertEqual(
                (error as? ActionError)?.message,
                "the element path contains a negative index")
        }
    }

    func testPathDepthIsBounded() {
        XCTAssertNoThrow(try Accessibility.validatePath(Array(repeating: 0, count: 40)))
        XCTAssertThrowsError(
            try Accessibility.validatePath(Array(repeating: 0, count: 41))) { error in
                XCTAssertEqual(
                    (error as? ActionError)?.message,
                    "the element path exceeds the maximum depth")
        }
    }

    func testBundleIdentifierHasPriorityOverDisplayName() {
        XCTAssertFalse(Accessibility.selectsApplication(
            candidateBundleID: "com.example.wrong", candidateName: "Duplicate",
            requestedBundleID: "com.example.target", requestedName: "Duplicate"))
        XCTAssertTrue(Accessibility.selectsApplication(
            candidateBundleID: "com.example.target", candidateName: "Renamed",
            requestedBundleID: "com.example.target", requestedName: "Duplicate"))
        XCTAssertTrue(Accessibility.selectsApplication(
            candidateBundleID: "com.example.any", candidateName: "Duplicate",
            requestedBundleID: nil, requestedName: "Duplicate"))
    }

    func testCannotCompleteIsABoundedTransportFailure() {
        XCTAssertLessThan(Accessibility.messagingTimeout, 25)
        XCTAssertThrowsError(try Accessibility.requireResponsive(
            .cannotComplete, operation: "test AX read")) { error in
            XCTAssertTrue(error is AccessibilityIPCError)
        }
        XCTAssertNoThrow(try Accessibility.requireResponsive(
            .success, operation: "test AX read"))

        XCTAssertLessThan(SystemLockScreenAuthorizationInteractor.messagingTimeout, 2)
        XCTAssertThrowsError(try SystemLockScreenAuthorizationInteractor.requireResponsive(
            .cannotComplete, operation: "test loginwindow read")) { error in
            XCTAssertTrue(error is LockScreenAuthorizationError)
        }
    }
}
