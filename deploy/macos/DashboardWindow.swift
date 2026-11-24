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

    private let reader: URL
    private let preferences: UserDefaults
    private let configKey = "dashboard.guardConfigPath"
    private var process: Process?

    init(reader: URL = Bundle.main.bundleURL.appendingPathComponent("Contents/Helpers/mihomo-guard"),
         preferences: UserDefaults = .standard) {
        self.reader = reader
        self.preferences = preferences
        if let path = preferences.string(forKey: configKey), path.hasPrefix("/") {
            configURL = URL(fileURLWithPath: path)
        }
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
        }
        refresh()
    }

    func refresh() {
        guard !loading else { return }
        let task = Process()
        let pipe = Pipe()
        task.executableURL = reader
        task.arguments = ["history"] + (configURL.map { ["-c", $0.path] } ?? [])
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
}

@MainActor
final class DashboardWindow: NSObject {
    let controller: DashboardController
    let window: NSWindow

    init(controller: DashboardController, restoreFrame: Bool = true) {
        self.controller = controller
        window = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 1100, height: 730),
                          styleMask: [.titled, .closable, .miniaturizable, .resizable],
                          backing: .buffered, defer: false)
        super.init()
        window.title = "mi-guard"
        window.isReleasedWhenClosed = false
        window.contentViewController = NSHostingController(rootView: DashboardView(controller: controller))
        window.setContentSize(NSSize(width: 1100, height: 730))
        window.contentMinSize = NSSize(width: 850, height: 565)
        window.center()
        if restoreFrame { window.setFrameAutosaveName("mi-guard-history") }
    }

    func present(_ destination: DashboardDestination? = nil) {
        if let destination {
            controller.section = destination.section
            if let url = destination.configURL { controller.useConfig(url, remember: false) }
        }
        controller.refresh()
        if window.isMiniaturized { window.deminiaturize(nil) }
        window.makeKeyAndOrderFront(nil)
        NSApp.activate(ignoringOtherApps: true)
    }

    @objc func openConfig(_ sender: Any?) { controller.chooseConfig() }
    @objc func defaultConfig(_ sender: Any?) { controller.useConfig(nil, remember: true) }
    @objc func refresh(_ sender: Any?) { controller.refresh() }

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
