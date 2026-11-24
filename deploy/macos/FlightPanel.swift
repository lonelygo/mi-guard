import AppKit
import Foundation
import SwiftUI

struct FlightStreamDecoder {
    private var buffer = Data()
    private let maximumLineBytes = 4 * 1024 * 1024

    mutating func append(_ data: Data) throws -> [Data] {
        buffer.append(data)
        guard buffer.count <= maximumLineBytes || buffer.contains(0x0A) else { throw FlightStreamError.lineTooLong }
        var lines: [Data] = []
        while let newline = buffer.firstIndex(of: 0x0A) {
            guard buffer.distance(from: buffer.startIndex, to: newline) <= maximumLineBytes else {
                throw FlightStreamError.lineTooLong
            }
            let line = clean(Data(buffer[..<newline]))
            buffer.removeSubrange(...newline)
            if !line.isEmpty { lines.append(line) }
        }
        if buffer.count > maximumLineBytes { throw FlightStreamError.lineTooLong }
        return lines
    }

    mutating func finish() throws -> [Data] {
        guard !buffer.isEmpty else { return [] }
        defer { buffer.removeAll(keepingCapacity: false) }
        guard buffer.count <= maximumLineBytes else { throw FlightStreamError.lineTooLong }
        let line = clean(buffer)
        return line.isEmpty ? [] : [line]
    }

    private func clean(_ data: Data) -> Data {
        let whitespace = CharacterSet.whitespacesAndNewlines
        guard let text = String(data: data, encoding: .utf8) else { return data }
        return Data(text.trimmingCharacters(in: whitespace).utf8)
    }
}

enum FlightStreamError: LocalizedError {
    case lineTooLong

    var errorDescription: String? { "线路数据单行超过 4 MiB，已停止读取。" }
}

enum FlightCommand {
    static func arguments(configURL: URL?) -> [String] {
        var result = ["flights", "--watch"]
        if let configURL { result += ["-c", configURL.standardizedFileURL.path] }
        return result
    }
}

protocol FlightPanelProcess: AnyObject {
    func stop()
}

protocol FlightPanelLaunching {
    func launch(reader: URL, configURL: URL?, receive: @escaping (Data) -> Void,
                failure: @escaping (Error) -> Void, finish: @escaping (Int32) -> Void) throws -> FlightPanelProcess
}

final class FlightLineChannel {
    private let lock = NSLock()
    private var decoder = FlightStreamDecoder()
    private var finished = false
    private let receive: (Data) -> Void
    private let failure: (Error) -> Void

    init(receive: @escaping (Data) -> Void, failure: @escaping (Error) -> Void) {
        self.receive = receive
        self.failure = failure
    }

    func append(_ data: Data) {
        lock.lock()
        guard !finished else { lock.unlock(); return }
        do {
            let lines = try decoder.append(data)
            lock.unlock()
            lines.forEach(receive)
        } catch {
            finished = true
            lock.unlock()
            failure(error)
        }
    }

    func finish() {
        lock.lock()
        guard !finished else { lock.unlock(); return }
        finished = true
        do {
            let lines = try decoder.finish()
            lock.unlock()
            lines.forEach(receive)
        } catch {
            lock.unlock()
            failure(error)
        }
    }
}

final class FlightSubprocessLauncher: FlightPanelLaunching {
    func launch(reader: URL, configURL: URL?, receive: @escaping (Data) -> Void,
                failure: @escaping (Error) -> Void, finish: @escaping (Int32) -> Void) throws -> FlightPanelProcess {
        let process = Process()
        let stdout = Pipe(), stderr = Pipe()
        let channel = FlightLineChannel(receive: receive, failure: failure)
        process.executableURL = reader
        process.arguments = FlightCommand.arguments(configURL: configURL)
        process.standardOutput = stdout
        process.standardError = stderr
        stdout.fileHandleForReading.readabilityHandler = { handle in
            let data = handle.availableData
            if !data.isEmpty { channel.append(data) }
        }
        stderr.fileHandleForReading.readabilityHandler = { handle in _ = handle.availableData }
        process.terminationHandler = { task in
            stdout.fileHandleForReading.readabilityHandler = nil
            stderr.fileHandleForReading.readabilityHandler = nil
            channel.append(stdout.fileHandleForReading.readDataToEndOfFile())
            channel.finish()
            finish(task.terminationStatus)
        }
        try process.run()
        return FlightSubprocess(process: process, stdout: stdout, stderr: stderr)
    }
}

final class FlightSubprocess: FlightPanelProcess {
    private let lock = NSLock()
    private var process: Process?
    private let stdout: Pipe
    private let stderr: Pipe

    init(process: Process, stdout: Pipe, stderr: Pipe) {
        self.process = process
        self.stdout = stdout
        self.stderr = stderr
    }

    func stop() {
        lock.lock()
        let running = process
        process = nil
        lock.unlock()
        stdout.fileHandleForReading.readabilityHandler = nil
        stderr.fileHandleForReading.readabilityHandler = nil
        if running?.isRunning == true { running?.terminate() }
    }

    deinit { stop() }
}

@MainActor
final class FlightPanelController: ObservableObject {
    @Published private(set) var data: FlightMapData?
    @Published private(set) var connecting = false
    @Published private(set) var streamProblem: String?

    private let reader: URL
    private let launcher: FlightPanelLaunching
    private var configURL: URL?
    private var session: FlightPanelProcess?
    private var generation = 0
    private var visible = false
    private var foreground = false

    init(reader: URL, configURL: URL?, launcher: FlightPanelLaunching = FlightSubprocessLauncher()) {
        self.reader = reader
        self.configURL = configURL?.standardizedFileURL
        self.launcher = launcher
    }

    func setVisible(_ value: Bool) {
        visible = value
        value ? startIfNeeded() : stop()
    }

    func setForeground(_ value: Bool) {
        foreground = value
        value ? startIfNeeded() : stop()
    }

    func update(configURL: URL?) {
        let next = configURL?.standardizedFileURL
        guard next != self.configURL else { return }
        stop()
        self.configURL = next
        data = nil
        streamProblem = nil
        startIfNeeded()
    }

    func refresh() {
        stop()
        streamProblem = nil
        startIfNeeded()
    }

    private func startIfNeeded() {
        guard visible, foreground, session == nil else { return }
        generation += 1
        let token = generation
        connecting = true
        do {
            session = try launcher.launch(reader: reader, configURL: configURL, receive: { [weak self] line in
                DispatchQueue.main.async { self?.receive(line, generation: token) }
            }, failure: { [weak self] error in
                DispatchQueue.main.async { self?.fail(error, generation: token) }
            }, finish: { [weak self] status in
                DispatchQueue.main.async { self?.finish(status, generation: token) }
            })
        } catch {
            connecting = false
            streamProblem = "无法启动线路数据进程：\(error.localizedDescription)"
        }
    }

    private func receive(_ line: Data, generation token: Int) {
        guard token == generation else { return }
        do {
            data = try FlightMapData.decodeLine(line)
            connecting = false
            streamProblem = nil
        } catch {
            connecting = false
            streamProblem = "收到无法解析的线路数据：\(error.localizedDescription)"
        }
    }

    private func fail(_ error: Error, generation token: Int) {
        guard token == generation else { return }
        generation += 1
        connecting = false
        streamProblem = error.localizedDescription
        session?.stop()
        session = nil
    }

    private func finish(_ status: Int32, generation token: Int) {
        guard token == generation else { return }
        session = nil
        connecting = false
        if status != 0 {
            streamProblem = "线路数据进程已退出（状态 \(status)）。"
        } else if data == nil {
            streamProblem = "线路数据进程未返回数据。"
        } else {
            streamProblem = "线路数据流已结束，可重新连接。"
        }
    }

    private func stop() {
        generation += 1
        session?.stop()
        session = nil
        connecting = false
    }
}

struct FlightPanel: View {
    let configURL: URL?
    @SwiftUI.StateObject private var controller: FlightPanelController

    init(reader: URL, configURL: URL?) {
        self.configURL = configURL
        _controller = SwiftUI.StateObject(wrappedValue: FlightPanelController(reader: reader, configURL: configURL))
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            header
            ScrollView {
                VStack(alignment: .leading, spacing: 12) {
                    notices
                    if let data = controller.data {
                        FlightMapView(data: data)
                    } else if controller.connecting {
                        FlightPanelEmpty(symbol: "antenna.radiowaves.left.and.right", title: "正在连接 Mihomo…",
                                         detail: "首次快照到达后可查看活跃连接和守护进程观测到的历史流量。")
                    } else {
                        FlightPanelEmpty(symbol: "map", title: "尚无线路数据",
                                         detail: "检查配置后重新连接。已有历史不会因内核离线而被清除。")
                    }
                }
                .padding(.horizontal, 22).padding(.bottom, 20)
            }
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .topLeading)
        .onAppear {
            controller.setForeground(NSApplication.shared.isActive)
            controller.setVisible(true)
        }
        .onDisappear { controller.setVisible(false) }
        .onChange(of: configURL) { controller.update(configURL: $0) }
        .onReceive(NotificationCenter.default.publisher(for: NSApplication.didBecomeActiveNotification)) { _ in
            controller.setForeground(true)
        }
        .onReceive(NotificationCenter.default.publisher(for: NSApplication.didResignActiveNotification)) { _ in
            controller.setForeground(false)
        }
        .onReceive(NotificationCenter.default.publisher(for: NSApplication.didHideNotification)) { _ in
            controller.setForeground(false)
        }
        .onReceive(NotificationCenter.default.publisher(for: NSApplication.didUnhideNotification)) { _ in
            controller.setForeground(NSApplication.shared.isActive)
        }
        .onReceive(NotificationCenter.default.publisher(for: NSWindow.didMiniaturizeNotification)) { _ in
            controller.setForeground(false)
        }
        .onReceive(NotificationCenter.default.publisher(for: NSWindow.didDeminiaturizeNotification)) { _ in
            controller.setForeground(NSApplication.shared.isActive)
        }
    }

    private var header: some View {
        HStack(spacing: 12) {
            VStack(alignment: .leading, spacing: 4) {
                Text("线路地图").font(.system(size: 26, weight: .semibold)).accessibilityAddTraits(.isHeader)
                if let generatedAt = controller.data?.generatedAt {
                    Text("更新于 \(FlightPanelFormat.stamp.string(from: generatedAt))")
                        .font(.callout).foregroundColor(.secondary)
                } else {
                    Text("Mihomo 实时线路与本地历史").font(.callout).foregroundColor(.secondary)
                }
            }
            Spacer()
            if controller.connecting { ProgressView().controlSize(.small).accessibilityLabel("正在连接线路数据") }
            FlightOnlineBadge(online: controller.data?.online)
            Button(action: controller.refresh) { Label("重新连接", systemImage: "arrow.clockwise") }
                .disabled(controller.connecting)
        }
        .padding(.horizontal, 22).padding(.top, 18).padding(.bottom, 15)
    }

    @ViewBuilder private var notices: some View {
        if let problem = controller.streamProblem {
            FlightNotice(symbol: "exclamationmark.arrow.triangle.2.circlepath", title: "数据连接中断", detail: problem,
                         color: .orange, retry: controller.refresh)
        }
        if let error = controller.data?.liveError {
            FlightNotice(symbol: "bolt.slash", title: "实时线路暂不可用", detail: error, color: .orange)
        } else if controller.data?.online == false {
            FlightNotice(symbol: "bolt.slash", title: "Mihomo 当前离线",
                         detail: "历史地图仍可查看。重新上线后点击重新连接即可恢复实时线路。", color: .orange)
        }
        if let error = controller.data?.historyError {
            FlightNotice(symbol: "clock.badge.exclamationmark", title: "部分历史暂不可用", detail: error, color: .orange)
        }
        if let notice = controller.data?.geoNotice {
            FlightNotice(symbol: "location.circle", title: "地区信息", detail: notice, color: .secondary)
        }
    }
}

private struct FlightOnlineBadge: View {
    let online: Bool?
    var body: some View {
        HStack(spacing: 6) {
            Circle().fill(online == true ? Color.green : online == false ? Color.orange : Color.secondary)
                .frame(width: 7, height: 7)
            Text(online == true ? "内核在线" : online == false ? "内核离线" : "等待状态")
                .font(.caption.weight(.medium))
        }
        .padding(.horizontal, 9).padding(.vertical, 5)
        .background(Capsule().fill(Color.secondary.opacity(0.12)))
    }
}

private struct FlightNotice: View {
    let symbol: String
    let title: String
    let detail: String
    let color: Color
    var retry: (() -> Void)? = nil

    var body: some View {
        HStack(alignment: .top, spacing: 10) {
            Image(systemName: symbol).foregroundColor(color).frame(width: 20).accessibilityHidden(true)
            VStack(alignment: .leading, spacing: 3) {
                Text(title).font(.callout.weight(.semibold))
                Text(verbatim: detail).font(.caption).foregroundColor(.secondary).textSelection(.enabled)
            }
            Spacer(minLength: 8)
            if let retry { Button("重试", action: retry).controlSize(.small) }
        }
        .padding(11).frame(maxWidth: .infinity, alignment: .leading)
        .background(RoundedRectangle(cornerRadius: 10).fill(color.opacity(0.075)))
        .overlay(RoundedRectangle(cornerRadius: 10).strokeBorder(color.opacity(0.20), lineWidth: 0.7))
    }
}

private struct FlightPanelEmpty: View {
    let symbol: String
    let title: String
    let detail: String

    var body: some View {
        VStack(spacing: 8) {
            Image(systemName: symbol).font(.title2).foregroundColor(.secondary).accessibilityHidden(true)
            Text(title).font(.headline)
            Text(detail).font(.callout).foregroundColor(.secondary).multilineTextAlignment(.center)
        }
        .frame(maxWidth: .infinity).padding(.vertical, 48)
        .background(RoundedRectangle(cornerRadius: 12).fill(Color(nsColor: .controlBackgroundColor)))
    }
}

private enum FlightPanelFormat {
    static let stamp: DateFormatter = {
        let value = DateFormatter()
        value.locale = Locale(identifier: "zh_CN")
        value.timeZone = .autoupdatingCurrent
        value.dateFormat = "M月d日 HH:mm:ss"
        return value
    }()
}
