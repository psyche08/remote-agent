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
//   remote-agent-desktop --socket <path>
//   remote-agent-desktop --self-check     one-shot: no socket, no run loop

let defaultSocketPath = NSString(
    string: "~/Library/Application Support/remote-agent/desktop.sock"
).expandingTildeInPath

var socketPath = defaultSocketPath
var selfCheck = false

var arguments = Array(CommandLine.arguments.dropFirst())
while let argument = arguments.first {
    arguments.removeFirst()
    switch argument {
    case "--socket":
        guard let value = arguments.first else {
            FileHandle.standardError.write(Data("--socket requires a path\n".utf8))
            exit(2)
        }
        arguments.removeFirst()
        socketPath = value
    case "--self-check":
        selfCheck = true
    case "-h", "--help":
        print("usage: remote-agent-desktop [--socket <path>] [--self-check]")
        exit(0)
    default:
        FileHandle.standardError.write(Data("unknown option: \(argument)\n".utf8))
        exit(2)
    }
}

let desktop = DesktopService()
let router = RequestRouter(desktop: desktop)

// A broken client connection must not kill the helper mid-window; write errors
// are handled where they happen.
signal(SIGPIPE, SIG_IGN)

if selfCheck {
    // The read-only probes, so a preflight can confirm the helper answers on
    // this Mac without opening a socket or holding a run loop.
    for op in ["lock_state", "idle_seconds", "shield_state"] {
        let line = Data("{\"op\":\"\(op)\"}".utf8)
        FileHandle.standardOutput.write(router.handle(line: line))
    }
    exit(0)
}

let server = SocketServer(configuration: .init(path: socketPath), router: router)
do {
    try server.start()
} catch {
    FileHandle.standardError.write(Data("\(error)\n".utf8))
    exit(1)
}

// Release the shield on the way out. A helper that exits with the screen
// covered and unlocked would leave the desktop in exactly the state every
// safeguard exists to prevent.
//
// The sources are held for the life of the process on purpose. A dispatch
// signal source cancels itself when it is deallocated, so keeping these in a
// loop-local would leave SIG_IGN installed with nothing listening — a helper
// that ignores SIGTERM outright, reachable only by SIGKILL, which skips this
// handler and strands the desktop covered.
var terminationSources: [DispatchSourceSignal] = []
for terminating in [SIGTERM, SIGINT] {
    signal(terminating, SIG_IGN)
    let source = DispatchSource.makeSignalSource(signal: terminating, queue: .main)
    source.setEventHandler {
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
