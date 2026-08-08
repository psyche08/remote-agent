import AppKit
import Foundation
import RemoteAgentDesktopCore

// remote-agent-desktop — the resident macOS helper behind computer use.
//
// It serves one unix socket and owns the display shield. AppKit runs on the
// main thread because the shield is made of windows; the socket is served on
// its own queue and hops to main for anything AppKit touches.
//
// Usage:
//   remote-agent-desktop --socket <path> [--config <path>]
//   remote-agent-desktop --self-check [--check-shield] [--check-lock]
//
// Without --config the helper serves the desktop surface only, and every Locked
// Use request is refused. Configuration is never accepted over the socket: a
// capability that lets a machine unlock itself has to be granted on the device,
// and a configure op would hand it to any process that could connect.

let defaultSocketPath = NSString(
    string: "~/Library/Application Support/remote-agent/desktop.sock"
).expandingTildeInPath

var socketPath = defaultSocketPath
var configPath = ""
var selfCheck = false
var checkShield = false
var checkLock = false

func fail(_ message: String, code: Int32 = 2) -> Never {
    FileHandle.standardError.write(Data((message + "\n").utf8))
    exit(code)
}

var arguments = Array(CommandLine.arguments.dropFirst())
while let argument = arguments.first {
    arguments.removeFirst()
    func value(for flag: String) -> String {
        guard let next = arguments.first else { fail("\(flag) requires a value") }
        arguments.removeFirst()
        return next
    }
    switch argument {
    case "--socket": socketPath = value(for: "--socket")
    case "--config": configPath = value(for: "--config")
    case "--self-check": selfCheck = true
    case "--check-shield": checkShield = true
    case "--check-lock": checkLock = true
    case "-h", "--help":
        print("""
            usage: remote-agent-desktop [--socket <path>] [--config <path>]
                   remote-agent-desktop --self-check [--check-shield] [--check-lock]
            """)
        exit(0)
    default:
        fail("unknown option: \(argument)")
    }
}

let desktop = DesktopService()

// A broken client connection must not kill the helper mid-window; write errors
// are handled where they happen.
signal(SIGPIPE, SIG_IGN)

// MARK: - Diagnostics
//
// The disruptive probes live here as one-shot flags rather than as socket ops.
// Locking the screen and raising or dropping the shield belong to the
// controller: a shield that any connected process could release is a shield
// that can be taken down while a window is open, which is the exposure the
// whole safeguard exists to prevent. An operator running this binary by hand is
// a different thing from a process that reached the socket.

if selfCheck || checkShield || checkLock {
    let router = RequestRouter(desktop: desktop)
    for op in ["lock_state", "idle_seconds", "shield_state"] {
        FileHandle.standardOutput.write(router.handle(line: Data("{\"op\":\"\(op)\"}".utf8)))
    }
    if checkShield {
        // The shield is AppKit windows, so this needs what AppKit needs: an
        // initialized NSApplication and a running run loop. Engaging and then
        // sleeping on the main thread would block the very loop that composites
        // the windows — the check would pass against windows nobody ever saw,
        // which is worse than not checking, because it would certify a shield
        // that does not cover anything.
        let application = NSApplication.shared
        application.setActivationPolicy(.accessory)
        application.finishLaunching()
        let engaged = desktop.engageShield()
        RunLoop.main.run(until: Date().addingTimeInterval(2))
        let live = desktop.shieldState()
        desktop.releaseShield()
        // Let the release reach the window server before the process exits, so
        // a failure to drop the shield surfaces here rather than as a black
        // screen the operator has to reason about.
        RunLoop.main.run(until: Date().addingTimeInterval(0.3))
        let result: [String: Any] = [
            "ok": engaged.engaged && live.engaged,
            "check": "shield",
            "displays": engaged.displays,
            "reported_live": live.engaged,
        ]
        FileHandle.standardOutput.write(
            (try? JSONSerialization.data(withJSONObject: result)) ?? Data())
        FileHandle.standardOutput.write(Data("\n".utf8))
    }
    if checkLock {
        let locked = desktop.lockScreen()
        let result: [String: Any] = ["ok": locked, "check": "lock"]
        FileHandle.standardOutput.write(
            (try? JSONSerialization.data(withJSONObject: result)) ?? Data())
        FileHandle.standardOutput.write(Data("\n".utf8))
    }
    exit(0)
}

// MARK: - Serve

var controller: LockedUseController?
if !configPath.isEmpty {
    do {
        let file = try AgentConfigFile.load(path: configPath)
        if let computerUse = file.computerUse {
            let built = LockedUseController(
                config: computerUse, deviceID: file.deviceID,
                system: DesktopSystem(desktop: desktop))
            built.start()
            controller = built
        }
    } catch {
        // A config that cannot be read is not a reason to serve an
        // unconfigured desktop: the operator asked for a configured one, and
        // silently degrading would present Locked Use as simply off.
        fail("could not read \(configPath): \(error)", code: 1)
    }
}

let server = SocketServer(
    configuration: .init(path: socketPath),
    router: RequestRouter(desktop: desktop, controller: controller))
do {
    try server.start()
} catch {
    fail("\(error)", code: 1)
}

// Close any open window and release the shield on the way out. A helper that
// exits with the screen unlocked and covered would leave the desktop in exactly
// the state every safeguard exists to prevent.
//
// The sources are held for the life of the process on purpose. A dispatch
// signal source cancels itself when it is deallocated, so keeping these in a
// loop-local would leave SIG_IGN installed with nothing listening — a helper
// that ignores SIGTERM outright, reachable only by SIGKILL, which skips this
// handler and strands the desktop.
var terminationSources: [DispatchSourceSignal] = []
for terminating in [SIGTERM, SIGINT] {
    signal(terminating, SIG_IGN)
    let source = DispatchSource.makeSignalSource(signal: terminating, queue: .main)
    source.setEventHandler {
        // stop() blocks until the relock is confirmed; that is the point.
        controller?.stop()
        desktop.releaseShield()
        server.stop()
        exit(0)
    }
    source.resume()
    terminationSources.append(source)
}

let application = NSApplication.shared
application.setActivationPolicy(.accessory)
application.run()
