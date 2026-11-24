import SwiftUI

/// Inspect an actual observation, including a failed one, without interpolating a speed.
struct ChartInspection: View {
    let plot: DashboardPlot
    let plotFrame: CGRect
    @State private var pointer: CGFloat?

    private struct Reading {
        let time: Date
        let mbps: Double?
    }

    private func nearest(_ position: CGFloat?) -> Reading? {
        guard let position, plotFrame.width > 0, position >= plotFrame.minX, position <= plotFrame.maxX else { return nil }
        let fraction = (position - plotFrame.minX) / plotFrame.width
        let when = plot.domain.lowerBound.addingTimeInterval(plot.domain.upperBound.timeIntervalSince(plot.domain.lowerBound) * Double(fraction))
        let readings = plot.segments.flatMap { $0.map { Reading(time: $0.time, mbps: $0.mbps) } }
        return (readings + plot.failures.map { Reading(time: $0, mbps: nil) }).min {
            abs($0.time.timeIntervalSince(when)) < abs($1.time.timeIntervalSince(when))
        }
    }

    var body: some View {
        let overlay = ZStack(alignment: .topLeading) {
            Color.clear
            if let reading = nearest(pointer) {
                let x = plotFrame.minX + CGFloat(plot.fraction(at: reading.time)) * plotFrame.width
                Path { path in
                    path.move(to: CGPoint(x: x, y: plotFrame.minY))
                    path.addLine(to: CGPoint(x: x, y: plotFrame.maxY))
                }
                .stroke(Color.secondary.opacity(0.75), style: StrokeStyle(lineWidth: 1, dash: [3, 3]))
                HStack(spacing: 7) {
                    Text(verbatim: DashboardFormat.stamp.string(from: reading.time))
                    Text(verbatim: reading.mbps.map { "\(DashboardFormat.number($0)) Mbps" } ?? "测速失败")
                        .fontWeight(.semibold)
                }
                .font(.caption.monospacedDigit())
                .padding(.horizontal, 8).padding(.vertical, 4)
                .background(.regularMaterial, in: RoundedRectangle(cornerRadius: 5))
                .fixedSize()
                .frame(maxWidth: .infinity, alignment: x > plotFrame.midX ? .leading : .trailing)
                .padding(.leading, plotFrame.minX + 4).padding(.trailing, 8)
            }
        }
        .contentShape(Rectangle())
        .gesture(DragGesture(minimumDistance: 0, coordinateSpace: .local).onChanged { pointer = $0.location.x })
        .onHover { inside in if !inside { pointer = nil } }
        .accessibilityHidden(true)
        if #available(macOS 13, *) {
            overlay.onContinuousHover { phase in
                switch phase {
                case .active(let point): pointer = point.x
                case .ended: pointer = nil
                }
            }
        } else {
            overlay
        }
    }
}
