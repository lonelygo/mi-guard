import AppKit
import SwiftUI

@main
private enum FlightMapRenderTest {
    static func main() throws {
        let arguments = Array(CommandLine.arguments.dropFirst())
        let output = arguments.first { !$0.hasPrefix("--") } ?? "/tmp/mi-guard-flight-map.png"
        let light = arguments.contains("--light")
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
        let root = FlightMapView(data: data).frame(width: 1000, height: 620).padding(20)
            .background(Color(nsColor: .windowBackgroundColor)).preferredColorScheme(light ? .light : .dark)
        let view = NSHostingView(rootView: root)
        view.appearance = NSAppearance(named: light ? .aqua : .darkAqua)
        view.frame = CGRect(x: 0, y: 0, width: 1040, height: 660)
        view.layoutSubtreeIfNeeded()
        guard let bitmap = view.bitmapImageRepForCachingDisplay(in: view.bounds) else {
            throw RenderError.failed
        }
        view.cacheDisplay(in: view.bounds, to: bitmap)
        guard bitmap.pixelsWide > 900, bitmap.pixelsHigh > 550,
              let image = bitmap.representation(using: .png, properties: [:]) else {
            throw RenderError.failed
        }
        try image.write(to: URL(fileURLWithPath: output), options: .atomic)
        print("Flight map render passed: \(output)")
    }

    private static func place(_ id: String, _ name: String, _ latitude: Double, _ longitude: Double) -> FlightPlace {
        FlightPlace(id: id, name: name, countryCode: id, latitude: latitude, longitude: longitude)
    }

    private enum RenderError: Error { case failed }
}
