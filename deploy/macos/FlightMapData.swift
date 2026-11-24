import Foundation

enum FlightHorizon: String, CaseIterable, Decodable, Identifiable, Sendable {
    case live
    case today
    case week
    case month

    var id: String { rawValue }

    var title: String {
        switch self {
        case .live: return "实时"
        case .today: return "今天"
        case .week: return "本周"
        case .month: return "本月"
        }
    }

    func interval(endingAt now: Date, calendar: Calendar = .autoupdatingCurrent) -> DateInterval? {
        switch self {
        case .live:
            return nil
        case .today:
            return DateInterval(start: calendar.startOfDay(for: now), end: now)
        case .week:
            var isoCalendar = calendar
            isoCalendar.firstWeekday = 2
            isoCalendar.minimumDaysInFirstWeek = 4
            return DateInterval(start: isoCalendar.dateInterval(of: .weekOfYear, for: now)?.start ?? now, end: now)
        case .month:
            return DateInterval(start: calendar.dateInterval(of: .month, for: now)?.start ?? now, end: now)
        }
    }
}

struct FlightCoordinate: Decodable, Hashable, Sendable {
    let latitude: Double
    let longitude: Double

    var isValid: Bool {
        latitude.isFinite && longitude.isFinite && (-90...90).contains(latitude) && (-180...180).contains(longitude)
    }
}

struct FlightPlace: Decodable, Hashable, Identifiable, Sendable {
    let id: String
    let name: String
    let countryCode: String?
    let latitude: Double?
    let longitude: Double?

    var coordinate: FlightCoordinate? {
        if let latitude, let longitude {
            let value = FlightCoordinate(latitude: latitude, longitude: longitude)
            if value.isValid { return value }
        }
        return FlightOrigins.coordinate(for: regionCode)
    }

    var displayName: String {
        guard let regionCode else { return name }
        if name.isEmpty || name.caseInsensitiveCompare(regionCode) == .orderedSame {
            return Locale.autoupdatingCurrent.localizedString(forRegionCode: regionCode) ?? regionCode
        }
        return name
    }

    private var regionCode: String? {
        let value = (countryCode ?? (id.count == 2 ? id : nil))?.uppercased()
        return value?.count == 2 ? value : nil
    }
}

struct FlightRoute: Decodable, Hashable, Identifiable, Sendable {
    let id: String
    let chain: String
    let entry: FlightPlace?
    let exit: FlightPlace?
    let target: FlightPlace?
    let uploadBytes: Int64
    let downloadBytes: Int64
    let activeConnections: Int
    let observedAt: Date?

    init(id: String, chain: String = "", entry: FlightPlace? = nil, exit: FlightPlace? = nil,
         target: FlightPlace? = nil, uploadBytes: Int64 = 0, downloadBytes: Int64 = 0,
         activeConnections: Int = 0, observedAt: Date? = nil) {
        self.id = id
        self.chain = chain
        self.entry = entry
        self.exit = exit
        self.target = target
        self.uploadBytes = uploadBytes
        self.downloadBytes = downloadBytes
        self.activeConnections = activeConnections
        self.observedAt = observedAt
    }

    private enum CodingKeys: String, CodingKey {
        case id, chain, entry, exit, target, uploadBytes, downloadBytes, activeConnections, observedAt
    }

    init(from decoder: Decoder) throws {
        let values = try decoder.container(keyedBy: CodingKeys.self)
        id = try values.decode(String.self, forKey: .id)
        chain = try values.decodeIfPresent(String.self, forKey: .chain) ?? ""
        entry = try values.decodeIfPresent(FlightPlace.self, forKey: .entry)
        exit = try values.decodeIfPresent(FlightPlace.self, forKey: .exit)
        target = try values.decodeIfPresent(FlightPlace.self, forKey: .target)
        uploadBytes = try values.decodeIfPresent(Int64.self, forKey: .uploadBytes) ?? 0
        downloadBytes = try values.decodeIfPresent(Int64.self, forKey: .downloadBytes) ?? 0
        activeConnections = try values.decodeIfPresent(Int.self, forKey: .activeConnections) ?? 0
        observedAt = try values.decodeIfPresent(Date.self, forKey: .observedAt)
    }

    var bytes: Int64 { FlightMapData.addBytes(uploadBytes, downloadBytes) }
    var hasKnownGeography: Bool { entry?.coordinate != nil || exit?.coordinate != nil || target?.coordinate != nil }
}

struct FlightBucket: Decodable, Hashable, Sendable {
    let start: Date
    let end: Date
    let chain: String
    let entry: FlightPlace?
    let exit: FlightPlace?
    let target: FlightPlace?
    let uploadBytes: Int64
    let downloadBytes: Int64
    let connections: Int

    init(start: Date, end: Date, chain: String = "", entry: FlightPlace? = nil, exit: FlightPlace? = nil,
         target: FlightPlace? = nil, uploadBytes: Int64 = 0, downloadBytes: Int64 = 0, connections: Int = 0) {
        self.start = start
        self.end = end
        self.chain = chain
        self.entry = entry
        self.exit = exit
        self.target = target
        self.uploadBytes = uploadBytes
        self.downloadBytes = downloadBytes
        self.connections = connections
    }

    private enum CodingKeys: String, CodingKey {
        case start, end, chain, entry, exit, target, uploadBytes, downloadBytes, connections
    }

    init(from decoder: Decoder) throws {
        let values = try decoder.container(keyedBy: CodingKeys.self)
        start = try values.decode(Date.self, forKey: .start)
        end = try values.decode(Date.self, forKey: .end)
        chain = try values.decodeIfPresent(String.self, forKey: .chain) ?? ""
        entry = try values.decodeIfPresent(FlightPlace.self, forKey: .entry)
        exit = try values.decodeIfPresent(FlightPlace.self, forKey: .exit)
        target = try values.decodeIfPresent(FlightPlace.self, forKey: .target)
        uploadBytes = try values.decodeIfPresent(Int64.self, forKey: .uploadBytes) ?? 0
        downloadBytes = try values.decodeIfPresent(Int64.self, forKey: .downloadBytes) ?? 0
        connections = try values.decodeIfPresent(Int.self, forKey: .connections) ?? 0
    }
}

struct FlightMapData: Decodable, Hashable, Sendable {
    let generatedAt: Date?
    let online: Bool
    let liveError: String?
    let historyError: String?
    let geoNotice: String?
    let origins: [FlightPlace]
    let preferredOriginID: String?
    let live: [FlightRoute]
    let buckets: [FlightBucket]

    init(generatedAt: Date? = nil, online: Bool = true, liveError: String? = nil,
         historyError: String? = nil, geoNotice: String? = nil, origins: [FlightPlace] = [],
         preferredOriginID: String? = nil, live: [FlightRoute] = [], buckets: [FlightBucket] = []) {
        self.generatedAt = generatedAt
        self.online = online
        self.liveError = liveError
        self.historyError = historyError
        self.geoNotice = geoNotice
        self.origins = origins
        self.preferredOriginID = preferredOriginID
        self.live = live
        self.buckets = buckets
    }

    private enum CodingKeys: String, CodingKey {
        case generatedAt, online, liveError, historyError, geoNotice, origins, preferredOriginID, live, buckets
    }

    init(from decoder: Decoder) throws {
        let values = try decoder.container(keyedBy: CodingKeys.self)
        generatedAt = try values.decodeIfPresent(Date.self, forKey: .generatedAt)
        online = try values.decodeIfPresent(Bool.self, forKey: .online) ?? false
        liveError = try values.decodeIfPresent(String.self, forKey: .liveError)
        historyError = try values.decodeIfPresent(String.self, forKey: .historyError)
        geoNotice = try values.decodeIfPresent(String.self, forKey: .geoNotice)
        origins = try values.decodeIfPresent([FlightPlace].self, forKey: .origins) ?? []
        preferredOriginID = try values.decodeIfPresent(String.self, forKey: .preferredOriginID)
        live = try values.decodeIfPresent([FlightRoute].self, forKey: .live) ?? []
        buckets = try values.decodeIfPresent([FlightBucket].self, forKey: .buckets) ?? []
    }

    func routes(for horizon: FlightHorizon, now: Date = Date(), calendar: Calendar = .autoupdatingCurrent) -> [FlightRoute] {
        guard let interval = horizon.interval(endingAt: now, calendar: calendar) else {
            return live.sorted(by: Self.routeOrder)
        }
        struct Key: Hashable {
            let chain: String
            let entry: FlightPlace?
            let exit: FlightPlace?
            let target: FlightPlace?
        }
        var rows: [Key: (upload: Int64, download: Int64, connections: Int, observed: Date)] = [:]
        for bucket in buckets where bucket.end > interval.start && bucket.start <= interval.end {
            let key = Key(chain: bucket.chain, entry: bucket.entry, exit: bucket.exit, target: bucket.target)
            var value = rows[key] ?? (0, 0, 0, bucket.end)
            value.upload = Self.addBytes(value.upload, bucket.uploadBytes)
            value.download = Self.addBytes(value.download, bucket.downloadBytes)
            value.connections = Self.addCount(value.connections, bucket.connections)
            value.observed = max(value.observed, bucket.end)
            rows[key] = value
        }
        return rows.map { key, value in
            FlightRoute(id: Self.routeID(key.chain, key.entry, key.exit, key.target), chain: key.chain,
                        entry: key.entry, exit: key.exit,
                        target: key.target, uploadBytes: value.upload, downloadBytes: value.download,
                        activeConnections: value.connections, observedAt: value.observed)
        }.sorted(by: Self.routeOrder)
    }

    func origin(selectedID: String?) -> FlightPlace? {
        let requested = selectedID ?? preferredOriginID
        return originChoices.first { $0.id == requested && $0.coordinate != nil }
    }

    var originChoices: [FlightPlace] { origins.isEmpty ? FlightOrigins.regions : origins }

    static func decodeLine(_ data: Data) throws -> FlightMapData {
        let decoder = JSONDecoder()
        decoder.dateDecodingStrategy = .custom { decoder in
            let value = try decoder.singleValueContainer()
            let text = try value.decode(String.self)
            let format = Date.ISO8601FormatStyle(includingFractionalSeconds: text.contains("."))
            guard let date = try? format.parse(text) else {
                throw DecodingError.dataCorruptedError(in: value, debugDescription: "Expected an RFC 3339 timestamp")
            }
            return date
        }
        return try decoder.decode(FlightMapData.self, from: data)
    }

    fileprivate static func addBytes(_ left: Int64, _ right: Int64) -> Int64 {
        let (sum, overflow) = max(0, left).addingReportingOverflow(max(0, right))
        return overflow ? Int64.max : sum
    }

    private static func addCount(_ left: Int, _ right: Int) -> Int {
        let (sum, overflow) = max(0, left).addingReportingOverflow(max(0, right))
        return overflow ? Int.max : sum
    }

    private static func routeOrder(_ left: FlightRoute, _ right: FlightRoute) -> Bool {
        if left.bytes != right.bytes { return left.bytes > right.bytes }
        if left.activeConnections != right.activeConnections { return left.activeConnections > right.activeConnections }
        return left.id < right.id
    }

    private static func routeID(_ chain: String, _ entry: FlightPlace?, _ exit: FlightPlace?, _ target: FlightPlace?) -> String {
        [chain, entry?.id ?? "?", exit?.id ?? "?", target?.id ?? "?"].joined(separator: "→")
    }
}

enum FlightOrigins {
    // Region label points derived from Natural Earth 1:50m admin-0 data (public domain).
    // Rounded coordinates are region-level anchors for layout, not user or city locations.
    static let anchors: [String: FlightCoordinate] = Dictionary(uniqueKeysWithValues: encoded.split(separator: ";").compactMap { row in
        let values = row.split(separator: ",")
        guard values.count == 3, let longitude = Double(values[1]), let latitude = Double(values[2]) else { return nil }
        return (String(values[0]), FlightCoordinate(latitude: latitude, longitude: longitude))
    })

    static let regions: [FlightPlace] = anchors.keys.map { code in
        FlightPlace(id: code, name: code, countryCode: code, latitude: nil, longitude: nil)
    }.sorted { $0.displayName.localizedStandardCompare($1.displayName) == .orderedAscending }

    static func coordinate(for code: String?) -> FlightCoordinate? {
        code.flatMap { anchors[$0.uppercased()] }
    }

    private static let encoded = """
AD,1.54,42.55;AE,54.55,23.47;AF,66.5,34.16;AG,-61.79,17.35;AI,-63.03,18.24;AL,20.11,40.65;AM,44.8,40.46;AO,17.98,-12.18;AQ,35.89,-79.84;AR,-64.17,-33.5;AS,-170.75,-14.33;AT,14.13,47.52;AU,134.05,-24.13;AW,-69.97,12.52;AX,19.87,60.16;AZ,47.21,40.4;BA,18.07,44.09;BB,-59.57,13.16;BD,89.68,24.21;BE,4.8,50.79;BF,-1.36,12.67;BG,25.16,42.51;BH,50.55,26.06;BI,29.92,-3.33;BJ,2.35,10.32;BL,-62.83,17.9;BM,-64.76,32.3;BN,114.55,4.45;BO,-64.59,-16.67;BR,-49.56,-12.1;BS,-77.15,26.4;BT,90.04,27.54;BW,24.18,-22.1;BY,28.42,53.82;BZ,-88.71,17.2;CA,-101.91,60.32;CD,23.46,-1.86;CF,20.91,6.99;CG,15.9,0.14;CH,7.46,46.72;CI,-5.57,7.49;CK,-159.79,-21.22;CL,-72.32,-38.15;CM,12.47,4.59;CN,106.34,32.5;CO,-73.17,3.37;CR,-84.08,10.07;CU,-77.98,21.33;CV,-23.64,15.07;CW,-68.92,12.15;CY,33.08,34.91;CZ,15.38,49.88;DE,9.68,50.96;DJ,42.5,11.98;DK,9.02,55.97;DM,-61.34,15.46;DO,-70.65,19.1;DZ,2.81,27.4;EC,-78.19,-1.26;EE,25.87,58.72;EG,29.45,26.19;EH,-12.63,23.97;ER,38.29,15.79;ES,-3.46,40.09;ET,39.09,8.03;FI,27.28,63.25;FJ,177.98,-17.83;FK,-58.74,-51.61;FM,158.23,6.89;FO,-7.06,62.19;FR,2.55,46.7;GA,11.84,-0.44;GB,-2.12,54.4;GD,-61.68,12.11;GE,43.74,41.87;GG,-2.56,49.46;GH,-1.04,7.72;GL,-39.34,74.32;GM,-15,13.64;GN,-10.02,10.62;GQ,8.99,2.33;GR,21.73,39.49;GS,-31.06,-55.68;GT,-90.5,14.98;GU,144.7,13.35;GW,-14.52,12.16;GY,-58.94,5.12;HK,114.1,22.45;HM,73.51,-53.1;HN,-86.89,14.79;HR,16.37,45.81;HT,-72.22,19.26;HU,19.45,47.09;ID,101.89,-0.95;IE,-7.8,53.08;IL,34.85,30.91;IM,-4.53,54.22;IN,79.36,22.69;IO,71.35,-6.19;IQ,43.26,33.09;IR,54.93,32.17;IS,-18.67,64.78;IT,11.08,44.73;JE,-2.09,49.22;JM,-77.32,18.14;JO,36.38,30.81;JP,138.44,36.14;KE,37.91,0.55;KG,74.53,41.67;KH,104.5,12.65;KI,-157.38,1.82;KM,43.32,-11.73;KN,-62.76,17.34;KP,126.44,39.89;KR,128.13,36.38;KW,47.31,29.41;KY,-81.24,19.32;KZ,68.69,49.05;LA,102.53,19.43;LB,35.99,34.13;LC,-60.98,13.89;LI,9.56,47.11;LK,80.7,7.58;LR,-9.46,6.45;LS,28.25,-29.48;LT,24.09,55.1;LU,6.08,49.73;LV,25.46,57.07;LY,18.01,26.64;MA,-7.19,31.65;MC,7.4,43.74;MD,28.49,47.43;ME,19.14,42.8;MF,-63.05,18.08;MG,46.7,-18.63;MH,171.19,7.08;MK,21.56,41.56;ML,-2.04,18.69;MM,95.8,21.57;MN,104.15,46;MO,113.56,22.13;MP,145.73,15.19;MR,-9.74,19.59;MS,-62.19,16.74;MT,14.43,35.89;MU,57.57,-20.3;MV,73.51,4.17;MW,33.61,-13.39;MX,-102.29,23.92;MY,113.84,2.53;MZ,37.84,-13.94;NA,17.11,-20.58;NC,165.08,-21.06;NE,9.5,17.45;NF,167.95,-29.03;NG,7.5,9.44;NI,-85.07,12.67;NL,5.61,52.42;NO,9.68,61.36;NP,83.64,28.3;NR,166.93,-0.52;NU,-169.86,-19.05;NZ,172.79,-39.76;OM,57.34,22.12;PA,-80.35,8.72;PE,-72.9,-12.98;PF,-149.46,-17.63;PG,143.91,-5.7;PH,122.47,11.2;PK,68.55,29.33;PL,19.49,51.99;PM,-56.33,47.04;PN,-128.32,-24.36;PR,-66.48,18.23;PS,35.29,32.05;PT,-8.27,39.61;PW,134.58,7.52;PY,-60.15,-21.67;QA,51.14,25.24;RO,24.97,45.73;RS,20.79,44.19;RU,44.69,58.25;RW,30.1,-1.9;SA,44.7,23.81;SB,159.17,-8.03;SC,55.48,-4.68;SD,29.26,16.33;SE,19.02,65.86;SG,103.82,1.37;SH,-5.71,-15.95;SI,14.92,46.06;SK,19.05,48.73;SL,-11.76,8.62;SM,12.44,43.93;SN,-14.78,15.14;SO,45.19,3.57;SR,-55.91,4.14;SS,30.39,7.23;ST,7.02,0.97;SV,-88.89,13.69;SX,-63.07,18.04;SY,38.28,35.01;SZ,31.47,-26.53;TC,-71.75,21.82;TD,18.65,15.14;TF,69.12,-49.3;TG,1.06,8.81;TH,101.07,15.46;TJ,72.59,38.2;TL,125.85,-8.8;TM,58.68,39.86;TN,9.01,33.69;TO,-175.16,-21.21;TR,34.51,39.35;TT,-60.92,11;TV,179.21,-8.51;TW,120.87,23.65;TZ,34.96,-6.05;UA,32.14,49.72;UG,32.95,1.97;US,-97.48,39.54;UY,-55.97,-32.96;UZ,64.01,41.69;VA,12.45,41.9;VC,-61.34,13.09;VE,-64.6,7.18;VG,-64.64,18.43;VI,-64.78,17.75;VN,105.39,21.72;VU,166.91,-15.37;WF,-178.14,-14.29;WS,-172.44,-13.64;XK,20.86,42.59;YE,45.87,15.33;ZA,23.67,-29.71;ZM,26.4,-14.66;ZW,29.93,-18.91
"""
}
