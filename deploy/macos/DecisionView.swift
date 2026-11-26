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
                ForEach(groups, id: \.name) { group in
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
    @State private var historyExpanded = false

    var body: some View {
        if let focus = selected {
            DecisionDetailCard(decision: focus).id(focus.id)
        }
        VStack(alignment: .leading, spacing: 10) {
            DashboardDisclosureButton(isExpanded: $historyExpanded) {
                HStack(spacing: 8) {
                    Label("近期决策", systemImage: "clock.arrow.circlepath").font(.headline)
                    Spacer()
                    Text(decisions.isEmpty ? "暂无记录" : "\(decisions.count) 条")
                        .font(.caption.monospacedDigit()).foregroundColor(.secondary)
                }
            }
            if historyExpanded {
                if decisions.isEmpty {
                    Text("完成一次测速判断后，这里会显示保持、切换、跳过和失败的证据。")
                        .font(.callout).foregroundColor(.secondary)
                } else {
                    ForEach(decisions) { decision in
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
            }
        }.dashboardCard()
    }
}

private struct DecisionDetailCard: View {
    let decision: DecisionRecord
    @State private var evidenceExpanded = false

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
                decisionMetric(decision.trigger == "hardFailure" ? "历史带宽下限" : "绝对下限",
                               positive(decision.minMbps).map { "\(DashboardFormat.number($0)) Mbps" } ?? "未配置")
            }

            if decision.trigger != "hardFailure", let factor = positive(decision.latencyFactor) {
                HStack(spacing: 15) {
                    decisionMetric("历史延迟基准", latencyBaseline)
                    decisionMetric("抖动阈值", finite(decision.latencyThresholdMS).map {
                        "\(DashboardFormat.number($0)) ms"
                    } ?? "本轮不筛选")
                    decisionMetric("阈值倍率", "\(DashboardFormat.number(factor))×")
                }
            }

            if let recoverySummary = legacyRecoverySummary {
                Label(recoverySummary, systemImage: "info.circle")
                    .font(.callout).foregroundColor(.secondary).fixedSize(horizontal: false, vertical: true)
            }

            if !decision.evidence.isEmpty {
                Divider()
                DashboardDisclosureButton(isExpanded: $evidenceExpanded) {
                    HStack(spacing: 8) {
                        Label("候选证据", systemImage: "list.number").font(.headline)
                        Spacer()
                        Text("\(decision.evidence.count) 个候选")
                            .font(.caption.monospacedDigit()).foregroundColor(.secondary)
                    }
                }
                if evidenceExpanded {
                    Text(evidenceExplanation)
                        .font(.caption).foregroundColor(.secondary).fixedSize(horizontal: false, vertical: true)
                    let maximum = max(decision.evidence.compactMap(\.mbps).filter { $0.isFinite && $0 > 0 }.max() ?? 0,
                                      decision.thresholdMbps ?? 0, decision.minMbps ?? 0)
                    let candidates = DecisionEvidencePresentation.displayOrder(decision.evidence)
                    ForEach(candidates.indices, id: \.self) { index in
                        let candidate = candidates[index]
                        CandidateEvidenceRow(candidate: candidate, maximum: maximum,
                                             threshold: decision.thresholdMbps ?? 0, minimum: decision.minMbps ?? 0,
                                             recovery: decision.trigger == "hardFailure")
                    }
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

    private var evidenceExplanation: String {
        if decision.trigger == "hardFailure" {
            return "故障恢复按长期可靠性排序执行节点与链路验证。严格地区模式要求出口匹配；宽松模式可在地区验证均未确认后采用首个链路可用候选。带宽是历史参考样本，常规带宽容差不参与故障恢复。"
        }
        if decision.latencyThresholdMS != nil {
            return "先按本组历史 HTTP 延迟基准剔除抖动或缺少有效延迟的候选，再按本轮带宽分档。同档候选依次比较可靠性失败率、同测法历史 P25、当前节点保持和 HTTP 延迟。"
        }
        if decision.latencyFactor != nil {
            if (decision.latencySamples ?? 0) > 0 {
                return "历史延迟均值为 0 ms，本轮沿用带宽分档与可靠性规则。"
            }
            return "本轮开始时尚无组延迟历史，按带宽选优。分组测速完成后会用有效的 HTTP 204 读数建立基准。"
        }
        return "同档候选依次比较可靠性失败率、同测法历史 P25、当前节点保持和 HTTP 延迟；低于门槛的后备候选先比较本轮带宽。"
    }

    private var latencyBaseline: String {
        guard let samples = decision.latencySamples, samples > 0,
              let mean = finite(decision.latencyMeanMS) else { return "冷启动 · 尚无样本" }
        return "\(DashboardFormat.number(mean)) ms · \(samples) 次"
    }

    private var legacyRecoverySummary: String? {
        DecisionEvidencePresentation.legacyRecoverySummary(decision)
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
    let recovery: Bool

    var body: some View {
        let speed = candidate.mbps.flatMap { $0.isFinite && $0 > 0 ? $0 : nil }
        let latency = candidate.latencyMS ?? 0
        VStack(alignment: .leading, spacing: 5) {
            HStack(spacing: 8) {
                Image(systemName: validationSymbol)
                    .foregroundColor(validationColor)
                if let order = candidate.validationOrder, order > 0 {
                    Text(verbatim: "验证 #\(order)").font(.caption.weight(.semibold)).monospacedDigit()
                        .foregroundColor(candidate.selected == true ? DashboardStyle.accent : .secondary)
                }
                if let rank = candidate.rank, rank > 0 {
                    Text(verbatim: recovery ? "排序 #\(rank)" : "#\(rank)").font(.caption.weight(.semibold)).monospacedDigit()
                        .foregroundColor(candidate.selected == true ? DashboardStyle.accent : .secondary)
                }
                Text(verbatim: candidate.node).font(.callout.weight(candidate.selected == true ? .semibold : .regular))
                    .lineLimit(1).truncationMode(.middle)
                Spacer(minLength: 4)
                if latency > 0 || (latency == 0 && (candidate.attempts ?? 0) >= 2) {
                    Text(verbatim: "\(latency) ms").font(.caption.monospacedDigit()).foregroundColor(.secondary)
                }
                Text(verbatim: speed.map { (recovery ? "历史 " : "") + "\(DashboardFormat.number($0)) Mbps" } ?? "—")
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
        let measurement = DecisionAppearance.measurement(candidate.kind)
        var parts: [String] = recovery
            ? ["历史带宽参考", measurement == "故障恢复候选" ? "来源未记录" : "来源 \(measurement)"]
            : [measurement]
        if let observedAt = candidate.observedAt {
            parts.append("样本 \(DashboardFormat.precise.string(from: observedAt))")
        } else if recovery {
            parts.append("样本时间未记录")
        }
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
            parts.append(DecisionEvidencePresentation.strategyReliability(
                failures: candidate.strategyFailures ?? 0, attempts: attempts, recovery: recovery
            ))
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
        if let validation = DecisionEvidencePresentation.validationLabel(candidate, recovery: recovery) {
            parts.append(validation)
        }
        return parts.joined(separator: " · ")
    }

    private var validationSymbol: String {
        if candidate.selected == true || DecisionAppearance.validationSucceeded(candidate.validation) {
            return "checkmark.circle.fill"
        }
        if let validation = candidate.validation, validation != "notReached" {
            return "xmark.circle.fill"
        }
        return "circle"
    }

    private var validationColor: Color {
        if candidate.selected == true || DecisionAppearance.validationSucceeded(candidate.validation) {
            return DashboardStyle.accent
        }
        return candidate.validation == nil || candidate.validation == "notReached"
            ? .secondary : Color(nsColor: .systemOrange)
    }
}

enum DecisionEvidencePresentation {
    static func displayOrder(_ candidates: [DecisionCandidate]) -> [DecisionCandidate] {
        candidates.sorted { left, right in
            let leftValidation = positive(left.validationOrder) ?? Int.max
            let rightValidation = positive(right.validationOrder) ?? Int.max
            if leftValidation != rightValidation { return leftValidation < rightValidation }
            let leftRank = positive(left.rank) ?? Int.max
            let rightRank = positive(right.rank) ?? Int.max
            if leftRank != rightRank { return leftRank < rightRank }
            if left.selected != right.selected { return left.selected == true }
            return left.node.localizedStandardCompare(right.node) == .orderedAscending
        }
    }

    static func legacyRecoverySummary(_ decision: DecisionRecord) -> String? {
        guard decision.trigger == "hardFailure",
              !decision.evidence.contains(where: { positive($0.validationOrder) != nil }),
              let selectedRank = decision.evidence.first(where: { $0.selected == true })?.rank,
              selectedRank > 0 else { return nil }
        if selectedRank == 1 {
            return "采用排序 #1。此旧记录未保存逐候选在线验证阶段。"
        }
        return "采用排序 #\(selectedRank)。此旧记录未保存逐候选在线验证结果，无法还原排序 #1–#\(selectedRank - 1) 的具体失败阶段。"
    }

    static func strategyReliability(failures: Int, attempts: Int, recovery: Bool) -> String {
        if recovery {
            return "排序可靠性失败 \(failures)/\(attempts)（截至参考样本）"
        }
        return "可靠性失败 \(failures)/\(attempts)（含本轮）"
    }

    static func validationLabel(_ candidate: DecisionCandidate, recovery: Bool) -> String? {
        if let validation = candidate.validation {
            return DecisionAppearance.validation(validation)
        }
        return recovery ? "在线验证结果未记录（旧记录）" : nil
    }

    private static func positive(_ value: Int?) -> Int? {
        guard let value, value > 0 else { return nil }
        return value
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

    static func validation(_ value: String) -> String {
        switch value {
        case "passed": return "在线验证通过"
        case "fallbackRegionMismatch": return "链路通过；出口地区不符，按宽松策略采用"
        case "fallbackRegionProbeFailed": return "链路通过；地区探测失败，按宽松策略采用"
        case "fallbackRegionUnknown": return "链路通过；地区结果未确认，按宽松策略采用"
        case "measurementFailed": return "测速记录不可用"
        case "latencyAboveLimit": return "延迟高于本组抖动阈值，未参加带宽选优"
        case "latencyUnknown": return "缺少有效 HTTP 延迟，未参加带宽选优"
        case "ineligible": return "不在可切换成员中"
        case "belowMinimum": return "低于绝对带宽下限"
        case "proxyReadFailed": return "无法读取节点状态"
        case "notPhysical": return "不是可直接选择的物理节点"
        case "nodeUnavailable": return "节点存活验证失败"
        case "selectionRejected": return "控制器未接受切换"
        case "selectionFailed": return "切换未确认"
        case "routeUnavailable": return "链路验证失败"
        case "regionProbeFailed": return "地区探测请求失败"
        case "regionMismatch": return "出口地区验证不符"
        case "routeProofFailed": return "无法证明验证流量经过目标链路"
        case "readbackFailed": return "切换回读验证失败"
        case "notReached": return "此前已有候选结束本次选择，未执行在线验证"
        default: return "在线验证结果：\(value)"
        }
    }

    static func validationSucceeded(_ value: String?) -> Bool {
        switch value {
        case "passed", "fallbackRegionMismatch", "fallbackRegionProbeFailed", "fallbackRegionUnknown":
            return true
        default:
            return false
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
