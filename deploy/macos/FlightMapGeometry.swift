import CoreGraphics
import Foundation

enum FlightLegKind: Sendable {
    case originToEntry
    case entryToExit
    case exitToTarget
}

struct FlightLeg: Sendable {
    let from: FlightPlace
    let to: FlightPlace
    let kind: FlightLegKind
}

extension FlightRoute {
    func legs(origin: FlightPlace?, includeTarget: Bool) -> [FlightLeg] {
        var result: [FlightLeg] = []
        let firstProxyPlace = entry?.coordinate != nil ? entry : exit?.coordinate != nil ? exit : nil
        if let origin, origin.coordinate != nil, let firstProxyPlace, firstProxyPlace.id != origin.id {
            result.append(FlightLeg(from: origin, to: firstProxyPlace, kind: .originToEntry))
        }
        if let entry, let exit, entry.coordinate != nil, exit.coordinate != nil, entry.id != exit.id {
            result.append(FlightLeg(from: entry, to: exit, kind: .entryToExit))
        }
        if includeTarget, let target, target.coordinate != nil {
            let lastProxyPlace = exit?.coordinate != nil ? exit : entry?.coordinate != nil ? entry : nil
            if let lastProxyPlace, lastProxyPlace.id != target.id {
                result.append(FlightLeg(from: lastProxyPlace, to: target, kind: .exitToTarget))
            } else if lastProxyPlace == nil, let origin, origin.coordinate != nil, origin.id != target.id {
                result.append(FlightLeg(from: origin, to: target, kind: .exitToTarget))
            }
        }
        return result
    }
}

enum FlightProjection {
    static let aspectRatio: CGFloat = 2

    static func mapRect(in size: CGSize) -> CGRect {
        guard size.width > 0, size.height > 0 else { return .zero }
        let width = min(size.width, size.height * aspectRatio)
        let height = width / aspectRatio
        return CGRect(x: (size.width - width) / 2, y: (size.height - height) / 2,
                      width: width, height: height)
    }

    static func point(for coordinate: FlightCoordinate, in size: CGSize) -> CGPoint? {
        guard coordinate.isValid, size.width > 0, size.height > 0 else { return nil }
        let rect = mapRect(in: size)
        return CGPoint(x: rect.minX + (coordinate.longitude + 180) / 360 * rect.width,
                       y: rect.minY + (90 - coordinate.latitude) / 180 * rect.height)
    }

    static func shortestLongitude(from: Double, to: Double) -> Double {
        var value = to
        while value - from > 180 { value -= 360 }
        while value - from < -180 { value += 360 }
        return value
    }

    static func arc(from: FlightCoordinate, to: FlightCoordinate, in size: CGSize) -> (start: CGPoint, control: CGPoint, end: CGPoint)? {
        guard let start = point(for: from, in: size) else { return nil }
        let rect = mapRect(in: size)
        let unwrapped = FlightCoordinate(latitude: to.latitude,
                                         longitude: shortestLongitude(from: from.longitude, to: to.longitude))
        let end = CGPoint(x: rect.minX + (unwrapped.longitude + 180) / 360 * rect.width,
                          y: rect.minY + (90 - unwrapped.latitude) / 180 * rect.height)
        let distance = hypot(end.x - start.x, end.y - start.y)
        let direction: CGFloat = (start.y + end.y) / 2 < rect.midY ? 1 : -1
        let lift = min(rect.height * 0.23, max(12, distance * 0.19)) * direction
        let control = CGPoint(x: (start.x + end.x) / 2, y: (start.y + end.y) / 2 - lift)
        return (start, control, end)
    }

    static func shiftedArcs(from: FlightCoordinate, to: FlightCoordinate, in size: CGSize) -> [(CGPoint, CGPoint, CGPoint)] {
        guard let arc = arc(from: from, to: to, in: size) else { return [] }
        let width = mapRect(in: size).width
        return [-width, 0, width].map { shift in
            (CGPoint(x: arc.start.x + shift, y: arc.start.y),
             CGPoint(x: arc.control.x + shift, y: arc.control.y),
             CGPoint(x: arc.end.x + shift, y: arc.end.y))
        }
    }

    static func visibleMidpoint(from: FlightCoordinate, to: FlightCoordinate, in size: CGSize) -> CGPoint? {
        guard let arc = arc(from: from, to: to, in: size) else { return nil }
        let bounds = mapRect(in: size)
        var point = quadraticPoint(0.5, start: arc.start, control: arc.control, end: arc.end)
        if point.x < bounds.minX { point.x += bounds.width }
        if point.x > bounds.maxX { point.x -= bounds.width }
        return point
    }

    static func quadraticPoint(_ progress: Double, start: CGPoint, control: CGPoint, end: CGPoint) -> CGPoint {
        let t = CGFloat(min(1, max(0, progress))), inverse = 1 - t
        return CGPoint(x: inverse * inverse * start.x + 2 * inverse * t * control.x + t * t * end.x,
                       y: inverse * inverse * start.y + 2 * inverse * t * control.y + t * t * end.y)
    }
}

enum FlightWorld {
    static let land = FlightNaturalEarth.land
}

final class FlightLandDotCloud: NSObject {
    let normalizedPoints: [CGPoint]
    let columns: Int

    init(normalizedPoints: [CGPoint], columns: Int) {
        self.normalizedPoints = normalizedPoints
        self.columns = columns
    }
}

enum FlightLandDotCache {
    private struct Polygon {
        let points: [FlightCoordinate]
        let longitude: ClosedRange<Double>
        let latitude: ClosedRange<Double>

        init?(_ points: [FlightCoordinate]) {
            guard let first = points.first else { return nil }
            var minLongitude = first.longitude, maxLongitude = first.longitude
            var minLatitude = first.latitude, maxLatitude = first.latitude
            for point in points.dropFirst() {
                minLongitude = min(minLongitude, point.longitude)
                maxLongitude = max(maxLongitude, point.longitude)
                minLatitude = min(minLatitude, point.latitude)
                maxLatitude = max(maxLatitude, point.latitude)
            }
            self.points = points
            longitude = minLongitude...maxLongitude
            latitude = minLatitude...maxLatitude
        }

        func contains(longitude x: Double, latitude y: Double) -> Bool {
            guard longitude.contains(x), latitude.contains(y) else { return false }
            var inside = false
            var previous = points[points.count - 1]
            for current in points {
                if (current.latitude > y) != (previous.latitude > y) {
                    let crossing = (previous.longitude - current.longitude) * (y - current.latitude)
                        / (previous.latitude - current.latitude) + current.longitude
                    if x < crossing { inside.toggle() }
                }
                previous = current
            }
            return inside
        }
    }

    private static let polygons = FlightWorld.land.compactMap(Polygon.init)
    private static let cache: NSCache<NSNumber, FlightLandDotCloud> = {
        let value = NSCache<NSNumber, FlightLandDotCloud>()
        value.countLimit = 16
        return value
    }()

    static func cloud(in size: CGSize) -> FlightLandDotCloud {
        let rect = FlightProjection.mapRect(in: size)
        let columns = min(180, max(54, Int((rect.width / 8).rounded())))
        let key = NSNumber(value: columns)
        if let cached = cache.object(forKey: key) { return cached }

        let rows = max(27, columns / 2)
        var points: [CGPoint] = []
        points.reserveCapacity(columns * rows / 3)
        for row in 0..<rows {
            let normalizedY = (CGFloat(row) + 0.5) / CGFloat(rows)
            let latitude = 90 - Double(normalizedY) * 180
            for column in 0..<columns {
                let normalizedX = (CGFloat(column) + 0.5) / CGFloat(columns)
                let longitude = Double(normalizedX) * 360 - 180
                if polygons.contains(where: { $0.contains(longitude: longitude, latitude: latitude) }) {
                    points.append(CGPoint(x: normalizedX, y: normalizedY))
                }
            }
        }
        let cloud = FlightLandDotCloud(normalizedPoints: points, columns: columns)
        cache.setObject(cloud, forKey: key)
        return cloud
    }
}
