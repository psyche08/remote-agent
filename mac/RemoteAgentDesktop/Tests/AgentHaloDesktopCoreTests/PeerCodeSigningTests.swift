import XCTest
@testable import AgentHaloDesktopCore

final class PeerCodeSigningTests: XCTestCase {
    func testEnabledComputerUsePinsTheExactAgentSigningIdentifier() {
        let privileged = SocketServer.Configuration(
            path: "/tmp/agenthalo.sock", computerUseEnabled: true)
        XCTAssertEqual(
            privileged.requiredPeerSigningIdentifier,
            SocketServer.Configuration.agentSigningIdentifier)

        let desktopOnly = SocketServer.Configuration(
            path: "/tmp/agenthalo.sock", computerUseEnabled: false)
        XCTAssertNil(desktopOnly.requiredPeerSigningIdentifier)
    }

    private let helper = PeerCodeSigning.Identity(
        identifier: PeerCodeSigning.helperIdentifier,
        teamIdentifier: "89LGY6BD53")

    func testAcceptsOnlyExactAgentOnSameTeam() {
        let peer = PeerCodeSigning.Identity(
            identifier: "dev.linsheng.agenthalo",
            teamIdentifier: "89LGY6BD53")
        XCTAssertTrue(PeerCodeSigning.permits(
            helper: helper, peer: peer,
            expectedPeerIdentifier: SocketServer.Configuration.agentSigningIdentifier))
    }

    func testRejectsLegacyAgentIdentityAfterFreshReplacement() {
        let peer = PeerCodeSigning.Identity(
            identifier: "com.psyche08.remote-agent",
            teamIdentifier: "89LGY6BD53")
        XCTAssertFalse(PeerCodeSigning.permits(
            helper: helper, peer: peer,
            expectedPeerIdentifier: SocketServer.Configuration.agentSigningIdentifier))
    }

    func testRejectsAnotherSameUserApplication() {
        let peer = PeerCodeSigning.Identity(
            identifier: "com.example.other",
            teamIdentifier: "89LGY6BD53")
        XCTAssertFalse(PeerCodeSigning.permits(
            helper: helper, peer: peer,
            expectedPeerIdentifier: SocketServer.Configuration.agentSigningIdentifier))
    }

    func testRejectsSameIdentifierSignedByAnotherTeam() {
        let peer = PeerCodeSigning.Identity(
            identifier: "dev.linsheng.agenthalo",
            teamIdentifier: "ABCDEFGHIJ")
        XCTAssertFalse(PeerCodeSigning.permits(
            helper: helper, peer: peer,
            expectedPeerIdentifier: SocketServer.Configuration.agentSigningIdentifier))
    }

    func testRejectsHelperWithUnexpectedIdentity() {
        let replaced = PeerCodeSigning.Identity(
            identifier: "com.example.replacement",
            teamIdentifier: "89LGY6BD53")
        let peer = PeerCodeSigning.Identity(
            identifier: "dev.linsheng.agenthalo",
            teamIdentifier: "89LGY6BD53")
        XCTAssertFalse(PeerCodeSigning.permits(
            helper: replaced, peer: peer,
            expectedPeerIdentifier: SocketServer.Configuration.agentSigningIdentifier))
    }
}
