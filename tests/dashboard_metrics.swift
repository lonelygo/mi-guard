import Foundation

@main
private enum DashboardMetricsTests {
    static func main() {
        let start = Date(timeIntervalSince1970: 1_600_000_000)
        func at(_ seconds: Double) -> Date { start.addingTimeInterval(seconds) }
        func sample(_ seconds: Double, node: String = "Tokyo", provider: String = "A", scope: String = "front",
                    kind: String = "bench", streams: Int = 4, request: Int64 = 4096, target: String = "one",
                    error: String = "") -> LedgerSample {
            LedgerSample(time: at(seconds), node: node, provider: provider, scope: scope, kind: kind, mbps: 56.123456,
                         latencyMS: 17, bytes: 71, attempts: 2, failures: error.isEmpty ? 0 : 1, error: error,
                         streams: streams, requestedBytes: request, target: target)
        }
        let changes = [sample(8, node: "Osaka"), sample(8, provider: "B"), sample(8, scope: "chain:Tokyo"),
                       sample(8, kind: "sweep"), sample(8, streams: 2), sample(8, request: 2048), sample(8, target: "two")]
        let old = sample(300, streams: 0, request: 0, target: "")
        let failed = sample(4, error: "HTTP 504")
        let history = DashboardHistory(changes + [sample(3), failed, sample(0), old, old])
        let focus = history.selection(nil, matching: "")!
        precondition(history.records.count == changes.count + 2 && history.routes.count == 4)
        precondition(history.sampleCount == 12 && history.nodeCount == 3)
        precondition(history.records[focus] == [sample(0), sample(3), failed])
        precondition(history.selection(focus, matching: "osAkA")?.route.node == "Osaka")
        precondition(history.selection(focus, matching: "no-such-route") == nil)
        let legacy = history.preferred.first { !$0.profile.isKnown }!
        precondition(history.records[legacy]?.count == 2 && history.selection(legacy, matching: "") == legacy)
        let changed = DashboardHistory([sample(8, target: "two"), sample(9, node: "Seoul")])
        precondition(changed.selection(focus, matching: "")?.profile.target == "two")
        precondition(DashboardHistory([]).selection(focus, matching: "") == nil)

        let domain = DashboardPeriod.all.domain(first: at(0), last: at(300), now: at(9_000_000))
        precondition(domain == at(0)...at(300))
        let now = at(4_000_000)
        for (period, days) in [(DashboardPeriod.day, 1), (.week, 7), (.month, 30)] {
            let range = period.domain(first: at(0), last: at(300), now: now)
            precondition(range.upperBound == now && now.timeIntervalSince(range.lowerBound) == Double(days * 86_400))
            precondition(!range.contains(start) && range.contains(range.lowerBound))
        }
        let readings: [(Date, Double?)] = [(at(-1), 55), (at(0), 3), (at(3), 9), (at(4), nil), (at(300), 7)]
        let plot = DashboardPlot(series: focus, domain: domain, readings: readings)
        precondition(plot.segments.map { $0.map(\.mbps) } == [[3, 9], [7]])
        precondition(plot.failures == [at(4)] && plot.ceiling! >= 9)
        precondition(abs(plot.fraction(at: at(3)) - 0.01) < 1e-12)
        let scattered = DashboardPlot(series: legacy, domain: domain, readings: readings)
        precondition(scattered.segments.map(\.count) == [1, 1, 1] && scattered.failures == plot.failures)
        let invalid = [sample(1, scope: ""), sample(1, kind: "unrecognized"), sample(1, streams: 9),
                       sample(1, request: 3), sample(1, target: "")]
        precondition(DashboardHistory(invalid).preferred.allSatisfy { !$0.canConnect })
        let singleton = DashboardPeriod.all.domain(first: start, last: start, now: now)
        let point = DashboardPlot(series: focus, domain: singleton, readings: [(start, 5)])
        precondition(point.fraction(at: start) == 0.5 && point.segments.map(\.count) == [1])
        let ties = DashboardPlot(series: focus, domain: singleton, readings: [(start, 5), (start, 9)])
        precondition(ties.segments.map(\.count) == [1, 1])
        let bad = DashboardPlot(series: focus, domain: singleton, readings: [(start, nil), (start, .nan), (start, 0)])
        precondition(bad.ceiling == nil && bad.segments.isEmpty && bad.failures.count == 3)
        let empty = DashboardPlot(series: focus, domain: domain, readings: [])
        precondition(empty.ceiling == nil && empty.failures.isEmpty && empty.segments.isEmpty)

        precondition(DashboardFormat.settled(used: 100, pending: 40) == 60)
        precondition(DashboardFormat.span(since: at(3), lastSeen: at(9.25)) == 6.25)
        precondition(DashboardFormat.span(since: start, lastSeen: start) == 0)
        precondition(DashboardFormat.span(since: start, lastSeen: nil) == nil)
        precondition(DashboardFormat.span(since: now, lastSeen: start) == nil)
        print("Dashboard metrics tests passed")
    }
}
