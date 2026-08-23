import Foundation

/// The device's own config file, as written for the agent.
///
/// The helper reads this itself rather than being told what to do over the
/// socket. That is the whole point: Locked Use lets a machine unlock itself, so
/// the capability has to be granted on the device. If configuration arrived
/// over the wire, every process that could reach the socket could turn it on,
/// and "config is the ceiling" would mean nothing.
///
/// Unknown keys are ignored, so this reads the agent's full config.json
/// without having to model the rest of it.
public struct AgentConfigFile: Decodable, Sendable {
    public let deviceID: String
    public let computerUse: ComputerUseConfig?
    public let providers: [String: DesktopProviderConfig]

    enum CodingKeys: String, CodingKey {
        case deviceID = "device_id"
        case computerUse = "computer_use"
        case providers
    }

    public init(
        deviceID: String, computerUse: ComputerUseConfig?,
        providers: [String: DesktopProviderConfig] = [:]
    ) {
        self.deviceID = deviceID
        self.computerUse = computerUse
        self.providers = providers
    }

    public init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        deviceID = (try? container.decode(String.self, forKey: .deviceID)) ?? ""
        computerUse = try? container.decode(ComputerUseConfig.self, forKey: .computerUse)
        providers = (try? container.decode(
            [String: DesktopProviderConfig].self, forKey: .providers)) ?? [:]
    }

    /// Exact on-device identity policy for Claude Desktop Accessibility. The
    /// Go provider uses the same defaults, so even a compact config that omits
    /// `providers.claude` cannot silently downgrade the helper to bundle-id-
    /// only selection. Explicit values may replace the defaults, but malformed
    /// identities fail helper startup instead of becoming an unverified route.
    public func accessibilityTargetPolicies() throws -> [Accessibility.TargetPolicy] {
        let claude = providers["claude"]
        return [try Accessibility.TargetPolicy(
            appName: Self.nonEmpty(claude?.appName) ?? "Claude",
            bundleIdentifier: Self.nonEmpty(claude?.desktopBundleID)
                ?? "com.anthropic.claudefordesktop",
            teamIdentifier: Self.nonEmpty(claude?.desktopTeamID) ?? "Q6L2SF6YDW",
            appPath: Self.nonEmpty(claude?.desktopAppPath) ?? "/Applications/Claude.app")]
    }

    private static func nonEmpty(_ value: String?) -> String? {
        guard let value else { return nil }
        let trimmed = value.trimmingCharacters(in: .whitespacesAndNewlines)
        return trimmed.isEmpty ? nil : trimmed
    }

    public static func load(path: String) throws -> AgentConfigFile {
        let data = try Data(contentsOf: URL(fileURLWithPath: path))
        return try JSONDecoder().decode(AgentConfigFile.self, from: data)
    }
}

/// The helper intentionally decodes only the provider fields that form the AX
/// target boundary. Unknown provider options remain owned by the Go service.
public struct DesktopProviderConfig: Decodable, Sendable {
    public let appName: String?
    public let desktopBundleID: String?
    public let desktopTeamID: String?
    public let desktopAppPath: String?

    enum CodingKeys: String, CodingKey {
        case appName = "app_name"
        case desktopBundleID = "desktop_bundle_id"
        case desktopTeamID = "desktop_team_id"
        case desktopAppPath = "desktop_app_path"
    }
}
