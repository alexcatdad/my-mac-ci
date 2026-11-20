import AppKit
import Combine
import MenubarKit
import SwiftUI

@main
struct MMCApp: App {
    @NSApplicationDelegateAdaptor(AppDelegate.self) private var delegate

    var body: some Scene {
        Settings { MMCSettings(notifier: delegate.helper.notifier) }
    }
}

@MainActor
final class AppDelegate: NSObject, NSApplicationDelegate {
    let helper = HelperSupervisor()
    private var statusItem: NSStatusItem?
    private var statusUpdates: AnyCancellable?
    private let popover = NSPopover()

    func applicationDidFinishLaunching(_ notification: Notification) {
        NSApp.setActivationPolicy(.accessory)
        installStatusItem()
        helper.start()
    }

    private func installStatusItem() {
        let item = NSStatusBar.system.statusItem(withLength: NSStatusItem.variableLength)
        item.autosaveName = "MMC"
        statusItem = item
        guard let button = item.button else { return }
        button.target = self
        button.action = #selector(statusItemClicked(_:))
        button.sendAction(on: [.leftMouseUp, .rightMouseUp])
        let dev = Bundle.main.bundleIdentifier?.hasSuffix(".dev") == true
        if dev {
            button.title = "DEV"
            button.imagePosition = .imageLeading
            button.font = .systemFont(ofSize: NSFont.smallSystemFontSize, weight: .semibold)
        }
        let icon = TrayIconLoader.load(resourcesDirectory: Bundle.main.resourceURL, baseName: "MMCTrayIcon")
        statusUpdates = helper.$state.combineLatest(helper.$health)
            .map { state, health in state.presentation(health: health) }
            .removeDuplicates()
            .sink { [weak button] status in
                button?.image = TrayIconComposer.image(icon: icon, fallbackSymbol: "macmini", tone: status.tone)
                button?.setAccessibilityLabel(TrayAccessibility.label(appName: dev ? "MMC DEV" : "MMC", statusWord: status.word))
            }
        let content = NSHostingController(rootView: HelperPanel(helper: helper))
        content.sizingOptions = [.preferredContentSize]
        popover.contentViewController = content
        popover.behavior = .transient
    }

    @objc private func statusItemClicked(_ button: NSStatusBarButton) {
        let event = NSApp.currentEvent
        if event?.type == .rightMouseUp || event?.modifierFlags.contains(.control) == true {
            popover.performClose(nil)
            let menu = NSMenu()
            menu.addItem(withTitle: "Quit MMC", action: #selector(NSApplication.terminate(_:)), keyEquivalent: "q").target = NSApp
            menu.popUp(positioning: nil, at: NSPoint(x: button.bounds.minX, y: button.bounds.minY), in: button)
        } else if popover.isShown {
            popover.performClose(nil)
        } else {
            popover.show(relativeTo: button.bounds, of: button, preferredEdge: .minY)
            popover.contentViewController?.view.window?.makeKey()
        }
    }

    private var quitTask: Task<Void, Never>?

    func applicationShouldTerminate(_ sender: NSApplication) -> NSApplication.TerminateReply {
        guard helper.ownsHelper else { return .terminateNow }
        if quitTask == nil {
            quitTask = Task {
                let quit = await QuitCoordinator.shouldQuit(
                    command: { await self.helper.command($0)?.quit },
                    choose: QuitCoordinator.choose,
                    quitWithoutStatus: QuitCoordinator.quitWithoutStatus)
                quitTask = nil
                sender.reply(toApplicationShouldTerminate: quit)
            }
        }
        return .terminateLater
    }

    func applicationWillTerminate(_ notification: Notification) {
        helper.stop()
    }
}
