package mcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/engine"
	"github.com/scrypster/muninndb/internal/plugin"
	"github.com/scrypster/muninndb/internal/provenance"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// mcpEngineAdapter adapts *engine.Engine to mcp.EngineInterface.
// Implemented here in internal/mcp/engine_adapter.go.
type mcpEngineAdapter struct {
	eng      *engine.Engine
	enricher plugin.EnrichPlugin
	pStore   plugin.PluginStore // needed by RetryEnrich to persist entities/relationships
}

// NewEngineAdapter returns an EngineInterface backed by eng with optional enricher.
// pStore is used by RetryEnrich to persist entity and relationship data; pass nil when
// no enrichment plugin is configured (RetryEnrich will error before using pStore).
func NewEngineAdapter(eng *engine.Engine, enricher plugin.EnrichPlugin, pStore plugin.PluginStore) EngineInterface {
	return &mcpEngineAdapter{eng: eng, enricher: enricher, pStore: pStore}
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
	res, err := a.eng.Consolidate(ctx, vault, ids, merged)
	if err != nil {
		return nil, err
	}
	return &ConsolidateResult{ID: res.MergedID.String(), Archived: res.Archived, Warnings: res.Warnings}, nil
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
	res, err := a.eng.Decide(ctx, vault, decision, rationale, alternatives, evidenceIDs)
	if err != nil {
		return nil, err
	}
	return &WriteResult{ID: res.ID.String(), Warnings: res.Warnings}, nil
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
	nodes, edges, err := a.eng.Traverse(ctx, vault, req.StartID, maxHops, maxNodes, req.FollowEntities)
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
			FromID:  e.From.String(),
			ToID:    e.To.String(),
			RelType: relTypeToString(e.RelType),
			Weight:  e.Weight,
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

	// Persist the full enrichment result: summary, key points, entities, relationships.
	// Unlike the retroactive processor, RetryEnrich is a forced manual re-run: it does
	// not check existing DigestEntities/DigestRelationships flags before writing.
	// This is intentional — the caller explicitly requested re-enrichment.
	if err := a.pStore.UpdateDigest(ctx, plugin.ULID(ulid), result); err != nil {
		return nil, fmt.Errorf("retry enrich: persist digest: %w", err)
	}

	// Persist entities, links, and co-occurrence pairs.
	var linkedEntityNames []string
	for _, entity := range result.Entities {
		if err := a.pStore.UpsertEntity(ctx, entity); err != nil {
			slog.Warn("retry enrich: failed to upsert entity", "id", id, "name", entity.Name, "err", err)
			continue
		}
		if err := a.pStore.LinkEngramToEntity(ctx, plugin.ULID(ulid), entity.Name); err != nil {
			slog.Warn("retry enrich: failed to link engram to entity — entity upserted but not linked",
				"id", id, "name", entity.Name, "err", err)
			continue
		}
		linkedEntityNames = append(linkedEntityNames, entity.Name)
	}
	for i := 0; i < len(linkedEntityNames); i++ {
		for j := i + 1; j < len(linkedEntityNames); j++ {
			_ = a.pStore.IncrementEntityCoOccurrence(ctx, plugin.ULID(ulid), linkedEntityNames[i], linkedEntityNames[j])
		}
	}
	if len(result.Entities) > 0 {
		if err := a.pStore.SetDigestFlag(ctx, plugin.ULID(ulid), plugin.DigestEntities); err != nil {
			slog.Warn("retry enrich: failed to set DigestEntities flag", "id", id, "err", err)
		}
	}

	// Persist relationships.
	for _, rel := range result.Relationships {
		if err := a.pStore.UpsertRelationship(ctx, plugin.ULID(ulid), rel); err != nil {
			slog.Warn("retry enrich: failed to upsert relationship", "id", id, "err", err)
		}
	}
	if len(result.Relationships) > 0 {
		if err := a.pStore.SetDigestFlag(ctx, plugin.ULID(ulid), plugin.DigestRelationships); err != nil {
			slog.Warn("retry enrich: failed to set DigestRelationships flag", "id", id, "err", err)
		}
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

func (a *mcpEngineAdapter) RememberTree(ctx context.Context, req *RememberTreeRequest) (*RememberTreeResult, error) {
	engineReq := &engine.RememberTreeRequest{
		Vault: req.Vault,
		Root:  convertTreeNodeInput(req.Root),
	}
	r, err := a.eng.RememberTree(ctx, engineReq)
	if err != nil {
		return nil, err
	}
	return &RememberTreeResult{RootID: r.RootID, NodeMap: r.NodeMap}, nil
}

func (a *mcpEngineAdapter) RecallTree(ctx context.Context, vault, rootID string, maxDepth, limit int, includeCompleted bool) (*RecallTreeResult, error) {
	node, err := a.eng.RecallTree(ctx, vault, rootID, maxDepth, limit, includeCompleted)
	if err != nil {
		return nil, err
	}
	return &RecallTreeResult{Root: convertTreeNode(node)}, nil
}

func (a *mcpEngineAdapter) CountChildren(ctx context.Context, vault, engramID string) (int, error) {
	return a.eng.CountChildren(ctx, vault, engramID)
}

func (a *mcpEngineAdapter) GetEnrichmentMode(ctx context.Context) string {
	return a.eng.GetEnrichmentMode()
}

func (a *mcpEngineAdapter) AddChild(ctx context.Context, vault, parentID string, child *AddChildRequest) (*AddChildResult, error) {
	input := &engine.AddChildInput{
		Concept: child.Concept,
		Content: child.Content,
		Type:    child.Type,
		Tags:    child.Tags,
		Ordinal: child.Ordinal,
	}
	r, err := a.eng.AddChild(ctx, vault, parentID, input)
	if err != nil {
		return nil, err
	}
	return &AddChildResult{ChildID: r.ChildID, Ordinal: r.Ordinal}, nil
}

func (a *mcpEngineAdapter) FindByEntity(ctx context.Context, vault, entityName string, limit int) ([]*storage.Engram, error) {
	return a.eng.FindByEntity(ctx, vault, entityName, limit)
}

func (a *mcpEngineAdapter) CheckIdempotency(ctx context.Context, opID string) (*storage.IdempotencyReceipt, error) {
	return a.eng.Store().CheckIdempotency(ctx, opID)
}

func (a *mcpEngineAdapter) WriteIdempotency(ctx context.Context, opID, engramID string) error {
	return a.eng.Store().WriteIdempotency(ctx, opID, engramID)
}

func (a *mcpEngineAdapter) SetEntityState(ctx context.Context, entityName, state, mergedInto, entityType string) error {
	return a.eng.SetEntityState(ctx, entityName, state, mergedInto, entityType)
}

func (a *mcpEngineAdapter) SetEntityStateBatch(ctx context.Context, ops []engine.EntityStateOp) []error {
	return a.eng.SetEntityStateBatch(ctx, ops)
}

func (a *mcpEngineAdapter) ExportGraph(ctx context.Context, vault string, includeEngrams bool) (*engine.ExportGraph, error) {
	return a.eng.ExportGraph(ctx, vault, includeEngrams)
}

func (a *mcpEngineAdapter) GetEntityTimeline(ctx context.Context, vault, entityName string, limit int) (*engine.EntityTimeline, error) {
	return a.eng.GetEntityTimeline(ctx, vault, entityName, limit)
}

func (a *mcpEngineAdapter) GetEntityClusters(ctx context.Context, vault string, minCount, topN int) ([]EntityClusterResult, error) {
	clusters, err := a.eng.GetEntityClusters(ctx, vault, minCount, topN)
	if err != nil {
		return nil, err
	}
	result := make([]EntityClusterResult, len(clusters))
	for i, c := range clusters {
		result[i] = EntityClusterResult{
			EntityA: c.EntityA,
			EntityB: c.EntityB,
			Count:   c.Count,
		}
	}
	return result, nil
}

func (a *mcpEngineAdapter) WhereLeftOff(ctx context.Context, vault string, limit int) ([]WhereLeftOffEntry, error) {
	engrams, err := a.eng.WhereLeftOff(ctx, vault, limit)
	if err != nil {
		return nil, err
	}
	entries := make([]WhereLeftOffEntry, 0, len(engrams))
	for _, eng := range engrams {
		if eng == nil {
			continue
		}
		entries = append(entries, WhereLeftOffEntry{
			ID:         eng.ID.String(),
			Concept:    eng.Concept,
			Summary:    eng.Summary,
			LastAccess: eng.LastAccess,
			State:      lifecycleStateLabel(eng.State),
		})
	}
	return entries, nil
}

// lifecycleStateLabel converts a storage.LifecycleState to a display string.
func lifecycleStateLabel(s storage.LifecycleState) string {
	switch s {
	case storage.StateActive:
		return "active"
	case storage.StatePaused:
		return "paused"
	case storage.StateArchived:
		return "archived"
	case storage.StateBlocked:
		return "blocked"
	case storage.StatePlanning:
		return "planning"
	case storage.StateCompleted:
		return "completed"
	case storage.StateCancelled:
		return "cancelled"
	default:
		return "unknown"
	}
}

func (a *mcpEngineAdapter) FindSimilarEntities(ctx context.Context, vault string, threshold float64, topN int) ([]engine.SimilarEntityPair, error) {
	return a.eng.FindSimilarEntities(ctx, vault, threshold, topN)
}

func (a *mcpEngineAdapter) MergeEntity(ctx context.Context, vault, entityA, entityB string, dryRun bool) (*engine.MergeEntityResult, error) {
	return a.eng.MergeEntity(ctx, vault, entityA, entityB, dryRun)
}

func (a *mcpEngineAdapter) ReplayEnrichment(ctx context.Context, vault string, stages []string, limit int, dryRun bool) (*engine.ReplayEnrichmentResult, error) {
	return a.eng.ReplayEnrichment(ctx, vault, stages, limit, dryRun)
}

func (a *mcpEngineAdapter) RecordFeedback(ctx context.Context, vault, engramID string, useful bool) error {
	return a.eng.RecordFeedback(ctx, vault, engramID, useful)
}

func (a *mcpEngineAdapter) GetProvenance(ctx context.Context, vault, id string) ([]ProvenanceEntry, error) {
	entries, err := a.eng.GetProvenance(ctx, vault, id)
	if err != nil {
		return nil, err
	}
	result := make([]ProvenanceEntry, len(entries))
	for i, e := range entries {
		result[i] = ProvenanceEntry{
			Timestamp: e.Timestamp.UTC().Format(time.RFC3339),
			Source:    provenanceSourceString(e.Source),
			AgentID:   e.AgentID,
			Operation: e.Operation,
			Note:      e.Note,
		}
	}
	return result, nil
}

func (a *mcpEngineAdapter) GetEntityAggregate(ctx context.Context, vault, entityName string, limit int) (*EntityAggregate, error) {
	agg, err := a.eng.GetEntityAggregate(ctx, vault, entityName, limit)
	if err != nil {
		return nil, err
	}
	if agg == nil {
		return nil, nil
	}
	rec := agg.Record

	engSummaries := make([]EntityEngramSummary, len(agg.Engrams))
	for i, e := range agg.Engrams {
		engSummaries[i] = EntityEngramSummary{
			ID:        e.ID.String(),
			Concept:   e.Concept,
			CreatedAt: e.CreatedAt.UTC().Format(time.RFC3339),
		}
	}
	relSummaries := make([]EntityRelSummary, len(agg.Relations))
	for i, r := range agg.Relations {
		relSummaries[i] = EntityRelSummary{
			FromEntity: r.FromEntity,
			ToEntity:   r.ToEntity,
			RelType:    r.RelType,
			Weight:     r.Weight,
		}
	}
	coOcc := make([]EntityCoOccurrence, len(agg.CoOccurring))
	for i, c := range agg.CoOccurring {
		coOcc[i] = EntityCoOccurrence{EntityName: c.Name, Count: c.Count}
	}

	result := &EntityAggregate{
		Name:          rec.Name,
		Type:          rec.Type,
		Confidence:    rec.Confidence,
		State:         rec.State,
		MentionCount:  rec.MentionCount,
		MergedInto:    rec.MergedInto,
		Engrams:       engSummaries,
		Relationships: relSummaries,
		CoOccurring:   coOcc,
	}
	if rec.FirstSeen > 0 {
		result.FirstSeen = time.Unix(0, rec.FirstSeen).UTC().Format(time.RFC3339)
	}
	if rec.UpdatedAt > 0 {
		result.UpdatedAt = time.Unix(0, rec.UpdatedAt).UTC().Format(time.RFC3339)
	}
	return result, nil
}

func (a *mcpEngineAdapter) ListEntities(ctx context.Context, vault string, limit int, state string) ([]EntitySummary, error) {
	records, err := a.eng.ListEntities(ctx, vault, limit, state)
	if err != nil {
		return nil, err
	}
	summaries := make([]EntitySummary, len(records))
	for i, r := range records {
		s := EntitySummary{
			Name:         r.Name,
			Type:         r.Type,
			Confidence:   r.Confidence,
			State:        r.State,
			MentionCount: r.MentionCount,
		}
		if r.FirstSeen > 0 {
			s.FirstSeen = time.Unix(0, r.FirstSeen).UTC().Format(time.RFC3339)
		}
		summaries[i] = s
	}
	return summaries, nil
}

// provenanceSourceString converts a provenance.SourceType to its string label.
func provenanceSourceString(s provenance.SourceType) string {
	switch s {
	case provenance.SourceHuman:
		return "human"
	case provenance.SourceLLM:
		return "llm"
	case provenance.SourceDocument:
		return "document"
	case provenance.SourceInferred:
		return "inferred"
	case provenance.SourceExternal:
		return "external"
	case provenance.SourceWorkingMem:
		return "working_memory"
	case provenance.SourceSynthetic:
		return "synthetic"
	default:
		return fmt.Sprintf("unknown(%d)", int(s))
	}
}

// convertTreeNodeInput converts MCP → engine input types.
func convertTreeNodeInput(n TreeNodeInput) engine.TreeNodeInput {
	out := engine.TreeNodeInput{
		Concept: n.Concept,
		Content: n.Content,
		Type:    n.Type,
		Tags:    n.Tags,
	}
	for _, c := range n.Children {
		out.Children = append(out.Children, convertTreeNodeInput(c))
	}
	return out
}

// convertTreeNode converts engine.TreeNode → mcp.TreeNode recursively.
func convertTreeNode(n *engine.TreeNode) *TreeNode {
	if n == nil {
		return nil
	}
	out := &TreeNode{
		ID:           n.ID,
		Concept:      n.Concept,
		State:        n.State,
		Ordinal:      n.Ordinal,
		LastAccessed: n.LastAccessed,
		Children:     make([]TreeNode, 0, len(n.Children)),
	}
	for _, c := range n.Children {
		child := convertTreeNode(&c)
		if child != nil {
			out.Children = append(out.Children, *child)
		}
	}
	return out
}
