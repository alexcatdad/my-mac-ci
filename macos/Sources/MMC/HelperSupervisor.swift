import Combine
import Darwin
import Foundation
import MenubarKit

enum HelperState: Equatable {
    case stopped, starting, awaitingCredentials, authenticating, dispatching, dispatchFailed, invalidCredentials
    case credentialError(String)
    case unavailable, alreadyRunning

    var canRetry: Bool {
        switch self {
        case .awaitingCredentials, .dispatchFailed, .invalidCredentials, .credentialError, .alreadyRunning: true
        default: false
        }
    }
}

enum WireState: String, Decodable, Sendable {
    case awaitingCredentials = "awaiting_credentials"
    case dispatching
    case dispatchFailed = "dispatch_failed"
    case invalidCredentials = "invalid_credentials"

    var presentationState: HelperState {
        switch self {
        case .awaitingCredentials: .awaitingCredentials
        case .dispatching: .dispatching
        case .dispatchFailed: .dispatchFailed
        case .invalidCredentials: .invalidCredentials
        }
    }
}

enum QuitResult: String, Decodable, Sendable {
    case ready, busy, pending, unavailable
}

struct HelperReply: Decodable {
    let state: WireState
    let health: HealthSnapshot?
    let reloadError: String?
    let quit: QuitResult?
}

@MainActor
final class HelperSupervisor: ObservableObject {
    @Published private(set) var state = HelperState.stopped

    @Published private(set) var health: HealthSnapshot?
    @Published private(set) var reloading = false
    @Published private(set) var reloadError: String?
    let notifier = MenubarNotifier()
    var ownsHelper: Bool { process?.isRunning == true }

    private static let incidentKey = "mmc.reportedHealthIncidents"
    private var incidents = HealthIncidents(reported: Set(UserDefaults.standard.stringArray(forKey: incidentKey) ?? []))
    private var commandRevision = 0
    private let executable: URL
    private let socketPath: String
    private var process: Process?
    private var lifeline: FileHandle?
    private var monitor: Task<Void, Never>?
    private var restart: Task<Void, Never>?
    private var credentialProblem: HelperState?
    private let loadCredentials: @Sendable (Bool) throws -> MMCAppCredentials
    private var requested = false
    private var delay = 1

    init(
        executable: URL = Bundle.main.bundleURL.appending(path: "Contents/Helpers/mmc-helper"),
        socketPath: String = FileManager.default.homeDirectoryForCurrentUser
            .appending(path: "Library/Application Support/my-mac-ci/helper.sock").path,
        loadCredentials: @escaping @Sendable (Bool) throws -> MMCAppCredentials = MMCAppCredentials.load
    ) {
        self.executable = executable
        self.socketPath = socketPath
        self.loadCredentials = loadCredentials
    }

    func start() {
        guard !requested else { return }
        requested = true
        credentialProblem = nil
        delay = 1
        if process == nil { launch() }
    }

    func stop() {
        requested = false
        restart?.cancel()
        monitor?.cancel()
        state = .stopped
        health = nil
        try? lifeline?.close()
        lifeline = nil
        if let process, process.isRunning { process.terminate() }
    }

    private func launch() {
        let child = Process()
        child.executableURL = executable
        child.arguments = ["--app-mode"]
        child.environment = [
            "HOME": FileManager.default.homeDirectoryForCurrentUser.path,
            "PATH": "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin",
        ]
        let parentPipe = Pipe()
        child.standardInput = parentPipe
        child.standardOutput = FileHandle.nullDevice
        child.standardError = FileHandle.nullDevice
        child.terminationHandler = { [weak self] child in
            Task { @MainActor in self?.didExit(child) }
        }
        state = .starting
        do {
            try child.run()
        } catch {
            scheduleRestart()
            return
        }
        try? parentPipe.fileHandleForReading.close()
        lifeline = parentPipe.fileHandleForWriting
        process = child
        observe(child)
    }

    func retry() {
        guard state.canRetry else { return }
        if state == .alreadyRunning {
            start()
        } else if let process, process.isRunning {
            credentialProblem = nil
            state = .authenticating
            observe(process, retryAuth: true)
        }
    }

    func command(_ operation: String) async -> HelperReply? {
        guard let child = process, child.isRunning else { return nil }
        let pid = child.processIdentifier
        let path = socketPath
        commandRevision += 1
        let revision = commandRevision
        let reply = await Task.detached {
            HelperSocket.exchange(at: path, pid: pid, operation: operation) { child.isRunning }
        }.value
        guard process === child else { return nil }
        if revision == commandRevision {
            commandRevision += 1
            if let reply { health = reply.health }
        }
        return reply
    }

    func reload() async {
        guard !reloading else { return }
        reloading = true
        defer { reloading = false }
        if let reply = await command("reload") {
            reloadError = reply.reloadError
        } else {
            reloadError = "The helper did not confirm the reload."
        }
    }

    private func reportIncidents(_ snapshot: HealthSnapshot) async {
        guard NotificationPreferences.standard().enabled else {
            if !incidents.reported.isEmpty {
                incidents = HealthIncidents()
                UserDefaults.standard.removeObject(forKey: Self.incidentKey)
            }
            return
        }
        let previous = incidents.reported
        var next = incidents
        let messages = next.observe(snapshot.observations)
        var reported = next.reported
        for message in messages {
            guard !Task.isCancelled else { return }
            if await notifier.deliver(message) != .delivered, let key = message.threadIdentifier {
                if previous.contains(key) { reported.insert(key) } else { reported.remove(key) }
            }
        }
        guard !Task.isCancelled, previous != reported else { return }
        incidents = HealthIncidents(reported: reported)
        UserDefaults.standard.set(Array(reported), forKey: Self.incidentKey)
    }

    private func observe(_ child: Process, retryAuth: Bool = false) {
        monitor?.cancel()
        let pid = child.processIdentifier
        let path = socketPath
        monitor = Task { [weak self] in
            let started = ContinuousClock.now
            var mayAuthenticate = true
            while !Task.isCancelled {
                let revision = self?.commandRevision
                let reply = await Task.detached { HelperSocket.exchange(at: path, pid: pid) { child.isRunning } }.value
                guard !Task.isCancelled, let self, process === child else { return }
                guard revision == commandRevision else { continue }
                let status = reply?.state
                health = reply?.health
                if let status {
                    if started.duration(to: .now) >= .seconds(30) { delay = 1 }
                    if status == .dispatching {
                        credentialProblem = nil
                        state = .dispatching
                        mayAuthenticate = false
                    } else if mayAuthenticate && credentialProblem == nil
                        && (status == .awaitingCredentials || retryAuth)
                    {
                        mayAuthenticate = false
                        await authenticate(child, allowInteraction: retryAuth)
                    } else {
                        state = credentialProblem ?? status.presentationState
                    }
                } else if started.duration(to: .now) >= .seconds(3) {
                    state = .unavailable
                }
                if status == .dispatching, let health { await reportIncidents(health) }
                try? await Task.sleep(for: status == nil ? .seconds(1) : .seconds(5))
            }
        }
    }

    private func authenticate(_ child: Process, allowInteraction: Bool) async {
        state = .authenticating
        let load = loadCredentials
        let outcome: HelperState
        do {
            let credentials = try await Task.detached { try load(allowInteraction) }.value
            guard !Task.isCancelled, process === child, child.isRunning else { return }
            let path = socketPath
            let pid = child.processIdentifier
            let reply = await Task.detached {
                HelperSocket.request(at: path, pid: pid, credentials: credentials) { child.isRunning }
            }.value
            outcome = reply?.presentationState ?? .credentialError("The helper did not confirm receipt. Try again.")
        } catch let error as MMCAppCredentialError {
            outcome = .credentialError(error.localizedDescription)
        } catch {
            outcome = .credentialError("Unable to load GitHub App credentials.")
        }
        guard !Task.isCancelled, process === child else { return }
        state = outcome
        credentialProblem = outcome == .dispatching ? nil : outcome
    }

    private func didExit(_ child: Process) {
        guard process === child else { return }
        monitor?.cancel()
        try? lifeline?.close()
        lifeline = nil
        process = nil
        guard requested else { return }
        if child.terminationReason == .exit && child.terminationStatus == 3 {
            requested = false
            state = .alreadyRunning
        } else {
            scheduleRestart()
        }
    }

    private func scheduleRestart() {
        state = .unavailable
        let seconds = delay
        delay = min(2 * delay, 30)
        restart = Task { [weak self] in
            try? await Task.sleep(for: .seconds(seconds))
            guard !Task.isCancelled, let self, requested else { return }
            launch()
        }
    }
}

enum HelperSocket {
    /// Check the kernel-reported peer before sending either command or any credential bytes.
    static func request(
        at path: String, pid: pid_t, credentials: MMCAppCredentials? = nil, childIsRunning: () -> Bool = { true }
    ) -> WireState? {
        exchange(at: path, pid: pid, credentials: credentials, childIsRunning: childIsRunning)?.state
    }

    static func exchange(
        at path: String, pid: pid_t, operation: String = "status", credentials: MMCAppCredentials? = nil,
        childIsRunning: () -> Bool = { true }
    ) -> HelperReply? {
        let fd = socket(AF_UNIX, SOCK_STREAM, 0)
        guard fd >= 0 else { return nil }
        defer { close(fd) }
        var noSignal: Int32 = 1
        guard
            setsockopt(
                fd, SOL_SOCKET, SO_NOSIGPIPE, &noSignal, socklen_t(MemoryLayout.size(ofValue: noSignal)))
                == 0,
            fcntl(fd, F_SETFL, O_NONBLOCK) == 0
        else { return nil }

        let deadline = ProcessInfo.processInfo.systemUptime + 3
        func wait(_ events: Int16) -> Bool {
            let remaining = deadline - ProcessInfo.processInfo.systemUptime
            var entry = pollfd(fd: fd, events: events, revents: 0)
            return remaining > 0 && poll(&entry, 1, Int32(ceil(remaining * 1000))) > 0
        }

        var address = sockaddr_un()
        let bytes = Array(path.utf8) + [0]
        guard bytes.count <= MemoryLayout.size(ofValue: address.sun_path),
            let offset = MemoryLayout<sockaddr_un>.offset(of: \.sun_path)
        else { return nil }
        address.sun_family = sa_family_t(AF_UNIX)
        address.sun_len = UInt8(offset + bytes.count)
        withUnsafeMutableBytes(of: &address.sun_path) { $0.copyBytes(from: bytes) }
        let length = socklen_t(address.sun_len)
        let connected = withUnsafePointer(to: &address) {
            $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { connect(fd, $0, length) }
        }
        if connected != 0 {
            var result: Int32 = 0
            var size = socklen_t(MemoryLayout.size(ofValue: result))
            guard errno == EINPROGRESS, wait(Int16(POLLOUT)),
                getsockopt(fd, SOL_SOCKET, SO_ERROR, &result, &size) == 0, result == 0
            else { return nil }
        }
        var peer: pid_t = 0
        var peerSize = socklen_t(MemoryLayout.size(ofValue: peer))
        guard getsockopt(fd, SOL_LOCAL, LOCAL_PEERPID, &peer, &peerSize) == 0, peer == pid, childIsRunning() else {
            return nil
        }

        var request: Data
        if let credentials {
            guard let json = try? JSONEncoder().encode(credentials), json.count < 128 * 1024 else { return nil }
            request = Data("auth\n".utf8)
            request.append(json)
            request.append(10)
        } else {
            guard ["status", "reload", "quit-prepare", "quit-stop", "quit-resume"].contains(operation) else {
                return nil
            }
            request = Data((operation + "\n").utf8)
        }
        var sent = 0
        while sent < request.count {
            guard wait(Int16(POLLOUT)) else { return nil }
            let count = request.withUnsafeBytes { buffer in
                send(fd, buffer.baseAddress!.advanced(by: sent), request.count - sent, 0)
            }
            guard count > 0 else { return nil }
            sent += count
        }

        var data = Data()
        var buffer = [UInt8](repeating: 0, count: 8192)
        let limit = 1 << 20
        while data.count < limit && wait(Int16(POLLIN)) {
            let count = recv(fd, &buffer, min(buffer.count, limit - data.count), 0)
            guard count > 0 else { return nil }
            let chunk = buffer.prefix(count)
            if let end = chunk.firstIndex(of: 10) {
                data.append(contentsOf: chunk[..<end])
                return try? JSONDecoder().decode(HelperReply.self, from: data)
            }
            data.append(contentsOf: chunk)
        }
        return nil
    }
}
