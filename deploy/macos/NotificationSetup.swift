import Foundation
import UserNotifications

struct HelperError: LocalizedError {
    let message: String
    init(_ message: String) { self.message = message }
    var errorDescription: String? { message }
}

enum HelperMode: Equatable {
    case check
    case dashboard(config: String?)
    case authorize
    case send(kind: String, title: String, body: String, eventID: String?)

    init(arguments raw: [String]) throws {
        // LaunchServices can prepend a legacy process serial number to the arguments.
        let arguments = raw.first?.hasPrefix("-psn_") == true ? Array(raw.dropFirst()) : raw
        if arguments.isEmpty || arguments == ["--dashboard"] {
            self = .dashboard(config: nil)
        } else if arguments.count == 3 && arguments[0] == "--dashboard" && ["-c", "--config"].contains(arguments[1]) && !arguments[2].isEmpty {
            self = .dashboard(config: arguments[2])
        } else if arguments == ["--authorize"] {
            self = .authorize
        } else if arguments == ["--check"] {
            self = .check
        } else if arguments.first == "--send" {
            func value(_ key: String) throws -> String {
                guard let i = arguments.firstIndex(of: key), i + 1 < arguments.count else {
                    throw HelperError("missing \(key)")
                }
                return arguments[i + 1]
            }
            let eventID: String?
            if let index = arguments.firstIndex(of: "--event-id"), index + 1 < arguments.count, !arguments[index + 1].isEmpty {
                eventID = arguments[index + 1]
            } else {
                eventID = nil
            }
            self = try .send(kind: value("--kind"), title: value("--title"), body: value("--body"), eventID: eventID)
        } else {
            throw HelperError("usage: mi-guard-notify [--dashboard [-c guard.yaml]] | --check | --authorize | --send --kind KIND --title TITLE --body BODY [--event-id ID]")
        }
    }
}

enum DashboardSection: String, CaseIterable {
    case overview, nodes, selections, flights
}

struct DashboardDestination: Equatable {
    let section: DashboardSection
    let configURL: URL?
    let decisionID: String?

    init?(action: String, thread: String, userInfo: [AnyHashable: Any]) {
        guard action == UNNotificationDefaultActionIdentifier else { return nil }
        section = thread == "mi-guard.switch" ? .selections : .overview
        decisionID = (userInfo["decisionID"] as? String).flatMap { $0.isEmpty ? nil : $0 }
        if let path = userInfo["guardConfigPath"] as? String, path.hasPrefix("/") {
            configURL = URL(fileURLWithPath: path).standardizedFileURL
        } else {
            configURL = nil
        }
    }
}

extension UNAuthorizationStatus {
    var statusName: String {
        switch self {
        case .notDetermined: return "notDetermined"
        case .denied: return "denied"
        case .authorized: return "authorized"
        case .provisional: return "provisional"
        case .ephemeral: return "ephemeral"
        @unknown default: return "unknown(\(rawValue))"
        }
    }

    var allowsDelivery: Bool {
        switch self {
        case .authorized, .provisional, .ephemeral: return true
        default: return false
        }
    }
}

let deniedMessage = "Notification permission is denied. Enable mi-guard in System Settings > Notifications."

enum AuthorizationResult: Equatable {
    case granted
    case alreadyAllowed(UNAuthorizationStatus)
}

@MainActor
func authorizeIfNeeded(
    settings: () async -> UNAuthorizationStatus,
    request: () async throws -> Bool
) async throws -> AuthorizationResult {
    let status = await settings()
    if status.allowsDelivery { return .alreadyAllowed(status) }
    switch status {
    case .notDetermined:
        do {
            guard try await request() else { throw HelperError("macOS did not grant notification permission.") }
            return .granted
        } catch {
            // The effective setting can change while the system prompt is outstanding.
            let effective = await settings()
            if effective.allowsDelivery { return .granted }
            if effective == .denied { throw HelperError(deniedMessage) }
            throw error
        }
    case .denied:
        throw HelperError(deniedMessage)
    default:
        throw HelperError("Unsupported notification authorization status: \(status.statusName)")
    }
}
