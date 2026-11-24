import Foundation
import UserNotifications

struct HelperError: LocalizedError {
    let message: String
    init(_ message: String) { self.message = message }
    var errorDescription: String? { message }
}

enum HelperMode: Equatable {
    case check
    case authorize(finder: Bool)
    case send(kind: String, title: String, body: String)

    init(arguments raw: [String]) throws {
        // LaunchServices can prepend a legacy process serial number to the arguments.
        let arguments = raw.first?.hasPrefix("-psn_") == true ? Array(raw.dropFirst()) : raw
        if arguments.isEmpty {
            self = .authorize(finder: true)
        } else if arguments == ["--authorize"] {
            self = .authorize(finder: false)
        } else if arguments == ["--check"] {
            self = .check
        } else if arguments.first == "--send" {
            func value(_ key: String) throws -> String {
                guard let i = arguments.firstIndex(of: key), i + 1 < arguments.count else {
                    throw HelperError("missing \(key)")
                }
                return arguments[i + 1]
            }
            self = try .send(kind: value("--kind"), title: value("--title"), body: value("--body"))
        } else {
            throw HelperError("usage: mi-guard-notify --check | --authorize | --send --kind KIND --title TITLE --body BODY")
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
