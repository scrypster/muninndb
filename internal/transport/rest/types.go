package rest

import (
	"context"
	"io"
	"time"

	"github.com/scrypster/muninndb/internal/cognitive"
	"github.com/scrypster/muninndb/internal/engine"
	"github.com/scrypster/muninndb/internal/engine/trigger"
	"github.com/scrypster/muninndb/internal/engine/vaultjob"
	"github.com/scrypster/muninndb/internal/plugin"
	"github.com/scrypster/muninndb/internal/storage"
	mbp "github.com/scrypster/muninndb/internal/transport/mbp"
)

// Re-export MBP types for convenience
type HelloRequest = mbp.HelloRequest
type HelloResponse = mbp.HelloResponse
type WriteRequest = mbp.WriteRequest
type WriteResponse = mbp.WriteResponse
type ReadRequest = mbp.ReadRequest
type ReadResponse = mbp.ReadResponse
type ActivateRequest = mbp.ActivateRequest
type ActivateResponse = mbp.ActivateResponse
type ActivationItem = mbp.ActivationItem
// LinkRequest is the REST-specific link request with proper JSON tags.
// The mbp.LinkRequest only has msgpack tags which don't decode from JSON.
type LinkRequest struct {
	SourceID string  `json:"source_id"`
	TargetID string  `json:"target_id"`
	RelType  uint16  `json:"rel_type"`
	Weight   float32 `json:"weight,omitempty"`
	Vault    string  `json:"vault,omitempty"`
}
type LinkResponse = mbp.LinkResponse
type ForgetRequest = mbp.ForgetRequest
type ForgetResponse = mbp.ForgetResponse
type StatRequest = mbp.StatRequest
type StatResponse = mbp.StatResponse
type ErrorCode = mbp.ErrorCode

const (
	ErrOK                   = mbp.ErrOK
	ErrEngramNotFound       = mbp.ErrEngramNotFound
	ErrVaultNotFound        = mbp.ErrVaultNotFound
	ErrInvalidEngram        = mbp.ErrInvalidEngram
	ErrIdempotencyViolation = mbp.ErrIdempotencyViolation
	ErrInvalidAssociation   = mbp.ErrInvalidAssociation
	ErrSubscriptionNotFound = mbp.ErrSubscriptionNotFound
	ErrThresholdInvalid     = mbp.ErrThresholdInvalid
	ErrHopDepthExceeded     = mbp.ErrHopDepthExceeded
	ErrWeightsInvalid       = mbp.ErrWeightsInvalid
	ErrAuthFailed           = mbp.ErrAuthFailed
	ErrVaultForbidden       = mbp.ErrVaultForbidden
	ErrRateLimited          = mbp.ErrRateLimited
	ErrMaxResultsExceeded   = mbp.ErrMaxResultsExceeded
	ErrStorageError         = mbp.ErrStorageError
	ErrIndexError           = mbp.ErrIndexError
	ErrEnrichmentError      = mbp.ErrEnrichmentError
	ErrShardUnavailable     = mbp.ErrShardUnavailable
	ErrInternal             = mbp.ErrInternal
)

// EngineAPI is the interface the REST server requires from the engine.
// All methods accept a context so client disconnects can cancel in-flight operations.
type EngineAPI interface {
	Hello(ctx context.Context, req *HelloRequest) (*HelloResponse, error)
	Write(ctx context.Context, req *WriteRequest) (*WriteResponse, error)
	WriteBatch(ctx context.Context, reqs []*WriteRequest) ([]*WriteResponse, []error)
	Read(ctx context.Context, req *ReadRequest) (*ReadResponse, error)
	Activate(ctx context.Context, req *ActivateRequest) (*ActivateResponse, error)
	Link(ctx context.Context, req *mbp.LinkRequest) (*LinkResponse, error)
	Forget(ctx context.Context, req *ForgetRequest) (*ForgetResponse, error)
	Stat(ctx context.Context, req *StatRequest) (*StatResponse, error)
	ListEngrams(ctx context.Context, req *ListEngramsRequest) (*ListEngramsResponse, error)
	GetEngramLinks(ctx context.Context, req *GetEngramLinksRequest) (*GetEngramLinksResponse, error)
	GetBatchEngramLinks(ctx context.Context, req *BatchGetEngramLinksRequest) (*BatchGetEngramLinksResponse, error)
	ListVaults(ctx context.Context) ([]string, error)
	GetSession(ctx context.Context, req *GetSessionRequest) (*GetSessionResponse, error)
	WorkerStats() cognitive.EngineWorkerStats
	// SubscribeWithDeliver registers a push subscription with a delivery function.
	// Returns the subscription ID. The deliver func is called from a goroutine
	// on each qualifying push; it must be non-blocking.
	SubscribeWithDeliver(ctx context.Context, req *mbp.SubscribeRequest, deliver trigger.DeliverFunc) (string, error)
	Unsubscribe(ctx context.Context, subID string) error
	// ClearVault removes all engrams from the named vault, leaving the vault intact.
	ClearVault(ctx context.Context, vaultName string) error
	// DeleteVault removes the named vault and all its data permanently.
	DeleteVault(ctx context.Context, vaultName string) error
	// StartClone starts an async job to clone sourceVault into a new vault named newName.
	// Returns the job immediately (202 pattern).
	StartClone(ctx context.Context, sourceVault, newName string) (*vaultjob.Job, error)
	// StartMerge starts an async job to merge sourceVault into targetVault.
	// If deleteSource is true, the source vault is deleted after the merge completes.
	StartMerge(ctx context.Context, sourceVault, targetVault string, deleteSource bool) (*vaultjob.Job, error)
	// GetVaultJob returns the status of a vault clone/merge job by ID.
	GetVaultJob(jobID string) (*vaultjob.Job, bool)
	// ExportVault synchronously exports the named vault to w as a .muninn archive.
	ExportVault(ctx context.Context, vaultName, embedderModel string, dimension int, resetMeta bool, w io.Writer) (*storage.ExportResult, error)
	// StartImport starts an async job to import a .muninn archive into a new vault.
	StartImport(ctx context.Context, vaultName, embedderModel string, dimension int, resetMeta bool, r io.Reader) (*vaultjob.Job, error)
	// ReindexFTSVault clears and rebuilds the FTS index for the named vault using
	// the current (Porter2-stemmed) tokenizer. Sets the FTS version marker to 1
	// upon completion. Returns the number of engrams re-indexed.
	ReindexFTSVault(ctx context.Context, vaultName string) (int64, error)
	// StartReembedVault clears stale embeddings and digest flags for the named vault,
	// allowing the RetroactiveProcessor to re-embed everything with the current model.
	// Returns a Job immediately (202 pattern).
	StartReembedVault(ctx context.Context, vaultName, modelName string) (*vaultjob.Job, error)
	// CountEmbedded returns the number of engrams with the DigestEmbed flag set.
	CountEmbedded(ctx context.Context) int64
	// Observability returns the full system observability snapshot.
	Observability(ctx context.Context, version string, uptimeSeconds int64) (*engine.ObservabilitySnapshot, error)
	// GetProcessorStats returns stats for all retroactive processors.
	GetProcessorStats() []plugin.RetroactiveStats
	// RenameVault atomically renames a vault (metadata-only, no engram data changes).
	RenameVault(ctx context.Context, oldName, newName string) error
	// Checkpoint creates a Pebble checkpoint (point-in-time snapshot) at destDir.
	Checkpoint(destDir string) error

	// Extended operations — previously MCP-only.
	Evolve(ctx context.Context, vault, engramID, newContent, reason string) (*EvolveResponse, error)
	Consolidate(ctx context.Context, vault string, ids []string, mergedContent string) (*ConsolidateResponse, error)
	Decide(ctx context.Context, vault, decision, rationale string, alternatives, evidenceIDs []string) (*DecideResponse, error)
	Restore(ctx context.Context, vault, engramID string) (*RestoreResponse, error)
	Traverse(ctx context.Context, vault string, req *TraverseRequest) (*TraverseResponse, error)
	Explain(ctx context.Context, vault string, req *ExplainRequest) (*ExplainResponse, error)
	UpdateState(ctx context.Context, vault, engramID, state, reason string) error
	UpdateTags(ctx context.Context, vault, engramID string, tags []string) error
	ListDeleted(ctx context.Context, vault string, limit int) (*ListDeletedResponse, error)
	RetryEnrich(ctx context.Context, vault, engramID string) (*RetryEnrichResponse, error)
	GetContradictions(ctx context.Context, vault string) (*ContradictionsResponse, error)
	ResolveContradiction(ctx context.Context, vault, idA, idB string) error
	GetGuide(ctx context.Context, vault string) (string, error)
	// ExportGraph builds the entity→relationship graph for the vault.
	// If includeEngrams is true the entity types are enriched from the entity record table.
	ExportGraph(ctx context.Context, vault string, includeEngrams bool) (*engine.ExportGraph, error)
}

// ── Web UI types ─────────────────────────────────────────────────────────

// EngramItem is a summary of an engram for listing.
type EngramItem struct {
	ID         string   `json:"id"`
	Concept    string   `json:"concept"`
	Content    string   `json:"content"`
	Confidence float32  `json:"confidence"`
	Tags       []string `json:"tags,omitempty"`
	Vault      string   `json:"vault"`
	CreatedAt  int64    `json:"created_at"`
	// EmbedDim is the stored embedding dimensionality code (0 = no embedding).
	// 1 = 384-dim, 2 = 768-dim, 3 = 1536-dim, 4 = 3072-dim, 255 = embedded (unknown dimension).
	EmbedDim uint8 `json:"embed_dim,omitempty"`
}

// ListEngramsRequest lists engrams for a vault with optional filtering and sorting.
type ListEngramsRequest struct {
	Vault   string   `json:"vault"`
	Limit   int      `json:"limit"`
	Offset  int      `json:"offset"`
	Sort    string   `json:"sort"`             // "created" (default) or "accessed"
	Tags    []string `json:"tags,omitempty"`   // AND logic — engram must have ALL tags
	State   string   `json:"state,omitempty"`  // lifecycle state filter
	MinConf float32  `json:"min_confidence"`   // minimum confidence (0 = no min)
	MaxConf float32  `json:"max_confidence"`   // maximum confidence (0 = no max)
	Since   string   `json:"since,omitempty"`  // RFC3339 — created after
	Before  string   `json:"before,omitempty"` // RFC3339 — created before
}

// ListEngramsResponse returns paginated engrams.
type ListEngramsResponse struct {
	Engrams []EngramItem `json:"engrams"`
	Total   int          `json:"total"`
	Limit   int          `json:"limit"`
	Offset  int          `json:"offset"`
}

// AssociationItem is a graph edge for the UI.
type AssociationItem struct {
	TargetID          string  `json:"target_id"`
	RelType           uint16  `json:"rel_type"`
	Weight            float32 `json:"weight"`
	CoActivationCount uint32  `json:"co_activation_count"`
	RestoredAt        int64   `json:"restored_at,omitempty"`
}

// GetEngramLinksRequest requests associations for an engram.
type GetEngramLinksRequest struct {
	ID    string `json:"id"`
	Vault string `json:"vault"`
}

// GetEngramLinksResponse returns association edges.
type GetEngramLinksResponse struct {
	Links []AssociationItem `json:"links"`
}

// BatchGetEngramLinksRequest requests associations for multiple engrams in one call.
type BatchGetEngramLinksRequest struct {
	IDs        []string `json:"ids"`
	Vault      string   `json:"vault,omitempty"`
	MaxPerNode int      `json:"max_per_node,omitempty"`
}

// BatchGetEngramLinksResponse returns association edges keyed by source engram ID.
// Every requested ID is present in the map, even if it has zero associations.
type BatchGetEngramLinksResponse struct {
	Links map[string][]AssociationItem `json:"links"`
}

// GetSessionRequest requests recent writes.
type GetSessionRequest struct {
	Vault  string    `json:"vault"`
	Since  time.Time `json:"since"`
	Limit  int       `json:"limit"`
	Offset int       `json:"offset"`
}

// SessionItem is a single session timeline entry.
type SessionItem struct {
	ID        string `json:"id"`
	Concept   string `json:"concept"`
	Content   string `json:"content"`
	CreatedAt int64  `json:"created_at"`
}

// GetSessionResponse returns session timeline entries.
type GetSessionResponse struct {
	Entries []SessionItem `json:"entries"`
	Total   int           `json:"total"`
	Offset  int           `json:"offset"`
	Limit   int           `json:"limit"`
}

// EvolveResponse is returned by the evolve endpoint.
type EvolveResponse struct {
	ID string `json:"id"`
}

// ConsolidateRequest is the body for POST /api/consolidate.
type ConsolidateRequest struct {
	Vault         string   `json:"vault"`
	IDs           []string `json:"ids"`
	MergedContent string   `json:"merged_content"`
}

// ConsolidateResponse is returned by the consolidate endpoint.
type ConsolidateResponse struct {
	ID       string   `json:"id"`
	Archived []string `json:"archived"`
	Warnings []string `json:"warnings,omitempty"`
}

// DecideRequest is the body for POST /api/decide.
type DecideRequest struct {
	Vault        string   `json:"vault"`
	Decision     string   `json:"decision"`
	Rationale    string   `json:"rationale"`
	Alternatives []string `json:"alternatives,omitempty"`
	EvidenceIDs  []string `json:"evidence_ids,omitempty"`
}

// DecideResponse is returned by the decide endpoint.
type DecideResponse struct {
	ID string `json:"id"`
}

// RestoreResponse is returned by the restore endpoint.
type RestoreResponse struct {
	ID       string `json:"id"`
	Concept  string `json:"concept"`
	Restored bool   `json:"restored"`
	State    string `json:"state"`
}

// TraverseRequest is the body for POST /api/traverse.
type TraverseRequest struct {
	Vault           string   `json:"vault"`
	StartID         string   `json:"start_id"`
	MaxHops         int      `json:"max_hops,omitempty"`
	MaxNodes        int      `json:"max_nodes,omitempty"`
	RelTypes        []string `json:"rel_types,omitempty"`
	FollowEntities  bool     `json:"follow_entities,omitempty"`
}

// TraversalNode is a single node in a graph traversal result.
type TraversalNode struct {
	ID      string `json:"id"`
	Concept string `json:"concept"`
	HopDist int    `json:"hop_dist"`
	Summary string `json:"summary,omitempty"`
}

// TraversalEdge is an association edge in a graph traversal result.
type TraversalEdge struct {
	FromID  string  `json:"from_id"`
	ToID    string  `json:"to_id"`
	RelType string  `json:"rel_type"`
	Weight  float32 `json:"weight"`
}

// TraverseResponse is returned by the traverse endpoint.
type TraverseResponse struct {
	Nodes          []TraversalNode `json:"nodes"`
	Edges          []TraversalEdge `json:"edges"`
	TotalReachable int             `json:"total_reachable"`
	QueryMs        float64         `json:"query_ms"`
}

// ExplainRequest is the body for POST /api/explain.
type ExplainRequest struct {
	Vault    string   `json:"vault"`
	EngramID string   `json:"engram_id"`
	Query    []string `json:"query"`
}

// ExplainComponents holds per-component score breakdown.
type ExplainComponents struct {
	FullTextRelevance  float64 `json:"full_text_relevance"`
	SemanticSimilarity float64 `json:"semantic_similarity"`
	DecayFactor        float64 `json:"decay_factor"`
	HebbianBoost       float64 `json:"hebbian_boost"`
	AccessFrequency    float64 `json:"access_frequency"`
	Confidence         float64 `json:"confidence"`
}

// ExplainResponse is returned by the explain endpoint.
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

// SetStateRequest is the body for PUT /api/engrams/{id}/state.
type SetStateRequest struct {
	Vault  string `json:"vault"`
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
}

// SetStateResponse is returned by the state endpoint.
type SetStateResponse struct {
	ID      string `json:"id"`
	State   string `json:"state"`
	Updated bool   `json:"updated"`
}

// UpdateTagsRequest is the body for PUT /api/engrams/{id}/tags.
type UpdateTagsRequest struct {
	Vault string   `json:"vault,omitempty"`
	Tags  []string `json:"tags"`
}

// UpdateTagsResponse is returned by the tags endpoint.
type UpdateTagsResponse struct {
	ID   string   `json:"id"`
	Tags []string `json:"tags"`
}

// DeletedEngramItem is a soft-deleted engram in list results.
type DeletedEngramItem struct {
	ID               string   `json:"id"`
	Concept          string   `json:"concept"`
	DeletedAt        int64    `json:"deleted_at"`
	RecoverableUntil int64    `json:"recoverable_until"`
	Tags             []string `json:"tags,omitempty"`
}

// ListDeletedResponse is returned by the list-deleted endpoint.
type ListDeletedResponse struct {
	Deleted []DeletedEngramItem `json:"deleted"`
	Count   int                 `json:"count"`
}

// RetryEnrichResponse is returned by the retry-enrich endpoint.
type RetryEnrichResponse struct {
	EngramID        string   `json:"engram_id"`
	PluginsQueued   []string `json:"plugins_queued"`
	AlreadyComplete []string `json:"already_complete"`
	Note            string   `json:"note,omitempty"`
}

// ContradictionItem is a pair of contradicting engrams.
type ContradictionItem struct {
	IDa        string `json:"id_a"`
	ConceptA   string `json:"concept_a"`
	IDb        string `json:"id_b"`
	ConceptB   string `json:"concept_b"`
	DetectedAt int64  `json:"detected_at"`
}

// ContradictionsResponse is returned by the contradictions endpoint.
type ContradictionsResponse struct {
	Contradictions []ContradictionItem `json:"contradictions"`
}

// ResolveContradictionRequest is the body for POST /api/admin/contradictions/resolve.
type ResolveContradictionRequest struct {
	Vault string `json:"vault"`
	IDA   string `json:"id_a"`
	IDB   string `json:"id_b"`
}

// ResolveContradictionResponse is returned by the resolve-contradiction endpoint.
type ResolveContradictionResponse struct {
	Resolved bool `json:"resolved"`
}

// GuideResponse is returned by the guide endpoint.
type GuideResponse struct {
	Guide string `json:"guide"`
}

// ErrorResponse is the standard error format returned by REST endpoints.
type ErrorResponse struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail contains error information.
type ErrorDetail struct {
	Code      ErrorCode `json:"code"`
	Message   string    `json:"message"`
	RequestID string    `json:"request_id,omitempty"`
}

// HealthResponse is returned by the health check endpoint.
type HealthResponse struct {
	Status        string `json:"status"`
	Version       string `json:"version"`
	UptimeSeconds int64  `json:"uptime_seconds"`
	DBWritable    bool   `json:"db_writable"`
}

// ReadyResponse is returned by the ready check endpoint.
type ReadyResponse struct {
	Status string `json:"status"`
}

// EntityGraphResponse is returned by GET /api/admin/entity-graph.
type EntityGraphResponse struct {
	Nodes []EntityGraphNode `json:"nodes"`
	Edges []EntityGraphEdge `json:"edges"`
}

// EntityGraphNode is a node in the entity graph.
type EntityGraphNode struct {
	ID   string `json:"id"`
	Type string `json:"type"`
}

// EntityGraphEdge is an edge in the entity graph.
type EntityGraphEdge struct {
	From    string  `json:"from"`
	To      string  `json:"to"`
	RelType string  `json:"rel_type"`
	Weight  float32 `json:"weight"`
}
