import UserNotifications

@main
private struct NotificationSetupTests {
    @MainActor
    static func main() async throws {
        for args in [[], ["-psn_0_12345"], ["--dashboard"]] {
            let mode = try HelperMode(arguments: args)
            precondition(mode == .dashboard(config: nil))
        }
        let customDashboard = try HelperMode(arguments: ["--dashboard", "-c", "/tmp/example.yaml"])
        precondition(customDashboard == .dashboard(config: "/tmp/example.yaml"))
        let explicit = try HelperMode(arguments: ["--authorize"])
        let check = try HelperMode(arguments: ["--check"])
        let launchedCheck = try HelperMode(arguments: ["-psn_0_12345", "--check"])
        let send = try HelperMode(arguments: ["--send", "--kind", "switch", "--title", "title", "--body", "body"])
        let linkedSend = try HelperMode(arguments: ["--send", "--event-id", "decision-1", "--body", "body", "--kind", "switch", "--title", "title"])
        precondition(explicit == .authorize && check == .check && launchedCheck == .check)
        precondition(send == .send(kind: "switch", title: "title", body: "body", eventID: nil))
        precondition(linkedSend == .send(kind: "switch", title: "title", body: "body", eventID: "decision-1"))
        let source = "/tmp/guard config/guard.yaml"
        let click = UNNotificationDefaultActionIdentifier
        let switchClick = DashboardDestination(action: click, thread: "mi-guard.switch", userInfo: ["guardConfigPath": source, "decisionID": "decision-1"])
        precondition(switchClick?.section == .selections && switchClick?.configURL?.path == source && switchClick?.decisionID == "decision-1")
        let summaryClick = DashboardDestination(action: click, thread: "mi-guard.summary", userInfo: [:])
        precondition(summaryClick?.section == .overview && summaryClick?.configURL == nil && summaryClick?.decisionID == nil)
        let inboxClick = DashboardDestination(action: click, thread: "mi-guard.switch",
                                              userInfo: ["notificationID": "notification-1", "decisionID": "decision-1"])
        precondition(inboxClick?.section == .notifications && inboxClick?.notificationID == "notification-1")
        precondition(DashboardDestination(action: UNNotificationDismissActionIdentifier, thread: "mi-guard.switch", userInfo: [:]) == nil)

        var requests = 0
        let result = try await authorizeIfNeeded(settings: { .notDetermined }) {
            requests += 1
            return true
        }
        precondition(result == .granted && requests == 1)
        for state: UNAuthorizationStatus in [.authorized, .provisional] {
            let result = try await authorizeIfNeeded(settings: { state }) { fatalError("unexpected repeat prompt") }
            precondition(result == .alreadyAllowed(state))
        }
        for state: UNAuthorizationStatus in [.notDetermined, .denied] {
            var requested = false
            do {
                _ = try await authorizeIfNeeded(settings: { requested ? .denied : state }) {
                    requested = true
                    return false
                }
                fatalError("ungranted permission was reported as success")
            } catch let error as HelperError {
                precondition(error.message == deniedMessage)
                precondition(requested == (state == .notDetermined))
            }
        }
        // Notification Center can report an error even after the user has enabled notifications.
        let callbackError = NSError(domain: UNErrorDomain, code: UNError.Code.notificationsNotAllowed.rawValue)
        for throwsError in [false, true] {
            var reads = 0
            var requests = 0
            let result = try await authorizeIfNeeded(settings: {
                reads += 1
                return reads == 1 ? .notDetermined : .authorized
            }) {
                requests += 1
                if throwsError { throw callbackError }
                return false
            }
            precondition(result == .granted && reads == 2 && requests == 1)
        }
        do {
            _ = try await authorizeIfNeeded(settings: { .notDetermined }) { throw callbackError }
            fatalError("an unresolved error was reported as success")
        } catch {
            precondition(error as NSError == callbackError)
        }
        print("Notification setup tests passed")
    }
}
