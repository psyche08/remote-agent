import Foundation
import IOKit.pwr_mgt

/// Injectable public-power-management boundary. Production uses only Apple's
/// documented user-activity assertion APIs; tests replace these two synchronous
/// calls without waking a real display.
struct RemoteUserActivityPowerAPI: @unchecked Sendable {
    typealias Declare = @Sendable (
        _ name: String, _ userType: IOPMUserActiveType,
        _ assertionID: inout IOPMAssertionID
    ) -> IOReturn
    typealias Release = @Sendable (_ assertionID: IOPMAssertionID) -> IOReturn

    let declare: Declare
    let release: Release

    static let system = RemoteUserActivityPowerAPI(
        declare: { name, userType, assertionID in
            IOPMAssertionDeclareUserActivity(
                name as CFString, userType, &assertionID)
        },
        release: { assertionID in
            IOPMAssertionRelease(assertionID)
        })
}

struct RemoteUserActivityError: Error, Equatable, CustomStringConvertible {
    let detail: String
    var description: String { detail }
}

/// A one-shot, thread-safe lease for the remote user-activity assertion.
///
/// Every caller may put `release()` in its own cleanup path. Concurrent and
/// repeated calls wait for, then replay, the first release result; the system
/// assertion is released at most once even when the primary path and a `defer`
/// race. A failed release is terminal too, so retry cannot accidentally let a
/// later grant proceed after an ambiguous assertion lifecycle.
final class RemoteUserActivityLease: @unchecked Sendable {
    static let assertionName = "AgentHalo Locked Use remote activity"

    private enum State {
        case active
        case releasing
        case released
        case releaseFailed(RemoteUserActivityError)
    }

    private let condition = NSCondition()
    private let assertionID: IOPMAssertionID
    private let releaseAssertion: RemoteUserActivityPowerAPI.Release
    private var state: State = .active

    private init(
        assertionID: IOPMAssertionID,
        releaseAssertion: @escaping RemoteUserActivityPowerAPI.Release
    ) {
        self.assertionID = assertionID
        self.releaseAssertion = releaseAssertion
    }

    static func declare(
        using api: RemoteUserActivityPowerAPI
    ) throws -> RemoteUserActivityLease {
        // IOPMLib's public contract requires a non-null name no longer than
        // 128 characters. Keep our fixed ASCII name strictly below that bound.
        guard !assertionName.isEmpty, assertionName.count < 128 else {
            throw RemoteUserActivityError(
                detail: "remote user-activity assertion name is invalid")
        }
        var assertionID = IOPMAssertionID(kIOPMNullAssertionID)
        let status = api.declare(
            assertionName, kIOPMUserActiveRemote, &assertionID)
        guard status == kIOReturnSuccess else {
            throw RemoteUserActivityError(
                detail: "could not declare remote user activity (IOKit error \(status))")
        }
        guard assertionID != IOPMAssertionID(kIOPMNullAssertionID) else {
            throw RemoteUserActivityError(
                detail: "remote user activity returned a null assertion")
        }
        return RemoteUserActivityLease(
            assertionID: assertionID, releaseAssertion: api.release)
    }

    var isActive: Bool {
        condition.lock()
        defer { condition.unlock() }
        if case .active = state { return true }
        return false
    }

    /// Defense in depth at the grant boundary. The production interactor
    /// orders release before `prepareGrant`, but the system boundary also
    /// verifies that fact under the lease's lock so an injected or future
    /// conformer cannot mint authority while activity remains live or while a
    /// release is ambiguous.
    func requireReleasedBeforeGrant() throws {
        condition.lock()
        while case .releasing = state { condition.wait() }
        defer { condition.unlock() }
        switch state {
        case .released:
            return
        case .releaseFailed(let error):
            throw error
        case .active:
            throw RemoteUserActivityError(
                detail: "remote user activity was not released before grant preparation")
        case .releasing:
            throw RemoteUserActivityError(
                detail: "remote user-activity release remained in progress")
        }
    }

    func release() throws {
        condition.lock()
        while case .releasing = state { condition.wait() }
        switch state {
        case .released:
            condition.unlock()
            return
        case .releaseFailed(let error):
            condition.unlock()
            throw error
        case .active:
            state = .releasing
            condition.unlock()
        case .releasing:
            // The wait above makes this unreachable while the condition lock
            // is held, but keep the switch exhaustive if State changes.
            condition.unlock()
            throw RemoteUserActivityError(
                detail: "remote user-activity release remained in progress")
        }

        let status = releaseAssertion(assertionID)
        let failure: RemoteUserActivityError? = status == kIOReturnSuccess
            ? nil
            : RemoteUserActivityError(
                detail: "could not release remote user activity (IOKit error \(status))")

        condition.lock()
        state = failure.map(State.releaseFailed) ?? .released
        condition.broadcast()
        condition.unlock()

        if let failure { throw failure }
    }
}

extension DesktopService {
    func beginRemoteUserActivity(
        using api: RemoteUserActivityPowerAPI = .system
    ) throws -> RemoteUserActivityLease {
        try RemoteUserActivityLease.declare(using: api)
    }
}
