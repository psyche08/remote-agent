import AppKit
import ApplicationServices
import Security

/// Distinguishes a bounded AX transport/deadline failure from a semantic
/// request error. The controller uses this signal to close and relock an open
/// Locked Use window after an app stops responding.
struct AccessibilityIPCError: Error, CustomStringConvertible, Sendable {
    let message: String
    var description: String { message }
}

/// A live AX target failed the identity boundary configured on this device.
/// This is separate from a malformed selector: retrying against another
/// same-bundle process would be an authority change, so callers fail closed.
struct AccessibilityTargetError: Error, CustomStringConvertible, Sendable {
    let message: String
    var description: String { message }
}

/// Verifies the code object attached to the *running pid*. Verifying only the
/// app bundle on disk leaves a gap: AX might have selected a different process
/// with the same bundle identifier, or the selected process might have changed
/// between discovery and the mutation.
enum AccessibilityTargetCodeSigning {
    struct Identity: Equatable, Sendable {
        let identifier: String
        let teamIdentifier: String
        let codePath: String
    }

    static func permits(
        policy: Accessibility.TargetPolicy, identity: Identity
    ) -> Bool {
        identity.identifier == policy.bundleIdentifier &&
            identity.teamIdentifier == policy.teamIdentifier &&
            canonicalPath(identity.codePath) == canonicalPath(policy.appPath)
    }

    static func verify(
        processIdentifier: pid_t, policy: Accessibility.TargetPolicy
    ) throws {
        guard processIdentifier > 0 else {
            throw AccessibilityTargetError(message: "the target application pid is invalid")
        }

        let attributes = [
            kSecGuestAttributePid as String: NSNumber(value: processIdentifier)
        ] as CFDictionary
        var dynamicCode: SecCode?
        let guestStatus = SecCodeCopyGuestWithAttributes(
            nil, attributes, SecCSFlags(), &dynamicCode)
        guard guestStatus == errSecSuccess, let dynamicCode else {
            throw AccessibilityTargetError(
                message: "could not inspect the running target signature (status \(guestStatus))")
        }

        let requirement = try makeRequirement(policy: policy)
        let validity = SecCodeCheckValidity(
            dynamicCode, SecCSFlags(rawValue: kSecCSStrictValidate), requirement)
        guard validity == errSecSuccess else {
            throw AccessibilityTargetError(
                message: "the running target code signature does not match configuration")
        }

        var staticCode: SecStaticCode?
        guard SecCodeCopyStaticCode(
            dynamicCode, SecCSFlags(), &staticCode) == errSecSuccess,
            let staticCode
        else {
            throw AccessibilityTargetError(
                message: "could not resolve the running target code object")
        }

        var information: CFDictionary?
        let informationStatus = SecCodeCopySigningInformation(
            staticCode, SecCSFlags(rawValue: kSecCSSigningInformation), &information)
        var codeURL: CFURL?
        let pathStatus = SecCodeCopyPath(staticCode, SecCSFlags(), &codeURL)
        guard informationStatus == errSecSuccess,
              pathStatus == errSecSuccess,
              let values = information as? [String: Any],
              let identifier = values[kSecCodeInfoIdentifier as String] as? String,
              let teamIdentifier = values[kSecCodeInfoTeamIdentifier as String] as? String,
              let codeURL
        else {
            throw AccessibilityTargetError(
                message: "the running target signing identity is unavailable")
        }

        let identity = Identity(
            identifier: identifier,
            teamIdentifier: teamIdentifier,
            codePath: (codeURL as URL).path)
        guard permits(policy: policy, identity: identity) else {
            throw AccessibilityTargetError(
                message: "the running target bundle, team, or app path does not match configuration")
        }
    }

    static func canonicalPath(_ value: String) -> String {
        URL(fileURLWithPath: value)
            .resolvingSymlinksInPath()
            .standardizedFileURL.path
    }

    private static func makeRequirement(
        policy: Accessibility.TargetPolicy
    ) throws -> SecRequirement {
        let text = "identifier \"\(policy.bundleIdentifier)\" and anchor apple generic " +
            "and certificate leaf[subject.OU] = \"\(policy.teamIdentifier)\""
        var requirement: SecRequirement?
        let status = SecRequirementCreateWithString(
            text as CFString, SecCSFlags(), &requirement)
        guard status == errSecSuccess, let requirement else {
            throw AccessibilityTargetError(
                message: "could not compile the target signing requirement (status \(status))")
        }
        return requirement
    }
}

/// Drives applications through the Accessibility element tree.
///
/// This layer is an operation mechanism, not an unlock mechanism. On a normal
/// desktop it runs under the signed peer/turn lease; during Locked Use it runs
/// only after `LockedUseController` has completed the exact authorization
/// transaction and opened the owning turn's shielded window.
public enum Accessibility {
    /// AX is synchronous IPC into an arbitrary target process. A hung app must
    /// not hold a controller operation lease forever and thereby prevent
    /// relock/cleanup. Individual messages are bounded, and each public
    /// operation also has an end-to-end deadline below the agent's 25s budget.
    static let messagingTimeout: Float = 1.0
    private static let operationTimeout: TimeInterval = 20

    /// Immutable target identity loaded from the helper's own device config.
    /// The wire request may select one of these policies, but cannot supply or
    /// weaken the expected signing team or application path.
    public struct TargetPolicy: Equatable, Sendable {
        public let appName: String
        public let bundleIdentifier: String
        public let teamIdentifier: String
        public let appPath: String

        public init(
            appName: String, bundleIdentifier: String,
            teamIdentifier: String, appPath: String
        ) throws {
            let appName = appName.trimmingCharacters(in: .whitespacesAndNewlines)
            let bundleIdentifier = bundleIdentifier.trimmingCharacters(
                in: .whitespacesAndNewlines)
            let teamIdentifier = teamIdentifier.trimmingCharacters(
                in: .whitespacesAndNewlines)
            let appPath = appPath.trimmingCharacters(in: .whitespacesAndNewlines)
            let identityCharacters = CharacterSet(
                charactersIn: "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789.-")
            guard !appName.isEmpty,
                  !bundleIdentifier.isEmpty,
                  bundleIdentifier.unicodeScalars.allSatisfy(identityCharacters.contains),
                  teamIdentifier.count == 10,
                  teamIdentifier.unicodeScalars.allSatisfy({
                      CharacterSet.uppercaseLetters.contains($0) ||
                          CharacterSet.decimalDigits.contains($0)
                  }),
                  appPath.hasPrefix("/"),
                  URL(fileURLWithPath: appPath).pathExtension == "app"
            else {
                throw AccessibilityTargetError(
                    message: "the configured Accessibility target identity is invalid")
            }
            self.appName = appName
            self.bundleIdentifier = bundleIdentifier
            self.teamIdentifier = teamIdentifier
            self.appPath = URL(fileURLWithPath: appPath).standardizedFileURL.path
        }
    }

    /// Value-only process snapshot used by production selection and pure unit
    /// tests. AppKit object identity is carried separately for PID-reuse checks.
    struct RunningApplicationIdentity: Equatable, Sendable {
        let bundleIdentifier: String?
        let bundlePath: String?
        let isTerminated: Bool
        let processIdentifier: pid_t
    }

    fileprivate struct LocatedApplication {
        let application: NSRunningApplication
        let element: AXUIElement
        let processIdentifier: pid_t
        let policy: TargetPolicy?
    }

    /// A process-generation token retained by the helper between one protected
    /// `ax_read` and later mutations for the same turn. Keeping the AppKit
    /// object as well as the PID lets revalidation reject PID reuse.
    final class TargetPin: @unchecked Sendable {
        fileprivate let application: NSRunningApplication
        let processIdentifier: pid_t
        let policy: TargetPolicy

        fileprivate init(located: LocatedApplication, policy: TargetPolicy) {
            self.application = located.application
            self.processIdentifier = located.processIdentifier
            self.policy = policy
        }
    }

    static func requireResponsive(_ status: AXError, operation: String) throws {
        if status == .cannotComplete {
            throw AccessibilityIPCError(
                message: "\(operation) timed out because the target application is unresponsive")
        }
    }

    private static func before(_ deadline: Date, operation: String) throws {
        guard Date() <= deadline else {
            throw AccessibilityIPCError(
                message: "\(operation) exceeded the Accessibility operation deadline")
        }
    }

    private static func configureTimeout(
        _ element: AXUIElement, deadline: Date
    ) throws {
        try before(deadline, operation: "configuring Accessibility IPC")
        let status = AXUIElementSetMessagingTimeout(element, messagingTimeout)
        guard status == .success else {
            throw AccessibilityIPCError(
                message: "could not bound Accessibility IPC (AX status \(status.rawValue))")
        }
    }
    /// Whether this process is trusted for Accessibility. Without it every call
    /// here fails, which the caller must report as a permission gap rather than
    /// a broken feature.
    public static func isTrusted() -> Bool {
        AXIsProcessTrusted()
    }

    /// A located element and the bare facts a caller needs to decide on it.
    public struct Node: Sendable {
        public let role: String
        public let identifier: String?
        public let subrole: String?
        public let label: String
        public let value: String?
        public let enabled: Bool?
        public let selected: Bool?
        public let focused: Bool?
        /// The semantic state exposed by ARIA `aria-current`. `nil` means the
        /// attribute was not exposed; `false` must remain distinct from that
        /// absence so a caller can identify the one current session row.
        public let current: Bool?
        /// Semantic control states. These are populated only from the matching
        /// ARIA AX attributes, or from AXValue on checkbox/radio/toggle roles.
        public let checked: Bool?
        public let pressed: Bool?
        public let actionable: Bool
        public let path: [Int]
    }

    /// Selects one unique live application whose AppKit identity already
    /// matches the configured bundle and path. Signing is checked separately
    /// against the running pid, so neither source is trusted on its own.
    static func exactTargetApplication<Application>(
        from applications: [Application], policy: TargetPolicy,
        identity: (Application) -> RunningApplicationIdentity
    ) throws -> Application {
        let expectedPath = AccessibilityTargetCodeSigning.canonicalPath(policy.appPath)
        let matches = applications.filter {
            let value = identity($0)
            guard !value.isTerminated,
                  value.processIdentifier > 0,
                  value.bundleIdentifier == policy.bundleIdentifier,
                  let bundlePath = value.bundlePath
            else { return false }
            return AccessibilityTargetCodeSigning.canonicalPath(bundlePath) == expectedPath
        }
        guard !matches.isEmpty else {
            throw AccessibilityTargetError(
                message: "the exact live target application was not found")
        }
        guard matches.count == 1 else {
            throw AccessibilityTargetError(
                message: "multiple exact live target applications were found")
        }
        return matches[0]
    }

    /// Re-resolves the unique exact target immediately before a sensitive AX
    /// step. PID equality alone is insufficient because a restarted process
    /// can reuse a PID, hence the independent AppKit-instance predicate.
    @discardableResult
    static func requireSameExactTarget<Application>(
        _ expected: Application, from applications: [Application],
        policy: TargetPolicy,
        identity: (Application) -> RunningApplicationIdentity,
        isSameInstance: (Application, Application) -> Bool,
        verifySignature: (pid_t, TargetPolicy) throws -> Void
    ) throws -> Application {
        let current = try exactTargetApplication(
            from: applications, policy: policy, identity: identity)
        let expectedIdentity = identity(expected)
        let currentIdentity = identity(current)
        guard isSameInstance(expected, current),
              expectedIdentity.processIdentifier > 0,
              currentIdentity.processIdentifier == expectedIdentity.processIdentifier else {
            throw AccessibilityTargetError(
                message: "the target application process instance changed during the Accessibility operation")
        }
        try verifySignature(currentIdentity.processIdentifier, policy)
        return current
    }

    private static func runningApplicationIdentity(
        _ application: NSRunningApplication
    ) -> RunningApplicationIdentity {
        RunningApplicationIdentity(
            bundleIdentifier: application.bundleIdentifier,
            bundlePath: application.bundleURL?.path,
            isTerminated: application.isTerminated,
            processIdentifier: application.processIdentifier)
    }

    /// The application element for a single pinned running process. A protected
    /// target is selected by exact bundle/path and then verified as a dynamic
    /// code object for that pid before any AX attribute is touched.
    private static func appElement(
        bundleID: String?, name: String?, policy: TargetPolicy?,
        targetPin: TargetPin? = nil, deadline: Date
    ) throws -> LocatedApplication {
        let running = NSWorkspace.shared.runningApplications
        let match: NSRunningApplication
        if let policy {
            if let bundleID, !bundleID.isEmpty,
               bundleID != policy.bundleIdentifier {
                throw AccessibilityTargetError(
                    message: "the requested bundle does not match the configured target")
            }
            if let targetPin {
                guard targetPin.policy == policy else {
                    throw AccessibilityTargetError(
                        message: "the pinned target identity does not match this request")
                }
                match = try requireSameExactTarget(
                    targetPin.application, from: running, policy: policy,
                    identity: runningApplicationIdentity,
                    isSameInstance: { $0.isEqual($1) },
                    verifySignature: AccessibilityTargetCodeSigning.verify)
                guard match.processIdentifier == targetPin.processIdentifier else {
                    throw AccessibilityTargetError(
                        message: "the pinned target pid changed between Accessibility requests")
                }
            } else {
                match = try exactTargetApplication(
                    from: running, policy: policy,
                    identity: runningApplicationIdentity)
                try AccessibilityTargetCodeSigning.verify(
                    processIdentifier: match.processIdentifier, policy: policy)
            }
        } else {
            // A bundle identifier is the stable, unambiguous address. When both
            // fields are present it must win rather than being ORed with a
            // display name: otherwise a stale name can select a different app.
            guard let selected = running.first(where: {
                selectsApplication(
                    candidateBundleID: $0.bundleIdentifier,
                    candidateName: $0.localizedName,
                    requestedBundleID: bundleID, requestedName: name)
            }) else {
                throw ActionError("no running application matches the given app")
            }
            match = selected
        }

        guard !match.isTerminated, match.processIdentifier > 0 else {
            throw AccessibilityTargetError(
                message: "the selected target application is no longer running")
        }
        let app = AXUIElementCreateApplication(match.processIdentifier)
        try configureTimeout(app, deadline: deadline)
        try requireProcessIdentifier(
            app, expected: match.processIdentifier, deadline: deadline)
        return LocatedApplication(
            application: match, element: app,
            processIdentifier: match.processIdentifier, policy: policy)
    }

    /// Re-checks both the selected AppKit process instance and its AX root. For
    /// protected targets this also re-evaluates the dynamic code signature.
    private static func revalidate(
        _ located: LocatedApplication, deadline: Date
    ) throws {
        try before(deadline, operation: "verifying the Accessibility target")
        let running = NSWorkspace.shared.runningApplications
        if let policy = located.policy {
            _ = try requireSameExactTarget(
                located.application, from: running, policy: policy,
                identity: runningApplicationIdentity,
                isSameInstance: { $0.isEqual($1) },
                verifySignature: AccessibilityTargetCodeSigning.verify)
        } else {
            guard let current = running.first(where: {
                $0.isEqual(located.application) &&
                    !$0.isTerminated &&
                    $0.processIdentifier == located.processIdentifier
            }) else {
                throw AccessibilityTargetError(
                    message: "the target application process instance changed during the Accessibility operation")
            }
            guard selectsApplication(
                candidateBundleID: current.bundleIdentifier,
                candidateName: current.localizedName,
                requestedBundleID: located.application.bundleIdentifier,
                requestedName: located.application.localizedName)
            else {
                throw AccessibilityTargetError(
                    message: "the target application identity changed during the Accessibility operation")
            }
        }
        try requireProcessIdentifier(
            located.element, expected: located.processIdentifier, deadline: deadline)
    }

    private static func requireProcessIdentifier(
        _ element: AXUIElement, expected: pid_t, deadline: Date
    ) throws {
        try configureTimeout(element, deadline: deadline)
        try before(deadline, operation: "verifying Accessibility element ownership")
        var processIdentifier: pid_t = 0
        let status = AXUIElementGetPid(element, &processIdentifier)
        try requireResponsive(status, operation: "verifying Accessibility element ownership")
        guard status == .success,
              expected > 0,
              processIdentifier == expected else {
            throw AccessibilityTargetError(
                message: "the Accessibility element is not owned by the pinned target process")
        }
    }

    /// Pure routing seam: a supplied bundle id has strict priority over a
    /// display name. Internal so tests can lock down that security-relevant
    /// choice without launching extra applications.
    static func selectsApplication(
        candidateBundleID: String?, candidateName: String?,
        requestedBundleID: String?, requestedName: String?
    ) -> Bool {
        if let requestedBundleID, !requestedBundleID.isEmpty {
            return candidateBundleID == requestedBundleID
        }
        if let requestedName, !requestedName.isEmpty {
            return candidateName == requestedName
        }
        return false
    }

    private static func attribute(
        _ element: AXUIElement, _ name: String, deadline: Date
    ) throws -> CFTypeRef? {
        try configureTimeout(element, deadline: deadline)
        try before(deadline, operation: "reading an Accessibility attribute")
        var ref: CFTypeRef?
        let status = AXUIElementCopyAttributeValue(element, name as CFString, &ref)
        try requireResponsive(status, operation: "reading an Accessibility attribute")
        guard status == .success else {
            return nil
        }
        return ref
    }

    private static func string(
        _ element: AXUIElement, _ attribute: String, deadline: Date
    ) throws -> String? {
        try self.attribute(element, attribute, deadline: deadline) as? String
    }

    private static func boolean(
        _ element: AXUIElement, _ attribute: String, deadline: Date
    ) throws -> Bool? {
        try self.attribute(element, attribute, deadline: deadline) as? Bool
    }

    /// Chromium exposes ARIA `aria-current` as AXARIACurrent. Depending on the
    /// renderer/SDK, the value may be a Boolean or an ARIA token (`page`,
    /// `step`, `true`, and so on). Per ARIA semantics only the token `false`
    /// (and an empty token) is false; any other non-empty token is current.
    /// Keep this conversion narrow: it does not inspect arbitrary DOM data
    /// attributes or infer current state from labels/classes.
    static func ariaCurrent(_ value: Any?) -> Bool? {
        guard let value else { return nil }
        if let current = value as? Bool {
            return current
        }
        if let token = value as? String {
            let normalized = token.trimmingCharacters(in: .whitespacesAndNewlines).lowercased()
            return !normalized.isEmpty && normalized != "false"
        }
        return nil
    }

    /// Normalizes only binary AX control state. `mixed` and non-binary numbers
    /// remain unknown rather than being treated as selected and accidentally
    /// toggled by a provider adapter.
    static func binaryControlState(_ value: Any?) -> Bool? {
        guard let value else { return nil }
        if let state = value as? Bool {
            return state
        }
        if let number = value as? NSNumber {
            let numeric = number.doubleValue
            if numeric == 0 { return false }
            if numeric == 1 { return true }
            return nil
        }
        if let token = value as? String {
            switch token.trimmingCharacters(in: .whitespacesAndNewlines).lowercased() {
            case "false": return false
            case "true": return true
            default: return nil
            }
        }
        return nil
    }

    /// Pure role-gated seam for deriving checked/pressed. An ARIA attribute,
    /// when present, is authoritative even if it contains an unsupported state
    /// such as `mixed`; AXValue fallback is intentionally limited to native
    /// roles whose value actually represents that state.
    static func controlStates(
        role: String, subrole: String?, value: Any?,
        ariaChecked: Any?, ariaPressed: Any?
    ) -> (checked: Bool?, pressed: Bool?) {
        let checked: Bool?
        if ariaChecked != nil {
            checked = binaryControlState(ariaChecked)
        } else if role == kAXCheckBoxRole as String ||
                    role == kAXRadioButtonRole as String {
            checked = binaryControlState(value)
        } else {
            checked = nil
        }

        let pressed: Bool?
        if ariaPressed != nil {
            pressed = binaryControlState(ariaPressed)
        } else if subrole == kAXToggleSubrole as String {
            pressed = binaryControlState(value)
        } else {
            pressed = nil
        }
        return (checked, pressed)
    }

    private static func children(
        _ element: AXUIElement, deadline: Date
    ) throws -> [AXUIElement] {
        try configureTimeout(element, deadline: deadline)
        try before(deadline, operation: "reading Accessibility children")
        var ref: CFTypeRef?
        let status = AXUIElementCopyAttributeValue(
            element, kAXChildrenAttribute as CFString, &ref)
        try requireResponsive(status, operation: "reading Accessibility children")
        guard status == .success else { return [] }
        return (ref as? [AXUIElement]) ?? []
    }

    private static func actionNames(
        _ element: AXUIElement, deadline: Date
    ) throws -> [String] {
        try configureTimeout(element, deadline: deadline)
        try before(deadline, operation: "reading Accessibility actions")
        var ref: CFArray?
        let status = AXUIElementCopyActionNames(element, &ref)
        try requireResponsive(status, operation: "reading Accessibility actions")
        guard status == .success else { return [] }
        return (ref as? [String]) ?? []
    }

    /// A label for an element, from whichever attribute carries a human string.
    private static func label(_ element: AXUIElement, deadline: Date) throws -> String {
        for attribute in [kAXTitleAttribute, kAXDescriptionAttribute,
                          kAXValueAttribute as CFString as String] {
            if let s = try string(element, attribute, deadline: deadline), !s.isEmpty {
                return s
            }
        }
        return ""
    }

    private static let maxDepth = 40
    private static let maxNodes = 2000

    /// Enumerates the actionable and labelled elements of an app, breadth first,
    /// bounded so a deep or cyclic tree cannot run away.
    /// Electron/Chromium apps expose their web content to Accessibility only
    /// after a client sets AXManualAccessibility on the application element —
    /// otherwise the tree is just the native menu bar. Setting it is how a
    /// screen reader (and the reference implementation) makes an Electron app's
    /// buttons and fields addressable. Harmless if the app is not Electron.
    private static func enableWebContent(
        _ app: AXUIElement, expectedProcessIdentifier: pid_t, deadline: Date
    ) throws {
        // Set on the application and on every window: Chromium exposes a
        // window's web content only when the flag is set on that window
        // element, not merely on the application. Both attributes are the
        // documented switches a screen reader uses; harmless on non-Electron
        // apps, which simply ignore them.
        for attribute in ["AXManualAccessibility", "AXEnhancedUserInterface"] {
            try requireProcessIdentifier(
                app, expected: expectedProcessIdentifier, deadline: deadline)
            try configureTimeout(app, deadline: deadline)
            try before(deadline, operation: "enabling Accessibility web content")
            let status = AXUIElementSetAttributeValue(
                app, attribute as CFString, kCFBooleanTrue)
            try requireResponsive(status, operation: "enabling Accessibility web content")
        }
        var windowsRef: CFTypeRef?
        try before(deadline, operation: "reading Accessibility windows")
        let windowsStatus = AXUIElementCopyAttributeValue(
            app, kAXWindowsAttribute as CFString, &windowsRef)
        try requireResponsive(windowsStatus, operation: "reading Accessibility windows")
        if windowsStatus == .success,
           let windows = windowsRef as? [AXUIElement] {
            for window in windows {
                try requireProcessIdentifier(
                    window, expected: expectedProcessIdentifier, deadline: deadline)
                try configureTimeout(window, deadline: deadline)
                try before(deadline, operation: "enabling window web content")
                let status = AXUIElementSetAttributeValue(
                    window, "AXManualAccessibility" as CFString, kCFBooleanTrue)
                try requireResponsive(status, operation: "enabling window web content")
            }
        }
    }

    public static func read(
        bundleID: String?, name: String?, policy: TargetPolicy? = nil
    ) throws -> [Node] {
        try readPinned(bundleID: bundleID, name: name, policy: policy).nodes
    }

    /// Returns the process-generation pin created by the same fully verified
    /// read that produced these index paths. The router retains it under the
    /// authoritative turn id; it is never serialized to or accepted from the
    /// socket peer.
    static func readPinned(
        bundleID: String?, name: String?, policy: TargetPolicy? = nil
    ) throws -> (nodes: [Node], targetPin: TargetPin?) {
        let deadline = Date().addingTimeInterval(operationTimeout)
        let located = try appElement(
            bundleID: bundleID, name: name, policy: policy, deadline: deadline)
        try revalidate(located, deadline: deadline)
        try enableWebContent(
            located.element, expectedProcessIdentifier: located.processIdentifier,
            deadline: deadline)
        try revalidate(located, deadline: deadline)
        var out: [Node] = []
        var stack: [(AXUIElement, [Int])] = [(located.element, [])]
        var visited = 0
        // AX trees contain cycles: a child can reference an ancestor (an
        // Electron app's window points back at the application element), so an
        // un-deduped walk spends its whole budget revisiting the same nodes on
        // ever-growing paths — [0], [0,0], [0,0,0]… — and the real window
        // content is pushed past the node cap. CFEqual identity dedup is what
        // keeps the walk on distinct elements.
        var seen: [AXUIElement] = []
        while let (element, path) = stack.first {
            try before(deadline, operation: "reading the Accessibility tree")
            stack.removeFirst()
            if seen.contains(where: { CFEqual($0, element) }) { continue }
            seen.append(element)
            try requireProcessIdentifier(
                element, expected: located.processIdentifier, deadline: deadline)
            visited += 1
            if visited > maxNodes { break }
            if path.count > maxDepth { continue }
            let role = try string(
                element, kAXRoleAttribute, deadline: deadline) ?? ""
            let identifier = try string(
                element, kAXIdentifierAttribute, deadline: deadline)
            let subrole = try string(
                element, kAXSubroleAttribute, deadline: deadline)
            let text = try label(element, deadline: deadline)
            let rawValue = try attribute(
                element, kAXValueAttribute as CFString as String,
                deadline: deadline)
            let value = rawValue as? String
            let enabled = try boolean(
                element, kAXEnabledAttribute, deadline: deadline)
            let selected = try boolean(
                element, kAXSelectedAttribute, deadline: deadline)
            let focused = try boolean(
                element, kAXFocusedAttribute, deadline: deadline)
            // `kAXARIACurrentAttribute` is not present in every SDK supported
            // by this package. Its stable AX wire name is used directly so the
            // same signed helper can build with those SDKs.
            let current = ariaCurrent(try attribute(
                element, "AXARIACurrent", deadline: deadline))
            // Like AXARIACurrent, these web attributes are stable AX wire
            // names but are absent from some SDK headers supported by AgentHalo.
            let ariaChecked = try attribute(
                element, "AXARIAChecked", deadline: deadline)
            let ariaPressed = try attribute(
                element, "AXARIAPressed", deadline: deadline)
            let states = controlStates(
                role: role, subrole: subrole, value: rawValue,
                ariaChecked: ariaChecked, ariaPressed: ariaPressed)
            let actions = try actionNames(element, deadline: deadline)
            let actionable = actions.contains(kAXPressAction)
            // Blank composers and other editable controls still need a stable
            // path so a caller can focus or populate them. Do not include every
            // enabled container: Chromium trees are large and the traversal is
            // deliberately bounded.
            let addressableRole = [
                kAXTextFieldRole as String,
                kAXTextAreaRole as String,
                kAXComboBoxRole as String,
                kAXCheckBoxRole as String,
                kAXRadioButtonRole as String,
            ].contains(role)
            if !text.isEmpty || actionable || identifier?.isEmpty == false ||
                addressableRole || selected == true || focused == true || current == true ||
                states.checked != nil || states.pressed != nil {
                out.append(Node(
                    role: role, identifier: identifier, subrole: subrole,
                    label: text, value: value,
                    enabled: enabled, selected: selected, focused: focused, current: current,
                    checked: states.checked, pressed: states.pressed,
                    actionable: actionable, path: path))
            }
            for (index, child) in try children(element, deadline: deadline).enumerated() {
                stack.append((child, path + [index]))
            }
        }
        try revalidate(located, deadline: deadline)
        return (
            out,
            policy.map { TargetPin(located: located, policy: $0) })
    }

    /// Resolves an element by the index path `read` reports, so an action names
    /// exactly the element the caller saw and nothing shifts under it.
    static func validatePath(_ path: [Int]) throws {
        guard path.count <= maxDepth else {
            throw ActionError("the element path exceeds the maximum depth")
        }
        guard path.allSatisfy({ $0 >= 0 }) else {
            throw ActionError("the element path contains a negative index")
        }
    }

    private static func resolve(
        _ app: AXUIElement, path: [Int], expectedProcessIdentifier: pid_t,
        deadline: Date
    ) throws -> AXUIElement {
        try validatePath(path)
        var element = app
        try requireProcessIdentifier(
            element, expected: expectedProcessIdentifier, deadline: deadline)
        for index in path {
            let kids = try children(element, deadline: deadline)
            // A negative index satisfies `index < kids.count` and then traps in
            // Array's subscript. Since paths arrive over the socket, validate
            // both bounds before touching the array.
            guard index < kids.count else {
                throw ActionError("the element path no longer resolves; re-read the tree")
            }
            element = kids[index]
            try requireProcessIdentifier(
                element, expected: expectedProcessIdentifier, deadline: deadline)
        }
        return element
    }

    /// Performs an element's press action — the click without a pointer.
    public static func press(
        bundleID: String?, name: String?, path: [Int],
        policy: TargetPolicy? = nil
    ) throws {
        try pressPinned(
            bundleID: bundleID, name: name, path: path,
            policy: policy, targetPin: nil)
    }

    static func pressPinned(
        bundleID: String?, name: String?, path: [Int],
        policy: TargetPolicy? = nil, targetPin: TargetPin? = nil
    ) throws {
        if policy != nil, targetPin == nil {
            throw AccessibilityTargetError(
                message: "a protected Accessibility mutation requires a fresh turn-bound read")
        }
        let deadline = Date().addingTimeInterval(operationTimeout)
        let located = try appElement(
            bundleID: bundleID, name: name, policy: policy,
            targetPin: targetPin, deadline: deadline)
        try revalidate(located, deadline: deadline)
        try enableWebContent(
            located.element, expectedProcessIdentifier: located.processIdentifier,
            deadline: deadline)
        try revalidate(located, deadline: deadline)
        let element = try resolve(
            located.element, path: path,
            expectedProcessIdentifier: located.processIdentifier, deadline: deadline)
        guard try actionNames(element, deadline: deadline).contains(kAXPressAction) else {
            throw ActionError("the element does not accept a press")
        }
        try revalidate(located, deadline: deadline)
        try requireProcessIdentifier(
            element, expected: located.processIdentifier, deadline: deadline)
        try configureTimeout(element, deadline: deadline)
        try before(deadline, operation: "pressing an Accessibility element")
        let status = AXUIElementPerformAction(element, kAXPressAction as CFString)
        try requireResponsive(status, operation: "pressing an Accessibility element")
        guard status == .success else {
            throw ActionError("press failed (AX status \(status.rawValue))")
        }
        try revalidate(located, deadline: deadline)
    }

    /// Sets an element's value — the text field fill without keystrokes.
    public static func setValue(
        bundleID: String?, name: String?, path: [Int], value: String,
        policy: TargetPolicy? = nil
    ) throws {
        try setValuePinned(
            bundleID: bundleID, name: name, path: path, value: value,
            policy: policy, targetPin: nil)
    }

    static func setValuePinned(
        bundleID: String?, name: String?, path: [Int], value: String,
        policy: TargetPolicy? = nil, targetPin: TargetPin? = nil
    ) throws {
        if policy != nil, targetPin == nil {
            throw AccessibilityTargetError(
                message: "a protected Accessibility mutation requires a fresh turn-bound read")
        }
        let deadline = Date().addingTimeInterval(operationTimeout)
        let located = try appElement(
            bundleID: bundleID, name: name, policy: policy,
            targetPin: targetPin, deadline: deadline)
        try revalidate(located, deadline: deadline)
        try enableWebContent(
            located.element, expectedProcessIdentifier: located.processIdentifier,
            deadline: deadline)
        try revalidate(located, deadline: deadline)
        let element = try resolve(
            located.element, path: path,
            expectedProcessIdentifier: located.processIdentifier, deadline: deadline)
        var settable: DarwinBoolean = false
        try configureTimeout(element, deadline: deadline)
        try before(deadline, operation: "checking an Accessibility value")
        let query = AXUIElementIsAttributeSettable(
            element, kAXValueAttribute as CFString, &settable)
        try requireResponsive(query, operation: "checking an Accessibility value")
        guard query == .success,
              settable.boolValue else {
            throw ActionError("the element's value is not settable")
        }
        try revalidate(located, deadline: deadline)
        try requireProcessIdentifier(
            element, expected: located.processIdentifier, deadline: deadline)
        try before(deadline, operation: "setting an Accessibility value")
        let status = AXUIElementSetAttributeValue(
            element, kAXValueAttribute as CFString, value as CFTypeRef)
        try requireResponsive(status, operation: "setting an Accessibility value")
        guard status == .success else {
            throw ActionError("setting the value failed (AX status \(status.rawValue))")
        }
        try revalidate(located, deadline: deadline)
    }

    /// Focuses one exact element resolved from the latest tree. This is kept as
    /// an internal desktop primitive rather than a model-facing tool: focus is a
    /// prerequisite for a bounded provider adapter, not authority to send keys.
    public static func focus(
        bundleID: String?, name: String?, path: [Int],
        policy: TargetPolicy? = nil
    ) throws {
        try focusPinned(
            bundleID: bundleID, name: name, path: path,
            policy: policy, targetPin: nil)
    }

    static func focusPinned(
        bundleID: String?, name: String?, path: [Int],
        policy: TargetPolicy? = nil, targetPin: TargetPin? = nil
    ) throws {
        if policy != nil, targetPin == nil {
            throw AccessibilityTargetError(
                message: "a protected Accessibility mutation requires a fresh turn-bound read")
        }
        let deadline = Date().addingTimeInterval(operationTimeout)
        let located = try appElement(
            bundleID: bundleID, name: name, policy: policy,
            targetPin: targetPin, deadline: deadline)
        try revalidate(located, deadline: deadline)
        try enableWebContent(
            located.element, expectedProcessIdentifier: located.processIdentifier,
            deadline: deadline)
        try revalidate(located, deadline: deadline)
        let element = try resolve(
            located.element, path: path,
            expectedProcessIdentifier: located.processIdentifier, deadline: deadline)
        var settable: DarwinBoolean = false
        try configureTimeout(element, deadline: deadline)
        try before(deadline, operation: "checking Accessibility focus")
        let query = AXUIElementIsAttributeSettable(
            element, kAXFocusedAttribute as CFString, &settable)
        try requireResponsive(query, operation: "checking Accessibility focus")
        guard query == .success, settable.boolValue else {
            throw ActionError("the element cannot be focused")
        }
        try revalidate(located, deadline: deadline)
        try requireProcessIdentifier(
            element, expected: located.processIdentifier, deadline: deadline)
        try before(deadline, operation: "focusing an Accessibility element")
        let status = AXUIElementSetAttributeValue(
            element, kAXFocusedAttribute as CFString, kCFBooleanTrue)
        try requireResponsive(status, operation: "focusing an Accessibility element")
        guard status == .success else {
            throw ActionError("focusing the element failed (AX status \(status.rawValue))")
        }
        guard try boolean(element, kAXFocusedAttribute, deadline: deadline) == true else {
            throw ActionError("the element did not confirm focus")
        }
        try revalidate(located, deadline: deadline)
    }
}
