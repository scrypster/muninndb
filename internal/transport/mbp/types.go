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

	// Trust is an optional trust-level label (verified|inferred|external|untrusted).
	// Empty defaults to "inferred" — the level for all AI-generated content.
	// Setting "verified" requires a write or full credential (S8): the engine
	// rejects it under an observe credential. source_type is provenance-derived
	// and never a write argument, so trust is the only discriminator the write
	// path exposes for the anti-pollution capture tiering.
	Trust string `msgpack:"trust,omitempty" json:"trust,omitempty"`

	// Valid-time (application-time) axis, half-open [valid_from, valid_until).
	// ValidFrom nil defaults to CreatedAt ("valid from creation"); ValidUntil
	// nil means open / "still current". Use for historical facts whose truth
	// window differs from when they were stored.
	ValidFrom  *time.Time `msgpack:"valid_from,omitempty" json:"valid_from,omitempty"`
	ValidUntil *time.Time `msgpack:"valid_until,omitempty" json:"valid_until,omitempty"`

	// Importance is the caller-asserted priority in [0,1] (orthogonal to
	// Confidence, which is truth). Pointer so an explicit 0 is distinct from
	// unset: nil = unset (a use-time default is derived from the memory type
	// at read/prune time and never stored); an explicit value is clamped to
	// [0,1] and an explicit 0.0 is quantized to 0.01 on write so the stored
	// 0 = unset sentinel stays intact. In this increment importance drives
	// pruning protection (COG-20) and is surfaced on read — it does not
	// modify decay or recall ranking.
	Importance *float32 `msgpack:"importance,omitempty" json:"importance,omitempty"`

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
	Hint      string `msgpack:"hint,omitempty" json:"hint,omitempty"`
}

// ReadRequest retrieves an engram by ID.
type ReadRequest struct {
	ID    string `msgpack:"id" json:"id"`
	Vault string `msgpack:"vault,omitempty" json:"vault,omitempty"`
	// ReadOnly, when true, suppresses reinforcement side effects (TouchAccess
	// and the implicit feedback signal) for this read. Set by the MCP/REST/gRPC
	// surface from the effective read-only decision (S3): observe-mode
	// credential OR an explicit read_only request flag.
	ReadOnly bool `msgpack:"read_only,omitempty" json:"read_only,omitempty"`
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
	// Trust is the TrustLevel uint8 (0=unset/inferred, 1=verified, 2=inferred, 3=external, 4=untrusted).
	// omitempty is intentional: TrustUnset(0x00) and TrustInferred(0x02) both render as "inferred"
	// in the MCP layer, and legacy records (pre-trust) written as 0x00 are treated as inferred.
	// Clients should treat an absent trust field as equivalent to TrustUnset (0x00).
	Trust uint8 `msgpack:"trust,omitempty" json:"trust,omitempty"`

	// Valid-time axis (teaches the two time axes: created_at/updated_at are
	// transaction time; valid_from/valid_until are application time).
	// ValidFrom is always set (defaults to CreatedAt). ValidUntil is 0 while
	// the window is open. IsCurrent = (ValidUntil == 0).
	ValidFrom  int64 `msgpack:"valid_from,omitempty" json:"valid_from,omitempty"`
	ValidUntil int64 `msgpack:"valid_until,omitempty" json:"valid_until,omitempty"`
	IsCurrent  bool  `msgpack:"is_current" json:"is_current"`

	// Importance is the STORED caller-asserted importance (0 = unset; the
	// presentation layer derives the effective value from MemoryType/Trust —
	// the derived value is never stored, so the wire carries only the
	// assertion). omitempty intentional: absent means unset/derived.
	Importance float32 `msgpack:"importance,omitempty" json:"importance,omitempty"`

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
	// CallerOwner is the lease-owner identity of the recall caller. Engrams held by
	// a live lease owned by someone else are hidden (work-queue checkout); the
	// caller's own leased engrams are returned normally. Empty means the caller
	// owns no leases, so any live foreign lease hides its engram.
	CallerOwner string `json:"caller_owner,omitempty" msgpack:"caller_owner,omitempty"`
	// IncludeLeased disables lease-based visibility filtering (admin/debugging).
	IncludeLeased bool `json:"include_leased,omitempty" msgpack:"include_leased,omitempty"`
	// ReadOnly, when true, marks this activation as a pure read (S3): skips
	// activation-log side effects. Recall/Activate never reinforces
	// AccessCount regardless of this flag (COG-12) — this only affects the
	// existing activation-log write path (see engine.go activateCore).
	ReadOnly bool `json:"read_only,omitempty" msgpack:"read_only,omitempty"`
	// AsOf, when set, asks "what was true at T" on the valid-time axis: results
	// are gated by the full half-open interval check [ValidFrom, ValidUntil) at
	// T. Distinct from the since/before filters, which are the TRANSACTION axis
	// (CreatedAt). Nil = default gate ("what is true now": drop only engrams
	// whose closed ValidUntil <= now; a future ValidFrom is NOT hidden).
	AsOf *time.Time `json:"as_of,omitempty" msgpack:"as_of,omitempty"`
	// IncludeInvalid disables the valid-time gate entirely (show history):
	// expired facts are returned, annotated with expired=true.
	IncludeInvalid bool `json:"include_invalid,omitempty" msgpack:"include_invalid,omitempty"`
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
	SourceType      string          `msgpack:"source_type,omitempty" json:"source_type,omitempty"`
	// State is the LifecycleState uint8, so recall can label engram lifecycle state.
	State uint8 `msgpack:"state,omitempty" json:"state,omitempty"`
	// Trust is the TrustLevel uint8. omitempty intentional — see ReadResponse.Trust comment.
	Trust uint8 `msgpack:"trust,omitempty" json:"trust,omitempty"`
	// MemoryType is the stored MemoryType uint8 (see storage.MemoryType).
	// omitempty intentional — absent means TypeFact (0), the zero value.
	MemoryType uint8 `msgpack:"memory_type,omitempty" json:"memory_type,omitempty"`
	// TypeLabel is the writer's free-form type label; empty when none was stored.
	TypeLabel string `msgpack:"type_label,omitempty" json:"type_label,omitempty"`
	// Tags carries the engram's stored tags (S4) so muninn_recall doesn't
	// require a follow-up muninn_read just to see them.
	Tags []string `msgpack:"tags,omitempty" json:"tags,omitempty"`
	// SupersededBy / CurrentVersion are set by supersedes-aware recall when this
	// result is superseded by a newer active engram: SupersededBy is the immediate
	// superseder ULID, CurrentVersion the chain head (the fact to consult now).
	// Always populated when the supersession exists — no annotate flag required —
	// so any transport can say "this is stale, current is X". Empty otherwise.
	SupersededBy   string `msgpack:"superseded_by,omitempty"   json:"superseded_by,omitempty"`
	CurrentVersion string `msgpack:"current_version,omitempty" json:"current_version,omitempty"`
	// Valid-time annotations. ValidFrom is set only when it differs from
	// CreatedAt (an explicitly backdated/forward-dated fact); ValidUntil is set
	// only when the window is closed. Expired marks a fact whose ValidUntil <=
	// now — only reachable in results under include_invalid=true.
	ValidFrom  int64 `msgpack:"valid_from,omitempty" json:"valid_from,omitempty"`
	ValidUntil int64 `msgpack:"valid_until,omitempty" json:"valid_until,omitempty"`
	Expired    bool  `msgpack:"expired,omitempty" json:"expired,omitempty"`
	// Importance is the STORED caller-asserted importance (0 = unset; the
	// presentation layer derives the effective value — see ReadResponse).
	Importance float32 `msgpack:"importance,omitempty" json:"importance,omitempty"`
}

// ScoreComponents breaks down the activation score.
type ScoreComponents struct {
	SemanticSimilarity float32 `msgpack:"semantic_similarity"           json:"semantic_similarity"`
	// SemanticSimilarityRaw is the uncalibrated cosine similarity (COG-26's
	// honesty backstop) — see internal/engine/activation.ScoreComponents.
	// SemanticSimilarityRaw for the full rationale. omitempty is NOT used:
	// zero is a meaningful value (identity transform / truly orthogonal),
	// distinct from "field absent".
	SemanticSimilarityRaw float32 `msgpack:"semantic_similarity_raw"        json:"semantic_similarity_raw"`
	FullTextRelevance     float32 `msgpack:"full_text_relevance"           json:"full_text_relevance"`
	DecayFactor           float32 `msgpack:"decay_factor"                  json:"decay_factor"`
	HebbianBoost          float32 `msgpack:"hebbian_boost"                 json:"hebbian_boost"`
	TransitionBoost       float32 `msgpack:"transition_boost,omitempty"    json:"transition_boost,omitempty"`
	EntityBoost           float32 `msgpack:"entity_boost,omitempty"        json:"entity_boost,omitempty"`
	AccessFrequency       float32 `msgpack:"access_frequency"              json:"access_frequency"`
	Recency               float32 `msgpack:"recency"                       json:"recency"`
	Raw                   float32 `msgpack:"raw"                           json:"raw"`
	Final                 float32 `msgpack:"final"                         json:"final"`
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

// ForgetRequest soft-deletes an engram — unless NotTrueSince is set, in which
// case the engram is invalidated on the valid-time axis instead: ValidUntil is
// stamped and the record stays active (recoverable via as_of/include_invalid).
// Invalidation is always a stamp, never a delete (COG-19).
type ForgetRequest struct {
	ID           string     `msgpack:"id" json:"id"`
	Hard         bool       `msgpack:"hard,omitempty" json:"hard,omitempty"`
	Vault        string     `msgpack:"vault,omitempty" json:"vault,omitempty"`
	NotTrueSince *time.Time `msgpack:"not_true_since,omitempty" json:"not_true_since,omitempty"`
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
