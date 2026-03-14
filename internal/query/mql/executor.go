package mql

import (
	"context"
	"fmt"
	"strings"

	"github.com/scrypster/muninndb/internal/query"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// Engine is the minimal interface needed by the executor.
type Engine interface {
	Activate(ctx context.Context, req *mbp.ActivateRequest) (*mbp.ActivateResponse, error)
}

// EpisodicStoreInterface is the interface for episodic memory operations.
type EpisodicStoreInterface interface {
	ListFrames(ctx context.Context, ws [8]byte, episodeID interface{}) (interface{}, error)
}

// WorkingMemoryManagerInterface is the interface for working memory operations.
type WorkingMemoryManagerInterface interface {
	Get(sessionID string) (interface{}, bool)
}

// ConsolidationWorkerInterface is the interface for consolidation operations.
type ConsolidationWorkerInterface interface {
	RunOnce(ctx context.Context, vault string) (interface{}, error)
}

// ExecuteQuery dispatches query execution based on query type.
func ExecuteQuery(ctx context.Context, eng Engine, q Query, episodicStore EpisodicStoreInterface, workingManager WorkingMemoryManagerInterface, consolidationWorker ConsolidationWorkerInterface) (interface{}, error) {
	if q == nil {
		return nil, fmt.Errorf("query is nil")
	}

	switch query := q.(type) {
	case *ActivateQuery:
		return Execute(ctx, eng, query)
	case *RecallEpisodeQuery:
		return executeRecallEpisode(ctx, episodicStore, query)
	case *TraverseQuery:
		return executeTraverse(ctx, eng, query)
	case *ConsolidateQuery:
		return executeConsolidate(ctx, consolidationWorker, query)
	case *WorkingMemoryQuery:
		return executeWorkingMemory(ctx, workingManager, query)
	default:
		return nil, fmt.Errorf("unknown query type: %T", q)
	}
}

// Execute translates an ActivateQuery into an engine.ActivateRequest and executes it.
func Execute(ctx context.Context, eng Engine, q *ActivateQuery) (*mbp.ActivateResponse, error) {
	if eng == nil {
		return nil, fmt.Errorf("engine is nil")
	}
	if q == nil {
		return nil, fmt.Errorf("query is nil")
	}

	// Build ActivateRequest
	req := &mbp.ActivateRequest{
		Vault:   q.Vault,
		Context: q.Context,
	}

	// Set MaxResults
	if q.MaxResults > 0 {
		req.MaxResults = q.MaxResults
	} else {
		req.MaxResults = 20 // default
	}

	// Set MaxHops
	if q.Hops > 0 {
		req.MaxHops = q.Hops
	} else {
		req.MaxHops = 2 // default
	}

	// Build Filter from WHERE predicate
	var structuredFilter *query.Filter
	if q.Where != nil {
		filter := &query.Filter{}
		if err := buildFilter(filter, q.Where); err != nil {
			return nil, err
		}
		// Convert filter to mbp.Filter format
		req.Filters = convertFilterToMBPFilters(filter)
		// Save the structured filter for optional post-retrieval use
		structuredFilter = filter
	}

	// Set MinRelevance if provided
	if q.MinRelevance > 0 {
		// MinRelevance is set via threshold in the request, not in Filters
		// For now, we store it as a filter constraint
		if len(req.Filters) == 0 {
			req.Filters = make([]mbp.Filter, 0)
		}
		req.Filters = append(req.Filters, mbp.Filter{
			Field: "relevance",
			Op:    ">=",
			Value: q.MinRelevance,
		})
	}

	// Execute activation with structured filter if available.
	// The engine may support a more efficient method that wires the filter
	// to activation.Run as StructuredFilter for proper post-retrieval filtering.
	if structuredFilter != nil {
		// Try to use the extended interface if available
		if extEng, ok := eng.(interface {
			ActivateWithStructuredFilter(ctx context.Context, req *mbp.ActivateRequest, structuredFilter interface{}) (*mbp.ActivateResponse, error)
		}); ok {
			return extEng.ActivateWithStructuredFilter(ctx, req, structuredFilter)
		}
	}

	// Fall back to standard Activate
	return eng.Activate(ctx, req)
}

// buildFilter walks the predicate tree and populates the Filter struct.
func buildFilter(f *query.Filter, pred Predicate) error {
	switch p := pred.(type) {
	case *StatePredicate:
		state, err := storage.ParseLifecycleState(p.State)
		if err != nil {
			return fmt.Errorf("invalid state: %w", err)
		}
		f.States = append(f.States, state)

	case *ScorePredicate:
		switch p.Field {
		case "relevance":
			if p.Op == ">" {
				f.MinRelevance = p.Value
			} else if p.Op == ">=" {
				f.MinRelevance = p.Value
			}
		case "confidence":
			if p.Op == ">" {
				f.MinConfidence = p.Value
			} else if p.Op == ">=" {
				f.MinConfidence = p.Value
			}
		default:
			return fmt.Errorf("unknown score field: %s", p.Field)
		}

	case *TagPredicate:
		f.Tags = append(f.Tags, p.Tag)

	case *CreatorPredicate:
		f.Creator = p.Creator

	case *CreatedAfterPredicate:
		f.CreatedAfter = &p.After

	case *AndPredicate:
		// For AND, both predicates must match.
		// We build into the same filter (all constraints are ANDed together).
		if err := buildFilter(f, p.Left); err != nil {
			return err
		}
		if err := buildFilter(f, p.Right); err != nil {
			return err
		}

	case *OrPredicate:
		// OR is more complex: we'd need to return multiple filters or handle it differently.
		// For now, we raise an error to avoid silent misinterpretation.
		// In a production system, you'd flatten OR predicates or use a different approach.
		return fmt.Errorf("OR predicates not yet supported in filter conversion")

	case *ProvenanceSourcePredicate:
		// Store provenance source for later use
		// For now, just note that it exists
		_ = p.Source

	case *ProvenanceAgentPredicate:
		// Store provenance agent for later use
		// For now, just note that it exists
		_ = p.Agent

	default:
		return fmt.Errorf("unknown predicate type")
	}

	return nil
}

// convertFilterToMBPFilters converts a query.Filter to the mbp.Filter format
// expected by ActivateRequest.
func convertFilterToMBPFilters(f *query.Filter) []mbp.Filter {
	var filters []mbp.Filter

	// State filters — use storage.LifecycleState directly so passesMetaFilter's
	// type assertion (f.Value.(storage.LifecycleState)) succeeds.
	for _, state := range f.States {
		filters = append(filters, mbp.Filter{
			Field: "state",
			Op:    "=",
			Value: state,
		})
	}

	// Tag filters
	for _, tag := range f.Tags {
		filters = append(filters, mbp.Filter{
			Field: "tag",
			Op:    "=",
			Value: tag,
		})
	}

	// Creator filter
	if f.Creator != "" {
		filters = append(filters, mbp.Filter{
			Field: "creator",
			Op:    "=",
			Value: f.Creator,
		})
	}

	// CreatedAfter filter — use time.Time so extractTimeBounds' type assertion
	// (f.Value.(time.Time)) succeeds and enables time-bounded candidate injection.
	if f.CreatedAfter != nil {
		filters = append(filters, mbp.Filter{
			Field: "created_after",
			Op:    ">=",
			Value: *f.CreatedAfter,
		})
	}

	// Score thresholds
	if f.MinRelevance > 0 {
		filters = append(filters, mbp.Filter{
			Field: "relevance",
			Op:    ">=",
			Value: f.MinRelevance,
		})
	}
	if f.MinConfidence > 0 {
		filters = append(filters, mbp.Filter{
			Field: "confidence",
			Op:    ">=",
			Value: f.MinConfidence,
		})
	}
	if f.MinStability > 0 {
		filters = append(filters, mbp.Filter{
			Field: "stability",
			Op:    ">=",
			Value: f.MinStability,
		})
	}

	return filters
}

// executeRecallEpisode executes a RECALL EPISODE query.
func executeRecallEpisode(ctx context.Context, episodicStore EpisodicStoreInterface, q *RecallEpisodeQuery) (interface{}, error) {
	if episodicStore == nil {
		return nil, fmt.Errorf("episodic store is nil")
	}

	// For now, return a placeholder response. In production, parse episodeID and call ListFrames.
	return map[string]interface{}{
		"query_type": "recall_episode",
		"episode_id": q.EpisodeID,
		"frames":     q.Frames,
		"status":     "not_yet_implemented",
	}, nil
}

// executeTraverse executes a TRAVERSE query.
func executeTraverse(ctx context.Context, eng Engine, q *TraverseQuery) (interface{}, error) {
	if eng == nil {
		return nil, fmt.Errorf("engine is nil")
	}

	// For now, return a placeholder response. In production, use engine to walk graph.
	return map[string]interface{}{
		"query_type":  "traverse",
		"start_id":    q.StartID,
		"hops":        q.Hops,
		"min_weight":  q.MinWeight,
		"status":      "not_yet_implemented",
	}, nil
}

// executeConsolidate executes a CONSOLIDATE VAULT query.
func executeConsolidate(ctx context.Context, consolidationWorker ConsolidationWorkerInterface, q *ConsolidateQuery) (interface{}, error) {
	if consolidationWorker == nil {
		return nil, fmt.Errorf("consolidation worker is nil")
	}

	// For now, return a placeholder response. In production, call consolidationWorker.RunOnce.
	return map[string]interface{}{
		"query_type": "consolidate",
		"vault":      q.Vault,
		"dry_run":    q.DryRun,
		"status":     "not_yet_implemented",
	}, nil
}

// executeWorkingMemory executes a WORKING_MEMORY SESSION query.
func executeWorkingMemory(ctx context.Context, workingManager WorkingMemoryManagerInterface, q *WorkingMemoryQuery) (interface{}, error) {
	if workingManager == nil {
		return nil, fmt.Errorf("working memory manager is nil")
	}

	// For now, return a placeholder response. In production, call workingManager.Get.
	return map[string]interface{}{
		"query_type": "working_memory",
		"session_id": q.SessionID,
		"status":     "not_yet_implemented",
	}, nil
}

// ParseAndExecute is a convenience function that parses an MQL string and executes it.
func ParseAndExecute(ctx context.Context, eng Engine, mqlStr string) (*mbp.ActivateResponse, error) {
	// Trim leading/trailing whitespace
	mqlStr = strings.TrimSpace(mqlStr)

	query, err := Parse(mqlStr)
	if err != nil {
		return nil, err
	}

	// For backward compatibility with ActivateQuery only
	activateQuery, ok := query.(*ActivateQuery)
	if !ok {
		return nil, fmt.Errorf("ParseAndExecute only supports ACTIVATE queries; got %T", query)
	}

	return Execute(ctx, eng, activateQuery)
}
