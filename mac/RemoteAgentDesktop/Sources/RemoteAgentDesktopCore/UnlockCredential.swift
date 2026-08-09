import Foundation
import LocalAuthentication
import Security

/// Holds and submits the user's unlock credential so an authorized turn can
/// complete a real system unlock without a person typing.
///
/// This is the piece the reference implementation (Codex) has and we lacked.
/// The screensaver right runs as a chain: our plug-in is only the *gate* that
/// says "a Locked Use turn is pending", and macOS then runs `builtin:
/// authenticate`, which needs an actual credential. Codex supplies it
/// programmatically through the authorization context; here that is done with
/// the public `LAContext.setCredential` + `evaluatePolicy`, which Apple
/// documents as the way an application provides the password for a policy.
///
/// SECURITY MODEL — stated plainly because this stores an unlock credential:
///
///   * The credential lives in the Keychain, not in a file or in this
///     process's memory beyond the moment of a submission. It is stored with
///     `kSecAttrAccessibleWhenUnlockedThisDeviceOnly`, so it is readable only
///     while the user's keychain is unlocked and never leaves the device or
///     syncs.
///   * It is retrieved only inside `submit`, only when the controller has an
///     armed Locked Use turn that already passed grant verification and the
///     idle/shield safeguards. There is no op that returns the credential; the
///     socket cannot read it back.
///   * Binding the Keychain item's ACL to this helper's code signature (so no
///     other process of the same user can read it) is the required hardening
///     and is enforced by the installer that provisions the item, documented
///     in docs/locked-unlock-investigation.md. This type never widens that.
///
/// If no credential is provisioned, `submit` reports that plainly and the
/// unlock simply does not happen — the safe direction, identical to today.
public enum UnlockCredential {
    private static let service = "com.psyche08.remote-agent.locked-use.unlock"
    private static let account = "unlock-credential"

    public struct CredentialError: Error, CustomStringConvertible {
        public let message: String
        public var description: String { message }
        init(_ message: String) { self.message = message }
    }

    /// Whether an unlock credential is provisioned. Reports presence only —
    /// never the value.
    public static func isProvisioned() -> Bool {
        var query = baseQuery()
        query[kSecReturnData as String] = false
        query[kSecMatchLimit as String] = kSecMatchLimitOne
        return SecItemCopyMatching(query as CFDictionary, nil) == errSecSuccess
    }

    private static func baseQuery() -> [String: Any] {
        [
            kSecClass as String: kSecClassGenericPassword,
            kSecAttrService as String: service,
            kSecAttrAccount as String: account,
        ]
    }

    /// Attempts a real system unlock by submitting the provisioned credential
    /// to a device-owner authentication policy.
    ///
    /// Returns only success or a reason; the credential never appears in the
    /// result. The credential bytes are zeroed after use.
    public static func submit(reason: String) throws {
        var query = baseQuery()
        query[kSecReturnData as String] = true
        query[kSecMatchLimit as String] = kSecMatchLimitOne
        var item: CFTypeRef?
        let status = SecItemCopyMatching(query as CFDictionary, &item)
        guard status == errSecSuccess, var data = item as? Data else {
            throw CredentialError(
                "no unlock credential is provisioned (SecItem status \(status))")
        }
        defer { data.resetBytes(in: 0..<data.count) }
        guard let password = String(data: data, encoding: .utf8) else {
            throw CredentialError("the provisioned credential is not valid UTF-8")
        }

        let context = LAContext()
        context.setCredential(
            Data(password.utf8), type: LACredentialType.applicationPassword)

        let semaphore = DispatchSemaphore(value: 0)
        var evaluateError: Error?
        var ok = false
        context.evaluatePolicy(
            .deviceOwnerAuthentication, localizedReason: reason
        ) { success, error in
            ok = success
            evaluateError = error
            semaphore.signal()
        }
        // Bounded: this runs off the request path, but must not hang a turn.
        if semaphore.wait(timeout: .now() + 20) == .timedOut {
            throw CredentialError("credential submission timed out")
        }
        if let evaluateError {
            throw CredentialError("credential submission failed: \(evaluateError)")
        }
        guard ok else {
            throw CredentialError("credential was rejected")
        }
    }

    /// Provisions or replaces the unlock credential. Used only by the installer
    /// flow, on the device, with the user's explicit action — never over the
    /// socket. The item is device-only and non-syncing.
    public static func provision(password: String) throws {
        var attributes = baseQuery()
        attributes[kSecValueData as String] = Data(password.utf8)
        attributes[kSecAttrAccessible as String] =
            kSecAttrAccessibleWhenUnlockedThisDeviceOnly
        SecItemDelete(baseQuery() as CFDictionary)
        let status = SecItemAdd(attributes as CFDictionary, nil)
        guard status == errSecSuccess else {
            throw CredentialError("could not store the unlock credential (status \(status))")
        }
    }

    /// Removes the provisioned credential. Safe when none exists.
    public static func remove() {
        SecItemDelete(baseQuery() as CFDictionary)
    }
}
