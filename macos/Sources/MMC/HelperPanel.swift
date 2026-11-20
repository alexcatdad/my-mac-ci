import MenubarKit
import SwiftUI

extension HelperState {
    var presentation: MenubarStatus {
        switch self {
        case .stopped:
            MenubarStatus(tone: .off, word: "Stopped", headline: "Helper stopped")
        case .starting:
            MenubarStatus(tone: .off, word: "Starting", headline: "Starting helper…")
        case .awaitingCredentials:
            MenubarStatus(
                tone: .warning, word: "Waiting for credentials", headline: "Waiting for GitHub App credentials",
                detail: "The helper is ready. Job dispatch is paused.")
        case .authenticating:
            MenubarStatus(tone: .off, word: "Connecting", headline: "Loading GitHub App credentials…")
        case .dispatching:
            MenubarStatus(tone: .ok, word: "Running", headline: "Dispatch service running")
        case .dispatchFailed:
            MenubarStatus(
                tone: .error, word: "Dispatch failed", headline: "Dispatch service stopped",
                detail: "Check the local configuration and runtime, then try again.")
        case .invalidCredentials:
            MenubarStatus(
                tone: .warning, word: "Invalid credentials", headline: "GitHub App credentials rejected",
                detail: "Check the App identifiers and Keychain private key, then try again.")
        case .credentialError(let message):
            MenubarStatus(
                tone: .warning, word: "Credentials unavailable", headline: "GitHub App credentials unavailable",
                detail: message)
        case .unavailable:
            MenubarStatus(tone: .error, word: "Helper unavailable", headline: "Helper unavailable")
        case .alreadyRunning:
            MenubarStatus(
                tone: .warning, word: "Helper already running", headline: "Another helper is running",
                detail: "Try again once the other helper exits.")
        }
    }

    func presentation(health: HealthSnapshot?) -> MenubarStatus {
        guard self == .dispatching else { return presentation }
        return health?.presentation
            ?? MenubarStatus(tone: .off, word: "Starting dispatch", headline: "Starting dispatch service…")
    }
}

struct HelperPanel: View {
    @ObservedObject var helper: HelperSupervisor

    var body: some View {
        MenubarPanel {
            StatusHeader(helper.state.presentation(health: helper.health))
            if helper.state == .dispatching, let health = helper.health {
                BoundedScrollView(maxHeight: 360) {
                    VStack(alignment: .leading, spacing: 8) {
                        ForEach(health.labels, id: \.name) { label in
                            LabelHealthRow(label: label)
                        }
                        IsolationHealthRow(check: health.isolation)
                    }
                }
            }
            if let reloadError = helper.reloadError { PanelNotice(reloadError) }
            MenuSection(quitTitle: "Quit MMC") {
                if helper.state == .dispatching {
                    Button(helper.reloading ? "Reloading…" : "Reload Configuration") {
                        Task { await helper.reload() }
                    }.disabled(helper.reloading)
                }
                OpenSettingsButton()
                if helper.state.canRetry {
                    Button("Try Again") { helper.retry() }
                }
            }
        }
    }
}

private struct LabelHealthRow: View {
    let label: LabelHealth

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            HStack(alignment: .firstTextBaseline) {
                Text(verbatim: label.label).font(.headline)
                Spacer(minLength: 8)
                Text(verbatim: label.activity).font(.caption)
            }
            Text(verbatim: label.repo).foregroundStyle(.secondary)
            if label.lastContact > 0 {
                LabeledContent("GitHub contact") {
                    Text(Date(timeIntervalSince1970: Double(label.lastContact)), style: .relative)
                }
            } else {
                LabeledContent("GitHub contact", value: "No contact yet")
            }
            Text(verbatim: label.digest).font(.caption.monospaced()).foregroundStyle(.secondary)
                .textSelection(.enabled).accessibilityLabel("Image digest \(label.digest)")
            ForEach(label.observations.filter { $0.active == true }, id: \.key) { issue in
                PanelNotice(issue.title)
            }
        }
        .font(.caption)
        .padding(.horizontal, 10)
        .frame(maxWidth: .infinity, alignment: .leading)
        .groupedTile()
    }
}

private struct IsolationHealthRow: View {
    let check: HealthSnapshot.Isolation
    @State private var expanded = false

    var body: some View {
        DisclosureGroup(isExpanded: $expanded) {
            if check.checkedAt > 0 {
                let date = Date(timeIntervalSince1970: Double(check.checkedAt))
                LabeledContent("Last checked") { Text(date.formatted(date: .abbreviated, time: .standard)) }
            }
            ForEach(Array(check.targets.enumerated()), id: \.offset) { _, target in
                StatusRowView(
                    MenubarStatusRow(title: target.name, value: target.status.title, tone: target.status.tone))
            }
        } label: {
            LabeledContent("Network isolation", value: check.lastStatus?.title ?? "Awaiting check")
        }
        .font(.caption)
        .padding(.horizontal, 10)
        .groupedTile()
    }
}
