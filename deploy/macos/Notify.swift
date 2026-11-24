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
private func showStatus(_ message: String) {
    NSApplication.shared.activate(ignoringOtherApps: true)
    let alert = NSAlert()
    alert.messageText = "mi-guard notifications"
    alert.informativeText = message
    alert.runModal()
}

@main
@MainActor
private struct NotificationHelper {
    static func main() {
        let mode: HelperMode
        do {
            mode = try HelperMode(arguments: Array(CommandLine.arguments.dropFirst()))
        } catch {
            writeError(error.localizedDescription)
            exit(EXIT_FAILURE)
        }

        let app = NSApplication.shared
        app.setActivationPolicy(.accessory)
        switch mode {
        case .authorize:
            break // The user may leave the permission prompt open.
        case .check, .send:
            DispatchQueue.main.asyncAfter(deadline: .now() + .seconds(8)) {
                writeError("Notification Center timed out.")
                exit(EXIT_FAILURE)
            }
        }

        // Keep AppKit processing events while UserNotifications awaits the user's choice.
        Task { @MainActor in
            let feedback: String?
            do {
                guard Bundle.main.bundleIdentifier == bundleID else {
                    throw HelperError("unexpected bundle identifier: \(Bundle.main.bundleIdentifier ?? "none")")
                }
                feedback = try await perform(mode, center: .current())
            } catch {
                writeError(error.localizedDescription)
                if mode == .authorize(finder: true) { showStatus(error.localizedDescription) }
                exit(EXIT_FAILURE)
            }
            if let feedback { showStatus(feedback) }
            exit(EXIT_SUCCESS)
        }
        app.run()
        writeError("application event loop stopped unexpectedly")
        exit(EXIT_FAILURE)
    }

    // Return feedback only for a Finder launch which did not show a new permission prompt.
    private static func perform(_ mode: HelperMode, center: UNUserNotificationCenter) async throws -> String? {
        func settings() async -> UNAuthorizationStatus {
            await center.notificationSettings().authorizationStatus
        }
        switch mode {
        case .check:
            let status = await settings()
            writeOutput("{\"bundle_id\":\"\(bundleID)\",\"user_notifications\":true,\"authorization\":\"\(status.statusName)\"}")
        case .authorize(let finder):
            let result = try await authorizeIfNeeded(settings: settings) {
                if !finder { writeOutput("Waiting for your choice in the macOS notification prompt for mi-guard.") }
                return try await center.requestAuthorization(options: [.alert, .sound])
            }
            switch result {
            case .granted:
                writeOutput("Notification permission granted.")
            case .alreadyAllowed(let status):
                let message = "Notification permission: \(status.statusName)."
                writeOutput(message)
                if finder { return message }
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
            content.sound = .default
            try await center.add(UNNotificationRequest(identifier: UUID().uuidString, content: content, trigger: nil))
        }
        return nil
    }
}
