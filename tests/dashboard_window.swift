import AppKit
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
        let config = try writeExample(in: temp)
        let empty = temp.appendingPathComponent("second config.yaml")
        try "history: {file: empty.json}\ngroups: []\n".write(to: empty, atomically: true, encoding: .utf8)
        let controller = DashboardController(reader: URL(fileURLWithPath: args[1]), preferences: preferences)
        defer { controller.stop() }
        controller.useConfig(config, remember: true)
        try await waitForRead(controller)
        let first = try requireSnapshot(controller)
        precondition(first.samples.count > 30 && first.groups.count == 2 && first.pendingBytes == 16_777_216)
        precondition(first.samples.contains { $0.streams == 4 } && first.samples.contains { $0.streams == 0 })
        precondition(first.samples.contains { !$0.successful })
        let history = DashboardHistory(first.samples)
        let allTime = Date.distantPast...Date.distantFuture
        let four = history.preferred.first { $0.profile.streams == 4 }!
        let known = DashboardComparison(history: history, focus: four, domain: allTime)
        precondition(known.comparable && known.rows.count == 2 && known.rows.allSatisfy { $0.series.profile == four.profile })
        let legacy = history.preferred.first { !$0.profile.isKnown }!
        let recent = DashboardComparison(history: history, focus: legacy, domain: allTime)
        precondition(!recent.comparable && recent.rows.map(\.sample.time) == recent.rows.map(\.sample.time).sorted(by: >))

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
        let resumed = DashboardController(reader: URL(fileURLWithPath: args[1]), preferences: preferences)
        precondition(resumed.configURL == config.standardizedFileURL)
        if args.count == 3 {
            let directory = URL(fileURLWithPath: args[2], isDirectory: true)
            try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
            try await render(controller, config: config, into: directory)
        }
        print("Dashboard source, refresh and offline history tests passed")
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
                          "美西优选": selection(nodes[2], hours: 12, longest: 80_000)]
        for (name, object) in [("history.json", ledger), ("history.json.selections.json", selections as [String: Any])] {
            try JSONSerialization.data(withJSONObject: object).write(to: directory.appendingPathComponent(name), options: .atomic)
        }
        try writeDecisionHistory(in: directory, at: now)
        return config
    }

    private static func writeDecisionHistory(in directory: URL, at: Date) throws {
        let decision: [String: Any] = [
            "id": "fixture-switch", "at": ISO8601DateFormatter().string(from: at), "source": "manual",
            "group": "美西优选", "chain": "美西出口", "before": "洛杉矶 · LA 02", "after": "洛杉矶 · LA 01",
            "action": "switched", "trigger": "manualBench", "nearBestPct": 20, "thresholdMbps": 92.8, "minMbps": 40,
            "candidates": [
                ["node": "洛杉矶 · LA 01", "kind": "bench", "mbps": 97.0, "latencyMS": 168, "attempts": 2,
                 "streams": 4, "requestedBytes": 16_777_216, "bytes": 16_777_216, "rank": 1, "preferredBand": true,
                 "strategyAttempts": 42, "strategyFailures": 0, "historyAttempts": 40, "historyFailures": 0,
                 "historySamples": 3, "historyP25Mbps": 90.0, "selected": true],
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

    private static func render(_ controller: DashboardController, config: URL, into directory: URL) async throws {
        let app = NSApplication.shared
        app.setActivationPolicy(.accessory)
        app.finishLaunching()
        let view = DashboardWindow(controller: controller, restoreFrame: false)
        defer { view.window.orderOut(nil) }
        let switchClick = DashboardDestination(action: UNNotificationDefaultActionIdentifier, thread: "mi-guard.switch",
                                               userInfo: ["guardConfigPath": config.path, "decisionID": "fixture-switch"])!
        let summaryClick = DashboardDestination(action: UNNotificationDefaultActionIdentifier, thread: "mi-guard.summary", userInfo: [:])!
        view.present(switchClick)
        try await waitForRead(controller)
        precondition(view.window.isVisible && controller.section == .selections && controller.configURL == config.standardizedFileURL)
        precondition(controller.requestedDecisionID == "fixture-switch" && controller.snapshot?.selectedDecision?.id == "fixture-switch")
        controller.focusDecision("expired-decision-id")
        try await waitForRead(controller)
        precondition(controller.snapshot?.decisionWarning?.contains("已过期") == true)
        let windowNumber = view.window.windowNumber
        view.present(summaryClick)
        precondition(view.window.windowNumber == windowNumber && controller.section == .overview && controller.requestedDecisionID == nil)
        try await waitForRead(controller)
        for (section, appearance, name) in [
            (DashboardSection.overview, NSAppearance.Name.darkAqua, "overview-dark"),
            (.overview, .aqua, "overview-light"), (.nodes, .darkAqua, "nodes-dark"), (.selections, .aqua, "selections-light")
        ] {
            controller.section = section
            view.window.appearance = NSAppearance(named: appearance)
            try await capture(view.window, at: directory.appendingPathComponent(name + ".png"))
        }
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
        precondition(loaded.samples.allSatisfy { $0.streams == 0 } && loaded.groups.first?.lastSeen == nil)
        view.window.appearance = NSAppearance(named: .darkAqua)
        try await capture(view.window, at: directory.appendingPathComponent("overview-legacy.png"))
        view.window.setContentSize(view.window.contentMinSize)
        try await capture(view.window, at: directory.appendingPathComponent("overview-compact.png"))
        print("Dashboard notification destinations, window reuse, compact layout and previews passed")
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
