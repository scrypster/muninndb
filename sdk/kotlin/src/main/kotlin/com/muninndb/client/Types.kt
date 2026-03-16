package com.muninndb.client

import com.google.gson.annotations.SerializedName

data class WriteOptions(
    val concept: String,
    val content: String,
    val vault: String? = null,
    val tags: List<String>? = null,
    val confidence: Double? = null,
    val stability: Double? = null,
    @SerializedName("memory_type") val memoryType: Int? = null,
    @SerializedName("type_label") val typeLabel: String? = null,
    val summary: String? = null,
    val entities: List<InlineEntity>? = null,
    val relationships: List<InlineRelationship>? = null
)

data class InlineEntity(val name: String, val type: String)

data class InlineRelationship(
    @SerializedName("target_id") val targetId: String,
    val relation: String,
    val weight: Double
)

data class WriteResponse(
    val id: String,
    @SerializedName("created_at") val createdAt: Long
)

data class BatchWriteResult(
    val index: Int,
    val id: String?,
    val status: String,
    val error: String?
)

data class BatchWriteBody(val engrams: List<WriteOptions>)

data class BatchWriteResponse(val results: List<BatchWriteResult>)

data class Engram(
    val id: String,
    val concept: String,
    val content: String,
    val confidence: Double,
    val relevance: Double,
    val stability: Double,
    @SerializedName("access_count") val accessCount: Int,
    val tags: List<String>?,
    val state: Int,
    @SerializedName("created_at") val createdAt: Long,
    @SerializedName("updated_at") val updatedAt: Long,
    @SerializedName("last_access") val lastAccess: Long,
    val summary: String?,
    @SerializedName("key_points") val keyPoints: List<String>?,
    @SerializedName("memory_type") val memoryType: Int,
    @SerializedName("type_label") val typeLabel: String?,
    @SerializedName("embed_dim") val embedDim: Int?
)

data class Weights(
    @SerializedName("semantic_similarity") val semanticSimilarity: Double? = null,
    @SerializedName("full_text_relevance") val fullTextRelevance: Double? = null,
    @SerializedName("decay_factor") val decayFactor: Double? = null,
    @SerializedName("hebbian_boost") val hebbianBoost: Double? = null,
    @SerializedName("access_frequency") val accessFrequency: Double? = null,
    val recency: Double? = null
)

// Filter value can be String, Int, Double, Boolean, or List<String>.
// These are send-only (never deserialized), so Any serializes correctly via Gson reflection.
data class Filter(
    val field: String,
    val op: String,
    val value: Any
)

data class ActivateOptions(
    val context: List<String>,
    val vault: String? = null,
    @SerializedName("max_results") val maxResults: Int? = null,
    val threshold: Double? = null,
    @SerializedName("max_hops") val maxHops: Int? = null,
    @SerializedName("include_why") val includeWhy: Boolean? = null,
    @SerializedName("brief_mode") val briefMode: String? = null,
    @SerializedName("disable_hops") val disableHops: Boolean? = null,
    val weights: Weights? = null,
    val filters: List<Filter>? = null
)

data class ScoreComponents(
    @SerializedName("semantic_similarity") val semanticSimilarity: Double,
    @SerializedName("full_text_relevance") val fullTextRelevance: Double,
    @SerializedName("decay_factor") val decayFactor: Double,
    @SerializedName("hebbian_boost") val hebbianBoost: Double,
    @SerializedName("access_frequency") val accessFrequency: Double,
    val recency: Double,
    val raw: Double,
    @SerializedName("final") val finalScore: Double
)

data class ActivationItem(
    val id: String,
    val concept: String,
    val content: String,
    val summary: String?,
    val score: Double,
    val confidence: Double,
    val why: String?,
    @SerializedName("hop_path") val hopPath: List<String>?,
    val dormant: Boolean?,
    @SerializedName("created_at") val createdAt: Long?,
    @SerializedName("last_access") val lastAccess: Long?,
    @SerializedName("access_count") val accessCount: Int?,
    val relevance: Double?
)

data class BriefSentence(
    @SerializedName("engram_id") val engramId: String,
    val text: String,
    val score: Double
)

data class ActivateResponse(
    @SerializedName("query_id") val queryId: String,
    @SerializedName("total_found") val totalFound: Int,
    val activations: List<ActivationItem>,
    @SerializedName("latency_ms") val latencyMs: Double?,
    val brief: List<BriefSentence>?
)

data class LinkOptions(
    @SerializedName("source_id") val sourceId: String,
    @SerializedName("target_id") val targetId: String,
    @SerializedName("rel_type") val relType: Int,
    val weight: Double? = null,
    val vault: String? = null
)

data class TraverseOptions(
    val vault: String? = null,
    @SerializedName("start_id") val startId: String,
    @SerializedName("max_hops") val maxHops: Int? = null,
    @SerializedName("max_nodes") val maxNodes: Int? = null,
    @SerializedName("rel_types") val relTypes: List<String>? = null,
    @SerializedName("follow_entities") val followEntities: Boolean? = null
)

data class TraversalNode(
    val id: String,
    val concept: String,
    @SerializedName("hop_dist") val hopDist: Int,
    val summary: String?
)

data class TraversalEdge(
    @SerializedName("from_id") val fromId: String,
    @SerializedName("to_id") val toId: String,
    @SerializedName("rel_type") val relType: String,
    val weight: Double
)

data class TraverseResponse(
    val nodes: List<TraversalNode>,
    val edges: List<TraversalEdge>,
    @SerializedName("total_reachable") val totalReachable: Int,
    @SerializedName("query_ms") val queryMs: Double
)

data class EvolveBody(
    val vault: String?,
    @SerializedName("new_content") val newContent: String,
    val reason: String
)

data class EvolveResponse(val id: String)

data class ConsolidateOptions(
    val vault: String?,
    val ids: List<String>,
    @SerializedName("merged_content") val mergedContent: String
)

data class ConsolidateResponse(
    val id: String,
    val archived: List<String>,
    val warnings: List<String>?
)

data class DecideOptions(
    val vault: String? = null,
    val decision: String,
    val rationale: String,
    val alternatives: List<String>? = null,
    @SerializedName("evidence_ids") val evidenceIds: List<String>? = null
)

data class DecideResponse(
    val id: String,
    val warnings: List<String>?
)

data class RestoreResponse(
    val id: String,
    val concept: String,
    val restored: Boolean,
    val state: String
)

data class ExplainOptions(
    val vault: String? = null,
    @SerializedName("engram_id") val engramId: String,
    val query: List<String>
)

data class ExplainComponents(
    @SerializedName("full_text_relevance") val fullTextRelevance: Double,
    @SerializedName("semantic_similarity") val semanticSimilarity: Double,
    @SerializedName("decay_factor") val decayFactor: Double,
    @SerializedName("hebbian_boost") val hebbianBoost: Double,
    @SerializedName("access_frequency") val accessFrequency: Double,
    val confidence: Double
)

data class ExplainResponse(
    @SerializedName("engram_id") val engramId: String,
    val concept: String,
    @SerializedName("final_score") val finalScore: Double,
    val components: ExplainComponents,
    @SerializedName("fts_matches") val ftsMatches: List<String>,
    @SerializedName("assoc_path") val assocPath: List<String>,
    @SerializedName("would_return") val wouldReturn: Boolean,
    val threshold: Double
)

data class SetStateBody(
    val vault: String?,
    val state: String,
    val reason: String?
)

data class SetStateResponse(
    val id: String,
    val state: String,
    val updated: Boolean
)

data class DeletedEngramItem(
    val id: String,
    val concept: String,
    @SerializedName("deleted_at") val deletedAt: Long,
    @SerializedName("recoverable_until") val recoverableUntil: Long,
    val tags: List<String>?
)

data class ListDeletedResponse(
    val deleted: List<DeletedEngramItem>,
    val count: Int
)

data class RetryEnrichResponse(
    @SerializedName("engram_id") val engramId: String,
    @SerializedName("plugins_queued") val pluginsQueued: List<String>,
    @SerializedName("already_complete") val alreadyComplete: List<String>,
    val note: String?
)

data class ContradictionItem(
    @SerializedName("id_a") val idA: String,
    @SerializedName("concept_a") val conceptA: String,
    @SerializedName("id_b") val idB: String,
    @SerializedName("concept_b") val conceptB: String,
    @SerializedName("detected_at") val detectedAt: Long
)

data class ContradictionsResponse(
    val contradictions: List<ContradictionItem>
)

data class CoherenceResult(
    val score: Double,
    @SerializedName("orphan_ratio") val orphanRatio: Double,
    @SerializedName("contradiction_density") val contradictionDensity: Double,
    @SerializedName("duplication_pressure") val duplicationPressure: Double,
    @SerializedName("temporal_variance") val temporalVariance: Double,
    @SerializedName("total_engrams") val totalEngrams: Long
)

data class StatsResponse(
    @SerializedName("engram_count") val engramCount: Long,
    @SerializedName("vault_count") val vaultCount: Int,
    @SerializedName("index_size") val indexSize: Long,
    @SerializedName("storage_bytes") val storageBytes: Long,
    val coherence: Map<String, CoherenceResult>?
)

data class EngramItem(
    val id: String,
    val concept: String,
    val content: String,
    val confidence: Double,
    val tags: List<String>?,
    val vault: String,
    @SerializedName("created_at") val createdAt: Long,
    @SerializedName("embed_dim") val embedDim: Int?
)

data class ListEngramsResponse(
    val engrams: List<EngramItem>,
    val total: Int,
    val limit: Int,
    val offset: Int
)

data class AssociationItem(
    @SerializedName("target_id") val targetId: String,
    @SerializedName("rel_type") val relType: Int,
    val weight: Double,
    @SerializedName("co_activation_count") val coActivationCount: Int,
    @SerializedName("restored_at") val restoredAt: Long?
)

data class SessionItem(
    val id: String,
    val concept: String,
    val content: String?,
    @SerializedName("created_at") val createdAt: Long
)

data class SessionResponse(
    val entries: List<SessionItem>,
    val total: Int,
    val offset: Int,
    val limit: Int
)

data class SseEvent(
    val type: String,
    val rawData: String
)

data class HealthResponse(
    val status: String,
    val version: String,
    @SerializedName("uptime_seconds") val uptimeSeconds: Long,
    @SerializedName("db_writable") val dbWritable: Boolean
)

// Internal wrappers for JSON deserialization
internal data class GuideResponseBody(val guide: String)
internal data class VaultsResponse(val vaults: List<String>)
internal data class GetLinksResponse(val links: List<AssociationItem>)
internal data class ErrorDetail(
    val code: Int?,
    val message: String?,
    @SerializedName("request_id") val requestId: String?
)
internal data class ErrorEnvelope(val error: ErrorDetail?)
