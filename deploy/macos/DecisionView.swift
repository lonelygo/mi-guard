import SwiftUI

struct ManagedRoutesCard: View {
    let groups: [LedgerGroup]
    let decisions: [DecisionRecord]
    let onSelections: () -> Void
    let onDecision: (String) -> Void

    var body: some View {
        VStack(alignment: .leading, spacing: 11) {
            HStack {
                Label("纳管线路", systemImage: "arrow.triangle.branch").font(.headline)
                Spacer()
                Button("全部记录", action: onSelections).buttonStyle(.link).font(.caption)
            }
            if groups.isEmpty {
                Text("当前配置没有纳管线路。知情的普通组决策仍会显示在线路选用页。")
                    .font(.callout).foregroundColor(.secondary)
            } else {
                ForEach(groups.prefix(5), id: \.name) { group in
                    let latest = decisions.first { $0.group == group.name }
                    Button {
                        if let latest { onDecision(latest.id) } else { onSelections() }
                    } label: {
                        HStack(spacing: 10) {
                            VStack(alignment: .leading, spacing: 3) {
                                Text(verbatim: group.name).font(.callout.weight(.semibold)).lineLimit(1).truncationMode(.middle)
                                Text(verbatim: group.chain.isEmpty ? "手动策略组" : "前置组 · \(group.chain)")
                                    .font(.caption).foregroundColor(.secondary).lineLimit(1).truncationMode(.middle)
                            }
                            Spacer(minLength: 4)
                            VStack(alignment: .trailing, spacing: 3) {
                                Text(verbatim: group.node.isEmpty ? "尚无观测" : group.node)
                                    .font(.callout.weight(.medium)).lineLimit(1).truncationMode(.middle)
                                Text(verbatim: observedSpan(group)).font(.caption.monospacedDigit()).foregroundColor(.secondary)
                            }
                            Image(systemName: "chevron.right").font(.caption).foregroundColor(.secondary)
                        }.contentShape(Rectangle())
                    }.buttonStyle(.plain)
                }
            }
        }.dashboardCard()
    }

    private func observedSpan(_ group: LedgerGroup) -> String {
        guard let span = DashboardFormat.span(since: group.since, lastSeen: group.lastSeen) else { return "时长未记录" }
        return span == 0 && !group.guardSelected ? "首次观察" : "已观测 \(DashboardFormat.duration(span))"
    }
}

struct DecisionHistoryPanel: View {
    let decisions: [DecisionRecord]
    let selected: DecisionRecord?
    let requestedID: String?
    let onDecision: (String) -> Void

    var body: some View {
        if let focus = selected ?? (requestedID == nil ? decisions.first : nil) {
            DecisionDetailCard(decision: focus)
        }
        VStack(alignment: .leading, spacing: 10) {
            HStack {
                Label("近期决策", systemImage: "clock.arrow.circlepath").font(.headline)
                Spacer()
                Text("保留最近记录").font(.caption).foregroundColor(.secondary)
            }
            if decisions.isEmpty {
                Text("新版本完成一次测速判断后，这里会显示保持、切换、跳过和失败的证据。")
                    .font(.callout).foregroundColor(.secondary)
            } else {
                ForEach(decisions.prefix(12)) { decision in
                    Button { onDecision(decision.id) } label: {
                        HStack(spacing: 9) {
                            Image(systemName: DecisionAppearance.symbol(decision.action))
                                .foregroundColor(DecisionAppearance.color(decision.action)).frame(width: 17)
                            VStack(alignment: .leading, spacing: 2) {
                                Text(verbatim: decision.group).font(.callout.weight(.medium)).lineLimit(1).truncationMode(.middle)
                                Text(verbatim: DecisionAppearance.route(decision)).font(.caption).foregroundColor(.secondary)
                                    .lineLimit(1).truncationMode(.middle)
                            }
                            Spacer(minLength: 4)
                            Text(verbatim: DashboardFormat.stamp.string(from: decision.at))
                                .font(.caption.monospacedDigit()).foregroundColor(.secondary)
                            Image(systemName: "chevron.right").font(.caption).foregroundColor(.secondary)
                        }
                        .padding(.vertical, 3).contentShape(Rectangle())
                        .background((requestedID == decision.id || selected?.id == decision.id) ? DashboardStyle.accent.opacity(0.1) : .clear)
                    }.buttonStyle(.plain)
                }
            }
        }.dashboardCard()
    }
}

private struct DecisionDetailCard: View {
    let decision: DecisionRecord

    var body: some View {
        VStack(alignment: .leading, spacing: 13) {
            HStack(alignment: .top, spacing: 12) {
                Image(systemName: DecisionAppearance.symbol(decision.action))
                    .font(.title2).foregroundColor(DecisionAppearance.color(decision.action)).frame(width: 28)
                VStack(alignment: .leading, spacing: 4) {
                    Text(verbatim: decision.group).font(.title3.weight(.semibold)).lineLimit(2)
                    Text(verbatim: "\(DecisionAppearance.action(decision.action)) · \(DecisionAppearance.trigger(decision.trigger)) · \(DashboardFormat.precise.string(from: decision.at))")
                        .font(.callout).foregroundColor(.secondary)
                }
                Spacer()
                if decision.source == "manual" {
                    Text("手动测速").font(.caption.weight(.medium)).padding(.horizontal, 8).padding(.vertical, 4)
                        .background(Capsule().fill(DashboardStyle.accent.opacity(0.14)))
                }
            }

            HStack(spacing: 7) {
                Text(verbatim: decision.before?.isEmpty == false ? decision.before! : "未记录")
                Image(systemName: decision.action == "kept" ? "equal" : decision.after?.isEmpty == false ? "arrow.right" : "questionmark")
                    .foregroundColor(.secondary)
                Text(verbatim: decision.after?.isEmpty == false ? decision.after! : "结果未确认").fontWeight(.semibold)
            }.font(.system(size: 17)).textSelection(.enabled)
            if let chain = decision.chain, !chain.isEmpty {
                Text(verbatim: "验证路径  \(chain)").font(.callout).foregroundColor(.secondary).textSelection(.enabled)
            }

            HStack(spacing: 15) {
                if decision.trigger == "hardFailure" {
                    decisionMetric("恢复策略", "依次验证可用候选")
                    decisionMetric("带宽分档", "故障恢复不适用")
                } else {
                    decisionMetric(decision.action == "skipped" ? "参考容差" : "选优容差",
                                   decision.nearBestPct.map { "\(DashboardFormat.number($0))%" } ?? "未记录")
                    decisionMetric("同档门槛", finite(decision.thresholdMbps).map { "\(DashboardFormat.number($0)) Mbps" } ?? "未记录")
                }
                decisionMetric(decision.trigger == "hardFailure" ? "已测候选下限" : "绝对下限",
                               positive(decision.minMbps).map { "\(DashboardFormat.number($0)) Mbps" } ?? "未配置")
            }

            if !decision.evidence.isEmpty {
                Divider()
                Text("候选证据").font(.headline)
                if decision.evidence.contains(where: { ($0.rank ?? 0) > 0 }) {
                    Text("同档候选依次比较可靠性失败率、同测法历史 P25、当前节点保持和 HTTP 延迟；低于门槛的后备候选先比较本轮带宽。")
                        .font(.caption).foregroundColor(.secondary)
                }
                let maximum = max(decision.evidence.compactMap(\.mbps).filter { $0.isFinite && $0 > 0 }.max() ?? 0,
                                  decision.thresholdMbps ?? 0, decision.minMbps ?? 0)
                ForEach(displayOrder(decision.evidence)) { candidate in
                    CandidateEvidenceRow(candidate: candidate, maximum: maximum,
                                         threshold: decision.thresholdMbps ?? 0, minimum: decision.minMbps ?? 0)
                }
            }

            if !decision.followResults.isEmpty {
                Divider()
                Text("随动组结果").font(.headline)
                ForEach(decision.followResults) { follower in
                    HStack(alignment: .firstTextBaseline, spacing: 9) {
                        Image(systemName: DecisionAppearance.symbol(follower.action))
                            .foregroundColor(DecisionAppearance.color(follower.action)).frame(width: 16)
                        Text(verbatim: follower.group).font(.callout.weight(.medium))
                        Text(verbatim: DecisionAppearance.route(before: follower.before, after: follower.after, action: follower.action))
                            .font(.callout).foregroundColor(.secondary).lineLimit(1).truncationMode(.middle)
                        Spacer()
                        Text(verbatim: follower.reason?.isEmpty == false ? follower.reason! : "未记录额外原因")
                            .font(.caption).foregroundColor(.secondary)
                    }
                }
            }

            Divider()
            HStack(alignment: .top, spacing: 8) {
                Text("判断说明").font(.callout.weight(.medium))
                Text(verbatim: decision.reason?.isEmpty == false ? decision.reason! : "未记录额外原因")
                    .font(.callout).foregroundColor(.secondary).textSelection(.enabled)
            }
        }.dashboardCard()
    }

    private func positive(_ value: Double?) -> Double? {
        guard let value, value.isFinite, value > 0 else { return nil }
        return value
    }

    private func finite(_ value: Double?) -> Double? {
        guard let value, value.isFinite, value >= 0 else { return nil }
        return value
    }

    private func displayOrder(_ candidates: [DecisionCandidate]) -> [DecisionCandidate] {
        candidates.sorted { left, right in
            let leftRank = left.rank ?? Int.max
            let rightRank = right.rank ?? Int.max
            if leftRank != rightRank { return leftRank < rightRank }
            if left.selected != right.selected { return left.selected == true }
            return left.node.localizedStandardCompare(right.node) == .orderedAscending
        }
    }

    private func decisionMetric(_ title: String, _ value: String) -> some View {
        VStack(alignment: .leading, spacing: 3) {
            Text(title).font(.caption).foregroundColor(.secondary)
            Text(verbatim: value).font(.callout.weight(.medium)).monospacedDigit()
        }
    }
}

private struct CandidateEvidenceRow: View {
    let candidate: DecisionCandidate
    let maximum: Double
    let threshold: Double
    let minimum: Double

    var body: some View {
        let speed = candidate.mbps.flatMap { $0.isFinite && $0 > 0 ? $0 : nil }
        VStack(alignment: .leading, spacing: 5) {
            HStack(spacing: 8) {
                Image(systemName: candidate.selected == true ? "checkmark.circle.fill" : "circle")
                    .foregroundColor(candidate.selected == true ? DashboardStyle.accent : .secondary)
                if let rank = candidate.rank, rank > 0 {
                    Text(verbatim: "#\(rank)").font(.caption.weight(.semibold)).monospacedDigit()
                        .foregroundColor(candidate.selected == true ? DashboardStyle.accent : .secondary)
                }
                Text(verbatim: candidate.node).font(.callout.weight(candidate.selected == true ? .semibold : .regular))
                    .lineLimit(1).truncationMode(.middle)
                Spacer(minLength: 4)
                if let latency = candidate.latencyMS, latency > 0 {
                    Text(verbatim: "\(latency) ms").font(.caption.monospacedDigit()).foregroundColor(.secondary)
                }
                Text(verbatim: speed.map { "\(DashboardFormat.number($0)) Mbps" } ?? "—")
                    .font(.callout.monospacedDigit()).foregroundColor(speed == nil ? Color(nsColor: .systemRed) : .primary)
            }
            if maximum > 0 {
                GeometryReader { geo in
                    ZStack(alignment: .leading) {
                        Capsule().fill(Color.secondary.opacity(0.13))
                        if let speed {
                            Capsule().fill(candidate.selected == true ? DashboardStyle.accent : DashboardStyle.accent.opacity(0.55))
                                .frame(width: geo.size.width * CGFloat(min(1, speed / maximum)))
                        }
                        thresholdMarker(threshold, width: geo.size.width, maximum: maximum, color: .orange)
                        thresholdMarker(minimum, width: geo.size.width, maximum: maximum, color: Color(nsColor: .systemRed))
                    }
                }.frame(height: 6).accessibilityHidden(true)
            }
            Text(verbatim: methodLabel).font(.caption).foregroundColor(.secondary)
        }
        .padding(.vertical, 3)
        .accessibilityElement(children: .combine)
    }

    @ViewBuilder
    private func thresholdMarker(_ value: Double, width: CGFloat, maximum: Double, color: Color) -> some View {
        if value > 0, value.isFinite, maximum > 0 {
            Rectangle().fill(color).frame(width: 1.5, height: 9)
                .offset(x: width * CGFloat(min(1, value / maximum)) - 0.75)
        }
    }

    private var methodLabel: String {
        var parts: [String] = [DecisionAppearance.measurement(candidate.kind)]
        if let streams = candidate.streams, streams > 0 { parts.append("\(streams) 流") }
        if let bytes = candidate.requestedBytes, bytes > 0 { parts.append("每节点 \(DashboardFormat.bytes(bytes))") }
        if candidate.preferredBand == true { parts.append("同档候选") }
        else if candidate.preferredBand == false { parts.append("后备候选") }
        if candidate.stage == "probe" {
            parts.append("204 探测 \(candidate.status ?? "失败")")
            parts.append("未开始下载")
        } else if candidate.stage == "download" {
            parts.append(candidate.status == "interrupted" ? "下载中断" : "下载失败")
            if let bytes = candidate.bytes, bytes > 0 { parts.append("已读取 \(DashboardFormat.bytes(bytes))") }
        } else if let failures = candidate.failures, let attempts = candidate.attempts, failures > 0, attempts > 0 {
            parts.append("失败 \(failures)/\(attempts)")
        }
        if let attempts = candidate.strategyAttempts, attempts > 0 {
            parts.append("可靠性失败 \(candidate.strategyFailures ?? 0)/\(attempts)（含本轮）")
        }
        if let attempts = candidate.historyAttempts, attempts > 0 {
            parts.append("同测法历史失败 \(candidate.historyFailures ?? 0)/\(attempts)")
        }
        if let p25 = candidate.historyP25Mbps, p25.isFinite {
            parts.append("同测法历史 P25 \(DashboardFormat.number(p25)) Mbps · \(candidate.historySamples ?? 0) 样本")
        } else if let samples = candidate.historySamples, (candidate.rank ?? 0) > 0 {
            parts.append(samples == 0 ? "同测法历史带宽缺失" : "同测法历史 \(samples) 条 · P25 至少需 3 条")
        }
        if let provider = candidate.provider, !provider.isEmpty { parts.append(provider) }
        return parts.joined(separator: " · ")
    }
}

private enum DecisionAppearance {
    static func action(_ value: String) -> String {
        switch value {
        case "switched": return "已切换"
        case "kept": return "保持当前节点"
        case "skipped": return "已跳过"
        case "failed": return "判断失败"
        default: return value.isEmpty ? "结果未记录" : value
        }
    }

    static func symbol(_ action: String) -> String {
        switch action {
        case "switched": return "arrow.triangle.swap"
        case "kept": return "checkmark.circle.fill"
        case "skipped": return "forward.end.circle"
        case "failed": return "exclamationmark.triangle.fill"
        default: return "questionmark.circle"
        }
    }

    static func color(_ action: String) -> Color {
        switch action {
        case "switched": return DashboardStyle.accent
        case "kept": return Color(nsColor: .systemGreen)
        case "skipped": return Color(nsColor: .systemOrange)
        case "failed": return Color(nsColor: .systemRed)
        default: return .secondary
        }
    }

    static func trigger(_ value: String?) -> String {
        switch value {
        case "manualBench": return "手动测速"
        case "scheduledSweep": return "完整扫描"
        case "activeCheck": return "在用节点快筛"
        case "slowConfirmation": return "持续降速确认"
        case "hardFailure": return "链路故障恢复"
        case "daemon": return "守护进程判断"
        default: return value?.isEmpty == false ? value! : "触发原因未记录"
        }
    }

    static func measurement(_ value: String?) -> String {
        switch value {
        case "bench": return "手动测速"
        case "sweep": return "完整扫描"
        case "quick": return "快筛"
        case "confirm": return "复测"
        case "recovery": return "故障恢复候选"
        default: return value?.isEmpty == false ? value! : "测法未记录"
        }
    }

    static func route(_ decision: DecisionRecord) -> String {
        route(before: decision.before, after: decision.after, action: decision.action)
    }

    static func route(before: String?, after: String?, action: String) -> String {
        let old = before?.isEmpty == false ? before! : "未记录"
        guard let after, !after.isEmpty else { return "\(old) · 结果未确认" }
        let new = after
        return action == "kept" ? "保持 \(new)" : "\(old) → \(new)"
    }
}
