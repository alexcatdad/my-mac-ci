import Darwin
import Foundation
import Testing
import os

@testable import MMC

private func readable(_ fd: Int32) -> Bool {
    var entry = pollfd(fd: fd, events: Int16(POLLIN), revents: 0)
    return poll(&entry, 1, 2000) > 0
}

private func acceptPeer(_ server: Int32) -> Int32 {
    readable(server) ? accept(server, nil, nil) : -1
}

@Test func statusRequiresOwnedPeerAndValidReply() throws {
    let path = "/tmp/mmc-\(UUID().uuidString).sock"
    let server = socket(AF_UNIX, SOCK_STREAM, 0)
    #expect(server >= 0)
    guard server >= 0 else { return }
    defer {
        close(server)
        unlink(path)
    }
    var address = sockaddr_un()
    address.sun_family = sa_family_t(AF_UNIX)
    address.sun_len = UInt8(MemoryLayout<sockaddr_un>.size)
    withUnsafeMutableBytes(of: &address.sun_path) { $0.copyBytes(from: Array(path.utf8) + [0]) }
    let bound = withUnsafePointer(to: &address) {
        $0.withMemoryRebound(to: sockaddr.self, capacity: 1) {
            bind(server, $0, socklen_t(MemoryLayout<sockaddr_un>.size))
        }
    }
    try #require(bound == 0)
    try #require(fcntl(server, F_SETFL, O_NONBLOCK) == 0)
    try #require(listen(server, 4) == 0)

    let credentials = MMCAppCredentials(clientID: "test", installationID: 7, privateKey: "synthetic")
    #expect(HelperSocket.request(at: path, pid: -1, credentials: credentials) == nil)
    let other = acceptPeer(server)
    try #require(other >= 0)
    var byte: UInt8 = 0
    #expect(recv(other, &byte, 1, MSG_DONTWAIT) == 0)
    close(other)

    let replies: [(String, WireState?)] = [
        ("{\"state\":\"awaiting_credentials\"}\n", .awaitingCredentials),
        ("{\"state\":\"dispatching\"}\n", .dispatching),
        ("{\"state\":\"dispatch_failed\"}\n", .dispatchFailed),
        ("{\"state\":\"invalid_credentials\"}\n", .invalidCredentials),
        ("{}\n", nil),
    ]
    for (reply, expected) in replies {
        let queue = DispatchGroup()
        queue.enter()
        DispatchQueue.global().async {
            defer { queue.leave() }
            let peer = acceptPeer(server)
            guard peer >= 0 else { return }
            defer { close(peer) }
            var command = [UInt8](repeating: 0, count: 7)
            guard readable(peer), recv(peer, &command, command.count, MSG_DONTWAIT) == command.count,
                String(decoding: command, as: UTF8.self) == "status\n"
            else { return }
            _ = Array(reply.utf8).withUnsafeBytes { send(peer, $0.baseAddress, $0.count, MSG_NOSIGNAL | MSG_DONTWAIT) }
        }
        #expect(HelperSocket.request(at: path, pid: getpid()) == expected)
        #expect(queue.wait(timeout: .now() + 2) == .success)
    }
}

@Test @MainActor func helperRestartsAfterCrashAndReportsLockConflict() async throws {
    let dir = FileManager.default.temporaryDirectory.appending(path: UUID().uuidString)
    try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: false)
    defer { try? FileManager.default.removeItem(at: dir) }
    let script = dir.appending(path: "helper")
    let fixture = """
        #!/bin/sh
        [ "$1" = --app-mode ] || exit 1
        [ -f "$0.attempted" ] && exit 3
        : > "$0.attempted"
        exit 1
        """
    try Data(fixture.utf8).write(to: script)
    try FileManager.default.setAttributes([.posixPermissions: 0o700], ofItemAtPath: script.path)
    let supervisor = HelperSupervisor(
        executable: script, socketPath: dir.appending(path: "missing.sock").path)
    supervisor.start()
    defer { supervisor.stop() }
    let deadline = ContinuousClock.now.advanced(by: .seconds(5))
    while supervisor.state != .alreadyRunning && ContinuousClock.now < deadline {
        try await Task.sleep(for: .milliseconds(20))
    }
    #expect(supervisor.state == .alreadyRunning)
}

@Test(.enabled(if: ProcessInfo.processInfo.environment["MMC_TEST_HELPER"] != nil))
@MainActor func credentialFailureRequiresManualRetry() async throws {
    let fixture = try #require(ProcessInfo.processInfo.environment["MMC_TEST_HELPER"])
    let dir = URL(fileURLWithPath: "/tmp/mmc-\(UUID().uuidString)")
    try FileManager.default.createDirectory(
        at: dir, withIntermediateDirectories: false, attributes: [.posixPermissions: 0o700])
    defer { try? FileManager.default.removeItem(at: dir) }
    let script = dir.appending(path: "start")
    try FileManager.default.createSymbolicLink(
        at: dir.appending(path: "helper"), withDestinationURL: URL(fileURLWithPath: fixture))
    let command = """
        #!/bin/sh
        cd "${0%/*}" || exit 1
        export MMC_TEST_SOCKET_DIR="$PWD"
        exec ./helper -test.run='^TestAppHandoffFixture$'
        """
    try Data(command.utf8).write(to: script)
    try FileManager.default.setAttributes([.posixPermissions: 0o700], ofItemAtPath: script.path)

    let reads = OSAllocatedUnfairLock(initialState: [Bool]())
    let supervisor = HelperSupervisor(executable: script, socketPath: dir.appending(path: "helper.sock").path) { allowInteraction in
        reads.withLock { $0.append(allowInteraction) }
        throw MMCAppCredentialError.keyAccessDenied
    }
    supervisor.start()
    defer { supervisor.stop() }
    func waitForFailure() async throws {
        let deadline = ContinuousClock.now.advanced(by: .seconds(5))
        while ContinuousClock.now < deadline {
            if case .credentialError = supervisor.state { return }
            try await Task.sleep(for: .milliseconds(25))
        }
        Issue.record("Keychain read failure was not reported")
    }
    try await waitForFailure()
    try await Task.sleep(for: .milliseconds(5200))
    #expect(reads.withLock { $0 } == [false])
    supervisor.retry()
    try await waitForFailure()
    #expect(reads.withLock { $0 } == [false, true])
}
