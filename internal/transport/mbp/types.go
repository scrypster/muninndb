package mbp

import "time"

// Local stub types that will be implemented/provided by the engine later.
// These are minimal definitions to allow the transport layer to compile independently.

// ULID is a 16-byte identifier
type ULID [16]byte

// LifecycleState represents the state of an engram
type LifecycleState uint8

// RelType is the relationship type between engrams
type RelType uint16

// Association represents a directed link between two engrams
type Association struct {
	TargetID          string  `msgpack:"target_id" json:"target_id"`
	RelType           uint16  `msgpack:"rel_type" json:"rel_type"`
	Weight            float32 `msgpack:"weight" json:"weight"`
	Confidence        float32 `msgpack:"confidence" json:"confidence"`
	CreatedAt         int64   `msgpack:"created_at" json:"created_at"`
	LastActivated     int32   `msgpack:"last_activated" json:"last_activated"`
	CoActivationCount uint32  `msgpack:"co_activation_count,omitempty" json:"co_activation_count,omitempty"`
}

// HelloRequest is the HELLO handshake payload.
type HelloRequest struct {
	Version      string   `msgpack:"version" json:"version"`
	AuthMethod   string   `msgpack:"auth_method" json:"auth_method"`
	Token        string   `msgpack:"token,omitempty" json:"token,omitempty"`
	Vault        string   `msgpack:"vault,omitempty" json:"vault,omitempty"`
	Client       string   `msgpack:"client,omitempty" json:"client,omitempty"`
	Capabilities []string `msgpack:"capabilities,omitempty" json:"capabilities,omitempty"`
}

// HelloResponse is the HELLO_OK response payload.
type HelloResponse struct {
	ServerVersion string   `msgpack:"server_version" json:"server_version"`
	SessionID     string   `msgpack:"session_id" json:"session_id"`
	VaultID       string   `msgpack:"vault_id" json:"vault_id"`
	Capabilities  []string `msgpack:"capabilities" json:"capabilities"`
	Limits        Limits   `msgpack:"limits" json:"limits"`
}

// Limits defines the server's operational constraints.
type Limits struct {
	MaxResults   int `msgpack:"max_results" json:"max_results"`
	MaxHopDepth  int `msgpack:"max_hop_depth" json:"max_hop_depth"`
	MaxRate      int `msgpack:"max_rate" json:"max_rate"`
	MaxPayloadMB int `msgpack:"max_payload_mb" json:"max_payload_mb"`
}

// InlineEntity is a caller-provided entity for inline enrichment.
type InlineEntity struct {
	Name string `msgpack:"name" json:"name"`
	Type string `msgpack:"type" json:"type"`
}

// InlineRelationship is a caller-provided relationship for inline enrichment.
type InlineRelationship struct {
	TargetID string  `msgpack:"target_id" json:"target_id"`
	Relation string  `msgpack:"relation" json:"relation"`
	Weight   float32 `msgpack:"weight" json:"weight"`
}

// InlineEntityRelationship is a caller-provided typed entity-to-entity relationship.
// Stored as a RelationshipRecord in the 0x21 entity relationship index.
type InlineEntityRelationship struct {
	FromEntity string  `msgpack:"from_entity" json:"from_entity"`
	ToEntity   string  `msgpack:"to_entity" json:"to_entity"`
	RelType    string  `msgpack:"rel_type" json:"rel_type"`
	Weight     float32 `msgpack:"weight" json:"weight"`
}

// WriteRequest stores a new engram.
type WriteRequest struct {
	Concept      string        `msgpack:"concept" json:"concept"`
	Content      string        `msgpack:"content" json:"content"`
	Tags         []string      `msgpack:"tags,omitempty" json:"tags,omitempty"`
	Confidence   float32       `msgpack:"confidence,omitempty" json:"confidence,omitempty"`
	Stability    float32       `msgpack:"stability,omitempty" json:"stability,omitempty"`
	CreatedAt    *time.Time    `msgpack:"created_at,omitempty" json:"created_at,omitempty"`
	Associations []Association `msgpack:"associations,omitempty" json:"associations,omitempty"`
	Embedding    []float32     `msgpack:"embedding,omitempty" json:"embedding,omitempty"`
	Vault        string        `msgpack:"vault,omitempty" json:"vault,omitempty"`
	IdempotentID string        `msgpack:"idempotent_id,omitempty" json:"idempotent_id,omitempty"`
	MemoryType   uint8         `msgpack:"memory_type,omitempty" json:"memory_type,omitempty"`
	TypeLabel    string        `msgpack:"type_label,omitempty" json:"type_label,omitempty"`

	// Inline enrichment: caller-provided data that bypasses background enrichment.
	Summary             string                     `msgpack:"summary,omitempty" json:"summary,omitempty"`
	Entities            []InlineEntity             `msgpack:"entities,omitempty" json:"entities,omitempty"`
	Relationships       []InlineRelationship       `msgpack:"relationships,omitempty" json:"relationships,omitempty"`
	EntityRelationships []InlineEntityRelationship `msgpack:"entity_relationships,omitempty" json:"entity_relationships,omitempty"`
}

// WriteResponse confirms a write and returns the assigned ULID.
type WriteResponse struct {
	ID        string `msgpack:"id"         json:"id"`
	CreatedAt int64  `msgpack:"created_at" json:"created_at"`
}

// ReadRequest retrieves an engram by ID.
type ReadRequest struct {
	ID    string `msgpack:"id" json:"id"`
	Vault string `msgpack:"vault,omitempty" json:"vault,omitempty"`
}

// ReadResponse returns the full engram data.
type ReadResponse struct {
	ID             string   `msgpack:"id"                    json:"id"`
	Concept        string   `msgpack:"concept"               json:"concept"`
	Content        string   `msgpack:"content"               json:"content"`
	Confidence     float32  `msgpack:"confidence"            json:"confidence"`
	Relevance      float32  `msgpack:"relevance"             json:"relevance"`
	Stability      float32  `msgpack:"stability"             json:"stability"`
	AccessCount    uint32   `msgpack:"access_count"          json:"access_count"`
	Tags           []string `msgpack:"tags,omitempty"        json:"tags,omitempty"`
	State          uint8    `msgpack:"state"                 json:"state"`
	CreatedAt      int64    `msgpack:"created_at"            json:"created_at"`
	UpdatedAt      int64    `msgpack:"updated_at"            json:"updated_at"`
	LastAccess     int64    `msgpack:"last_access"           json:"last_access"`
	Summary        string   `msgpack:"summary,omitempty"     json:"summary,omitempty"`
	KeyPoints      []string `msgpack:"key_points,omitempty"  json:"key_points,omitempty"`
	MemoryType     uint8    `msgpack:"memory_type" json:"memory_type"`
	TypeLabel      string   `msgpack:"type_label,omitempty"  json:"type_label,omitempty"`
	Classification uint16   `msgpack:"classification,omitempty" json:"classification,omitempty"`
	// EmbedDim is the stored embedding dimensionality code (0 = no embedding).
	// 1 = 384-dim, 2 = 768-dim, 3 = 1536-dim.
	EmbedDim uint8 `msgpack:"embed_dim,omitempty" json:"embed_dim,omitempty"`

	// Entities and EntityRelationships are populated by muninn_read to expose what
	// was stored via inline enrichment. Empty when no entities/relationships were linked.
	Entities            []InlineEntity             `msgpack:"entities,omitempty"              json:"entities,omitempty"`
	EntityRelationships []InlineEntityRelationship `msgpack:"entity_relationships,omitempty"  json:"entity_relationships,omitempty"`
}

// ActivateRequest queries for relevant engrams.
type ActivateRequest struct {
	Context     []string  `msgpack:"context" json:"context"`
	Threshold   float32   `msgpack:"threshold,omitempty" json:"threshold,omitempty"`
	MaxResults  int       `msgpack:"max_results,omitempty" json:"max_results,omitempty"`
	MaxHops     int       `msgpack:"max_hops,omitempty" json:"max_hops,omitempty"`
	IncludeWhy  bool      `msgpack:"include_why,omitempty" json:"include_why,omitempty"`
	Weights     *Weights  `msgpack:"weights,omitempty" json:"weights,omitempty"`
	Filters     []Filter  `msgpack:"filters,omitempty" json:"filters,omitempty"`
	Vault       string    `msgpack:"vault,omitempty" json:"vault,omitempty"`
	Embedding   []float32 `msgpack:"embedding,omitempty" json:"embedding,omitempty"`
	BriefMode   string    `msgpack:"brief_mode,omitempty" json:"brief_mode,omitempty"`     // "extractive"|"llm"|"auto"|"" (default: "auto")
	DisableHops bool      `msgpack:"disable_hops,omitempty" json:"disable_hops,omitempty"` // when true, override default hop traversal to 0
	Profile     string    `json:"profile,omitempty" msgpack:"profile,omitempty"`           // traversal profile override: ""|"default"|"causal"|"confirmatory"|"adversarial"|"structural"
	Mode        string    `json:"mode,omitempty" msgpack:"mode,omitempty"`                 // recall mode preset: "semantic"|"recent"|"balanced"|"deep"
}

// Weights defines scoring weight distribution.
type Weights struct {
	SemanticSimilarity float32 `msgpack:"semantic_similarity" json:"semantic_similarity"`
	FullTextRelevance  float32 `msgpack:"full_text_relevance" json:"full_text_relevance"`
	DecayFactor        float32 `msgpack:"decay_factor" json:"decay_factor"`
	HebbianBoost       float32 `msgpack:"hebbian_boost" json:"hebbian_boost"`
	AccessFrequency    float32 `msgpack:"access_frequency" json:"access_frequency"`
	Recency            float32 `msgpack:"recency" json:"recency"`
	// CGDN: Cognitive-Gated Divisive Normalization (Carandini & Heeger 2012).
	// When UseCGDN=true, replaces additive weighted sum with multiplicative
	// cognitive gating and divisive normalization across all candidates.
	UseCGDN   bool    `msgpack:"use_cgdn,omitempty" json:"use_cgdn,omitempty"`
	CGDNAlpha float32 `msgpack:"cgdn_alpha,omitempty" json:"cgdn_alpha,omitempty"` // Ebbinghaus gate exponent (default 1.5)
	CGDNBeta  float32 `msgpack:"cgdn_beta,omitempty" json:"cgdn_beta,omitempty"`   // Hebbian gate exponent (default 0.5)
	CGDNPower float32 `msgpack:"cgdn_power,omitempty" json:"cgdn_power,omitempty"` // divisive normalization power (default 2.0)
	// ACT-R: total recall mode. Score = ContentMatch × softplus(B(M) + scale×Hebbian).
	UseACTR      bool    `msgpack:"use_actr,omitempty" json:"use_actr,omitempty"`
	ACTRDecay    float32 `msgpack:"actr_decay,omitempty" json:"actr_decay,omitempty"`         // power-law exponent d (default 0.5)
	ACTRHebScale float32 `msgpack:"actr_heb_scale,omitempty" json:"actr_heb_scale,omitempty"` // Hebbian amplifier (default 4.0)
	DisableACTR  bool    `msgpack:"disable_actr,omitempty" json:"disable_actr,omitempty"`     // when true, use legacy weighted-sum scoring instead of ACT-R
}

// Filter restricts activation results.
type Filter struct {
	Field string      `msgpack:"field" json:"field"`
	Op    string      `msgpack:"op" json:"op"`
	Value interface{} `msgpack:"value" json:"value"`
}

// BriefSentence is a single sentence from the activation brief.
type BriefSentence struct {
	EngramID string  `msgpack:"engram_id" json:"engram_id"`
	Text     string  `msgpack:"text"      json:"text"`
	Score    float64 `msgpack:"score"     json:"score"`
}

// ActivateResponse returns activation results (may be multi-frame).
type ActivateResponse struct {
	QueryID     string           `msgpack:"query_id"               json:"query_id"`
	TotalFound  int              `msgpack:"total_found"            json:"total_found"`
	Activations []ActivationItem `msgpack:"activations"            json:"activations"`
	LatencyMs   float64          `msgpack:"latency_ms,omitempty"   json:"latency_ms,omitempty"`
	Frame       int              `msgpack:"frame,omitempty"        json:"frame,omitempty"`
	TotalFrames int              `msgpack:"total_frames,omitempty" json:"total_frames,omitempty"`
	Brief       []BriefSentence  `msgpack:"brief,omitempty"        json:"brief,omitempty"` // extractive activation brief
}

// ActivationItem is a single activated engram.
type ActivationItem struct {
	ID              string          `msgpack:"id"                          json:"id"`
	Concept         string          `msgpack:"concept"                     json:"concept"`
	Content         string          `msgpack:"content"                     json:"content"`
	Summary         string          `msgpack:"summary,omitempty"           json:"summary,omitempty"`
	Score           float32         `msgpack:"score"                       json:"score"`
	Confidence      float32         `msgpack:"confidence"                  json:"confidence"`
	ScoreComponents ScoreComponents `msgpack:"score_components,omitempty"  json:"score_components,omitempty"`
	Why             string          `msgpack:"why,omitempty"               json:"why,omitempty"`
	HopPath         []string        `msgpack:"hop_path,omitempty"          json:"hop_path,omitempty"`
	Dormant         bool            `msgpack:"dormant,omitempty"           json:"dormant,omitempty"`
	CreatedAt       int64           `msgpack:"created_at,omitempty"        json:"created_at,omitempty"`
	LastAccess      int64           `msgpack:"last_access,omitempty"       json:"last_access,omitempty"`
	AccessCount     uint32          `msgpack:"access_count,omitempty"      json:"access_count,omitempty"`
	Relevance       float32         `msgpack:"relevance,omitempty"         json:"relevance,omitempty"`
	SourceType      string          `msgpack:"source_type,omitempty"       json:"source_type,omitempty"`
}

// ScoreComponents breaks down the activation score.
type ScoreComponents struct {
	SemanticSimilarity float32 `msgpack:"semantic_similarity"           json:"semantic_similarity"`
	FullTextRelevance  float32 `msgpack:"full_text_relevance"           json:"full_text_relevance"`
	DecayFactor        float32 `msgpack:"decay_factor"                  json:"decay_factor"`
	HebbianBoost       float32 `msgpack:"hebbian_boost"                 json:"hebbian_boost"`
	TransitionBoost    float32 `msgpack:"transition_boost,omitempty"    json:"transition_boost,omitempty"`
	AccessFrequency    float32 `msgpack:"access_frequency"              json:"access_frequency"`
	Recency            float32 `msgpack:"recency"                       json:"recency"`
	Raw                float32 `msgpack:"raw"                           json:"raw"`
	Final              float32 `msgpack:"final"                         json:"final"`
}

// SubscribeRequest registers a context subscription.
type SubscribeRequest struct {
	SubscriptionID string   `msgpack:"subscription_id,omitempty" json:"subscription_id,omitempty"`
	Context        []string `msgpack:"context" json:"context"`
	Threshold      float32  `msgpack:"threshold,omitempty" json:"threshold,omitempty"`
	Vault          string   `msgpack:"vault,omitempty" json:"vault,omitempty"`
	TTL            int      `msgpack:"ttl,omitempty" json:"ttl,omitempty"`
	RateLimit      int      `msgpack:"rate_limit,omitempty" json:"rate_limit,omitempty"`
	PushOnWrite    bool     `msgpack:"push_on_write,omitempty" json:"push_on_write,omitempty"`
	DeltaThreshold float32  `msgpack:"delta_threshold,omitempty" json:"delta_threshold,omitempty"`
}

// SubscribeResponse confirms subscription creation.
type SubscribeResponse struct {
	SubID  string `msgpack:"sub_id" json:"sub_id"`
	Status string `msgpack:"status" json:"status"`
}

// ActivationPush is an unsolicited server push.
type ActivationPush struct {
	SubscriptionID string         `msgpack:"subscription_id" json:"subscription_id"`
	Activation     ActivationItem `msgpack:"activation" json:"activation"`
	Trigger        string         `msgpack:"trigger" json:"trigger"`
	PushNumber     int            `msgpack:"push_number" json:"push_number"`
	At             int64          `msgpack:"at" json:"at"`
}

// UnsubscribeRequest cancels a subscription.
type UnsubscribeRequest struct {
	SubID string `msgpack:"sub_id" json:"sub_id"`
}

// UnsubscribeResponse confirms unsubscription.
type UnsubscribeResponse struct {
	OK bool `msgpack:"ok" json:"ok"`
}

// LinkRequest creates/updates an association.
type LinkRequest struct {
	SourceID string  `msgpack:"source_id" json:"source_id"`
	TargetID string  `msgpack:"target_id" json:"target_id"`
	RelType  uint16  `msgpack:"rel_type" json:"rel_type"`
	Weight   float32 `msgpack:"weight,omitempty" json:"weight,omitempty"`
	Vault    string  `msgpack:"vault,omitempty" json:"vault,omitempty"`
}

// LinkResponse confirms association.
type LinkResponse struct {
	OK bool `msgpack:"ok" json:"ok"`
}

// ForgetRequest soft-deletes an engram.
type ForgetRequest struct {
	ID    string `msgpack:"id" json:"id"`
	Hard  bool   `msgpack:"hard,omitempty" json:"hard,omitempty"`
	Vault string `msgpack:"vault,omitempty" json:"vault,omitempty"`
}

// ForgetResponse confirms deletion.
type ForgetResponse struct {
	OK bool `msgpack:"ok" json:"ok"`
}

// StatRequest queries database statistics.
type StatRequest struct {
	Vault string `msgpack:"vault,omitempty" json:"vault,omitempty"`
}

// CoherenceResult holds coherence metrics for a single vault.
type CoherenceResult struct {
	Score                float64 `msgpack:"score"                 json:"score"`
	OrphanRatio          float64 `msgpack:"orphan_ratio"          json:"orphan_ratio"`
	ContradictionDensity float64 `msgpack:"contradiction_density" json:"contradiction_density"`
	DuplicationPressure  float64 `msgpack:"duplication_pressure"  json:"duplication_pressure"`
	TemporalVariance     float64 `msgpack:"temporal_variance"     json:"temporal_variance"`
	TotalEngrams         int64   `msgpack:"total_engrams"         json:"total_engrams"`
}

// StatResponse returns database stats.
type StatResponse struct {
	EngramCount     int64                      `msgpack:"engram_count"        json:"engram_count"`
	VaultCount      int                        `msgpack:"vault_count"         json:"vault_count"`
	IndexSize       int64                      `msgpack:"index_size"          json:"index_size"`
	StorageBytes    int64                      `msgpack:"storage_bytes"       json:"storage_bytes"`
	CoherenceScores map[string]CoherenceResult `msgpack:"coherence,omitempty" json:"coherence,omitempty"`
}

// PingRequest is a keepalive probe.
type PingRequest struct {
	Data string `msgpack:"data,omitempty" json:"data,omitempty"`
}

// PongResponse is a keepalive response.
type PongResponse struct {
	Data string `msgpack:"data,omitempty" json:"data,omitempty"`
}
