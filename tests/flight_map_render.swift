import AppKit
import SwiftUI

@main
private enum FlightMapRenderTest {
    @MainActor static func main() throws {
        let arguments = Array(CommandLine.arguments.dropFirst())
        let output = arguments.first { !$0.hasPrefix("--") } ?? "/tmp/mi-guard-flight-map.png"
        let light = arguments.contains("--light")
        let compact = arguments.contains("--compact")
        let panel = arguments.contains("--panel")
        let error = arguments.contains("--error")
        let contentSize = compact ? CGSize(width: 680, height: 430) : CGSize(width: 1080, height: 680)
        let surfaceSize = CGSize(width: contentSize.width + 40, height: contentSize.height + 40)
        let cn = place("CN", "中国", 32.5, 106.34)
        let hk = place("HK", "香港", 22.45, 114.1)
        let sg = place("SG", "新加坡", 1.37, 103.82)
        let us = place("US", "美国", 39.54, -97.48)
        let jp = place("JP", "日本", 36.14, 138.44)
        let data = FlightMapData(origins: [cn, hk, sg], preferredOriginID: "CN", live: [
            FlightRoute(id: "warp", chain: "US-WARP", entry: hk, exit: us, target: jp,
                        uploadBytes: 14_500_000, downloadBytes: 320_000_000, activeConnections: 18),
            FlightRoute(id: "sg", chain: "SG-WARP", entry: sg, exit: sg,
                        uploadBytes: 2_500_000, downloadBytes: 64_000_000, activeConnections: 5),
            FlightRoute(id: "direct", chain: "DIRECT", target: hk,
                        uploadBytes: 120_000, downloadBytes: 8_000_000, activeConnections: 2),
            FlightRoute(id: "unknown", chain: "未知地区线路", downloadBytes: 4_000_000, activeConnections: 1)
        ])
        let content: AnyView
        if panel {
            let launcher = FlightMapPreviewLauncher()
            let controller = FlightPanelController(reader: URL(fileURLWithPath: "/tmp/flight-map-preview"),
                                                   configURL: nil, launcher: launcher)
            controller.setForeground(true)
            controller.setVisible(true)
            launcher.send(previewDocument)
            RunLoop.main.run(until: Date().addingTimeInterval(0.02))
            if error {
                launcher.finish(1)
                RunLoop.main.run(until: Date().addingTimeInterval(0.02))
            }
            precondition(controller.data?.geoNotice != nil)
            content = AnyView(FlightPanel(controller: controller, configURL: nil))
        } else {
            content = AnyView(FlightMapView(data: data))
        }
        let root = content.frame(width: contentSize.width, height: contentSize.height).padding(20)
            .background(Color(nsColor: .windowBackgroundColor)).preferredColorScheme(light ? .light : .dark)
        let view = NSHostingView(rootView: root)
        view.appearance = NSAppearance(named: light ? .aqua : .darkAqua)
        view.frame = CGRect(origin: .zero, size: surfaceSize)
        view.layoutSubtreeIfNeeded()
        guard let bitmap = view.bitmapImageRepForCachingDisplay(in: view.bounds) else {
            throw RenderError.failed
        }
        view.cacheDisplay(in: view.bounds, to: bitmap)
        guard bitmap.pixelsWide >= Int(surfaceSize.width), bitmap.pixelsHigh >= Int(surfaceSize.height),
              let image = bitmap.representation(using: .png, properties: [:]) else {
            throw RenderError.failed
        }
        try image.write(to: URL(fileURLWithPath: output), options: .atomic)
        print("Flight map render passed: \(output)")
    }

    private static let previewDocument = #"{"generatedAt":"2026-11-26T02:13:49Z","online":true,"geoNotice":"当前使用内核报告和手动配置的地区。可指定本机 Country.mmdb 补全 IP 所在地区。","origins":[{"id":"CN","name":"中国","countryCode":"CN","latitude":32.5,"longitude":106.34},{"id":"HK","name":"香港","countryCode":"HK","latitude":22.45,"longitude":114.1}],"preferredOriginID":"CN","live":[{"id":"warp","chain":"US-WARP","entry":{"id":"HK","name":"香港","countryCode":"HK","latitude":22.45,"longitude":114.1},"exit":{"id":"US","name":"美国","countryCode":"US","latitude":39.54,"longitude":-97.48},"target":{"id":"JP","name":"日本","countryCode":"JP","latitude":36.14,"longitude":138.44},"uploadBytes":14500000,"downloadBytes":320000000,"activeConnections":18}],"buckets":[]}"#

    private static func place(_ id: String, _ name: String, _ latitude: Double, _ longitude: Double) -> FlightPlace {
        FlightPlace(id: id, name: name, countryCode: id, latitude: latitude, longitude: longitude)
    }

    private enum RenderError: Error { case failed }
}

private final class FlightMapPreviewProcess: FlightPanelProcess {
    func stop() {}
}

private final class FlightMapPreviewLauncher: FlightPanelLaunching {
    private var receive: ((Data) -> Void)?
    private var finishHandler: ((Int32) -> Void)?

    func launch(reader: URL, configURL: URL?, receive: @escaping (Data) -> Void,
                failure: @escaping (Error) -> Void, finish: @escaping (Int32) -> Void) throws -> FlightPanelProcess {
        self.receive = receive
        finishHandler = finish
        return FlightMapPreviewProcess()
    }

    func send(_ document: String) { receive?(Data(document.utf8)) }
    func finish(_ status: Int32) { finishHandler?(status) }
}
