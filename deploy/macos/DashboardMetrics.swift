import Foundation

struct DashboardRoute: Hashable {
    let node: String
    let provider: String
    let scope: String

    var name: String { node.isEmpty ? "未记录节点名" : node }
    var path: String {
        if scope == "front" { return "前置路径" }
        if scope.hasPrefix("chain:") { return "链式路径 · \(scope.dropFirst(6))" }
        return scope.isEmpty ? "路径未记录" : scope
    }
    var context: String { "\(provider.isEmpty ? "提供方未记录" : provider) · \(path)" }
    var title: String { "\(name) · \(context)" }

    func matches(_ query: String) -> Bool {
        query.isEmpty || title.localizedCaseInsensitiveContains(query) || scope.localizedCaseInsensitiveContains(query)
    }
}

struct DashboardProfile: Hashable {
    let kind: String
    let streams: Int
    let requestedBytes: Int64
    let target: String

    var isKnown: Bool {
        ["bench", "sweep", "quick", "confirm"].contains(kind) && (1...8).contains(streams)
            && requestedBytes >= Int64(streams) && !target.isEmpty
    }

    var source: String {
        switch kind {
        case "bench": return "手动测速"
        case "sweep": return "守护进程 · 完整扫描"
        case "quick": return "守护进程 · 快筛"
        case "confirm": return "守护进程 · 复测"
        case "": return "来源未记录"
        default: return "未知方式（\(kind)）"
        }
    }

    var parameters: String {
        "\(streams) 流 · 每节点 \(DashboardFormat.bytes(requestedBytes))"
    }

    var title: String { "\(source) · \(parameters)" }

    var details: String {
        let flows = streams > 0 ? "\(streams)" : "未记录"
        let size = requestedBytes > 0 ? "\(requestedBytes) B" : "未记录"
        return "\(source)（\(kind.isEmpty ? "未知" : kind)） · 并发流数 \(flows) · 请求总量 \(size)\n目标标识  \(target.isEmpty ? "未记录" : target)"
    }
}

struct DashboardSeries: Hashable {
    let route: DashboardRoute
    let profile: DashboardProfile

    var canConnect: Bool { profile.isKnown && !route.scope.isEmpty && !route.node.isEmpty }
}

// The index retains raw observations, including duplicates and errors. The two pickers
// select one dictionary key, so different routes and measurement profiles cannot mix.
struct DashboardHistory {
    let records: [DashboardSeries: [LedgerSample]]
    let routes: [DashboardRoute]
    let profiles: [DashboardRoute: [DashboardSeries]]
    let preferred: [DashboardSeries]
    let nodeCount: Int
    let sampleCount: Int

    init(_ samples: [LedgerSample]) {
        let grouped = Dictionary(grouping: samples) { sample in
            DashboardSeries(route: DashboardRoute(node: sample.node, provider: sample.provider, scope: sample.scope),
                            profile: DashboardProfile(kind: sample.kind, streams: sample.streams,
                                                      requestedBytes: sample.requestedBytes, target: sample.target))
        }.filter { $0.key.profile.isKnown }.mapValues { $0.sorted { $0.time < $1.time } }
        records = grouped
        sampleCount = grouped.values.reduce(0) { $0 + $1.count }
        let ranked = grouped.map { key, rows in
            let hasLine = key.canConnect && zip(rows, rows.dropFirst()).contains { a, b in
                a.successful && b.successful && a.time < b.time
            }
            let hasSpeed = rows.contains(where: \.successful)
            let rank = hasLine ? 0 : hasSpeed && key.canConnect ? 1 : hasSpeed ? 2 : 3
            return (key: key, rank: rank, last: rows.last!.time)
        }.sorted { a, b in
            if a.rank != b.rank { return a.rank < b.rank }
            if a.last != b.last { return a.last > b.last }
            let l = a.key, r = b.key
            if l.route != r.route { return Self.routeOrder(l.route, r.route) }
            let p = l.profile, q = r.profile
            return (p.kind, p.streams, p.requestedBytes, p.target) < (q.kind, q.streams, q.requestedBytes, q.target)
        }
        preferred = ranked.map(\.key)
        profiles = Dictionary(grouping: preferred, by: \.route)
        routes = profiles.keys.sorted(by: Self.routeOrder)
        nodeCount = Dictionary(grouping: routes.filter { !$0.node.isEmpty }, by: \.provider).values.reduce(0) {
            $0 + Set($1.map(\.node)).count
        }
    }

    private static func routeOrder(_ l: DashboardRoute, _ r: DashboardRoute) -> Bool {
        (l.node, l.provider, l.scope) < (r.node, r.provider, r.scope)
    }

    func selection(_ requested: DashboardSeries?, matching query: String) -> DashboardSeries? {
        if let requested, requested.route.matches(query) {
            if records[requested] != nil { return requested }
            if let replacement = profiles[requested.route]?.first { return replacement }
        }
        return preferred.first { $0.route.matches(query) }
    }

    func profileLabel(_ series: DashboardSeries) -> String {
        let label = series.profile.title
        let ambiguous = profiles[series.route]?.contains { $0 != series && $0.profile.title == label } ?? false
        return ambiguous ? "\(label) · \(series.profile.target.prefix(8))" : label
    }
}

enum DashboardPeriod: CaseIterable {
    case day, week, month, all

    var title: String {
        switch self {
        case .day: return "24 小时"
        case .week: return "7 天"
        case .month: return "30 天"
        case .all: return "全部"
        }
    }

    func domain(first: Date?, last: Date?, now: Date) -> ClosedRange<Date> {
        switch self {
        case .day: return now.addingTimeInterval(-86_400)...now
        case .week: return now.addingTimeInterval(-7 * 86_400)...now
        case .month: return now.addingTimeInterval(-30 * 86_400)...now
        case .all:
            if let first, let last, first < last { return first...last }
            let center = first ?? last ?? now
            return center.addingTimeInterval(-30)...center.addingTimeInterval(30)
        }
    }
}

struct DashboardPlot {
    struct Point {
        let time: Date
        let mbps: Double
    }

    let domain: ClosedRange<Date>
    let segments: [[Point]]
    let failures: [Date]
    let ceiling: Double?

    // Input is chronological. A nil speed is an observation with no valid throughput.
    init(series: DashboardSeries, domain: ClosedRange<Date>, readings: [(Date, Double?)]) {
        self.domain = domain
        var runs: [[Point]] = [], failed: [Date] = [], run: [Point] = []
        var peak: Double = 0
        for (time, speed) in readings where domain.contains(time) {
            guard let speed, speed.isFinite, speed > 0 else {
                failed.append(time)
                if !run.isEmpty { runs.append(run); run = [] }
                continue
            }
            if !run.isEmpty && (!series.canConnect || run.last!.time == time) {
                runs.append(run)
                run = []
            }
            run.append(Point(time: time, mbps: speed))
            peak = max(peak, speed)
        }
        if !run.isEmpty { runs.append(run) }
        segments = runs
        failures = failed
        if peak > 0 {
            let magnitude = pow(10, floor(log10(peak)))
            let multiple = [1.0, 1.2, 1.5, 2, 2.5, 3, 4, 5, 6, 8, 10].first { $0 * magnitude >= peak } ?? 10
            let top = multiple * magnitude
            ceiling = top.isFinite && top >= peak ? top : peak
        } else {
            ceiling = nil
        }
    }

    func fraction(at time: Date) -> Double {
        time.timeIntervalSince(domain.lowerBound) / domain.upperBound.timeIntervalSince(domain.lowerBound)
    }
}

enum DashboardFormat {
    static func settled(used: Int64, pending: Int64) -> Int64 {
        max(0, max(0, used) - max(0, pending))
    }

    static func span(since: Date?, lastSeen: Date?) -> TimeInterval? {
        guard let since, let lastSeen, since <= lastSeen else { return nil }
        return lastSeen.timeIntervalSince(since)
    }

    static func bytes(_ value: Int64) -> String {
        var size = Double(value)
        for unit in ["B", "KiB", "MiB", "GiB", "TiB", "PiB"] {
            if abs(size) < 1024 { return "\(number(size)) \(unit)" }
            size /= 1024
        }
        return "\(number(size)) EiB"
    }

    static func number(_ value: Double) -> String {
        if value != 0 && (abs(value) < 0.01 || abs(value) >= 1e7) { return String(format: "%.2g", value) }
        return value.formatted(.number.locale(Locale(identifier: "zh_CN")).precision(.fractionLength(0...2)))
    }

    static func duration(_ seconds: Double) -> String {
        guard seconds.isFinite, seconds >= 0, seconds < Double(Int.max) else { return "未记录" }
        let s = Int(seconds)
        if s >= 86_400 { return "\(s / 86_400) 天 \(s % 86_400 / 3_600) 小时" }
        if s >= 3_600 { return "\(s / 3_600) 小时 \(s % 3_600 / 60) 分" }
        if s >= 60 { return "\(s / 60) 分 \(s % 60) 秒" }
        return "\(number(seconds)) 秒"
    }

    private static func formatter(_ format: String) -> DateFormatter {
        let result = DateFormatter()
        result.locale = Locale(identifier: "zh_CN")
        result.calendar = Calendar(identifier: .gregorian)
        result.timeZone = .autoupdatingCurrent
        result.dateFormat = format
        return result
    }

    static let date = formatter("yyyy年M月d日")
    static let clock = formatter("HH:mm")
    static let stamp = formatter("yyyy-MM-dd HH:mm:ss")
    static let precise = formatter("yyyy-MM-dd HH:mm:ss.SSS Z")
    static let shortStamp = formatter("M/d HH:mm")
    static let day = formatter("M/d")
    static let seconds = formatter("HH:mm:ss")
    static let milliseconds = formatter("HH:mm:ss.SSS")

    static func tick(_ time: Date, in domain: ClosedRange<Date>) -> String {
        let interval = domain.upperBound.timeIntervalSince(domain.lowerBound)
        if interval < 3 { return milliseconds.string(from: time) }
        if interval < 180 { return seconds.string(from: time) }
        if interval <= 3 * 86_400 { return shortStamp.string(from: time) }
        if interval <= 180 * 86_400 { return day.string(from: time) }
        return date.string(from: time)
    }
}
