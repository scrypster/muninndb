package mcp

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/engine"
	"github.com/scrypster/muninndb/internal/plugin"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// mcpEngineAdapter adapts *engine.Engine to mcp.EngineInterface.
// Implemented here in internal/mcp/engine_adapter.go.
type mcpEngineAdapter struct {
	eng      *engine.Engine
	enricher plugin.EnrichPlugin
}

// NewEngineAdapter returns an EngineInterface backed by eng with optional enricher.
func NewEngineAdapter(eng *engine.Engine, enricher plugin.EnrichPlugin) EngineInterface {
	return &mcpEngineAdapter{eng: eng, enricher: enricher}
}

func (a *mcpEngineAdapter) Write(ctx context.Context, req *mbp.WriteRequest) (*mbp.WriteResponse, error) {
	return a.eng.Write(ctx, req)
}
func (a *mcpEngineAdapter) WriteBatch(ctx context.Context, reqs []*mbp.WriteRequest) ([]*mbp.WriteResponse, []error) {
	return a.eng.WriteBatch(ctx, reqs)
}
func (a *mcpEngineAdapter) Activate(ctx context.Context, req *mbp.ActivateRequest) (*mbp.ActivateResponse, error) {
	return a.eng.Activate(ctx, req)
}
func (a *mcpEngineAdapter) Read(ctx context.Context, req *mbp.ReadRequest) (*mbp.ReadResponse, error) {
	return a.eng.Read(ctx, req)
}
func (a *mcpEngineAdapter) Forget(ctx context.Context, req *mbp.ForgetRequest) (*mbp.ForgetResponse, error) {
	return a.eng.Forget(ctx, req)
}
func (a *mcpEngineAdapter) Link(ctx context.Context, req *mbp.LinkRequest) (*mbp.LinkResponse, error) {
	return a.eng.Link(ctx, req)
}
func (a *mcpEngineAdapter) Stat(ctx context.Context, req *mbp.StatRequest) (*mbp.StatResponse, error) {
	return a.eng.Stat(ctx, req)
}
func (a *mcpEngineAdapter) GetContradictions(ctx context.Context, vault string) ([]ContradictionPair, error) {
	pairs, err := a.eng.GetContradictions(ctx, vault)
	if err != nil {
		return nil, err
	}
	result := make([]ContradictionPair, len(pairs))
	for i, p := range pairs {
		result[i] = ContradictionPair{IDa: p[0].String(), IDb: p[1].String()}
	}
	return result, nil
}
func (a *mcpEngineAdapter) Evolve(ctx context.Context, vault, oldID, newContent, reason string) (*WriteResult, error) {
	id, err := a.eng.Evolve(ctx, vault, oldID, newContent, reason)
	if err != nil {
		return nil, err
	}
	return &WriteResult{ID: id.String()}, nil
}
func (a *mcpEngineAdapter) Consolidate(ctx context.Context, vault string, ids []string, merged string) (*ConsolidateResult, error) {
	newID, archived, warnings, err := a.eng.Consolidate(ctx, vault, ids, merged)
	if err != nil {
		return nil, err
	}
	return &ConsolidateResult{ID: newID.String(), Archived: archived, Warnings: warnings}, nil
}
func (a *mcpEngineAdapter) Session(ctx context.Context, vault string, since time.Time) (*SessionSummary, error) {
	res, err := a.eng.Session(ctx, vault, since)
	if err != nil {
		return nil, err
	}
	summary := &SessionSummary{Since: since}
	for _, w := range res.Writes {
		summary.Writes = append(summary.Writes, SessionEntry{
			ID:        w.ID,
			Concept:   w.Concept,
			CreatedAt: w.At,
		})
	}
	return summary, nil
}
func (a *mcpEngineAdapter) Decide(ctx context.Context, vault, decision, rationale string, alternatives, evidenceIDs []string) (*WriteResult, error) {
	id, err := a.eng.Decide(ctx, vault, decision, rationale, alternatives, evidenceIDs)
	if err != nil {
		return nil, err
	}
	return &WriteResult{ID: id.String()}, nil
}

func (a *mcpEngineAdapter) Restore(ctx context.Context, vault, id string) (*RestoreResult, error) {
	eng, err := a.eng.Restore(ctx, vault, id)
	if err != nil {
		return nil, err
	}
	return &RestoreResult{
		ID:      eng.ID.String(),
		Concept: eng.Concept,
		State:   "active",
	}, nil
}

func (a *mcpEngineAdapter) Traverse(ctx context.Context, vault string, req *TraverseRequest) (*TraverseResult, error) {
	maxHops := req.MaxHops
	if maxHops <= 0 {
		maxHops = 3
	}
	maxNodes := req.MaxNodes
	if maxNodes <= 0 {
		maxNodes = 50
	}
	nodes, edges, err := a.eng.Traverse(ctx, vault, req.StartID, maxHops, maxNodes)
	if err != nil {
		return nil, err
	}
	result := &TraverseResult{
		TotalReachable: len(nodes),
	}
	for _, n := range nodes {
		result.Nodes = append(result.Nodes, TraversalNode{
			ID:      n.ID.String(),
			Concept: n.Concept,
			HopDist: n.HopDist,
			Summary: n.Summary,
		})
	}
	for _, e := range edges {
		result.Edges = append(result.Edges, TraversalEdge{
			FromID: e.From.String(),
			ToID:   e.To.String(),
			Weight: e.Weight,
		})
	}
	return result, nil
}

func (a *mcpEngineAdapter) Explain(ctx context.Context, vault string, req *ExplainRequest) (*ExplainResult, error) {
	data, err := a.eng.Explain(ctx, vault, req.EngramID, req.Query)
	if err != nil {
		return nil, err
	}
	return &ExplainResult{
		EngramID:    data.EngramID,
		WouldReturn: data.WouldReturn,
		Threshold:   data.Threshold,
		FinalScore:  data.FinalScore,
		Components: ExplainComponents{
			FullTextRelevance:  float64(data.Components.FullTextRelevance),
			SemanticSimilarity: float64(data.Components.SemanticSimilarity),
			DecayFactor:        float64(data.Components.DecayFactor),
			HebbianBoost:       float64(data.Components.HebbianBoost),
			AccessFrequency:    float64(data.Components.AccessFrequency),
		},
	}, nil
}

func (a *mcpEngineAdapter) UpdateState(ctx context.Context, vault, id, state, reason string) error {
	return a.eng.UpdateLifecycleState(ctx, vault, id, state)
}

func (a *mcpEngineAdapter) ListDeleted(ctx context.Context, vault string, limit int) ([]DeletedEngram, error) {
	engrams, err := a.eng.ListDeleted(ctx, vault, limit)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	result := make([]DeletedEngram, 0, len(engrams))
	for _, eng := range engrams {
		if eng == nil {
			continue
		}
		result = append(result, DeletedEngram{
			ID:               eng.ID.String(),
			Concept:          eng.Concept,
			DeletedAt:        eng.UpdatedAt,
			RecoverableUntil: now.Add(7 * 24 * time.Hour),
			Tags:             eng.Tags,
		})
	}
	return result, nil
}

func (a *mcpEngineAdapter) RetryEnrich(ctx context.Context, vault, id string) (*RetryEnrichResult, error) {
	if a.enricher == nil {
		return nil, errors.New("no enrich plugin configured")
	}

	// Parse the engram ID
	ulid, err := storage.ParseULID(id)
	if err != nil {
		return nil, fmt.Errorf("retry enrich: parse id: %w", err)
	}

	// Get the vault prefix and fetch the engram
	store := a.eng.Store()
	wsPrefix := store.ResolveVaultPrefix(vault)
	eng, err := store.GetEngram(ctx, wsPrefix, ulid)
	if err != nil {
		return nil, fmt.Errorf("retry enrich: get engram: %w", err)
	}

	// Run enrichment
	result, err := a.enricher.Enrich(ctx, eng)
	if err != nil {
		return nil, fmt.Errorf("retry enrich: enrich failed: %w", err)
	}

	// Persist enrichment results back to the engram.
	// Summary and KeyPoints map directly. MemoryType is a uint8 enum with no clean
	// string mapping, and Classification in Engram is a uint16 cluster ID — both are
	// left unchanged since the string values from EnrichmentResult cannot be losslessly
	// converted without a lookup table.
	eng.Summary = result.Summary
	eng.KeyPoints = result.KeyPoints
	if _, err := store.WriteEngram(ctx, wsPrefix, eng); err != nil {
		return nil, fmt.Errorf("retry enrich: persist results: %w", err)
	}

	return &RetryEnrichResult{
		EngramID:      id,
		PluginsQueued: []string{a.enricher.Name()},
		Note:          "enrichment applied and persisted",
	}, nil
}

func (a *mcpEngineAdapter) GetVaultPlasticity(_ context.Context, vault string) (*auth.ResolvedPlasticity, error) {
	r := a.eng.ResolveVaultPlasticity(vault)
	return &r, nil
}
