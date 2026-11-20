import Foundation
import Security

enum MMCAppCredentialError: Error, LocalizedError, Equatable {
    case identifiersUnavailable, keyUnavailable, keyAccessDenied

    var errorDescription: String? {
        switch self {
        case .identifiersUnavailable: "Check the GitHub App IDs in github-app.env."
        case .keyUnavailable: "Check the MMC GitHub App key in your login Keychain."
        case .keyAccessDenied: "Click Try Again to let macOS ask for access to the GitHub App key."
        }
    }
}

struct MMCAppCredentials: Codable, Sendable {
    let clientID: String
    let installationID: Int64
    let privateKey: String

    static func load(allowInteraction: Bool) throws -> Self {
        let home = FileManager.default.homeDirectoryForCurrentUser
        return try load(
            identifiersURL: home.appending(path: "Library/Application Support/my-mac-ci/github-app.env")
        ) {
            try readKey(at: home.appending(path: "Library/Keychains/login.keychain-db"), allowInteraction: allowInteraction)
        }
    }

    static func load(identifiersURL: URL, readKey: () throws -> Data) throws -> Self {
        guard let contents = try? String(contentsOf: identifiersURL, encoding: .utf8) else {
            throw MMCAppCredentialError.identifiersUnavailable
        }
        var clientID: String?
        var installationID: Int64?
        for rawLine in contents.split(whereSeparator: \.isNewline) {
            var line = rawLine.trimmingCharacters(in: .whitespaces)[...]
            if line.hasPrefix("export ") { line = line.dropFirst(7) }
            let parts = line.split(separator: "=", maxSplits: 1, omittingEmptySubsequences: false)
            guard parts.count == 2,
                parts[0] == "MMC_GITHUB_APP_CLIENT_ID" || parts[0] == "MMC_GITHUB_APP_INSTALLATION_ID"
            else { continue }
            var value = parts[1]
            if let quote = value.first, quote == "'" || quote == "\"", value.count >= 2, value.last == quote {
                value = value.dropFirst().dropLast()
            }
            guard !value.isEmpty,
                value.allSatisfy({ $0.isASCII && ($0.isLetter || $0.isNumber || $0 == "-" || $0 == "_") })
            else { throw MMCAppCredentialError.identifiersUnavailable }
            if parts[0] == "MMC_GITHUB_APP_CLIENT_ID", clientID == nil {
                clientID = String(value)
            } else if parts[0] == "MMC_GITHUB_APP_INSTALLATION_ID", installationID == nil, let id = Int64(value), id > 0
            {
                installationID = id
            } else {
                throw MMCAppCredentialError.identifiersUnavailable
            }
        }
        guard let clientID, let installationID else { throw MMCAppCredentialError.identifiersUnavailable }
        guard let key = String(data: try readKey(), encoding: .utf8), !key.isEmpty else {
            throw MMCAppCredentialError.keyUnavailable
        }
        return Self(clientID: clientID, installationID: installationID, privateKey: key)
    }

    private static let interactionLock = NSLock()

    // The owner's item is in the file-based login Keychain. Its interaction
    // setting is process-wide; restore it before another MMC reader enters.
    #if compiler(>=6.4)
        @diagnose(DeprecatedDeclaration, as: ignored)
    #endif
    private static func readKey(at url: URL, allowInteraction: Bool) throws -> Data {
        try interactionLock.withLock {
            var previous = DarwinBoolean(false)
            guard SecKeychainGetUserInteractionAllowed(&previous) == errSecSuccess,
                SecKeychainSetUserInteractionAllowed(allowInteraction) == errSecSuccess
            else { throw MMCAppCredentialError.keyAccessDenied }
            defer { _ = SecKeychainSetUserInteractionAllowed(previous.boolValue) }

            var keychain: SecKeychain?
            guard SecKeychainOpen(url.path, &keychain) == errSecSuccess, let keychain else {
                throw MMCAppCredentialError.keyUnavailable
            }
            var flags: SecKeychainStatus = 0
            guard SecKeychainGetStatus(keychain, &flags) == errSecSuccess else {
                throw MMCAppCredentialError.keyUnavailable
            }
            guard allowInteraction || flags & UInt32(kSecUnlockStateStatus) != 0 else { throw MMCAppCredentialError.keyAccessDenied }
            let query: [String: Any] = [
                kSecClass as String: kSecClassGenericPassword,
                kSecMatchSearchList as String: [keychain],
                kSecAttrService as String: "com.alexcatdad.my-mac-ci",
                kSecAttrAccount as String: "github-app-mac",
                kSecReturnData as String: true,
                kSecMatchLimit as String: kSecMatchLimitOne,
            ]
            var result: CFTypeRef?
            switch SecItemCopyMatching(query as CFDictionary, &result) {
            case errSecSuccess:
                guard let data = result as? Data else { throw MMCAppCredentialError.keyUnavailable }
                return data
            case errSecInteractionNotAllowed, errSecInteractionRequired, errSecAuthFailed, errSecUserCanceled:
                throw MMCAppCredentialError.keyAccessDenied
            default:
                throw MMCAppCredentialError.keyUnavailable
            }
        }
    }
}
