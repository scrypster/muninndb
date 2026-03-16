package com.muninndb.client

import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.flow
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.Response

internal fun sseFlow(client: OkHttpClient, request: Request): Flow<SseEvent> = flow {
    val response: Response = client.newCall(request).execute()
    if (!response.isSuccessful) {
        response.close()
        throw MuninnException.ServerError(response.code, "SSE subscribe failed: ${response.code}")
    }
    val source = response.body?.source() ?: run {
        response.close()
        return@flow
    }
    try {
        var eventType = "message"
        var dataBuffer = StringBuilder()
        while (!source.exhausted()) {
            val line = source.readUtf8Line() ?: break
            when {
                line.startsWith("event: ") -> eventType = line.removePrefix("event: ")
                line.startsWith("data: ") -> dataBuffer.append(line.removePrefix("data: "))
                line.isEmpty() && dataBuffer.isNotEmpty() -> {
                    emit(SseEvent(type = eventType, rawData = dataBuffer.toString()))
                    dataBuffer = StringBuilder()
                    eventType = "message"
                }
            }
        }
    } finally {
        source.close()
        response.close()
    }
}
