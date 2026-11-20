import Testing
@testable import MMC

@MainActor
struct QuitCoordinatorTests {
    @Test func drainsOrStopsAfterOnePrompt() async {
        for choice in [QuitChoice.finish, .stop, .cancel] {
            var replies: [QuitResult] = [.pending, .busy, .busy, .ready]
            var calls: [String] = []
            var prompts = 0
            let result = await QuitCoordinator.shouldQuit(command: {
                calls.append($0)
                return replies.removeFirst()
            }, choose: {
                prompts += 1
                return choice
            }, quitWithoutStatus: { false }, wait: {})
            #expect(prompts == 1)
            #expect(result == (choice != .cancel))
            switch choice {
            case .finish: #expect(calls == Array(repeating: "quit-prepare", count: 4))
            case .stop: #expect(calls == ["quit-prepare", "quit-prepare", "quit-stop", "quit-prepare"])
            case .cancel: #expect(calls == ["quit-prepare", "quit-prepare", "quit-resume"])
            }
        }
    }

    @Test func unavailableRequiresExplicitDecision() async {
        var calls: [String] = []
        let result = await QuitCoordinator.shouldQuit(command: {
            calls.append($0)
            return nil
        }, choose: { Issue.record("Unexpected active job prompt"); return .cancel },
        quitWithoutStatus: { false })
        #expect(!result)
        #expect(calls == ["quit-prepare", "quit-resume"])
    }
}
