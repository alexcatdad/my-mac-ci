import Foundation
import Testing

@testable import MMC

private func withIDs(_ text: String, test: (URL) throws -> Void) throws {
    let url = FileManager.default.temporaryDirectory.appending(path: UUID().uuidString)
    defer { try? FileManager.default.removeItem(at: url) }
    try text.write(to: url, atomically: true, encoding: .utf8)
    try test(url)
}

@Test func appCredentialsUseOnlyExplicitIdentifiersAndKeychainReader() throws {
    try withIDs(
        """
        export MMC_GITHUB_APP_CLIENT_ID='test-client'
        MMC_GITHUB_APP_KEY_FILE="/ignored/legacy.pem"
        MMC_GITHUB_APP_INSTALLATION_ID=42
        MMC_GITHUB_APP_PRIVATE_KEY=ignored
        """
    ) { url in
        let credentials = try MMCAppCredentials.load(identifiersURL: url) { Data("synthetic".utf8) }
        #expect(credentials.clientID == "test-client")
        #expect(credentials.installationID == 42)
        #expect(credentials.privateKey == "synthetic")
        #expect(throws: MMCAppCredentialError.keyAccessDenied) {
            try MMCAppCredentials.load(identifiersURL: url) { throw MMCAppCredentialError.keyAccessDenied }
        }
    }
}

@Test func appCredentialsRejectMissingDuplicateOrShellValuesBeforeReadingKey() throws {
    for text in [
        "MMC_GITHUB_APP_CLIENT_ID=x",
        "MMC_GITHUB_APP_CLIENT_ID=x\nMMC_GITHUB_APP_INSTALLATION_ID=0",
        "MMC_GITHUB_APP_CLIENT_ID=x\nMMC_GITHUB_APP_INSTALLATION_ID=2\nMMC_GITHUB_APP_INSTALLATION_ID=3",
        "MMC_GITHUB_APP_CLIENT_ID=$OTHER_ID\nMMC_GITHUB_APP_INSTALLATION_ID=1",
    ] {
        try withIDs(text) { url in
            #expect(throws: MMCAppCredentialError.identifiersUnavailable) {
                try MMCAppCredentials.load(identifiersURL: url) {
                    Issue.record("Invalid identifiers reached the Keychain reader")
                    return Data()
                }
            }
        }
    }
}
