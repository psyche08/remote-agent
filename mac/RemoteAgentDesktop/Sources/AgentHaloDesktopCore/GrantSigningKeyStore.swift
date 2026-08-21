import CryptoKit
import Foundation
import Security

/// File-based login Keychain storage for the grant-signing key.
///
/// A standalone Developer ID command-line executable cannot claim a data-
/// protection Keychain access group without an embedded provisioning profile;
/// taskgated rejects that restricted entitlement before main() runs. The macOS
/// file-based Keychain needs no entitlement and gives a newly-created item a
/// default ACL that trusts only its creator, tracked by the creator's code-
/// signing designated requirement. Thus a same-uid unrelated process cannot
/// read the private key, while a properly signed update with the same DR can.
///
/// The helper loads the key while its user session is active and retains the
/// P-256 key in memory for Locked Use. If the login Keychain is manually or
/// policy-locked, loading fails closed; this code never prompts, deletes, or
/// rotates the item in response to an authentication failure.
enum GrantSigningKeyStore {
    private static let account = "p256-signing-key-v1"
    private static let service = "dev.linsheng.agenthalo.locked-use.grant-signing"

    private static func requireFinalHelperIdentity() throws {
        // Provisioning must be performed by the final Developer ID helper. The
        // default file-Keychain ACL records this process's DR at creation time;
        // letting an ad-hoc/test binary create the item would permanently trust
        // the wrong creator and make the release helper unable to read it.
        _ = try PeerCodeSigning.makePeerRequirement(
            expectedPeerIdentifier: SocketServer.Configuration.agentSigningIdentifier)
    }

    static func loadExisting() throws -> P256.Signing.PrivateKey {
        try requireFinalHelperIdentity()
        guard let current = try load() else {
            throw GrantError(
                "the grant signing key is not provisioned or is unavailable without Keychain interaction")
        }
        return current
    }

    static func loadOrCreate(deviceID: String) throws -> P256.Signing.PrivateKey {
        try requireFinalHelperIdentity()

        if let current = try load() {
            return current
        }

        let key = P256.Signing.PrivateKey()
        let stored = try store(key, deviceID: deviceID)
        return stored
    }

    private static func load() throws -> P256.Signing.PrivateKey? {
        let request = loadQuery()
        var value: CFTypeRef?
        let status = SecItemCopyMatching(request as CFDictionary, &value)
        if status == errSecItemNotFound { return nil }
        guard status == errSecSuccess, let data = value as? Data else {
            throw GrantError(
                "could not read the grant signing key from the login Keychain " +
                "without user interaction (status \(status))")
        }
        guard let key = try? P256.Signing.PrivateKey(derRepresentation: data) else {
            throw GrantError("the Keychain grant signing key is malformed")
        }
        return key
    }

    /// Internal policy seam for tests. Omitting all data-protection/access-group
    /// attributes is intentional: SecItem therefore targets the file-based
    /// Keychain, whose default creator ACL is the no-profile security boundary.
    static func itemQuery() -> [String: Any] {
        return [
            kSecClass as String: kSecClassGenericPassword,
            kSecAttrService as String: service,
            kSecAttrAccount as String: account,
        ]
    }

    static func loadQuery() -> [String: Any] {
        var request = itemQuery()
        request[kSecReturnData as String] = true
        request[kSecMatchLimit as String] = kSecMatchLimitOne
        // A resident GUI-session helper must never hang behind a Keychain UI
        // prompt, especially while the display is locked. Authentication being
        // unavailable is a hard failure, not permission to replace the key.
        request[kSecUseAuthenticationUI as String] = kSecUseAuthenticationUISkip
        return request
    }

    private static func store(
        _ key: P256.Signing.PrivateKey, deviceID: String
    ) throws -> P256.Signing.PrivateKey {
        var attributes = itemQuery()
        attributes[kSecValueData as String] = key.derRepresentation
        attributes[kSecAttrDescription as String] =
            "AgentHalo Locked Use grant key for \(deviceID)"
        // Do not set kSecAttrAccess. For a single creator, the file-based
        // Keychain's default ACL is both narrower and safer: it trusts only the
        // creating helper and automatically tracks its designated requirement.
        let status = SecItemAdd(attributes as CFDictionary, nil)
        if status == errSecDuplicateItem, let raced = try load() {
            guard raced.publicKey.x963Representation == key.publicKey.x963Representation else {
                throw GrantError("another signing key was provisioned concurrently")
            }
            return raced
        }
        guard status == errSecSuccess else {
            throw GrantError(
                "could not store the grant signing key in the login Keychain " +
                "(status \(status)); verify the Keychain is unlocked and this is the final signed helper")
        }
        guard let stored = try load(),
              stored.publicKey.x963Representation == key.publicKey.x963Representation
        else {
            throw GrantError("could not verify the grant signing key after storing it")
        }
        return stored
    }

}

extension GrantSigner {
    /// Production loader. AgentHalo has no plaintext-key or prior-product
    /// import path: a fresh installation creates and reads only its own login-
    /// Keychain item, protected by the final helper's creator DR ACL.
    public static func loadOrCreateSecure(deviceID: String) throws -> GrantSigner {
        let key = try GrantSigningKeyStore.loadOrCreate(deviceID: deviceID)
        return GrantSigner(key: key, deviceID: deviceID)
    }

    /// Runtime loader. Starting the resident helper never creates or rotates a
    /// key: missing, locked, or untrusted Keychain state leaves Locked Use
    /// disarmed until explicit provisioning succeeds in an unlocked session.
    public static func loadSecure(deviceID: String) throws -> GrantSigner {
        let key = try GrantSigningKeyStore.loadExisting()
        return GrantSigner(key: key, deviceID: deviceID)
    }
}
