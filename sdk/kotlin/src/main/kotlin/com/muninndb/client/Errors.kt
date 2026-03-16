package com.muninndb.client

sealed class MuninnException(message: String, cause: Throwable? = null) : Exception(message, cause) {
    class NotFound(message: String) : MuninnException(message)
    class Unauthorized(message: String) : MuninnException(message)
    class Conflict(message: String) : MuninnException(message)
    class Validation(message: String) : MuninnException(message)
    class ServerError(val status: Int, message: String) : MuninnException(message)
    class ConnectionFailed(cause: Throwable) : MuninnException(cause.message ?: "Connection failed", cause)
    class Timeout : MuninnException("Request timed out")
    class DecodingFailed(message: String, cause: Throwable? = null) : MuninnException(message, cause)
}
