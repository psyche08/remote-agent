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
        XCTAssertEqual(
            SystemLockScreenAuthorizationInteractor.discoveryTimeout, 8,
            "wake-to-field discovery must be bounded without consuming grant TTL")
        XCTAssertThrowsError(try SystemLockScreenAuthorizationInteractor.requireResponsive(
            .cannotComplete, operation: "test loginwindow read")) { error in
            XCTAssertTrue(error is LockScreenAuthorizationError)
        }
    }

    func testLoginwindowSelectionRequiresExactBundlePathsAndOneLivePID() throws {
        typealias Identity = SystemLockScreenAuthorizationInteractor.RunningApplicationIdentity
        let exact = Identity(
            bundleIdentifier: "com.apple.loginwindow",
            bundlePath: "/System/Library/CoreServices/loginwindow.app",
            executablePath:
                "/System/Library/CoreServices/loginwindow.app/Contents/MacOS/loginwindow",
            isTerminated: false,
            processIdentifier: 41)
        XCTAssertEqual(
            try SystemLockScreenAuthorizationInteractor
                .exactLoginwindowProcessIdentifier(from: [exact]),
            41)

        let wrongFocusedFake = Identity(
            bundleIdentifier: "com.apple.loginwindow",
            bundlePath: "/tmp/loginwindow.app",
            executablePath: "/tmp/loginwindow.app/Contents/MacOS/loginwindow",
            isTerminated: false,
            processIdentifier: 99)
        XCTAssertEqual(
            try SystemLockScreenAuthorizationInteractor
                .exactLoginwindowProcessIdentifier(from: [wrongFocusedFake, exact]),
            41,
            "a focused same-bundle fake must never displace the exact system process")

        for missingIdentity in [
            Identity(
                bundleIdentifier: nil, bundlePath: exact.bundlePath,
                executablePath: exact.executablePath, isTerminated: false,
                processIdentifier: 41),
            Identity(
                bundleIdentifier: exact.bundleIdentifier, bundlePath: nil,
                executablePath: exact.executablePath, isTerminated: false,
                processIdentifier: 41),
            Identity(
                bundleIdentifier: exact.bundleIdentifier, bundlePath: exact.bundlePath,
                executablePath: nil, isTerminated: false, processIdentifier: 41),
            Identity(
                bundleIdentifier: exact.bundleIdentifier, bundlePath: exact.bundlePath,
                executablePath: exact.executablePath, isTerminated: true,
                processIdentifier: 41),
            Identity(
                bundleIdentifier: exact.bundleIdentifier, bundlePath: exact.bundlePath,
                executablePath: exact.executablePath, isTerminated: false,
                processIdentifier: 0),
        ] {
            XCTAssertThrowsError(
                try SystemLockScreenAuthorizationInteractor
                    .exactLoginwindowProcessIdentifier(from: [missingIdentity]))
        }

        var duplicate = exact
        duplicate = Identity(
            bundleIdentifier: duplicate.bundleIdentifier,
            bundlePath: duplicate.bundlePath,
            executablePath: duplicate.executablePath,
            isTerminated: duplicate.isTerminated,
            processIdentifier: 42)
        XCTAssertThrowsError(
            try SystemLockScreenAuthorizationInteractor
                .exactLoginwindowProcessIdentifier(from: [exact, duplicate])) { error in
            XCTAssertTrue(
                (error as? LockScreenAuthorizationError)?.detail.contains("multiple exact") == true)
        }

        struct App: Equatable {
            let token: Int
            let identity: Identity
        }
        let bound = App(token: 1, identity: exact)
        XCTAssertNoThrow(
            try SystemLockScreenAuthorizationInteractor.requireSameExactLoginwindow(
                bound,
                from: [bound],
                identity: { $0.identity },
                isSameInstance: { $0.token == $1.token }))
        let samePIDReplacement = App(token: 2, identity: exact)
        XCTAssertThrowsError(
            try SystemLockScreenAuthorizationInteractor.requireSameExactLoginwindow(
                bound,
                from: [samePIDReplacement],
                identity: { $0.identity },
                isSameInstance: { $0.token == $1.token })) { error in
            XCTAssertTrue(
                (error as? LockScreenAuthorizationError)?.detail.contains(
                    "instance changed during authorization") == true)
        }
        XCTAssertThrowsError(
            try SystemLockScreenAuthorizationInteractor.requireSameExactLoginwindow(
                bound,
                from: [],
                identity: { $0.identity },
                isSameInstance: { $0.token == $1.token }))
        XCTAssertThrowsError(
            try SystemLockScreenAuthorizationInteractor.requireSameExactLoginwindow(
                bound,
                from: [bound, samePIDReplacement],
                identity: { $0.identity },
                isSameInstance: { $0.token == $1.token }))
    }

    func testLoginwindowSeedsAreOrderedAndDeduplicated() {
        let seeds = SystemLockScreenAuthorizationInteractor.orderedSearchSeeds(
            focusedUIElement: 10,
            focusedWindow: 20,
            windows: [20, 30, 10],
            applicationRoot: 40,
            areEqual: ==)
        XCTAssertEqual(seeds, [10, 20, 30, 40])
    }

    func testEachOrderedLoginwindowSeedCanReachTheExactField() throws {
        struct Node: Equatable {
            let id: Int
            let identifier: String?
            let children: [Int]
        }
        for targetSeed in 1...4 {
            let nodes = Dictionary(uniqueKeysWithValues: (1...4).map { seed in
                let fieldID = seed + 10
                return (
                    seed,
                    Node(
                        id: seed,
                        identifier: nil,
                        children: seed == targetSeed ? [fieldID] : []))
            } + [
                (
                    targetSeed + 10,
                    Node(
                        id: targetSeed + 10,
                        identifier: "UserPasswordTextField",
                        children: []))
            ])
            let seeds = [1, 2, 3, 4].map { nodes[$0]! }
            let result = try SystemLockScreenAuthorizationInteractor.exactPasswordField(
                seeds: seeds,
                expectedProcessIdentifier: 41,
                areEqual: ==,
                processIdentifier: { _ in 41 },
                identifier: { $0.identifier },
                children: { node in try node.children.map { try XCTUnwrap(nodes[$0]) } })
            XCTAssertEqual(result?.id, targetSeed + 10, "seed \(targetSeed)")
        }
    }

    func testPasswordFieldSearchRejectsForeignPIDAndUsesLaterExactSeed() throws {
        struct Node: Equatable {
            let id: Int
            let pid: pid_t
            let identifier: String?
            let children: [Int]
        }
        let nodes = [
            1: Node(id: 1, pid: 900, identifier: "UserPasswordTextField", children: [2]),
            2: Node(id: 2, pid: 41, identifier: "UserPasswordTextField", children: []),
            3: Node(id: 3, pid: 41, identifier: nil, children: [4]),
            4: Node(id: 4, pid: 41, identifier: "UserPasswordTextField", children: []),
        ]
        let result = try SystemLockScreenAuthorizationInteractor.exactPasswordField(
            seeds: [nodes[1]!, nodes[3]!],
            expectedProcessIdentifier: 41,
            areEqual: ==,
            processIdentifier: { $0.pid },
            identifier: { $0.identifier },
            children: { node in try node.children.map { try XCTUnwrap(nodes[$0]) } })
        XCTAssertEqual(result?.id, 4)
    }

    func testPasswordFieldSearchIsBoundedByDepthAndNodeCount() throws {
        struct Node: Equatable {
            let id: Int
            let children: [Int]
        }
        let nodes = [
            1: Node(id: 1, children: [2, 3]),
            2: Node(id: 2, children: [4]),
            3: Node(id: 3, children: []),
            4: Node(id: 4, children: []),
        ]
        func search(depth: Int, count: Int) throws -> Node? {
            try SystemLockScreenAuthorizationInteractor.exactPasswordField(
                seeds: [nodes[1]!],
                expectedProcessIdentifier: 41,
                maximumDepth: depth,
                maximumNodes: count,
                areEqual: ==,
                processIdentifier: { _ in 41 },
                identifier: { $0.id == 4 ? "UserPasswordTextField" : nil },
                children: { node in try node.children.map { try XCTUnwrap(nodes[$0]) } })
        }
        XCTAssertNil(try search(depth: 1, count: 20))
        XCTAssertNil(try search(depth: 20, count: 3))
        XCTAssertEqual(try search(depth: 2, count: 4)?.id, 4)
    }

    func testRootAndFieldPIDMismatchAreRejected() {
        for description in ["application root", "password field"] {
            XCTAssertThrowsError(
                try SystemLockScreenAuthorizationInteractor.requireExactProcessIdentifier(
                    actual: 99,
                    expected: 41,
                    elementDescription: description)) { error in
                XCTAssertTrue(
                    (error as? LockScreenAuthorizationError)?.detail.contains(
                        "not owned by the exact loginwindow process") == true)
            }
        }
        XCTAssertNoThrow(
            try SystemLockScreenAuthorizationInteractor.requireExactProcessIdentifier(
                actual: 41, expected: 41, elementDescription: "password field"))
    }

    func testEmptyLockScreenValueRequiresASettableAttribute() {
        XCTAssertNoThrow(try SystemLockScreenAuthorizationInteractor.requireEmptyValueWritable(
            queryStatus: .success, isSettable: true))

        XCTAssertThrowsError(
            try SystemLockScreenAuthorizationInteractor.requireEmptyValueWritable(
                queryStatus: .success, isSettable: false)) { error in
            XCTAssertEqual(
                (error as? LockScreenAuthorizationError)?.detail,
                "the macOS lock-screen authorization field does not accept an empty value")
        }
        XCTAssertThrowsError(
            try SystemLockScreenAuthorizationInteractor.requireEmptyValueWritable(
                queryStatus: .attributeUnsupported, isSettable: true)) { error in
            XCTAssertTrue(
                (error as? LockScreenAuthorizationError)?.detail.contains("could not verify") == true)
        }
        XCTAssertThrowsError(
            try SystemLockScreenAuthorizationInteractor.requireEmptyValueWritable(
                queryStatus: .cannotComplete, isSettable: true)) { error in
            XCTAssertTrue(
                (error as? LockScreenAuthorizationError)?.detail.contains("timed out") == true)
        }
    }

    func testEmptyLockScreenValueWriteMustSucceedBeforeSubmit() {
        XCTAssertNoThrow(
            try SystemLockScreenAuthorizationInteractor.requireEmptyValueWritten(.success))
        XCTAssertThrowsError(
            try SystemLockScreenAuthorizationInteractor.requireEmptyValueWritten(.failure)) { error in
            XCTAssertTrue(
                (error as? LockScreenAuthorizationError)?.detail.contains("could not write") == true)
        }
        XCTAssertThrowsError(
            try SystemLockScreenAuthorizationInteractor.requireEmptyValueWritten(.cannotComplete)) {
                error in
            XCTAssertTrue(
                (error as? LockScreenAuthorizationError)?.detail.contains("timed out") == true)
        }
    }

    func testConfirmFailureNeverRetriesTheEmptyValueAssignment() {
        var preparationCount = 0
        var confirmationCount = 0
        XCTAssertThrowsError(
            try SystemLockScreenAuthorizationInteractor.performSingleSubmission(
                prepareEmptyValue: {
                    preparationCount += 1
                },
                confirm: {
                    confirmationCount += 1
                    throw LockScreenAuthorizationError("confirm failed")
                })) { error in
            XCTAssertEqual(
                (error as? LockScreenAuthorizationError)?.detail, "confirm failed")
        }
        XCTAssertEqual(preparationCount, 1)
        XCTAssertEqual(confirmationCount, 1)
    }

    func testEmptyValueFailureNeverAttemptsConfirm() {
        var preparationCount = 0
        var confirmationCount = 0
        XCTAssertThrowsError(
            try SystemLockScreenAuthorizationInteractor.performSingleSubmission(
                prepareEmptyValue: {
                    preparationCount += 1
                    throw LockScreenAuthorizationError("empty value failed")
                },
                confirm: {
                    confirmationCount += 1
                })) { error in
            XCTAssertEqual(
                (error as? LockScreenAuthorizationError)?.detail,
                "empty value failed")
        }
        XCTAssertEqual(preparationCount, 1)
        XCTAssertEqual(confirmationCount, 0)
    }

    func testSubmissionPreflightFailurePublishesNoGrant() {
        let directory = NSTemporaryDirectory() + "agenthalo-ax-preflight-\(UUID().uuidString)"
        let grantPath = (directory as NSString).appendingPathComponent("grant.json")
        addTeardownBlock { try? FileManager.default.removeItem(atPath: directory) }
        var grantPreparationCount = 0
        var submissionCount = 0

        XCTAssertThrowsError(
            try SystemLockScreenAuthorizationInteractor.performGrantGatedSubmission(
                preflight: { () -> String in
                    throw LockScreenAuthorizationError(
                        "the exact field exposes no confirmation action")
                },
                prepareGrant: {
                    grantPreparationCount += 1
                    try FileManager.default.createDirectory(
                        atPath: directory, withIntermediateDirectories: true)
                    try Data("ambient authority".utf8).write(
                        to: URL(fileURLWithPath: grantPath))
                },
                submit: { _ in submissionCount += 1 })) { error in
            XCTAssertEqual(
                (error as? LockScreenAuthorizationError)?.detail,
                "the exact field exposes no confirmation action")
        }
        XCTAssertEqual(grantPreparationCount, 0)
        XCTAssertEqual(submissionCount, 0)
        XCTAssertFalse(
            FileManager.default.fileExists(atPath: grantPath),
            "failed value/action preflight published ambient grant authority")
    }

    func testLoginwindowReplacementBeforeGrantPublishesAndSubmitsNothing() {
        var grantCount = 0
        var submissionCount = 0
        XCTAssertThrowsError(
            try SystemLockScreenAuthorizationInteractor.performGrantGatedSubmission(
                preflight: { kAXConfirmAction as String },
                revalidateBeforeGrant: {
                    throw LockScreenAuthorizationError(
                        "loginwindow instance changed before grant")
                },
                prepareGrant: { grantCount += 1 },
                submit: { _ in submissionCount += 1 }))
        XCTAssertEqual(grantCount, 0)
        XCTAssertEqual(submissionCount, 0)
    }

    func testLoginwindowReplacementAfterEmptyWritePerformsNoActionOrRetry() {
        var emptyWriteCount = 0
        var actionCount = 0
        var sameInstance = true
        XCTAssertThrowsError(
            try SystemLockScreenAuthorizationInteractor.performSingleSubmission(
                prepareEmptyValue: {
                    XCTAssertTrue(sameInstance)
                    emptyWriteCount += 1
                    sameInstance = false
                },
                confirm: {
                    guard sameInstance else {
                        throw LockScreenAuthorizationError(
                            "loginwindow instance changed before action")
                    }
                    actionCount += 1
                }))
        XCTAssertEqual(emptyWriteCount, 1)
        XCTAssertEqual(actionCount, 0)
    }

    func testPreparedSubmissionCallsGrantExactlyOnceBeforeMutation() throws {
        var events: [String] = []
        try SystemLockScreenAuthorizationInteractor.performGrantGatedSubmission(
            preflight: {
                events.append("preflight")
                return kAXConfirmAction as String
            },
            prepareGrant: { events.append("grant") },
            submit: { action in events.append("submit:\(action)") })
        XCTAssertEqual(
            events, ["preflight", "grant", "submit:\(kAXConfirmAction as String)"])
    }
}
