import CoreGraphics
import Foundation

@main
private enum FlightMapTests {
    static func main() {
        let hk = place("HK", 22.45, 114.1)
        let sg = place("SG", 1.37, 103.82)
        let us = place("US", 39.54, -97.48)
        let cn = place("CN", 32.5, 106.34)
        let invalid = FlightPlace(id: "bad", name: "bad", countryCode: nil, latitude: 95, longitude: 1)
        precondition(invalid.coordinate == nil)
        precondition(FlightOrigins.regions.count >= 230)
        precondition(FlightOrigins.regions.filter { $0.id == "HK" }.count == 1)
        precondition(FlightOrigins.regions.contains { $0.id == "SG" && $0.coordinate != nil })
        precondition(FlightNaturalEarth.land.count > 30)
        precondition(FlightNaturalEarth.land.reduce(0) { $0 + $1.count } > 500)

        let entryOnly = FlightRoute(id: "entry", chain: "WARP", entry: hk)
        precondition(entryOnly.legs(origin: cn, includeTarget: false).count == 1)
        let targetOnly = FlightRoute(id: "direct", chain: "DIRECT", target: us)
        precondition(targetOnly.legs(origin: cn, includeTarget: false).isEmpty)
        precondition(targetOnly.legs(origin: cn, includeTarget: true).map(\.kind).count == 1)
        let full = FlightRoute(id: "full", entry: hk, exit: sg, target: us)
        precondition(full.legs(origin: cn, includeTarget: true).count == 3)
        precondition(full.legs(origin: cn, includeTarget: false).count == 2)

        let size = CGSize(width: 360, height: 180)
        let west = FlightCoordinate(latitude: 35, longitude: 170)
        let east = FlightCoordinate(latitude: 40, longitude: -170)
        precondition(FlightProjection.shortestLongitude(from: west.longitude, to: east.longitude) == 190)
        let arc = FlightProjection.arc(from: west, to: east, in: size)!
        precondition(arc.end.x > size.width)
        precondition(FlightProjection.shiftedArcs(from: west, to: east, in: size).count == 3)
        let midpoint = FlightProjection.quadraticPoint(0.5, start: arc.start, control: arc.control, end: arc.end)
        precondition(midpoint.x.isFinite && midpoint.y.isFinite)
        precondition(FlightProjection.point(for: invalidCoordinate(), in: size) == nil)
        let tall = CGSize(width: 600, height: 600)
        let tallRect = FlightProjection.mapRect(in: tall)
        precondition(tallRect == CGRect(x: 0, y: 150, width: 600, height: 300))
        let tallOrigin = FlightProjection.point(for: .init(latitude: 0, longitude: 0), in: tall)!
        let tallEast = FlightProjection.point(for: .init(latitude: 0, longitude: 1), in: tall)!
        let tallNorth = FlightProjection.point(for: .init(latitude: 1, longitude: 0), in: tall)!
        precondition(abs((tallEast.x - tallOrigin.x) - (tallOrigin.y - tallNorth.y)) < 0.0001)
        let wide = CGSize(width: 1_200, height: 400)
        precondition(FlightProjection.mapRect(in: wide) == CGRect(x: 200, y: 0, width: 800, height: 400))
        let acrossPacific = FlightCoordinate(latitude: 40, longitude: -100)
        let visible = FlightProjection.visibleMidpoint(from: west, to: acrossPacific, in: wide)!
        precondition(FlightProjection.mapRect(in: wide).contains(visible))
        let reverse = FlightProjection.visibleMidpoint(from: acrossPacific, to: west, in: wide)!
        precondition(abs(visible.x - reverse.x) < 0.001 && abs(visible.y - reverse.y) < 0.001)
        let dotCloud = FlightLandDotCache.cloud(in: tall)
        precondition(dotCloud.normalizedPoints.count > 500)
        precondition(FlightLandDotCache.cloud(in: tall) === dotCloud)

        let calendar = fixedCalendar()
        let now = Date(timeIntervalSince1970: 2_000_000)
        let today = calendar.startOfDay(for: now)
        precondition(FlightHorizon.today.interval(endingAt: now, calendar: calendar)?.start == today)
        var isoCalendar = calendar; isoCalendar.firstWeekday = 2; isoCalendar.minimumDaysInFirstWeek = 4
        precondition(FlightHorizon.week.interval(endingAt: now, calendar: calendar)?.start
                     == isoCalendar.dateInterval(of: .weekOfYear, for: now)?.start)
        precondition(FlightHorizon.month.interval(endingAt: now, calendar: calendar)?.start
                     == calendar.dateInterval(of: .month, for: now)?.start)
        let recentA = FlightBucket(start: today, end: today.addingTimeInterval(60), chain: "WARP", entry: hk,
                                   exit: sg, target: us, uploadBytes: 10, downloadBytes: 30, connections: 1)
        let recentB = FlightBucket(start: today.addingTimeInterval(60), end: today.addingTimeInterval(120), chain: "WARP",
                                   entry: hk, exit: sg, target: us, uploadBytes: 4, downloadBytes: 6, connections: 2)
        let old = FlightBucket(start: today.addingTimeInterval(-900_000), end: today.addingTimeInterval(-899_000),
                               chain: "WARP", entry: hk, exit: sg, target: us,
                               uploadBytes: 1_000, downloadBytes: 1_000, connections: 20)
        let otherChain = FlightBucket(start: today, end: today.addingTimeInterval(60), chain: "DIRECT", target: us,
                                      uploadBytes: 3, downloadBytes: 7, connections: 1)
        let live = FlightRoute(id: "live", chain: "WARP", entry: hk, downloadBytes: 2, activeConnections: 1)
        let data = FlightMapData(live: [live], buckets: [recentA, recentB, old, otherChain])
        precondition(data.routes(for: .live, now: now, calendar: calendar) == [live])
        let todayRows = data.routes(for: .today, now: now, calendar: calendar)
        precondition(todayRows.count == 2)
        let warp = todayRows.first { $0.chain == "WARP" }!
        precondition(warp.uploadBytes == 14 && warp.downloadBytes == 36 && warp.activeConnections == 3)
        precondition(todayRows.first { $0.chain == "DIRECT" }?.bytes == 10)
        precondition(data.originChoices.count == FlightOrigins.regions.count)
        precondition(data.origin(selectedID: "HK")?.id == "HK")
        precondition(data.origin(selectedID: nil) == nil)

        let custom = FlightMapData(origins: [cn], preferredOriginID: "CN")
        precondition(custom.originChoices == [cn] && custom.origin(selectedID: nil) == cn)
        let saturated = FlightRoute(id: "overflow", uploadBytes: Int64.max, downloadBytes: Int64.max)
        precondition(saturated.bytes == Int64.max)
        let decoder = JSONDecoder(); decoder.dateDecodingStrategy = .secondsSince1970
        let defaults = try! decoder.decode(FlightMapData.self, from: Data("{}".utf8))
        precondition(!defaults.online && defaults.live.isEmpty && defaults.buckets.isEmpty && defaults.origins.isEmpty)
        let sparse = try! decoder.decode(FlightRoute.self, from: Data(#"{"id":"route"}"#.utf8))
        precondition(sparse.chain.isEmpty && sparse.bytes == 0 && sparse.entry == nil)
        let schema = try! FlightMapData.decodeLine(Data(#"{"generatedAt":"2026-11-24T12:00:00Z","online":true,"live":[{"id":"US-WARP-route","chain":"US-WARP","entry":{"id":"US","name":"US","countryCode":"US"},"target":{"id":"JP","name":"JP","countryCode":"JP"},"downloadBytes":2048,"activeConnections":1}],"buckets":[]}"#.utf8))
        let decoded = schema.live[0]
        precondition(decoded.entry?.coordinate == FlightOrigins.coordinate(for: "US"))
        precondition(decoded.target?.coordinate == FlightOrigins.coordinate(for: "JP"))
        precondition(decoded.entry?.displayName != "US")
        let decodedLegs = decoded.legs(origin: FlightOrigins.regions.first { $0.id == "CN" }, includeTarget: true)
        precondition(decodedLegs.count == 2)
        precondition(FlightProjection.arc(from: decodedLegs[0].from.coordinate!, to: decodedLegs[0].to.coordinate!,
                                          in: CGSize(width: 720, height: 360)) != nil)
        print("Flight map tests passed")
    }

    private static func place(_ id: String, _ latitude: Double, _ longitude: Double) -> FlightPlace {
        FlightPlace(id: id, name: id, countryCode: id, latitude: latitude, longitude: longitude)
    }

    private static func invalidCoordinate() -> FlightCoordinate {
        FlightCoordinate(latitude: .nan, longitude: 0)
    }

    private static func fixedCalendar() -> Calendar {
        var result = Calendar(identifier: .gregorian)
        result.timeZone = TimeZone(secondsFromGMT: 0)!
        return result
    }
}
