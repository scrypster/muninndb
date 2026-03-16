package com.muninndb.client

import com.google.gson.Gson
import com.google.gson.GsonBuilder
import com.google.gson.JsonSyntaxException
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.withContext
import okhttp3.HttpUrl
import okhttp3.HttpUrl.Companion.toHttpUrl
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.RequestBody.Companion.toRequestBody
import java.io.IOException
import java.net.SocketTimeoutException
import java.util.concurrent.TimeUnit
import kotlin.math.pow

class MuninnClient(
    val baseUrl: String = "http://localhost:8476",
    val token: String = "",
    val timeoutMs: Long = 30_000L,
    val maxRetries: Int = 3,
    val defaultVault: String = "default",
    httpClient: OkHttpClient? = null
) {
    internal val gson: Gson = GsonBuilder().create()
    private val jsonMediaType = "application/json; charset=utf-8".toMediaType()

    internal val http: OkHttpClient = httpClient ?: OkHttpClient.Builder()
        .connectTimeout(timeoutMs, TimeUnit.MILLISECONDS)
        .readTimeout(timeoutMs, TimeUnit.MILLISECONDS)
        .writeTimeout(timeoutMs, TimeUnit.MILLISECONDS)
        .build()

    // ── Write ────────────────────────────────────────────────────────

    suspend fun write(options: WriteOptions): WriteResponse {
        return request("POST", "/api/engrams", body = options, responseType = WriteResponse::class.java)
    }

    suspend fun writeBatch(vault: String = defaultVault, engrams: List<WriteOptions>): BatchWriteResponse {
        val body = BatchWriteBody(engrams.map { it.copy(vault = it.vault ?: vault) })
        return request("POST", "/api/engrams/batch", body = body, responseType = BatchWriteResponse::class.java)
    }

    // ── Read ─────────────────────────────────────────────────────────

    suspend fun read(id: String, vault: String? = null): Engram {
        return request(
            "GET", "/api/engrams/$id",
            params = mapOf("vault" to (vault ?: defaultVault)),
            responseType = Engram::class.java
        )
    }

    // ── Forget ───────────────────────────────────────────────────────

    suspend fun forget(id: String, vault: String? = null, hard: Boolean = false) {
        val v = vault ?: defaultVault
        val params = mutableMapOf<String, String?>("vault" to v)
        if (hard) params["hard"] = "true"
        request("DELETE", "/api/engrams/$id", params = params, responseType = Unit::class.java)
    }

    // ── Activate ─────────────────────────────────────────────────────

    suspend fun activate(options: ActivateOptions): ActivateResponse {
        val opts = if (options.vault == null) options.copy(vault = defaultVault) else options
        return request("POST", "/api/activate", body = opts, responseType = ActivateResponse::class.java)
    }

    // ── Link ─────────────────────────────────────────────────────────

    suspend fun link(options: LinkOptions) {
        val opts = if (options.vault == null) options.copy(vault = defaultVault) else options
        request("POST", "/api/link", body = opts, responseType = Unit::class.java)
    }

    // ── Traverse ─────────────────────────────────────────────────────

    suspend fun traverse(options: TraverseOptions): TraverseResponse {
        val opts = if (options.vault == null) options.copy(vault = defaultVault) else options
        return request("POST", "/api/traverse", body = opts, responseType = TraverseResponse::class.java)
    }

    // ── Evolve ───────────────────────────────────────────────────────

    suspend fun evolve(id: String, newContent: String, reason: String, vault: String? = null): EvolveResponse {
        val body = EvolveBody(vault = vault ?: defaultVault, newContent = newContent, reason = reason)
        return request("POST", "/api/engrams/$id/evolve", body = body, responseType = EvolveResponse::class.java)
    }

    // ── Consolidate ──────────────────────────────────────────────────

    suspend fun consolidate(ids: List<String>, mergedContent: String, vault: String? = null): ConsolidateResponse {
        val body = ConsolidateOptions(vault = vault ?: defaultVault, ids = ids, mergedContent = mergedContent)
        return request("POST", "/api/consolidate", body = body, responseType = ConsolidateResponse::class.java)
    }

    // ── Decide ───────────────────────────────────────────────────────

    suspend fun decide(options: DecideOptions): DecideResponse {
        val opts = if (options.vault == null) options.copy(vault = defaultVault) else options
        return request("POST", "/api/decide", body = opts, responseType = DecideResponse::class.java)
    }

    // ── Restore ──────────────────────────────────────────────────────

    suspend fun restore(id: String, vault: String? = null): RestoreResponse {
        return request(
            "POST", "/api/engrams/$id/restore",
            params = mapOf("vault" to (vault ?: defaultVault)),
            responseType = RestoreResponse::class.java
        )
    }

    // ── Explain ──────────────────────────────────────────────────────

    suspend fun explain(options: ExplainOptions): ExplainResponse {
        val opts = if (options.vault == null) options.copy(vault = defaultVault) else options
        return request("POST", "/api/explain", body = opts, responseType = ExplainResponse::class.java)
    }

    // ── Set State ────────────────────────────────────────────────────

    suspend fun setState(id: String, state: String, reason: String? = null, vault: String? = null): SetStateResponse {
        val body = SetStateBody(vault = vault ?: defaultVault, state = state, reason = reason)
        return request("PUT", "/api/engrams/$id/state", body = body, responseType = SetStateResponse::class.java)
    }

    // ── List Deleted ─────────────────────────────────────────────────

    suspend fun listDeleted(vault: String? = null, limit: Int? = null): ListDeletedResponse {
        val params = mutableMapOf<String, String?>("vault" to (vault ?: defaultVault))
        if (limit != null) params["limit"] = limit.toString()
        return request("GET", "/api/deleted", params = params, responseType = ListDeletedResponse::class.java)
    }

    // ── Retry Enrich ─────────────────────────────────────────────────

    suspend fun retryEnrich(id: String, vault: String? = null): RetryEnrichResponse {
        return request(
            "POST", "/api/engrams/$id/retry-enrich",
            params = mapOf("vault" to (vault ?: defaultVault)),
            responseType = RetryEnrichResponse::class.java
        )
    }

    // ── Contradictions ───────────────────────────────────────────────

    suspend fun contradictions(vault: String? = null): ContradictionsResponse {
        return request(
            "GET", "/api/contradictions",
            params = mapOf("vault" to (vault ?: defaultVault)),
            responseType = ContradictionsResponse::class.java
        )
    }

    // ── Guide ────────────────────────────────────────────────────────

    suspend fun guide(vault: String? = null): String {
        val resp = request(
            "GET", "/api/guide",
            params = mapOf("vault" to (vault ?: defaultVault)),
            responseType = GuideResponseBody::class.java
        )
        return resp.guide
    }

    // ── Stats ────────────────────────────────────────────────────────

    suspend fun stats(vault: String? = null): StatsResponse {
        val params = mutableMapOf<String, String?>()
        if (vault != null) params["vault"] = vault
        return request("GET", "/api/stats", params = params, responseType = StatsResponse::class.java)
    }

    // ── List Engrams ─────────────────────────────────────────────────

    suspend fun listEngrams(vault: String? = null, limit: Int? = null, offset: Int? = null): ListEngramsResponse {
        val params = mutableMapOf<String, String?>("vault" to (vault ?: defaultVault))
        if (limit != null) params["limit"] = limit.toString()
        if (offset != null) params["offset"] = offset.toString()
        return request("GET", "/api/engrams", params = params, responseType = ListEngramsResponse::class.java)
    }

    // ── Get Links ────────────────────────────────────────────────────

    suspend fun getLinks(id: String, vault: String? = null): List<AssociationItem> {
        val resp = request(
            "GET", "/api/engrams/$id/links",
            params = mapOf("vault" to (vault ?: defaultVault)),
            responseType = GetLinksResponse::class.java
        )
        return resp.links
    }

    // ── List Vaults ──────────────────────────────────────────────────

    suspend fun listVaults(): List<String> {
        val resp = request("GET", "/api/vaults", responseType = VaultsResponse::class.java)
        return resp.vaults
    }

    // ── Session ──────────────────────────────────────────────────────

    suspend fun session(vault: String? = null, since: String? = null, limit: Int? = null, offset: Int? = null): SessionResponse {
        val params = mutableMapOf<String, String?>("vault" to (vault ?: defaultVault))
        if (since != null) params["since"] = since
        if (limit != null) params["limit"] = limit.toString()
        if (offset != null) params["offset"] = offset.toString()
        return request("GET", "/api/session", params = params, responseType = SessionResponse::class.java)
    }

    // ── Subscribe (SSE) ──────────────────────────────────────────────

    fun subscribe(vault: String? = null, pushOnWrite: Boolean = true, threshold: Double? = null): Flow<SseEvent> {
        val urlBuilder = (baseUrl + "/api/subscribe").toHttpUrl().newBuilder()
            .addQueryParameter("vault", vault ?: defaultVault)
            .addQueryParameter("push_on_write", pushOnWrite.toString())
        if (threshold != null) {
            urlBuilder.addQueryParameter("threshold", threshold.toString())
        }
        val request = Request.Builder()
            .url(urlBuilder.build())
            .apply { if (token.isNotEmpty()) addHeader("Authorization", "Bearer $token") }
            .addHeader("Accept", "text/event-stream")
            .build()
        return sseFlow(http, request)
    }

    // ── Health ───────────────────────────────────────────────────────

    suspend fun health(): HealthResponse {
        return request("GET", "/api/health", responseType = HealthResponse::class.java)
    }

    // ── Internal ─────────────────────────────────────────────────────

    private suspend fun <T> request(
        method: String,
        path: String,
        params: Map<String, String?> = emptyMap(),
        body: Any? = null,
        responseType: Class<T>
    ): T = withRetry {
        withContext(Dispatchers.IO) {
            val urlBuilder = (baseUrl + path).toHttpUrl().newBuilder()
            params.forEach { (k, v) -> if (v != null) urlBuilder.addQueryParameter(k, v) }
            val url = urlBuilder.build()

            val requestBody = if (body != null) {
                gson.toJson(body).toRequestBody(jsonMediaType)
            } else null

            val request = Request.Builder()
                .url(url)
                .method(method, requestBody ?: if (method == "POST" || method == "PUT") "".toRequestBody(null) else null)
                .apply { if (token.isNotEmpty()) addHeader("Authorization", "Bearer $token") }
                .addHeader("Accept", "application/json")
                .build()

            val response = http.newCall(request).execute()
            response.use { resp ->
                val responseBody = resp.body?.string() ?: ""
                if (!resp.isSuccessful) {
                    throw mapError(resp.code, responseBody)
                }
                if (responseType == Unit::class.java) {
                    @Suppress("UNCHECKED_CAST")
                    Unit as T
                } else {
                    try {
                        gson.fromJson(responseBody, responseType)
                            ?: throw MuninnException.DecodingFailed("Null response body")
                    } catch (e: JsonSyntaxException) {
                        throw MuninnException.DecodingFailed("Failed to decode response: ${e.message}", e)
                    }
                }
            }
        }
    }

    private suspend fun <T> withRetry(block: suspend () -> T): T {
        var attempt = 0
        while (true) {
            try {
                return block()
            } catch (e: MuninnException.ServerError) {
                if (attempt >= maxRetries) throw e
                delay((500L * 2.0.pow(attempt)).toLong())
                attempt++
            } catch (e: SocketTimeoutException) {
                if (attempt >= maxRetries) throw MuninnException.Timeout()
                delay((500L * 2.0.pow(attempt)).toLong())
                attempt++
            } catch (e: IOException) {
                if (attempt >= maxRetries) throw MuninnException.ConnectionFailed(e)
                delay((500L * 2.0.pow(attempt)).toLong())
                attempt++
            }
        }
    }

    private fun mapError(code: Int, body: String): MuninnException {
        val message = extractErrorMessage(body)
        return when (code) {
            400 -> MuninnException.Validation(message)
            401, 403 -> MuninnException.Unauthorized(message)
            404 -> MuninnException.NotFound(message)
            409 -> MuninnException.Conflict(message)
            429 -> MuninnException.ServerError(code, message)
            in 500..599 -> MuninnException.ServerError(code, message)
            else -> MuninnException.ServerError(code, message)
        }
    }

    private fun extractErrorMessage(body: String): String {
        return try {
            val env = gson.fromJson(body, ErrorEnvelope::class.java)
            env?.error?.message ?: body
        } catch (_: Exception) {
            body
        }
    }
}
