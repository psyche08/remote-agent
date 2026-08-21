import CryptoKit
import Security
import XCTest
@testable import AgentHaloDesktopCore

final class GrantTests: XCTestCase {
    func testStandaloneDeveloperIDKeychainUsesFileBasedCreatorACL() {
        let item = GrantSigningKeyStore.itemQuery()
        XCTAssertEqual(
            item[kSecClass as String] as! CFString,
            kSecClassGenericPassword)
        XCTAssertEqual(
            item[kSecAttrService as String] as? String,
            "dev.linsheng.agenthalo.locked-use.grant-signing")
        XCTAssertEqual(item[kSecAttrAccount as String] as? String, "p256-signing-key-v1")

        // Each of these selects the data-protection Keychain or a restricted
        // entitlement. A bare Developer ID Mach-O cannot use that model without
        // an embedded provisioning profile and would be killed by taskgated.
        XCTAssertNil(item[kSecUseDataProtectionKeychain as String])
        XCTAssertNil(item[kSecAttrAccessGroup as String])
        XCTAssertNil(item[kSecAttrAccessible as String])
        XCTAssertNil(item[kSecAttrAccessControl as String])
        XCTAssertNil(item[kSecAttrSynchronizable as String])

        // No custom trusted list: the file-Keychain default ACL trusts only the
        // creator and tracks the final helper's designated requirement.
        XCTAssertNil(item[kSecAttrAccess as String])
    }

    func testLockedScreenKeychainReadCannotRequestAuthenticationUI() {
        let query = GrantSigningKeyStore.loadQuery()
        XCTAssertEqual(
            query[kSecUseAuthenticationUI as String] as! CFString,
            kSecUseAuthenticationUISkip)
        XCTAssertEqual(query[kSecReturnData as String] as? Bool, true)
        XCTAssertEqual(
            query[kSecMatchLimit as String] as! CFString,
            kSecMatchLimitOne)
    }

    func testHelperBuildDoesNotClaimARestrictedKeychainEntitlement() throws {
        let root = URL(fileURLWithPath: #filePath)
            .deletingLastPathComponent()
            .deletingLastPathComponent()
            .deletingLastPathComponent()
        let build = try String(
            contentsOf: root.appendingPathComponent("build.sh"), encoding: .utf8)
        XCTAssertFalse(build.contains("--entitlements \"$ENTITLEMENTS\""))
        XCTAssertFalse(
            FileManager.default.fileExists(
                atPath: root.appendingPathComponent("agenthalo-desktop.entitlements").path))
    }

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
            consoleUID: UInt32(getuid()), consoleUsername: NSUserName(),
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
        XCTAssertThrowsError(try GrantVerifier.verify(
            minted, publicKey: signer.publicKey, deviceID: "", now: now)) { error in
            XCTAssertEqual(error as? GrantError, GrantError.wrongDevice)
        }

        let payload = GrantPayload(
            version: GrantContract.version, purpose: "something-else",
            nonce: String(repeating: "cd", count: GrantContract.nonceBytes),
            deviceID: "mac-1", turnID: "turn-1",
            consoleUID: UInt32(getuid()), consoleUsername: NSUserName(),
            issuedAt: Int64(now.timeIntervalSince1970),
            expiresAt: Int64(now.addingTimeInterval(5).timeIntervalSince1970))
        XCTAssertThrowsError(try GrantVerifier.verify(
            try grant(signer, payload: payload), publicKey: signer.publicKey,
            deviceID: "mac-1", now: now)) { error in
            XCTAssertEqual(error as? GrantError, GrantError.wrongPurpose)
        }
    }

    func testRejectsGrantForAnotherConsoleIdentity() throws {
        let signer = try makeSigner()
        let now = Date()
        let (minted, payload) = try signer.mint(
            turnID: "turn-user", ttl: 10, now: now,
            consoleUID: getuid(), consoleUsername: NSUserName())

        XCTAssertThrowsError(try GrantVerifier.verify(
            minted, publicKey: signer.publicKey, deviceID: "mac-1", now: now,
            consoleUID: getuid() &+ 1, consoleUsername: payload.consoleUsername)) { error in
            XCTAssertEqual(error as? GrantError, .wrongUser)
        }
        XCTAssertThrowsError(try GrantVerifier.verify(
            minted, publicKey: signer.publicKey, deviceID: "mac-1", now: now,
            consoleUID: getuid(), consoleUsername: payload.consoleUsername + "-other")) { error in
            XCTAssertEqual(error as? GrantError, .wrongUser)
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

        try GrantStore.remove(from: directory)
        XCTAssertFalse(FileManager.default.fileExists(atPath: path))
        // Withdrawing twice is a normal path: every close calls it, including
        // ones that never published.
        try GrantStore.remove(from: directory)
    }

    func testScrubRemovesAStagedGrantACrashLeftBehind() throws {
        let directory = NSTemporaryDirectory() + "ra-grants-\(UUID().uuidString)"
        defer { try? FileManager.default.removeItem(atPath: directory) }
        try FileManager.default.createDirectory(
            atPath: directory, withIntermediateDirectories: true)
        let staged = (directory as NSString)
            .appendingPathComponent(GrantContract.fileName + ".tmp")
        try Data("{}".utf8).write(to: URL(fileURLWithPath: staged))

        try GrantStore.scrub(directory: directory)
        XCTAssertFalse(FileManager.default.fileExists(atPath: staged))
    }

    // MARK: - Cross-language vector

    /// Writes a grant minted by the real signer so mac/preflight.sh can run it
    /// through the plug-in's own verifier. The two verifiers live in different
    /// languages and cannot be tested together here; nothing else can tell
    /// "both sides agree" apart from "each is self-consistent and rejects the
    /// other", which fails silently as a Mac that never unlocks.
    ///
    /// Five lines: public key, payload, signature, a different public key that
    /// must NOT verify, and the signed console username. The preflight harness
    /// also feeds that username and a wrong one through the plug-in's enforcing
    /// user-claim matcher.
    func testInteropVector() throws {
        let signer = try makeSigner("mac-interop")
        let other = try makeSigner("mac-interop")
        let now = Date()
        let (minted, payload) = try signer.mint(
            turnID: "turn-interop", ttl: 10, now: now)

        XCTAssertNoThrow(try GrantVerifier.verify(
            minted, publicKey: signer.publicKey, deviceID: "mac-interop", now: now))
        XCTAssertThrowsError(try GrantVerifier.verify(
            minted, publicKey: other.publicKey, deviceID: "mac-interop", now: now))

        guard let out = ProcessInfo.processInfo.environment["AGENTHALO_INTEROP_VECTOR_OUT"] else {
            return
        }
        let lines = [
            signer.publicKeyBase64, minted.payload, minted.signature,
            other.publicKeyBase64, payload.consoleUsername,
        ].joined(separator: "\n") + "\n"
        try Data(lines.utf8).write(to: URL(fileURLWithPath: out))
    }
}

/// Publishing crosses a privilege boundary: the plug-in refuses a grant file
/// that is not root-owned, and this process cannot create one. The installer
/// pre-creates the file root-owned with a named-user write ACL, and the helper
/// fills it in place — so publishing must never replace the file, and
/// withdrawing must never depend on unlinking it.
extension GrantTests {
    func testPendingReceiptUsesTheSameStrictNonceProofBoundary() throws {
        let directory = NSTemporaryDirectory() + "ra-pending-receipt-\(UUID().uuidString)"
        defer { try? FileManager.default.removeItem(atPath: directory) }
        try FileManager.default.createDirectory(
            atPath: directory, withIntermediateDirectories: true)
        let nonce = String(repeating: "12", count: GrantContract.nonceBytes)
        let path = (directory as NSString).appendingPathComponent(
            GrantContract.pendingReceiptFileName)
        try Data(nonce.utf8).write(to: URL(fileURLWithPath: path))
        try FileManager.default.setAttributes(
            [.posixPermissions: 0o400], ofItemAtPath: path)

        XCTAssertTrue(try GrantStore.pendingReceiptMatches(
            nonce: nonce, directory: directory, requiredOwner: getuid()))
        XCTAssertFalse(try GrantStore.pendingReceiptMatches(
            nonce: String(repeating: "34", count: GrantContract.nonceBytes),
            directory: directory, requiredOwner: getuid()))

        let replacement = (directory as NSString).appendingPathComponent("replacement")
        try Data(nonce.utf8).write(to: URL(fileURLWithPath: replacement))
        unlink(path)
        XCTAssertEqual(symlink(replacement, path), 0)
        XCTAssertThrowsError(try GrantStore.pendingReceiptMatches(
            nonce: nonce, directory: directory, requiredOwner: getuid()))
    }

    func testCompletionReceiptUsesTheSameStrictNonceProofBoundary() throws {
        let directory = NSTemporaryDirectory() + "ra-complete-receipt-\(UUID().uuidString)"
        defer { try? FileManager.default.removeItem(atPath: directory) }
        try FileManager.default.createDirectory(
            atPath: directory, withIntermediateDirectories: true)
        let nonce = String(repeating: "56", count: GrantContract.nonceBytes)
        let path = (directory as NSString).appendingPathComponent(
            GrantContract.completionReceiptFileName)
        try Data(nonce.utf8).write(to: URL(fileURLWithPath: path))
        try FileManager.default.setAttributes(
            [.posixPermissions: 0o400], ofItemAtPath: path)

        XCTAssertTrue(try GrantStore.completionReceiptMatches(
            nonce: nonce, directory: directory, requiredOwner: getuid()))
        XCTAssertFalse(try GrantStore.completionReceiptMatches(
            nonce: String(repeating: "78", count: GrantContract.nonceBytes),
            directory: directory, requiredOwner: getuid()))
    }

    func testPublishingFillsAPreCreatedFileInPlace() throws {
        let directory = NSTemporaryDirectory() + "ra-inplace-\(UUID().uuidString)"
        defer { try? FileManager.default.removeItem(atPath: directory) }
        try FileManager.default.createDirectory(
            atPath: directory, withIntermediateDirectories: true)
        let path = (directory as NSString).appendingPathComponent(GrantContract.fileName)
        // Stand in for the installer's file. Only the inode matters here: on a
        // real device it is root-owned, and a publish that replaced it would
        // hand the plug-in a file owned by this user, which it refuses.
        FileManager.default.createFile(atPath: path, contents: Data())
        let before = try FileManager.default.attributesOfItem(atPath: path)[.systemFileNumber] as? Int

        let signer = try makeSigner()
        let (minted, _) = try signer.mint(turnID: "turn-1", ttl: 5, now: Date())
        try GrantStore.write(minted, to: directory)

        let after = try FileManager.default.attributesOfItem(atPath: path)[.systemFileNumber] as? Int
        XCTAssertEqual(before, after, "publishing replaced the file instead of filling it")

        let onDisk = try JSONDecoder().decode(
            Grant.self, from: Data(contentsOf: URL(fileURLWithPath: path)))
        XCTAssertEqual(onDisk, minted)

        // A second publish must also stay in place, and must not leave the
        // previous grant's bytes behind it.
        let (second, _) = try signer.mint(turnID: "turn-2", ttl: 5, now: Date())
        try GrantStore.write(second, to: directory)
        let reread = try JSONDecoder().decode(
            Grant.self, from: Data(contentsOf: URL(fileURLWithPath: path)))
        XCTAssertEqual(reread, second, "a shorter grant left a tail of the previous one")
    }

    /// Withdrawal has to work without unlinking, because the directory belongs
    /// to root. An empty file is a withdrawn grant: the plug-in rejects a
    /// zero-length one.
    func testWithdrawalEmptiesAGrantItCannotUnlink() throws {
        let directory = NSTemporaryDirectory() + "ra-withdraw-\(UUID().uuidString)"
        defer {
            // Restore write permission so the temp tree can be cleaned up.
            try? FileManager.default.setAttributes(
                [.posixPermissions: 0o700], ofItemAtPath: directory)
            try? FileManager.default.removeItem(atPath: directory)
        }
        try FileManager.default.createDirectory(
            atPath: directory, withIntermediateDirectories: true)
        let path = (directory as NSString).appendingPathComponent(GrantContract.fileName)
        FileManager.default.createFile(atPath: path, contents: Data())

        let signer = try makeSigner()
        let (minted, _) = try signer.mint(turnID: "turn-1", ttl: 5, now: Date())
        try GrantStore.write(minted, to: directory)

        // Take away the directory write permission, which is what root
        // ownership means for this process: unlink cannot work.
        try FileManager.default.setAttributes(
            [.posixPermissions: 0o500], ofItemAtPath: directory)

        try GrantStore.remove(from: directory)

        XCTAssertTrue(
            FileManager.default.fileExists(atPath: path),
            "the file could not be unlinked, which is the point of this case")
        let size = try FileManager.default.attributesOfItem(atPath: path)[.size] as? Int
        XCTAssertEqual(size, 0, "a grant survived withdrawal in a directory we cannot unlink from")
    }

    func testWithdrawalFailsWhenNeitherTruncateNorUnlinkCanBeProven() throws {
        let directory = NSTemporaryDirectory() + "ra-withdraw-fail-\(UUID().uuidString)"
        let path = (directory as NSString).appendingPathComponent(GrantContract.fileName)
        defer {
            try? FileManager.default.setAttributes(
                [.posixPermissions: 0o700], ofItemAtPath: directory)
            try? FileManager.default.setAttributes(
                [.posixPermissions: 0o600], ofItemAtPath: path)
            try? FileManager.default.removeItem(atPath: directory)
        }
        try FileManager.default.createDirectory(
            atPath: directory, withIntermediateDirectories: true)
        try Data("still-live".utf8).write(to: URL(fileURLWithPath: path))
        try FileManager.default.setAttributes(
            [.posixPermissions: 0o400], ofItemAtPath: path)
        try FileManager.default.setAttributes(
            [.posixPermissions: 0o500], ofItemAtPath: directory)

        XCTAssertThrowsError(try GrantStore.remove(from: directory))
        XCTAssertEqual(
            try Data(contentsOf: URL(fileURLWithPath: path)),
            Data("still-live".utf8),
            "a failed withdrawal must not be reported as success")
    }

    func testWithdrawalNeverBlocksIndefinitelyOnVerifierLock() throws {
        let directory = NSTemporaryDirectory() + "ra-withdraw-lock-\(UUID().uuidString)"
        defer { try? FileManager.default.removeItem(atPath: directory) }
        try FileManager.default.createDirectory(
            atPath: directory, withIntermediateDirectories: true)
        let path = (directory as NSString).appendingPathComponent(GrantContract.fileName)
        try Data("still-live".utf8).write(to: URL(fileURLWithPath: path))

        let verifierFD = open(path, O_RDONLY | O_NOFOLLOW | O_CLOEXEC)
        XCTAssertGreaterThanOrEqual(verifierFD, 0)
        defer {
            flock(verifierFD, LOCK_UN)
            close(verifierFD)
        }
        XCTAssertEqual(flock(verifierFD, LOCK_SH), 0)

        let started = DispatchTime.now().uptimeNanoseconds
        XCTAssertThrowsError(
            try GrantStore.remove(from: directory, lockTimeout: 0.05))
        XCTAssertLessThan(
            Double(DispatchTime.now().uptimeNanoseconds &- started) / 1_000_000_000,
            0.5,
            "grant withdrawal blocked instead of escalating to quarantine")
        XCTAssertEqual(
            try Data(contentsOf: URL(fileURLWithPath: path)), Data("still-live".utf8))
    }
}

/// The receipt is the controller's evidence that the privileged plug-in
/// consumed the exact nonce whose lock transition it is observing.
extension GrantTests {
    func testReceiptRequiresAnExactNonceAndSafeMode() throws {
        let directory = NSTemporaryDirectory() + "ra-receipt-\(UUID().uuidString)"
        defer { try? FileManager.default.removeItem(atPath: directory) }
        try FileManager.default.createDirectory(
            atPath: directory, withIntermediateDirectories: true)
        let path = (directory as NSString).appendingPathComponent(
            GrantContract.receiptFileName)
        let nonce = String(repeating: "ab", count: GrantContract.nonceBytes)
        try Data(nonce.utf8).write(to: URL(fileURLWithPath: path))
        try FileManager.default.setAttributes(
            [.posixPermissions: 0o400], ofItemAtPath: path)

        XCTAssertTrue(try GrantStore.receiptMatches(
            nonce: nonce, directory: directory, requiredOwner: getuid()))
        XCTAssertFalse(try GrantStore.receiptMatches(
            nonce: String(repeating: "cd", count: GrantContract.nonceBytes),
            directory: directory, requiredOwner: getuid()))

        try FileManager.default.setAttributes(
            [.posixPermissions: 0o620], ofItemAtPath: path)
        XCTAssertThrowsError(try GrantStore.receiptMatches(
            nonce: nonce, directory: directory, requiredOwner: getuid()))
    }

    func testReceiptRefusesSymlinksAndNonRootProductionOwners() throws {
        let directory = NSTemporaryDirectory() + "ra-receipt-link-\(UUID().uuidString)"
        defer { try? FileManager.default.removeItem(atPath: directory) }
        try FileManager.default.createDirectory(
            atPath: directory, withIntermediateDirectories: true)
        let nonce = String(repeating: "ef", count: GrantContract.nonceBytes)
        let target = (directory as NSString).appendingPathComponent("target")
        let receipt = (directory as NSString).appendingPathComponent(
            GrantContract.receiptFileName)
        try Data(nonce.utf8).write(to: URL(fileURLWithPath: target))
        XCTAssertEqual(symlink(target, receipt), 0)

        XCTAssertThrowsError(try GrantStore.receiptMatches(
            nonce: nonce, directory: directory, requiredOwner: getuid()))

        if getuid() != 0 {
            unlink(receipt)
            XCTAssertEqual(rename(target, receipt), 0)
            try FileManager.default.setAttributes(
                [.posixPermissions: 0o400], ofItemAtPath: receipt)
            XCTAssertThrowsError(try GrantStore.receiptMatches(
                nonce: nonce, directory: directory))
        }
    }
}
