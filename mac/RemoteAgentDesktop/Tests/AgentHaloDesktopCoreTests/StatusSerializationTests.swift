import XCTest
@testable import AgentHaloDesktopCore

/// The status response is assembled as `[String: Any]` and handed to
/// JSONSerialization. That API cannot encode a Swift struct, and it does not
/// return an error when given one — it raises an Objective-C exception, which
/// Swift cannot catch, so the helper does not fail the request: it dies.
///
/// This is not theoretical. Enabling Locked Use on a real device crashed the
/// helper on the first status query, because the audit ring held `AuditEntry`
/// values. Nothing caught it earlier because with the feature off the ring is
/// empty, and an empty array encodes fine — so the type error only appears once
/// something has actually happened.
final class StatusSerializationTests: XCTestCase {
    private func makeController(_ system: FakeSystem) -> LockedUseController {
        let directory = NSTemporaryDirectory() + "ra-status-\(UUID().uuidString)"
        addTeardownBlock { try? FileManager.default.removeItem(atPath: directory) }
        let config = ComputerUseConfig(
            enabled: true,
            lockedUse: LockedUseConfig(
                enabled: true,
                grantDirectory: (directory as NSString).appendingPathComponent("locked-use")))
        system.grantDirectory = config.lockedUse.grantDirectory
        let controller = LockedUseController(
            config: config, deviceID: "mac-test", system: system,
            receiptVerifier: { nonce, _ in system.receiptMatches(nonce) },
            pendingReceiptVerifier: { nonce, _ in system.pendingReceiptMatches(nonce) },
            completionReceiptVerifier: { nonce, _ in
                system.completionReceiptMatches(nonce)
            })
        controller.start()
        addTeardownBlock { controller.stop() }
        return controller
    }

    func testStatusIsSerializableOnceSomethingHasHappened() throws {
        let system = FakeSystem()
        let controller = makeController(system)

        // Arming alone records an entry, which is what the crash needed.
        XCTAssertFalse(controller.auditEntries().isEmpty, "nothing was audited to serialize")

        let status = controller.status()
        let lockedUse = try XCTUnwrap(status["locked_use"] as? [String: Any])
        XCTAssertNotNil(
            lockedUse["requires_manual_recovery"] as? Bool,
            "permanent quarantine state must remain an explicit JSON boolean")
        XCTAssertTrue(
            JSONSerialization.isValidJSONObject(status),
            "status contains a value JSONSerialization cannot encode: \(status)")
        XCTAssertNoThrow(try JSONSerialization.data(withJSONObject: status))
    }

    /// A window's whole lifecycle records the richest entries — turn ids, nonce
    /// prefixes, reasons — so it exercises every optional field.
    func testStatusStaysSerializableAcrossAWindowLifecycle() throws {
        let system = FakeSystem()
        let controller = makeController(system)
        try controller.openWindow(turnID: "turn-1")
        controller.closeWindow(reason: "done")

        let status = controller.status()
        XCTAssertTrue(
            JSONSerialization.isValidJSONObject(status),
            "status is not serializable after a window opened and closed")
        let data = try JSONSerialization.data(withJSONObject: status)

        // The round trip must preserve the audit fields the console reads.
        let decoded = try XCTUnwrap(
            try JSONSerialization.jsonObject(with: data) as? [String: Any])
        let audit = try XCTUnwrap(decoded["audit"] as? [[String: Any]])
        XCTAssertTrue(audit.contains { $0["event"] as? String == "window_opened" })
        XCTAssertTrue(audit.contains { ($0["turn_id"] as? String) == "turn-1" })

        // And it must still carry no secrets.
        let text = String(decoding: data, as: UTF8.self)
        XCTAssertFalse(text.contains("signature"))
        XCTAssertFalse(text.contains("payload"))
    }

    /// The response the socket actually returns has to encode too — the router
    /// wraps status in its own envelope, and a failure there is the same crash
    /// one layer out.
    func testTheRoutedStatusResponseEncodes() {
        let system = FakeSystem()
        let controller = makeController(system)
        let router = RequestRouter(desktop: DesktopService(), controller: controller)

        let reply = router.handle(line: Data(#"{"op":"status"}"#.utf8))
        XCTAssertFalse(reply.isEmpty)
        XCTAssertTrue(
            String(decoding: reply, as: UTF8.self).contains("\"ok\":true"),
            "the routed status reply was not a success: \(String(decoding: reply, as: UTF8.self))")
    }
}
