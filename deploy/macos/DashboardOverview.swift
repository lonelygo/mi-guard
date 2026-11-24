import SwiftUI

struct DashboardComparison {
    struct Row: Identifiable {
        let series: DashboardSeries
        let sample: LedgerSample
        var id: DashboardRoute { series.route }
    }

    let comparable: Bool
    let rows: [Row]

    init(history: DashboardHistory, focus: DashboardSeries, domain: ClosedRange<Date>) {
        comparable = focus.canConnect
        var latest: [DashboardRoute: Row] = [:]
        for series in history.preferred {
            if focus.canConnect && (series.profile != focus.profile || series.route.scope != focus.route.scope) { continue }
            guard let sample = history.records[series]?.last(where: { domain.contains($0.time) }) else { continue }
            if let previous = latest[series.route], previous.sample.time >= sample.time { continue }
            latest[series.route] = Row(series: series, sample: sample)
        }
        rows = latest.values.sorted { a, b in
            if focus.canConnect {
                if a.sample.successful != b.sample.successful { return a.sample.successful }
                if a.sample.successful && a.sample.mbps != b.sample.mbps { return a.sample.mbps > b.sample.mbps }
            }
            if a.sample.time != b.sample.time { return a.sample.time > b.sample.time }
            return a.series.route.title < b.series.route.title
        }
    }
}

struct DashboardOverview: View {
    let history: DashboardHistory
    let focus: DashboardSeries
    let period: DashboardPeriod
    let onNode: (DashboardSeries) -> Void

    var body: some View {
        let values = history.records[focus] ?? []
        let domain = period == .all ? Date.distantPast...Date.distantFuture
                                    : period.domain(first: values.first?.time, last: values.last?.time, now: Date())
        let comparison = DashboardComparison(history: history, focus: focus, domain: domain)
        let maximum = comparison.rows.first?.sample.mbps ?? 0
        VStack(alignment: .leading, spacing: 9) {
                Label(comparison.comparable ? "相同测法的节点" : "最近测速节点", systemImage: "chart.bar.xaxis")
                    .font(.headline)
                if comparison.rows.isEmpty {
                    Text("所选时段没有测量值").font(.callout).foregroundColor(.secondary)
                }
                ForEach(comparison.rows.prefix(3)) { row in
                    Button {
                        onNode(row.series)
                    } label: {
                        HStack(spacing: 8) {
                            Text(verbatim: row.series.route.name).lineLimit(1).truncationMode(.middle)
                            Spacer(minLength: 2)
                            if comparison.comparable, maximum > 0, row.sample.successful {
                                GeometryReader { geo in
                                    ZStack(alignment: .leading) {
                                        Capsule().fill(Color.secondary.opacity(0.15))
                                        Capsule().fill(DashboardStyle.accent).frame(width: geo.size.width * min(1, row.sample.mbps / maximum))
                                    }
                                }.frame(width: 45, height: 4).accessibilityHidden(true)
                            } else if !comparison.comparable {
                                Text(row.series.profile.isKnown ? "\(row.series.profile.streams) 流" : "流数未知")
                                    .font(.caption).foregroundColor(.secondary)
                            }
                            Text(verbatim: row.sample.successful ? "\(DashboardFormat.number(row.sample.mbps)) Mbps" : "测速失败")
                                .foregroundColor(row.sample.successful ? .primary : Color(nsColor: .systemRed)).monospacedDigit()
                        }.font(.callout).contentShape(Rectangle())
                    }.buttonStyle(.plain)
                    .help("\(row.series.route.context) · \(row.series.profile.title) · \(DashboardFormat.stamp.string(from: row.sample.time))")
                }
                Text(comparison.comparable ? "按各节点在所选时段的最后一次结果比较" : "按最后测速时间排列")
                    .font(.caption).foregroundColor(.secondary)
        }.dashboardCard(13)
    }
}
