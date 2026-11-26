import AppKit
import SwiftUI
import UniformTypeIdentifiers

@MainActor
final class DashboardController: ObservableObject {
    @Published var section: DashboardSection = .overview
    @Published private(set) var snapshot: DashboardSnapshot?
    @Published private(set) var problem: String?
    @Published private(set) var loading = false
    @Published private(set) var configURL: URL?
    @Published private(set) var requestedDecisionID: String?
    @Published private(set) var notificationRecords: [NotificationRecord] = []
    @Published private(set) var selectedNotificationID: String?
    @Published private(set) var notificationProblem: String?

    let readerURL: URL
    private let preferences: UserDefaults
    private let notificationInbox: NotificationInboxStore
    private var transientNotifications: [NotificationRecord] = []
    private let configKey = "dashboard.guardConfigPath"
    private var process: Process?

    init(reader: URL = Bundle.main.bundleURL.appendingPathComponent("Contents/Helpers/mihomo-guard"),
         preferences: UserDefaults = .standard,
         notificationInbox: NotificationInboxStore = NotificationInboxStore()) {
        self.readerURL = reader
        self.preferences = preferences
        self.notificationInbox = notificationInbox
        if let path = preferences.string(forKey: configKey), path.hasPrefix("/") {
            configURL = URL(fileURLWithPath: path)
        }
        refreshNotifications()
    }

    func chooseConfig() {
        let panel = NSOpenPanel()
        panel.title = "选择 mihomo-guard 配置"
        panel.prompt = "打开"
        panel.allowedContentTypes = [UTType(filenameExtension: "yaml"), UTType(filenameExtension: "yml")].compactMap { $0 }
        panel.directoryURL = configURL?.deletingLastPathComponent()
        guard panel.runModal() == .OK, let url = panel.url else { return }
        useConfig(url, remember: true)
    }

    func useConfig(_ url: URL?, remember: Bool) {
        let source = url?.standardizedFileURL
        if remember {
            preferences.set(source?.path, forKey: configKey)
        }
        if source != configURL {
            stop()
            snapshot = nil
            problem = nil
            configURL = source
            requestedDecisionID = nil
        }
        refresh()
    }

    func refresh() {
        guard !loading else { return }
        let task = Process()
        let pipe = Pipe()
        task.executableURL = readerURL
        task.arguments = ["history"] + (configURL.map { ["-c", $0.path] } ?? []) + (requestedDecisionID.map { ["--decision", $0] } ?? [])
        // The CLI writes one JSON document on success, or a diagnostic on failure.
        // Drain a single pipe off the main thread, including for a large ledger.
        task.standardOutput = pipe
        task.standardError = pipe
        do {
            try task.run()
        } catch {
            problem = "无法启动 App 内的历史读取工具。\(error.localizedDescription)"
            return
        }
        process = task
        loading = true
        Task { [weak self] in
            let result = await Task.detached(priority: .utility) { () -> Result<DashboardSnapshot, Error> in
                Result {
                    let bytes = try pipe.fileHandleForReading.readToEnd() ?? Data()
                    task.waitUntilExit()
                    guard task.terminationStatus == 0 else {
                        let detail = String(decoding: bytes.prefix(2048), as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines)
                        throw HelperError(detail.isEmpty ? "历史读取工具意外退出。" : detail)
                    }
                    return try DashboardSnapshot.decode(bytes)
                }
            }.value
            guard let self, self.process === task else { return }
            self.process = nil
            self.loading = false
            switch result {
            case .success(let value):
                self.snapshot = value
                self.problem = nil
            case .failure(let error):
                self.problem = error.localizedDescription
            }
        }
    }

    func stop() {
        if let process, process.isRunning { process.terminate() }
        process = nil
        loading = false
    }

    func focusDecision(_ id: String) {
        section = .selections
        guard !id.isEmpty, requestedDecisionID != id else { return }
        stop()
        requestedDecisionID = id
        refresh()
    }

    func clearDecisionFocus() {
        guard requestedDecisionID != nil else { return }
        stop()
        requestedDecisionID = nil
        refresh()
    }

    func refreshNotifications() {
        do {
            notificationRecords = mergedNotifications(try notificationInbox.records())
            notificationProblem = nil
        } catch {
            notificationRecords = mergedNotifications([])
            notificationProblem = error.localizedDescription
        }
    }

    func observeNotification(_ record: NotificationRecord, select: Bool = false) {
        transientNotifications.removeAll { $0.id == record.id }
        transientNotifications.insert(record, at: 0)
        if transientNotifications.count > NotificationInboxStore.retentionLimit {
            transientNotifications.removeLast(transientNotifications.count - NotificationInboxStore.retentionLimit)
        }
        if select { selectedNotificationID = record.id }
        refreshNotifications()
        if select { section = .notifications }
    }

    func focusNotification(_ id: String) {
        refreshNotifications()
        selectedNotificationID = notificationRecords.contains { $0.id == id } ? id : nil
        section = .notifications
    }

    func focusNotification(_ record: NotificationRecord) {
        observeNotification(record, select: true)
    }

    func toggleNotification(_ id: String) {
        selectedNotificationID = selectedNotificationID == id ? nil : id
    }

    func openNotificationDestination(_ record: NotificationRecord) {
        if let path = record.configPath {
            useConfig(URL(fileURLWithPath: path), remember: false)
        }
        if let decisionID = record.decisionID {
            focusDecision(decisionID)
        } else {
            clearDecisionFocus()
            section = .overview
        }
    }

    private func mergedNotifications(_ persisted: [NotificationRecord]) -> [NotificationRecord] {
        var byID = Dictionary(persisted.map { ($0.id, $0) }, uniquingKeysWith: { first, _ in first })
        for record in transientNotifications where byID[record.id] == nil {
            byID[record.id] = record
        }
        let ordered = byID.values.sorted(by: NotificationRecord.newestFirst)
        guard ordered.count > NotificationInboxStore.retentionLimit else { return ordered }
        let newest = Array(ordered.prefix(NotificationInboxStore.retentionLimit))
        guard let selectedNotificationID,
              !newest.contains(where: { $0.id == selectedNotificationID }),
              let selected = ordered.first(where: { $0.id == selectedNotificationID }) else {
            return newest
        }
        return Array(newest.dropLast()) + [selected]
    }
}

@MainActor
final class DashboardWindow: NSObject {
    let controller: DashboardController
    let window: NSWindow
    private(set) var settingsWindow: NSWindow?

    init(controller: DashboardController, restoreFrame: Bool = true) {
        self.controller = controller
        window = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 1100, height: 730),
                          styleMask: [.titled, .closable, .miniaturizable, .resizable],
                          backing: .buffered, defer: false)
        super.init()
        window.title = "mi-guard"
        window.isReleasedWhenClosed = false
        window.contentViewController = NSHostingController(rootView: DashboardView(controller: controller) { [weak self] in
            self?.showSettings(nil)
        })
        window.setContentSize(NSSize(width: 1100, height: 730))
        window.contentMinSize = NSSize(width: 850, height: 565)
        window.center()
        if restoreFrame { window.setFrameAutosaveName("mi-guard-history") }
    }

    func present(_ destination: DashboardDestination? = nil, notification: NotificationRecord? = nil) {
        let refreshHistory: Bool
        if let destination {
            if let id = destination.notificationID {
                if let notification { controller.focusNotification(notification) }
                else { controller.focusNotification(id) }
                refreshHistory = false
            } else {
                if let url = destination.configURL { controller.useConfig(url, remember: false) }
                controller.section = destination.section
                if let id = destination.decisionID { controller.focusDecision(id) }
                else { controller.clearDecisionFocus() }
                refreshHistory = true
            }
        } else {
            refreshHistory = true
        }
        if refreshHistory { controller.refresh() }
        if window.isMiniaturized { window.deminiaturize(nil) }
        window.makeKeyAndOrderFront(nil)
        NSApp.activate(ignoringOtherApps: true)
    }

    @objc func showSettings(_ sender: Any?) {
        if settingsWindow == nil {
            let settings = NSWindow(contentViewController: NSHostingController(rootView: DashboardSettingsView(controller: controller)))
            settings.title = "mi-guard 设置"
            settings.styleMask = [.titled, .closable]
            settings.isReleasedWhenClosed = false
            settings.center()
            settingsWindow = settings
        }
        settingsWindow?.makeKeyAndOrderFront(nil)
    }

    @objc func openConfig(_ sender: Any?) { controller.chooseConfig() }
    @objc func defaultConfig(_ sender: Any?) { controller.useConfig(nil, remember: true) }
    @objc func refresh(_ sender: Any?) {
        if controller.section == .notifications { controller.refreshNotifications() }
        else { controller.refresh() }
    }

    func installMenus() {
        let menu = NSMenu()
        func submenu(_ title: String) -> NSMenu {
            let item = NSMenuItem()
            let child = NSMenu(title: title)
            item.submenu = child
            menu.addItem(item)
            return child
        }
        let app = submenu("mi-guard")
        app.addItem(withTitle: "关于 mi-guard", action: #selector(NSApplication.orderFrontStandardAboutPanel(_:)), keyEquivalent: "")
        app.addItem(.separator())
        app.addItem(withTitle: "设置…", action: #selector(showSettings(_:)), keyEquivalent: ",").target = self
        app.addItem(.separator())
        app.addItem(withTitle: "隐藏 mi-guard", action: #selector(NSApplication.hide(_:)), keyEquivalent: "h")
        app.addItem(withTitle: "退出 mi-guard", action: #selector(NSApplication.terminate(_:)), keyEquivalent: "q")
        let file = submenu("文件")
        file.addItem(withTitle: "打开配置…", action: #selector(openConfig(_:)), keyEquivalent: "o").target = self
        file.addItem(withTitle: "使用默认配置", action: #selector(defaultConfig(_:)), keyEquivalent: "").target = self
        file.addItem(withTitle: "刷新历史", action: #selector(refresh(_:)), keyEquivalent: "r").target = self
        file.addItem(.separator())
        file.addItem(withTitle: "关闭窗口", action: #selector(NSWindow.performClose(_:)), keyEquivalent: "w")
        let edit = submenu("编辑")
        edit.addItem(withTitle: "剪切", action: #selector(NSText.cut(_:)), keyEquivalent: "x")
        edit.addItem(withTitle: "复制", action: #selector(NSText.copy(_:)), keyEquivalent: "c")
        edit.addItem(withTitle: "粘贴", action: #selector(NSText.paste(_:)), keyEquivalent: "v")
        edit.addItem(withTitle: "全选", action: #selector(NSText.selectAll(_:)), keyEquivalent: "a")
        let windows = submenu("窗口")
        windows.addItem(withTitle: "最小化", action: #selector(NSWindow.performMiniaturize(_:)), keyEquivalent: "m")
        windows.addItem(withTitle: "缩放", action: #selector(NSWindow.performZoom(_:)), keyEquivalent: "")
        NSApp.windowsMenu = windows
        NSApp.mainMenu = menu
    }
}
