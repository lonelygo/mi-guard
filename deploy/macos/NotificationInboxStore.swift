import Darwin
import Foundation
import UserNotifications

struct NotificationRecord: Codable, Hashable, Identifiable, Sendable {
    let id: String
    let kind: String
    let title: String
    let body: String
    let deliveredAt: Date
    let configPath: String?
    let decisionID: String?

    static func newestFirst(_ lhs: Self, _ rhs: Self) -> Bool {
        lhs.deliveredAt == rhs.deliveredAt ? lhs.id > rhs.id : lhs.deliveredAt > rhs.deliveredAt
    }

    init(id: String = UUID().uuidString, kind: String, title: String, body: String,
         deliveredAt: Date = Date(), configPath: String? = nil, decisionID: String? = nil) {
        self.id = id
        self.kind = kind
        self.title = title
        self.body = body
        self.deliveredAt = deliveredAt
        self.configPath = configPath.flatMap { $0.hasPrefix("/") ? $0 : nil }
        self.decisionID = decisionID.flatMap { $0.isEmpty ? nil : $0 }
    }

    init(requestIdentifier: String, content: UNNotificationContent, deliveredAt: Date) {
        let prefix = "mi-guard."
        let kind = content.threadIdentifier.hasPrefix(prefix)
            ? String(content.threadIdentifier.dropFirst(prefix.count)) : content.threadIdentifier
        self.init(
            id: (content.userInfo["notificationID"] as? String).flatMap { $0.isEmpty ? nil : $0 }
                ?? requestIdentifier,
            kind: kind,
            title: content.title,
            body: content.body,
            deliveredAt: deliveredAt,
            configPath: content.userInfo["guardConfigPath"] as? String,
            decisionID: content.userInfo["decisionID"] as? String
        )
    }
}

struct NotificationInboxStore: Sendable {
    private struct Document: Codable, Equatable {
        let version: Int
        var records: [NotificationRecord]
    }

    static let retentionLimit = 200
    let fileURL: URL

    init(fileURL: URL = NotificationInboxStore.defaultFileURL()) {
        self.fileURL = fileURL.standardizedFileURL
    }

    static func defaultFileURL(fileManager: FileManager = .default) -> URL {
        let base = fileManager.urls(for: .applicationSupportDirectory, in: .userDomainMask).first
            ?? fileManager.homeDirectoryForCurrentUser.appendingPathComponent("Library/Application Support", isDirectory: true)
        return base.appendingPathComponent("mi-guard", isDirectory: true)
            .appendingPathComponent("notifications.json")
    }

    func records() throws -> [NotificationRecord] {
        try withLock { $0.records }
    }

    func upsert(_ record: NotificationRecord) throws {
        try upsert([record])
    }

    func upsert(_ records: [NotificationRecord]) throws {
        guard !records.isEmpty else { return }
        try withLock { document in
            var byID = [String: NotificationRecord]()
            for record in document.records + records {
                byID[record.id] = record
            }
            document.records = Array(byID.values.sorted(by: NotificationRecord.newestFirst).prefix(Self.retentionLimit))
        }
    }

    func deliverAndPersist(
        _ record: NotificationRecord,
        content: UNNotificationContent,
        deliver: (UNNotificationRequest) async throws -> Void
    ) async throws -> Error? {
        try await deliver(UNNotificationRequest(identifier: record.id, content: content, trigger: nil))
        do {
            try upsert(record)
            return nil
        } catch {
            return error
        }
    }

    private func withLock<Result>(_ operation: (inout Document) throws -> Result) throws -> Result {
        let fileManager = FileManager.default
        let directory = fileURL.deletingLastPathComponent()
        try fileManager.createDirectory(at: directory, withIntermediateDirectories: true,
                                        attributes: [.posixPermissions: 0o700])
        try? fileManager.setAttributes([.posixPermissions: 0o700], ofItemAtPath: directory.path)
        let lockURL = fileURL.appendingPathExtension("lock")
        let descriptor = open(lockURL.path, O_CREAT | O_RDWR, S_IRUSR | S_IWUSR)
        guard descriptor >= 0 else {
            throw HelperError("无法锁定通知记录：\(String(cString: strerror(errno)))")
        }
        defer { close(descriptor) }
        guard flock(descriptor, LOCK_EX) == 0 else {
            throw HelperError("无法锁定通知记录：\(String(cString: strerror(errno)))")
        }
        defer { flock(descriptor, LOCK_UN) }

        var document = try readDocument()
        let original = document
        let result = try operation(&document)
        if document != original {
            let encoder = JSONEncoder()
            encoder.dateEncodingStrategy = .millisecondsSince1970
            encoder.outputFormatting = [.sortedKeys]
            try encoder.encode(document).write(to: fileURL, options: .atomic)
            try? fileManager.setAttributes([.posixPermissions: 0o600], ofItemAtPath: fileURL.path)
        }
        return result
    }

    private func readDocument() throws -> Document {
        guard FileManager.default.fileExists(atPath: fileURL.path) else {
            return Document(version: 1, records: [])
        }
        let data = try Data(contentsOf: fileURL)
        guard !data.isEmpty else { return Document(version: 1, records: []) }
        let decoder = JSONDecoder()
        decoder.dateDecodingStrategy = .millisecondsSince1970
        let document = try decoder.decode(Document.self, from: data)
        guard document.version == 1 else {
            throw HelperError("不支持的通知记录版本：\(document.version)")
        }
        return document
    }
}
