import Foundation
import IOKit.pwr_mgt
import XCTest
@testable import AgentHaloDesktopCore

private enum RemoteActivityTestError: Error {
    case expected
}

private final class ReleaseCount: @unchecked Sendable {
    private let lock = NSLock()
    private var value = 0

    func increment() {
        lock.lock()
        value += 1
        lock.unlock()
    }

    var count: Int {
        lock.lock()
        defer { lock.unlock() }
        return value
    }
}

private final class ActivityFlag: @unchecked Sendable {
    private let lock = NSLock()
    private var value = false

    func set(_ newValue: Bool) {
        lock.lock()
        value = newValue
        lock.unlock()
    }

    var isActive: Bool {
        lock.lock()
        defer { lock.unlock() }
        return value
    }
}

private final class StubLockScreenAuthorization:
    LockScreenAuthorizationRequesting, @unchecked Sendable
{
    enum Mode {
        case failBeforeFieldReady
        case failAfterFieldReadyBeforeRelease
        case failAfterRelease
        case prepareGrantBeforeRelease
        case complete
    }

    private let lock = NSLock()
    private(set) var requestCount = 0
    let mode: Mode

    init(mode: Mode) { self.mode = mode }

    func requestAuthorization(
        authorizationFieldReady: @Sendable () -> Void,
        releaseRemoteUserActivity: @Sendable () throws -> Void,
        prepareGrant: @Sendable () throws -> Void,
        emptyValueWriteAttempted: @Sendable () -> Void,
        emptyValueWritten: @Sendable () -> Void,
        confirmActionAttempted: @Sendable () -> Void,
        confirmActionPerformed: @Sendable () -> Void,
        completionReceiptObserved: @Sendable () throws -> Bool,
        isLocked: @Sendable () throws -> Bool
    ) throws {
        lock.lock()
        requestCount += 1
        lock.unlock()

        if mode == .failBeforeFieldReady { throw RemoteActivityTestError.expected }
        authorizationFieldReady()
        if mode == .failAfterFieldReadyBeforeRelease {
            throw RemoteActivityTestError.expected
        }
        if mode == .prepareGrantBeforeRelease {
            try prepareGrant()
            return
        }
        try releaseRemoteUserActivity()
        if mode == .failAfterRelease { throw RemoteActivityTestError.expected }
        try prepareGrant()
        emptyValueWriteAttempted()
        emptyValueWritten()
        confirmActionAttempted()
        confirmActionPerformed()
    }
}

final class RemoteUserActivityTests: XCTestCase {
    private func powerAPI(
        releaseStatus: IOReturn = kIOReturnSuccess,
        releaseCount: ReleaseCount = ReleaseCount(),
        activity: ActivityFlag = ActivityFlag()
    ) -> RemoteUserActivityPowerAPI {
        RemoteUserActivityPowerAPI(
            declare: { _, _, assertionID in
                XCTAssertEqual(assertionID, IOPMAssertionID(kIOPMNullAssertionID))
                assertionID = 41
                activity.set(true)
                return kIOReturnSuccess
            },
            release: { assertionID in
                XCTAssertEqual(assertionID, 41)
                releaseCount.increment()
                activity.set(false)
                return releaseStatus
            })
    }

    func testDeclareUsesFixedShortNameAndRemoteUserType() throws {
        let api = RemoteUserActivityPowerAPI(
            declare: { name, userType, assertionID in
                XCTAssertEqual(name, RemoteUserActivityLease.assertionName)
                XCTAssertFalse(name.isEmpty)
                XCTAssertLessThan(name.count, 128)
                XCTAssertEqual(
                    userType.rawValue, kIOPMUserActiveRemote.rawValue)
                XCTAssertEqual(
                    assertionID, IOPMAssertionID(kIOPMNullAssertionID))
                assertionID = 73
                return kIOReturnSuccess
            },
            release: { _ in kIOReturnSuccess })

        let lease = try RemoteUserActivityLease.declare(using: api)
        try lease.release()
    }

    func testDeclareFailureDoesNotReachAuthorizationOrRelease() {
        let releaseCount = ReleaseCount()
        let authorization = StubLockScreenAuthorization(mode: .complete)
        let api = RemoteUserActivityPowerAPI(
            declare: { _, userType, assertionID in
                XCTAssertEqual(userType.rawValue, kIOPMUserActiveRemote.rawValue)
                XCTAssertEqual(assertionID, IOPMAssertionID(kIOPMNullAssertionID))
                return kIOReturnError
            },
            release: { _ in
                releaseCount.increment()
                return kIOReturnSuccess
            })
        let system = DesktopSystem(
            desktop: DesktopService(),
            lockScreenAuthorization: authorization,
            remoteUserActivityPowerAPI: api)

        XCTAssertThrowsError(try system.requestUnlockAuthorization(
            authorizationFieldReady: {}, prepareGrant: {},
            emptyValueWriteAttempted: {}, emptyValueWritten: {},
            confirmActionAttempted: {}, confirmActionPerformed: {},
            completionReceiptObserved: { false }))
        XCTAssertEqual(authorization.requestCount, 0)
        XCTAssertEqual(releaseCount.count, 0)
    }

    func testSuccessfulDeclareWithNullAssertionFailsClosed() {
        let releaseCount = ReleaseCount()
        let api = RemoteUserActivityPowerAPI(
            declare: { _, _, _ in kIOReturnSuccess },
            release: { _ in
                releaseCount.increment()
                return kIOReturnSuccess
            })

        XCTAssertThrowsError(try RemoteUserActivityLease.declare(using: api)) {
            XCTAssertEqual(
                ($0 as? RemoteUserActivityError)?.detail,
                "remote user activity returned a null assertion")
        }
        XCTAssertEqual(releaseCount.count, 0)
    }

    func testFieldReadyRunsWhileLeaseIsAliveAndReleasePrecedesGrant() throws {
        let activity = ActivityFlag()
        let releaseCount = ReleaseCount()
        let authorization = StubLockScreenAuthorization(mode: .complete)
        let system = DesktopSystem(
            desktop: DesktopService(),
            lockScreenAuthorization: authorization,
            remoteUserActivityPowerAPI: powerAPI(
                releaseCount: releaseCount, activity: activity))
        let fieldReadyCount = ReleaseCount()
        let grantCount = ReleaseCount()

        try system.requestUnlockAuthorization(
            authorizationFieldReady: {
                XCTAssertTrue(activity.isActive)
                fieldReadyCount.increment()
            },
            prepareGrant: {
                XCTAssertFalse(activity.isActive)
                grantCount.increment()
            },
            emptyValueWriteAttempted: {}, emptyValueWritten: {},
            confirmActionAttempted: {}, confirmActionPerformed: {},
            completionReceiptObserved: { false })

        XCTAssertEqual(fieldReadyCount.count, 1)
        XCTAssertEqual(grantCount.count, 1)
        XCTAssertEqual(releaseCount.count, 1)
    }

    func testReleaseFailurePublishesNoGrantAndIsNotRetriedByDefer() {
        let releaseCount = ReleaseCount()
        let authorization = StubLockScreenAuthorization(mode: .complete)
        let system = DesktopSystem(
            desktop: DesktopService(),
            lockScreenAuthorization: authorization,
            remoteUserActivityPowerAPI: powerAPI(
                releaseStatus: kIOReturnError, releaseCount: releaseCount))
        let grantCount = ReleaseCount()

        XCTAssertThrowsError(try system.requestUnlockAuthorization(
            authorizationFieldReady: {},
            prepareGrant: { grantCount.increment() },
            emptyValueWriteAttempted: {}, emptyValueWritten: {},
            confirmActionAttempted: {}, confirmActionPerformed: {},
            completionReceiptObserved: { false }))
        XCTAssertEqual(grantCount.count, 0)
        XCTAssertEqual(releaseCount.count, 1)
    }

    func testInjectedInteractorCannotPrepareGrantBeforeSuccessfulRelease() {
        let releaseCount = ReleaseCount()
        let authorization = StubLockScreenAuthorization(
            mode: .prepareGrantBeforeRelease)
        let system = DesktopSystem(
            desktop: DesktopService(),
            lockScreenAuthorization: authorization,
            remoteUserActivityPowerAPI: powerAPI(releaseCount: releaseCount))
        let grantCount = ReleaseCount()

        XCTAssertThrowsError(try system.requestUnlockAuthorization(
            authorizationFieldReady: {},
            prepareGrant: { grantCount.increment() },
            emptyValueWriteAttempted: {}, emptyValueWritten: {},
            confirmActionAttempted: {}, confirmActionPerformed: {},
            completionReceiptObserved: { false })) {
            XCTAssertTrue(
                String(describing: $0).contains(
                    "not released before grant preparation"))
        }
        XCTAssertEqual(grantCount.count, 0)
        XCTAssertEqual(releaseCount.count, 1)
    }

    func testBodyAndCleanupFailuresAreBothReportedWithoutAssertionID() {
        let releaseCount = ReleaseCount()
        let authorization = StubLockScreenAuthorization(
            mode: .failBeforeFieldReady)
        let system = DesktopSystem(
            desktop: DesktopService(),
            lockScreenAuthorization: authorization,
            remoteUserActivityPowerAPI: powerAPI(
                releaseStatus: kIOReturnError, releaseCount: releaseCount))

        XCTAssertThrowsError(try system.requestUnlockAuthorization(
            authorizationFieldReady: {}, prepareGrant: {},
            emptyValueWriteAttempted: {}, emptyValueWritten: {},
            confirmActionAttempted: {}, confirmActionPerformed: {},
            completionReceiptObserved: { false })) {
            let detail = String(describing: $0)
            XCTAssertTrue(detail.contains("authorization failed: expected"))
            XCTAssertTrue(detail.contains("cleanup also failed"))
            XCTAssertTrue(detail.contains("could not release remote user activity"))
            XCTAssertFalse(detail.contains("assertion 41"))
            XCTAssertFalse(detail.contains("assertionID"))
        }
        XCTAssertEqual(releaseCount.count, 1)
    }

    func testEveryAuthorizationFailurePathReleasesUnderlyingAssertionOnce() {
        for mode in [
            StubLockScreenAuthorization.Mode.failBeforeFieldReady,
            .failAfterFieldReadyBeforeRelease,
            .failAfterRelease,
        ] {
            let releaseCount = ReleaseCount()
            let authorization = StubLockScreenAuthorization(mode: mode)
            let system = DesktopSystem(
                desktop: DesktopService(),
                lockScreenAuthorization: authorization,
                remoteUserActivityPowerAPI: powerAPI(releaseCount: releaseCount))

            XCTAssertThrowsError(try system.requestUnlockAuthorization(
                authorizationFieldReady: {}, prepareGrant: {},
                emptyValueWriteAttempted: {}, emptyValueWritten: {},
                confirmActionAttempted: {}, confirmActionPerformed: {},
                completionReceiptObserved: { false }))
            XCTAssertEqual(
                releaseCount.count, 1,
                "failure mode \(mode) did not release exactly once")
        }
    }

    func testConcurrentDoubleReleaseCallsSystemReleaseOnce() throws {
        let releaseCount = ReleaseCount()
        let releaseStarted = DispatchSemaphore(value: 0)
        let allowRelease = DispatchSemaphore(value: 0)
        let api = RemoteUserActivityPowerAPI(
            declare: { _, _, assertionID in
                assertionID = 91
                return kIOReturnSuccess
            },
            release: { _ in
                releaseCount.increment()
                releaseStarted.signal()
                allowRelease.wait()
                return kIOReturnSuccess
            })
        let lease = try RemoteUserActivityLease.declare(using: api)
        let group = DispatchGroup()
        let errors = ReleaseCount()

        for _ in 0..<2 {
            group.enter()
            DispatchQueue.global().async {
                defer { group.leave() }
                do { try lease.release() } catch { errors.increment() }
            }
        }
        XCTAssertEqual(releaseStarted.wait(timeout: .now() + 1), .success)
        allowRelease.signal()
        XCTAssertEqual(group.wait(timeout: .now() + 1), .success)
        XCTAssertEqual(releaseCount.count, 1)
        XCTAssertEqual(errors.count, 0)
        XCTAssertFalse(lease.isActive)
    }

    func testFailedReleaseIsIdempotentAndReplaysOriginalFailure() throws {
        let releaseCount = ReleaseCount()
        let lease = try RemoteUserActivityLease.declare(
            using: powerAPI(
                releaseStatus: kIOReturnError, releaseCount: releaseCount))

        for _ in 0..<2 {
            XCTAssertThrowsError(try lease.release()) {
                XCTAssertTrue(
                    ($0 as? RemoteUserActivityError)?.detail.contains(
                        "could not release remote user activity") == true)
            }
        }
        XCTAssertEqual(releaseCount.count, 1)
    }

    func testWakePathContainsNoSyntheticEventOrOnlineGeometryRemnants() throws {
        let packageDirectory = URL(fileURLWithPath: #filePath)
            .deletingLastPathComponent()
            .deletingLastPathComponent()
            .deletingLastPathComponent()
        let desktopSource = try String(
            contentsOf: packageDirectory.appendingPathComponent(
                "Sources/AgentHaloDesktopCore/Desktop.swift"),
            encoding: .utf8)
        let systemSource = try String(
            contentsOf: packageDirectory.appendingPathComponent(
                "Sources/AgentHaloDesktopCore/LockedUseSystem.swift"),
            encoding: .utf8)

        for forbidden in [
            "provokeUnlockAttempt", "wakeProbePoint", "onlineDisplayFrames",
            "CGGetOnlineDisplayList", "lock-screen wake event",
        ] {
            XCTAssertFalse(desktopSource.contains(forbidden))
            XCTAssertFalse(systemSource.contains(forbidden))
        }
        XCTAssertTrue(systemSource.contains("beginRemoteUserActivity"))
    }
}
