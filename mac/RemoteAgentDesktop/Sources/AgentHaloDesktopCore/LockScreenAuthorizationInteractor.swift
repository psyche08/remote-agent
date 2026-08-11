import ApplicationServices
import Foundation

/// The narrow boundary used by `DesktopSystem` to begin loginwindow's own
/// authorization transaction. Keeping this separate from general desktop
/// actions prevents a caller from turning arbitrary AX controls into an
/// alternate unlock surface.
public protocol LockScreenAuthorizationRequesting: Sendable {
    func requestAuthorization(
        completionReceiptObserved: @escaping @Sendable () throws -> Bool,
        isLocked: @escaping @Sendable () throws -> Bool
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
/// AX value to ask loginwindow to observe an edit before this implementation's
/// existing semantic submit action. On-device E2E remains the authority for
/// whether that submit action is needed and harmless on each supported macOS.
///
/// The exact identifier is intentionally the only selectable target. Falling
/// back to a title such as "Unlock" would be localization-dependent and, more
/// importantly, could press an unrelated control if focus unexpectedly stayed
/// on an application in the user session.
public final class SystemLockScreenAuthorizationInteractor:
    LockScreenAuthorizationRequesting, @unchecked Sendable
{
    static let passwordFieldIdentifier = "UserPasswordTextField"
    private static let pollInterval: TimeInterval = 0.05
    /// Strictly below the minimum two-second grant lifetime. A hung
    /// loginwindow cannot strand `.opening` (and its shield) beyond the grant
    /// boundary; the controller receives a failure and runs normal cleanup.
    private static let discoveryTimeout: TimeInterval = 1.5
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

    /// The empty AX assignment can itself be the event that starts the system
    /// authorization transaction. Keep it and the following semantic action
    /// in a single, non-retrying boundary so a failed confirm can never cause
    /// this request to assign the empty value a second time.
    static func performSingleSubmission(
        prepareEmptyValue: () throws -> Void,
        confirm: () throws -> Void
    ) throws {
        try prepareEmptyValue()
        try confirm()
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
        completionReceiptObserved: @escaping @Sendable () throws -> Bool,
        isLocked: @escaping @Sendable () throws -> Bool
    ) throws {
        guard AXIsProcessTrusted() else {
            throw LockScreenAuthorizationError(
                "Accessibility permission is required to drive the macOS lock screen")
        }

        let deadline = Date().addingTimeInterval(Self.discoveryTimeout)
        var lastFailure = "the macOS lock-screen password field was not found"
        var focusedField: AXUIElement?
        while Date() <= deadline {
            do {
                let field = try passwordField(deadline: deadline)
                try focus(field, deadline: deadline)
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

        // The retryable discovery phase ends before the first possible
        // trigger. From the empty-value attempt onward, every failure is
        // ambiguous and must escape unchanged; never loop, rewrite, or submit
        // again within this request.
        try Self.performSingleSubmission(
            prepareEmptyValue: {
                try self.prepareEmptySubmission(focusedField, deadline: deadline)
            },
            confirm: {
                try self.confirm(focusedField, deadline: deadline)
            })

        // Never re-submit after this boundary. A lifecycle failure is
        // ambiguous and must reach controller quarantine unchanged.
        try waitForTransactionCompletion(
            field: focusedField,
            completionReceiptObserved: completionReceiptObserved,
            isLocked: isLocked)
    }

    private func passwordField(deadline: Date) throws -> AXUIElement {
        guard let field = try passwordFieldIfPresent(deadline: deadline) else {
            throw LockScreenAuthorizationError(
                "the macOS lock-screen password field was not found")
        }
        return field
    }

    private func passwordFieldIfPresent(deadline: Date) throws -> AXUIElement? {
        let system = AXUIElementCreateSystemWide()
        try Self.configureTimeout(system, deadline: deadline)
        try Self.before(deadline, operation: "locating the lock-screen application")
        var focused: CFTypeRef?
        let result = AXUIElementCopyAttributeValue(
            system, kAXFocusedApplicationAttribute as CFString, &focused)
        try Self.requireResponsive(result, operation: "locating the lock-screen application")
        guard result == .success, let application = focused else {
            throw LockScreenAuthorizationError(
                "the focused macOS lock-screen application is unavailable (AX error \(result.rawValue))")
        }
        let root = unsafeDowncast(application, to: AXUIElement.self)
        try Self.configureTimeout(root, deadline: deadline)

        var queue: [(element: AXUIElement, depth: Int)] = [(root, 0)]
        var visited: [AXUIElement] = []
        var examined = 0
        while !queue.isEmpty, examined < Self.maximumNodes {
            try Self.before(deadline, operation: "locating the lock-screen authorization field")
            let item = queue.removeFirst()
            if visited.contains(where: { CFEqual($0, item.element) }) { continue }
            visited.append(item.element)
            examined += 1

            if try string(
                item.element, kAXIdentifierAttribute as String, deadline: deadline) ==
                Self.passwordFieldIdentifier
            {
                return item.element
            }
            guard item.depth < Self.maximumDepth else { continue }
            for child in try children(item.element, deadline: deadline) {
                queue.append((child, item.depth + 1))
            }
        }
        return nil
    }

    private func waitForTransactionCompletion(
        field: AXUIElement,
        completionReceiptObserved: @escaping @Sendable () throws -> Bool,
        isLocked: @escaping @Sendable () throws -> Bool
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
        _ expected: AXUIElement, deadline: Date
    ) throws -> Bool {
        guard let current = try passwordFieldIfPresent(deadline: deadline) else {
            return false
        }
        return CFEqual(current, expected)
    }

    private func focus(_ field: AXUIElement, deadline: Date) throws {
        try Self.configureTimeout(field, deadline: deadline)
        try Self.before(deadline, operation: "focusing the lock-screen authorization field")
        var settable = DarwinBoolean(false)
        let query = AXUIElementIsAttributeSettable(
            field, kAXFocusedAttribute as CFString, &settable)
        try Self.requireResponsive(query, operation: "focusing the lock-screen authorization field")
        if query == .success, settable.boolValue {
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
    }

    private func confirm(_ field: AXUIElement, deadline: Date) throws {
        try Self.configureTimeout(field, deadline: deadline)
        try Self.before(deadline, operation: "reading lock-screen authorization actions")
        var values: CFArray?
        let copied = AXUIElementCopyActionNames(field, &values)
        try Self.requireResponsive(copied, operation: "reading lock-screen authorization actions")
        guard copied == .success, let actions = values as? [String] else {
            throw LockScreenAuthorizationError(
                "the macOS lock-screen authorization field exposes no actions")
        }
        // AXConfirm is the semantic action for submitting a text field. Some
        // macOS releases expose AXPress instead, so accept that on this exact
        // identifier only.
        let selected: String?
        if actions.contains(kAXConfirmAction as String) {
            selected = kAXConfirmAction as String
        } else if actions.contains(kAXPressAction as String) {
            selected = kAXPressAction as String
        } else {
            selected = nil
        }
        guard let selected else {
            throw LockScreenAuthorizationError(
                "the macOS lock-screen authorization field cannot be confirmed")
        }
        try Self.before(deadline, operation: "submitting lock-screen authorization")
        let result = AXUIElementPerformAction(field, selected as CFString)
        try Self.requireResponsive(result, operation: "submitting lock-screen authorization")
        guard result == .success else {
            throw LockScreenAuthorizationError(
                "macOS rejected the lock-screen authorization action (AX error \(result.rawValue))")
        }
    }

    private func prepareEmptySubmission(
        _ field: AXUIElement, deadline: Date
    ) throws {
        try Self.configureTimeout(field, deadline: deadline)
        try Self.before(
            deadline,
            operation: "checking whether the lock-screen authorization field accepts an empty value")
        var settable = DarwinBoolean(false)
        let query = AXUIElementIsAttributeSettable(
            field, kAXValueAttribute as CFString, &settable)
        try Self.requireEmptyValueWritable(
            queryStatus: query, isSettable: settable.boolValue)

        // This is a deliberately credential-free empty public AX assignment,
        // not a sentinel. Never copy/read the secure field's current value.
        // Although the resulting field value is empty, the assignment itself
        // may trigger authorization and therefore must never be retried.
        // If loginwindow cannot acknowledge this edit, do not perform either
        // AXConfirm or AXPress: the authorization request is not attributable.
        try Self.before(
            deadline, operation: "writing an empty lock-screen authorization value")
        let written = AXUIElementSetAttributeValue(
            field, kAXValueAttribute as CFString, "" as CFString)
        try Self.requireEmptyValueWritten(written)
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
