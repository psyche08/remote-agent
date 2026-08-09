import AppKit
import ApplicationServices

/// Drives applications through the Accessibility element tree.
///
/// This is the channel that actually satisfies "operate while the screen is
/// locked". Synthetic HID events and window-server (CGEvent) posts are
/// partitioned away from a locked session — measured on device, they never
/// reach the desktop while locked. The Accessibility API reaches an app's UI
/// element tree in its own process, which the lock screen does not partition:
/// with the screen locked, `AXUIElementCopyAttributeValue` returned CatDesk's
/// window title and children.
///
/// So the lock screen stays a *visual* barrier — a bystander sees the lock, and
/// the shield covers the desktop — while the agent reads and drives the UI
/// underneath it. Nothing here unlocks anything, injects any input, or bypasses
/// a security boundary; it is the ordinary Accessibility automation macOS
/// grants to a process the user has authorized, which the resident helper is.
///
/// The reference implementation (OpenAI's Codex Computer Use) works the same
/// way: its binaries import no event-posting API and hold no HID entitlement —
/// only Accessibility — and drive apps by element, which is why it too operates
/// against a locked screen without unlocking it.
public enum Accessibility {
    /// Whether this process is trusted for Accessibility. Without it every call
    /// here fails, which the caller must report as a permission gap rather than
    /// a broken feature.
    public static func isTrusted() -> Bool {
        AXIsProcessTrusted()
    }

    /// A located element and the bare facts a caller needs to decide on it.
    public struct Node: Sendable {
        public let role: String
        public let label: String
        public let value: String?
        public let actionable: Bool
        public let path: [Int]
    }

    /// The application element for a running app chosen by bundle id or name.
    private static func appElement(bundleID: String?, name: String?) throws -> AXUIElement {
        let running = NSWorkspace.shared.runningApplications
        let match = running.first { app in
            if let bundleID, app.bundleIdentifier == bundleID { return true }
            if let name, app.localizedName == name { return true }
            return false
        }
        guard let match else {
            throw ActionError("no running application matches the given app")
        }
        return AXUIElementCreateApplication(match.processIdentifier)
    }

    private static func string(_ element: AXUIElement, _ attribute: String) -> String? {
        var ref: CFTypeRef?
        guard AXUIElementCopyAttributeValue(element, attribute as CFString, &ref) == .success else {
            return nil
        }
        return ref as? String
    }

    private static func children(_ element: AXUIElement) -> [AXUIElement] {
        var ref: CFTypeRef?
        guard AXUIElementCopyAttributeValue(element, kAXChildrenAttribute as CFString, &ref) == .success
        else { return [] }
        return (ref as? [AXUIElement]) ?? []
    }

    private static func actionNames(_ element: AXUIElement) -> [String] {
        var ref: CFArray?
        guard AXUIElementCopyActionNames(element, &ref) == .success else { return [] }
        return (ref as? [String]) ?? []
    }

    /// A label for an element, from whichever attribute carries a human string.
    private static func label(_ element: AXUIElement) -> String {
        for attribute in [kAXTitleAttribute, kAXDescriptionAttribute,
                          kAXValueAttribute as CFString as String] {
            if let s = string(element, attribute), !s.isEmpty { return s }
        }
        return ""
    }

    private static let maxDepth = 40
    private static let maxNodes = 2000

    /// Enumerates the actionable and labelled elements of an app, breadth first,
    /// bounded so a deep or cyclic tree cannot run away.
    public static func read(bundleID: String?, name: String?) throws -> [Node] {
        let app = try appElement(bundleID: bundleID, name: name)
        var out: [Node] = []
        var stack: [(AXUIElement, [Int])] = [(app, [])]
        var visited = 0
        while let (element, path) = stack.first {
            stack.removeFirst()
            visited += 1
            if visited > maxNodes { break }
            if path.count > maxDepth { continue }
            let role = string(element, kAXRoleAttribute) ?? ""
            let text = label(element)
            let actions = actionNames(element)
            let actionable = actions.contains(kAXPressAction)
            if !text.isEmpty || actionable {
                out.append(Node(
                    role: role, label: text,
                    value: string(element, kAXValueAttribute as CFString as String),
                    actionable: actionable, path: path))
            }
            for (index, child) in children(element).enumerated() {
                stack.append((child, path + [index]))
            }
        }
        return out
    }

    /// Resolves an element by the index path `read` reports, so an action names
    /// exactly the element the caller saw and nothing shifts under it.
    private static func resolve(_ app: AXUIElement, path: [Int]) throws -> AXUIElement {
        var element = app
        for index in path {
            let kids = children(element)
            guard index < kids.count else {
                throw ActionError("the element path no longer resolves; re-read the tree")
            }
            element = kids[index]
        }
        return element
    }

    /// Performs an element's press action — the click without a pointer.
    public static func press(bundleID: String?, name: String?, path: [Int]) throws {
        let app = try appElement(bundleID: bundleID, name: name)
        let element = try resolve(app, path: path)
        guard actionNames(element).contains(kAXPressAction) else {
            throw ActionError("the element does not accept a press")
        }
        let status = AXUIElementPerformAction(element, kAXPressAction as CFString)
        guard status == .success else {
            throw ActionError("press failed (AX status \(status.rawValue))")
        }
    }

    /// Sets an element's value — the text field fill without keystrokes.
    public static func setValue(
        bundleID: String?, name: String?, path: [Int], value: String
    ) throws {
        let app = try appElement(bundleID: bundleID, name: name)
        let element = try resolve(app, path: path)
        var settable: DarwinBoolean = false
        guard AXUIElementIsAttributeSettable(
                element, kAXValueAttribute as CFString, &settable) == .success,
              settable.boolValue else {
            throw ActionError("the element's value is not settable")
        }
        let status = AXUIElementSetAttributeValue(
            element, kAXValueAttribute as CFString, value as CFTypeRef)
        guard status == .success else {
            throw ActionError("setting the value failed (AX status \(status.rawValue))")
        }
    }
}
