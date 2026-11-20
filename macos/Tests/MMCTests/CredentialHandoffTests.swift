import Darwin
import Foundation
import Security
import Testing

@testable import MMC

/// The local gate supplies a Go test binary whose dispatch implementation never contacts GitHub.
@Test(.enabled(if: ProcessInfo.processInfo.environment["MMC_TEST_HELPER"] != nil))
func swiftCredentialsStartGoDispatch() async throws {
    let binary = try #require(ProcessInfo.processInfo.environment["MMC_TEST_HELPER"])
    let dir = URL(fileURLWithPath: "/tmp/mmc-\(UUID().uuidString)")
    try FileManager.default.createDirectory(
        at: dir, withIntermediateDirectories: false, attributes: [.posixPermissions: 0o700])
    defer { try? FileManager.default.removeItem(at: dir) }

    let child = Process()
    let lifetime = Pipe()
    child.executableURL = URL(fileURLWithPath: binary)
    child.arguments = ["-test.run=^TestAppHandoffFixture$"]
    child.environment = ["MMC_TEST_SOCKET_DIR": dir.path]
    child.standardInput = lifetime
    child.standardOutput = FileHandle.nullDevice
    child.standardError = FileHandle.nullDevice
    try child.run()
    defer {
        try? lifetime.fileHandleForWriting.close()
        if child.isRunning { kill(child.processIdentifier, SIGKILL) }
        child.waitUntilExit()
    }

    let path = dir.appending(path: "helper.sock").path
    let pid = child.processIdentifier
    let deadline = ContinuousClock.now.advanced(by: .seconds(5))
    var state = HelperSocket.request(at: path, pid: pid)
    while state == nil && child.isRunning && ContinuousClock.now < deadline {
        try await Task.sleep(for: .milliseconds(25))
        state = HelperSocket.request(at: path, pid: pid)
    }
    try #require(state == .awaitingCredentials)

    let options: [CFString: Any] = [kSecAttrKeyType: kSecAttrKeyTypeRSA, kSecAttrKeySizeInBits: 2048]
    let key = try #require(SecKeyCreateRandomKey(options as CFDictionary, nil))
    let data = try #require(SecKeyCopyExternalRepresentation(key, nil) as Data?)
    // The fixture generates its PEM at runtime without a static private-key marker in source.
    let keyType = "RSA PRIVATE KEY"
    let pem =
        "-----BEGIN \(keyType)-----\n"
        + data.base64EncodedString(options: [.lineLength64Characters, .endLineWithLineFeed])
        + "\n-----END \(keyType)-----\n"
    let ids = dir.appending(path: "github-app.env")
    try Data("MMC_GITHUB_APP_CLIENT_ID=fixture\nMMC_GITHUB_APP_INSTALLATION_ID=1\n".utf8).write(to: ids)
    let credentials = try MMCAppCredentials.load(identifiersURL: ids) { Data(pem.utf8) }
    state = HelperSocket.request(at: path, pid: pid, credentials: credentials)
    #expect(state == .dispatching)
    state = HelperSocket.request(at: path, pid: pid, credentials: credentials)
    #expect(state == .dispatching)
    state = HelperSocket.request(at: path, pid: pid)
    #expect(state == .dispatching)

    try lifetime.fileHandleForWriting.close()
    let stop = ContinuousClock.now.advanced(by: .seconds(5))
    while child.isRunning && ContinuousClock.now < stop { try await Task.sleep(for: .milliseconds(25)) }
    try #require(!child.isRunning)
    #expect(child.terminationStatus == 0)
    let starts = try String(contentsOf: dir.appending(path: "dispatch.started"), encoding: .utf8)
    #expect(starts == "started\n")
    #expect(FileManager.default.fileExists(atPath: dir.appending(path: "dispatch.stopped").path))
}
