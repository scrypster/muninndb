import Foundation

public struct SseEvent: Sendable {
    public let type: String
    public let rawData: String

    public init(type: String, rawData: String) {
        self.type = type
        self.rawData = rawData
    }
}

public func makeSubscribeStream(
    session: URLSession,
    request: URLRequest
) -> AsyncThrowingStream<SseEvent, Error> {
    AsyncThrowingStream { continuation in
        let task = Task {
            do {
                let (bytes, response) = try await session.bytes(for: request)

                if let httpResponse = response as? HTTPURLResponse,
                   httpResponse.statusCode != 200 {
                    continuation.finish(
                        throwing: MuninnError.serverError(
                            httpResponse.statusCode, "SSE connection failed"
                        )
                    )
                    return
                }

                var eventType = "message"
                var dataLines: [String] = []

                for try await line in bytes.lines {
                    if line.isEmpty {
                        // Empty line = end of event
                        if !dataLines.isEmpty {
                            let rawData = dataLines.joined(separator: "\n")
                            let event = SseEvent(type: eventType, rawData: rawData)
                            continuation.yield(event)
                            dataLines.removeAll()
                            eventType = "message"
                        }
                        continue
                    }

                    if line.hasPrefix("event:") {
                        eventType = String(line.dropFirst(6)).trimmingCharacters(in: .whitespaces)
                    } else if line.hasPrefix("data:") {
                        let data = String(line.dropFirst(5)).trimmingCharacters(in: .whitespaces)
                        dataLines.append(data)
                    }
                    // Ignore comments (lines starting with ':') and other fields
                }

                // Yield any remaining event
                if !dataLines.isEmpty {
                    let rawData = dataLines.joined(separator: "\n")
                    continuation.yield(SseEvent(type: eventType, rawData: rawData))
                }

                continuation.finish()
            } catch {
                continuation.finish(throwing: error)
            }
        }

        continuation.onTermination = { _ in
            task.cancel()
        }
    }
}
