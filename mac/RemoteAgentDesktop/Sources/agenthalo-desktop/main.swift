import AppKit
import Foundation
import AgentHaloDesktopCore

// agenthalo-desktop — the resident macOS helper behind computer use.
//
// It serves one unix socket and owns the display shield. AppKit runs on the
// main thread because the shield is made of windows; the socket is served on
// its own queue and hops to main for anything AppKit touches.
//
// Usage:
//   agenthalo-desktop --socket <path> [--config <path>]
//   agenthalo-desktop --self-check [--check-shield] [--check-lock]
//   agenthalo-desktop --provision-locked-use-key --config <path>
//
// Without --config the helper serves the desktop surface only, and every Locked
// Use request is refused. Configuration is never accepted over the socket: a
// capability that lets a machine unlock itself has to be granted on the device,
// and a configure op would hand it to any process that could connect.

let defaultSocketPath = NSString(
    string: "~/Library/Application Support/AgentHalo/desktop.sock"
).expandingTildeInPath

var socketPath = defaultSocketPath
var configPath = ""
var selfCheck = false
var checkShield = false
var checkLock = false
var provisionLockedUseKey = false

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
    case "--provision-locked-use-key": provisionLockedUseKey = true
    case "-h", "--help":
        print("""
            usage: agenthalo-desktop [--socket <path>] [--config <path>]
                   agenthalo-desktop --self-check [--check-shield] [--check-lock]
                   agenthalo-desktop --provision-locked-use-key --config <path>
            """)
        exit(0)
    default:
        fail("unknown option: \(argument)")
    }
}

// Provisioning is deliberately a separate, non-disruptive mode. It proves
// that this final signed binary can access its file-based login-Keychain item and
// emits only the public verifying key needed by the Authorization Plug-in. It
// does not instantiate the desktop service, lock the screen, or start a socket.
if provisionLockedUseKey {
    guard !selfCheck, !checkShield, !checkLock else {
        fail("--provision-locked-use-key cannot be combined with diagnostic checks")
    }
    guard !configPath.isEmpty else {
        fail("--provision-locked-use-key requires --config <path>")
    }
    do {
        let file = try AgentConfigFile.load(path: configPath)
        let deviceID = file.deviceID.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !deviceID.isEmpty else {
            fail("--provision-locked-use-key requires a non-empty device_id in the config")
        }
        let signer = try GrantSigner.loadOrCreateSecure(deviceID: deviceID)
        let encoded = try JSONSerialization.data(
            withJSONObject: ["public_key": signer.publicKeyBase64],
            options: [.sortedKeys])
        FileHandle.standardOutput.write(encoded)
        FileHandle.standardOutput.write(Data("\n".utf8))
        exit(0)
    } catch {
        fail("could not provision the Locked Use signing key: \(error)", code: 1)
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
var computerUseEnabled = false
var accessibilityTargetPolicies: [Accessibility.TargetPolicy] = []
if !configPath.isEmpty {
    do {
        let file = try AgentConfigFile.load(path: configPath)
        accessibilityTargetPolicies = try file.accessibilityTargetPolicies()
        // Configured production mode always installs a controller, including
        // when the block is absent or disabled. A nil controller is the
        // explicit no-config development mode and routes actions directly;
        // falling back to it after an operator removed `computer_use` would
        // turn disabling the capability into enabling an unguarded one.
        let computerUse = file.computerUse ?? ComputerUseConfig()
        computerUseEnabled = computerUse.normalized().enabled
        let built = LockedUseController(
            config: computerUse, deviceID: file.deviceID,
            system: DesktopSystem(desktop: desktop))
        built.start()
        controller = built
    } catch {
        // A config that cannot be read is not a reason to serve an
        // unconfigured desktop: the operator asked for a configured one, and
        // silently degrading would present Locked Use as simply off.
        fail("could not read \(configPath): \(error)", code: 1)
    }
}

let server = SocketServer(
    configuration: .init(path: socketPath, computerUseEnabled: computerUseEnabled),
    router: RequestRouter(
        desktop: desktop, controller: controller,
        accessibilityTargetPolicies: accessibilityTargetPolicies))
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
var terminationStarted = false
for terminating in [SIGTERM, SIGINT] {
    signal(terminating, SIG_IGN)
    let source = DispatchSource.makeSignalSource(signal: terminating, queue: .main)
    source.setEventHandler {
        guard !terminationStarted else { return }
        terminationStarted = true
        // stop() intentionally blocks until relock is confirmed. Run it away
        // from main so DisplayShield can synchronously use the AppKit run loop
        // during that cleanup instead of deadlocking termination.
        DispatchQueue.global(qos: .userInitiated).async {
            let safe = controller?.stop() ?? true
            if !safe {
                // The main AppKit loop must remain alive to keep the shield on
                // screen. Quarantine repeatedly withdraws/relocks; exit only
                // after the controller proves the boundary safe.
                while controller?.isSafeToExit == false {
                    Thread.sleep(forTimeInterval: 0.25)
                }
            }
            // The controller releases the shield only after it has verified
            // both grant withdrawal and relock. An unconditional release here
            // would undo that fail-closed decision on the error path.
            server.stop()
            exit(0)
        }
    }
    source.resume()
    terminationSources.append(source)
}

let application = NSApplication.shared
application.setActivationPolicy(.accessory)
application.run()
