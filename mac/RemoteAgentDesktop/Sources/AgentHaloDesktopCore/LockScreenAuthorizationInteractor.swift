import ApplicationServices
import AppKit
import Foundation

/// The narrow boundary used by `DesktopSystem` to begin loginwindow's own
/// authorization transaction. Keeping this separate from general desktop
/// actions prevents a caller from turning arbitrary AX controls into an
/// alternate unlock surface.
public protocol LockScreenAuthorizationRequesting: Sendable {
    func requestAuthorization(
        authorizationFieldReady: @Sendable () -> Void,
        releaseRemoteUserActivity: @Sendable () throws -> Void,
        prepareGrant: @Sendable () throws -> Void,
        emptyValueWritten: @Sendable () -> Void,
        completionReceiptObserved: @Sendable () throws -> Bool,
        isLocked: @Sendable () throws -> Bool
    ) throws
}

public struct LockScreenAuthorizationError: Error, Equatable, CustomStringConvertible {
    public let detail: String

    public init(_ detail: String) {
        self.detail = detail
    }

    public var description: String { detail }
}

/// Starts the screensaver/loginwindow authorization flow through the control
/// macOS exposes for that purpose. It never reads the password value or writes
/// a credential. After focusing the exact field it explicitly writes an empty
/// AX value as the sole candidate trigger for loginwindow's authorization
/// transaction. The empty assignment is the entire submission: this interactor
/// deliberately does not discover or perform AX actions on the secure field.
///
/// The exact identifier is intentionally the only selectable target. Falling
/// back to a title such as "Unlock" would be localization-dependent and, more
/// importantly, could press an unrelated control if focus unexpectedly stayed
/// on an application in the user session.
public final class SystemLockScreenAuthorizationInteractor:
    LockScreenAuthorizationRequesting, @unchecked Sendable
{
    static let loginwindowBundleIdentifier = "com.apple.loginwindow"
    static let loginwindowBundlePath = "/System/Library/CoreServices/loginwindow.app"
    static let loginwindowExecutablePath =
        "/System/Library/CoreServices/loginwindow.app/Contents/MacOS/loginwindow"
    static let passwordFieldIdentifier = "UserPasswordTextField"
    private static let pollInterval: TimeInterval = 0.05
    /// Wake-to-ready is not instantaneous on real hardware. Discovery/focus is
    /// a non-authorizing, pre-submission phase and deliberately runs before the
    /// controller publishes a grant, so this wait consumes none of its TTL.
    static let discoveryTimeout: TimeInterval = 8
    /// Once the exact field is focused and the grant has been published, the
    /// empty assignment remains a short, single-attempt boundary.
    /// A fresh deadline prevents slow discovery from stealing this phase's
    /// bounded AX IPC budget.
    private static let submissionTimeout: TimeInterval = 1.5
    /// This is a system-version-sensitive lifecycle acknowledgement, not a
    /// sleep used as proof. The request succeeds only after an exact terminal
    /// receipt is bracketed by the same live field + locked state, followed by
    /// that field leaving the focused AX tree and the session becoming
    /// unlocked. Timeout or an unreadable state is an error and the controller
    /// quarantines the transaction.
    static let completionTimeout: TimeInterval = 10
    static let messagingTimeout: Float = 0.25
    private static let maximumDepth = 24
    private static let maximumNodes = 512

    public init() {}

    /// A value-only snapshot is the selection seam for the privileged target.
    /// Production fills it from `NSRunningApplication`; tests can prove every
    /// rejection without launching or impersonating loginwindow.
    struct RunningApplicationIdentity: Equatable, Sendable {
        let bundleIdentifier: String?
        let bundlePath: String?
        let executablePath: String?
        let isTerminated: Bool
        let processIdentifier: pid_t
    }

    private struct LocatedPasswordField {
        let element: AXUIElement
        let application: NSRunningApplication
        let processIdentifier: pid_t
    }

    /// Only one live process with all three immutable identity handles may be
    /// used. A bundle identifier alone is not an ownership boundary, and a
    /// best-effort choice among multiple exact processes would be ambiguous.
    static func exactLoginwindow<Application>(
        from applications: [Application],
        identity: (Application) -> RunningApplicationIdentity
    ) throws -> Application {
        let matches = applications.filter {
            let value = identity($0)
            return !value.isTerminated &&
                value.processIdentifier > 0 &&
                value.bundleIdentifier == loginwindowBundleIdentifier &&
                value.bundlePath == loginwindowBundlePath &&
                value.executablePath == loginwindowExecutablePath
        }
        guard !matches.isEmpty else {
            throw LockScreenAuthorizationError(
                "the exact live macOS loginwindow process was not found")
        }
        guard matches.count == 1 else {
            throw LockScreenAuthorizationError(
                "multiple exact live macOS loginwindow processes were found")
        }
        return matches[0]
    }

    static func exactLoginwindowProcessIdentifier(
        from applications: [RunningApplicationIdentity]
    ) throws -> pid_t {
        try exactLoginwindow(from: applications, identity: { $0 }).processIdentifier
    }

    /// PID equality is insufficient because a restarted process can reuse a
    /// PID. Production supplies `NSRunningApplication.isEqual`; tests supply a
    /// fake instance token and exercise same-PID replacement explicitly.
    @discardableResult
    static func requireSameExactLoginwindow<Application>(
        _ expected: Application,
        from applications: [Application],
        identity: (Application) -> RunningApplicationIdentity,
        isSameInstance: (Application, Application) -> Bool
    ) throws -> Application {
        let current = try exactLoginwindow(
            from: applications, identity: identity)
        let expectedIdentity = identity(expected)
        let currentIdentity = identity(current)
        guard isSameInstance(expected, current),
              expectedIdentity.processIdentifier > 0,
              currentIdentity.processIdentifier == expectedIdentity.processIdentifier else {
            throw LockScreenAuthorizationError(
                "the exact macOS loginwindow process instance changed during authorization")
        }
        return current
    }

    static func requireExactProcessIdentifier(
        actual: pid_t,
        expected: pid_t,
        elementDescription: String
    ) throws {
        guard actual > 0, actual == expected else {
            throw LockScreenAuthorizationError(
                "the \(elementDescription) Accessibility element is not owned by the exact loginwindow process")
        }
    }

    /// Seed order is part of the routing contract. Values may repeat across AX
    /// attributes, so de-duplicate them before the bounded traversal.
    static func orderedSearchSeeds<Node>(
        focusedUIElement: Node?,
        focusedWindow: Node?,
        windows: [Node],
        applicationRoot: Node,
        areEqual: (Node, Node) -> Bool
    ) -> [Node] {
        var result: [Node] = []
        let candidates = [focusedUIElement, focusedWindow] +
            windows.map(Optional.some) + [Optional.some(applicationRoot)]
        for candidate in candidates {
            guard let candidate,
                  !result.contains(where: { areEqual($0, candidate) }) else {
                continue
            }
            result.append(candidate)
        }
        return result
    }

    /// The production AX walk and the fake-graph tests use the same bounded
    /// algorithm. A foreign-PID focused element is ignored as a whole; it can
    /// neither match the exact identifier nor introduce descendants into the
    /// trusted loginwindow search.
    static func exactPasswordField<Node>(
        seeds: [Node],
        expectedProcessIdentifier: pid_t,
        maximumDepth: Int = maximumDepth,
        maximumNodes: Int = maximumNodes,
        areEqual: (Node, Node) -> Bool,
        processIdentifier: (Node) throws -> pid_t,
        identifier: (Node) throws -> String?,
        children: (Node) throws -> [Node]
    ) rethrows -> Node? {
        var queue = seeds.map { (node: $0, depth: 0) }
        var visited: [Node] = []
        var examined = 0
        while !queue.isEmpty, examined < maximumNodes {
            let item = queue.removeFirst()
            if visited.contains(where: { areEqual($0, item.node) }) { continue }
            visited.append(item.node)
            examined += 1

            guard try processIdentifier(item.node) == expectedProcessIdentifier else {
                continue
            }
            if try identifier(item.node) == passwordFieldIdentifier {
                return item.node
            }
            guard item.depth < maximumDepth else { continue }
            for child in try children(item.node) {
                queue.append((node: child, depth: item.depth + 1))
            }
        }
        return nil
    }

    static func requireResponsive(_ status: AXError, operation: String) throws {
        if status == .cannotComplete {
            throw LockScreenAuthorizationError(
                "\(operation) timed out because loginwindow did not respond")
        }
    }

    /// Pure status classification seams keep the credential-free field
    /// preparation contract testable without depending on a live loginwindow.
    /// A caller may submit the field only after both checks return.
    static func requireEmptyValueWritable(
        queryStatus: AXError, isSettable: Bool
    ) throws {
        try requireResponsive(
            queryStatus,
            operation: "checking whether the lock-screen authorization field accepts an empty value")
        guard queryStatus == .success else {
            throw LockScreenAuthorizationError(
                "could not verify that the macOS lock-screen authorization field accepts an empty value (AX error \(queryStatus.rawValue))")
        }
        guard isSettable else {
            throw LockScreenAuthorizationError(
                "the macOS lock-screen authorization field does not accept an empty value")
        }
    }

    static func requireEmptyValueWritten(_ status: AXError) throws {
        try requireResponsive(
            status,
            operation: "writing an empty lock-screen authorization value")
        guard status == .success else {
            throw LockScreenAuthorizationError(
                "could not write an empty value to the macOS lock-screen authorization field (AX error \(status.rawValue))")
        }
    }

    /// The complete pregrant readiness vocabulary. In particular, this seam
    /// has no action-discovery/action-performance closure: the only accepted
    /// phases are exact-field resolution, focus/readback, and empty-value
    /// settability. Production and deterministic tests share the same order.
    static func performCredentialFreeFieldReadiness<Field>(
        locateExactField: () throws -> Field,
        focusAndReadBack: (Field) throws -> Void,
        requireEmptyValueSettable: (Field) throws -> Void
    ) rethrows -> Field {
        let field = try locateExactField()
        try focusAndReadBack(field)
        try requireEmptyValueSettable(field)
        return field
    }

    /// The empty AX assignment can itself be the event that starts the system
    /// authorization transaction. Keep the single mutation behind an explicit
    /// non-retrying seam: any ambiguous result must flow to controller proof
    /// verification/quarantine rather than causing a second write. The
    /// nonescaping `didWrite` observer runs only after the successful write and
    /// carries no grant material.
    static func performSingleEmptyValueSubmission(
        writeEmptyValue: () throws -> Void,
        didWrite: () -> Void = {}
    ) throws {
        try writeEmptyValue()
        didWrite()
    }

    /// Retain and revalidate the exact prepared field immediately before the
    /// only postgrant mutation. Tests use a value-token identity here so a
    /// same-PID process/field replacement cannot disappear into AX plumbing.
    static func writePreparedEmptyValue<PreparedField>(
        _ preparedField: PreparedField,
        revalidate: (PreparedField) throws -> Void,
        write: (PreparedField) throws -> Void,
        didWrite: () -> Void = {}
    ) throws {
        try revalidate(preparedField)
        try performSingleEmptyValueSubmission(
            writeEmptyValue: { try write(preparedField) },
            didWrite: didWrite)
    }

    /// Orders every non-authorizing readiness check before grant publication.
    /// `submit` is the first phase allowed to write the empty value. Keeping
    /// this orchestration as a pure seam makes it impossible for a failed field
    /// readiness preflight to publish ambient
    /// authority, and keeps that invariant testable without live loginwindow.
    static func performGrantGatedSubmission<PreparedField>(
        preflight: () throws -> PreparedField,
        revalidateBeforeGrant: (PreparedField) throws -> Void = { _ in },
        authorizationFieldReady: () -> Void = {},
        releaseRemoteUserActivity: () throws -> Void = {},
        prepareGrant: () throws -> Void,
        submit: (PreparedField) throws -> Void
    ) throws {
        let preparedField = try preflight()
        try revalidateBeforeGrant(preparedField)
        authorizationFieldReady()
        try releaseRemoteUserActivity()
        try prepareGrant()
        try submit(preparedField)
    }

    private static func before(_ deadline: Date, operation: String) throws {
        guard Date() <= deadline else {
            throw LockScreenAuthorizationError(
                "\(operation) exceeded the lock-screen authorization deadline")
        }
    }

    private static func configureTimeout(
        _ element: AXUIElement, deadline: Date
    ) throws {
        try before(deadline, operation: "configuring loginwindow Accessibility IPC")
        let status = AXUIElementSetMessagingTimeout(element, messagingTimeout)
        guard status == .success else {
            throw LockScreenAuthorizationError(
                "could not bound loginwindow Accessibility IPC (AX error \(status.rawValue))")
        }
    }

    public func requestAuthorization(
        authorizationFieldReady: @Sendable () -> Void,
        releaseRemoteUserActivity: @Sendable () throws -> Void,
        prepareGrant: @Sendable () throws -> Void,
        emptyValueWritten: @Sendable () -> Void,
        completionReceiptObserved: @Sendable () throws -> Bool,
        isLocked: @Sendable () throws -> Bool
    ) throws {
        guard AXIsProcessTrusted() else {
            throw LockScreenAuthorizationError(
                "Accessibility permission is required to drive the macOS lock screen")
        }

        let deadline = Date().addingTimeInterval(Self.discoveryTimeout)
        var lastFailure = "the macOS lock-screen password field was not found"
        var focusedField: LocatedPasswordField?
        while Date() <= deadline {
            do {
                // loginwindow can expose the field before its focus or
                // empty-value surface is ready. Treat all of that as one
                // pregrant polling phase so staged UI readiness cannot cause
                // an early failure.
                let field = try Self.performCredentialFreeFieldReadiness(
                    locateExactField: {
                        try self.passwordField(deadline: deadline)
                    },
                    focusAndReadBack: { field in
                        try self.focus(
                            field.element,
                            expectedProcessIdentifier: field.processIdentifier,
                            deadline: deadline)
                    },
                    requireEmptyValueSettable: { field in
                        try self.preflightEmptySubmission(
                            field.element,
                            expectedProcessIdentifier: field.processIdentifier,
                            deadline: deadline)
                    })
                focusedField = field
                break
            } catch let error as LockScreenAuthorizationError {
                lastFailure = error.detail
            } catch {
                lastFailure = "could not inspect the macOS lock screen: \(error)"
            }
            Thread.sleep(forTimeInterval: Self.pollInterval)
        }
        guard let focusedField else {
            throw LockScreenAuthorizationError(lastFailure)
        }

        // The entire find/focus/value readiness phase above completed before
        // this gate. From prepareGrant onward no readiness query is allowed,
        // no AX action API exists in this interactor, and no failure is retried.
        try Self.performGrantGatedSubmission(
            preflight: { focusedField },
            revalidateBeforeGrant: { preparedField in
                try self.requireSameLoginwindowApplication(preparedField.application)
                try self.requireProcessIdentifier(
                    preparedField.element,
                    expected: preparedField.processIdentifier,
                    deadline: deadline)
            },
            // Keep the remote user-activity assertion alive through exact
            // field discovery, focus readback, value readiness, and identity
            // revalidation. Then synchronously release it before the
            // controller is allowed to mint or write any grant.
            authorizationFieldReady: authorizationFieldReady,
            releaseRemoteUserActivity: releaseRemoteUserActivity,
            prepareGrant: prepareGrant,
            submit: { preparedField in
                // Freshly bound after grant preparation, so slow wake,
                // discovery, focus, and readiness checks consume none of this
                // single empty write's bounded AX budget.
                let submissionDeadline = Date().addingTimeInterval(
                    Self.submissionTimeout)
                try Self.writePreparedEmptyValue(
                    preparedField,
                    revalidate: { field in
                        try self.requireSameLoginwindowApplication(field.application)
                        try self.requireProcessIdentifier(
                            field.element,
                            expected: field.processIdentifier,
                            deadline: submissionDeadline)
                    },
                    write: { field in
                        try self.writeEmptySubmission(
                            field.element,
                            expectedProcessIdentifier: field.processIdentifier,
                            deadline: submissionDeadline)
                    },
                    didWrite: emptyValueWritten)
            })

        // Never re-submit after this boundary. A lifecycle failure is
        // ambiguous and must reach controller quarantine unchanged.
        try waitForTransactionCompletion(
            field: focusedField,
            completionReceiptObserved: completionReceiptObserved,
            isLocked: isLocked)
    }

    private func passwordField(deadline: Date) throws -> LocatedPasswordField {
        guard let field = try passwordFieldIfPresent(deadline: deadline) else {
            throw LockScreenAuthorizationError(
                "the macOS lock-screen password field was not found")
        }
        return field
    }

    private func passwordFieldIfPresent(
        deadline: Date,
        expectedProcessIdentifier: pid_t? = nil
    ) throws -> LocatedPasswordField? {
        try Self.before(deadline, operation: "locating the exact loginwindow process")
        let runningApplications = runningLoginwindowApplications()
        let application = try Self.exactLoginwindow(
            from: runningApplications,
            identity: Self.runningApplicationIdentity)
        let processIdentifier = application.processIdentifier
        if let expectedProcessIdentifier {
            guard processIdentifier == expectedProcessIdentifier else {
                throw LockScreenAuthorizationError(
                    "the exact macOS loginwindow process changed during authorization")
            }
        }
        let root = AXUIElementCreateApplication(processIdentifier)
        try Self.configureTimeout(root, deadline: deadline)
        try requireProcessIdentifier(
            root, expected: processIdentifier, deadline: deadline)

        let focusedUIElement = try element(
            root, kAXFocusedUIElementAttribute as String, deadline: deadline)
        let focusedWindow = try element(
            root, kAXFocusedWindowAttribute as String, deadline: deadline)
        let windows = try elements(
            root, kAXWindowsAttribute as String, deadline: deadline)
        let seeds = Self.orderedSearchSeeds(
            focusedUIElement: focusedUIElement,
            focusedWindow: focusedWindow,
            windows: windows,
            applicationRoot: root,
            areEqual: { CFEqual($0, $1) })
        let field = try Self.exactPasswordField(
            seeds: seeds,
            expectedProcessIdentifier: processIdentifier,
            areEqual: { CFEqual($0, $1) },
            processIdentifier: {
                try self.processIdentifier($0, deadline: deadline)
            },
            identifier: {
                try Self.before(
                    deadline,
                    operation: "locating the lock-screen authorization field")
                return try self.string(
                    $0, kAXIdentifierAttribute as String, deadline: deadline)
            },
            children: {
                try Self.before(
                    deadline,
                    operation: "locating the lock-screen authorization field")
                return try self.children($0, deadline: deadline)
            })
        guard let field else { return nil }
        try requireProcessIdentifier(
            field, expected: processIdentifier, deadline: deadline)
        return LocatedPasswordField(
            element: field,
            application: application,
            processIdentifier: processIdentifier)
    }

    private func waitForTransactionCompletion(
        field: LocatedPasswordField,
        completionReceiptObserved: @Sendable () throws -> Bool,
        isLocked: @Sendable () throws -> Bool
    ) throws {
        let deadline = Date().addingTimeInterval(Self.completionTimeout)
        var terminalObserved = false
        while Date() <= deadline {
            if !terminalObserved {
                // Sample the UI/lock boundary on both sides of the exact proof
                // read. A field loss or unrelated unlock before terminal is an
                // ambiguous transaction and must never be repaired by a later
                // receipt into a successful open.
                guard try isLocked(),
                      try samePasswordFieldIsReachable(field, deadline: deadline) else {
                    throw LockScreenAuthorizationError(
                        "lock-screen UI changed before the exact authorization terminal")
                }
                if try completionReceiptObserved() {
                    guard try isLocked(),
                          try samePasswordFieldIsReachable(field, deadline: deadline) else {
                        throw LockScreenAuthorizationError(
                            "lock-screen UI changed while observing the authorization terminal")
                    }
                    terminalObserved = true
                }
            } else {
                let sameField = try samePasswordFieldIsReachable(
                    field, deadline: deadline)
                let locked = try isLocked()
                if !sameField && !locked { return }
                if sameField && !locked {
                    throw LockScreenAuthorizationError(
                        "the screen unlocked before the authorization UI transaction completed")
                }
            }
            Thread.sleep(forTimeInterval: Self.pollInterval)
        }
        throw LockScreenAuthorizationError(
            "the lock-screen authorization UI transaction did not complete before its deadline")
    }

    private func samePasswordFieldIsReachable(
        _ expected: LocatedPasswordField, deadline: Date
    ) throws -> Bool {
        try requireSameLoginwindowApplication(expected.application)
        guard let current = try passwordFieldIfPresent(
            deadline: deadline,
            expectedProcessIdentifier: expected.processIdentifier) else {
            return false
        }
        return current.application.isEqual(expected.application) &&
            current.processIdentifier == expected.processIdentifier &&
            CFEqual(current.element, expected.element)
    }

    private func focus(
        _ field: AXUIElement,
        expectedProcessIdentifier: pid_t,
        deadline: Date
    ) throws {
        try requireProcessIdentifier(
            field, expected: expectedProcessIdentifier, deadline: deadline)
        try Self.configureTimeout(field, deadline: deadline)
        try Self.before(deadline, operation: "focusing the lock-screen authorization field")
        var settable = DarwinBoolean(false)
        let query = AXUIElementIsAttributeSettable(
            field, kAXFocusedAttribute as CFString, &settable)
        try Self.requireResponsive(query, operation: "focusing the lock-screen authorization field")
        guard query == .success else {
            throw LockScreenAuthorizationError(
                "could not inspect focus for the macOS lock-screen authorization field (AX error \(query.rawValue))")
        }
        if settable.boolValue {
            try Self.before(deadline, operation: "focusing the lock-screen authorization field")
            let result = AXUIElementSetAttributeValue(
                field, kAXFocusedAttribute as CFString, kCFBooleanTrue)
            try Self.requireResponsive(
                result, operation: "focusing the lock-screen authorization field")
            guard result == .success else {
                throw LockScreenAuthorizationError(
                    "could not focus the macOS lock-screen authorization field (AX error \(result.rawValue))")
            }
        }
        try Self.before(deadline, operation: "confirming lock-screen authorization focus")
        var focusedValue: CFTypeRef?
        let readback = AXUIElementCopyAttributeValue(
            field, kAXFocusedAttribute as CFString, &focusedValue)
        try Self.requireResponsive(
            readback, operation: "confirming lock-screen authorization focus")
        guard readback == .success, focusedValue as? Bool == true else {
            throw LockScreenAuthorizationError(
                "the macOS lock-screen authorization field did not become focused (AX error \(readback.rawValue))")
        }
    }

    private func preflightEmptySubmission(
        _ field: AXUIElement,
        expectedProcessIdentifier: pid_t,
        deadline: Date
    ) throws {
        try requireProcessIdentifier(
            field, expected: expectedProcessIdentifier, deadline: deadline)
        try Self.configureTimeout(field, deadline: deadline)
        try Self.before(
            deadline,
            operation: "checking whether the lock-screen authorization field accepts an empty value")
        var settable = DarwinBoolean(false)
        let query = AXUIElementIsAttributeSettable(
            field, kAXValueAttribute as CFString, &settable)
        try Self.requireEmptyValueWritable(
            queryStatus: query, isSettable: settable.boolValue)
    }

    private func writeEmptySubmission(
        _ field: AXUIElement,
        expectedProcessIdentifier: pid_t,
        deadline: Date
    ) throws {
        // This is a deliberately credential-free empty public AX assignment,
        // not a sentinel. Never copy/read the secure field's current value.
        // Although the resulting field value is empty, the assignment itself
        // may trigger authorization and therefore must never be retried.
        // There is deliberately no following AX action. If loginwindow cannot
        // acknowledge this edit, the authorization request is not attributable.
        // `writePreparedEmptyValue` re-resolved the unique exact AppKit
        // identity and AX field owner immediately before entering this write.
        try Self.before(
            deadline, operation: "writing an empty lock-screen authorization value")
        let written = AXUIElementSetAttributeValue(
            field, kAXValueAttribute as CFString, "" as CFString)
        try Self.requireEmptyValueWritten(written)
    }

    private static func runningApplicationIdentity(
        _ application: NSRunningApplication
    ) -> RunningApplicationIdentity {
        RunningApplicationIdentity(
            bundleIdentifier: application.bundleIdentifier,
            bundlePath: application.bundleURL?.path,
            executablePath: application.executableURL?.path,
            isTerminated: application.isTerminated,
            processIdentifier: application.processIdentifier)
    }

    private func runningLoginwindowApplications() -> [NSRunningApplication] {
        NSRunningApplication.runningApplications(
            withBundleIdentifier: Self.loginwindowBundleIdentifier)
    }

    private func requireSameLoginwindowApplication(
        _ expected: NSRunningApplication
    ) throws {
        try Self.requireSameExactLoginwindow(
            expected,
            from: runningLoginwindowApplications(),
            identity: Self.runningApplicationIdentity,
            isSameInstance: { $0.isEqual($1) })
    }

    private func processIdentifier(
        _ element: AXUIElement, deadline: Date
    ) throws -> pid_t {
        try Self.configureTimeout(element, deadline: deadline)
        try Self.before(deadline, operation: "verifying loginwindow Accessibility ownership")
        var processIdentifier: pid_t = 0
        let status = AXUIElementGetPid(element, &processIdentifier)
        try Self.requireResponsive(
            status, operation: "verifying loginwindow Accessibility ownership")
        guard status == .success, processIdentifier > 0 else {
            throw LockScreenAuthorizationError(
                "could not verify loginwindow Accessibility ownership (AX error \(status.rawValue))")
        }
        return processIdentifier
    }

    private func requireProcessIdentifier(
        _ element: AXUIElement,
        expected: pid_t,
        deadline: Date
    ) throws {
        try Self.requireExactProcessIdentifier(
            actual: try processIdentifier(element, deadline: deadline),
            expected: expected,
            elementDescription: "lock-screen")
    }

    private func element(
        _ owner: AXUIElement, _ attribute: String, deadline: Date
    ) throws -> AXUIElement? {
        try Self.configureTimeout(owner, deadline: deadline)
        try Self.before(deadline, operation: "reading a loginwindow Accessibility seed")
        var value: CFTypeRef?
        let status = AXUIElementCopyAttributeValue(
            owner, attribute as CFString, &value)
        try Self.requireResponsive(
            status, operation: "reading a loginwindow Accessibility seed")
        guard status == .success, let value else { return nil }
        guard CFGetTypeID(value) == AXUIElementGetTypeID() else { return nil }
        return unsafeDowncast(value, to: AXUIElement.self)
    }

    private func elements(
        _ owner: AXUIElement, _ attribute: String, deadline: Date
    ) throws -> [AXUIElement] {
        try Self.configureTimeout(owner, deadline: deadline)
        try Self.before(deadline, operation: "reading loginwindow Accessibility seeds")
        var value: CFTypeRef?
        let status = AXUIElementCopyAttributeValue(
            owner, attribute as CFString, &value)
        try Self.requireResponsive(
            status, operation: "reading loginwindow Accessibility seeds")
        guard status == .success else { return [] }
        return value as? [AXUIElement] ?? []
    }

    private func string(
        _ element: AXUIElement, _ attribute: String, deadline: Date
    ) throws -> String? {
        try Self.configureTimeout(element, deadline: deadline)
        try Self.before(deadline, operation: "reading a loginwindow Accessibility attribute")
        var value: CFTypeRef?
        let status = AXUIElementCopyAttributeValue(
            element, attribute as CFString, &value)
        try Self.requireResponsive(
            status, operation: "reading a loginwindow Accessibility attribute")
        guard status == .success else { return nil }
        return value as? String
    }

    private func children(
        _ element: AXUIElement, deadline: Date
    ) throws -> [AXUIElement] {
        try Self.configureTimeout(element, deadline: deadline)
        try Self.before(deadline, operation: "reading loginwindow Accessibility children")
        var value: CFTypeRef?
        let status = AXUIElementCopyAttributeValue(
            element, kAXChildrenAttribute as CFString, &value)
        try Self.requireResponsive(
            status, operation: "reading loginwindow Accessibility children")
        guard status == .success else { return [] }
        return value as? [AXUIElement] ?? []
    }
}
