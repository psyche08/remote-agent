import Foundation

/// Gates the computer-use surface and its Locked Use extension. Both are opt-in
/// and off by default: an unset block means the feature is off and every
/// endpoint reports unavailable.
public struct ComputerUseConfig: Codable, Sendable {
    /// Turns on the action surface. When false, actions are refused.
    public var enabled: Bool
    /// Participates in the macOS unlock flow so an authorized turn can drive
    /// the desktop after the screen locks. Requires `enabled` and the
    /// separately installed Apple Authorization Plug-in.
    public var lockedUse: LockedUseConfig

    enum CodingKeys: String, CodingKey {
        case enabled
        case lockedUse = "locked_use"
    }

    public init(enabled: Bool = false, lockedUse: LockedUseConfig = .init()) {
        self.enabled = enabled
        self.lockedUse = lockedUse
    }

    /// Every key is optional on the wire, because a real config.json fills in
    /// only what the operator set. The synthesized decoder would reject a
    /// partial block outright, and a config that fails to decode presents as
    /// "computer use is not configured" — the feature silently absent rather
    /// than misconfigured.
    public init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        enabled = try container.decodeIfPresent(Bool.self, forKey: .enabled) ?? false
        lockedUse = try container.decodeIfPresent(LockedUseConfig.self, forKey: .lockedUse)
            ?? LockedUseConfig()
    }

    /// Every field fails closed: an unset or invalid value collapses to the
    /// most restrictive safe default rather than a wider unlock window.
    public func normalized() -> ComputerUseConfig {
        var copy = self
        var lu = copy.lockedUse
        lu.grantTTLSeconds = Self.clamp(lu.grantTTLSeconds, 2, 15, 10)
        lu.windowTTLSeconds = Self.clamp(lu.windowTTLSeconds, 15, 900, 300)
        lu.inputRelockGraceMs = Self.clamp(lu.inputRelockGraceMs, 100, 5000, 250)
        if lu.requireDisplayShield == nil { lu.requireDisplayShield = true }
        // Locked Use extends computer use; it can never be the only thing on.
        if !copy.enabled { lu.enabled = false }
        // A grant TTL at or above the window ceiling would let one minted grant
        // outlive the window it was issued for.
        if lu.grantTTLSeconds > lu.windowTTLSeconds {
            lu.grantTTLSeconds = lu.windowTTLSeconds
        }
        copy.lockedUse = lu
        return copy
    }

    /// Zero means "unset" and takes the default; anything else is clamped, so
    /// an out-of-range value can never widen a security window.
    private static func clamp(_ value: Int, _ low: Int, _ high: Int, _ fallback: Int) -> Int {
        if value == 0 { return fallback }
        return Swift.min(Swift.max(value, low), high)
    }
}

public struct LockedUseConfig: Codable, Sendable {
    /// Opts this device into Locked Use. Off by default. Turning it on still
    /// requires the provisioned Authorization Plug-in; without a verifiable key
    /// pair the controller stays disarmed.
    public var enabled: Bool
    /// Where the controller writes signed grants for the plug-in to read.
    public var grantDirectory: String
    /// The ECDSA P-256 private key, base64 PKCS#8, 0600.
    public var signingKeyPath: String
    /// Bounds how long a single grant is valid. Minted just before an unlock
    /// and consumed by it, so deliberately tiny: a grant that lingers on disk
    /// is ambient authorization any local process could ride. Clamped [2, 15].
    public var grantTTLSeconds: Int
    /// The hard ceiling on one per-turn window regardless of activity.
    /// Clamped [15, 900].
    public var windowTTLSeconds: Int
    /// How long the machine must already have been idle before a window may
    /// open. It does NOT set the monitor's cadence: that is fixed, so this knob
    /// cannot be widened into a window where a present human types unnoticed.
    /// Clamped [100, 5000].
    public var inputRelockGraceMs: Int
    /// When true (the default), refuses to open a window unless the shield
    /// engages first. Optional so "unset" is distinguishable from "false".
    public var requireDisplayShield: Bool?

    enum CodingKeys: String, CodingKey {
        case enabled
        case grantDirectory = "grant_dir"
        case signingKeyPath = "signing_key_path"
        case grantTTLSeconds = "grant_ttl_seconds"
        case windowTTLSeconds = "window_ttl_seconds"
        case inputRelockGraceMs = "input_relock_grace_ms"
        case requireDisplayShield = "require_display_shield"
    }

    public init(
        enabled: Bool = false, grantDirectory: String = "", signingKeyPath: String = "",
        grantTTLSeconds: Int = 0, windowTTLSeconds: Int = 0, inputRelockGraceMs: Int = 0,
        requireDisplayShield: Bool? = nil
    ) {
        self.enabled = enabled
        self.grantDirectory = grantDirectory
        self.signingKeyPath = signingKeyPath
        self.grantTTLSeconds = grantTTLSeconds
        self.windowTTLSeconds = windowTTLSeconds
        self.inputRelockGraceMs = inputRelockGraceMs
        self.requireDisplayShield = requireDisplayShield
    }

    /// Absent keys take the unset value, which `normalized()` then collapses to
    /// the most restrictive safe default. Note `requireDisplayShield` stays nil
    /// when absent rather than defaulting to false: "not stated" must mean the
    /// shield is required, never that it was waived.
    public init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        enabled = try container.decodeIfPresent(Bool.self, forKey: .enabled) ?? false
        grantDirectory = try container.decodeIfPresent(String.self, forKey: .grantDirectory) ?? ""
        signingKeyPath = try container.decodeIfPresent(String.self, forKey: .signingKeyPath) ?? ""
        grantTTLSeconds = try container.decodeIfPresent(Int.self, forKey: .grantTTLSeconds) ?? 0
        windowTTLSeconds = try container.decodeIfPresent(Int.self, forKey: .windowTTLSeconds) ?? 0
        inputRelockGraceMs = try container.decodeIfPresent(Int.self, forKey: .inputRelockGraceMs) ?? 0
        requireDisplayShield = try container.decodeIfPresent(
            Bool.self, forKey: .requireDisplayShield)
    }

    public var shieldRequired: Bool { requireDisplayShield ?? true }
}

/// Where the Authorization Plug-in looks for a grant. It must stay in step with
/// RA_LOCKED_USE_DIR in mac/authorization-plugin/RemoteAgentLockedUse.m.
///
/// Defaulting to anywhere else would produce a controller that publishes grants
/// nowhere the verifier looks: Locked Use would appear armed and simply never
/// unlock.
public let defaultGrantDirectory = "/Library/Application Support/remote-agent/locked-use"
