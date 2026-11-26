import Foundation

struct DashboardSnapshot: Decodable, Sendable {
    let budgetBytes: Int64
    /// Includes pending reservations. Settled traffic is usedBytes - pendingBytes.
    let usedBytes: Int64
    let pendingBytes: Int64
    let cycleStart: Date
    let cycleEnd: Date
    let lastSweep: Date?
    let samples: [LedgerSample]
    let groups: [LedgerGroup]
    /// Optional for dashboards reading a v0.1.x helper.
    let decisions: [DecisionRecord]?
    let selectedDecision: DecisionRecord?
    let decisionWarning: String?

    var decisionHistory: [DecisionRecord] { decisions ?? [] }

    static func decode(_ data: Data) throws -> DashboardSnapshot {
        let decoder = JSONDecoder()
        decoder.dateDecodingStrategy = .custom { decoder in
            let container = try decoder.singleValueContainer()
            let text = try container.decode(String.self)
            let format = Date.ISO8601FormatStyle(includingFractionalSeconds: text.contains("."))
            guard let date = try? format.parse(text) else {
                throw DecodingError.dataCorruptedError(in: container, debugDescription: "Expected an RFC 3339 timestamp")
            }
            return date
        }
        return try decoder.decode(Self.self, from: data)
    }
}

struct DecisionRecord: Decodable, Hashable, Identifiable, Sendable {
    let id: String
    let at: Date
    let source: String
    let group: String
    let chain: String?
    let before: String?
    let after: String?
    let action: String
    let reason: String?
    let trigger: String?
    let nearBestPct: Double?
    let thresholdMbps: Double?
    let minMbps: Double?
    let latencyMeanMS: Double?
    let latencySamples: Int?
    let latencyFactor: Double?
    let latencyThresholdMS: Double?
    let candidates: [DecisionCandidate]?
    let followers: [DecisionFollower]?

    var evidence: [DecisionCandidate] { candidates ?? [] }
    var followResults: [DecisionFollower] { followers ?? [] }
}

struct DecisionCandidate: Decodable, Hashable, Sendable {
    let node: String
    let provider: String?
    let kind: String?
    let observedAt: Date?
    let target: String?
    let mbps: Double?
    let latencyMS: Int?
    let attempts: Int?
    let failures: Int?
    let streams: Int?
    let requestedBytes: Int64?
    let bytes: Int64?
    let stage: String?
    let status: String?
    let rank: Int?
    let preferredBand: Bool?
    let strategyAttempts: Int?
    let strategyFailures: Int?
    let historyAttempts: Int?
    let historyFailures: Int?
    let historySamples: Int?
    let historyP25Mbps: Double?
    let validationOrder: Int?
    let validation: String?
    let selected: Bool?
}

struct DecisionFollower: Decodable, Hashable, Identifiable, Sendable {
    var id: String { group }
    let group: String
    let before: String?
    let after: String?
    let action: String
    let reason: String?
}

struct LedgerSample: Decodable, Hashable, Sendable {
    let time: Date
    let node: String
    let provider: String
    let scope: String
    let kind: String
    let mbps: Double
    let latencyMS: Int
    let bytes: Int64
    let attempts: Int
    let failures: Int
    let error: String
    /// Zero for samples recorded without a download profile.
    let streams: Int
    let requestedBytes: Int64
    let target: String

    var successful: Bool {
        error.isEmpty && failures == 0 && mbps.isFinite && mbps > 0
    }
}

struct LedgerGroup: Decodable, Hashable, Sendable {
    let name: String
    let chain: String
    let configured: Bool
    /// The selection recorded at lastSeen.
    let node: String
    let since: Date?
    let lastSeen: Date?
    let guardSelected: Bool
    let longestNode: String
    let longestSeconds: Double
}
