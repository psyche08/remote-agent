import AppKit
import ApplicationServices

/// Distinguishes a bounded AX transport/deadline failure from a semantic
/// request error. The controller uses this signal to close and relock an open
/// Locked Use window after an app stops responding.
struct AccessibilityIPCError: Error, CustomStringConvertible, Sendable {
    let message: String
    var description: String { message }
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

    /// The application element for a running app chosen by bundle id or name.
    private static func appElement(
        bundleID: String?, name: String?, deadline: Date
    ) throws -> AXUIElement {
        let running = NSWorkspace.shared.runningApplications
        // A bundle identifier is the stable, unambiguous address. When both
        // fields are present it must win rather than being ORed with a display
        // name: otherwise a stale name can select a different app before the
        // requested bundle appears in `runningApplications`.
        let match = running.first {
            selectsApplication(
                candidateBundleID: $0.bundleIdentifier, candidateName: $0.localizedName,
                requestedBundleID: bundleID, requestedName: name)
        }
        guard let match else {
            throw ActionError("no running application matches the given app")
        }
        let app = AXUIElementCreateApplication(match.processIdentifier)
        try configureTimeout(app, deadline: deadline)
        return app
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
    private static func enableWebContent(_ app: AXUIElement, deadline: Date) throws {
        // Set on the application and on every window: Chromium exposes a
        // window's web content only when the flag is set on that window
        // element, not merely on the application. Both attributes are the
        // documented switches a screen reader uses; harmless on non-Electron
        // apps, which simply ignore them.
        for attribute in ["AXManualAccessibility", "AXEnhancedUserInterface"] {
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
                try configureTimeout(window, deadline: deadline)
                try before(deadline, operation: "enabling window web content")
                let status = AXUIElementSetAttributeValue(
                    window, "AXManualAccessibility" as CFString, kCFBooleanTrue)
                try requireResponsive(status, operation: "enabling window web content")
            }
        }
    }

    public static func read(bundleID: String?, name: String?) throws -> [Node] {
        let deadline = Date().addingTimeInterval(operationTimeout)
        let app = try appElement(bundleID: bundleID, name: name, deadline: deadline)
        try enableWebContent(app, deadline: deadline)
        var out: [Node] = []
        var stack: [(AXUIElement, [Int])] = [(app, [])]
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
        return out
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
        _ app: AXUIElement, path: [Int], deadline: Date
    ) throws -> AXUIElement {
        try validatePath(path)
        var element = app
        for index in path {
            let kids = try children(element, deadline: deadline)
            // A negative index satisfies `index < kids.count` and then traps in
            // Array's subscript. Since paths arrive over the socket, validate
            // both bounds before touching the array.
            guard index < kids.count else {
                throw ActionError("the element path no longer resolves; re-read the tree")
            }
            element = kids[index]
        }
        return element
    }

    /// Performs an element's press action — the click without a pointer.
    public static func press(bundleID: String?, name: String?, path: [Int]) throws {
        let deadline = Date().addingTimeInterval(operationTimeout)
        let app = try appElement(bundleID: bundleID, name: name, deadline: deadline)
        try enableWebContent(app, deadline: deadline)
        let element = try resolve(app, path: path, deadline: deadline)
        guard try actionNames(element, deadline: deadline).contains(kAXPressAction) else {
            throw ActionError("the element does not accept a press")
        }
        try configureTimeout(element, deadline: deadline)
        try before(deadline, operation: "pressing an Accessibility element")
        let status = AXUIElementPerformAction(element, kAXPressAction as CFString)
        try requireResponsive(status, operation: "pressing an Accessibility element")
        guard status == .success else {
            throw ActionError("press failed (AX status \(status.rawValue))")
        }
    }

    /// Sets an element's value — the text field fill without keystrokes.
    public static func setValue(
        bundleID: String?, name: String?, path: [Int], value: String
    ) throws {
        let deadline = Date().addingTimeInterval(operationTimeout)
        let app = try appElement(bundleID: bundleID, name: name, deadline: deadline)
        try enableWebContent(app, deadline: deadline)
        let element = try resolve(app, path: path, deadline: deadline)
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
        try before(deadline, operation: "setting an Accessibility value")
        let status = AXUIElementSetAttributeValue(
            element, kAXValueAttribute as CFString, value as CFTypeRef)
        try requireResponsive(status, operation: "setting an Accessibility value")
        guard status == .success else {
            throw ActionError("setting the value failed (AX status \(status.rawValue))")
        }
    }

    /// Focuses one exact element resolved from the latest tree. This is kept as
    /// an internal desktop primitive rather than a model-facing tool: focus is a
    /// prerequisite for a bounded provider adapter, not authority to send keys.
    public static func focus(bundleID: String?, name: String?, path: [Int]) throws {
        let deadline = Date().addingTimeInterval(operationTimeout)
        let app = try appElement(bundleID: bundleID, name: name, deadline: deadline)
        try enableWebContent(app, deadline: deadline)
        let element = try resolve(app, path: path, deadline: deadline)
        var settable: DarwinBoolean = false
        try configureTimeout(element, deadline: deadline)
        try before(deadline, operation: "checking Accessibility focus")
        let query = AXUIElementIsAttributeSettable(
            element, kAXFocusedAttribute as CFString, &settable)
        try requireResponsive(query, operation: "checking Accessibility focus")
        guard query == .success, settable.boolValue else {
            throw ActionError("the element cannot be focused")
        }
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
    }
}
