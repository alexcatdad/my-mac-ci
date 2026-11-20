import Foundation
import MenubarKit

enum IsolationResult: String, Decodable {
    case pass, fail, inconclusive

    var title: String {
        switch self {
        case .pass: "Passed"
        case .fail: "Failed"
        case .inconclusive: "Inconclusive"
        }
    }

    var tone: StatusTone {
        switch self {
        case .pass: .ok
        case .fail: .error
        case .inconclusive: .warning
        }
    }
}

struct LabelHealth: Decodable {
    let repo: String
    let label: String
    let starting: Int
    let running: Int
    let digest: String
    let lastContact: Int64
    let stale: Bool
    let startFailed: Bool
    let sessionConflict: Bool
    let listenerFailure: Bool
    let versionBehind: Bool?
    let imageUnavailable: Bool?

    var name: String { "\(repo) · \(label)" }

    var activity: String {
        var parts = [String]()
        if running > 0 { parts.append("\(running) running") }
        if starting > 0 { parts.append("\(starting) starting") }
        return parts.isEmpty ? "Idle" : parts.joined(separator: ", ")
    }

    var observations: [HealthObservation] {
        [
            HealthObservation("GitHub contact overdue", active: stale, source: name),
            HealthObservation("Runner start failed", active: startFailed, source: name),
            HealthObservation("Listener session conflict", active: sessionConflict, source: name),
            HealthObservation("GitHub listener unavailable", active: listenerFailure && !sessionConflict, source: name),
            HealthObservation("Runner image out of date", active: versionBehind, source: name),
            HealthObservation("Runner image unavailable", active: imageUnavailable, source: name),
        ]
    }
}

struct HealthSnapshot: Decodable {
    struct Isolation: Decodable {
        struct Target: Decodable {
            let name: String
            let status: IsolationResult
        }
        let status: IsolationResult
        let lastStatus: IsolationResult?
        let checkedAt: Int64
        let targets: [Target]
    }
    let labels: [LabelHealth]
    let isolation: Isolation

    var presentation: MenubarStatus {
        if isolation.status != .pass {
            return MenubarStatus(
                tone: isolation.status.tone, word: "Isolation needs attention",
                headline: isolation.status == .fail ? "Isolation check failed" : "Isolation unconfirmed",
                detail: "New runner starts are paused.")
        }
        if let issue = labels.lazy.flatMap(\.observations).first(where: { $0.active == true }) {
            return MenubarStatus(tone: .warning, word: "Needs attention", headline: issue.title, detail: issue.source)
        }
        return HelperState.dispatching.presentation
    }

    var observations: [HealthObservation] {
        let isolationIssue = HealthObservation(
            "Isolation check needs attention",
            active: isolation.lastStatus.map { $0 != .pass }, source: "Network isolation")
        return [isolationIssue] + labels.flatMap(\.observations)
    }
}

struct HealthObservation {
    let title: String
    let active: Bool?
    let source: String
    var key: String { "\(title) · \(source)" }

    init(_ title: String, active: Bool?, source: String) {
        self.title = title
        self.active = active
        self.source = source
    }
}

struct HealthIncidents {
    private var tracker: IncidentDeduplicator<String>

    init(reported: Set<String> = []) {
        tracker = IncidentDeduplicator(reportedIncidents: reported)
    }

    var reported: Set<String> { tracker.reportedIncidents }

    mutating func observe(_ observations: [HealthObservation]) -> [MenubarNotification] {
        // Removing a label deliberately ends its incidents without a recovery notification.
        for removed in reported.subtracting(observations.map(\.key)) { tracker.reset(removed) }
        return observations.compactMap { observation in
            guard let active = observation.active,
                let change = tracker.observe(observation.key, isActive: active)
            else { return nil }
            let title: String
            switch change {
            case .incident: title = observation.title
            case .recovered: title = "Resolved · " + observation.title
            }
            return MenubarNotification(
                title: title, body: observation.source, threadIdentifier: observation.key,
                identifier: title + " · " + observation.source)
        }
    }
}
