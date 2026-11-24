import AppKit
import UserNotifications

private let bundleID = "com.lonelygo.mi-guard.notify"

private func writeOutput(_ message: String) {
    FileHandle.standardOutput.write(Data("\(message)\n".utf8))
}

private func writeError(_ message: String) {
    FileHandle.standardError.write(Data("mi-guard-notify: \(message)\n".utf8))
}

@MainActor
private final class GuardApplication: NSObject, NSApplicationDelegate, UNUserNotificationCenterDelegate {
    let mode: HelperMode
    private var dashboard: DashboardWindow?
    private var refreshTimer: Timer?

    init(mode: HelperMode) { self.mode = mode }

    func applicationDidFinishLaunching(_ notification: Notification) {
        switch mode {
        case .dashboard(let path):
            // A cold-start notification response may already have opened its destination.
            if dashboard == nil { present(config: path.map { URL(fileURLWithPath: $0) }) }
            Task {
                // Opening history always works, including when delivery is turned off.
                let center = UNUserNotificationCenter.current()
                if await center.notificationSettings().authorizationStatus == .notDetermined {
                    do { _ = try await center.requestAuthorization(options: [.alert, .sound]) }
                    catch { writeError(error.localizedDescription) }
                }
            }
        case .check, .send, .authorize:
            if mode != .authorize {
                DispatchQueue.main.asyncAfter(deadline: .now() + .seconds(8)) { [weak self] in
                    guard let self, self.dashboard == nil else { return }
                    writeError("Notification Center timed out.")
                    exit(EXIT_FAILURE)
                }
            }
            Task {
                do {
                    try await runHelperCommand()
                    if dashboard == nil { exit(EXIT_SUCCESS) }
                } catch {
                    writeError(error.localizedDescription)
                    if dashboard == nil { exit(EXIT_FAILURE) }
                }
            }
        }
    }

    private func present(config: URL? = nil, destination: DashboardDestination? = nil) {
        if dashboard == nil {
            NSApp.setActivationPolicy(.regular)
            let window = DashboardWindow(controller: DashboardController())
            dashboard = window
            window.installMenus()
            refreshTimer = Timer.scheduledTimer(timeInterval: 60, target: self, selector: #selector(refreshVisibleWindow), userInfo: nil, repeats: true)
        }
        if let config { dashboard?.controller.useConfig(config, remember: false) }
        dashboard?.present(destination)
    }

    @objc private func refreshVisibleWindow() {
        if NSApp.isActive, let dashboard, dashboard.window.isVisible, !dashboard.window.isMiniaturized {
            dashboard.controller.refresh()
        }
    }

    func applicationDidBecomeActive(_ notification: Notification) { refreshVisibleWindow() }

    func applicationShouldHandleReopen(_ sender: NSApplication, hasVisibleWindows flag: Bool) -> Bool {
        present()
        return false
    }

    func applicationShouldTerminateAfterLastWindowClosed(_ sender: NSApplication) -> Bool { dashboard != nil }

    func applicationWillTerminate(_ notification: Notification) {
        refreshTimer?.invalidate()
        dashboard?.controller.stop()
    }

    nonisolated func userNotificationCenter(_ center: UNUserNotificationCenter, didReceive response: UNNotificationResponse,
                                             withCompletionHandler completionHandler: @escaping () -> Void) {
        let content = response.notification.request.content
        guard let route = DashboardDestination(action: response.actionIdentifier, thread: content.threadIdentifier, userInfo: content.userInfo) else {
            completionHandler()
            return
        }
        Task { @MainActor in
            self.present(destination: route)
            completionHandler()
        }
    }

    nonisolated func userNotificationCenter(_ center: UNUserNotificationCenter, willPresent notification: UNNotification,
                                             withCompletionHandler completionHandler: @escaping (UNNotificationPresentationOptions) -> Void) {
        completionHandler([.banner, .list, .sound])
    }

    private func runHelperCommand() async throws {
        let center = UNUserNotificationCenter.current()
        func settings() async -> UNAuthorizationStatus { await center.notificationSettings().authorizationStatus }
        switch mode {
        case .check:
            let status = await settings()
            writeOutput("{\"bundle_id\":\"\(bundleID)\",\"user_notifications\":true,\"authorization\":\"\(status.statusName)\"}")
        case .authorize:
            let result = try await authorizeIfNeeded(settings: settings) {
                writeOutput("Waiting for your choice in the macOS notification prompt for mi-guard.")
                return try await center.requestAuthorization(options: [.alert, .sound])
            }
            switch result {
            case .granted: writeOutput("Notification permission granted.")
            case .alreadyAllowed(let status): writeOutput("Notification permission: \(status.statusName).")
            }
        case .send(let kind, let title, let body):
            let status = await settings()
            guard status.allowsDelivery else {
                if status == .denied { throw HelperError(deniedMessage) }
                throw HelperError("Notification permission: \(status.statusName). Run mihomo-guard notify-setup.")
            }
            let content = UNMutableNotificationContent()
            content.title = title
            content.body = body
            content.threadIdentifier = "mi-guard.\(kind)"
            if let path = ProcessInfo.processInfo.environment["MI_GUARD_NOTIFICATION_CONFIG"], path.hasPrefix("/") {
                content.userInfo = ["guardConfigPath": path]
            }
            content.sound = .default
            try await center.add(UNNotificationRequest(identifier: UUID().uuidString, content: content, trigger: nil))
        case .dashboard:
            break
        }
    }
}

@main
@MainActor
private struct NotificationHelper {
    static func main() {
        do {
            let mode = try HelperMode(arguments: Array(CommandLine.arguments.dropFirst()))
            guard Bundle.main.bundleIdentifier == bundleID else {
                throw HelperError("unexpected bundle identifier: \(Bundle.main.bundleIdentifier ?? "none")")
            }
            let app = NSApplication.shared
            let delegate = GuardApplication(mode: mode)
            app.delegate = delegate
            // Notification Center can deliver a response during launch. Its delegate is weak.
            UNUserNotificationCenter.current().delegate = delegate
            if case .dashboard = mode { app.setActivationPolicy(.regular) }
            else { app.setActivationPolicy(.accessory) }
            withExtendedLifetime(delegate) { app.run() }
        } catch {
            writeError(error.localizedDescription)
            exit(EXIT_FAILURE)
        }
    }
}
