import Foundation

@main
private enum FlightPanelTests {
    @MainActor static func main() {
        testDecoder()
        testCommand()
        testController()
        print("Flight panel tests passed")
    }

    private static func testDecoder() {
        var decoder = FlightStreamDecoder()
        precondition(try! decoder.append(Data(" {\"online\":true".utf8)).isEmpty)
        let lines = try! decoder.append(Data("}\n\n{\"online\":false}\r\npartial".utf8))
        precondition(lines.map { String(decoding: $0, as: UTF8.self) } == ["{\"online\":true}", "{\"online\":false}"])
        precondition(try! decoder.finish().map { String(decoding: $0, as: UTF8.self) } == ["partial"])
        var oversized = FlightStreamDecoder()
        do {
            _ = try oversized.append(Data(repeating: 65, count: 4 * 1024 * 1024 + 1))
            preconditionFailure("oversized line was accepted")
        } catch FlightStreamError.lineTooLong {}
        catch { preconditionFailure("unexpected decoder error: \(error)") }
        var framedOversized = FlightStreamDecoder()
        var frame = Data(repeating: 65, count: 4 * 1024 * 1024 + 1); frame.append(0x0A)
        do {
            _ = try framedOversized.append(frame)
            preconditionFailure("oversized newline-terminated frame was accepted")
        } catch FlightStreamError.lineTooLong {}
        catch { preconditionFailure("unexpected framed decoder error: \(error)") }
    }

    private static func testCommand() {
        precondition(FlightCommand.arguments(configURL: nil) == ["flights", "--watch"])
        let url = URL(fileURLWithPath: "/tmp/guard config.yaml")
        precondition(FlightCommand.arguments(configURL: url) == ["flights", "--watch", "-c", "/tmp/guard config.yaml"])
    }

    @MainActor private static func testController() {
        let launcher = FakeLauncher()
        let configA = URL(fileURLWithPath: "/tmp/a.yaml")
        let controller = FlightPanelController(reader: URL(fileURLWithPath: "/tmp/fixture"),
                                               configURL: configA, launcher: launcher)
        controller.setForeground(true)
        precondition(launcher.launches == 0)
        controller.setVisible(true)
        precondition(launcher.launches == 1 && controller.connecting)

        launcher.send(#"{"generatedAt":"2026-11-24T12:00:00Z","online":false,"liveError":"offline","live":[],"buckets":[]}"#)
        drainMainQueue()
        precondition(controller.data?.online == false && controller.data?.liveError == "offline")
        precondition(!controller.connecting && controller.streamProblem == nil)

        controller.setForeground(false)
        precondition(launcher.processes.last?.stops == 1)
        controller.setForeground(true)
        precondition(launcher.launches == 2)
        launcher.finish(0)
        drainMainQueue()
        precondition(launcher.launches == 2)
        precondition(controller.streamProblem?.contains("已结束") == true)

        controller.refresh()
        precondition(launcher.launches == 3)
        let configB = URL(fileURLWithPath: "/tmp/b.yaml")
        controller.update(configURL: configB)
        precondition(launcher.launches == 4 && controller.data == nil)
        precondition(launcher.configs.last == configB.standardizedFileURL)
        controller.setVisible(false)
        precondition(launcher.processes.last?.stops == 1)
    }

    private static func drainMainQueue() {
        RunLoop.main.run(until: Date().addingTimeInterval(0.02))
    }
}

private final class FakeProcess: FlightPanelProcess {
    private(set) var stops = 0
    func stop() { stops += 1 }
}

private final class FakeLauncher: FlightPanelLaunching {
    private(set) var launches = 0
    private(set) var configs: [URL?] = []
    private(set) var processes: [FakeProcess] = []
    private var receive: ((Data) -> Void)?
    private var finishHandler: ((Int32) -> Void)?

    func launch(reader: URL, configURL: URL?, receive: @escaping (Data) -> Void,
                failure: @escaping (Error) -> Void, finish: @escaping (Int32) -> Void) throws -> FlightPanelProcess {
        launches += 1
        configs.append(configURL)
        self.receive = receive
        finishHandler = finish
        let process = FakeProcess()
        processes.append(process)
        return process
    }

    func send(_ line: String) { receive?(Data(line.utf8)) }
    func finish(_ status: Int32) { finishHandler?(status) }
}
