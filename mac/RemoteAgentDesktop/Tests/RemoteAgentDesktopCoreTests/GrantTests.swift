import CryptoKit
import XCTest
@testable import RemoteAgentDesktopCore

final class GrantTests: XCTestCase {
    private func makeSigner(_ deviceID: String = "mac-1") throws -> GrantSigner {
        let path = (NSTemporaryDirectory() as NSString)
            .appendingPathComponent("ra-key-\(UUID().uuidString)")
        addTeardownBlock { unlink(path) }
        return try GrantSigner.loadOrCreate(path: path, deviceID: deviceID)
    }

    /// Signs a payload with the signer's own key, so a test grant is validly
    /// signed and fails only on the property under test.
    private func grant(_ signer: GrantSigner, payload: GrantPayload) throws -> Grant {
        let raw = try JSONEncoder().encode(payload)
        return Grant(
            payload: raw.base64EncodedString(),
            signature: try signer.sign(raw).base64EncodedString())
    }

    func testMintedGrantVerifies() throws {
        let signer = try makeSigner()
        let now = Date()
        let (grant, payload) = try signer.mint(turnID: "turn-1", ttl: 10, now: now)
        let verified = try GrantVerifier.verify(
            grant, publicKey: signer.publicKey, deviceID: "mac-1",
            now: now.addingTimeInterval(1))
        XCTAssertEqual(verified, payload)
        XCTAssertEqual(verified.turnID, "turn-1")
        XCTAssertEqual(signer.publicKey.count, GrantContract.publicKeyBytes)
    }

    func testMintClampsTTLToTheCeiling() throws {
        let signer = try makeSigner()
        let now = Date()
        let (_, payload) = try signer.mint(turnID: "turn-1", ttl: 3600, now: now)
        XCTAssertEqual(payload.expiresAt - payload.issuedAt, Int64(GrantContract.maxTTL))

        // An unspecified TTL takes the shortest useful life, not the ceiling: a
        // caller that failed to specify must not get the most permissive grant.
        let (_, unspecified) = try signer.mint(turnID: "turn-1", ttl: 0, now: now)
        XCTAssertEqual(unspecified.expiresAt - unspecified.issuedAt, Int64(GrantContract.minTTL))
    }

    func testRejectsForeignKeyAndTamperedPayload() throws {
        let signer = try makeSigner()
        let other = try makeSigner("mac-1")
        let now = Date()
        let (grant, _) = try signer.mint(turnID: "turn-1", ttl: 10, now: now)

        XCTAssertThrowsError(try GrantVerifier.verify(
            grant, publicKey: other.publicKey, deviceID: "mac-1", now: now))

        // A payload edited after signing must not verify — otherwise the
        // signature would cover nothing that matters.
        var raw = Data(base64Encoded: grant.payload)!
        raw[raw.count - 2] = raw[raw.count - 2] ^ 0x01
        let tampered = Grant(
            payload: raw.base64EncodedString(), signature: grant.signature)
        XCTAssertThrowsError(try GrantVerifier.verify(
            tampered, publicKey: signer.publicKey, deviceID: "mac-1", now: now))
    }

    /// The verifier enforces its own ceiling rather than trusting the grant. A
    /// grant declaring a long life is rejected outright, not clamped, so a
    /// mis-minted grant fails loudly instead of becoming a skeleton key.
    func testRejectsOverlongDeclaredTTL() throws {
        let signer = try makeSigner()
        let now = Date()
        let payload = GrantPayload(
            version: GrantContract.version, purpose: GrantContract.purpose,
            nonce: String(repeating: "ab", count: GrantContract.nonceBytes),
            deviceID: "mac-1", turnID: "turn-1",
            issuedAt: Int64(now.timeIntervalSince1970),
            expiresAt: Int64(now.addingTimeInterval(3600).timeIntervalSince1970))
        XCTAssertThrowsError(try GrantVerifier.verify(
            try grant(signer, payload: payload), publicKey: signer.publicKey,
            deviceID: "mac-1", now: now)) { error in
            XCTAssertEqual(error as? GrantError, GrantError.ttlTooLong)
        }
    }

    func testRejectsExpiredAndFutureGrants() throws {
        let signer = try makeSigner()
        let now = Date()
        let (grant, _) = try signer.mint(turnID: "turn-1", ttl: 5, now: now)

        XCTAssertThrowsError(try GrantVerifier.verify(
            grant, publicKey: signer.publicKey, deviceID: "mac-1",
            now: now.addingTimeInterval(60))) { error in
            XCTAssertEqual(error as? GrantError, GrantError.expired)
        }
        XCTAssertThrowsError(try GrantVerifier.verify(
            grant, publicKey: signer.publicKey, deviceID: "mac-1",
            now: now.addingTimeInterval(-3600))) { error in
            XCTAssertEqual(error as? GrantError, GrantError.notYetValid)
        }
    }

    func testRejectsWrongDeviceAndPurpose() throws {
        let signer = try makeSigner()
        let now = Date()
        let (minted, _) = try signer.mint(turnID: "turn-1", ttl: 10, now: now)
        XCTAssertThrowsError(try GrantVerifier.verify(
            minted, publicKey: signer.publicKey, deviceID: "mac-2", now: now)) { error in
            XCTAssertEqual(error as? GrantError, GrantError.wrongDevice)
        }

        let payload = GrantPayload(
            version: GrantContract.version, purpose: "something-else",
            nonce: String(repeating: "cd", count: GrantContract.nonceBytes),
            deviceID: "mac-1", turnID: "turn-1",
            issuedAt: Int64(now.timeIntervalSince1970),
            expiresAt: Int64(now.addingTimeInterval(5).timeIntervalSince1970))
        XCTAssertThrowsError(try GrantVerifier.verify(
            try grant(signer, payload: payload), publicKey: signer.publicKey,
            deviceID: "mac-1", now: now)) { error in
            XCTAssertEqual(error as? GrantError, GrantError.wrongPurpose)
        }
    }

    func testSigningKeySurvivesReload() throws {
        let path = (NSTemporaryDirectory() as NSString)
            .appendingPathComponent("ra-key-\(UUID().uuidString)")
        defer { unlink(path) }
        let first = try GrantSigner.loadOrCreate(path: path, deviceID: "mac-1")
        let second = try GrantSigner.loadOrCreate(path: path, deviceID: "mac-1")
        XCTAssertEqual(first.publicKeyBase64, second.publicKeyBase64)

        var mode = stat()
        XCTAssertEqual(stat(path, &mode), 0)
        XCTAssertEqual(mode.st_mode & 0o777, 0o600)
    }

    func testRejectsMalformedSigningKey() throws {
        let path = (NSTemporaryDirectory() as NSString)
            .appendingPathComponent("ra-key-\(UUID().uuidString)")
        defer { unlink(path) }
        try Data("not-a-key".utf8).write(to: URL(fileURLWithPath: path))
        XCTAssertThrowsError(try GrantSigner.loadOrCreate(path: path, deviceID: "mac-1")) { error in
            XCTAssertEqual(error as? GrantError, GrantError.malformedKey)
        }
    }

    /// A key file written by the Go signer must still load here, and must yield
    /// the same public key. A device that upgrades to this helper otherwise
    /// mints under a new key while the plug-in still holds the old public half
    /// — armed, and silently unable to unlock. The vector is a real key emitted
    /// by internal/computeruse's LoadOrCreateSigner.
    func testLoadsAKeyWrittenByTheGoSigner() throws {
        let goKey = """
            MIGHAgEAMBMGByqGSM49AgEGCCqGSM49AwEHBG0wawIBAQQg/LPtZ8qUcqHJudvOgnx2\
            zvqKUGrP30golfVK+tOfJXqhRANCAAT11HvaM6AZNQr3RVJggxRzdNtyib6+rlYMqcIf\
            jrA20gdHW9LhA8FNbe4PvS03mg7EgSTDPh5TXmlTXd6ZokHR
            """
        let goPublicKey = "BPXUe9ozoBk1CvdFUmCDFHN023KJvr6uVgypwh+OsDbSB0db0uEDwU1t7g+9LTeaDsSBJMM+HlNeaVNd3pmiQdE="

        let path = (NSTemporaryDirectory() as NSString)
            .appendingPathComponent("ra-gokey-\(UUID().uuidString)")
        defer { unlink(path) }
        try Data((goKey + "\n").utf8).write(to: URL(fileURLWithPath: path))

        let signer = try GrantSigner.loadOrCreate(path: path, deviceID: "mac-1")
        XCTAssertEqual(signer.publicKeyBase64, goPublicKey)
    }

    // MARK: - Nonce ledger

    func testNonceIsSingleUse() throws {
        let directory = NSTemporaryDirectory() + "ra-ledger-\(UUID().uuidString)"
        defer { try? FileManager.default.removeItem(atPath: directory) }
        let nonce = String(repeating: "0f", count: GrantContract.nonceBytes)
        let expires = Date().addingTimeInterval(10)

        XCTAssertTrue(try NonceLedger.consume(
            directory: directory, nonce: nonce, expiresAt: expires))
        XCTAssertFalse(try NonceLedger.consume(
            directory: directory, nonce: nonce, expiresAt: expires))
    }

    /// Single-use has to hold under concurrency, or replay is a race away.
    func testConcurrentConsumersElectExactlyOneWinner() throws {
        let directory = NSTemporaryDirectory() + "ra-ledger-\(UUID().uuidString)"
        defer { try? FileManager.default.removeItem(atPath: directory) }
        let nonce = String(repeating: "7a", count: GrantContract.nonceBytes)
        let expires = Date().addingTimeInterval(10)
        // The directory is created up front so the race is over the entry, not
        // over mkdir.
        try FileManager.default.createDirectory(
            atPath: directory, withIntermediateDirectories: true)

        let winners = NSMutableArray()
        let lock = NSLock()
        DispatchQueue.concurrentPerform(iterations: 32) { _ in
            if let won = try? NonceLedger.consume(
                directory: directory, nonce: nonce, expiresAt: expires), won {
                lock.lock()
                winners.add(true)
                lock.unlock()
            }
        }
        XCTAssertEqual(winners.count, 1)
    }

    func testRejectsMalformedNonces() {
        let directory = NSTemporaryDirectory() + "ra-ledger-\(UUID().uuidString)"
        defer { try? FileManager.default.removeItem(atPath: directory) }
        for bad in ["", "zz", String(repeating: "0", count: 31),
                    String(repeating: "g", count: 32), "../escape"] {
            XCTAssertThrowsError(try NonceLedger.consume(
                directory: directory, nonce: bad, expiresAt: Date()), bad)
        }
    }

    /// Pruning is strictly by expiry, never by count, so it can never forget a
    /// nonce that could still be replayed.
    func testPruneKeepsEntriesThatCouldStillBeReplayed() throws {
        let directory = NSTemporaryDirectory() + "ra-ledger-\(UUID().uuidString)"
        defer { try? FileManager.default.removeItem(atPath: directory) }
        let now = Date()
        let fresh = String(repeating: "11", count: GrantContract.nonceBytes)
        let stale = String(repeating: "22", count: GrantContract.nonceBytes)
        try NonceLedger.consume(
            directory: directory, nonce: fresh, expiresAt: now.addingTimeInterval(5))
        try NonceLedger.consume(
            directory: directory, nonce: stale, expiresAt: now.addingTimeInterval(-3600))

        NonceLedger.prune(directory: directory, now: now)

        let remaining = try FileManager.default.contentsOfDirectory(atPath: directory)
        XCTAssertTrue(remaining.contains(fresh))
        XCTAssertFalse(remaining.contains(stale))
    }

    // MARK: - Grant files

    func testPublishAndWithdraw() throws {
        let directory = NSTemporaryDirectory() + "ra-grants-\(UUID().uuidString)"
        defer { try? FileManager.default.removeItem(atPath: directory) }
        let signer = try makeSigner()
        let (minted, _) = try signer.mint(turnID: "turn-1", ttl: 5, now: Date())

        try GrantStore.write(minted, to: directory)
        let path = (directory as NSString).appendingPathComponent(GrantContract.fileName)
        XCTAssertTrue(FileManager.default.fileExists(atPath: path))

        let onDisk = try JSONDecoder().decode(
            Grant.self, from: Data(contentsOf: URL(fileURLWithPath: path)))
        XCTAssertEqual(onDisk, minted)

        GrantStore.remove(from: directory)
        XCTAssertFalse(FileManager.default.fileExists(atPath: path))
        // Withdrawing twice is a normal path: every close calls it, including
        // ones that never published.
        GrantStore.remove(from: directory)
    }

    func testScrubRemovesAStagedGrantACrashLeftBehind() throws {
        let directory = NSTemporaryDirectory() + "ra-grants-\(UUID().uuidString)"
        defer { try? FileManager.default.removeItem(atPath: directory) }
        try FileManager.default.createDirectory(
            atPath: directory, withIntermediateDirectories: true)
        let staged = (directory as NSString)
            .appendingPathComponent(GrantContract.fileName + ".tmp")
        try Data("{}".utf8).write(to: URL(fileURLWithPath: staged))

        GrantStore.scrub(directory: directory)
        XCTAssertFalse(FileManager.default.fileExists(atPath: staged))
    }

    // MARK: - Cross-language vector

    /// Writes a grant minted by the real signer so mac/preflight.sh can run it
    /// through the plug-in's own verifier. The two verifiers live in different
    /// languages and cannot be tested together here; nothing else can tell
    /// "both sides agree" apart from "each is self-consistent and rejects the
    /// other", which fails silently as a Mac that never unlocks.
    ///
    /// Four lines: public key, payload, signature, and the public key of a
    /// different signer that must NOT verify — without a case the verifier has
    /// to refuse, one that always allowed would pass.
    func testInteropVector() throws {
        let signer = try makeSigner("mac-interop")
        let other = try makeSigner("mac-interop")
        let now = Date()
        let (minted, _) = try signer.mint(turnID: "turn-interop", ttl: 10, now: now)

        XCTAssertNoThrow(try GrantVerifier.verify(
            minted, publicKey: signer.publicKey, deviceID: "mac-interop", now: now))
        XCTAssertThrowsError(try GrantVerifier.verify(
            minted, publicKey: other.publicKey, deviceID: "mac-interop", now: now))

        guard let out = ProcessInfo.processInfo.environment["RA_INTEROP_VECTOR_OUT"] else {
            return
        }
        let lines = [
            signer.publicKeyBase64, minted.payload, minted.signature, other.publicKeyBase64,
        ].joined(separator: "\n") + "\n"
        try Data(lines.utf8).write(to: URL(fileURLWithPath: out))
    }
}
