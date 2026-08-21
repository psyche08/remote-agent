// swift-tools-version:5.9
//
// AgentHaloDesktop — the macOS-native half of computer use and Locked Use.
//
// This exists as its own component, rather than as scripts the Go service
// shells out to, for reasons that are specific rather than stylistic:
//
//   * A resident process can hold the display shield as windows it owns. The
//     one-shot script had to track the shield host by pid file in $HOME, and
//     any process running as this user could write a live pid there — making
//     "the shield is up" believable to the controller while the desktop sat
//     uncovered. In-process state has no such forgeable surface.
//   * TCC (Accessibility, Screen Recording) grants attach to a signed binary
//     at a stable path. `swift script.swift` attaches them to the interpreter,
//     which is both wrong and fragile.
//   * Compiling once at build time instead of on every operation removes a
//     multi-second cost from a path that polls safeguards every 40ms.
//
// The core is a library so the vocabulary, bounds, and grant logic stay unit
// testable without a run loop; the executable is a thin shell around it.
import PackageDescription

let package = Package(
    name: "AgentHaloDesktop",
    platforms: [.macOS(.v12)],
    targets: [
        .target(
            name: "AgentHaloDesktopCore",
            path: "Sources/AgentHaloDesktopCore"
        ),
        .executableTarget(
            name: "agenthalo-desktop",
            dependencies: ["AgentHaloDesktopCore"],
            path: "Sources/agenthalo-desktop"
        ),
        .testTarget(
            name: "AgentHaloDesktopCoreTests",
            dependencies: ["AgentHaloDesktopCore"],
            path: "Tests/AgentHaloDesktopCoreTests"
        ),
    ]
)
