import SwiftUI

struct DashboardChart: View {
    let plot: DashboardPlot

    var body: some View {
        GeometryReader { geometry in
            let width = geometry.size.width, height = geometry.size.height
            let left: CGFloat = 44, right = max(left, width - 6)
            let top: CGFloat = 18, bottom = height - (plot.failures.isEmpty ? 24 : 48)
            let x: (Date) -> CGFloat = { left + (right - left) * CGFloat(plot.fraction(at: $0)) }
            ZStack(alignment: .topLeading) {
                if let ceiling = plot.ceiling {
                    let y: (Double) -> CGFloat = { bottom - (bottom - top) * CGFloat($0 / ceiling) }
                    Path { path in
                        for value in [0, ceiling / 2, ceiling] {
                            path.move(to: CGPoint(x: left, y: y(value)))
                            path.addLine(to: CGPoint(x: right, y: y(value)))
                        }
                    }
                    .stroke(Color.secondary.opacity(0.24), style: StrokeStyle(lineWidth: 0.7, dash: [3, 4]))

                    Text("Mbps").font(.caption2).foregroundColor(.secondary)
                    ForEach(0..<3) { i in
                        let value = ceiling * (Double(i) / 2)
                        Text(verbatim: DashboardFormat.number(value))
                            .font(.caption.monospacedDigit()).foregroundColor(.secondary)
                            .lineLimit(1).minimumScaleFactor(0.75)
                            .frame(width: left - 8, alignment: .trailing)
                            .position(x: (left - 8) / 2, y: y(value))
                    }

                    Path { path in
                        for run in plot.segments where run.count > 1 {
                            guard let first = run.first, let last = run.last else { continue }
                            path.move(to: CGPoint(x: x(first.time), y: bottom))
                            for point in run { path.addLine(to: CGPoint(x: x(point.time), y: y(point.mbps))) }
                            path.addLine(to: CGPoint(x: x(last.time), y: bottom))
                            path.closeSubpath()
                        }
                    }
                    .fill(LinearGradient(colors: [DashboardStyle.accent.opacity(0.19), DashboardStyle.accent.opacity(0.025)],
                                         startPoint: .top, endPoint: .bottom))

                    Path { path in
                        for run in plot.segments {
                            guard let first = run.first else { continue }
                            path.move(to: CGPoint(x: x(first.time), y: y(first.mbps)))
                            for point in run.dropFirst() { path.addLine(to: CGPoint(x: x(point.time), y: y(point.mbps))) }
                        }
                    }
                    .stroke(DashboardStyle.accent, style: StrokeStyle(lineWidth: 2, lineCap: .round, lineJoin: .round))

                    Path { path in
                        for run in plot.segments {
                            for point in run {
                                path.addEllipse(in: CGRect(x: x(point.time) - 3, y: y(point.mbps) - 3, width: 6, height: 6))
                            }
                        }
                    }
                    .fill(DashboardStyle.accent)
                } else {
                    Text("此时段没有成功的测速").font(.callout).foregroundColor(.secondary)
                        .frame(maxWidth: .infinity).offset(y: 8)
                }

                if !plot.failures.isEmpty {
                    let row = height - 28
                    Text("失败").font(.caption).foregroundColor(.secondary)
                        .frame(width: left - 8, alignment: .trailing)
                        .position(x: (left - 8) / 2, y: row)
                    Path { path in
                        path.move(to: CGPoint(x: left, y: row))
                        path.addLine(to: CGPoint(x: right, y: row))
                    }.stroke(Color.secondary.opacity(0.2), lineWidth: 0.7)
                    Path { path in
                        for time in plot.failures {
                            let center = x(time)
                            path.move(to: CGPoint(x: center - 3, y: row - 3))
                            path.addLine(to: CGPoint(x: center + 3, y: row + 3))
                            path.move(to: CGPoint(x: center + 3, y: row - 3))
                            path.addLine(to: CGPoint(x: center - 3, y: row + 3))
                        }
                    }.stroke(Color(nsColor: .systemRed), style: StrokeStyle(lineWidth: 1.8, lineCap: .round))
                }

                let span = plot.domain.upperBound.timeIntervalSince(plot.domain.lowerBound)
                ForEach(0..<3) { i in
                    let time = plot.domain.lowerBound.addingTimeInterval(span * Double(i) / 2)
                    let labelWidth = (right - left) / 3
                    let offset = i == 0 ? labelWidth / 2 : i == 2 ? -labelWidth / 2 : 0
                    Text(verbatim: DashboardFormat.tick(time, in: plot.domain))
                        .font(.caption.monospacedDigit()).foregroundColor(.secondary)
                        .lineLimit(1).minimumScaleFactor(0.8)
                        .frame(width: labelWidth, alignment: i == 0 ? .leading : i == 2 ? .trailing : .center)
                        .position(x: x(time) + offset, y: height - 8)
                }
                ChartInspection(plot: plot, plotFrame: CGRect(x: left, y: top, width: right - left, height: bottom - top))
            }
        }
        .frame(height: plot.ceiling == nil ? 82 : plot.failures.isEmpty ? 148 : 168)
        .accessibilityElement(children: .ignore)
        .accessibilityLabel("历史吞吐量图，单位 Mbps")
        .accessibilityValue("\(DashboardFormat.stamp.string(from: plot.domain.lowerBound)) 至 \(DashboardFormat.stamp.string(from: plot.domain.upperBound))，成功 \(plot.segments.reduce(0) { $0 + $1.count }) 次，失败 \(plot.failures.count) 次。逐条数据见测速明细。")
    }
}
