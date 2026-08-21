import Darwin
import Foundation
import Security

/// A unix-domain socket server speaking newline-delimited JSON.
///
/// UDS rather than XPC because the only client is a Go process: XPC is a C API
/// over Mach ports and would put cgo into the service binary, which this repo
/// deliberately avoids. The security properties that matter here — who may
/// connect — come from the filesystem and from the peer credential check
/// below, not from the transport.
public final class SocketServer {
    public struct Configuration {
        public static let agentSigningIdentifier = "dev.linsheng.agenthalo"

        public let path: String
        public let requiredPeerSigningIdentifier: String?

        public init(path: String, requiredPeerSigningIdentifier: String? = nil) {
            self.path = path
            self.requiredPeerSigningIdentifier = requiredPeerSigningIdentifier
        }

        /// Every enabled computer-use endpoint (screen capture, input, and AX,
        /// not only Locked Use) is restricted to the exact signed Go agent.
        /// The nil requirement is reserved for the unconfigured desktop-only
        /// development mode, where the same-uid check remains the boundary.
        public init(path: String, computerUseEnabled: Bool) {
            self.init(
                path: path,
                requiredPeerSigningIdentifier: computerUseEnabled
                    ? Self.agentSigningIdentifier
                    : nil)
        }
    }

    private let configuration: Configuration
    private let router: RequestRouter
    private let queue = DispatchQueue(label: "agenthalo-desktop.socket")
    private var listener: Int32 = -1
    private var peerRequirement: SecRequirement?

    public init(configuration: Configuration, router: RequestRouter) {
        self.configuration = configuration
        self.router = router
    }

    public enum StartError: Error, CustomStringConvertible {
        case pathTooLong(String)
        case syscall(String, errno: Int32)
        case peerCodeSigning(String)

        public var description: String {
            switch self {
            case .pathTooLong(let path):
                return "socket path is too long for sockaddr_un: \(path)"
            case .syscall(let call, let code):
                return "\(call) failed: \(String(cString: strerror(code)))"
            case .peerCodeSigning(let detail):
                return "peer code-signing policy failed: \(detail)"
            }
        }
    }

    public func start() throws {
        if let identifier = configuration.requiredPeerSigningIdentifier {
            do {
                peerRequirement = try PeerCodeSigning.makePeerRequirement(
                    expectedPeerIdentifier: identifier)
            } catch {
                throw StartError.peerCodeSigning("\(error)")
            }
        }
        // A dead socket file from a previous run would make bind fail with
        // EADDRINUSE, so it is removed first. Anything else at that path is a
        // configuration error the operator must see, not something to unlink.
        var info = stat()
        if stat(configuration.path, &info) == 0 {
            guard (info.st_mode & S_IFMT) == S_IFSOCK else {
                throw StartError.syscall("bind", errno: EEXIST)
            }
            unlink(configuration.path)
        }

        let directory = (configuration.path as NSString).deletingLastPathComponent
        try? FileManager.default.createDirectory(
            atPath: directory, withIntermediateDirectories: true,
            attributes: [.posixPermissions: 0o700])

        let fd = socket(AF_UNIX, SOCK_STREAM, 0)
        guard fd >= 0 else { throw StartError.syscall("socket", errno: errno) }

        var address = sockaddr_un()
        address.sun_family = sa_family_t(AF_UNIX)
        let pathBytes = Array(configuration.path.utf8)
        let capacity = MemoryLayout.size(ofValue: address.sun_path)
        guard pathBytes.count < capacity else {
            close(fd)
            throw StartError.pathTooLong(configuration.path)
        }
        withUnsafeMutableBytes(of: &address.sun_path) { raw in
            raw.copyBytes(from: pathBytes)
        }

        // The socket is created 0600 so only this user can connect at all; the
        // peer check below narrows it further. umask cannot be relied on, so
        // the mode is set explicitly after bind.
        let bound = withUnsafePointer(to: &address) { pointer in
            pointer.withMemoryRebound(to: sockaddr.self, capacity: 1) { sockaddrPointer in
                bind(fd, sockaddrPointer, socklen_t(MemoryLayout<sockaddr_un>.size))
            }
        }
        guard bound == 0 else {
            let code = errno
            close(fd)
            throw StartError.syscall("bind", errno: code)
        }
        chmod(configuration.path, 0o600)

        guard listen(fd, 16) == 0 else {
            let code = errno
            close(fd)
            unlink(configuration.path)
            throw StartError.syscall("listen", errno: code)
        }

        listener = fd
        queue.async { [weak self] in self?.acceptLoop() }
    }

    public func stop() {
        if listener >= 0 {
            close(listener)
            listener = -1
        }
        unlink(configuration.path)
    }

    private func acceptLoop() {
        while true {
            let client = accept(listener, nil, nil)
            if client < 0 {
                if errno == EINTR { continue }
                return  // the listener was closed
            }
            guard Self.peerIsAuthorized(client, requirement: peerRequirement) else {
                close(client)
                continue
            }
            // One serial queue per connection: requests from a single caller
            // are answered in order, and a slow action cannot interleave with
            // the safeguard polling on another connection.
            DispatchQueue(label: "agenthalo-desktop.conn").async { [router] in
                Self.serve(client: client, router: router)
            }
        }
    }

    /// Only the configured, signed agent process may drive computer use.
    ///
    /// The socket mode already restricts this, but mode alone is a statement
    /// about the file, not about who is on the other end; a check on the
    /// connection itself is what survives a mis-set permission. In
    /// desktop-only development mode `requirement` is nil and uid is the
    /// boundary. Enabled computer use always supplies a requirement, narrowing
    /// the peer to the exact agent identifier and this helper's Developer ID
    /// team.
    private static func peerIsAuthorized(
        _ fd: Int32, requirement: SecRequirement?
    ) -> Bool {
        var credentials = xucred()
        var length = socklen_t(MemoryLayout<xucred>.size)
        guard getsockopt(fd, SOL_LOCAL, LOCAL_PEERCRED, &credentials, &length) == 0,
              credentials.cr_version == XUCRED_VERSION else {
            return false
        }
        guard credentials.cr_uid == getuid() else { return false }
        guard let requirement else { return true }
        return PeerCodeSigning.peerSatisfies(fd: fd, requirement: requirement)
    }

    private static func serve(client: Int32, router: RequestRouter) {
        defer { close(client) }
        var pending = Data()
        var buffer = [UInt8](repeating: 0, count: 8192)
        while true {
            let got = read(client, &buffer, buffer.count)
            if got < 0 {
                if errno == EINTR { continue }
                return
            }
            if got == 0 { return }
            pending.append(contentsOf: buffer[0..<got])
            // A request that never terminates must not grow without bound.
            guard pending.count <= maxRequestBytes else { return }
            while let newline = pending.firstIndex(of: UInt8(ascii: "\n")) {
                let line = pending[pending.startIndex..<newline]
                pending = pending[pending.index(after: newline)...]
                if line.isEmpty { continue }
                let response = router.handle(line: Data(line))
                guard write(fd: client, data: response) else { return }
            }
        }
    }

    private static let maxRequestBytes = 1 << 20

    private static func write(fd: Int32, data: Data) -> Bool {
        var remaining = data
        while !remaining.isEmpty {
            let wrote = remaining.withUnsafeBytes { raw -> Int in
                Darwin.write(fd, raw.baseAddress, raw.count)
            }
            if wrote < 0 {
                if errno == EINTR { continue }
                return false
            }
            remaining = remaining.dropFirst(wrote)
        }
        return true
    }
}
