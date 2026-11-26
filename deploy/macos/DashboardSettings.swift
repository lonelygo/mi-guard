import SwiftUI

struct DashboardSettingsView: View {
    @ObservedObject var controller: DashboardController

    var body: some View {
        let defaultURL = FileManager.default.homeDirectoryForCurrentUser.appendingPathComponent(".config/mihomo-guard/mihomo-guard.yaml")
        let current = controller.configURL ?? defaultURL
        VStack(alignment: .leading, spacing: 14) {
            Label("配置文件", systemImage: "doc.text").font(.headline)
            VStack(alignment: .leading, spacing: 8) {
                Text(verbatim: current.lastPathComponent).font(.body.weight(.semibold))
                Text(verbatim: (current.path as NSString).abbreviatingWithTildeInPath)
                    .font(.callout).foregroundColor(.secondary).textSelection(.enabled)
                    .fixedSize(horizontal: false, vertical: true)
                Divider()
                HStack {
                    Button("选择配置…", action: controller.chooseConfig)
                    if controller.configURL != nil {
                        Button("使用默认配置") { controller.useConfig(nil, remember: true) }
                    }
                }
            }.dashboardCard(14)
            Text("仪表盘使用此配置读取测速历史、线路选用和地图数据。")
                .font(.callout).foregroundColor(.secondary)
        }
        .padding(22).frame(width: 460, alignment: .leading)
        .background(Color(nsColor: .windowBackgroundColor))
    }
}
