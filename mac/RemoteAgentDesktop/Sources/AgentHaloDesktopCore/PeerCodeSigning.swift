import Darwin
import Foundation
import Security

/// Builds and evaluates the code-signing boundary on the helper socket.
/// Filesystem mode and uid checks are necessary but not sufficient: every app
/// launched by the logged-in account has the same uid. Locked Use accepts only
/// the release agent signed by the same Developer ID team as this helper.
enum PeerCodeSigning {
    static let helperIdentifier = "dev.linsheng.agenthalo.desktop"

    struct Identity: Equatable {
        let identifier: String
        let teamIdentifier: String
    }

    enum VerificationError: Error, CustomStringConvertible {
        case detail(String)

        var description: String {
            switch self {
            case .detail(let value): return value
            }
        }
    }

    /// Pure policy seam used by tests and by requirement construction.
    static func permits(
        helper: Identity, peer: Identity, expectedPeerIdentifier: String
    ) -> Bool {
        helper.identifier == helperIdentifier &&
            peer.identifier == expectedPeerIdentifier &&
            !helper.teamIdentifier.isEmpty &&
            helper.teamIdentifier == peer.teamIdentifier
    }

    /// Pins the peer to an exact signing identifier, the helper's Developer ID
    /// team, and Apple's signing anchor. An ad-hoc or differently signed helper
    /// cannot produce this requirement and therefore refuses to serve Locked
    /// Use rather than silently falling back to uid-only authorization.
    static func makePeerRequirement(
        expectedPeerIdentifier: String
    ) throws -> SecRequirement {
        guard expectedPeerIdentifier == SocketServer.Configuration.agentSigningIdentifier else {
            throw VerificationError.detail("unexpected agent signing identifier")
        }
        guard let selfCode = copySelfCode(),
              let helper = identity(of: selfCode),
              helper.identifier == helperIdentifier,
              validTeamIdentifier(helper.teamIdentifier)
        else {
            throw VerificationError.detail(
                "Locked Use requires a Developer ID-signed desktop helper with identifier \(helperIdentifier)")
        }

        // Verify the running helper against its own pinned identity before
        // trusting the team value extracted from its signature.
        let helperRequirement = try makeRequirement(
            identifier: helperIdentifier, teamIdentifier: helper.teamIdentifier)
        guard SecCodeCheckValidity(
            selfCode, SecCSFlags(rawValue: kSecCSStrictValidate), helperRequirement) == errSecSuccess
        else {
            throw VerificationError.detail("the desktop helper code signature is not valid")
        }
        return try makeRequirement(
            identifier: expectedPeerIdentifier, teamIdentifier: helper.teamIdentifier)
    }

    static func currentIdentity() -> Identity? {
        guard let code = copySelfCode() else { return nil }
        return identity(of: code)
    }

    static func peerSatisfies(fd: Int32, requirement: SecRequirement) -> Bool {
        var token = audit_token_t()
        var length = socklen_t(MemoryLayout<audit_token_t>.size)
        guard getsockopt(fd, SOL_LOCAL, LOCAL_PEERTOKEN, &token, &length) == 0,
              length == MemoryLayout<audit_token_t>.size
        else { return false }

        let tokenData = withUnsafeBytes(of: &token) { Data($0) }
        let attributes = [kSecGuestAttributeAudit as String: tokenData] as CFDictionary
        var peerCode: SecCode?
        guard SecCodeCopyGuestWithAttributes(
            nil, attributes, SecCSFlags(), &peerCode) == errSecSuccess,
              let peerCode
        else { return false }
        return SecCodeCheckValidity(
            peerCode, SecCSFlags(rawValue: kSecCSStrictValidate), requirement) == errSecSuccess
    }

    private static func copySelfCode() -> SecCode? {
        var code: SecCode?
        guard SecCodeCopySelf(SecCSFlags(), &code) == errSecSuccess else { return nil }
        return code
    }

    private static func identity(of code: SecCode) -> Identity? {
        var staticCode: SecStaticCode?
        guard SecCodeCopyStaticCode(code, SecCSFlags(), &staticCode) == errSecSuccess,
              let staticCode
        else { return nil }
        var information: CFDictionary?
        guard SecCodeCopySigningInformation(
            staticCode, SecCSFlags(rawValue: kSecCSSigningInformation), &information) ==
                errSecSuccess,
              let values = information as? [String: Any],
              let identifier = values[kSecCodeInfoIdentifier as String] as? String,
              let team = values[kSecCodeInfoTeamIdentifier as String] as? String
        else { return nil }
        return Identity(identifier: identifier, teamIdentifier: team)
    }

    private static func validTeamIdentifier(_ value: String) -> Bool {
        value.count == 10 && value.unicodeScalars.allSatisfy {
            CharacterSet.uppercaseLetters.contains($0) || CharacterSet.decimalDigits.contains($0)
        }
    }

    private static func makeRequirement(
        identifier: String, teamIdentifier: String
    ) throws -> SecRequirement {
        let text = "identifier \"\(identifier)\" and anchor apple generic " +
            "and certificate leaf[subject.OU] = \"\(teamIdentifier)\""
        var requirement: SecRequirement?
        let status = SecRequirementCreateWithString(
            text as CFString, SecCSFlags(), &requirement)
        guard status == errSecSuccess, let requirement else {
            throw VerificationError.detail(
                "could not compile the peer code-signing requirement (status \(status))")
        }
        return requirement
    }

}
