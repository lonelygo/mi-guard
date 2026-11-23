import AppKit
import Foundation
import UserNotifications

private let expectedBundleIdentifier = "com.lonelygo.mi-guard.notify"
private let center = UNUserNotificationCenter.current()
private let callbackTimeout: TimeInterval = 8

private enum HelperError: Error, CustomStringConvertible {
    case usage(String)
    case bundle(String)
    case authorization(String)
    case delivery(String)

    var description: String {
        switch self {
        case .usage(let message), .bundle(let message), .authorization(let message), .delivery(let message):
            return message
        }
    }
}

private func argumentValue(_ name: String, in arguments: [String]) throws -> String {
    guard let index = arguments.firstIndex(of: name), index + 1 < arguments.count else {
        throw HelperError.usage("missing \(name)")
    }
    return arguments[index + 1]
}

private func waitForCallback(timeout: DispatchTime = .now() + callbackTimeout, _ start: (@escaping (Error?) -> Void) -> Void) throws {
    let semaphore = DispatchSemaphore(value: 0)
    var callbackError: Error?
    start { error in
        callbackError = error
        semaphore.signal()
    }
    if semaphore.wait(timeout: timeout) == .timedOut {
        throw HelperError.delivery("notification center timed out")
    }
    if let callbackError {
        throw callbackError
    }
}

private func checkBundle() throws {
    guard Bundle.main.bundleIdentifier == expectedBundleIdentifier else {
        throw HelperError.bundle("unexpected bundle identifier: \(Bundle.main.bundleIdentifier ?? "none")")
    }
    var authorization = "unknown"
    try waitForCallback { complete in
        center.getNotificationSettings { settings in
            switch settings.authorizationStatus {
            case .notDetermined: authorization = "notDetermined"
            case .denied: authorization = "denied"
            case .authorized: authorization = "authorized"
            case .provisional: authorization = "provisional"
            default: break
            }
            complete(nil)
        }
    }
    print("{\"bundle_id\":\"\(expectedBundleIdentifier)\",\"user_notifications\":true,\"authorization\":\"\(authorization)\"}")
}

private func authorize() throws {
    try waitForCallback(timeout: .distantFuture) { complete in
        center.requestAuthorization(options: [.alert, .sound]) { granted, error in
            if let error {
                complete(error)
            } else if !granted {
                complete(HelperError.authorization("notification permission was not granted"))
            } else {
                complete(nil)
            }
        }
    }
}

private func send(arguments: [String]) throws {
    let kind = try argumentValue("--kind", in: arguments)
    let title = try argumentValue("--title", in: arguments)
    let body = try argumentValue("--body", in: arguments)

    let settingsSemaphore = DispatchSemaphore(value: 0)
    var allowed = false
    center.getNotificationSettings { settings in
        switch settings.authorizationStatus {
        case .authorized, .provisional, .ephemeral:
            allowed = true
        default:
            allowed = false
        }
        settingsSemaphore.signal()
    }
    if settingsSemaphore.wait(timeout: .now() + callbackTimeout) == .timedOut {
        throw HelperError.delivery("notification settings timed out")
    }
    guard allowed else {
        throw HelperError.authorization("notification permission is unavailable; run notify-setup")
    }

    let content = UNMutableNotificationContent()
    content.title = title
    content.body = body
    content.threadIdentifier = "mi-guard.\(kind)"
    content.sound = .default
    let request = UNNotificationRequest(identifier: UUID().uuidString, content: content, trigger: nil)
    try waitForCallback { complete in
        center.add(request, withCompletionHandler: complete)
    }
}

NSApplication.shared.setActivationPolicy(.accessory)

do {
    let arguments = Array(CommandLine.arguments.dropFirst())
    if arguments == ["--check"] {
        try checkBundle()
    } else if arguments == ["--authorize"] {
        try authorize()
    } else if arguments.first == "--send" {
        try send(arguments: arguments)
    } else {
        throw HelperError.usage("usage: mi-guard-notify --check | --authorize | --send --kind KIND --title TITLE --body BODY")
    }
} catch {
    FileHandle.standardError.write(Data("mi-guard-notify: \(error)\n".utf8))
    exit(EXIT_FAILURE)
}
