import Foundation

public final class MuninnClient: @unchecked Sendable {
    private let baseURL: URL
    private let token: String
    private let timeout: TimeInterval
    private let maxRetries: Int
    private let defaultVault: String
    private let session: URLSession

    public init(
        baseURL: URL = URL(string: "http://localhost:8476")!,
        token: String,
        timeout: TimeInterval = 30,
        maxRetries: Int = 3,
        defaultVault: String = "default",
        session: URLSession = .shared
    ) {
        self.baseURL = baseURL
        self.token = token
        self.timeout = timeout
        self.maxRetries = maxRetries
        self.defaultVault = defaultVault
        self.session = session
    }

    // MARK: - Public API

    public func write(_ options: WriteOptions) async throws -> WriteResponse {
        return try await _request("POST", path: "/api/engrams", body: options)
    }

    public func writeBatch(vault: String? = nil, engrams: [WriteOptions]) async throws -> BatchWriteResponse {
        let v = vault ?? defaultVault
        let body = BatchWriteRequest(engrams: engrams)
        return try await _request("POST", path: "/api/engrams/batch", queryItems: [URLQueryItem(name: "vault", value: v)], body: body)
    }

    public func read(_ id: String, vault: String? = nil) async throws -> Engram {
        let v = vault ?? defaultVault
        return try await _request("GET", path: "/api/engrams/\(id)", queryItems: [URLQueryItem(name: "vault", value: v)])
    }

    public func forget(_ id: String, vault: String? = nil, hard: Bool = false) async throws {
        let v = vault ?? defaultVault
        // Server exposes DELETE /api/engrams/{id} for both soft and hard delete.
        // Pass hard=true as a query param; the server currently performs soft delete
        // regardless — hard delete support is tracked server-side.
        var queryItems = [URLQueryItem(name: "vault", value: v)]
        if hard {
            queryItems.append(URLQueryItem(name: "hard", value: "true"))
        }
        try await _requestVoid("DELETE", path: "/api/engrams/\(id)", queryItems: queryItems)
    }

    public func activate(_ options: ActivateOptions) async throws -> ActivateResponse {
        return try await _request("POST", path: "/api/activate", body: options)
    }

    public func link(_ options: LinkOptions) async throws {
        try await _requestVoid("POST", path: "/api/link", body: options)
    }

    public func traverse(_ options: TraverseOptions) async throws -> TraverseResponse {
        return try await _request("POST", path: "/api/traverse", body: options)
    }

    public func evolve(id: String, newContent: String, reason: String, vault: String? = nil) async throws -> EvolveResponse {
        let v = vault ?? defaultVault
        let body = EvolveRequest(newContent: newContent, reason: reason, vault: v)
        return try await _request("POST", path: "/api/engrams/\(id)/evolve", body: body)
    }

    public func consolidate(ids: [String], mergedContent: String, vault: String? = nil) async throws -> ConsolidateResponse {
        let opts = ConsolidateOptions(ids: ids, mergedContent: mergedContent, vault: vault ?? defaultVault)
        return try await _request("POST", path: "/api/consolidate", body: opts)
    }

    public func decide(_ options: DecideOptions) async throws -> DecideResponse {
        return try await _request("POST", path: "/api/decide", body: options)
    }

    public func restore(_ id: String, vault: String? = nil) async throws -> RestoreResponse {
        let v = vault ?? defaultVault
        return try await _request(
            "POST",
            path: "/api/engrams/\(id)/restore",
            queryItems: [URLQueryItem(name: "vault", value: v)]
        )
    }

    public func explain(_ options: ExplainOptions) async throws -> ExplainResponse {
        return try await _request("POST", path: "/api/explain", body: options)
    }

    public func setState(id: String, state: String, reason: String? = nil, vault: String? = nil) async throws -> SetStateResponse {
        let body = SetStateRequest(vault: vault ?? defaultVault, state: state, reason: reason)
        return try await _request("PUT", path: "/api/engrams/\(id)/state", body: body)
    }

    public func listDeleted(vault: String? = nil, limit: Int? = nil) async throws -> ListDeletedResponse {
        let v = vault ?? defaultVault
        var items = [URLQueryItem(name: "vault", value: v)]
        if let limit = limit {
            items.append(URLQueryItem(name: "limit", value: String(limit)))
        }
        return try await _request("GET", path: "/api/deleted", queryItems: items)
    }

    public func retryEnrich(_ id: String, vault: String? = nil) async throws -> RetryEnrichResponse {
        let v = vault ?? defaultVault
        return try await _request(
            "POST",
            path: "/api/engrams/\(id)/retry-enrich",
            queryItems: [URLQueryItem(name: "vault", value: v)]
        )
    }

    public func contradictions(vault: String? = nil) async throws -> ContradictionsResponse {
        let v = vault ?? defaultVault
        return try await _request("GET", path: "/api/contradictions", queryItems: [URLQueryItem(name: "vault", value: v)])
    }

    public func guide(vault: String? = nil) async throws -> String {
        let v = vault ?? defaultVault
        let resp: GuideResponse = try await _request("GET", path: "/api/guide", queryItems: [URLQueryItem(name: "vault", value: v)])
        return resp.guide
    }

    public func stats(vault: String? = nil) async throws -> StatsResponse {
        var items: [URLQueryItem]?
        if let v = vault {
            items = [URLQueryItem(name: "vault", value: v)]
        }
        return try await _request("GET", path: "/api/stats", queryItems: items)
    }

    public func listEngrams(vault: String? = nil, limit: Int? = nil, offset: Int? = nil) async throws -> ListEngramsResponse {
        let v = vault ?? defaultVault
        var items = [URLQueryItem(name: "vault", value: v)]
        if let limit = limit {
            items.append(URLQueryItem(name: "limit", value: String(limit)))
        }
        if let offset = offset {
            items.append(URLQueryItem(name: "offset", value: String(offset)))
        }
        return try await _request("GET", path: "/api/engrams", queryItems: items)
    }

    public func getLinks(_ id: String, vault: String? = nil) async throws -> [AssociationItem] {
        let v = vault ?? defaultVault
        let resp: LinksResponse = try await _request(
            "GET",
            path: "/api/engrams/\(id)/links",
            queryItems: [URLQueryItem(name: "vault", value: v)]
        )
        return resp.links
    }

    public func listVaults() async throws -> [String] {
        let resp: VaultsResponse = try await _request("GET", path: "/api/vaults")
        return resp.vaults
    }

    public func session(vault: String? = nil, since: String? = nil, limit: Int? = nil, offset: Int? = nil) async throws -> SessionResponse {
        let v = vault ?? defaultVault
        var items = [URLQueryItem(name: "vault", value: v)]
        if let since = since {
            items.append(URLQueryItem(name: "since", value: since))
        }
        if let limit = limit {
            items.append(URLQueryItem(name: "limit", value: String(limit)))
        }
        if let offset = offset {
            items.append(URLQueryItem(name: "offset", value: String(offset)))
        }
        return try await _request("GET", path: "/api/session", queryItems: items)
    }

    public func subscribe(vault: String? = nil, pushOnWrite: Bool = true, threshold: Double? = nil) -> AsyncThrowingStream<SseEvent, Error> {
        let v = vault ?? defaultVault
        var items = [
            URLQueryItem(name: "vault", value: v),
            URLQueryItem(name: "push_on_write", value: pushOnWrite ? "true" : "false"),
        ]
        if let threshold = threshold {
            items.append(URLQueryItem(name: "threshold", value: String(threshold)))
        }

        var components = URLComponents(url: baseURL.appendingPathComponent("/api/subscribe"), resolvingAgainstBaseURL: false)!
        components.queryItems = items

        var request = URLRequest(url: components.url!)
        request.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization")
        request.setValue("text/event-stream", forHTTPHeaderField: "Accept")
        request.timeoutInterval = 0 // SSE streams should not time out

        return makeSubscribeStream(session: session, request: request)
    }

    public func health() async throws -> HealthResponse {
        return try await _request("GET", path: "/api/health")
    }

    // MARK: - Private Helpers

    private func _request<T: Decodable>(
        _ method: String,
        path: String,
        queryItems: [URLQueryItem]? = nil,
        body: (any Encodable)? = nil
    ) async throws -> T {
        let data = try await _executeRequest(method, path: path, queryItems: queryItems, body: body)
        do {
            let decoder = JSONDecoder()
            return try decoder.decode(T.self, from: data)
        } catch {
            throw MuninnError.decodingFailed(error)
        }
    }

    private func _requestVoid(
        _ method: String,
        path: String,
        queryItems: [URLQueryItem]? = nil,
        body: (any Encodable)? = nil
    ) async throws {
        _ = try await _executeRequest(method, path: path, queryItems: queryItems, body: body)
    }

    private func _executeRequest(
        _ method: String,
        path: String,
        queryItems: [URLQueryItem]?,
        body: (any Encodable)?
    ) async throws -> Data {
        var components = URLComponents(url: baseURL.appendingPathComponent(path), resolvingAgainstBaseURL: false)!
        components.queryItems = queryItems

        guard let url = components.url else {
            throw MuninnError.validation("Invalid URL: \(path)")
        }

        var request = URLRequest(url: url)
        request.httpMethod = method
        request.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization")
        request.setValue("application/json", forHTTPHeaderField: "Content-Type")
        request.setValue("application/json", forHTTPHeaderField: "Accept")
        request.timeoutInterval = timeout

        if let body = body {
            let encoder = JSONEncoder()
            request.httpBody = try encoder.encode(body)
        }

        var lastError: Error?

        for attempt in 0..<maxRetries {
            do {
                let (data, response) = try await session.data(for: request)

                guard let httpResponse = response as? HTTPURLResponse else {
                    throw MuninnError.connectionFailed(
                        NSError(domain: "MuninnDB", code: -1, userInfo: [NSLocalizedDescriptionKey: "Invalid response"])
                    )
                }

                let statusCode = httpResponse.statusCode

                if (200..<300).contains(statusCode) {
                    return data
                }

                let errorMsg = _extractError(from: data)

                // Retry on 429 or 5xx
                if statusCode == 429 || statusCode >= 500 {
                    lastError = MuninnError.serverError(statusCode, errorMsg)
                    if attempt < maxRetries - 1 {
                        let delay = Double(1 << attempt) * 0.5 // 0.5s, 1s, 2s
                        try await Task.sleep(nanoseconds: UInt64(delay * 1_000_000_000))
                        continue
                    }
                }

                // Non-retryable errors
                switch statusCode {
                case 401: throw MuninnError.unauthorized(errorMsg)
                case 404: throw MuninnError.notFound(errorMsg)
                case 409: throw MuninnError.conflict(errorMsg)
                case 429: throw MuninnError.serverError(statusCode, errorMsg) // rate limited — retries exhausted
                case 400..<500: throw MuninnError.validation(errorMsg)
                default: throw MuninnError.serverError(statusCode, errorMsg)
                }

            } catch let error as MuninnError {
                throw error
            } catch let error as URLError where error.code == .timedOut {
                throw MuninnError.timeout
            } catch {
                if attempt < maxRetries - 1 {
                    lastError = error
                    let delay = Double(1 << attempt) * 0.5
                    try await Task.sleep(nanoseconds: UInt64(delay * 1_000_000_000))
                    continue
                }
                throw MuninnError.connectionFailed(error)
            }
        }

        throw lastError ?? MuninnError.connectionFailed(
            NSError(domain: "MuninnDB", code: -1, userInfo: [NSLocalizedDescriptionKey: "Max retries exceeded"])
        )
    }

    private func _extractError(from data: Data) -> String {
        if let errorBody = try? JSONDecoder().decode(ErrorBody.self, from: data),
           let msg = errorBody.error?.message {
            return msg
        }
        return String(data: data, encoding: .utf8) ?? "Unknown error"
    }
}
