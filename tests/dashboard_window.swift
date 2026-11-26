import AppKit
import SwiftUI
import UserNotifications

@main
@MainActor
private struct DashboardWindowTests {
    static func main() async throws {
        let args = CommandLine.arguments
        precondition(args.count == 2 || args.count == 3, "usage: dashboard-window-tests READER [PREVIEW_DIRECTORY]")
        let temp = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString, isDirectory: true)
        try FileManager.default.createDirectory(at: temp, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: temp) }
        let suite = "com.lonelygo.mi-guard.test.\(UUID().uuidString)"
        let preferences = UserDefaults(suiteName: suite)!
        defer { preferences.removePersistentDomain(forName: suite) }
        try testDecisionEvidencePresentation()
        let config = try writeExample(in: temp)
        try testTransientNotificationFocus(in: temp, reader: URL(fileURLWithPath: args[1]),
                                           preferences: preferences)
        let empty = temp.appendingPathComponent("second config.yaml")
        try "history: {file: empty.json}\ngroups: []\n".write(to: empty, atomically: true, encoding: .utf8)
        let inbox = NotificationInboxStore(fileURL: temp.appendingPathComponent("notifications.json"))
        try inbox.upsert(NotificationRecord(
            id: "fixture-switch-notification", kind: "switch", title: "美西优选 已切换",
            body: "链路故障恢复：洛杉矶 · LA 02 → 洛杉矶 · LA 01",
            deliveredAt: Date(), configPath: config.path, decisionID: "fixture-switch"
        ))
        let controller = DashboardController(reader: URL(fileURLWithPath: args[1]), preferences: preferences,
                                             notificationInbox: inbox)
        defer { controller.stop() }
        precondition(controller.notificationRecords.map(\.id) == ["fixture-switch-notification"])
        controller.focusNotification("fixture-switch-notification")
        precondition(controller.section == .notifications
                     && controller.selectedNotificationID == "fixture-switch-notification")
        precondition(!controller.loading && controller.snapshot == nil)
        controller.section = .overview
        controller.useConfig(config, remember: true)
        try await waitForRead(controller)
        let first = try requireSnapshot(controller)
        precondition(first.samples.count > 30 && first.groups.count == 4 && first.pendingBytes == 16_777_216)
        precondition(Set(first.samples.map(\.streams)) == [1, 4])
        precondition(first.samples.contains { !$0.successful })
        let history = DashboardHistory(first.samples)
        let allTime = Date.distantPast...Date.distantFuture
        let four = history.preferred.first { $0.profile.streams == 4 }!
        let known = DashboardComparison(history: history, focus: four, domain: allTime)
        precondition(known.comparable && known.rows.count == 2 && known.rows.allSatisfy { $0.series.profile == four.profile })
        precondition(!history.routes.contains { $0.name == "香港 · HK 03" })

        // Keep the last good view on a read error, clear it immediately when changing sources.
        let ledger = temp.appendingPathComponent("history.json")
        let good = try Data(contentsOf: ledger)
        try Data("broken json".utf8).write(to: ledger, options: .atomic)
        controller.refresh()
        try await waitForRead(controller)
        precondition(controller.problem != nil && controller.snapshot?.samples.count == first.samples.count)
        try good.write(to: ledger, options: .atomic)
        controller.refresh()
        controller.useConfig(empty, remember: false)
        precondition(controller.snapshot == nil)
        try await waitForRead(controller)
        let blank = try requireSnapshot(controller)
        precondition(blank.samples.isEmpty && blank.usedBytes == 0)
        precondition(!FileManager.default.fileExists(atPath: temp.appendingPathComponent("empty.json").path))
        let resumed = DashboardController(reader: URL(fileURLWithPath: args[1]), preferences: preferences,
                                          notificationInbox: inbox)
        precondition(resumed.configURL == config.standardizedFileURL)
        if args.count == 3 {
            let directory = URL(fileURLWithPath: args[2], isDirectory: true)
            try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
            try await render(controller, config: config, inbox: inbox, into: directory)
        }
        print("Dashboard source, refresh and offline history tests passed")
    }

    private static func testDecisionEvidencePresentation() throws {
        let legacyJSON = #"""
        {
          "id":"legacy-recovery","at":"2026-11-20T08:00:00Z","source":"daemon",
          "group":"warp","before":"a","after":"g","action":"switched","trigger":"hardFailure",
          "candidates":[
            {"node":"a","kind":"recovery","mbps":18,"rank":1},
            {"node":"g","kind":"recovery","mbps":12,"rank":7,"selected":true}
          ]
        }
        """#.data(using: .utf8)!
        let decoder = JSONDecoder()
        decoder.dateDecodingStrategy = .iso8601
        let legacy = try decoder.decode(DecisionRecord.self, from: legacyJSON)
        precondition(DecisionEvidencePresentation.legacyRecoverySummary(legacy) ==
                     "采用排序 #7。此旧记录未保存逐候选在线验证结果，无法还原排序 #1–#6 的具体失败阶段。")
        precondition(legacy.evidence.allSatisfy {
            DecisionEvidencePresentation.validationLabel($0, recovery: true) == "在线验证结果未记录（旧记录）"
        })
        precondition(DecisionEvidencePresentation.displayOrder(legacy.evidence).map(\.rank) == [1, 7])

        let orderedJSON = #"""
        {
          "id":"ordered-recovery","at":"2026-11-25T08:00:00Z","source":"daemon",
          "group":"warp","before":"a","after":"a","action":"switched","trigger":"hardFailure",
          "candidates":[
            {"node":"a","rank":1,"validationOrder":3,"validation":"fallbackRegionMismatch","selected":true},
            {"node":"b","rank":2,"validationOrder":1,"validation":"regionMismatch"},
            {"node":"c","rank":3,"validationOrder":2,"validation":"regionProbeFailed"},
            {"node":"d","rank":4,"validation":"notReached"}
          ]
        }
        """#.data(using: .utf8)!
        let ordered = try decoder.decode(DecisionRecord.self, from: orderedJSON)
        precondition(DecisionEvidencePresentation.displayOrder(ordered.evidence).map(\.node) == ["b", "c", "a", "d"])
        let recoveryReliability = DecisionEvidencePresentation.strategyReliability(failures: 2, attempts: 9, recovery: true)
        precondition(recoveryReliability == "排序可靠性失败 2/9（截至参考样本）" && !recoveryReliability.contains("本轮"))
        precondition(DecisionEvidencePresentation.strategyReliability(failures: 1, attempts: 4, recovery: false).contains("本轮"))

        let latencyJSON = #"{"id":"latency","at":"2026-11-25T08:00:00Z","source":"manual","group":"warp","action":"kept","latencyFactor":1.5,"candidates":[{"node":"slow","latencyMS":975,"validation":"latencyAboveLimit"},{"node":"unknown","validation":"latencyUnknown"}]}"#.data(using: .utf8)!
        let latency = try decoder.decode(DecisionRecord.self, from: latencyJSON)
        precondition(latency.latencyFactor == 1.5 && latency.latencyThresholdMS == nil)
        precondition(DecisionEvidencePresentation.validationLabel(latency.evidence[0], recovery: false)?.contains("抖动阈值") == true)
        precondition(DecisionEvidencePresentation.validationLabel(latency.evidence[1], recovery: false)?.contains("缺少有效") == true)
    }

    private static func testTransientNotificationFocus(in directory: URL, reader: URL,
                                                       preferences: UserDefaults) throws {
        let fullStore = NotificationInboxStore(fileURL: directory.appendingPathComponent("full-notifications.json"))
        for index in 0..<NotificationInboxStore.retentionLimit {
            try fullStore.upsert(NotificationRecord(
                id: "recent-\(index)", kind: "summary", title: "扫描", body: "\(index)",
                deliveredAt: Date(timeIntervalSince1970: TimeInterval(10_000 + index))
            ))
        }
        let controller = DashboardController(reader: reader, preferences: preferences, notificationInbox: fullStore)
        let oldClick = NotificationRecord(id: "old-click", kind: "switch", title: "旧横幅", body: "仍应显示",
                                          deliveredAt: Date(timeIntervalSince1970: 1), decisionID: "old-decision")
        controller.observeNotification(oldClick, select: true)
        precondition(controller.notificationRecords.count == NotificationInboxStore.retentionLimit)
        precondition(controller.notificationRecords.contains { $0.id == "old-click" })
        precondition(controller.selectedNotificationID == "old-click" && controller.section == .notifications)

        let blockedParent = directory.appendingPathComponent("notification-storage-blocker")
        try Data("block".utf8).write(to: blockedParent)
        let blockedStore = NotificationInboxStore(fileURL: blockedParent.appendingPathComponent("notifications.json"))
        let fallback = DashboardController(reader: reader, preferences: preferences, notificationInbox: blockedStore)
        fallback.observeNotification(oldClick, select: true)
        precondition(fallback.notificationProblem != nil)
        precondition(fallback.notificationRecords == [oldClick] && fallback.selectedNotificationID == oldClick.id)
    }

    private static func waitForRead(_ controller: DashboardController) async throws {
        for _ in 0..<250 {
            if !controller.loading { return }
            try await Task.sleep(nanoseconds: 20_000_000)
        }
        throw HelperError("dashboard history reader timed out")
    }

    private static func requireSnapshot(_ controller: DashboardController) throws -> DashboardSnapshot {
        guard let snapshot = controller.snapshot, controller.problem == nil else {
            throw HelperError(controller.problem ?? "dashboard snapshot is missing")
        }
        return snapshot
    }

    private static func writeExample(in directory: URL) throws -> URL {
        let config = directory.appendingPathComponent("example config.yaml")
        try """
        history: {file: history.json}
        groups:
          - {name: 亚太优选, mode: selector}
          - {name: 美西优选, chain: 美西出口}
          - {name: 亚洲直出, mode: selector}
          - {name: 美国直出, mode: selector}
        """.write(to: config, atomically: true, encoding: .utf8)
        let now = Date()
        let fmt = ISO8601DateFormatter()
        fmt.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        fmt.timeZone = .current // the on-disk billing day starts at local midnight
        func ago(_ hours: Double) -> String { fmt.string(from: now.addingTimeInterval(-3600 * hours)) }
        let start = Calendar.current.dateInterval(of: .month, for: now)!.start
        let nodes = ["东京 · Tokyo 01", "新加坡 · SG 02", "洛杉矶 · LA 01", "香港 · HK 03"]
        let profile = String(repeating: "a", count: 64)
        var samples: [[String: Any]] = []
        for i in 0..<21 {
            for (n, node) in nodes.enumerated() {
                let speed = 78.0 + 15 * sin(Double(i) * 0.78 + Double(n)) + Double(n * 8 + i)
                let failed = i == 11 && n == 0
                let legacy = n == 3
                let single = n == 1
                let total = single || legacy ? 4_194_304 : 16_777_216
                samples.append([
                    "time": ago(Double(20 - i) * 4.4 + Double(n) * 0.15 + 0.2),
                    "node": node, "provider": n < 2 ? "订阅 A" : "订阅 B",
                    "kind": single ? "sweep" : "bench",
                    "scope": legacy ? "front" : "download/v1/\(single ? 1 : 4)/\(total)/\(profile)/front",
                    "download_mbps": failed ? 0 : speed, "bytes": failed ? 0 : total, "elapsed_ms": 1200,
                    "probe_latency_ms": failed ? 0 : 42 + n * 23 + i % 7, "probe_attempts": failed ? 1 : 2,
                    "probe_failures": failed ? 1 : 0, "error": failed ? "204 probe: HTTP 504" : ""
                ])
            }
        }
        let ledger: [String: Any] = ["version": 1, "cycle_start": fmt.string(from: start), "used_bytes": 2_031_616_000,
                                     "reservations": ["sample": ["bytes": 16_777_216, "created_at": ago(0.02)]],
                                     "samples": samples, "last_sweep": ago(2.7)]
        func selection(_ node: String, hours: Double, longest: Int64) -> [String: Any] {
            ["node": node, "since": ago(hours), "last_seen": ago(0.15), "guard_selected": true,
             "longest_observed_ns": [node: longest * 1_000_000_000]]
        }
        let selections = ["亚太优选": selection(nodes[0], hours: 31, longest: 200_000),
                          "美西优选": selection(nodes[2], hours: 12, longest: 80_000),
                          "亚洲直出": selection(nodes[1], hours: 8, longest: 70_000),
                          "美国直出": selection(nodes[2], hours: 12, longest: 80_000)]
        for (name, object) in [("history.json", ledger), ("history.json.selections.json", selections as [String: Any])] {
            try JSONSerialization.data(withJSONObject: object).write(to: directory.appendingPathComponent(name), options: .atomic)
        }
        try writeDecisionHistory(in: directory, at: now)
        return config
    }

    private static func writeDecisionHistory(in directory: URL, at: Date) throws {
        let sampleAt = ISO8601DateFormatter().string(from: at.addingTimeInterval(-3600))
        let decision: [String: Any] = [
            "id": "fixture-switch", "at": ISO8601DateFormatter().string(from: at), "source": "manual",
            "group": "美西优选", "chain": "美西出口", "before": "洛杉矶 · LA 02", "after": "洛杉矶 · LA 01",
            "action": "switched", "trigger": "manualBench", "nearBestPct": 20, "thresholdMbps": 92.8, "minMbps": 40,
            "latencyMeanMS": 140.0, "latencySamples": 40, "latencyFactor": 1.5, "latencyThresholdMS": 210.0,
            "candidates": [
                ["node": "洛杉矶 · LA 01", "kind": "bench", "observedAt": sampleAt,
                 "mbps": 97.0, "latencyMS": 168, "attempts": 2,
                 "streams": 4, "requestedBytes": 16_777_216, "bytes": 16_777_216, "rank": 1, "preferredBand": true,
                 "strategyAttempts": 42, "strategyFailures": 0, "historyAttempts": 40, "historyFailures": 0,
                 "historySamples": 3, "historyP25Mbps": 90.0, "validationOrder": 1,
                 "validation": "passed", "selected": true],
                ["node": "洛杉矶 · LA 02", "kind": "bench", "mbps": 116.0, "latencyMS": 182, "attempts": 2,
                 "streams": 4, "requestedBytes": 16_777_216, "bytes": 16_777_216, "rank": 2, "preferredBand": true,
                 "strategyAttempts": 42, "strategyFailures": 8, "historyAttempts": 40, "historyFailures": 8,
                 "historySamples": 3, "historyP25Mbps": 70.0],
                ["node": "洛杉矶 · LA 03", "kind": "bench", "attempts": 1, "failures": 1,
                 "streams": 4, "requestedBytes": 16_777_216, "stage": "probe", "status": "HTTP 504"]
            ],
            "followers": [["group": "美国节点", "before": "洛杉矶 · LA 02", "after": "洛杉矶 · LA 01", "action": "switched"]]
        ]
        let document = try JSONSerialization.data(withJSONObject: decision)
        let hex = document.map { String(format: "%02x", $0) }.joined()
        let nanos = Int64(at.timeIntervalSince1970 * 1_000_000_000)
        let sql = "CREATE TABLE decisions (id TEXT PRIMARY KEY NOT NULL, at_ns INTEGER NOT NULL, document BLOB NOT NULL);" +
                  "CREATE INDEX decisions_at ON decisions(at_ns DESC);PRAGMA user_version = 1;" +
                  "INSERT INTO decisions VALUES('fixture-switch',\(nanos),X'\(hex)');"
        let process = Process()
        process.executableURL = URL(fileURLWithPath: "/usr/bin/sqlite3")
        process.arguments = [directory.appendingPathComponent("history.json.analytics.sqlite3").path, sql]
        let diagnostics = Pipe()
        process.standardError = diagnostics
        try process.run()
        process.waitUntilExit()
        guard process.terminationStatus == 0 else {
            let detail = String(decoding: diagnostics.fileHandleForReading.readDataToEndOfFile(), as: UTF8.self)
            throw HelperError("cannot create decision fixture: \(detail)")
        }
    }

    private static func render(_ controller: DashboardController, config: URL, inbox: NotificationInboxStore,
                               into directory: URL) async throws {
        let app = NSApplication.shared
        app.setActivationPolicy(.accessory)
        app.finishLaunching()
        let view = DashboardWindow(controller: controller, restoreFrame: false)
        defer { view.window.orderOut(nil) }
        let switchClick = DashboardDestination(action: UNNotificationDefaultActionIdentifier, thread: "mi-guard.switch",
                                               userInfo: ["guardConfigPath": config.path, "decisionID": "fixture-switch",
                                                          "notificationID": "fixture-switch-notification"])!
        let previousConfig = controller.configURL
        view.present(switchClick)
        try await waitForRead(controller)
        precondition(view.window.isVisible && controller.section == .notifications
                     && controller.configURL == previousConfig)
        precondition(controller.selectedNotificationID == "fixture-switch-notification")
        view.window.appearance = NSAppearance(named: .darkAqua)
        try await capture(view.window, at: directory.appendingPathComponent("switch-notification.png"))
        let switchRecord = controller.notificationRecords.first { $0.id == "fixture-switch-notification" }!
        controller.openNotificationDestination(switchRecord)
        try await waitForRead(controller)
        precondition(controller.section == .selections && controller.requestedDecisionID == "fixture-switch")
        precondition(controller.snapshot?.selectedDecision?.id == "fixture-switch")
        precondition(controller.snapshot?.selectedDecision?.evidence.first?.validation == "passed")
        precondition(controller.snapshot?.selectedDecision?.evidence.first?.observedAt != nil)
        controller.focusDecision("expired-decision-id")
        try await waitForRead(controller)
        precondition(controller.snapshot?.decisionWarning?.contains("已过期") == true)
        let windowNumber = view.window.windowNumber
        try inbox.upsert(NotificationRecord(id: "fixture-summary-notification", kind: "summary", title: "完整扫描",
                                            body: "2 个组完成扫描", deliveredAt: Date()))
        let summaryClick = DashboardDestination(action: UNNotificationDefaultActionIdentifier, thread: "mi-guard.summary",
                                                userInfo: ["notificationID": "fixture-summary-notification"])!
        view.present(summaryClick)
        precondition(view.window.windowNumber == windowNumber && controller.section == .notifications)
        precondition(controller.selectedNotificationID == "fixture-summary-notification")
        precondition(controller.notificationRecords.contains { $0.id == "fixture-summary-notification" })
        view.window.appearance = NSAppearance(named: .darkAqua)
        try await capture(view.window, at: directory.appendingPathComponent("notifications-dark.png"))
        let legacySummaryClick = DashboardDestination(action: UNNotificationDefaultActionIdentifier,
                                                      thread: "mi-guard.summary", userInfo: [:])!
        view.present(legacySummaryClick)
        precondition(controller.section == .overview && controller.requestedDecisionID == nil)
        try await waitForRead(controller)
        try await verifyInteractions(view, controller: controller, config: config, inbox: inbox)
        view.showSettings(nil)
        if let settings = view.settingsWindow {
            settings.appearance = NSAppearance(named: .darkAqua)
            try await capture(settings, at: directory.appendingPathComponent("settings.png"))
            settings.orderOut(nil)
        }
        controller.clearDecisionFocus()
        try await waitForRead(controller)
        for (section, appearance, name) in [
            (DashboardSection.overview, NSAppearance.Name.darkAqua, "overview-dark"),
            (.overview, .aqua, "overview-light"), (.nodes, .darkAqua, "nodes-dark"), (.selections, .aqua, "selections-light")
        ] {
            controller.section = section
            view.window.appearance = NSAppearance(named: appearance)
            try await capture(view.window, at: directory.appendingPathComponent(name + ".png"))
        }
        view.window.setContentSize(view.window.contentMinSize)
        view.window.appearance = NSAppearance(named: .darkAqua)
        try await capture(view.window, at: directory.appendingPathComponent("selections-compact.png"))
        // A v0.1.3 installation can have only unlabelled downloads and no selection sidecar.
        let fixture = config.deletingLastPathComponent()
        var old = try JSONSerialization.jsonObject(with: Data(contentsOf: fixture.appendingPathComponent("history.json"))) as! [String: Any]
        old["samples"] = (old["samples"] as! [[String: Any]]).map { row in
            var legacy = row
            legacy["scope"] = "front"
            return legacy
        }
        try JSONSerialization.data(withJSONObject: old).write(to: fixture.appendingPathComponent("legacy.json"))
        let oldConfig = fixture.appendingPathComponent("legacy.yaml")
        try "history: {file: legacy.json}\ngroups: [{name: 亚太优选, mode: selector}]\n".write(to: oldConfig, atomically: true, encoding: .utf8)
        controller.useConfig(oldConfig, remember: false)
        controller.section = .overview
        try await waitForRead(controller)
        let loaded = try requireSnapshot(controller)
        precondition(loaded.samples.isEmpty && loaded.groups.first?.lastSeen == nil)
        precondition(loaded.usedBytes == 2_031_616_000 && loaded.pendingBytes == 16_777_216)
        view.window.appearance = NSAppearance(named: .darkAqua)
        try await capture(view.window, at: directory.appendingPathComponent("overview-legacy.png"))
        view.window.setContentSize(view.window.contentMinSize)
        try await capture(view.window, at: directory.appendingPathComponent("overview-compact.png"))
        print("Dashboard notification destinations, window reuse, compact layout and previews passed")
    }

    private static func verifyInteractions(_ dashboard: DashboardWindow, controller: DashboardController,
                                           config: URL, inbox: NotificationInboxStore) async throws {
        dashboard.installMenus()
        try await verifyDisclosurePointerArea()
        controller.useConfig(config, remember: true)
        controller.section = .overview
        try await waitForRead(controller)
        try await settle(dashboard.window)

        let nodeMenu = try requirePopUp(named: "节点与路径", in: dashboard.window)
        let methodMenu = try requirePopUp(named: "测量方式", in: dashboard.window)
        for menu in [nodeMenu, methodMenu] {
            guard menu.bounds.height >= 38 && menu.bounds.width > 180 else {
                throw HelperError("popup hit area too small: \(menu.accessibilityLabel() ?? "unknown") \(menu.bounds)")
            }
            for point in [CGPoint(x: 4, y: 4), CGPoint(x: menu.bounds.midX, y: menu.bounds.midY),
                          CGPoint(x: menu.bounds.maxX - 4, y: menu.bounds.maxY - 4)] {
                guard menu.hitTest(point) === menu else {
                    throw HelperError("popup did not accept pointer at \(point): \(menu.bounds)")
                }
            }
        }
        precondition(nodeMenu.numberOfItems >= 2)
        let beforeSelection = renderedData(dashboard.window)
        let formerNode = nodeMenu.titleOfSelectedItem
        let nextNode = (nodeMenu.indexOfSelectedItem + 1) % nodeMenu.numberOfItems
        nodeMenu.selectItem(at: nextNode)
        nodeMenu.sendAction(nodeMenu.action, to: nodeMenu.target)
        try await settle(dashboard.window)
        let changedNode = try requirePopUp(named: "节点与路径", in: dashboard.window)
        precondition(changedNode.titleOfSelectedItem != formerNode && changedNode.indexOfSelectedItem == nextNode)
        let afterSelection = renderedData(dashboard.window)
        precondition(beforeSelection != nil && afterSelection != nil && beforeSelection != afterSelection)

        controller.clearDecisionFocus()
        controller.section = .selections
        try await settle(dashboard.window)
        precondition(controller.snapshot?.groups.map(\.name) == ["亚太优选", "美西优选", "亚洲直出", "美国直出"])
        controller.focusDecision("fixture-switch")
        try await waitForRead(controller)
        try await settle(dashboard.window)
        precondition(controller.snapshot?.selectedDecision?.id == "fixture-switch")

        // Exercise both settings entry points. The menu test uses the real Command-comma key equivalent.
        dashboard.settingsWindow?.orderOut(nil)
        let commandComma = NSEvent.keyEvent(with: .keyDown, location: .zero, modifierFlags: .command,
                                             timestamp: ProcessInfo.processInfo.systemUptime,
                                             windowNumber: dashboard.window.windowNumber, context: nil,
                                             characters: ",", charactersIgnoringModifiers: ",",
                                             isARepeat: false, keyCode: 43)!
        precondition(NSApp.mainMenu?.performKeyEquivalent(with: commandComma) == true)
        try await settle(dashboard.settingsWindow!)
        precondition(dashboard.settingsWindow?.isVisible == true)
        dashboard.settingsWindow?.orderOut(nil)
        // DashboardSidebar is 176 pt wide and its settings action occupies the bottom row.
        try pointerClick(CGPoint(x: 88, y: 22), in: dashboard.window)
        try await settle(dashboard.settingsWindow!)
        precondition(dashboard.settingsWindow?.isVisible == true)
        dashboard.settingsWindow?.orderOut(nil)

        // Verify the settings default action without reading the operator's default config.
        let settingsSuite = "com.lonelygo.mi-guard.settings-test.\(UUID().uuidString)"
        let settingsPreferences = UserDefaults(suiteName: settingsSuite)!
        defer { settingsPreferences.removePersistentDomain(forName: settingsSuite) }
        let settingsController = DashboardController(reader: URL(fileURLWithPath: "/usr/bin/false"),
                                                     preferences: settingsPreferences, notificationInbox: inbox)
        settingsController.useConfig(config, remember: true)
        precondition(settingsPreferences.string(forKey: "dashboard.guardConfigPath") == config.path)
        let settingsWindow = NSWindow(contentViewController: NSHostingController(rootView: DashboardSettingsView(controller: settingsController)))
        settingsWindow.makeKeyAndOrderFront(nil)
        defer { settingsWindow.orderOut(nil); settingsController.stop() }
        try await settle(settingsWindow)
        settingsController.useConfig(nil, remember: true)
        try await settle(settingsWindow)
        precondition(settingsController.configURL == nil)
        precondition(settingsPreferences.string(forKey: "dashboard.guardConfigPath") == nil)
        let restored = DashboardController(reader: URL(fileURLWithPath: "/usr/bin/false"),
                                           preferences: settingsPreferences, notificationInbox: inbox)
        precondition(restored.configURL == nil)
        restored.stop()
        print("Dashboard full-row pointer, picker action, disclosure and settings interaction tests passed")
    }

    @MainActor
    private final class DisclosureProbe: ObservableObject {
        @Published var expanded = false
    }

    private struct DisclosureProbeView: View {
        @ObservedObject var probe: DisclosureProbe

        var body: some View {
            DashboardDisclosureButton(isExpanded: $probe.expanded) {
                VStack(alignment: .leading) {
                    Text("完整行")
                    Text("副标题").font(.caption)
                }
            }
            .frame(width: 520, height: 54)
            .padding(20)
        }
    }

    private static func verifyDisclosurePointerArea() async throws {
        let probe = DisclosureProbe()
        let window = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 560, height: 94),
                              styleMask: [.borderless], backing: .buffered, defer: false)
        window.contentView = NSHostingView(rootView: DisclosureProbeView(probe: probe))
        window.makeKeyAndOrderFront(nil)
        defer { window.orderOut(nil) }
        try await settle(window)
        try pointerClick(CGPoint(x: 28, y: 47), in: window)
        try await settle(window)
        precondition(probe.expanded)
        try pointerClick(CGPoint(x: 530, y: 47), in: window)
        try await settle(window)
        precondition(!probe.expanded)
    }

    private static func settle(_ window: NSWindow) async throws {
        window.contentView?.layoutSubtreeIfNeeded()
        window.contentView?.displayIfNeeded()
        try await Task.sleep(nanoseconds: 120_000_000)
    }

    private static func pointerClick(_ point: CGPoint, in window: NSWindow) throws {
        for type in [NSEvent.EventType.leftMouseDown, .leftMouseUp] {
            guard let event = NSEvent.mouseEvent(with: type, location: point, modifierFlags: [],
                                                  timestamp: ProcessInfo.processInfo.systemUptime,
                                                  windowNumber: window.windowNumber, context: nil,
                                                  eventNumber: 0, clickCount: 1, pressure: 1) else {
                throw HelperError("cannot create pointer event")
            }
            window.sendEvent(event)
        }
    }

    private static func requirePopUp(named name: String, in window: NSWindow) throws -> NSPopUpButton {
        guard let value = popUp(named: name, in: window) else { throw HelperError("missing popup \(name)") }
        return value
    }

    private static func popUp(named name: String, in window: NSWindow) -> NSPopUpButton? {
        allSubviews(window.contentView).compactMap { $0 as? NSPopUpButton }.first { $0.accessibilityLabel() == name }
    }

    private static func allSubviews(_ root: NSView?) -> [NSView] {
        guard let root else { return [] }
        return [root] + root.subviews.flatMap(allSubviews)
    }

    private static func renderedData(_ window: NSWindow) -> Data? {
        guard let view = window.contentView,
              let image = view.bitmapImageRepForCachingDisplay(in: view.bounds) else { return nil }
        view.cacheDisplay(in: view.bounds, to: image)
        return image.representation(using: .png, properties: [:])
    }

    private static func capture(_ window: NSWindow, at url: URL) async throws {
        try await Task.sleep(nanoseconds: 250_000_000)
        guard let surface = window.contentView else { throw HelperError("missing dashboard content view") }
        surface.layoutSubtreeIfNeeded()
        surface.displayIfNeeded()
        guard let image = surface.bitmapImageRepForCachingDisplay(in: surface.bounds) else { throw HelperError("cannot render dashboard") }
        surface.cacheDisplay(in: surface.bounds, to: image)
        guard let png = image.representation(using: .png, properties: [:]) else { throw HelperError("cannot encode dashboard image") }
        try png.write(to: url, options: .atomic)
    }
}
