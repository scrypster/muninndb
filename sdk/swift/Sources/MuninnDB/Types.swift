import Foundation

// MARK: - Write

public struct InlineEntity: Codable, Sendable {
    public var name: String
    public var type: String

    public init(name: String, type: String) {
        self.name = name
        self.type = type
    }
}

public struct InlineRelationship: Codable, Sendable {
    public var targetId: String
    public var relation: String
    public var weight: Double

    public init(targetId: String, relation: String, weight: Double) {
        self.targetId = targetId
        self.relation = relation
        self.weight = weight
    }

    enum CodingKeys: String, CodingKey {
        case targetId = "target_id"
        case relation, weight
    }
}

public struct WriteOptions: Codable, Sendable {
    public var concept: String
    public var content: String
    public var vault: String?
    public var tags: [String]?
    public var confidence: Double?
    public var stability: Double?
    public var memoryType: Int?
    public var typeLabel: String?
    public var summary: String?
    public var entities: [InlineEntity]?
    public var relationships: [InlineRelationship]?

    public init(
        concept: String,
        content: String,
        vault: String? = nil,
        tags: [String]? = nil,
        confidence: Double? = nil,
        stability: Double? = nil,
        memoryType: Int? = nil,
        typeLabel: String? = nil,
        summary: String? = nil,
        entities: [InlineEntity]? = nil,
        relationships: [InlineRelationship]? = nil
    ) {
        self.concept = concept
        self.content = content
        self.vault = vault
        self.tags = tags
        self.confidence = confidence
        self.stability = stability
        self.memoryType = memoryType
        self.typeLabel = typeLabel
        self.summary = summary
        self.entities = entities
        self.relationships = relationships
    }

    enum CodingKeys: String, CodingKey {
        case concept, content, vault, tags, confidence, stability
        case memoryType = "memory_type"
        case typeLabel = "type_label"
        case summary, entities, relationships
    }
}

public struct WriteResponse: Codable, Sendable {
    public let id: String
    public let createdAt: Int64

    enum CodingKeys: String, CodingKey {
        case id
        case createdAt = "created_at"
    }
}

// MARK: - Batch Write

public struct BatchWriteResult: Codable, Sendable {
    public let id: String
    public let createdAt: Int64

    enum CodingKeys: String, CodingKey {
        case id
        case createdAt = "created_at"
    }
}

public struct BatchWriteResponse: Codable, Sendable {
    public let results: [BatchWriteResult]
}

// MARK: - Read (Engram)

public struct Engram: Codable, Sendable {
    public let id: String
    public let concept: String
    public let content: String
    public let confidence: Double
    public let relevance: Double
    public let stability: Double
    public let accessCount: Int
    public let tags: [String]?
    public let state: Int
    public let createdAt: Int64
    public let updatedAt: Int64
    public let lastAccess: Int64
    public let summary: String?
    public let keyPoints: [String]?
    public let memoryType: Int
    public let typeLabel: String?
    public let embedDim: Int?

    enum CodingKeys: String, CodingKey {
        case id, concept, content, confidence, relevance, stability
        case accessCount = "access_count"
        case tags, state
        case createdAt = "created_at"
        case updatedAt = "updated_at"
        case lastAccess = "last_access"
        case summary
        case keyPoints = "key_points"
        case memoryType = "memory_type"
        case typeLabel = "type_label"
        case embedDim = "embed_dim"
    }
}

// MARK: - Activate

public struct Weights: Codable, Sendable {
    public var semanticSimilarity: Double?
    public var fullTextRelevance: Double?
    public var decayFactor: Double?
    public var hebbianBoost: Double?
    public var accessFrequency: Double?
    public var recency: Double?

    public init(
        semanticSimilarity: Double? = nil,
        fullTextRelevance: Double? = nil,
        decayFactor: Double? = nil,
        hebbianBoost: Double? = nil,
        accessFrequency: Double? = nil,
        recency: Double? = nil
    ) {
        self.semanticSimilarity = semanticSimilarity
        self.fullTextRelevance = fullTextRelevance
        self.decayFactor = decayFactor
        self.hebbianBoost = hebbianBoost
        self.accessFrequency = accessFrequency
        self.recency = recency
    }

    enum CodingKeys: String, CodingKey {
        case semanticSimilarity = "semantic_similarity"
        case fullTextRelevance = "full_text_relevance"
        case decayFactor = "decay_factor"
        case hebbianBoost = "hebbian_boost"
        case accessFrequency = "access_frequency"
        case recency
    }
}

/// Filter restricts activation results. Matches the server's Filter{field, op, value} wire format.
/// Common fields: "memory_type", "state", "confidence", "tags".
/// Common ops: "eq", "gt", "lt", "gte", "lte", "in".
public struct Filter: Codable, Sendable {
    public var field: String
    public var op: String
    public var value: AnyCodable

    public init(field: String, op: String, value: AnyCodable) {
        self.field = field
        self.op = op
        self.value = value
    }
}

/// Type-erased Codable wrapper for Filter.value which can be String, Int, Double, Bool, or [String].
public struct AnyCodable: Codable, Sendable {
    public let rawValue: Any

    public init(_ value: Any) { self.rawValue = value }

    public init(from decoder: Decoder) throws {
        let c = try decoder.singleValueContainer()
        if let v = try? c.decode(Bool.self)   { rawValue = v; return }
        if let v = try? c.decode(Int.self)    { rawValue = v; return }
        if let v = try? c.decode(Double.self) { rawValue = v; return }
        if let v = try? c.decode(String.self) { rawValue = v; return }
        if let v = try? c.decode([String].self) { rawValue = v; return }
        rawValue = ""
    }

    public func encode(to encoder: Encoder) throws {
        var c = encoder.singleValueContainer()
        switch rawValue {
        case let v as Bool:     try c.encode(v)
        case let v as Int:      try c.encode(v)
        case let v as Double:   try c.encode(v)
        case let v as String:   try c.encode(v)
        case let v as [String]: try c.encode(v)
        default: try c.encodeNil()
        }
    }
}

public struct ActivateOptions: Codable, Sendable {
    public var context: [String]
    public var vault: String?
    public var maxResults: Int?
    public var threshold: Double?
    public var maxHops: Int?
    public var includeWhy: Bool?
    public var briefMode: String?
    public var disableHops: Bool?
    public var weights: Weights?
    public var filters: [Filter]?

    public init(
        context: [String],
        vault: String? = nil,
        maxResults: Int? = nil,
        threshold: Double? = nil,
        maxHops: Int? = nil,
        includeWhy: Bool? = nil,
        briefMode: String? = nil,
        disableHops: Bool? = nil,
        weights: Weights? = nil,
        filters: [Filter]? = nil
    ) {
        self.context = context
        self.vault = vault
        self.maxResults = maxResults
        self.threshold = threshold
        self.maxHops = maxHops
        self.includeWhy = includeWhy
        self.briefMode = briefMode
        self.disableHops = disableHops
        self.weights = weights
        self.filters = filters
    }

    enum CodingKeys: String, CodingKey {
        case context, vault
        case maxResults = "max_results"
        case threshold
        case maxHops = "max_hops"
        case includeWhy = "include_why"
        case briefMode = "brief_mode"
        case disableHops = "disable_hops"
        case weights, filters
    }
}

public struct ScoreComponents: Codable, Sendable {
    public let fullTextRelevance: Double?
    public let semanticSimilarity: Double?
    public let decayFactor: Double?
    public let hebbianBoost: Double?
    public let accessFrequency: Double?
    public let confidence: Double?

    enum CodingKeys: String, CodingKey {
        case fullTextRelevance = "full_text_relevance"
        case semanticSimilarity = "semantic_similarity"
        case decayFactor = "decay_factor"
        case hebbianBoost = "hebbian_boost"
        case accessFrequency = "access_frequency"
        case confidence
    }
}

public struct ActivationItem: Codable, Sendable {
    public let id: String
    public let concept: String
    public let content: String
    public let summary: String?
    public let score: Double
    public let confidence: Double
    public let why: String?
    public let hopPath: [String]?
    public let dormant: Bool?
    public let createdAt: Int64?
    public let lastAccess: Int64?
    public let accessCount: Int?
    public let relevance: Double?

    enum CodingKeys: String, CodingKey {
        case id, concept, content, summary, score, confidence, why
        case hopPath = "hop_path"
        case dormant
        case createdAt = "created_at"
        case lastAccess = "last_access"
        case accessCount = "access_count"
        case relevance
    }
}

public struct BriefSentence: Codable, Sendable {
    public let engramId: String
    public let text: String
    public let score: Double

    enum CodingKeys: String, CodingKey {
        case engramId = "engram_id"
        case text, score
    }
}

public struct ActivateResponse: Codable, Sendable {
    public let queryId: String
    public let totalFound: Int
    public let activations: [ActivationItem]
    public let latencyMs: Double?
    public let brief: [BriefSentence]?

    enum CodingKeys: String, CodingKey {
        case queryId = "query_id"
        case totalFound = "total_found"
        case activations
        case latencyMs = "latency_ms"
        case brief
    }
}

// MARK: - Link

public struct LinkOptions: Codable, Sendable {
    public var sourceId: String
    public var targetId: String
    public var relType: Int
    public var weight: Double?
    public var vault: String?

    public init(
        sourceId: String,
        targetId: String,
        relType: Int,
        weight: Double? = nil,
        vault: String? = nil
    ) {
        self.sourceId = sourceId
        self.targetId = targetId
        self.relType = relType
        self.weight = weight
        self.vault = vault
    }

    enum CodingKeys: String, CodingKey {
        case sourceId = "source_id"
        case targetId = "target_id"
        case relType = "rel_type"
        case weight, vault
    }
}

// MARK: - Traverse

public struct TraverseOptions: Codable, Sendable {
    public var vault: String?
    public var startId: String
    public var maxHops: Int?
    public var maxNodes: Int?
    public var relTypes: [String]?
    public var followEntities: Bool?

    public init(
        startId: String,
        vault: String? = nil,
        maxHops: Int? = nil,
        maxNodes: Int? = nil,
        relTypes: [String]? = nil,
        followEntities: Bool? = nil
    ) {
        self.startId = startId
        self.vault = vault
        self.maxHops = maxHops
        self.maxNodes = maxNodes
        self.relTypes = relTypes
        self.followEntities = followEntities
    }

    enum CodingKeys: String, CodingKey {
        case vault
        case startId = "start_id"
        case maxHops = "max_hops"
        case maxNodes = "max_nodes"
        case relTypes = "rel_types"
        case followEntities = "follow_entities"
    }
}

public struct TraversalNode: Codable, Sendable {
    public let id: String
    public let concept: String
    public let hopDist: Int
    public let summary: String?

    enum CodingKeys: String, CodingKey {
        case id, concept
        case hopDist = "hop_dist"
        case summary
    }
}

public struct TraversalEdge: Codable, Sendable {
    public let fromId: String
    public let toId: String
    public let relType: String
    public let weight: Double

    enum CodingKeys: String, CodingKey {
        case fromId = "from_id"
        case toId = "to_id"
        case relType = "rel_type"
        case weight
    }
}

public struct TraverseResponse: Codable, Sendable {
    public let nodes: [TraversalNode]
    public let edges: [TraversalEdge]
    public let totalReachable: Int
    public let queryMs: Double

    enum CodingKeys: String, CodingKey {
        case nodes, edges
        case totalReachable = "total_reachable"
        case queryMs = "query_ms"
    }
}

// MARK: - Evolve

public struct EvolveResponse: Codable, Sendable {
    public let id: String
}

// MARK: - Consolidate

public struct ConsolidateOptions: Codable, Sendable {
    public var vault: String?
    public var ids: [String]
    public var mergedContent: String

    public init(ids: [String], mergedContent: String, vault: String? = nil) {
        self.ids = ids
        self.mergedContent = mergedContent
        self.vault = vault
    }

    enum CodingKeys: String, CodingKey {
        case vault, ids
        case mergedContent = "merged_content"
    }
}

public struct ConsolidateResponse: Codable, Sendable {
    public let id: String
    public let archived: [String]
    public let warnings: [String]?
}

// MARK: - Decide

public struct DecideOptions: Codable, Sendable {
    public var vault: String?
    public var decision: String
    public var rationale: String
    public var alternatives: [String]?
    public var evidenceIds: [String]?

    public init(
        decision: String,
        rationale: String,
        vault: String? = nil,
        alternatives: [String]? = nil,
        evidenceIds: [String]? = nil
    ) {
        self.decision = decision
        self.rationale = rationale
        self.vault = vault
        self.alternatives = alternatives
        self.evidenceIds = evidenceIds
    }

    enum CodingKeys: String, CodingKey {
        case vault, decision, rationale, alternatives
        case evidenceIds = "evidence_ids"
    }
}

public struct DecideResponse: Codable, Sendable {
    public let id: String
    public let warnings: [String]?
}

// MARK: - Restore

public struct RestoreResponse: Codable, Sendable {
    public let id: String
    public let concept: String
    public let restored: Bool
    public let state: String
}

// MARK: - Explain

public struct ExplainOptions: Codable, Sendable {
    public var vault: String?
    public var engramId: String
    public var query: [String]

    public init(engramId: String, query: [String], vault: String? = nil) {
        self.engramId = engramId
        self.query = query
        self.vault = vault
    }

    enum CodingKeys: String, CodingKey {
        case vault
        case engramId = "engram_id"
        case query
    }
}

public struct ExplainComponents: Codable, Sendable {
    public let fullTextRelevance: Double
    public let semanticSimilarity: Double
    public let decayFactor: Double
    public let hebbianBoost: Double
    public let accessFrequency: Double
    public let confidence: Double

    enum CodingKeys: String, CodingKey {
        case fullTextRelevance = "full_text_relevance"
        case semanticSimilarity = "semantic_similarity"
        case decayFactor = "decay_factor"
        case hebbianBoost = "hebbian_boost"
        case accessFrequency = "access_frequency"
        case confidence
    }
}

public struct ExplainResponse: Codable, Sendable {
    public let engramId: String
    public let concept: String
    public let finalScore: Double
    public let components: ExplainComponents
    public let ftsMatches: [String]
    public let assocPath: [String]
    public let wouldReturn: Bool
    public let threshold: Double

    enum CodingKeys: String, CodingKey {
        case engramId = "engram_id"
        case concept
        case finalScore = "final_score"
        case components
        case ftsMatches = "fts_matches"
        case assocPath = "assoc_path"
        case wouldReturn = "would_return"
        case threshold
    }
}

// MARK: - Set State

public struct SetStateResponse: Codable, Sendable {
    public let id: String
    public let state: String
    public let updated: Bool
}

// MARK: - List Deleted

public struct DeletedEngramItem: Codable, Sendable {
    public let id: String
    public let concept: String
    public let deletedAt: Int64
    public let recoverableUntil: Int64
    public let tags: [String]?

    enum CodingKeys: String, CodingKey {
        case id, concept
        case deletedAt = "deleted_at"
        case recoverableUntil = "recoverable_until"
        case tags
    }
}

public struct ListDeletedResponse: Codable, Sendable {
    public let deleted: [DeletedEngramItem]
    public let count: Int
}

// MARK: - Retry Enrich

public struct RetryEnrichResponse: Codable, Sendable {
    public let engramId: String
    public let pluginsQueued: [String]
    public let alreadyComplete: [String]
    public let note: String?

    enum CodingKeys: String, CodingKey {
        case engramId = "engram_id"
        case pluginsQueued = "plugins_queued"
        case alreadyComplete = "already_complete"
        case note
    }
}

// MARK: - Contradictions

public struct ContradictionItem: Codable, Sendable {
    public let idA: String
    public let conceptA: String
    public let idB: String
    public let conceptB: String
    public let detectedAt: Int64

    enum CodingKeys: String, CodingKey {
        case idA = "id_a"
        case conceptA = "concept_a"
        case idB = "id_b"
        case conceptB = "concept_b"
        case detectedAt = "detected_at"
    }
}

public struct ContradictionsResponse: Codable, Sendable {
    public let contradictions: [ContradictionItem]
}

// MARK: - Stats

public struct CoherenceResult: Codable, Sendable {
    public let score: Double
    public let orphanRatio: Double
    public let contradictionDensity: Double
    public let duplicationPressure: Double
    public let temporalVariance: Double
    public let totalEngrams: Int64

    enum CodingKeys: String, CodingKey {
        case score
        case orphanRatio = "orphan_ratio"
        case contradictionDensity = "contradiction_density"
        case duplicationPressure = "duplication_pressure"
        case temporalVariance = "temporal_variance"
        case totalEngrams = "total_engrams"
    }
}

public struct StatsResponse: Codable, Sendable {
    public let engramCount: Int64
    public let vaultCount: Int
    public let indexSize: Int64
    public let storageBytes: Int64
    public let coherence: [String: CoherenceResult]?

    enum CodingKeys: String, CodingKey {
        case engramCount = "engram_count"
        case vaultCount = "vault_count"
        case indexSize = "index_size"
        case storageBytes = "storage_bytes"
        case coherence
    }
}

// MARK: - List Engrams

public struct EngramItem: Codable, Sendable {
    public let id: String
    public let concept: String
    public let content: String
    public let confidence: Double
    public let tags: [String]?
    public let vault: String
    public let createdAt: Int64
    public let embedDim: Int?

    enum CodingKeys: String, CodingKey {
        case id, concept, content, confidence, tags, vault
        case createdAt = "created_at"
        case embedDim = "embed_dim"
    }
}

public struct ListEngramsResponse: Codable, Sendable {
    public let engrams: [EngramItem]
    public let total: Int
    public let limit: Int
    public let offset: Int
}

// MARK: - Associations / Links

public struct AssociationItem: Codable, Sendable {
    public let targetId: String
    public let relType: Int
    public let weight: Double
    public let coActivationCount: Int
    public let restoredAt: Int64?

    enum CodingKeys: String, CodingKey {
        case targetId = "target_id"
        case relType = "rel_type"
        case weight
        case coActivationCount = "co_activation_count"
        case restoredAt = "restored_at"
    }
}

// MARK: - Session

public struct SessionItem: Codable, Sendable {
    public let id: String
    public let concept: String
    public let content: String?
    public let createdAt: Int64

    enum CodingKeys: String, CodingKey {
        case id, concept, content
        case createdAt = "created_at"
    }
}

public struct SessionResponse: Codable, Sendable {
    public let entries: [SessionItem]
    public let total: Int
    public let offset: Int
    public let limit: Int
}

// MARK: - Health

public struct HealthResponse: Codable, Sendable {
    public let status: String
    public let version: String
    public let uptimeSeconds: Int64
    public let dbWritable: Bool

    enum CodingKeys: String, CodingKey {
        case status, version
        case uptimeSeconds = "uptime_seconds"
        case dbWritable = "db_writable"
    }
}

// MARK: - Internal request/response helpers

struct ErrorDetail: Codable {
    let code: Int?
    let message: String?
    let requestId: String?

    enum CodingKeys: String, CodingKey {
        case code, message
        case requestId = "request_id"
    }
}

struct ErrorBody: Codable {
    let error: ErrorDetail?
}

struct GuideResponse: Codable {
    let guide: String
}

struct LinksResponse: Codable {
    let links: [AssociationItem]
}

struct VaultsResponse: Codable {
    let vaults: [String]
}

struct BatchWriteRequest: Codable {
    let engrams: [WriteOptions]
}

struct EvolveRequest: Codable {
    let newContent: String
    let reason: String
    let vault: String?

    enum CodingKeys: String, CodingKey {
        case newContent = "new_content"
        case reason, vault
    }
}

struct SetStateRequest: Codable {
    let vault: String?
    let state: String
    let reason: String?
}
