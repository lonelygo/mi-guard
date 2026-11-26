import SwiftUI

struct NotificationInboxView: View {
    @ObservedObject var controller: DashboardController

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            HStack(spacing: 10) {
                VStack(alignment: .leading, spacing: 4) {
                    Text("通知记录").font(.system(size: 26, weight: .semibold))
                    Text("保留最近 \(NotificationInboxStore.retentionLimit) 条已投递通知")
                        .font(.callout).foregroundColor(.secondary)
                }
                Spacer()
                Button(action: controller.refreshNotifications) {
                    Label("刷新", systemImage: "arrow.clockwise")
                }
                .help("重新读取本地通知记录")
            }
            .padding(.horizontal, 22).padding(.top, 18).padding(.bottom, 15)

            ScrollViewReader { proxy in
                ScrollView {
                    LazyVStack(alignment: .leading, spacing: 12) {
                        if let problem = controller.notificationProblem {
                            Label(problem, systemImage: "exclamationmark.triangle")
                                .font(.callout).foregroundColor(.secondary)
                                .dashboardCard(12)
                        }
                        if controller.notificationRecords.isEmpty {
                            DashboardEmpty(symbol: "bell.slash", title: "暂无通知记录",
                                           detail: "下一次扫描摘要或线路切换通知会保存在这里。")
                                .dashboardCard()
                        } else {
                            ForEach(controller.notificationRecords) { record in
                                NotificationRecordCard(
                                    record: record,
                                    expanded: controller.selectedNotificationID == record.id,
                                    toggle: { controller.toggleNotification(record.id) },
                                    openDestination: { controller.openNotificationDestination(record) }
                                )
                                .id(record.id)
                            }
                        }
                    }
                    .padding(.horizontal, 22).padding(.bottom, 20)
                    .frame(maxWidth: .infinity, alignment: .leading)
                }
                .onReceive(controller.$selectedNotificationID) { id in
                    guard let id else { return }
                    DispatchQueue.main.async { proxy.scrollTo(id, anchor: .top) }
                }
            }
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .topLeading)
        .onAppear(perform: controller.refreshNotifications)
    }

}

private struct NotificationRecordCard: View {
    let record: NotificationRecord
    let expanded: Bool
    let toggle: () -> Void
    let openDestination: () -> Void

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            Button(action: toggle) {
                HStack(alignment: .top, spacing: 12) {
                    Image(systemName: symbol)
                        .font(.system(size: 17, weight: .medium))
                        .foregroundColor(tint)
                        .frame(width: 24, height: 24)
                    VStack(alignment: .leading, spacing: 5) {
                        HStack(alignment: .firstTextBaseline, spacing: 8) {
                            Text(verbatim: record.title).font(.headline).lineLimit(2)
                            Spacer(minLength: 8)
                            Text(verbatim: relativeTime)
                                .font(.caption.monospacedDigit()).foregroundColor(.secondary)
                        }
                        if !expanded {
                            Text(verbatim: record.body)
                                .font(.callout).foregroundColor(.secondary).lineLimit(2)
                        }
                    }
                    Image(systemName: expanded ? "chevron.up" : "chevron.down")
                        .font(.caption.weight(.semibold)).foregroundColor(.secondary)
                        .padding(.top, 4)
                }
                .padding(15)
                .contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            .accessibilityLabel("\(record.title)，\(expanded ? "折叠" : "展开")通知")

            if expanded {
                Divider().padding(.horizontal, 15)
                VStack(alignment: .leading, spacing: 12) {
                    Text(verbatim: record.body)
                        .font(.body).fixedSize(horizontal: false, vertical: true).textSelection(.enabled)
                    HStack(alignment: .firstTextBaseline, spacing: 14) {
                        Label(exactTime, systemImage: "clock")
                        if let configPath = record.configPath {
                            Label(URL(fileURLWithPath: configPath).lastPathComponent, systemImage: "doc")
                                .lineLimit(1).truncationMode(.middle).help(configPath)
                        }
                    }
                    .font(.caption).foregroundColor(.secondary)
                    if record.decisionID != nil {
                        Button(action: openDestination) {
                            Label("查看此次线路选用", systemImage: "arrow.triangle.branch")
                        }
                        .controlSize(.small)
                    }
                }
                .padding(15)
            }
        }
        .background(RoundedRectangle(cornerRadius: 11).fill(DashboardStyle.card))
        .overlay(
            RoundedRectangle(cornerRadius: 11)
                .strokeBorder(expanded ? tint.opacity(0.58) : Color(nsColor: .separatorColor).opacity(0.6),
                              lineWidth: expanded ? 1 : 0.7)
        )
    }

    private var symbol: String {
        switch record.kind {
        case "switch": return "arrow.triangle.branch"
        case "summary": return "checkmark.circle"
        default: return "bell"
        }
    }

    private var tint: Color {
        record.kind == "switch" ? DashboardStyle.accent : Color(nsColor: .systemBlue)
    }

    private var relativeTime: String {
        let now = Date()
        if abs(record.deliveredAt.timeIntervalSince(now)) < 60 { return "刚刚" }
        let formatter = RelativeDateTimeFormatter()
        formatter.locale = Locale(identifier: "zh_CN")
        formatter.unitsStyle = .short
        return formatter.localizedString(for: record.deliveredAt, relativeTo: now)
    }

    private var exactTime: String {
        DashboardFormat.stamp.string(from: record.deliveredAt)
    }
}
