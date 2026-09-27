import Foundation

enum ApiError: LocalizedError {
    case transport(String)
    case problem(Problem)
    case invalidResponse

    var errorDescription: String? {
        switch self {
        case .transport(let m): return m
        case .problem(let p):
            var s = "\(p.status): \(p.title)"
            if let d = p.detail { s += " — \(d)" }
            return s
        case .invalidResponse: return "Unexpected server response."
        }
    }
}

/// Thin async client mirroring the Android `Api` object: bearer auth,
/// RFC 7807 problem decoding, snake_case conversion in both directions.
enum Api {
    static var token: String?
    static var baseURL = URL(string: "http://127.0.0.1:8080")!

    // Go marshals time.Time as RFC3339, usually with fractional seconds
    // (e.g. 2026-09-27T10:00:00.123456Z) — plain .iso8601 would fail.
    private static let isoFrac: ISO8601DateFormatter = {
        let f = ISO8601DateFormatter()
        f.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        return f
    }()
    private static let isoPlain = ISO8601DateFormatter()

    private static func decoder() -> JSONDecoder {
        let d = JSONDecoder()
        d.keyDecodingStrategy = .convertFromSnakeCase
        d.dateDecodingStrategy = .custom { decoder in
            let c = try decoder.singleValueContainer()
            let raw = try c.decode(String.self)
            if let date = isoFrac.date(from: raw) ?? isoPlain.date(from: raw) {
                return date
            }
            throw DecodingError.dataCorruptedError(
                in: c, debugDescription: "Unparsable RFC3339 date: \(raw)")
        }
        return d
    }

    private static func encoder() -> JSONEncoder {
        let e = JSONEncoder()
        e.keyEncodingStrategy = .convertToSnakeCase
        return e
    }

    /// Performs a request and decodes the 2xx payload as `T`.
    /// Non-2xx payloads are decoded as RFC 7807 `Problem` when possible.
    static func request<T: Decodable>(
        _ method: String,
        _ path: String,
        as type: T.Type = EmptyResponse.self,
        body: Encodable? = nil,
        idempotencyKey: String? = nil
    ) async throws -> T {
        var req = URLRequest(url: baseURL.appendingPathComponent(path))
        req.httpMethod = method
        req.setValue("application/json", forHTTPHeaderField: "Accept")
        if let t = token {
            req.setValue("Bearer \(t)", forHTTPHeaderField: "Authorization")
        }
        if let k = idempotencyKey {
            req.setValue(k, forHTTPHeaderField: "Idempotency-Key")
        }
        if let body = body {
            req.setValue("application/json", forHTTPHeaderField: "Content-Type")
            req.httpBody = try encoder().encode(AnyEncodable(body))
        }

        let data: Data
        let resp: URLResponse
        do {
            (data, resp) = try await URLSession.shared.data(for: req)
        } catch {
            throw ApiError.transport(error.localizedDescription)
        }
        guard let http = resp as? HTTPURLResponse else {
            throw ApiError.invalidResponse
        }
        if (200..<300).contains(http.statusCode) {
            if T.self == EmptyResponse.self {
                return EmptyResponse() as! T
            }
            do {
                return try decoder().decode(T.self, from: data)
            } catch {
                throw ApiError.invalidResponse
            }
        }
        if let problem = try? decoder().decode(Problem.self, from: data) {
            throw ApiError.problem(problem)
        }
        throw ApiError.transport("HTTP \(http.statusCode)")
    }
}

struct EmptyResponse: Codable {}

/// Type-erased wrapper so any `Encodable` body can be passed.
private struct AnyEncodable: Encodable {
    private let encodeFunc: (Encoder) throws -> Void
    init(_ wrapped: Encodable) { encodeFunc = wrapped.encode }
    func encode(to encoder: Encoder) throws { try encodeFunc(encoder) }
}
