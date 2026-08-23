import XCTest
@testable import AgentHaloDesktopCore

final class ConfigTests: XCTestCase {
    /// A real config.json fills in only what the operator set. A decoder that
    /// demanded every key would reject the common case, and the failure would
    /// present as "computer use is not configured" — the feature silently
    /// absent rather than misconfigured.
    func testDecodesAPartialBlock() throws {
        let json = Data(#"""
            {"device_id":"mac-1","computer_use":{"enabled":true,"locked_use":{"enabled":true}}}
            """#.utf8)
        let file = try JSONDecoder().decode(AgentConfigFile.self, from: json)
        XCTAssertEqual(file.deviceID, "mac-1")
        let computerUse = try XCTUnwrap(file.computerUse)
        XCTAssertTrue(computerUse.enabled)
        XCTAssertTrue(computerUse.lockedUse.enabled)
    }

    func testUnsetValuesTakeSafeDefaults() throws {
        let json = Data(#"{"computer_use":{"enabled":true,"locked_use":{"enabled":true}}}"#.utf8)
        let file = try JSONDecoder().decode(AgentConfigFile.self, from: json)
        let config = try XCTUnwrap(file.computerUse).normalized()

        XCTAssertEqual(config.lockedUse.grantTTLSeconds, 10)
        XCTAssertEqual(config.lockedUse.windowTTLSeconds, 300)
        XCTAssertEqual(config.lockedUse.inputRelockGraceMs, 250)
        // "Not stated" must mean the shield is required, never that it was
        // waived: the safe reading of silence is the restrictive one.
        XCTAssertTrue(config.lockedUse.shieldRequired)
    }

    func testOutOfRangeValuesAreClampedNotHonoured() {
        var config = ComputerUseConfig(
            enabled: true,
            lockedUse: LockedUseConfig(
                enabled: true, grantTTLSeconds: 9999, windowTTLSeconds: 99999,
                inputRelockGraceMs: 1))
        config = config.normalized()
        XCTAssertEqual(config.lockedUse.grantTTLSeconds, 15)
        XCTAssertEqual(config.lockedUse.windowTTLSeconds, 900)
        XCTAssertEqual(config.lockedUse.inputRelockGraceMs, 100)
    }

    /// A grant TTL at or above the window ceiling would let one minted grant
    /// outlive the window it was issued for.
    func testGrantTTLCannotOutliveTheWindow() {
        let config = ComputerUseConfig(
            enabled: true,
            lockedUse: LockedUseConfig(
                enabled: true, grantTTLSeconds: 15, windowTTLSeconds: 15)
        ).normalized()
        XCTAssertLessThanOrEqual(
            config.lockedUse.grantTTLSeconds, config.lockedUse.windowTTLSeconds)
    }

    /// Locked Use extends computer use; it can never be the only thing on.
    func testLockedUseCannotBeEnabledAlone() {
        let config = ComputerUseConfig(
            enabled: false, lockedUse: LockedUseConfig(enabled: true)
        ).normalized()
        XCTAssertFalse(config.lockedUse.enabled)
    }

    func testLockedUseCannotDisableTheShieldOrPhysicalInputGuard() {
        let config = ComputerUseConfig(
            enabled: true,
            lockedUse: LockedUseConfig(
                enabled: true, requireDisplayShield: false)
        ).normalized()
        XCTAssertTrue(config.lockedUse.shieldRequired)
    }

    func testMissingComputerUseBlockIsNotAnError() throws {
        let file = try JSONDecoder().decode(
            AgentConfigFile.self, from: Data(#"{"device_id":"mac-1"}"#.utf8))
        XCTAssertNil(file.computerUse)
    }

    func testClaudeAccessibilityIdentityUsesTheProviderDefaults() throws {
        let file = try JSONDecoder().decode(
            AgentConfigFile.self, from: Data(#"{"device_id":"mac-1"}"#.utf8))
        let policy = try XCTUnwrap(file.accessibilityTargetPolicies().first)
        XCTAssertEqual(policy.appName, "Claude")
        XCTAssertEqual(policy.bundleIdentifier, "com.anthropic.claudefordesktop")
        XCTAssertEqual(policy.teamIdentifier, "Q6L2SF6YDW")
        XCTAssertEqual(policy.appPath, "/Applications/Claude.app")
    }

    func testClaudeAccessibilityIdentityReadsExactConfiguredValues() throws {
        let file = try JSONDecoder().decode(
            AgentConfigFile.self,
            from: Data(#"""
                {"providers":{"claude":{
                  "app_name":"Claude Preview",
                  "desktop_bundle_id":"dev.example.claude",
                  "desktop_team_id":"A1B2C3D4E5",
                  "desktop_app_path":"/Applications/Claude Preview.app"
                }}}
                """#.utf8))
        let policy = try XCTUnwrap(file.accessibilityTargetPolicies().first)
        XCTAssertEqual(policy.appName, "Claude Preview")
        XCTAssertEqual(policy.bundleIdentifier, "dev.example.claude")
        XCTAssertEqual(policy.teamIdentifier, "A1B2C3D4E5")
        XCTAssertEqual(policy.appPath, "/Applications/Claude Preview.app")
    }

    func testMalformedClaudeAccessibilityIdentityFailsClosed() throws {
        let file = try JSONDecoder().decode(
            AgentConfigFile.self,
            from: Data(#"""
                {"providers":{"claude":{
                  "desktop_bundle_id":"dev.example.claude\" or true",
                  "desktop_team_id":"not-a-team",
                  "desktop_app_path":"relative/Claude.app"
                }}}
                """#.utf8))
        XCTAssertThrowsError(try file.accessibilityTargetPolicies()) { error in
            XCTAssertTrue(error is AccessibilityTargetError)
        }
    }
}
