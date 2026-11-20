import AppKit

enum QuitChoice {
    case finish, stop, cancel
}

@MainActor
enum QuitCoordinator {
    static func shouldQuit(
        command: (String) async -> QuitResult?,
        choose: () -> QuitChoice,
        quitWithoutStatus: () -> Bool,
        wait: () async -> Void = { try? await Task.sleep(for: .seconds(1)) }
    ) async -> Bool {
        var operation = "quit-prepare"
        var choice: QuitChoice?
        while true {
            let result = await command(operation)
            operation = "quit-prepare"
            switch result {
            case .ready:
                return true
            case .busy where choice == nil:
                choice = choose()
                if choice == .stop { operation = "quit-stop" }
                if choice == .cancel {
                    _ = await command("quit-resume")
                    return false
                }
            case .busy, .pending:
                await wait()
            case .unavailable, nil:
                if quitWithoutStatus() { return true }
                _ = await command("quit-resume")
                return false
            }
        }
    }

    static func choose() -> QuitChoice {
        let alert = NSAlert()
        alert.messageText = "Jobs are running"
        alert.informativeText = "MMC can wait for these jobs to finish before quitting."
        alert.addButton(withTitle: "Finish jobs and quit")
        alert.addButton(withTitle: "Stop jobs and quit").hasDestructiveAction = true
        alert.addButton(withTitle: "Cancel").keyEquivalent = "\u{1b}"
        NSApp.activate(ignoringOtherApps: true)
        switch alert.runModal() {
        case .alertFirstButtonReturn: return .finish
        case .alertSecondButtonReturn: return .stop
        default: return .cancel
        }
    }

    static func quitWithoutStatus() -> Bool {
        let alert = NSAlert()
        alert.messageText = "Active jobs could not be checked"
        alert.informativeText = "Quitting will leave any running jobs to finish."
        alert.addButton(withTitle: "Cancel")
        alert.addButton(withTitle: "Quit")
        NSApp.activate(ignoringOtherApps: true)
        return alert.runModal() == .alertSecondButtonReturn
    }
}
