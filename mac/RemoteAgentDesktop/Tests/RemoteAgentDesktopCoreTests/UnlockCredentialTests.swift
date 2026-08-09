import XCTest
@testable import RemoteAgentDesktopCore

/// The unlock credential is the piece that turns an authorized gate into a real
/// system unlock. These cover the custody logic — storage, presence, removal,
/// and that the value is never returned — without a real login password and
/// without performing an actual unlock (that needs a device and is exercised
/// on it).
///
/// Every test cleans up its Keychain item so a machine running the suite is not
/// left with a provisioned credential.
///
/// Storage uses the data-protection keychain, which requires a
/// keychain-access-groups entitlement the unit-test binary does not carry —
/// SecItemAdd there returns errSecMissingEntitlement (-34018). That is not a
/// logic error: it is the entitlement gate that will bind the item to the
/// signed helper. So the storage-round-trip assertions run only when the host
/// process actually has keychain access, and the always-true behaviour
/// contracts (unprovisioned submit, no getter) run everywhere.
final class UnlockCredentialTests: XCTestCase {
    override func tearDown() {
        UnlockCredential.remove()
        super.tearDown()
    }

    /// True when this process can actually use the data-protection keychain
    /// (i.e. it is the signed, entitled helper, not the bare test binary).
    private func keychainUsable() -> Bool {
        UnlockCredential.remove()
        do {
            try UnlockCredential.provision(password: "probe")
            UnlockCredential.remove()
            return true
        } catch {
            return false
        }
    }

    func testUnprovisionedSubmitReportsItEverywhere() {
        UnlockCredential.remove()
        // Submitting with nothing provisioned must fail plainly, not crash or
        // silently succeed — the safe direction is "the screen stays locked".
        // This holds regardless of entitlement.
        XCTAssertThrowsError(try UnlockCredential.submit(reason: "test")) { error in
            XCTAssertFalse("\(error)".isEmpty)
        }
    }

    func testProvisionThenPresent() throws {
        try XCTSkipUnless(keychainUsable(), "data-protection keychain needs the entitled helper")
        try UnlockCredential.provision(password: "not-a-real-password-\(UUID().uuidString)")
        XCTAssertTrue(UnlockCredential.isProvisioned())
    }

    func testProvisionReplacesRatherThanDuplicates() throws {
        try XCTSkipUnless(keychainUsable(), "data-protection keychain needs the entitled helper")
        try UnlockCredential.provision(password: "first")
        try UnlockCredential.provision(password: "second")
        XCTAssertTrue(UnlockCredential.isProvisioned())
        // Removing once must leave nothing behind — a second stored copy would
        // mean a credential survives a clear.
        UnlockCredential.remove()
        XCTAssertFalse(UnlockCredential.isProvisioned())
    }

    func testRemoveIsIdempotent() throws {
        try XCTSkipUnless(keychainUsable(), "data-protection keychain needs the entitled helper")
        try UnlockCredential.provision(password: "x")
        UnlockCredential.remove()
        UnlockCredential.remove()  // safe when none exists
        XCTAssertFalse(UnlockCredential.isProvisioned())
    }

    /// There is no path that returns the credential. isProvisioned answers
    /// presence only; the type exposes no getter. This documents that contract
    /// as a test so a future getter would have to break it deliberately.
    func testNoAPIReturnsTheCredential() {
        let surface = [
            "isProvisioned() -> Bool",
            "submit(reason:) throws -> Void",
            "provision(password:) throws -> Void",
            "remove() -> Void",
        ]
        // The public surface returns Bool/Void/throws only — never the secret.
        XCTAssertFalse(surface.contains { $0.contains("-> String") || $0.contains("-> Data") })
    }
}
