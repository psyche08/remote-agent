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

    enum CodingKeys: String, CodingKey {
        case deviceID = "device_id"
        case computerUse = "computer_use"
    }

    public init(deviceID: String, computerUse: ComputerUseConfig?) {
        self.deviceID = deviceID
        self.computerUse = computerUse
    }

    public init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        deviceID = (try? container.decode(String.self, forKey: .deviceID)) ?? ""
        computerUse = try? container.decode(ComputerUseConfig.self, forKey: .computerUse)
    }

    public static func load(path: String) throws -> AgentConfigFile {
        let data = try Data(contentsOf: URL(fileURLWithPath: path))
        return try JSONDecoder().decode(AgentConfigFile.self, from: data)
    }
}
