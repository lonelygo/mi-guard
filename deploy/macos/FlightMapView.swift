import SwiftUI

private enum FlightMapStyle {
    static let cyan = Color(red: 0.18, green: 0.77, blue: 0.94)
    static let teal = Color(red: 0.20, green: 0.84, blue: 0.69)
    static let violet = Color(red: 0.70, green: 0.43, blue: 0.98)
    static let coral = Color(red: 1.00, green: 0.42, blue: 0.43)
}

struct FlightMapView: View {
    let data: FlightMapData
    let onSelectRoute: ((String) -> Void)?

    @SwiftUI.State private var horizon: FlightHorizon
    @SwiftUI.State private var showTargets = true
    @SwiftUI.State private var selectedRouteID: String?
    @SwiftUI.State private var choosingOrigin = false
    @AppStorage("flightMap.originID") private var originID = ""

    init(data: FlightMapData, initialHorizon: FlightHorizon = .live,
         onSelectRoute: ((String) -> Void)? = nil) {
        self.data = data
        self.onSelectRoute = onSelectRoute
        _horizon = SwiftUI.State(initialValue: initialHorizon)
    }

    var body: some View {
        let routes = data.routes(for: horizon, now: data.generatedAt ?? Date())
        let origin = data.origin(selectedID: originID.isEmpty ? nil : originID)
        VStack(alignment: .leading, spacing: 14) {
            controls(origin: origin)
            GeometryReader { geometry in
                if geometry.size.width >= 760 {
                    HStack(alignment: .top, spacing: 14) {
                        map(routes: routes, origin: origin).frame(maxWidth: .infinity, maxHeight: .infinity)
                        routeList(routes: routes).frame(minWidth: 260, idealWidth: 260, maxWidth: 260,
                                                       maxHeight: .infinity, alignment: .top)
                    }
                } else {
                    VStack(alignment: .leading, spacing: 12) {
                        map(routes: routes, origin: origin).frame(maxWidth: .infinity, minHeight: 300)
                        routeList(routes: routes).frame(maxWidth: .infinity)
                    }
                }
            }
        }
        .frame(minHeight: 430)
        .onAppear {
            preserveOrSelectFirst(in: routes)
        }
        .onChange(of: horizon) { _ in preserveOrSelectFirst(in: data.routes(for: horizon, now: data.generatedAt ?? Date())) }
        .onChange(of: data) { _ in preserveOrSelectFirst(in: data.routes(for: horizon, now: data.generatedAt ?? Date())) }
    }

    private func controls(origin: FlightPlace?) -> some View {
        HStack(spacing: 12) {
            Picker("时间范围", selection: $horizon) {
                ForEach(FlightHorizon.allCases) { value in Text(value.title).tag(value) }
            }
            .pickerStyle(.segmented).frame(maxWidth: 330)

            Spacer(minLength: 8)
            Toggle("显示目标", isOn: $showTargets).toggleStyle(.switch).controlSize(.small)
                .help("补充代理出口到目标地区的线路")
            Button { choosingOrigin.toggle() } label: {
                Label(origin?.displayName ?? "选择起点", systemImage: "location")
            }
            .disabled(data.originChoices.allSatisfy { $0.coordinate == nil })
            .help("仅保存地区级示意起点，不保存 IP 或城市")
            .popover(isPresented: $choosingOrigin, arrowEdge: .bottom) {
                FlightOriginPicker(origins: data.originChoices, selectedID: $originID) { choosingOrigin = false }
            }
        }
    }

    private func map(routes: [FlightRoute], origin: FlightPlace?) -> some View {
        FlightMapCanvas(routes: routes, origin: origin, showTargets: showTargets, animated: horizon == .live,
                        selectedRouteID: selectedRouteID) { id in
            select(id)
        }
        .aspectRatio(2, contentMode: .fit)
        .clipShape(RoundedRectangle(cornerRadius: 14))
        .overlay(RoundedRectangle(cornerRadius: 14).strokeBorder(Color.secondary.opacity(0.22), lineWidth: 0.7))
        .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .top)
        .padding(.horizontal, 12)
    }

    private func routeList(routes: [FlightRoute]) -> some View {
        VStack(alignment: .leading, spacing: 10) {
            HStack {
                Text(horizon == .live ? "活跃线路" : "已观测流量排行").font(.headline)
                Spacer()
                Text("\(routes.count)").font(.caption.monospacedDigit()).foregroundColor(.secondary)
            }
            if horizon == .live {
                Text("字节数为当前活跃连接建立以来的累计值。").font(.caption).foregroundColor(.secondary)
            } else {
                Text("守护进程每 10 秒采样。短连接和连接关闭前的末段可能缺失，观测量是实际流量的下界。")
                    .font(.caption).foregroundColor(.secondary).fixedSize(horizontal: false, vertical: true)
            }
            if routes.isEmpty {
                VStack(spacing: 7) {
                    Image(systemName: "map").font(.title2).foregroundColor(.secondary)
                    Text(horizon == .live ? (data.online ? "当前没有活跃连接" : "尚无实时快照") : "所选时段没有观测记录").font(.callout)
                        .multilineTextAlignment(.center).foregroundColor(.secondary)
                }.frame(maxWidth: .infinity).padding(.vertical, 28)
            } else {
                ScrollView {
                    LazyVStack(spacing: 7) {
                        ForEach(routes) { route in
                            FlightRouteRow(route: route, selected: route.id == selectedRouteID) { select(route.id) }
                        }
                    }
                }
            }
        }
        .padding(14)
        .background(RoundedRectangle(cornerRadius: 12).fill(Color(nsColor: .controlBackgroundColor)))
        .overlay(RoundedRectangle(cornerRadius: 12).strokeBorder(Color.secondary.opacity(0.16), lineWidth: 0.7))
    }

    private func preserveOrSelectFirst(in routes: [FlightRoute]) {
        if !routes.contains(where: { $0.id == selectedRouteID }) { selectedRouteID = routes.first?.id }
    }

    private func select(_ id: String) {
        selectedRouteID = id
        onSelectRoute?(id)
    }
}

private struct FlightOriginPicker: View {
    let origins: [FlightPlace]
    @Binding var selectedID: String
    let dismiss: () -> Void
    @SwiftUI.State private var search = ""

    var body: some View {
        let query = search.trimmingCharacters(in: .whitespacesAndNewlines)
        let rows = origins.filter { place in
            place.coordinate != nil && (query.isEmpty || place.displayName.localizedCaseInsensitiveContains(query)
                                        || place.id.localizedCaseInsensitiveContains(query))
        }
        VStack(alignment: .leading, spacing: 10) {
            Text("示意起点").font(.headline)
            TextField("搜索地区或代码", text: $search).textFieldStyle(.roundedBorder)
            ScrollView {
                LazyVStack(spacing: 2) {
                    ForEach(rows) { place in
                        Button {
                            selectedID = place.id
                            dismiss()
                        } label: {
                            HStack {
                                Text(place.displayName).lineLimit(1)
                                Spacer()
                                Text(place.id).font(.caption.monospaced()).foregroundColor(.secondary)
                                if selectedID == place.id { Image(systemName: "checkmark").foregroundColor(FlightMapStyle.cyan) }
                            }
                            .padding(.horizontal, 8).frame(height: 30).contentShape(Rectangle())
                        }.buttonStyle(.plain)
                    }
                }
            }
            Text("只保存地区代码，用于地图布局。不会保存 IP 或城市。")
                .font(.caption).foregroundColor(.secondary)
        }
        .padding(14).frame(width: 290, height: 360)
    }
}

private struct FlightRouteRow: View {
    let route: FlightRoute
    let selected: Bool
    let action: () -> Void

    var body: some View {
        Button(action: action) {
            VStack(alignment: .leading, spacing: 6) {
                HStack(spacing: 7) {
                    Circle().fill(selected ? FlightMapStyle.cyan : Color.secondary.opacity(0.45)).frame(width: 7, height: 7)
                    Text(routeTitle).font(.callout.weight(.medium)).lineLimit(1)
                    Spacer(minLength: 4)
                    Text(FlightMapFormat.bytes(route.bytes)).font(.caption.monospacedDigit()).foregroundColor(.secondary)
                }
                if !geography.isEmpty {
                    Text(geography.joined(separator: " · ")).font(.caption).foregroundColor(.secondary).lineLimit(2)
                }
                HStack {
                    if route.activeConnections > 0 {
                        Label("\(route.activeConnections) 条", systemImage: "arrow.left.arrow.right")
                    }
                    if !route.hasKnownGeography {
                        Label("地区未知", systemImage: "questionmark.circle")
                    }
                    Spacer(minLength: 0)
                }.font(.caption).foregroundColor(.secondary)
            }
            .padding(9)
            .background(RoundedRectangle(cornerRadius: 8).fill(selected ? FlightMapStyle.cyan.opacity(0.13) : Color.clear))
            .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
        .accessibilityLabel(routeTitle)
        .accessibilityValue("流量 \(FlightMapFormat.bytes(route.bytes))，连接 \(route.activeConnections) 条\(route.hasKnownGeography ? "" : "，地区未知")")
        .accessibilityAddTraits(selected ? [.isSelected] : [])
    }

    private var routeTitle: String {
        let proxy = [route.entry?.displayName, route.exit?.displayName].compactMap { $0 }.reduce(into: [String]()) { values, item in
            if values.last != item { values.append(item) }
        }
        var parts = route.chain.isEmpty ? [] : [route.chain]
        parts.append(contentsOf: proxy)
        if let target = route.target { parts.append(target.displayName) }
        return parts.isEmpty ? "未知线路" : parts.joined(separator: " → ")
    }

    private var geography: [String] {
        [route.entry.map { "代理接入 \($0.displayName)" },
         route.exit.map { "配置的线路地区 \($0.displayName)" },
         route.target.map { "目标 \($0.displayName)" }].compactMap { $0 }
    }
}

private struct FlightMapCanvas: View {
    let routes: [FlightRoute]
    let origin: FlightPlace?
    let showTargets: Bool
    let animated: Bool
    let selectedRouteID: String?
    let select: (String) -> Void

    @Environment(\.colorScheme) private var colorScheme
    @Environment(\.accessibilityReduceMotion) private var reduceMotion
    @Environment(\.controlActiveState) private var controlActiveState

    var body: some View {
        let activeAnimation = animated && !reduceMotion && controlActiveState != .inactive
        ZStack {
            Canvas { context, size in
                drawBackground(context: &context, size: size)
                drawWorld(context: &context, size: size)
            }
            TimelineView(.animation(minimumInterval: activeAnimation ? 1 / 30 : 1,
                                    paused: routes.isEmpty || !activeAnimation)) { timeline in
                Canvas { context, size in
                    let peak = max(1, routes.map(\.bytes).max() ?? 1)
                    for route in routes where route.id != selectedRouteID {
                        draw(route: route, context: &context, size: size, peak: peak, selected: false,
                             time: timeline.date.timeIntervalSinceReferenceDate, animate: activeAnimation)
                    }
                    if let selected = routes.first(where: { $0.id == selectedRouteID }) {
                        draw(route: selected, context: &context, size: size, peak: peak, selected: true,
                             time: timeline.date.timeIntervalSinceReferenceDate, animate: activeAnimation)
                    }
                    drawMarkers(context: &context, size: size)
                }
            }
        }
        .background(Color(nsColor: colorScheme == .dark ? .black : .white))
        .contentShape(Rectangle())
        .overlay(alignment: .bottomLeading) {
            if routes.allSatisfy({ $0.legs(origin: origin, includeTarget: showTargets).isEmpty }) {
                Label(emptyMapMessage, systemImage: "mappin.and.ellipse")
                    .font(.callout).foregroundColor(.secondary).padding(14)
            }
        }
        .overlay(alignment: .topLeading) {
            HStack(spacing: 10) {
                FlightLegend(color: FlightMapStyle.cyan, title: "起点 → 接入")
                FlightLegend(color: FlightMapStyle.teal, title: "接入 → 配置地区")
                if showTargets { FlightLegend(color: FlightMapStyle.violet, title: "→ 目标") }
            }
            .padding(.horizontal, 10).padding(.vertical, 7)
            .background(.ultraThinMaterial, in: RoundedRectangle(cornerRadius: 8))
            .padding(10)
        }
        .overlay(FlightMapHitTargets(routes: routes, origin: origin, showTargets: showTargets,
                                     selectedRouteID: selectedRouteID, select: select))
        .accessibilityElement(children: .contain)
        .accessibilityLabel("网络线路地图")
    }

    private var emptyMapMessage: String {
        if routes.isEmpty { return animated ? "等待线路数据" : "暂无航线观测" }
        if routes.contains(where: \.hasKnownGeography) {
            return origin == nil ? "已标注已知地区，可选择地图起点" : "单一地区以落点显示"
        }
        return "线路地区未知"
    }

    private func drawBackground(context: inout GraphicsContext, size: CGSize) {
        let top = colorScheme == .dark ? Color(red: 0.055, green: 0.075, blue: 0.10) : Color(red: 0.92, green: 0.97, blue: 0.98)
        let bottom = colorScheme == .dark ? Color(red: 0.018, green: 0.025, blue: 0.042) : Color(red: 0.82, green: 0.91, blue: 0.93)
        context.fill(Path(CGRect(origin: .zero, size: size)), with: .linearGradient(Gradient(colors: [top, bottom]),
                                                                                   startPoint: .zero, endPoint: CGPoint(x: 0, y: size.height)))
        var grid = Path()
        for longitude in stride(from: -120.0, through: 120, by: 60) {
            if let a = FlightProjection.point(for: .init(latitude: -80, longitude: longitude), in: size),
               let b = FlightProjection.point(for: .init(latitude: 80, longitude: longitude), in: size) {
                grid.move(to: a); grid.addLine(to: b)
            }
        }
        for latitude in stride(from: -60.0, through: 60, by: 30) {
            if let a = FlightProjection.point(for: .init(latitude: latitude, longitude: -180), in: size),
               let b = FlightProjection.point(for: .init(latitude: latitude, longitude: 180), in: size) {
                grid.move(to: a); grid.addLine(to: b)
            }
        }
        context.stroke(grid, with: .color(Color.secondary.opacity(colorScheme == .dark ? 0.13 : 0.17)),
                       style: StrokeStyle(lineWidth: 0.6, dash: [2, 5]))
    }

    private func drawWorld(context: inout GraphicsContext, size: CGSize) {
        let fill = colorScheme == .dark ? Color.white.opacity(0.105) : Color(red: 0.27, green: 0.46, blue: 0.47).opacity(0.19)
        let line = colorScheme == .dark ? Color.white.opacity(0.20) : Color(red: 0.19, green: 0.36, blue: 0.38).opacity(0.30)
        for polygon in FlightWorld.land {
            var path = Path()
            for (index, coordinate) in polygon.enumerated() {
                guard let point = FlightProjection.point(for: coordinate, in: size) else { continue }
                if index == 0 { path.move(to: point) } else { path.addLine(to: point) }
            }
            path.closeSubpath()
            context.fill(path, with: .color(fill))
            context.stroke(path, with: .color(line), lineWidth: 0.7)
        }
    }

    private func draw(route: FlightRoute, context: inout GraphicsContext, size: CGSize, peak: Int64,
                      selected: Bool, time: TimeInterval, animate: Bool) {
        let width = routeWidth(route.bytes, peak: peak) + (selected ? 0.7 : 0)
        let opacity = selected ? 0.98 : 0.53
        for (legIndex, leg) in route.legs(origin: origin, includeTarget: showTargets).enumerated() {
            guard let from = leg.from.coordinate, let to = leg.to.coordinate else { continue }
            let color = legColor(leg.kind).opacity(opacity)
            for arc in FlightProjection.shiftedArcs(from: from, to: to, in: size) {
                var path = Path(); path.move(to: arc.0); path.addQuadCurve(to: arc.2, control: arc.1)
                context.stroke(path, with: .color(color), style: StrokeStyle(lineWidth: width, lineCap: .round))
                if selected {
                    let arrow = FlightProjection.quadraticPoint(0.64, start: arc.0, control: arc.1, end: arc.2)
                    context.draw(Text("→").font(.caption.bold()).foregroundColor(color), at: arrow)
                    let label = FlightProjection.quadraticPoint(0.46, start: arc.0, control: arc.1, end: arc.2)
                    if legIndex == 0, !route.chain.isEmpty, (0...size.width).contains(label.x) {
                        context.draw(Text(route.chain).font(.caption2.weight(.semibold)).foregroundColor(color),
                                     at: CGPoint(x: label.x, y: label.y - 10))
                    }
                }
                if animate && (selected || route.activeConnections > 0) {
                    let speed = 0.10 + Double(route.activeConnections.clamped(to: 0...20)) / 260
                    let progress = (time * speed + Double(legIndex) * 0.19).truncatingRemainder(dividingBy: 1)
                    let point = FlightProjection.quadraticPoint(progress, start: arc.0, control: arc.1, end: arc.2)
                    let glow = CGRect(x: point.x - width * 1.7, y: point.y - width * 1.7,
                                      width: width * 3.4, height: width * 3.4)
                    context.fill(Path(ellipseIn: glow), with: .color(color.opacity(0.24)))
                    let dot = CGRect(x: point.x - max(1.8, width / 2), y: point.y - max(1.8, width / 2),
                                     width: max(3.6, width), height: max(3.6, width))
                    context.fill(Path(ellipseIn: dot), with: .color(color))
                }
            }
        }
    }

    private func drawMarkers(context: inout GraphicsContext, size: CGSize) {
        var places: [String: (FlightPlace, Color)] = [:]
        if let origin, origin.coordinate != nil { places[origin.id] = (origin, FlightMapStyle.cyan) }
        for route in routes {
            if let entry = route.entry, entry.coordinate != nil { places[entry.id] = (entry, FlightMapStyle.cyan) }
            if let exit = route.exit, exit.coordinate != nil { places[exit.id] = (exit, FlightMapStyle.teal) }
            if showTargets, let target = route.target, target.coordinate != nil { places[target.id] = (target, FlightMapStyle.violet) }
        }
        for (_, item) in places {
            guard let coordinate = item.0.coordinate, let point = FlightProjection.point(for: coordinate, in: size) else { continue }
            context.fill(Path(ellipseIn: CGRect(x: point.x - 7, y: point.y - 7, width: 14, height: 14)),
                         with: .color(item.1.opacity(0.20)))
            context.fill(Path(ellipseIn: CGRect(x: point.x - 3, y: point.y - 3, width: 6, height: 6)), with: .color(item.1))
        }
    }

    private func legColor(_ kind: FlightLegKind) -> Color {
        switch kind {
        case .originToEntry: return FlightMapStyle.cyan
        case .entryToExit: return FlightMapStyle.teal
        case .exitToTarget: return FlightMapStyle.violet
        }
    }

    private func routeWidth(_ bytes: Int64, peak: Int64) -> CGFloat {
        guard bytes > 0, peak > 0 else { return 0.9 }
        let ratio = log10(Double(bytes) + 1) / max(1, log10(Double(peak) + 1))
        return 0.8 + CGFloat(ratio) * 2.0
    }
}

private struct FlightLegend: View {
    let color: Color
    let title: String

    var body: some View {
        HStack(spacing: 5) {
            Capsule().fill(color).frame(width: 15, height: 3)
            Text(title).font(.caption2).foregroundColor(.secondary)
        }
    }
}

private struct FlightMapHitTargets: View {
    let routes: [FlightRoute]
    let origin: FlightPlace?
    let showTargets: Bool
    let selectedRouteID: String?
    let select: (String) -> Void

    var body: some View {
        GeometryReader { geometry in
            ZStack {
                ForEach(routes) { route in
                    if let point = routeFocus(route, size: geometry.size) {
                        Button { select(route.id) } label: { Color.clear.frame(width: 30, height: 30) }
                            .buttonStyle(.plain).position(point)
                            .accessibilityLabel("选择线路 \(route.id)")
                            .accessibilityAddTraits(route.id == selectedRouteID ? [.isSelected] : [])
                    }
                }
            }
        }
    }

    private func routeFocus(_ route: FlightRoute, size: CGSize) -> CGPoint? {
        guard let leg = route.legs(origin: origin, includeTarget: showTargets).last,
              let from = leg.from.coordinate, let to = leg.to.coordinate,
              let arc = FlightProjection.arc(from: from, to: to, in: size) else { return nil }
        return FlightProjection.quadraticPoint(0.5, start: arc.start, control: arc.control, end: arc.end)
    }
}

private enum FlightMapFormat {
    static let byteFormatter: ByteCountFormatter = {
        let value = ByteCountFormatter()
        value.allowedUnits = [.useKB, .useMB, .useGB, .useTB]
        value.countStyle = .binary
        return value
    }()

    static func bytes(_ value: Int64) -> String { byteFormatter.string(fromByteCount: max(0, value)) }
}

private extension Int {
    func clamped(to range: ClosedRange<Int>) -> Int { Swift.min(range.upperBound, Swift.max(range.lowerBound, self)) }
}
