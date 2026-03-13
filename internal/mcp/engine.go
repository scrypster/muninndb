package mcp

import (
	"context"
	"time"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/engine"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// EngineInterface is the API surface the MCP layer uses.
// The first 6 methods delegate directly to engine via mbp types (stable internal contract).
// The last 5 methods are higher-level operations with no MBP counterpart.
// Implemented by mcpEngineAdapter in internal/mcp/engine_adapter.go.
type EngineInterface interface {
	// MBP-backed methods
	Write(ctx context.Context, req *mbp.WriteRequest) (*mbp.WriteResponse, error)
	WriteBatch(ctx context.Context, reqs []*mbp.WriteRequest) ([]*mbp.WriteResponse, []error)
	Activate(ctx context.Context, req *mbp.ActivateRequest) (*mbp.ActivateResponse, error)
	Read(ctx context.Context, req *mbp.ReadRequest) (*mbp.ReadResponse, error)
	Forget(ctx context.Context, req *mbp.ForgetRequest) (*mbp.ForgetResponse, error)
	Link(ctx context.Context, req *mbp.LinkRequest) (*mbp.LinkResponse, error)
	Stat(ctx context.Context, req *mbp.StatRequest) (*mbp.StatResponse, error)

	// Higher-level cognitive operations (tools 1-11)
	GetContradictions(ctx context.Context, vault string) ([]ContradictionPair, error)
	Evolve(ctx context.Context, vault, oldID, newContent, reason string) (*WriteResult, error)
	Consolidate(ctx context.Context, vault string, ids []string, mergedContent string) (*ConsolidateResult, error)
	Session(ctx context.Context, vault string, since time.Time) (*SessionSummary, error)
	Decide(ctx context.Context, vault, decision, rationale string, alternatives, evidenceIDs []string) (*WriteResult, error)

	// Epic 18: tools 12-17

	// Restore un-deletes a soft-deleted engram within the 7-day recovery window.
	// Returns an error if the engram does not exist or the window has passed.
	Restore(ctx context.Context, vault string, id string) (*RestoreResult, error)

	// Traverse performs a bounded BFS from the starting engram, following association edges.
	Traverse(ctx context.Context, vault string, req *TraverseRequest) (*TraverseResult, error)

	// Explain returns the score breakdown for why a specific engram would be returned
	// for a given query context.
	Explain(ctx context.Context, vault string, req *ExplainRequest) (*ExplainResult, error)

	// UpdateState transitions an engram to a new lifecycle state.
	// Invalid transitions return an error describing the valid next states.
	UpdateState(ctx context.Context, vault string, id string, state string, reason string) error

	// ListDeleted returns engrams that have been soft-deleted and are still within
	// the 7-day recovery window, ordered by deletion time descending.
	ListDeleted(ctx context.Context, vault string, limit int) ([]DeletedEngram, error)

	// RetryEnrich re-queues an engram for enrichment by all active plugins that have
	// not yet processed it. Returns an error if the engram is not found.
	RetryEnrich(ctx context.Context, vault string, id string) (*RetryEnrichResult, error)

	// GetVaultPlasticity returns the resolved plasticity config for a vault.
	GetVaultPlasticity(ctx context.Context, vault string) (*auth.ResolvedPlasticity, error)

	// RememberTree writes a nested engram tree in one call.
	// All engram records are committed atomically in a single Pebble batch (Phase 1).
	// Association and ordinal keys are wired in Phase 2 after the batch commits.
	RememberTree(ctx context.Context, req *RememberTreeRequest) (*RememberTreeResult, error)

	// RecallTree returns the complete ordered tree rooted at rootID.
	// maxDepth=0 means unlimited depth. limit caps children per node per level (0 = no limit).
	// When includeCompleted=false, completed nodes and their subtrees are excluded.
	RecallTree(ctx context.Context, vault, rootID string, maxDepth, limit int, includeCompleted bool) (*RecallTreeResult, error)

	// AddChild adds a single engram as a child of parentID, writing the is_part_of
	// association and ordinal key. ordinal=nil appends after the last existing child.
	AddChild(ctx context.Context, vault, parentID string, child *AddChildRequest) (*AddChildResult, error)

	// CountChildren returns the number of direct children registered under engramID
	// via the ordinal index. Returns 0 if the engram has no children or if the
	// engramID is invalid.
	CountChildren(ctx context.Context, vault, engramID string) (int, error)

	// GetEnrichmentMode returns a string describing the active enrichment configuration.
	// Returns "none" when no enrich plugin is configured, "plugin:<name>" when a plugin
	// is active, or "inline" when only inline enrichment is available.
	GetEnrichmentMode(ctx context.Context) string

	// WhereLeftOff returns the most recently accessed active engrams, sorted by
	// LastAccess descending. limit caps results (default 10, max 50).
	WhereLeftOff(ctx context.Context, vault string, limit int) ([]WhereLeftOffEntry, error)

	// FindByEntity returns all engrams that mention the given entity name,
	// scanned from the 0x23 reverse index. Results are limited to limit entries.
	FindByEntity(ctx context.Context, vault, entityName string, limit int) ([]*storage.Engram, error)

	// CheckIdempotency looks up an op_id receipt. Returns nil, nil if not found.
	CheckIdempotency(ctx context.Context, opID string) (*storage.IdempotencyReceipt, error)

	// WriteIdempotency stores an idempotency receipt (op_id → engramID).
	WriteIdempotency(ctx context.Context, opID, engramID string) error

	// SetEntityState sets the lifecycle state of a named entity, and optionally
	// corrects its type. entityType may be empty (preserves existing type).
	// For state="merged", mergedInto must be the canonical entity name.
	SetEntityState(ctx context.Context, entityName, state, mergedInto, entityType string) error

	// SetEntityStateBatch applies multiple entity state updates sequentially.
	// Returns one error per operation (nil = success). Partial success is preserved.
	SetEntityStateBatch(ctx context.Context, ops []engine.EntityStateOp) []error

	// GetEntityClusters returns entity pairs that frequently co-occur in the same engrams,
	// sorted by count descending. Only pairs with count >= minCount are returned.
	// Results are capped at topN entries.
	GetEntityClusters(ctx context.Context, vault string, minCount, topN int) ([]EntityClusterResult, error)

	// ExportGraph builds the entity→relationship graph for vault and returns
	// the raw graph data. The caller chooses the output format.
	ExportGraph(ctx context.Context, vault string, includeEngrams bool) (*engine.ExportGraph, error)

	// GetEntityTimeline returns a chronological view of when an entity first appeared
	// in memory and how it has evolved. Results are ordered by creation time (oldest first)
	// and capped at limit entries.
	GetEntityTimeline(ctx context.Context, vault, entityName string, limit int) (*engine.EntityTimeline, error)

	// FindSimilarEntities scans all entity names in vault and returns pairs whose
	// trigram similarity is >= threshold. Results are capped at topN entries.
	FindSimilarEntities(ctx context.Context, vault string, threshold float64, topN int) ([]engine.SimilarEntityPair, error)

	// MergeEntity merges entityA into entityB (canonical). When dryRun=true it
	// reports what would happen without writing any data.
	MergeEntity(ctx context.Context, vault, entityA, entityB string, dryRun bool) (*engine.MergeEntityResult, error)

	// ReplayEnrichment re-runs the enrichment pipeline for active engrams in a vault
	// that are missing specific digest stage flags.
	// stages is a subset of ["entities","relationships","classification","summary"].
	// limit caps how many engrams are processed in this call (1-200, default 50).
	// When dryRun=true, only counts engrams needing enrichment without writing.
	ReplayEnrichment(ctx context.Context, vault string, stages []string, limit int, dryRun bool) (*engine.ReplayEnrichmentResult, error)

	// GetProvenance returns the ordered audit log for an engram.
	// Returns an empty slice (not error) if no entries exist.
	GetProvenance(ctx context.Context, vault, id string) ([]ProvenanceEntry, error)

	// RecordFeedback records an explicit negative feedback signal for an engram.
	// useful=false means the engram was retrieved but was not helpful.
	// The engine computes the ScoreVector internally.
	RecordFeedback(ctx context.Context, vault, engramID string, useful bool) error

	// GetEntityAggregate returns the full aggregate view for a named entity.
	// limit caps engrams returned (0 = default 20).
	GetEntityAggregate(ctx context.Context, vault, entityName string, limit int) (*EntityAggregate, error)

	// ListEntities returns EntitySummary records sorted by mention_count desc.
	// state filters by lifecycle state ("active", "deprecated", "merged", "resolved", "" = all).
	// limit caps results (0 = default 50).
	ListEntities(ctx context.Context, vault string, limit int, state string) ([]EntitySummary, error)
}
