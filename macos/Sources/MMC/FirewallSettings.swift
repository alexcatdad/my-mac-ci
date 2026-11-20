import ServiceManagement
import SwiftUI

struct FirewallSettings: View {
    private static let plist = "com.alexcatdad.my-mac-ci.pf.plist"
    private let service = SMAppService.daemon(plistName: plist)
    @State private var status = SMAppService.Status.notRegistered
    @State private var failed = false

    var body: some View {
        if FileManager.default.fileExists(
            atPath: Bundle.main.bundleURL.appending(path: "Contents/Library/LaunchDaemons/\(Self.plist)").path)
        {
            VStack(alignment: .leading) {
                switch status {
                case .enabled:
                    Label("Firewall enabled at boot", systemImage: "checkmark.shield")
                case .requiresApproval:
                    Button("Approve Firewall in Login Items…") { SMAppService.openSystemSettingsLoginItems() }
                default:
                    Button("Enable Firewall at Boot…") {
                        do {
                            try service.register()
                            failed = false
                        } catch {
                            failed = true
                        }
                        status = service.status
                        if status == .requiresApproval { SMAppService.openSystemSettingsLoginItems() }
                    }
                    if failed {
                        Text("Place the signed MMC app in Applications and check Login Items for firewall approval.")
                    }
                }
            }
            .onAppear { status = service.status }
            .onReceive(NotificationCenter.default.publisher(for: NSApplication.didBecomeActiveNotification)) { _ in
                status = service.status
            }
        }
    }
}
