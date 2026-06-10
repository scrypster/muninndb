package muninn

// Engram represents a single memory unit in MuninnDB.
type Engram struct {
	ID          string   `json:"id"`
	Concept     string   `json:"concept"`
	Content     string   `json:"content"`
	Confidence  float64  `json:"confidence"`
	Relevance   float64  `json:"relevance"`
	Stability   float64  `json:"stability"`
	AccessCount int      `json:"access_count"`
	Tags        []string `json:"tags"`
	State       int      `json:"state"`
	CreatedAt   int64    `json:"created_at"`
	UpdatedAt   int64    `json:"updated_at"`
	LastAccess  *int64   `json:"last_access,omitempty"`
	MemoryType  int      `json:"memory_type,omitempty"`
	TypeLabel   string   `json:"type_label,omitempty"`
}

// InlineEntity is a caller-provided entity for inline enrichment.
type InlineEntity struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// InlineRelationship is a caller-provided relationship for inline enrichment.
type InlineRelationship struct {
	TargetID string  `json:"target_id"`
	Relation string  `json:"relation"`
	Weight   float64 `json:"weight,omitempty"`
}

// WriteRequest represents a request to write an engram.
type WriteRequest struct {
	Vault         string                 `json:"vault"`
	Concept       string                 `json:"concept"`
	Content       string                 `json:"content"`
	Tags          []string               `json:"tags,omitempty"`
	Confidence    float64                `json:"confidence,omitempty"`
	Stability     float64                `json:"stability,omitempty"`
	Embedding     []float64              `json:"embedding,omitempty"`
	Associations  map[string]interface{} `json:"associations,omitempty"`
	MemoryType    *int                   `json:"memory_type,omitempty"`
	TypeLabel     string                 `json:"type_label,omitempty"`
	Summary       string                 `json:"summary,omitempty"`
	Entities      []InlineEntity         `json:"entities,omitempty"`
	Relationships []InlineRelationship   `json:"relationships,omitempty"`
}

// WriteResponse represents a response from writing an engram.
type WriteResponse struct {
	ID        string `json:"id"`
	CreatedAt int64  `json:"created_at"`
	Hint      string `json:"hint,omitempty"`
}

// BatchWriteResult holds the result for a single item in a batch write.
type BatchWriteResult struct {
	Index  int    `json:"index"`
	ID     string `json:"id,omitempty"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// BatchWriteResponse holds the response from a batch write operation.
type BatchWriteResponse struct {
	Results []BatchWriteResult `json:"results"`
}

// ActivateRequest represents a request to activate memory.
type ActivateRequest struct {
	Vault      string   `json:"vault"`
	Context    []string `json:"context"`
	MaxResults int      `json:"max_results,omitempty"`
	Threshold  float64  `json:"threshold,omitempty"`
	MaxHops    int      `json:"max_hops,omitempty"`
	IncludeWhy bool     `json:"include_why,omitempty"`
	BriefMode  string   `json:"brief_mode,omitempty"`
}

// ActivationItem represents a single activated memory item.
type ActivationItem struct {
	ID         string   `json:"id"`
	Concept    string   `json:"concept"`
	Content    string   `json:"content"`
	Score      float64  `json:"score"`
	Confidence float64  `json:"confidence"`
	Why        *string  `json:"why,omitempty"`
	HopPath    []string `json:"hop_path,omitempty"`
	Dormant    bool     `json:"dormant,omitempty"`
	MemoryType int      `json:"memory_type,omitempty"`
	TypeLabel  string   `json:"type_label,omitempty"`
}

// BriefSentence represents a sentence extracted by brief mode.
type BriefSentence struct {
	EngramID string  `json:"engram_id"`
	Text     string  `json:"text"`
	Score    float64 `json:"score"`
}

// ActivateResponse represents a response from activating memory.
type ActivateResponse struct {
	QueryID     string           `json:"query_id"`
	TotalFound  int              `json:"total_found"`
	Activations []ActivationItem `json:"activations"`
	LatencyMs   float64          `json:"latency_ms,omitempty"`
	Brief       []BriefSentence  `json:"brief,omitempty"`
}

// CoherenceResult contains coherence metrics for a vault.
type CoherenceResult struct {
	Score                float64 `json:"score"`
	OrphanRatio          float64 `json:"orphan_ratio"`
	ContradictionDensity float64 `json:"contradiction_density"`
	DuplicationPressure  float64 `json:"duplication_pressure"`
	TemporalVariance     float64 `json:"temporal_variance"`
	TotalEngrams         int     `json:"total_engrams"`
}

// StatsResponse represents the response from the stats endpoint.
type StatsResponse struct {
	EngramCount  int64                      `json:"engram_count"`
	VaultCount   int                        `json:"vault_count"`
	StorageBytes int64                      `json:"storage_bytes"`
	Coherence    map[string]CoherenceResult `json:"coherence,omitempty"`
}

// LinkRequest represents a request to link two engrams.
type LinkRequest struct {
	Vault    string  `json:"vault"`
	SourceID string  `json:"source_id"`
	TargetID string  `json:"target_id"`
	RelType  int     `json:"rel_type"`
	Weight   float64 `json:"weight"`
}

// ForgetRequest represents a request to forget an engram.
type ForgetRequest struct {
	ID    string `json:"id"`
	Vault string `json:"vault"`
}

// Push represents an SSE push event from subscription.
type Push struct {
	SubscriptionID string  `json:"subscription_id"`
	Trigger        string  `json:"trigger"`
	PushNumber     int     `json:"push_number"`
	EngramID       *string `json:"engram_id,omitempty"`
	At             *int64  `json:"at,omitempty"`
}

// HealthResponse represents the response from the health endpoint.
type HealthResponse struct {
	Status string `json:"status"`
}

// ErrorResponse represents an error response from the API.
type ErrorResponse struct {
	Error   string `json:"error"`
	Code    int    `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

// EvolveResponse represents a response from evolving an engram.
type EvolveResponse struct {
	ID string `json:"id"`
}

// ConsolidateResponse represents a response from consolidating engrams.
type ConsolidateResponse struct {
	ID       string   `json:"id"`
	Archived []string `json:"archived"`
	Warnings []string `json:"warnings,omitempty"`
}

// DecideResponse represents a response from recording a decision.
type DecideResponse struct {
	ID string `json:"id"`
}

// RestoreResponse represents a response from restoring a deleted engram.
type RestoreResponse struct {
	ID       string `json:"id"`
	Concept  string `json:"concept"`
	Restored bool   `json:"restored"`
	State    string `json:"state"`
}

// TraversalNode is a node in a graph traversal result.
type TraversalNode struct {
	ID      string `json:"id"`
	Concept string `json:"concept"`
	HopDist int    `json:"hop_dist"`
	Summary string `json:"summary,omitempty"`
}

// TraversalEdge is an edge in a graph traversal result.
type TraversalEdge struct {
	FromID  string  `json:"from_id"`
	ToID    string  `json:"to_id"`
	RelType string  `json:"rel_type"`
	Weight  float32 `json:"weight"`
}

// TraverseResponse represents a response from graph traversal.
type TraverseResponse struct {
	Nodes          []TraversalNode `json:"nodes"`
	Edges          []TraversalEdge `json:"edges"`
	TotalReachable int             `json:"total_reachable"`
	QueryMs        float64         `json:"query_ms"`
}

// ExplainComponents holds the scoring breakdown for an explain result.
type ExplainComponents struct {
	FullTextRelevance  float64 `json:"full_text_relevance"`
	SemanticSimilarity float64 `json:"semantic_similarity"`
	DecayFactor        float64 `json:"decay_factor"`
	HebbianBoost       float64 `json:"hebbian_boost"`
	AccessFrequency    float64 `json:"access_frequency"`
	Confidence         float64 `json:"confidence"`
}

// ExplainResponse represents a response from explaining an engram's score.
type ExplainResponse struct {
	EngramID    string            `json:"engram_id"`
	Concept     string            `json:"concept"`
	FinalScore  float64           `json:"final_score"`
	Components  ExplainComponents `json:"components"`
	FTSMatches  []string          `json:"fts_matches"`
	AssocPath   []string          `json:"assoc_path"`
	WouldReturn bool              `json:"would_return"`
	Threshold   float64           `json:"threshold"`
}

// SetStateResponse represents a response from setting engram state.
type SetStateResponse struct {
	ID      string `json:"id"`
	State   string `json:"state"`
	Updated bool   `json:"updated"`
}

// DeletedEngram represents a soft-deleted engram.
type DeletedEngram struct {
	ID               string   `json:"id"`
	Concept          string   `json:"concept"`
	DeletedAt        int64    `json:"deleted_at"`
	RecoverableUntil int64    `json:"recoverable_until"`
	Tags             []string `json:"tags,omitempty"`
}

// ListDeletedResponse represents a response from listing deleted engrams.
type ListDeletedResponse struct {
	Deleted []DeletedEngram `json:"deleted"`
	Count   int             `json:"count"`
}

// RetryEnrichResponse represents a response from retrying enrichment.
type RetryEnrichResponse struct {
	EngramID        string   `json:"engram_id"`
	PluginsQueued   []string `json:"plugins_queued"`
	AlreadyComplete []string `json:"already_complete"`
	Note            string   `json:"note,omitempty"`
}

// ContradictionItem represents a detected contradiction between two engrams.
type ContradictionItem struct {
	IDa        string `json:"id_a"`
	ConceptA   string `json:"concept_a"`
	IDb        string `json:"id_b"`
	ConceptB   string `json:"concept_b"`
	DetectedAt int64  `json:"detected_at"`
}

// ContradictionsResponse represents a response from listing contradictions.
type ContradictionsResponse struct {
	Contradictions []ContradictionItem `json:"contradictions"`
}

// GuideResponse represents a response from the guide endpoint.
type GuideResponse struct {
	Guide string `json:"guide"`
}

// EngramItem represents an engram summary in a list response.
type EngramItem struct {
	ID         string   `json:"id"`
	Concept    string   `json:"concept"`
	Content    string   `json:"content"`
	Confidence float32  `json:"confidence"`
	EmbedDim   uint8    `json:"embed_dim,omitempty"`
	Tags       []string `json:"tags,omitempty"`
	Vault      string   `json:"vault"`
	CreatedAt  int64    `json:"created_at"`
}

// ListEngramsResponse represents a response from listing engrams.
type ListEngramsResponse struct {
	Engrams []EngramItem `json:"engrams"`
	Total   int          `json:"total"`
	Limit   int          `json:"limit"`
	Offset  int          `json:"offset"`
}

// AssociationItem represents an association/link from an engram.
type AssociationItem struct {
	TargetID          string  `json:"target_id"`
	RelType           uint16  `json:"rel_type"`
	Weight            float32 `json:"weight"`
	CoActivationCount uint32  `json:"co_activation_count,omitempty"`
	RestoredAt        int64   `json:"restored_at,omitempty"`
}

// SessionEntry represents an entry in session activity.
type SessionEntry struct {
	ID        string `json:"id"`
	Concept   string `json:"concept"`
	Content   string `json:"content,omitempty"`
	CreatedAt int64  `json:"created_at"`
}

// SessionResponse represents a response from session activity query.
type SessionResponse struct {
	Entries []SessionEntry `json:"entries"`
	Total   int            `json:"total"`
	Offset  int            `json:"offset"`
	Limit   int            `json:"limit"`
}
