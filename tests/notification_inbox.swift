import Foundation
import UserNotifications

@main
private struct NotificationInboxTests {
    static func main() async throws {
        let directory = FileManager.default.temporaryDirectory
            .appendingPathComponent("mi-guard-notification-inbox-\(UUID().uuidString)", isDirectory: true)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }

        let file = directory.appendingPathComponent("notifications.json")
        let store = NotificationInboxStore(fileURL: file)
        let content = UNMutableNotificationContent()
        content.title = "WARP-US 已切换"
        content.body = "链路故障恢复：A → B"
        content.threadIdentifier = "mi-guard.switch"
        content.userInfo = ["guardConfigPath": "/tmp/example.yaml", "decisionID": "decision-1"]
        let recovered = NotificationRecord(requestIdentifier: "legacy-request", content: content,
                                           deliveredAt: Date(timeIntervalSince1970: 100))
        precondition(recovered.id == "legacy-request" && recovered.kind == "switch")
        precondition(recovered.configPath == "/tmp/example.yaml" && recovered.decisionID == "decision-1")
        try store.upsert(recovered)
        try store.upsert(recovered)
        let recoveredRecords = try store.records()
        precondition(recoveredRecords == [recovered])

        try await withThrowingTaskGroup(of: Void.self) { group in
            for index in 0..<40 {
                group.addTask {
                    try store.upsert(NotificationRecord(
                        id: "parallel-\(index)", kind: "summary", title: "扫描完成", body: "\(index)",
                        deliveredAt: Date(timeIntervalSince1970: TimeInterval(200 + index))
                    ))
                }
            }
            try await group.waitForAll()
        }
        let concurrentRecords = try store.records()
        precondition(concurrentRecords.count == 41)

        for index in 0..<(NotificationInboxStore.retentionLimit + 9) {
            try store.upsert(NotificationRecord(
                id: "retained-\(index)", kind: "summary", title: "扫描完成", body: "\(index)",
                deliveredAt: Date(timeIntervalSince1970: TimeInterval(1_000 + index))
            ))
        }
        let records = try store.records()
        precondition(records.count == NotificationInboxStore.retentionLimit)
        precondition(records.first?.id == "retained-208")
        precondition(records.last?.id == "retained-9")
        precondition(!records.contains { $0.id == "legacy-request" })

        try store.upsert([records[0], records[0]])
        let reimported = try store.records()
        precondition(reimported == records)
        let attributes = try FileManager.default.attributesOfItem(atPath: file.path)
        let permissions = (attributes[.posixPermissions] as? NSNumber)?.intValue
        precondition(permissions == 0o600)
        let directoryAttributes = try FileManager.default.attributesOfItem(atPath: directory.path)
        let directoryPermissions = (directoryAttributes[.posixPermissions] as? NSNumber)?.intValue
        precondition(directoryPermissions == 0o700)

        content.userInfo = ["notificationID": "durable-id", "guardConfigPath": "relative.yaml"]
        let identified = NotificationRecord(requestIdentifier: "request-id", content: content, deliveredAt: Date())
        precondition(identified.id == "durable-id" && identified.configPath == nil && identified.decisionID == nil)
        var deliveredIdentifier = ""
        let persistenceError = try await store.deliverAndPersist(identified, content: content) { request in
            deliveredIdentifier = request.identifier
        }
        precondition(deliveredIdentifier == "durable-id" && persistenceError == nil)
        let afterDelivery = try store.records()
        precondition(afterDelivery.contains { $0.id == "durable-id" })
        let beforeFailedDelivery = afterDelivery
        do {
            let failed = NotificationRecord(id: "failed-delivery", kind: "summary", title: "失败", body: "不会投递")
            _ = try await store.deliverAndPersist(failed, content: content) { _ in
                throw HelperError("delivery failed")
            }
            preconditionFailure("failed delivery was reported as success")
        } catch let error as HelperError {
            precondition(error.message == "delivery failed")
        }
        let afterFailedDelivery = try store.records()
        precondition(afterFailedDelivery == beforeFailedDelivery)

        let blockedParent = directory.appendingPathComponent("not-a-directory")
        try Data("block".utf8).write(to: blockedParent)
        let blockedStore = NotificationInboxStore(fileURL: blockedParent.appendingPathComponent("notifications.json"))
        var deliveredDespiteStorageFailure = false
        let storageError = try await blockedStore.deliverAndPersist(
            NotificationRecord(id: "volatile", kind: "summary", title: "仍会出现", body: "磁盘故障"),
            content: content
        ) { _ in
            deliveredDespiteStorageFailure = true
        }
        precondition(deliveredDespiteStorageFailure && storageError != nil)
        print("Persistent notification recovery, concurrent writes and bounded retention tests passed")
    }
}
