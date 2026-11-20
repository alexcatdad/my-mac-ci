import MenubarKit
import ServiceManagement
import SwiftUI

struct MMCSettings: View {
    let notifier: MenubarNotifier
    @Environment(\.scenePhase) private var phase
    @State private var loginStatus = SMAppService.mainApp.status
    @State private var changing = false
    @State private var loginError: String?

    var body: some View {
        SettingsForm {
            NotificationSettingsSection(notifier: notifier, toggleTitle: "Notify me about CI failures and recoveries")
            Section("Startup") {
                Toggle(
                    "Launch at login",
                    isOn: Binding(
                        get: { loginStatus == .enabled || loginStatus == .requiresApproval },
                        set: { value in Task { await setLoginItem(value) } })
                )
                .disabled(changing)
                if loginStatus == .requiresApproval {
                    SettingsNote("Approve MMC in Login Items to finish enabling launch at login.")
                    Button("Open Login Items…") { SMAppService.openSystemSettingsLoginItems() }
                }
                if let loginError { SettingsNote(loginError) }
            }
            FirewallSettings()
        }
        .onAppear { loginStatus = SMAppService.mainApp.status }
        .onChange(of: phase) { _ in loginStatus = SMAppService.mainApp.status }
    }

    @MainActor private func setLoginItem(_ enabled: Bool) async {
        guard !changing else { return }
        changing = true
        defer {
            changing = false
            loginStatus = SMAppService.mainApp.status
        }
        loginError = nil
        do {
            if enabled { try SMAppService.mainApp.register() } else { try await SMAppService.mainApp.unregister() }
        } catch {
            loginError = "MMC could not change its launch-at-login setting."
        }
    }
}
