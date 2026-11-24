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
    static func point(for coordinate: FlightCoordinate, in size: CGSize) -> CGPoint? {
        guard coordinate.isValid, size.width > 0, size.height > 0 else { return nil }
        return CGPoint(x: (coordinate.longitude + 180) / 360 * size.width,
                       y: (90 - coordinate.latitude) / 180 * size.height)
    }

    static func shortestLongitude(from: Double, to: Double) -> Double {
        var value = to
        while value - from > 180 { value -= 360 }
        while value - from < -180 { value += 360 }
        return value
    }

    static func arc(from: FlightCoordinate, to: FlightCoordinate, in size: CGSize) -> (start: CGPoint, control: CGPoint, end: CGPoint)? {
        guard let start = point(for: from, in: size) else { return nil }
        let unwrapped = FlightCoordinate(latitude: to.latitude,
                                         longitude: shortestLongitude(from: from.longitude, to: to.longitude))
        let end = CGPoint(x: (unwrapped.longitude + 180) / 360 * size.width,
                          y: (90 - unwrapped.latitude) / 180 * size.height)
        let distance = hypot(end.x - start.x, end.y - start.y)
        let direction: CGFloat = (start.y + end.y) / 2 < size.height / 2 ? 1 : -1
        let lift = min(size.height * 0.23, max(12, distance * 0.19)) * direction
        let control = CGPoint(x: (start.x + end.x) / 2, y: (start.y + end.y) / 2 - lift)
        return (start, control, end)
    }

    static func shiftedArcs(from: FlightCoordinate, to: FlightCoordinate, in size: CGSize) -> [(CGPoint, CGPoint, CGPoint)] {
        guard let arc = arc(from: from, to: to, in: size) else { return [] }
        return [-size.width, 0, size.width].map { shift in
            (CGPoint(x: arc.start.x + shift, y: arc.start.y),
             CGPoint(x: arc.control.x + shift, y: arc.control.y),
             CGPoint(x: arc.end.x + shift, y: arc.end.y))
        }
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
