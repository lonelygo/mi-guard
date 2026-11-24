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
