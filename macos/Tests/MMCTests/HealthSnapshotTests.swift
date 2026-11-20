import Foundation
import Testing

@testable import MMC

@Test func statusSnapshotShowsLabelAndIsolationDetails() throws {
    let reply = try JSONDecoder().decode(
        HelperReply.self,
        from: Data(
            """
            {"state":"dispatching","health":{
                "labels":[{"repo":"example/project","label":"linux","digest":"sha256:example","starting":1,"running":2,
                "lastContact":1700000000,"stale":false,"startFailed":false,"sessionConflict":false,"listenerFailure":false,
                "versionBehind":true}],
                "isolation":{"status":"pass","lastStatus":"pass","checkedAt":1700000001,"targets":[{"name":"control to GitHub","status":"pass"}]}}}
            """.utf8))
    let health = try #require(reply.health)
    #expect(health.labels.first?.activity == "2 running, 1 starting")
    #expect(health.labels.first?.digest == "sha256:example")
    #expect(health.presentation.tone == .warning)
    #expect(health.presentation.headline == "Runner image out of date")
    #expect(health.isolation.targets.first?.status == .pass)
    #expect(health.isolation.checkedAt == 1_700_000_001)
}

@Test func healthIncidentsDeduplicateAcrossLaunchesAndForgetRemovedLabels() {
    let issue = HealthObservation("Runner start failed", active: true, source: "example · linux")
    var firstLaunch = HealthIncidents()
    #expect(firstLaunch.observe([issue]).count == 1)
    var secondLaunch = HealthIncidents(reported: firstLaunch.reported)
    #expect(secondLaunch.observe([issue]).isEmpty)
    #expect(secondLaunch.observe([HealthObservation(issue.title, active: nil, source: issue.source)]).isEmpty)
    let recovered = HealthObservation(issue.title, active: false, source: issue.source)
    #expect(secondLaunch.observe([recovered]).count == 1)
    #expect(secondLaunch.observe([recovered]).isEmpty)
    #expect(secondLaunch.observe([issue]).count == 1)
    #expect(secondLaunch.observe([]).isEmpty)
    #expect(secondLaunch.reported.isEmpty)
}

@Test func recheckPausesStartsWithoutReportingAnUnverifiedFailure() throws {
    let snapshot = try JSONDecoder().decode(
        HealthSnapshot.self,
        from: Data(
            """
            {"labels":[],"isolation":{"status":"inconclusive","lastStatus":"pass","checkedAt":1700000000,"targets":[]}}
            """.utf8))
    #expect(snapshot.presentation.tone == .warning)
    var incidents = HealthIncidents()
    #expect(incidents.observe(snapshot.observations).isEmpty)
}
