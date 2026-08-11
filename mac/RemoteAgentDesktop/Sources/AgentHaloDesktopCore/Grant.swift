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
/// AgentHaloLockedUse.m and compared by mac/preflight.sh. Drift here does not
/// fail loudly: the agent mints grants the plug-in rejects forever, and the
/// only symptom is a Mac that never unlocks.
public enum GrantContract {
    /// The wire version the plug-in and this package agree on. A verifier that
    /// does not recognise the version refuses the grant.
    public static let version = 2
    /// Scopes a grant to the screensaver-unlock right. A grant is not a general
    /// authorization token and must not verify for anything else.
    public static let purpose = "screensaver-unlock"
    /// The single file the plug-in reads. One name, one grant: there is no
    /// queue of pending authorizations to pick from.
    public static let fileName = "grant.json"
    /// Root-owned acknowledgement written by the Authorization Plug-in after
    /// it has verified and consumed the grant. The controller requires the
    /// exact nonce here as well as an unlocked screen before it opens a window.
    public static let receiptFileName = "receipt"
    /// Durable proof written before the plug-in submits Allow. If the final
    /// receipt never arrives, this exact nonce tells cleanup an authorization
    /// transition may still land and therefore requires quarantine.
    public static let pendingReceiptFileName = "receipt.pending"
    /// Durable terminal proof written only when the successful Authorization
    /// mechanism instance is destroyed. Unlike the final receipt, this says
    /// the nonce-bearing engine transaction reached its lifecycle boundary;
    /// the controller still requires the exact field lifecycle and unlocked
    /// state before opening because Apple does not document this as a visible
    /// loginwindow side-effect acknowledgement.
    public static let completionReceiptFileName = "receipt.complete"
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
    /// The primary console identity at the instant the helper minted this
    /// grant. The privileged verifier matches both claims against the username
    /// in this authorization transaction and resolves that name back to its
    /// uid. A grant from a Fast User Switching session therefore cannot be
    /// consumed by another user's loginwindow flow.
    public let consoleUID: UInt32
    public let consoleUsername: String
    public let issuedAt: Int64
    public let expiresAt: Int64

    enum CodingKeys: String, CodingKey {
        case version = "v"
        case purpose, nonce
        case deviceID = "device_id"
        case turnID = "turn_id"
        case consoleUID = "console_uid"
        case consoleUsername = "console_username"
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
    public static let wrongUser = GrantError("grant console user does not match")
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
    public func mint(
        turnID: String, ttl: TimeInterval, now: Date,
        consoleUID: uid_t = getuid(), consoleUsername: String = NSUserName()
    ) throws -> (Grant, GrantPayload) {
        // An unset or out-of-range TTL falls back to the shortest useful life,
        // not the ceiling: a caller that failed to specify one must not
        // silently get the most permissive grant this code can mint.
        var life = ttl
        if life <= 0 { life = GrantContract.minTTL }
        if life > GrantContract.maxTTL { life = GrantContract.maxTTL }
        guard consoleUID > 0,
              !consoleUsername.isEmpty,
              consoleUsername.utf8.count <= 256,
              !consoleUsername.utf8.contains(0) else {
            throw GrantError.wrongUser
        }

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
            consoleUID: UInt32(consoleUID),
            consoleUsername: consoleUsername,
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
        _ grant: Grant, publicKey: Data, deviceID: String, now: Date,
        consoleUID: uid_t = getuid(), consoleUsername: String = NSUserName()
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
        guard !deviceID.isEmpty, payload.deviceID == deviceID else {
            throw GrantError.wrongDevice
        }
        guard payload.consoleUID == UInt32(consoleUID),
              payload.consoleUsername == consoleUsername else {
            throw GrantError.wrongUser
        }
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

/// Publishing a grant crosses a privilege boundary, and how it crosses is not
/// an implementation detail.
///
/// The plug-in runs as root and refuses any grant file that is not root-owned,
/// so it can trust what it reads. This process runs as the user and cannot
/// create a root-owned file, cannot write into the root-owned directory, and
/// cannot unlink from it either. Publishing by rename is therefore impossible:
/// the renamed file would be the user's.
///
/// So the installer pre-creates `grant.json` root-owned with a named-user write
/// ACL for the one helper account, and this writes it **in place**. The file's
/// ownership never changes, so the plug-in's check still holds without making
/// every account in the common `staff` group a grant writer.
///
/// Two consequences are deliberate:
///
///   * Withdrawing truncates to zero rather than unlinking, because unlinking
///     needs write permission on the directory, which the user does not have.
///     The plug-in already rejects an empty file, so an emptied grant is a
///     withdrawn grant. Truncation happens first and unlink is only attempted
///     afterwards as tidying — a withdrawal that depended on unlink could fail
///     and leave a live grant outliving its window.
///   * A reader can observe a partially written grant, which rename would have
///     prevented. That is not a security hole: a torn grant fails signature
///     verification and is refused. It is the cost of not adding a root helper,
///     and the reason the stronger design in the docs is a root daemon that
///     writes the file itself.
public enum GrantStore {
    /// Publishes a grant for the Authorization Plug-in to read.
    public static func write(_ grant: Grant, to directory: String) throws {
        guard !directory.isEmpty else { throw GrantError("locked use has no grant directory") }
        let path = (directory as NSString).appendingPathComponent(GrantContract.fileName)
        let body = try JSONEncoder().encode(grant)

        // Write in place when the file exists: the installer made it root-owned
        // and granted this exact helper account a write ACL, so this process can
        // fill it without owning or replacing it.
        let fd = open(path, O_WRONLY | O_TRUNC | O_NOFOLLOW)
        if fd >= 0 {
            defer { close(fd) }
            let wrote = body.withUnsafeBytes { raw in
                Darwin.write(fd, raw.baseAddress, raw.count)
            }
            guard wrote == body.count else {
                ftruncate(fd, 0)
                throw GrantError("could not publish the grant")
            }
            return
        }
        if errno != ENOENT {
            throw GrantError(
                "could not publish the grant: \(String(cString: strerror(errno)))")
        }

        // No pre-created file: a directory this process owns, which is how
        // tests and any non-root grant directory work. Rename keeps the
        // publish atomic where it can.
        try FileManager.default.createDirectory(
            atPath: directory, withIntermediateDirectories: true,
            attributes: [.posixPermissions: 0o700])
        let staging = (directory as NSString)
            .appendingPathComponent(GrantContract.fileName + ".tmp")
        try body.write(to: URL(fileURLWithPath: staging), options: .atomic)
        try? FileManager.default.setAttributes(
            [.posixPermissions: 0o600], ofItemAtPath: staging)
        guard rename(staging, path) == 0 else {
            unlink(staging)
            throw GrantError("could not publish the grant")
        }
    }

    /// Withdraws any published grant. Safe when none exists, and invoked on
    /// every window-close path including failures — a grant outliving its
    /// window is precisely the ambient authority to avoid.
    ///
    /// This operation is deliberately *observable*. The old implementation
    /// ignored both a failed truncate and a failed unlink, which let startup
    /// report an armed controller even though a live grant could still be on
    /// disk. A caller may only continue once the file is proven absent or empty.
    public static func remove(from directory: String) throws {
        try remove(from: directory, lockTimeout: 0.5)
    }

    /// Internal timeout seam keeps the fd-serialization boundary deterministic
    /// in tests. Production never waits indefinitely on a verifier/authd
    /// process: failure enters controller quarantine, whose loop can retry
    /// while the shield remains engaged.
    static func remove(from directory: String, lockTimeout: TimeInterval) throws {
        guard !directory.isEmpty else {
            throw GrantError("locked use has no grant directory")
        }
        let path = (directory as NSString).appendingPathComponent(GrantContract.fileName)
        // Emptying is the withdrawal that always works; the plug-in refuses a
        // zero-length grant. Unlink is attempted afterwards only to tidy up,
        // and its failure is not a failure to withdraw.
        var withdrawn = false
        var failureCode: Int32 = 0
        let fd = open(path, O_WRONLY | O_NOFOLLOW | O_CLOEXEC | O_NONBLOCK)
        if fd >= 0 {
            // Serializes withdrawal against the privileged verifier. The
            // plug-in holds LOCK_SH from its first grant read until pending,
            // Allow, and final receipt publication have all finished. Once
            // this exclusive lock is acquired, no unobserved authorization
            // can still be derived from the bytes being withdrawn.
            let started = DispatchTime.now().uptimeNanoseconds
            let timeoutNanoseconds = UInt64(
                max(0, lockTimeout) * 1_000_000_000)
            while flock(fd, LOCK_EX | LOCK_NB) != 0 {
                let code = errno
                guard code == EWOULDBLOCK || code == EAGAIN || code == EINTR else {
                    close(fd)
                    throw GrantError(
                        "could not serialize grant withdrawal: "
                            + String(cString: strerror(code)))
                }
                let elapsed = DispatchTime.now().uptimeNanoseconds &- started
                guard elapsed < timeoutNanoseconds else {
                    close(fd)
                    throw GrantError(
                        "timed out serializing grant withdrawal; authorization verifier may be unresponsive")
                }
                Thread.sleep(forTimeInterval: 0.01)
            }
            var info = stat()
            if fstat(fd, &info) == 0,
               (info.st_mode & S_IFMT) == S_IFREG,
               ftruncate(fd, 0) == 0,
               fstat(fd, &info) == 0,
               info.st_size == 0 {
                withdrawn = true
            } else {
                failureCode = errno == 0 ? EINVAL : errno
            }

            // Unlink while the exclusive lock is still held. A verifier that
            // starts afterwards either opens the new/pre-created empty handoff
            // or sees no file; one that started earlier had to release LOCK_SH
            // (and publish its proofs) before we reached this point.
            if unlink(path) == 0 || errno == ENOENT {
                withdrawn = true
            } else if !withdrawn {
                failureCode = errno
            }
            flock(fd, LOCK_UN)
            close(fd)
        } else if errno == ENOENT {
            withdrawn = true
        } else {
            // Do not turn an open failure into an unlink success. Without an
            // fd lock, a privileged verifier may already be reading the inode;
            // removing its pathname would not cancel that in-flight decision.
            throw GrantError(
                "could not open the grant for withdrawal: "
                    + String(cString: strerror(errno)))
        }

        guard withdrawn else {
            let detail = failureCode == 0 ? "unknown error" : String(cString: strerror(failureCode))
            throw GrantError("could not withdraw the grant: \(detail)")
        }

        // A crashed mint can leave the staging file behind; it is not a valid
        // grant, but sweeping it keeps the directory's meaning unambiguous.
        let staging = (directory as NSString)
            .appendingPathComponent(GrantContract.fileName + ".tmp")
        if unlink(staging) != 0, errno != ENOENT {
            throw GrantError(
                "could not remove the grant staging file: \(String(cString: strerror(errno)))")
        }
    }

    /// Removes every grant artifact. Runs at startup, before Locked Use arms,
    /// so a grant that survived a crash can never be honored by a later unlock.
    public static func scrub(directory: String) throws {
        try remove(from: directory)
    }

    /// Verifies the privileged plug-in's acknowledgement for one exact grant.
    ///
    /// An unlocked screen by itself is ambiguous: a person may have unlocked
    /// it, or a late transition from another request may have landed. The
    /// receipt binds that transition to the nonce this controller just minted.
    /// It is deliberately read through a file descriptor with `O_NOFOLLOW` and
    /// validated after opening, so a path swap or symlink cannot substitute
    /// attacker-controlled bytes. Production receipts are root-owned and may
    /// not be group/other-writable.
    public static func receiptMatches(nonce: String, directory: String) throws -> Bool {
        try nonceProofMatches(
            nonce: nonce, directory: directory,
            fileName: GrantContract.receiptFileName, requiredOwner: 0)
    }

    public static func pendingReceiptMatches(
        nonce: String, directory: String
    ) throws -> Bool {
        try nonceProofMatches(
            nonce: nonce, directory: directory,
            fileName: GrantContract.pendingReceiptFileName, requiredOwner: 0)
    }

    public static func completionReceiptMatches(
        nonce: String, directory: String
    ) throws -> Bool {
        try nonceProofMatches(
            nonce: nonce, directory: directory,
            fileName: GrantContract.completionReceiptFileName, requiredOwner: 0)
    }

    /// Internal owner seam for filesystem tests, which cannot create a
    /// root-owned fixture. Production callers cannot override the root check.
    static func receiptMatches(
        nonce: String, directory: String, requiredOwner: uid_t
    ) throws -> Bool {
        try nonceProofMatches(
            nonce: nonce, directory: directory,
            fileName: GrantContract.receiptFileName, requiredOwner: requiredOwner)
    }

    static func pendingReceiptMatches(
        nonce: String, directory: String, requiredOwner: uid_t
    ) throws -> Bool {
        try nonceProofMatches(
            nonce: nonce, directory: directory,
            fileName: GrantContract.pendingReceiptFileName, requiredOwner: requiredOwner)
    }

    static func completionReceiptMatches(
        nonce: String, directory: String, requiredOwner: uid_t
    ) throws -> Bool {
        try nonceProofMatches(
            nonce: nonce, directory: directory,
            fileName: GrantContract.completionReceiptFileName, requiredOwner: requiredOwner)
    }

    private static func nonceProofMatches(
        nonce: String, directory: String, fileName: String, requiredOwner: uid_t
    ) throws -> Bool {
        guard !directory.isEmpty else {
            throw GrantError("locked use has no grant directory")
        }
        guard NonceLedger.isWellFormedNonce(nonce) else {
            throw GrantError("grant nonce is malformed")
        }

        let path = (directory as NSString).appendingPathComponent(fileName)
        let fd = open(path, O_RDONLY | O_NOFOLLOW | O_CLOEXEC | O_NONBLOCK)
        if fd < 0 {
            if errno == ENOENT { return false }
            throw GrantError(
                "could not read the grant receipt: \(String(cString: strerror(errno)))")
        }
        defer { close(fd) }

        var info = stat()
        guard fstat(fd, &info) == 0 else {
            throw GrantError(
                "could not inspect the grant receipt: \(String(cString: strerror(errno)))")
        }
        guard (info.st_mode & S_IFMT) == S_IFREG,
              info.st_uid == requiredOwner,
              (info.st_mode & 0o022) == 0 else {
            throw GrantError("grant receipt ownership or permissions are unsafe")
        }

        let expected = Array(nonce.utf8)
        // Exact size is both the bound and part of exact-nonce matching. Do not
        // trim whitespace or accept a prefix: either would weaken the binding.
        guard info.st_size == off_t(expected.count) else { return false }
        var body = [UInt8](repeating: 0, count: expected.count)
        let bodyCount = body.count
        var offset = 0
        while offset < bodyCount {
            let remaining = bodyCount - offset
            let count = body.withUnsafeMutableBytes { raw -> Int in
                guard let base = raw.baseAddress else { return 0 }
                return Darwin.read(fd, base.advanced(by: offset), remaining)
            }
            if count < 0 {
                if errno == EINTR { continue }
                throw GrantError(
                    "could not read the grant receipt: \(String(cString: strerror(errno)))")
            }
            guard count > 0 else { return false }
            offset += count
        }
        return body == expected
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
