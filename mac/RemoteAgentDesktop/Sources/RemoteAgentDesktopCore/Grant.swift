import CryptoKit
import Darwin
import Foundation

/// A grant is the only thing this process ever hands the unlock flow. It is a
/// short-lived, single-use, signed assertion that says "an authorized turn is
/// asking to unlock right now" — and nothing else. Specifically:
///
///   * It never contains, derives from, or substitutes for the user's password.
///     The Authorization Plug-in that reads it does not learn a credential; it
///     only decides whether to allow the unlock right it was already asked
///     about.
///   * Its lifetime is measured in seconds and it is minted immediately before
///     an unlock attempt, not held open for the duration of a window. A grant
///     resting on disk is ambient authority any local process could ride, which
///     is exactly the general-purpose remote unlock this feature must not
///     become.
///   * The verifier independently enforces its own freshness ceiling. A grant
///     that declares a long life is rejected, not honored, so a single leaked
///     or mis-minted grant can never become a durable skeleton key.
///
/// These constants are duplicated in mac/authorization-plugin/
/// RemoteAgentLockedUse.m and compared by mac/preflight.sh. Drift here does not
/// fail loudly: the agent mints grants the plug-in rejects forever, and the
/// only symptom is a Mac that never unlocks.
public enum GrantContract {
    /// The wire version the plug-in and this package agree on. A verifier that
    /// does not recognise the version refuses the grant.
    public static let version = 1
    /// Scopes a grant to the screensaver-unlock right. A grant is not a general
    /// authorization token and must not verify for anything else.
    public static let purpose = "screensaver-unlock"
    /// The single file the plug-in reads. One name, one grant: there is no
    /// queue of pending authorizations to pick from.
    public static let fileName = "grant.json"
    /// The fallback when a caller supplies no usable TTL.
    public static let minTTL: TimeInterval = 2
    /// The hard ceiling on a grant's life. Both the minter and the verifier
    /// enforce it; the verifier never trusts the grant's own expiry beyond it.
    public static let maxTTL: TimeInterval = 15
    /// Tolerates small clock differences between minter and verifier without
    /// opening a meaningful replay window.
    public static let maxClockSkew: TimeInterval = 5
    /// The nonce is the single-use key in the verifier's consumed ledger.
    public static let nonceBytes = 16
    /// The published verifying key: the X9.63 uncompressed point of a P-256
    /// public key, which is the form SecKeyCreateWithData expects.
    public static let publicKeyBytes = 65
}

/// Grants are signed with ECDSA P-256 over SHA-256, and the choice is forced by
/// the verifier rather than preferred here.
///
/// Ed25519 is the better primitive and CryptoKit offers it, but the plug-in
/// must verify through Security.framework, and SecKey's Ed25519 constants are
/// SPI: exported by Security.tbd, declared in no public header. A mechanism
/// bundle that binds a private symbol stops loading the day Apple drops it —
/// and this bundle sits in the screensaver-unlock right, where a mechanism that
/// cannot load is the lockout direction the design forbids.
/// `P256.Signing` hashes with SHA-256 and its `derRepresentation` is the X9.62
/// DER that kSecKeyAlgorithmECDSASignatureMessageX962SHA256 verifies.
public struct GrantPayload: Codable, Equatable, Sendable {
    public let version: Int
    public let purpose: String
    public let nonce: String
    public let deviceID: String
    public let turnID: String
    public let issuedAt: Int64
    public let expiresAt: Int64

    enum CodingKeys: String, CodingKey {
        case version = "v"
        case purpose, nonce
        case deviceID = "device_id"
        case turnID = "turn_id"
        case issuedAt = "issued_at"
        case expiresAt = "expires_at"
    }
}

/// The on-disk envelope: an opaque base64 payload plus its detached signature.
/// The verifier signature-checks the payload bytes and then parses those same
/// bytes, never a separately-parsed copy.
public struct Grant: Codable, Equatable, Sendable {
    public let payload: String
    public let signature: String
}

public struct GrantError: Error, Equatable, CustomStringConvertible {
    public let message: String
    public var description: String { message }
    init(_ message: String) { self.message = message }

    public static let noSigningKey = GrantError("locked use has no signing key")
    public static let expired = GrantError("grant expired")
    public static let notYetValid = GrantError("grant is not yet valid")
    public static let ttlTooLong = GrantError("grant declares a lifetime beyond the permitted ceiling")
    public static let badSignature = GrantError("grant signature is not valid")
    public static let wrongPurpose = GrantError("grant purpose does not match")
    public static let wrongDevice = GrantError("grant device does not match")
    public static let badVersion = GrantError("unsupported grant version")
    public static let malformedKey = GrantError(
        "locked-use signing key is malformed or is not a P-256 key; remove the file to mint a fresh one")
}

/// Mints grants. It holds the P-256 private key and is the only thing in the
/// process that can produce a verifiable unlock assertion.
///
/// Threat-model note: file permissions keep the key from other users, not from
/// a process already running as this user. Binding the key to the Secure
/// Enclave with an ACL scoped to this binary's code signature is the stronger
/// design and is recorded as required hardening in
/// docs/computer-use-locked-user.md.
public final class GrantSigner: @unchecked Sendable {
    private let key: P256.Signing.PrivateKey
    private let deviceID: String

    init(key: P256.Signing.PrivateKey, deviceID: String) {
        self.key = key
        self.deviceID = deviceID
    }

    /// Reads the private key at `path`, creating one on first use. The key file
    /// is 0600 in a 0700 directory.
    public static func loadOrCreate(path: String, deviceID: String) throws -> GrantSigner {
        guard !path.isEmpty else { throw GrantError.noSigningKey }
        let directory = (path as NSString).deletingLastPathComponent
        try? FileManager.default.createDirectory(
            atPath: directory, withIntermediateDirectories: true,
            attributes: [.posixPermissions: 0o700])

        if let raw = FileManager.default.contents(atPath: path) {
            let text = String(decoding: raw, as: UTF8.self)
                .trimmingCharacters(in: .whitespacesAndNewlines)
            guard let der = Data(base64Encoded: text),
                  let key = try? P256.Signing.PrivateKey(derRepresentation: der) else {
                throw GrantError.malformedKey
            }
            return GrantSigner(key: key, deviceID: deviceID)
        }

        let key = P256.Signing.PrivateKey()
        let encoded = key.derRepresentation.base64EncodedString() + "\n"
        // O_EXCL so a concurrent starter cannot silently replace a key that
        // another process is already publishing a public half for.
        let fd = open(path, O_WRONLY | O_CREAT | O_EXCL, 0o600)
        if fd < 0 {
            if errno == EEXIST { return try loadOrCreate(path: path, deviceID: deviceID) }
            throw GrantError("could not create the signing key: \(String(cString: strerror(errno)))")
        }
        let bytes = Array(encoded.utf8)
        let wrote = bytes.withUnsafeBufferPointer { write(fd, $0.baseAddress, $0.count) }
        close(fd)
        guard wrote == bytes.count else {
            unlink(path)
            throw GrantError("could not write the signing key")
        }
        return GrantSigner(key: key, deviceID: deviceID)
    }

    /// The verifying half, as the X9.63 uncompressed point the plug-in hands to
    /// SecKeyCreateWithData. The private half never leaves this process.
    public var publicKey: Data { key.publicKey.x963Representation }

    public var publicKeyBase64: String { publicKey.base64EncodedString() }

    /// Produces a signed grant valid for `ttl`. Callers mint immediately before
    /// an unlock attempt; `ttl` is clamped to the ceiling because the verifier
    /// rejects anything longer regardless.
    public func mint(turnID: String, ttl: TimeInterval, now: Date) throws -> (Grant, GrantPayload) {
        // An unset or out-of-range TTL falls back to the shortest useful life,
        // not the ceiling: a caller that failed to specify one must not
        // silently get the most permissive grant this code can mint.
        var life = ttl
        if life <= 0 { life = GrantContract.minTTL }
        if life > GrantContract.maxTTL { life = GrantContract.maxTTL }

        var nonce = Data(count: GrantContract.nonceBytes)
        let generated = nonce.withUnsafeMutableBytes { buffer in
            SecRandomCopyBytes(kSecRandomDefault, buffer.count, buffer.baseAddress!)
        }
        guard generated == errSecSuccess else {
            throw GrantError("could not generate a grant nonce")
        }

        let payload = GrantPayload(
            version: GrantContract.version,
            purpose: GrantContract.purpose,
            nonce: nonce.map { String(format: "%02x", $0) }.joined(),
            deviceID: deviceID,
            turnID: turnID,
            issuedAt: Int64(now.timeIntervalSince1970),
            expiresAt: Int64(now.addingTimeInterval(life).timeIntervalSince1970))

        let raw = try JSONEncoder().encode(payload)
        return (
            Grant(
                payload: raw.base64EncodedString(),
                signature: try sign(raw).base64EncodedString()),
            payload
        )
    }

    /// The detached signature over exact payload bytes: DER ECDSA over SHA-256,
    /// which is what kSecKeyAlgorithmECDSASignatureMessageX962SHA256 verifies.
    ///
    /// Internal rather than private so tests can mint a validly signed but
    /// semantically invalid grant, and so the signing format lives in one place.
    func sign(_ payload: Data) throws -> Data {
        try key.signature(for: payload).derRepresentation
    }
}

/// The reference implementation of the check the Authorization Plug-in
/// performs. The plug-in is the enforcing copy; this one keeps the contract
/// honest and testable in `swift test`, where the ObjC bundle cannot run.
///
/// This mirror is why deleting the Go one costs no coverage: the contract keeps
/// a testable implementation next to the code that mints against it, and
/// mac/preflight.sh still runs real minted bytes through the enforcing verifier.
///
/// Verification deliberately does not trust the grant about its own freshness:
/// a declared lifetime longer than the ceiling is rejected outright rather than
/// clamped-and-accepted, so a mis-minted grant fails loudly instead of quietly
/// becoming long-lived.
public enum GrantVerifier {
    public static func verify(
        _ grant: Grant, publicKey: Data, deviceID: String, now: Date
    ) throws -> GrantPayload {
        guard let raw = Data(base64Encoded: grant.payload) else {
            throw GrantError("grant payload is malformed")
        }
        guard let signature = Data(base64Encoded: grant.signature) else {
            throw GrantError("grant signature is malformed")
        }
        guard verifySignature(publicKey: publicKey, payload: raw, signature: signature) else {
            throw GrantError.badSignature
        }
        // Parse only the bytes the signature covered.
        guard let payload = try? JSONDecoder().decode(GrantPayload.self, from: raw) else {
            throw GrantError("grant payload is malformed")
        }
        guard payload.version == GrantContract.version else { throw GrantError.badVersion }
        guard payload.purpose == GrantContract.purpose else { throw GrantError.wrongPurpose }
        if !deviceID.isEmpty && payload.deviceID != deviceID { throw GrantError.wrongDevice }
        guard NonceLedger.isWellFormedNonce(payload.nonce) else {
            throw GrantError("grant nonce is malformed")
        }
        let issued = Date(timeIntervalSince1970: TimeInterval(payload.issuedAt))
        let expires = Date(timeIntervalSince1970: TimeInterval(payload.expiresAt))
        guard expires > issued else { throw GrantError.expired }
        guard expires.timeIntervalSince(issued) <= GrantContract.maxTTL else {
            throw GrantError.ttlTooLong
        }
        guard issued <= now.addingTimeInterval(GrantContract.maxClockSkew) else {
            throw GrantError.notYetValid
        }
        guard now <= expires else { throw GrantError.expired }
        return payload
    }

    /// Mirrors RAVerifySignature in the plug-in: an X9.63 uncompressed P-256
    /// point verifying a DER ECDSA signature over SHA-256 of the payload bytes.
    ///
    /// `x963Representation` validates the point, so this mirror refuses the
    /// keys the enforcing copy would refuse rather than accepting them and
    /// diverging.
    public static func verifySignature(
        publicKey: Data, payload: Data, signature: Data
    ) -> Bool {
        guard publicKey.count == GrantContract.publicKeyBytes,
              publicKey.first == 0x04,
              !payload.isEmpty, !signature.isEmpty,
              let key = try? P256.Signing.PublicKey(x963Representation: publicKey),
              let parsed = try? P256.Signing.ECDSASignature(derRepresentation: signature) else {
            return false
        }
        return key.isValidSignature(parsed, for: payload)
    }
}

public enum GrantStore {
    /// Publishes a grant for the Authorization Plug-in to read. The file is
    /// written to a temporary name and renamed, so the plug-in never observes a
    /// partially written grant it might parse as something else.
    public static func write(_ grant: Grant, to directory: String) throws {
        guard !directory.isEmpty else { throw GrantError("locked use has no grant directory") }
        try FileManager.default.createDirectory(
            atPath: directory, withIntermediateDirectories: true,
            attributes: [.posixPermissions: 0o700])
        let body = try JSONEncoder().encode(grant)
        let staging = (directory as NSString).appendingPathComponent(GrantContract.fileName + ".tmp")
        let final = (directory as NSString).appendingPathComponent(GrantContract.fileName)
        try body.write(to: URL(fileURLWithPath: staging), options: .atomic)
        try? FileManager.default.setAttributes([.posixPermissions: 0o600], ofItemAtPath: staging)
        do {
            _ = try FileManager.default.replaceItemAt(
                URL(fileURLWithPath: final), withItemAt: URL(fileURLWithPath: staging))
        } catch {
            // replaceItemAt requires the destination to exist on some paths;
            // rename is the primitive that matters and is atomic either way.
            guard rename(staging, final) == 0 else {
                unlink(staging)
                throw GrantError("could not publish the grant")
            }
        }
    }

    /// Withdraws any published grant. Safe when none exists, and invoked on
    /// every window-close path including failures — a grant outliving its
    /// window is precisely the ambient authority to avoid.
    public static func remove(from directory: String) {
        guard !directory.isEmpty else { return }
        unlink((directory as NSString).appendingPathComponent(GrantContract.fileName))
        // A crashed mint can leave the staging file behind; it is not a valid
        // grant, but sweeping it keeps the directory's meaning unambiguous.
        unlink((directory as NSString).appendingPathComponent(GrantContract.fileName + ".tmp"))
    }

    /// Removes every grant artifact. Runs at startup, before Locked Use arms,
    /// so a grant that survived a crash can never be honored by a later unlock.
    public static func scrub(directory: String) {
        remove(from: directory)
    }
}

/// This process's mirror of the plug-in's consumed-nonce ledger. The plug-in's
/// copy is the enforcing one and lives in a root-owned directory.
public enum NonceLedger {
    /// Records a nonce as used, returning false if it was already consumed.
    ///
    /// The create is O_EXCL so two concurrent verifiers cannot both believe
    /// they won. A failed write throws, and the caller must deny rather than
    /// proceed: an unrecordable consumption is not a permitted unlock.
    @discardableResult
    public static func consume(
        directory: String, nonce: String, expiresAt: Date
    ) throws -> Bool {
        guard !directory.isEmpty else { throw GrantError("no nonce ledger directory") }
        guard isWellFormedNonce(nonce) else { throw GrantError("grant nonce is malformed") }
        try FileManager.default.createDirectory(
            atPath: directory, withIntermediateDirectories: true,
            attributes: [.posixPermissions: 0o700])
        let path = (directory as NSString).appendingPathComponent(nonce)
        let fd = open(path, O_WRONLY | O_CREAT | O_EXCL, 0o600)
        if fd < 0 {
            if errno == EEXIST { return false }
            throw GrantError("could not record the nonce: \(String(cString: strerror(errno)))")
        }
        // The entry records only when it may be pruned. It carries no grant
        // body, no key material, and nothing derived from a credential.
        let line = Array("\(Int64(expiresAt.timeIntervalSince1970))\n".utf8)
        let wrote = line.withUnsafeBufferPointer { write(fd, $0.baseAddress, $0.count) }
        close(fd)
        guard wrote == line.count else {
            unlink(path)
            throw GrantError("could not record the nonce")
        }
        return true
    }

    /// Drops entries whose grants can no longer be valid. Entries are removed
    /// strictly by expiry, never by count, so pruning can never forget a nonce
    /// that could still be replayed.
    public static func prune(directory: String, now: Date) {
        guard let names = try? FileManager.default.contentsOfDirectory(atPath: directory) else {
            return
        }
        for name in names {
            let path = (directory as NSString).appendingPathComponent(name)
            guard let raw = FileManager.default.contents(atPath: path),
                  let expires = Int64(
                    String(decoding: raw, as: UTF8.self)
                        .trimmingCharacters(in: .whitespacesAndNewlines)) else {
                continue
            }
            // Keep entries well past expiry so clock jitter cannot resurrect a
            // nonce that is still inside a verifier's acceptance window.
            let safeUntil = Date(timeIntervalSince1970: TimeInterval(expires))
                .addingTimeInterval(GrantContract.maxTTL + GrantContract.maxClockSkew)
            if now > safeUntil { unlink(path) }
        }
    }

    static func isWellFormedNonce(_ nonce: String) -> Bool {
        guard nonce.count == GrantContract.nonceBytes * 2 else { return false }
        return nonce.allSatisfy { $0.isHexDigit && ($0.isNumber || $0.isLowercase) }
    }
}
