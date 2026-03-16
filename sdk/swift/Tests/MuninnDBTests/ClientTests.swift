import XCTest
@testable import MuninnDB

final class MockURLProtocol: URLProtocol {
    static var requestHandler: ((URLRequest) throws -> (HTTPURLResponse, Data))?

    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }

    override func startLoading() {
        guard let handler = MockURLProtocol.requestHandler else {
            client?.urlProtocol(self, didFailWithError: NSError(domain: "MockURLProtocol", code: -1))
            return
        }
        do {
            let (response, data) = try handler(request)
            client?.urlProtocol(self, didReceive: response, cacheStoragePolicy: .notAllowed)
            client?.urlProtocol(self, didLoad: data)
            client?.urlProtocolDidFinishLoading(self)
        } catch {
            client?.urlProtocol(self, didFailWithError: error)
        }
    }

    override func stopLoading() {}
}

extension URLRequest {
    /// Read body bytes from either httpBody or httpBodyStream (URLProtocol may convert httpBody to a stream).
    var bodyData: Data? {
        if let body = httpBody { return body }
        guard let stream = httpBodyStream else { return nil }
        stream.open()
        defer { stream.close() }
        var data = Data()
        let buf = UnsafeMutablePointer<UInt8>.allocate(capacity: 4096)
        defer { buf.deallocate() }
        while stream.hasBytesAvailable {
            let read = stream.read(buf, maxLength: 4096)
            if read <= 0 { break }
            data.append(buf, count: read)
        }
        return data
    }

    func bodyJSON() -> [String: Any]? {
        guard let data = bodyData else { return nil }
        return try? JSONSerialization.jsonObject(with: data) as? [String: Any]
    }
}

final class ClientTests: XCTestCase {
    var client: MuninnClient!

    override func setUp() {
        super.setUp()
        let config = URLSessionConfiguration.ephemeral
        config.protocolClasses = [MockURLProtocol.self]
        let session = URLSession(configuration: config)
        client = MuninnClient(
            baseURL: URL(string: "http://localhost:8476")!,
            token: "test-token",
            timeout: 5,
            maxRetries: 1,
            session: session
        )
    }

    override func tearDown() {
        MockURLProtocol.requestHandler = nil
        super.tearDown()
    }

    // MARK: - Helpers

    private func mockResponse(
        statusCode: Int = 200,
        json: [String: Any]
    ) -> (HTTPURLResponse, Data) {
        let data = try! JSONSerialization.data(withJSONObject: json)
        let response = HTTPURLResponse(
            url: URL(string: "http://localhost:8476")!,
            statusCode: statusCode,
            httpVersion: nil,
            headerFields: nil
        )!
        return (response, data)
    }

    private func mockResponse(
        statusCode: Int = 200,
        data: Data
    ) -> (HTTPURLResponse, Data) {
        let response = HTTPURLResponse(
            url: URL(string: "http://localhost:8476")!,
            statusCode: statusCode,
            httpVersion: nil,
            headerFields: nil
        )!
        return (response, data)
    }

    // MARK: - Write

    func testWrite() async throws {
        MockURLProtocol.requestHandler = { request in
            XCTAssertEqual(request.httpMethod, "POST")
            XCTAssertTrue(request.url!.path.hasSuffix("/api/engrams"))
            XCTAssertEqual(request.value(forHTTPHeaderField: "Authorization"), "Bearer test-token")

            let body = request.bodyJSON()!
            XCTAssertEqual(body["concept"] as? String, "test concept")
            XCTAssertEqual(body["content"] as? String, "test content")

            return self.mockResponse(json: ["id": "eng-123", "created_at": 1700000000])
        }

        let result = try await client.write(WriteOptions(concept: "test concept", content: "test content", vault: "default"))
        XCTAssertEqual(result.id, "eng-123")
        XCTAssertEqual(result.createdAt, 1700000000)
    }

    // MARK: - Write Batch

    func testWriteBatch() async throws {
        MockURLProtocol.requestHandler = { request in
            XCTAssertEqual(request.httpMethod, "POST")
            XCTAssertTrue(request.url!.path.hasSuffix("/api/engrams/batch"))

            return self.mockResponse(json: [
                "results": [
                    ["id": "eng-1", "created_at": 1700000000],
                    ["id": "eng-2", "created_at": 1700000001],
                ],
            ])
        }

        let engrams = [
            WriteOptions(concept: "c1", content: "content1"),
            WriteOptions(concept: "c2", content: "content2"),
        ]
        let result = try await client.writeBatch(vault: "default", engrams: engrams)
        XCTAssertEqual(result.results.count, 2)
        XCTAssertEqual(result.results[0].id, "eng-1")
        XCTAssertEqual(result.results[1].id, "eng-2")
    }

    // MARK: - Read

    func testRead() async throws {
        MockURLProtocol.requestHandler = { request in
            XCTAssertEqual(request.httpMethod, "GET")
            XCTAssertTrue(request.url!.path.hasSuffix("/api/engrams/eng-123"))
            XCTAssertTrue(request.url!.query!.contains("vault=default"))

            return self.mockResponse(json: [
                "id": "eng-123",
                "concept": "test",
                "content": "content",
                "confidence": 0.9,
                "relevance": 0.8,
                "stability": 0.7,
                "access_count": 5,
                "state": 0,
                "created_at": 1700000000,
                "updated_at": 1700000001,
                "last_access": 1700000002,
                "memory_type": 1,
            ])
        }

        let engram = try await client.read("eng-123")
        XCTAssertEqual(engram.id, "eng-123")
        XCTAssertEqual(engram.concept, "test")
        XCTAssertEqual(engram.confidence, 0.9)
        XCTAssertEqual(engram.accessCount, 5)
        XCTAssertEqual(engram.memoryType, 1)
    }

    // MARK: - Forget (soft)

    func testForgetSoft() async throws {
        MockURLProtocol.requestHandler = { request in
            XCTAssertEqual(request.httpMethod, "DELETE")
            XCTAssertTrue(request.url!.path.hasSuffix("/api/engrams/eng-123"))

            return self.mockResponse(json: ["ok": true])
        }

        try await client.forget("eng-123")
    }

    // MARK: - Forget (hard)

    func testForgetHard() async throws {
        MockURLProtocol.requestHandler = { request in
            XCTAssertEqual(request.httpMethod, "DELETE")
            XCTAssertTrue(request.url!.path.hasSuffix("/api/engrams/eng-123"))
            XCTAssertTrue(request.url!.query!.contains("hard=true"))

            return self.mockResponse(json: ["ok": true])
        }

        try await client.forget("eng-123", hard: true)
    }

    // MARK: - Activate

    func testActivate() async throws {
        MockURLProtocol.requestHandler = { request in
            XCTAssertEqual(request.httpMethod, "POST")
            XCTAssertTrue(request.url!.path.hasSuffix("/api/activate"))

            let body = request.bodyJSON()!
            XCTAssertEqual(body["context"] as? [String], ["hello world"])
            XCTAssertEqual(body["max_results"] as? Int, 5)

            return self.mockResponse(json: [
                "query_id": "q-1",
                "total_found": 1,
                "activations": [
                    [
                        "id": "eng-1",
                        "concept": "greeting",
                        "content": "hello",
                        "score": 0.95,
                        "confidence": 0.9,
                    ],
                ],
            ])
        }

        let result = try await client.activate(ActivateOptions(context: ["hello world"], maxResults: 5))
        XCTAssertEqual(result.queryId, "q-1")
        XCTAssertEqual(result.totalFound, 1)
        XCTAssertEqual(result.activations.count, 1)
        XCTAssertEqual(result.activations[0].score, 0.95)
    }

    // MARK: - Link

    func testLink() async throws {
        MockURLProtocol.requestHandler = { request in
            XCTAssertEqual(request.httpMethod, "POST")
            XCTAssertTrue(request.url!.path.hasSuffix("/api/link"))

            let body = request.bodyJSON()!
            XCTAssertEqual(body["source_id"] as? String, "eng-1")
            XCTAssertEqual(body["target_id"] as? String, "eng-2")
            XCTAssertEqual(body["rel_type"] as? Int, 1)

            return self.mockResponse(json: ["ok": true])
        }

        try await client.link(LinkOptions(sourceId: "eng-1", targetId: "eng-2", relType: 1))
    }

    // MARK: - Traverse

    func testTraverse() async throws {
        MockURLProtocol.requestHandler = { request in
            XCTAssertEqual(request.httpMethod, "POST")
            XCTAssertTrue(request.url!.path.hasSuffix("/api/traverse"))

            return self.mockResponse(json: [
                "nodes": [
                    ["id": "eng-1", "concept": "root", "hop_dist": 0],
                    ["id": "eng-2", "concept": "child", "hop_dist": 1],
                ],
                "edges": [
                    ["from_id": "eng-1", "to_id": "eng-2", "rel_type": "related", "weight": 0.8],
                ],
                "total_reachable": 2,
                "query_ms": 1.5,
            ])
        }

        let result = try await client.traverse(TraverseOptions(startId: "eng-1", maxHops: 2))
        XCTAssertEqual(result.nodes.count, 2)
        XCTAssertEqual(result.edges.count, 1)
        XCTAssertEqual(result.totalReachable, 2)
        XCTAssertEqual(result.edges[0].weight, 0.8)
    }

    // MARK: - Evolve

    func testEvolve() async throws {
        MockURLProtocol.requestHandler = { request in
            XCTAssertEqual(request.httpMethod, "POST")
            XCTAssertTrue(request.url!.path.contains("/evolve"))

            return self.mockResponse(json: ["id": "eng-123"])
        }

        let result = try await client.evolve(id: "eng-123", newContent: "updated", reason: "correction")
        XCTAssertEqual(result.id, "eng-123")
    }

    // MARK: - Consolidate

    func testConsolidate() async throws {
        MockURLProtocol.requestHandler = { request in
            XCTAssertEqual(request.httpMethod, "POST")
            XCTAssertTrue(request.url!.path.hasSuffix("/api/consolidate"))

            return self.mockResponse(json: [
                "id": "eng-new",
                "archived": ["eng-1", "eng-2"],
            ])
        }

        let result = try await client.consolidate(ids: ["eng-1", "eng-2"], mergedContent: "merged")
        XCTAssertEqual(result.id, "eng-new")
        XCTAssertEqual(result.archived, ["eng-1", "eng-2"])
    }

    // MARK: - Decide

    func testDecide() async throws {
        MockURLProtocol.requestHandler = { request in
            XCTAssertEqual(request.httpMethod, "POST")
            XCTAssertTrue(request.url!.path.hasSuffix("/api/decide"))

            return self.mockResponse(json: ["id": "dec-1"])
        }

        let result = try await client.decide(DecideOptions(decision: "use Swift", rationale: "type safety"))
        XCTAssertEqual(result.id, "dec-1")
    }

    // MARK: - Restore

    func testRestore() async throws {
        MockURLProtocol.requestHandler = { request in
            XCTAssertEqual(request.httpMethod, "POST")
            XCTAssertTrue(request.url!.path.contains("/restore"))

            return self.mockResponse(json: [
                "id": "eng-1",
                "concept": "restored concept",
                "restored": true,
                "state": "active",
            ])
        }

        let result = try await client.restore("eng-1")
        XCTAssertEqual(result.id, "eng-1")
        XCTAssertTrue(result.restored)
        XCTAssertEqual(result.state, "active")
    }

    // MARK: - Explain

    func testExplain() async throws {
        MockURLProtocol.requestHandler = { request in
            XCTAssertEqual(request.httpMethod, "POST")
            XCTAssertTrue(request.url!.path.hasSuffix("/api/explain"))

            return self.mockResponse(json: [
                "engram_id": "eng-1",
                "concept": "test",
                "final_score": 0.85,
                "components": [
                    "full_text_relevance": 0.7,
                    "semantic_similarity": 0.8,
                    "decay_factor": 0.9,
                    "hebbian_boost": 0.1,
                    "access_frequency": 0.5,
                    "confidence": 0.9,
                ],
                "fts_matches": ["test"],
                "assoc_path": [],
                "would_return": true,
                "threshold": 0.1,
            ])
        }

        let result = try await client.explain(ExplainOptions(engramId: "eng-1", query: ["test"]))
        XCTAssertEqual(result.engramId, "eng-1")
        XCTAssertEqual(result.finalScore, 0.85)
        XCTAssertEqual(result.components.fullTextRelevance, 0.7)
        XCTAssertTrue(result.wouldReturn)
    }

    // MARK: - Set State

    func testSetState() async throws {
        MockURLProtocol.requestHandler = { request in
            XCTAssertEqual(request.httpMethod, "PUT")
            XCTAssertTrue(request.url!.path.contains("/state"))

            return self.mockResponse(json: [
                "id": "eng-1",
                "state": "dormant",
                "updated": true,
            ])
        }

        let result = try await client.setState(id: "eng-1", state: "dormant", reason: "no longer relevant")
        XCTAssertEqual(result.id, "eng-1")
        XCTAssertEqual(result.state, "dormant")
        XCTAssertTrue(result.updated)
    }

    // MARK: - List Deleted

    func testListDeleted() async throws {
        MockURLProtocol.requestHandler = { request in
            XCTAssertEqual(request.httpMethod, "GET")
            XCTAssertTrue(request.url!.path.hasSuffix("/api/deleted"))

            return self.mockResponse(json: [
                "deleted": [
                    [
                        "id": "eng-del-1",
                        "concept": "old",
                        "deleted_at": 1700000000,
                        "recoverable_until": 1700100000,
                    ],
                ],
                "count": 1,
            ])
        }

        let result = try await client.listDeleted()
        XCTAssertEqual(result.count, 1)
        XCTAssertEqual(result.deleted[0].id, "eng-del-1")
    }

    // MARK: - Retry Enrich

    func testRetryEnrich() async throws {
        MockURLProtocol.requestHandler = { request in
            XCTAssertEqual(request.httpMethod, "POST")
            XCTAssertTrue(request.url!.path.contains("/retry-enrich"))

            return self.mockResponse(json: [
                "engram_id": "eng-1",
                "plugins_queued": ["summarize"],
                "already_complete": ["embed"],
            ])
        }

        let result = try await client.retryEnrich("eng-1")
        XCTAssertEqual(result.engramId, "eng-1")
        XCTAssertEqual(result.pluginsQueued, ["summarize"])
        XCTAssertEqual(result.alreadyComplete, ["embed"])
    }

    // MARK: - Contradictions

    func testContradictions() async throws {
        MockURLProtocol.requestHandler = { request in
            XCTAssertEqual(request.httpMethod, "GET")
            XCTAssertTrue(request.url!.path.hasSuffix("/api/contradictions"))

            return self.mockResponse(json: [
                "contradictions": [
                    [
                        "id_a": "eng-1",
                        "concept_a": "sky is blue",
                        "id_b": "eng-2",
                        "concept_b": "sky is green",
                        "detected_at": 1700000000,
                    ],
                ],
            ])
        }

        let result = try await client.contradictions()
        XCTAssertEqual(result.contradictions.count, 1)
        XCTAssertEqual(result.contradictions[0].idA, "eng-1")
    }

    // MARK: - Guide

    func testGuide() async throws {
        MockURLProtocol.requestHandler = { request in
            XCTAssertEqual(request.httpMethod, "GET")
            XCTAssertTrue(request.url!.path.hasSuffix("/api/guide"))

            return self.mockResponse(json: ["guide": "Welcome to MuninnDB"])
        }

        let result = try await client.guide()
        XCTAssertEqual(result, "Welcome to MuninnDB")
    }

    // MARK: - Stats

    func testStats() async throws {
        MockURLProtocol.requestHandler = { request in
            XCTAssertEqual(request.httpMethod, "GET")
            XCTAssertTrue(request.url!.path.hasSuffix("/api/stats"))

            return self.mockResponse(json: [
                "engram_count": 100,
                "vault_count": 2,
                "index_size": 4096,
                "storage_bytes": 1048576,
            ])
        }

        let result = try await client.stats()
        XCTAssertEqual(result.engramCount, 100)
        XCTAssertEqual(result.vaultCount, 2)
        XCTAssertEqual(result.storageBytes, 1048576)
    }

    // MARK: - List Engrams

    func testListEngrams() async throws {
        MockURLProtocol.requestHandler = { request in
            XCTAssertEqual(request.httpMethod, "GET")
            XCTAssertTrue(request.url!.path.hasSuffix("/api/engrams"))
            XCTAssertTrue(request.url!.query!.contains("limit=10"))

            return self.mockResponse(json: [
                "engrams": [
                    [
                        "id": "eng-1",
                        "concept": "test",
                        "content": "content",
                        "confidence": 0.9,
                        "vault": "default",
                        "created_at": 1700000000,
                    ],
                ],
                "total": 1,
                "limit": 10,
                "offset": 0,
            ])
        }

        let result = try await client.listEngrams(limit: 10)
        XCTAssertEqual(result.engrams.count, 1)
        XCTAssertEqual(result.total, 1)
        XCTAssertEqual(result.engrams[0].id, "eng-1")
    }

    // MARK: - Get Links

    func testGetLinks() async throws {
        MockURLProtocol.requestHandler = { request in
            XCTAssertEqual(request.httpMethod, "GET")
            XCTAssertTrue(request.url!.path.contains("/links"))

            return self.mockResponse(json: [
                "links": [
                    [
                        "target_id": "eng-2",
                        "rel_type": 1,
                        "weight": 0.8,
                        "co_activation_count": 3,
                    ],
                ],
            ])
        }

        let result = try await client.getLinks("eng-1")
        XCTAssertEqual(result.count, 1)
        XCTAssertEqual(result[0].targetId, "eng-2")
        XCTAssertEqual(result[0].coActivationCount, 3)
    }

    // MARK: - List Vaults

    func testListVaults() async throws {
        MockURLProtocol.requestHandler = { request in
            XCTAssertEqual(request.httpMethod, "GET")
            XCTAssertTrue(request.url!.path.hasSuffix("/api/vaults"))

            return self.mockResponse(json: ["vaults": ["default", "archive"]])
        }

        let result = try await client.listVaults()
        XCTAssertEqual(result, ["default", "archive"])
    }

    // MARK: - Session

    func testSession() async throws {
        MockURLProtocol.requestHandler = { request in
            XCTAssertEqual(request.httpMethod, "GET")
            XCTAssertTrue(request.url!.path.hasSuffix("/api/session"))

            return self.mockResponse(json: [
                "entries": [
                    [
                        "id": "eng-1",
                        "concept": "recent item",
                        "content": "some content",
                        "created_at": 1700000000,
                    ],
                ],
                "total": 1,
                "offset": 0,
                "limit": 50,
            ])
        }

        let result = try await client.session()
        XCTAssertEqual(result.entries.count, 1)
        XCTAssertEqual(result.entries[0].concept, "recent item")
        XCTAssertEqual(result.total, 1)
    }

    // MARK: - Health

    func testHealth() async throws {
        MockURLProtocol.requestHandler = { request in
            XCTAssertEqual(request.httpMethod, "GET")
            XCTAssertTrue(request.url!.path.hasSuffix("/api/health"))

            return self.mockResponse(json: [
                "status": "ok",
                "version": "1.0.0",
                "uptime_seconds": 3600,
                "db_writable": true,
            ])
        }

        let result = try await client.health()
        XCTAssertEqual(result.status, "ok")
        XCTAssertEqual(result.version, "1.0.0")
        XCTAssertEqual(result.uptimeSeconds, 3600)
        XCTAssertTrue(result.dbWritable)
    }

    // MARK: - Error Mapping

    // Builds the nested {"error": {"code": ..., "message": "..."}} format the server actually sends.
    private func errorJSON(code: Int, message: String) -> [String: Any] {
        ["error": ["code": code, "message": message]]
    }

    func testUnauthorizedError() async throws {
        MockURLProtocol.requestHandler = { _ in
            return self.mockResponse(statusCode: 401, json: self.errorJSON(code: 1010, message: "invalid token"))
        }

        do {
            _ = try await client.health()
            XCTFail("Expected unauthorized error")
        } catch let error as MuninnError {
            if case .unauthorized(let msg) = error {
                XCTAssertEqual(msg, "invalid token")
            } else {
                XCTFail("Expected unauthorized, got \(error)")
            }
        }
    }

    func testNotFoundError() async throws {
        MockURLProtocol.requestHandler = { _ in
            return self.mockResponse(statusCode: 404, json: self.errorJSON(code: 1000, message: "engram not found"))
        }

        do {
            _ = try await client.read("nonexistent")
            XCTFail("Expected not found error")
        } catch let error as MuninnError {
            if case .notFound(let msg) = error {
                XCTAssertEqual(msg, "engram not found")
            } else {
                XCTFail("Expected notFound, got \(error)")
            }
        }
    }

    func testConflictError() async throws {
        MockURLProtocol.requestHandler = { _ in
            return self.mockResponse(statusCode: 409, json: self.errorJSON(code: 1004, message: "already exists"))
        }

        do {
            try await client.link(LinkOptions(sourceId: "a", targetId: "b", relType: 1))
            XCTFail("Expected conflict error")
        } catch let error as MuninnError {
            if case .conflict(let msg) = error {
                XCTAssertEqual(msg, "already exists")
            } else {
                XCTFail("Expected conflict, got \(error)")
            }
        }
    }

    func testValidationError() async throws {
        MockURLProtocol.requestHandler = { _ in
            return self.mockResponse(statusCode: 422, json: self.errorJSON(code: 1002, message: "missing field"))
        }

        do {
            _ = try await client.write(WriteOptions(concept: "", content: ""))
            XCTFail("Expected validation error")
        } catch let error as MuninnError {
            if case .validation(let msg) = error {
                XCTAssertEqual(msg, "missing field")
            } else {
                XCTFail("Expected validation, got \(error)")
            }
        }
    }

    func testServerError() async throws {
        MockURLProtocol.requestHandler = { _ in
            return self.mockResponse(statusCode: 500, json: self.errorJSON(code: 1020, message: "internal error"))
        }

        do {
            _ = try await client.health()
            XCTFail("Expected server error")
        } catch let error as MuninnError {
            if case .serverError(let code, let msg) = error {
                XCTAssertEqual(code, 500)
                XCTAssertEqual(msg, "internal error")
            } else {
                XCTFail("Expected serverError, got \(error)")
            }
        }
    }

    func testRateLimitedMapsToServerError() async throws {
        // 429 should map to .serverError (not .validation) after retries exhausted
        MockURLProtocol.requestHandler = { _ in
            return self.mockResponse(statusCode: 429, json: self.errorJSON(code: 1016, message: "rate limited"))
        }

        do {
            _ = try await client.health()
            XCTFail("Expected server error for 429")
        } catch let error as MuninnError {
            if case .serverError(let code, _) = error {
                XCTAssertEqual(code, 429)
            } else {
                XCTFail("Expected serverError for 429, got \(error)")
            }
        }
    }

    // MARK: - SSE Subscribe

    func testSubscribeRequestShape() async throws {
        // Deliver a minimal valid SSE response so the stream completes
        let sseBody = "data: {\"type\":\"write\",\"id\":\"eng-1\"}\n\n"
        MockURLProtocol.requestHandler = { request in
            XCTAssertEqual(request.httpMethod, "GET")
            XCTAssertTrue(request.url!.path.hasSuffix("/api/subscribe"))
            XCTAssertTrue(request.url!.query!.contains("vault=myVault"))
            XCTAssertTrue(request.url!.query!.contains("push_on_write=true"))
            XCTAssertEqual(request.value(forHTTPHeaderField: "Authorization"), "Bearer test-token")
            XCTAssertEqual(request.value(forHTTPHeaderField: "Accept"), "text/event-stream")
            let data = sseBody.data(using: .utf8)!
            return self.mockResponse(statusCode: 200, data: data)
        }

        let stream = client.subscribe(vault: "myVault", pushOnWrite: true)
        var events: [SseEvent] = []
        for try await event in stream {
            events.append(event)
        }
        XCTAssertEqual(events.count, 1)
        XCTAssertEqual(events[0].type, "message")
        XCTAssertTrue(events[0].rawData.contains("eng-1"))
    }

    func testSubscribeWithThreshold() async throws {
        MockURLProtocol.requestHandler = { request in
            XCTAssertTrue(request.url!.query!.contains("threshold=0.5"))
            return self.mockResponse(statusCode: 200, data: Data())
        }

        let stream = client.subscribe(vault: "default", threshold: 0.5)
        // Just consuming to trigger the request; empty body → stream ends immediately
        for try await _ in stream {}
    }

    // MARK: - Auth Header

    func testAuthorizationHeader() async throws {
        MockURLProtocol.requestHandler = { request in
            XCTAssertEqual(request.value(forHTTPHeaderField: "Authorization"), "Bearer test-token")
            XCTAssertEqual(request.value(forHTTPHeaderField: "Content-Type"), "application/json")

            return self.mockResponse(json: [
                "status": "ok",
                "version": "1.0.0",
                "uptime_seconds": 0,
                "db_writable": true,
            ])
        }

        _ = try await client.health()
    }
}
