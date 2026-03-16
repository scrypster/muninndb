import Foundation

public enum MuninnError: Error, LocalizedError {
    case notFound(String)
    case unauthorized(String)
    case conflict(String)
    case validation(String)
    case serverError(Int, String)
    case connectionFailed(Error)
    case timeout
    case decodingFailed(Error)

    public var errorDescription: String? {
        switch self {
        case .notFound(let msg): return "Not found: \(msg)"
        case .unauthorized(let msg): return "Unauthorized: \(msg)"
        case .conflict(let msg): return "Conflict: \(msg)"
        case .validation(let msg): return "Validation error: \(msg)"
        case .serverError(let code, let msg): return "Server error \(code): \(msg)"
        case .connectionFailed(let err): return "Connection failed: \(err.localizedDescription)"
        case .timeout: return "Request timed out"
        case .decodingFailed(let err): return "Decoding failed: \(err.localizedDescription)"
        }
    }
}
