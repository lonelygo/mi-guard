import SwiftUI

// Select the Monterey property wrapper explicitly on SDKs that also export a State macro.
private typealias ViewState<Value> = SwiftUI.State<Value>

enum DashboardStyle {
    static let card = Color(nsColor: NSColor(name: nil) { appearance in
        appearance.bestMatch(from: [.darkAqua, .aqua]) == .darkAqua
            ? NSColor(srgbRed: 0.145, green: 0.153, blue: 0.158, alpha: 1) : .white
    })
    static let accent = Color(nsColor: NSColor(name: nil) { appearance in
        if appearance.bestMatch(from: [.darkAqua, .aqua]) == .darkAqua {
            return NSColor(srgbRed: 0.32, green: 0.80, blue: 0.79, alpha: 1)
        }
        return NSColor(srgbRed: 0.02, green: 0.43, blue: 0.45, alpha: 1)
    })
}

private extension DashboardSection {
    var dashboardTitle: String {
        switch self {
        case .overview: return "概览"
        case .nodes: return "节点分析"
        case .selections: return "线路选用"
        case .flights: return "飞行地图"
        case .notifications: return "通知记录"
        }
    }

    var dashboardSymbol: String {
        switch self {
        case .overview: return "square.grid.2x2"
        case .nodes: return "waveform.path.ecg"
        case .selections: return "arrow.triangle.branch"
        case .flights: return "map"
        case .notifications: return "bell"
        }
    }
}

extension View {
    func dashboardCard(_ padding: CGFloat = 16) -> some View {
        self.padding(padding)
            .frame(maxWidth: .infinity, alignment: .leading)
            .background(RoundedRectangle(cornerRadius: 11).fill(DashboardStyle.card))
            .overlay(RoundedRectangle(cornerRadius: 11).strokeBorder(Color(nsColor: .separatorColor).opacity(0.6), lineWidth: 0.7))
    }
}

struct DashboardDisclosureButton<Label: View>: View {
    @Binding var isExpanded: Bool
    @ViewBuilder let label: () -> Label

    var body: some View {
        Button {
            isExpanded.toggle()
        } label: {
            HStack(spacing: 9) {
                HStack(spacing: 9) { label() }
                    .frame(maxWidth: .infinity, alignment: .leading)
                Image(systemName: isExpanded ? "chevron.down" : "chevron.right")
                    .font(.caption.weight(.semibold)).foregroundColor(.secondary).accessibilityHidden(true)
            }
            .frame(maxWidth: .infinity, minHeight: 28, alignment: .leading)
            .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
        .accessibilityValue(isExpanded ? "已展开" : "已折叠")
    }
}

struct DashboardView: View {
    @ObservedObject var controller: DashboardController
    var showSettings: () -> Void

    var body: some View {
        HStack(spacing: 0) {
            DashboardSidebar(controller: controller, showSettings: showSettings)
            Divider()
            if controller.section == .flights {
                FlightPanel(reader: controller.readerURL, configURL: controller.configURL)
                    .id(controller.configURL)
            } else if controller.section == .notifications {
                NotificationInboxView(controller: controller)
            } else {
                DashboardPage(section: controller.section, snapshot: controller.snapshot,
                              history: DashboardHistory(controller.snapshot?.samples ?? []),
                              problem: controller.problem, loading: controller.loading, refresh: controller.refresh,
                              requestedDecisionID: controller.requestedDecisionID,
                              navigate: { controller.clearDecisionFocus(); controller.section = $0 },
                              focusDecision: controller.focusDecision)
                    .id(controller.configURL)
            }
        }
        .frame(minWidth: 835, idealWidth: 1080, maxWidth: .infinity, minHeight: 565, idealHeight: 700, maxHeight: .infinity)
        .background(Color(nsColor: .windowBackgroundColor))
        .accentColor(DashboardStyle.accent)
    }
}

private struct DashboardSidebar: View {
    @ObservedObject var controller: DashboardController
    let showSettings: () -> Void
    private static let icon = Bundle.main.url(forResource: "AppIcon", withExtension: "icns").flatMap { NSImage(contentsOf: $0) }

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            HStack(spacing: 9) {
                if let icon = Self.icon {
                    Image(nsImage: icon).resizable().frame(width: 32, height: 32).accessibilityHidden(true)
                } else {
                    Image(systemName: "shield.lefthalf.fill").font(.system(size: 22, weight: .medium))
                        .foregroundColor(DashboardStyle.accent).frame(width: 32, height: 32).accessibilityHidden(true)
                }
                Text("mi-guard").font(.system(size: 17, weight: .semibold))
            }
            .padding(.horizontal, 16).padding(.top, 19).padding(.bottom, 16)

            List {
                ForEach(DashboardSection.allCases, id: \.self) { section in
                    let selected = section == controller.section
                    Button { select(section) } label: {
                        HStack(spacing: 9) {
                            Image(systemName: section.dashboardSymbol)
                                .frame(width: 19).foregroundColor(selected ? DashboardStyle.accent : .secondary)
                            Text(section.dashboardTitle).fontWeight(selected ? .semibold : .regular).foregroundColor(.primary)
                            Spacer(minLength: 0)
                        }
                        .padding(.horizontal, 9).frame(height: 32)
                        .background(RoundedRectangle(cornerRadius: 7).fill(selected ? DashboardStyle.accent.opacity(0.16) : .clear))
                        .contentShape(Rectangle())
                    }
                    .buttonStyle(.plain)
                    .accessibilityAddTraits(selected ? [.isSelected] : [])
                    .listRowInsets(EdgeInsets(top: 1, leading: 0, bottom: 1, trailing: 0))
                }
            }
            .listStyle(.sidebar)
            .onMoveCommand { direction in
                let destinations = Array(DashboardSection.allCases)
                guard let i = destinations.firstIndex(of: controller.section) else { return }
                if direction == .up && i > 0 { select(destinations[i - 1]) }
                if direction == .down && i + 1 < destinations.count { select(destinations[i + 1]) }
            }

            Divider()
            Button(action: showSettings) {
                Label("设置…", systemImage: "gearshape")
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .padding(.horizontal, 17).padding(.vertical, 11).contentShape(Rectangle())
            }
            .buttonStyle(.plain).help("打开 mi-guard 设置 (⌘,)")
        }
        .frame(width: 176).background(.regularMaterial)
    }

    private func select(_ section: DashboardSection) {
        if section == .selections { controller.clearDecisionFocus() }
        controller.section = section
    }
}

private struct DashboardPage: View {
    let section: DashboardSection
    let snapshot: DashboardSnapshot?
    let history: DashboardHistory
    let problem: String?
    let loading: Bool
    let refresh: () -> Void
    let requestedDecisionID: String?
    let navigate: (DashboardSection) -> Void
    let focusDecision: (String) -> Void
    @ViewState private var requested: DashboardSeries?
    @ViewState private var period = DashboardPeriod.all
    @ViewState private var search = ""

    var body: some View {
        let query = section == .nodes ? search.trimmingCharacters(in: .whitespacesAndNewlines) : ""
        let selection = history.selection(requested, matching: query)
        VStack(alignment: .leading, spacing: 0) {
            HStack(spacing: 12) {
                VStack(alignment: .leading, spacing: 4) {
                    Text(section.dashboardTitle).font(.system(size: 26, weight: .semibold))
                        .accessibilityAddTraits(.isHeader)
                    if let snapshot { Text(subtitle(snapshot)).font(.callout).foregroundColor(.secondary) }
                }
                Spacer()
                if loading { ProgressView().controlSize(.small).accessibilityLabel("正在读取记录") }
                Button(action: refresh) { Label("刷新", systemImage: "arrow.clockwise") }
                    .disabled(loading).help("重新读取本地记录")
            }
            .padding(.horizontal, 22).padding(.top, 18).padding(.bottom, 15)

            ScrollView {
                VStack(alignment: .leading, spacing: 14) {
                    if let problem {
                        VStack(alignment: .leading, spacing: 6) {
                            Label(snapshot == nil ? "无法读取本地记录" : "读取失败，正在显示上一次的记录", systemImage: "exclamationmark.triangle")
                                .font(.headline)
                            Text(verbatim: problem).font(.callout).textSelection(.enabled)
                        }.dashboardCard()
                    }
                    if let snapshot {
                        switch section {
                        case .overview, .nodes:
                            if section == .overview {
                                ManagedRoutesCard(groups: snapshot.groups, decisions: snapshot.decisionHistory,
                                                  onSelections: { navigate(.selections) }, onDecision: focusDecision)
                                BudgetSummary(snapshot: snapshot, history: history)
                            } else if !history.routes.isEmpty {
                                RouteBrowser(history: history, routes: history.routes.filter { $0.matches(query) },
                                            selected: selection?.route, search: $search, choose: selectRoute)
                            }
                            if let selection {
                                TrendCard(history: history, series: selection, period: $period, inNodes: section == .nodes,
                                          selectRoute: selectRoute, selectSeries: { requested = $0 })
                                if section == .overview {
                                    DashboardOverview(history: history, focus: selection, period: period) { series in
                                        search = ""
                                        requested = series
                                        navigate(.nodes)
                                    }
                                }
                            } else if history.routes.isEmpty {
                                DashboardEmpty(symbol: "waveform.path.ecg", title: "暂无测速记录",
                                               detail: "手动测速或守护进程的测速记录会显示在这里。").dashboardCard()
                            }
                        case .selections:
                            if let warning = snapshot.decisionWarning, !warning.isEmpty {
                                Label(warning, systemImage: "exclamationmark.triangle")
                                    .font(.callout).foregroundColor(.secondary).dashboardCard(12)
                            }
                            if requestedDecisionID != nil { decisionPanel(snapshot) }
                            if snapshot.groups.isEmpty {
                                DashboardEmpty(symbol: "arrow.triangle.branch", title: "暂无线路选用记录",
                                               detail: "首次观测到策略组的选用节点后，这里会显示节点和观测时段。").dashboardCard()
                            } else {
                                if snapshot.groups.allSatisfy({ $0.lastSeen == nil }) {
                                    Text("运行 mihomo-guard check 或启动 daemon 后，这里会记录各组实际选用的节点。")
                                        .font(.callout).foregroundColor(.secondary)
                                }
                                SelectionGroupsCard(groups: snapshot.groups, decisions: snapshot.decisionHistory, onDecision: focusDecision)
                            }
                            if requestedDecisionID == nil { decisionPanel(snapshot) }
                        case .flights, .notifications:
                            EmptyView()
                        }
                    } else if problem == nil {
                        DashboardEmpty(symbol: loading ? "clock" : "tray", title: loading ? "正在读取本地记录…" : "尚未载入记录",
                                       detail: loading ? "" : "选择配置文件或刷新以读取测速历史。").dashboardCard()
                    }
                }
                .padding(.horizontal, 22).padding(.bottom, 20).frame(maxWidth: .infinity, alignment: .leading)
            }
            .id(section == .selections ? "selections/" + (requestedDecisionID ?? "") : section.rawValue)
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .topLeading)
        .onAppear { if let selection { requested = selection } }
        .onChange(of: selection) { if let choice = $0 { requested = choice } }
    }

    private func subtitle(_ snapshot: DashboardSnapshot) -> String {
        switch section {
        case .overview:
            return "本期 \(DashboardFormat.date.string(from: snapshot.cycleStart)) 起 · \(DashboardFormat.date.string(from: snapshot.cycleEnd)) 结转"
        case .nodes: return "\(history.routes.count) 条测速路径 · \(history.sampleCount) 条历史记录"
        case .selections: return "选用时长截至各策略组的末次观测"
        case .flights: return "Mihomo 内核路由的实时与历史流向"
        case .notifications: return "通知记录"
        }
    }

    private func decisionPanel(_ snapshot: DashboardSnapshot) -> some View {
        DecisionHistoryPanel(decisions: snapshot.decisionHistory,
                             selected: snapshot.selectedDecision.flatMap { $0.id == requestedDecisionID ? $0 : nil },
                             requestedID: requestedDecisionID, onDecision: focusDecision)
    }

    private func selectRoute(_ route: DashboardRoute) { requested = history.profiles[route]?.first }
}

struct DashboardEmpty: View {
    let symbol: String
    let title: String
    let detail: String

    var body: some View {
        VStack(spacing: 7) {
            Image(systemName: symbol).font(.title2).foregroundColor(.secondary).accessibilityHidden(true)
            Text(title).font(.headline)
            if !detail.isEmpty { Text(detail).font(.callout).foregroundColor(.secondary) }
        }
        .multilineTextAlignment(.center).frame(maxWidth: .infinity).padding(.vertical, 16)
    }
}

private struct BudgetSummary: View {
    let snapshot: DashboardSnapshot
    let history: DashboardHistory

    var body: some View {
        let settled = DashboardFormat.settled(used: snapshot.usedBytes, pending: snapshot.pendingBytes)
        HStack(alignment: .top, spacing: 15) {
            VStack(alignment: .leading, spacing: 6) {
                metricLabel("账期额度占用", "gauge")
                HStack(alignment: .firstTextBaseline, spacing: 5) {
                    Text(verbatim: DashboardFormat.bytes(snapshot.usedBytes)).font(.system(size: 21, weight: .semibold))
                    Text(verbatim: "/ \(DashboardFormat.bytes(snapshot.budgetBytes))").font(.callout).foregroundColor(.secondary)
                }.lineLimit(1).minimumScaleFactor(0.8)
                GeometryReader { geometry in
                    let cap = Double(max(1, snapshot.budgetBytes))
                    let used = min(1, max(0, Double(snapshot.usedBytes) / cap))
                    let paid = min(used, Double(settled) / cap)
                    ZStack(alignment: .leading) {
                        Capsule().fill(Color.secondary.opacity(0.14))
                        Rectangle().fill(DashboardStyle.accent.opacity(0.43)).frame(width: geometry.size.width * used)
                        Rectangle().fill(DashboardStyle.accent).frame(width: geometry.size.width * paid)
                    }.clipShape(Capsule())
                }.frame(height: 4).accessibilityHidden(true)
                Text(verbatim: "已结算 \(DashboardFormat.bytes(settled)) · 待结算 \(DashboardFormat.bytes(snapshot.pendingBytes))")
                    .font(.caption).foregroundColor(.secondary)
            }
            .frame(maxWidth: .infinity, alignment: .leading)
            .help("额度 \(snapshot.budgetBytes) B，占用 \(snapshot.usedBytes) B，其中已结算 \(settled) B，待结算 \(snapshot.pendingBytes) B。")
            .accessibilityElement(children: .ignore).accessibilityLabel("本账期测速预算")
            .accessibilityValue("额度 \(snapshot.budgetBytes) 字节，占用 \(snapshot.usedBytes) 字节，其中已结算 \(settled) 字节，待结算 \(snapshot.pendingBytes) 字节")
            Divider()
            HStack(alignment: .top, spacing: 14) {
                VStack(alignment: .leading, spacing: 6) {
                    metricLabel("已观测节点", "server.rack")
                    Text(verbatim: "\(history.nodeCount)").font(.system(size: 21, weight: .semibold)).monospacedDigit()
                    Text("共 \(history.sampleCount) 条测速记录").font(.caption).foregroundColor(.secondary)
                }
                .frame(maxWidth: .infinity, alignment: .leading).help("同一提供方的同名节点只计一次")
                Divider()
                VStack(alignment: .leading, spacing: 6) {
                    metricLabel("上次完整扫描", "clock.arrow.circlepath")
                    if let date = snapshot.lastSweep {
                        Text(verbatim: DashboardFormat.clock.string(from: date)).font(.system(size: 21, weight: .semibold)).monospacedDigit()
                        Text(verbatim: "\(DashboardFormat.date.string(from: date)) 开始").font(.caption).foregroundColor(.secondary)
                    } else {
                        Text("未记录").font(.headline).foregroundColor(.secondary)
                    }
                }
                .frame(maxWidth: .infinity, alignment: .leading)
            }
            .frame(maxWidth: .infinity, alignment: .leading)
        }
        .fixedSize(horizontal: false, vertical: true).dashboardCard()
    }

    private func metricLabel(_ title: String, _ symbol: String) -> some View {
        HStack(spacing: 5) {
            Image(systemName: symbol).foregroundColor(DashboardStyle.accent).accessibilityHidden(true)
            Text(title).foregroundColor(.secondary)
        }.font(.callout)
    }
}

private struct RouteBrowser: View {
    let history: DashboardHistory
    let routes: [DashboardRoute]
    let selected: DashboardRoute?
    @Binding var search: String
    let choose: (DashboardRoute) -> Void

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack {
                Text("测速路径").font(.headline)
                Spacer()
                Image(systemName: "magnifyingglass").foregroundColor(.secondary).accessibilityHidden(true)
                TextField("搜索节点、提供方或路径", text: $search).textFieldStyle(.roundedBorder).frame(maxWidth: 225)
            }
            if routes.isEmpty {
                HStack {
                    Text("没有匹配的路径").foregroundColor(.secondary)
                    Button("清除搜索") { search = "" }
                }.frame(maxWidth: .infinity).padding(.vertical, 8)
            } else {
                List(selection: Binding<DashboardRoute?>(get: { selected }, set: { if let route = $0 { choose(route) } })) {
                    ForEach(routes, id: \.self) { route in
                        HStack(spacing: 9) {
                            Text(verbatim: route.name).fontWeight(.medium).frame(maxWidth: .infinity, alignment: .leading)
                            Text(verbatim: route.context).foregroundColor(.secondary).frame(maxWidth: .infinity, alignment: .leading)
                            let count = (history.profiles[route] ?? []).reduce(0) { $0 + (history.records[$1]?.count ?? 0) }
                            Text("\(count) 条").font(.caption.monospacedDigit()).foregroundColor(.secondary)
                        }
                        .lineLimit(1).truncationMode(.middle).padding(.vertical, 2).help(route.title).tag(route)
                    }
                }
                .listStyle(.plain).frame(height: CGFloat(min(4, routes.count)) * 27 + 5)
            }
        }.dashboardCard(13)
    }
}

private struct TrendCard: View {
    let history: DashboardHistory
    let series: DashboardSeries
    @Binding var period: DashboardPeriod
    let inNodes: Bool
    let selectRoute: (DashboardRoute) -> Void
    let selectSeries: (DashboardSeries) -> Void
    @ViewState private var expanded = true
    @ViewState private var showRows = false

    var body: some View {
        let all = history.records[series] ?? []
        let domain = period.domain(first: all.first?.time, last: all.last?.time, now: Date())
        let rows = all.filter { domain.contains($0.time) }
        let plot = DashboardPlot(series: series, domain: domain, readings: rows.map { ($0.time, $0.successful ? $0.mbps : nil) })
        VStack(alignment: .leading, spacing: 12) {
            HStack(spacing: 16) {
                DashboardDisclosureButton(isExpanded: $expanded) {
                    Label("历史吞吐量", systemImage: "chart.xyaxis.line").font(.headline)
                }
                if expanded {
                    Picker("历史时段", selection: $period) {
                        ForEach(DashboardPeriod.allCases, id: \.self) { Text($0.title).tag($0) }
                    }.labelsHidden().pickerStyle(.segmented).frame(width: 208)
                }
            }
            if expanded {
                selectors
                measurements(rows: rows, plot: plot)
            }
        }.dashboardCard()
    }

    private var selectors: some View {
        HStack(spacing: 0) {
            if inNodes {
                SeriesLabel(current: series.route.name, detail: series.route.context, selectable: false)
                    .help(series.route.title)
            } else {
                SeriesMenu(name: "节点与路径", current: series.route.name, detail: series.route.context,
                           values: history.routes, selected: Binding(get: { series.route }, set: selectRoute), title: { $0.title })
            }
            Divider().padding(.vertical, 6)
            SeriesMenu(name: "测量方式", current: series.profile.source, detail: series.profile.parameters,
                       values: history.profiles[series.route] ?? [], selected: Binding(get: { series }, set: selectSeries),
                       title: history.profileLabel)
        }
        .fixedSize(horizontal: false, vertical: true)
        .background(RoundedRectangle(cornerRadius: 7).fill(Color.secondary.opacity(0.065)))
    }

    @ViewBuilder private func measurements(rows: [LedgerSample], plot: DashboardPlot) -> some View {
        if let latest = rows.last {
            HStack(alignment: .lastTextBaseline, spacing: 7) {
                if latest.successful {
                    Text(verbatim: DashboardFormat.number(latest.mbps)).font(.system(size: 25, weight: .semibold)).monospacedDigit()
                    Text("Mbps").font(.callout).foregroundColor(.secondary)
                } else {
                    Label("测速失败", systemImage: "xmark.circle").font(.headline).foregroundColor(Color(nsColor: .systemRed))
                }
                Text(verbatim: "最近一次 · \(DashboardFormat.shortStamp.string(from: latest.time))")
                    .font(.caption).foregroundColor(.secondary).help(DashboardFormat.precise.string(from: latest.time))
                Spacer(minLength: 4)
                Text(series.canConnect ? "成功 \(rows.count - plot.failures.count) 次 · 失败 \(plot.failures.count) 次" : "参数或路径不全 · 仅显示独立点")
                    .font(.caption).foregroundColor(.secondary)
            }
            if !latest.successful && !latest.error.isEmpty {
                Text(verbatim: latest.error == "failed" ? "未取得有效的下载结果" : latest.error)
                    .font(.callout).textSelection(.enabled)
            }
            DashboardChart(plot: plot)
            Divider()
            DashboardDisclosureButton(isExpanded: $showRows) {
                Text("测速明细（\(rows.count)）").font(.callout.weight(.medium))
            }
            if showRows { SampleRows(rows: rows) }
        } else {
            VStack(spacing: 0) {
                DashboardEmpty(symbol: "calendar", title: "此时段没有测速记录", detail: "")
                if period != .all { Button("查看全部时段") { period = .all } }
            }.padding(.bottom, 8)
        }
    }
}

private struct SeriesLabel: View {
    let current: String
    let detail: String
    let selectable: Bool

    var body: some View {
        HStack(spacing: 6) {
            VStack(alignment: .leading, spacing: 3) {
                Text(verbatim: current).font(.headline)
                Text(verbatim: detail).font(.caption).foregroundColor(.secondary)
            }.lineLimit(1).truncationMode(.middle)
            Spacer(minLength: 0)
            if selectable { Image(systemName: "chevron.down").font(.caption.weight(.semibold)).foregroundColor(.secondary) }
        }
        .padding(.horizontal, 10).padding(.vertical, 8)
        .frame(maxWidth: .infinity, alignment: .leading)
    }
}

private struct SeriesMenu<Value: Hashable>: View {
    let name: String
    let current: String
    let detail: String
    let values: [Value]
    let selected: Binding<Value>
    let title: (Value) -> String

    var body: some View {
        SeriesLabel(current: current, detail: detail, selectable: true)
            .accessibilityHidden(true)
            .overlay(SeriesPopUp(name: name, titles: values.map(title),
                                 selection: values.firstIndex(of: selected.wrappedValue)) { index in
                if values.indices.contains(index) { selected.wrappedValue = values[index] }
            }.frame(maxWidth: .infinity, maxHeight: .infinity))
            .help(title(selected.wrappedValue))
    }
}

// Keep the native menu, keyboard interaction and VoiceOver control over the full two-line label.
struct SeriesPopUp: NSViewRepresentable {
    let name: String
    let titles: [String]
    let selection: Int?
    let choose: (Int) -> Void

    final class Coordinator: NSObject {
        var choose: (Int) -> Void
        init(choose: @escaping (Int) -> Void) { self.choose = choose }
        @objc func picked(_ sender: NSPopUpButton) { choose(sender.indexOfSelectedItem) }
    }

    final class FullRowPopUpButton: NSPopUpButton {
        override var intrinsicContentSize: NSSize {
            NSSize(width: NSView.noIntrinsicMetric, height: NSView.noIntrinsicMetric)
        }
    }

    func makeCoordinator() -> Coordinator { Coordinator(choose: choose) }

    func makeNSView(context: Context) -> NSPopUpButton {
        let control = FullRowPopUpButton(frame: .zero, pullsDown: false)
        control.isTransparent = true
        control.target = context.coordinator
        control.action = #selector(Coordinator.picked(_:))
        control.setContentHuggingPriority(.defaultLow, for: .horizontal)
        control.setContentCompressionResistancePriority(.defaultLow, for: .horizontal)
        return control
    }

    func updateNSView(_ control: NSPopUpButton, context: Context) {
        context.coordinator.choose = choose
        if control.itemTitles != titles {
            control.removeAllItems()
            for (index, title) in titles.enumerated() {
                control.menu?.addItem(NSMenuItem(title: title, action: nil, keyEquivalent: ""))
                control.item(at: index)?.tag = index
            }
        }
        control.selectItem(at: selection ?? -1)
        control.setAccessibilityLabel(name)
    }
}

private struct SampleRows: View {
    let rows: [LedgerSample]

    var body: some View {
        VStack(alignment: .leading, spacing: 7) {
            HStack(spacing: 10) {
                Text("本地时间").frame(width: 150, alignment: .leading)
                Text("吞吐量（Mbps）").frame(maxWidth: .infinity, alignment: .leading)
                Text("探测延迟").frame(width: 64, alignment: .trailing)
                Text("实际下载量").frame(width: 96, alignment: .trailing)
                Text("失败 / 尝试").frame(width: 66, alignment: .trailing)
            }
            .font(.caption.weight(.medium)).foregroundColor(.secondary).padding(.top, 5)
            ForEach(rows.indices.reversed(), id: \.self) { index in
                let row = rows[index]
                let speed = row.successful ? DashboardFormat.number(row.mbps) : "失败"
                let latency = row.latencyMS > 0 ? "\(row.latencyMS) ms" : "未记录"
                let reason = row.error.isEmpty || row.error == "failed" ? "未取得有效的下载结果" : row.error
                Divider()
                VStack(alignment: .leading, spacing: 4) {
                    HStack(alignment: .firstTextBaseline, spacing: 10) {
                        Text(verbatim: DashboardFormat.stamp.string(from: row.time)).frame(width: 150, alignment: .leading)
                        Text(verbatim: speed).foregroundColor(row.successful ? .primary : Color(nsColor: .systemRed))
                            .frame(maxWidth: .infinity, alignment: .leading)
                        Text(verbatim: latency).frame(width: 64, alignment: .trailing)
                        Text(verbatim: DashboardFormat.bytes(row.bytes)).frame(width: 96, alignment: .trailing)
                        Text(verbatim: "\(row.failures) / \(row.attempts)").frame(width: 66, alignment: .trailing)
                    }.font(.callout.monospacedDigit())
                    if !row.successful {
                        Text(verbatim: reason).font(.callout).foregroundColor(.secondary).fixedSize(horizontal: false, vertical: true)
                    }
                }
                .textSelection(.enabled).help("\(DashboardFormat.precise.string(from: row.time)) · \(row.bytes) B")
                .accessibilityElement(children: .ignore)
                .accessibilityLabel("\(DashboardFormat.precise.string(from: row.time))，\(row.successful ? speed + " Mbps" : "失败，" + reason)，探测延迟 \(latency)，实际 \(row.bytes) 字节，\(row.attempts) 次尝试，\(row.failures) 次失败")
            }
        }
    }
}

private struct SelectionGroupsCard: View {
    let groups: [LedgerGroup]
    let decisions: [DecisionRecord]
    let onDecision: (String) -> Void

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            Label("策略组状态", systemImage: "arrow.triangle.branch").font(.headline)
            ForEach(groups, id: \.name) { group in
                Divider()
                SelectionGroupRow(group: group, latest: decisions.first { $0.group == group.name }, onDecision: onDecision)
            }
        }.dashboardCard()
    }
}

private struct SelectionGroupRow: View {
    let group: LedgerGroup
    let latest: DecisionRecord?
    let onDecision: (String) -> Void
    @ViewState private var expanded = false

    private var span: String {
        guard let seconds = DashboardFormat.span(since: group.since, lastSeen: group.lastSeen) else { return "尚无观测" }
        if seconds == 0 && !group.guardSelected { return "首次观测" }
        return "已观测 " + DashboardFormat.duration(seconds)
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            DashboardDisclosureButton(isExpanded: $expanded) {
                VStack(alignment: .leading, spacing: 3) {
                    Text(verbatim: group.name).font(.callout.weight(.semibold))
                    Text(verbatim: group.chain.isEmpty ? (group.configured ? "手动策略组" : "已观测策略组") : "前置组 · " + group.chain)
                        .font(.caption).foregroundColor(.secondary)
                }
                Spacer(minLength: 8)
                VStack(alignment: .trailing, spacing: 3) {
                    Text(verbatim: group.node.isEmpty ? "未记录节点" : group.node).font(.callout.weight(.medium))
                    Text(verbatim: span).font(.caption.monospacedDigit()).foregroundColor(.secondary)
                }
            }
            .lineLimit(1).truncationMode(.middle).padding(.vertical, 2)
            if expanded {
                VStack(alignment: .leading, spacing: 5) {
                    if let start = group.since, let end = group.lastSeen {
                        Text(verbatim: "观测时段  " + DashboardFormat.stamp.string(from: start) + " – " + DashboardFormat.stamp.string(from: end))
                        if group.guardSelected { Text("起于守护进程确认切换") }
                    } else { Text("尚未记录选用时段") }
                    if !group.longestNode.isEmpty {
                        Text(verbatim: "最长观测  " + group.longestNode + " · " + DashboardFormat.duration(group.longestSeconds))
                    }
                    if let latest {
                        Button("查看最近一次决策") { onDecision(latest.id) }.buttonStyle(.link)
                    }
                }
                .font(.callout).foregroundColor(.secondary).textSelection(.enabled).padding(.bottom, 5)
            }
        }
    }
}
