import Foundation

@main
private enum DashboardDataTests {
    static func main() throws {
        let wire = """
        {
          "budgetBytes":9000000000,"usedBytes":109,"pendingBytes":40,
          "cycleStart":"2026-03-15T00:00:00Z","cycleEnd":"2026-04-15T00:00:00+01:00",
          "lastSweep":"2026-03-15T05:30:00.5+05:30",
          "samples":[
            {"time":"2026-03-15T08:00:00.123456789+08:00","node":"leaf","provider":"","scope":"front",
             "kind":"bench","mbps":16.25,"latencyMS":8,"bytes":23,"attempts":2,"failures":0,
             "error":"","streams":0,"requestedBytes":0,"target":""},
            {"time":"2026-03-15T00:01:00Z","node":"leaf","provider":"p","scope":"front",
             "kind":"bench","mbps":0,"latencyMS":12,"bytes":7,"attempts":2,"failures":1,
             "error":"failed","streams":4,"requestedBytes":5000000000,"target":"fingerprint"}
          ],
          "groups":[
            {"name":"primary","chain":"exit","configured":true,"node":"","since":null,
             "guardSelected":false,"longestNode":"","longestSeconds":0},
            {"name":"archived","chain":"","configured":false,"node":"leaf","since":"2026-03-15T00:01:00Z",
             "lastSeen":"2026-03-15T00:01:03.75Z","guardSelected":true,"longestNode":"old","longestSeconds":8.5}
          ],
          "decisions":[
            {"id":"decision-1","at":"2026-03-15T08:02:00Z","source":"manual","group":"primary","chain":"exit",
             "before":"a","after":"b","action":"switched","trigger":"manualBench","nearBestPct":20,"thresholdMbps":80,
             "latencyMeanMS":120.5,"latencySamples":8,"latencyFactor":1.5,"latencyThresholdMS":180.75,
             "candidates":[{"node":"b","kind":"bench","observedAt":"2026-03-15T07:45:00Z",
                            "mbps":100,"latencyMS":30,"attempts":2,"failures":0,
                            "streams":4,"requestedBytes":16777216,"rank":1,"preferredBand":true,
                            "strategyAttempts":8,"strategyFailures":1,"historyAttempts":6,"historyFailures":1,
                            "historySamples":3,"historyP25Mbps":88,"validationOrder":1,
                            "validation":"passed","selected":true}],
             "followers":[{"group":"direct","before":"a","after":"b","action":"switched"}]}
          ],
          "selectedDecision":{"id":"decision-1","at":"2026-03-15T08:02:00Z","source":"manual","group":"primary",
             "before":"a","after":"b","action":"switched"}
        }
        """
        let snapshot = try DashboardSnapshot.decode(Data(wire.utf8))
        precondition(snapshot.budgetBytes == 9_000_000_000 && snapshot.usedBytes - snapshot.pendingBytes == 69)
        precondition(snapshot.cycleEnd.timeIntervalSince(snapshot.cycleStart) == (31 * 24 - 1) * 3600)
        precondition(snapshot.lastSweep?.timeIntervalSince(snapshot.cycleStart) == 0.5)
        let legacy = snapshot.samples[0], failed = snapshot.samples[1]
        precondition(abs(legacy.time.timeIntervalSince(snapshot.cycleStart) - 0.123456789) < 0.000001)
        precondition(legacy.successful && legacy.streams == 0 && legacy.requestedBytes == 0 && legacy.target.isEmpty)
        precondition(!failed.successful && failed.bytes == 7 && failed.latencyMS == 12 && failed.attempts == 2)
        precondition(failed.failures == 1 && failed.error == "failed" && failed.streams == 4 && failed.requestedBytes == 5_000_000_000)
        let unobserved = snapshot.groups[0], archived = snapshot.groups[1]
        precondition(unobserved.configured && unobserved.chain == "exit" && unobserved.node.isEmpty)
        precondition(unobserved.since == nil && unobserved.lastSeen == nil && !unobserved.guardSelected)
        precondition(!archived.configured && archived.guardSelected && archived.lastSeen?.timeIntervalSince(archived.since!) == 3.75)
        precondition(archived.longestNode == "old" && archived.longestSeconds == 8.5)
        let decision = snapshot.decisionHistory[0]
        precondition(decision.id == "decision-1" && decision.evidence[0].selected == true && decision.evidence[0].streams == 4)
        precondition(decision.latencyMeanMS == 120.5 && decision.latencySamples == 8 && decision.latencyFactor == 1.5)
        precondition(decision.latencyThresholdMS == 180.75)
        precondition(decision.evidence[0].rank == 1 && decision.evidence[0].preferredBand == true && decision.evidence[0].historyP25Mbps == 88)
        precondition(decision.evidence[0].strategyAttempts == 8 && decision.evidence[0].historyAttempts == 6)
        precondition(decision.evidence[0].observedAt?.timeIntervalSince(decision.at) == -17 * 60)
        precondition(decision.evidence[0].validationOrder == 1 && decision.evidence[0].validation == "passed")
        precondition(decision.followResults[0].group == "direct" && snapshot.selectedDecision?.id == decision.id)

        func sample(_ mbps: Double, failures: Int = 0, error: String = "") -> LedgerSample {
            LedgerSample(time: legacy.time, node: "n", provider: "", scope: "front", kind: "bench", mbps: mbps,
                         latencyMS: 0, bytes: 0, attempts: 2, failures: failures, error: error,
                         streams: 0, requestedBytes: 0, target: "")
        }
        for row in [sample(8, error: "failed"), sample(8, failures: 1), sample(0), sample(-1), sample(.infinity)] {
            precondition(!row.successful)
        }
        let group = LedgerGroup(name: "other", chain: "", configured: true, node: "", since: nil, lastSeen: nil,
                                guardSelected: false, longestNode: "", longestSeconds: 0)
        let constructed = DashboardSnapshot(budgetBytes: 1, usedBytes: 0, pendingBytes: 0, cycleStart: snapshot.cycleStart,
                                           cycleEnd: snapshot.cycleEnd, lastSweep: nil, samples: [legacy], groups: [group],
                                           decisions: nil, selectedDecision: nil, decisionWarning: nil)
        func checkSendable<T: Sendable>(_: T) {}
        checkSendable(constructed)
        checkSendable(Set(constructed.samples))
        checkSendable(Set(constructed.groups))

        let invalidDate = wire.replacingOccurrences(of: "2026-03-15T00:00:00Z", with: "unknown")
        do {
            _ = try DashboardSnapshot.decode(Data(invalidDate.utf8))
            fatalError("invalid cycleStart was accepted")
        } catch DecodingError.dataCorrupted {}
        print("Dashboard data tests passed")
    }
}
