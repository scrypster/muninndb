package rest

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/scrypster/muninndb/internal/cognitive"
	"github.com/scrypster/muninndb/internal/engine"
	"github.com/scrypster/muninndb/internal/engine/trigger"
	"github.com/scrypster/muninndb/internal/engine/vaultjob"
	hnswpkg "github.com/scrypster/muninndb/internal/index/hnsw"
	"github.com/scrypster/muninndb/internal/plugin"
	"github.com/scrypster/muninndb/internal/storage"
	mbp "github.com/scrypster/muninndb/internal/transport/mbp"
)

// RESTEngineWrapper wraps the Engine to adapt it for the REST interface.
// All methods accept a context and pass it through to the engine.
type RESTEngineWrapper struct {
	engine   *engine.Engine
	hnswReg  *hnswpkg.Registry
	enricher plugin.EnrichPlugin
}

// NewEngineWrapper returns an EngineAPI backed by eng with optional HNSW stat injection.
func NewEngineWrapper(eng *engine.Engine, hnswReg *hnswpkg.Registry) EngineAPI {
	return &RESTEngineWrapper{engine: eng, hnswReg: hnswReg}
}

// SetEnricher configures the enrichment plugin used by RetryEnrich.
func (w *RESTEngineWrapper) SetEnricher(ep plugin.EnrichPlugin) {
	w.enricher = ep
}

func (w *RESTEngineWrapper) Hello(ctx context.Context, req *HelloRequest) (*HelloResponse, error) {
	return w.engine.Hello(ctx, req)
}

func (w *RESTEngineWrapper) Write(ctx context.Context, req *WriteRequest) (*WriteResponse, error) {
	return w.engine.Write(ctx, req)
}

func (w *RESTEngineWrapper) WriteBatch(ctx context.Context, reqs []*WriteRequest) ([]*WriteResponse, []error) {
	return w.engine.WriteBatch(ctx, reqs)
}

func (w *RESTEngineWrapper) Read(ctx context.Context, req *ReadRequest) (*ReadResponse, error) {
	return w.engine.Read(ctx, req)
}

// coerceFilterValues returns a new slice of filters where string values for
// temporal fields ("created_after", "created_before") are parsed into time.Time.
// Values that are already time.Time are left unchanged. If parsing fails the
// value is left as-is so the engine can handle or ignore it gracefully.
// The original slice is never mutated.
func coerceFilterValues(filters []mbp.Filter) []mbp.Filter {
	out := make([]mbp.Filter, len(filters))
	copy(out, filters)
	for i, f := range out {
		if f.Field != "created_after" && f.Field != "created_before" {
			continue
		}
		if _, ok := f.Value.(time.Time); ok {
			continue
		}
		s, ok := f.Value.(string)
		if !ok {
			continue
		}
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			out[i].Value = t
			continue
		}
		if t, err := time.Parse("2006-01-02", s); err == nil {
			out[i].Value = t
		}
	}
	return out
}

func (w *RESTEngineWrapper) Activate(ctx context.Context, req *ActivateRequest) (*ActivateResponse, error) {
	if len(req.Filters) > 0 {
		// Make a shallow copy to avoid mutating the caller's request.
		reqCopy := *req
		reqCopy.Filters = coerceFilterValues(req.Filters)
		req = &reqCopy
	}
	return w.engine.Activate(ctx, req)
}

func (w *RESTEngineWrapper) Link(ctx context.Context, req *mbp.LinkRequest) (*LinkResponse, error) {
	return w.engine.Link(ctx, req)
}

func (w *RESTEngineWrapper) Forget(ctx context.Context, req *ForgetRequest) (*ForgetResponse, error) {
	return w.engine.Forget(ctx, req)
}

func (w *RESTEngineWrapper) Stat(ctx context.Context, req *StatRequest) (*StatResponse, error) {
	resp, err := w.engine.Stat(ctx, req)
	if err != nil {
		return nil, err
	}
	if w.hnswReg != nil {
		if req.Vault != "" {
			ws := w.engine.Store().ResolveVaultPrefix(req.Vault)
			resp.IndexSize = w.hnswReg.VaultVectorBytes(ws)
		} else {
			resp.IndexSize = w.hnswReg.TotalVectorBytes()
		}
	}
	return resp, nil
}

func (w *RESTEngineWrapper) ListEngrams(ctx context.Context, req *ListEngramsRequest) (*ListEngramsResponse, error) {
	params := engine.ListEngramsParams{
		Vault:   req.Vault,
		Limit:   req.Limit,
		Offset:  req.Offset,
		Sort:    req.Sort,
		Tags:    req.Tags,
		State:   req.State,
		MinConf: req.MinConf,
		MaxConf: req.MaxConf,
	}

	if req.Since != "" {
		if t, err := time.Parse(time.RFC3339, req.Since); err == nil {
			params.Since = t
		}
	}
	if req.Before != "" {
		if t, err := time.Parse(time.RFC3339, req.Before); err == nil {
			params.Before = t
		}
	}

	result, err := w.engine.ListEngrams(ctx, params)
	if err != nil {
		return nil, err
	}

	engrams := make([]EngramItem, len(result.Engrams))
	for i, eng := range result.Engrams {
		engrams[i] = EngramItem{
			ID:         eng.ID.String(),
			Concept:    eng.Concept,
			Content:    eng.Content,
			Confidence: eng.Confidence,
			Tags:       eng.Tags,
			Vault:      req.Vault,
			CreatedAt:  eng.CreatedAt.Unix(),
			EmbedDim:   uint8(eng.EmbedDim),
		}
	}
	return &ListEngramsResponse{
		Engrams: engrams,
		Total:   result.Total,
		Limit:   req.Limit,
		Offset:  req.Offset,
	}, nil
}

func (w *RESTEngineWrapper) GetEngramLinks(ctx context.Context, req *GetEngramLinksRequest) (*GetEngramLinksResponse, error) {
	vault := req.Vault
	if vault == "" {
		vault = "default"
	}
	assocs, err := w.engine.GetAssociations(ctx, vault, req.ID, 50)
	if err != nil {
		return nil, err
	}
	links := make([]AssociationItem, len(assocs))
	for i, a := range assocs {
		links[i] = AssociationItem{
			TargetID:          a.TargetID.String(),
			RelType:           uint16(a.RelType),
			Weight:            a.Weight,
			CoActivationCount: a.CoActivationCount,
			RestoredAt:        int64(a.RestoredAt),
		}
	}
	return &GetEngramLinksResponse{Links: links}, nil
}

func (w *RESTEngineWrapper) ListVaults(ctx context.Context) ([]string, error) {
	names, err := w.engine.ListVaults(ctx)
	if err != nil || len(names) == 0 {
		return []string{"default"}, nil
	}
	return names, nil
}

func (w *RESTEngineWrapper) GetSession(ctx context.Context, req *GetSessionRequest) (*GetSessionResponse, error) {
	limit := req.Limit
	if limit <= 0 {
		limit = 50
	}
	offset := req.Offset
	if offset < 0 {
		offset = 0
	}
	res, err := w.engine.SessionPaged(ctx, req.Vault, req.Since, offset, limit)
	if err != nil {
		return nil, err
	}
	entries := make([]SessionItem, 0, len(res.Writes))
	for _, wr := range res.Writes {
		entries = append(entries, SessionItem{
			ID:        wr.ID,
			Concept:   wr.Concept,
			CreatedAt: wr.At.Unix(),
		})
	}
	return &GetSessionResponse{
		Entries: entries,
		Total:   res.Total,
		Offset:  offset,
		Limit:   limit,
	}, nil
}

func (w *RESTEngineWrapper) WorkerStats() cognitive.EngineWorkerStats {
	return w.engine.WorkerStats()
}

func (w *RESTEngineWrapper) SubscribeWithDeliver(ctx context.Context, req *mbp.SubscribeRequest, deliver trigger.DeliverFunc) (string, error) {
	return w.engine.SubscribeWithDeliver(ctx, req, deliver)
}

func (w *RESTEngineWrapper) Unsubscribe(ctx context.Context, subID string) error {
	return w.engine.Unsubscribe(ctx, subID)
}

func (w *RESTEngineWrapper) CountEmbedded(ctx context.Context) int64 {
	return w.engine.CountEmbedded(ctx)
}

func (w *RESTEngineWrapper) RecordAccess(ctx context.Context, vault, id string) error {
	return w.engine.RecordAccess(ctx, vault, id)
}

func (w *RESTEngineWrapper) ClearVault(ctx context.Context, vaultName string) error {
	return w.engine.ClearVault(ctx, vaultName)
}

func (w *RESTEngineWrapper) DeleteVault(ctx context.Context, vaultName string) error {
	return w.engine.DeleteVault(ctx, vaultName)
}

func (w *RESTEngineWrapper) StartClone(ctx context.Context, sourceVault, newName string) (*vaultjob.Job, error) {
	return w.engine.StartClone(ctx, sourceVault, newName)
}

func (w *RESTEngineWrapper) StartMerge(ctx context.Context, sourceVault, targetVault string, deleteSource bool) (*vaultjob.Job, error) {
	return w.engine.StartMerge(ctx, sourceVault, targetVault, deleteSource)
}

func (w *RESTEngineWrapper) GetVaultJob(jobID string) (*vaultjob.Job, bool) {
	return w.engine.GetVaultJob(jobID)
}

func (w *RESTEngineWrapper) ExportVault(ctx context.Context, vaultName, embedderModel string, dimension int, resetMeta bool, wr io.Writer) (*storage.ExportResult, error) {
	return w.engine.ExportVault(ctx, vaultName, embedderModel, dimension, resetMeta, wr)
}

func (w *RESTEngineWrapper) StartImport(ctx context.Context, vaultName, embedderModel string, dimension int, resetMeta bool, r io.Reader) (*vaultjob.Job, error) {
	return w.engine.StartImport(ctx, vaultName, embedderModel, dimension, resetMeta, r)
}

func (w *RESTEngineWrapper) ReindexFTSVault(ctx context.Context, vaultName string) (int64, error) {
	return w.engine.ReindexFTSVault(ctx, vaultName)
}

func (w *RESTEngineWrapper) StartReembedVault(ctx context.Context, vaultName, modelName string) (*vaultjob.Job, error) {
	return w.engine.StartReembedVault(ctx, vaultName, modelName)
}

func (w *RESTEngineWrapper) RenameVault(ctx context.Context, oldName, newName string) error {
	return w.engine.RenameVault(ctx, oldName, newName)
}

func (w *RESTEngineWrapper) Checkpoint(destDir string) error {
	return w.engine.Checkpoint(destDir)
}

func (w *RESTEngineWrapper) Observability(ctx context.Context, version string, uptimeSeconds int64) (*engine.ObservabilitySnapshot, error) {
	return w.engine.Observability(ctx, version, uptimeSeconds)
}

func (w *RESTEngineWrapper) GetProcessorStats() []plugin.RetroactiveStats {
	return w.engine.GetProcessorStats()
}

// lifecycleStateLabel returns a human-readable label for a storage.LifecycleState.
func lifecycleStateLabel(s storage.LifecycleState) string {
	switch s {
	case storage.StatePlanning:
		return "planning"
	case storage.StateActive:
		return "active"
	case storage.StatePaused:
		return "paused"
	case storage.StateBlocked:
		return "blocked"
	case storage.StateCompleted:
		return "completed"
	case storage.StateCancelled:
		return "cancelled"
	case storage.StateArchived:
		return "archived"
	case storage.StateSoftDeleted:
		return "soft_deleted"
	default:
		return fmt.Sprintf("unknown(%d)", s)
	}
}

func (w *RESTEngineWrapper) Evolve(ctx context.Context, vault, engramID, newContent, reason string) (*EvolveResponse, error) {
	newID, err := w.engine.Evolve(ctx, vault, engramID, newContent, reason)
	if err != nil {
		return nil, err
	}
	return &EvolveResponse{ID: newID.String()}, nil
}

func (w *RESTEngineWrapper) Consolidate(ctx context.Context, vault string, ids []string, mergedContent string) (*ConsolidateResponse, error) {
	newID, archived, warnings, err := w.engine.Consolidate(ctx, vault, ids, mergedContent)
	if err != nil {
		return nil, err
	}
	return &ConsolidateResponse{
		ID:       newID.String(),
		Archived: archived,
		Warnings: warnings,
	}, nil
}

func (w *RESTEngineWrapper) Decide(ctx context.Context, vault, decision, rationale string, alternatives, evidenceIDs []string) (*DecideResponse, error) {
	newID, err := w.engine.Decide(ctx, vault, decision, rationale, alternatives, evidenceIDs)
	if err != nil {
		return nil, err
	}
	return &DecideResponse{ID: newID.String()}, nil
}

func (w *RESTEngineWrapper) Restore(ctx context.Context, vault, engramID string) (*RestoreResponse, error) {
	eng, err := w.engine.Restore(ctx, vault, engramID)
	if err != nil {
		return nil, err
	}
	return &RestoreResponse{
		ID:       eng.ID.String(),
		Concept:  eng.Concept,
		Restored: true,
		State:    lifecycleStateLabel(eng.State),
	}, nil
}

func (w *RESTEngineWrapper) Traverse(ctx context.Context, vault string, req *TraverseRequest) (*TraverseResponse, error) {
	start := time.Now()
	nodes, edges, err := w.engine.Traverse(ctx, vault, req.StartID, req.MaxHops, req.MaxNodes, req.FollowEntities)
	if err != nil {
		return nil, err
	}
	elapsed := time.Since(start)

	restNodes := make([]TraversalNode, len(nodes))
	for i, n := range nodes {
		restNodes[i] = TraversalNode{
			ID:      n.ID.String(),
			Concept: n.Concept,
			HopDist: n.HopDist,
			Summary: n.Summary,
		}
	}
	restEdges := make([]TraversalEdge, len(edges))
	for i, e := range edges {
		restEdges[i] = TraversalEdge{
			FromID: e.From.String(),
			ToID:   e.To.String(),
			Weight: e.Weight,
		}
	}
	return &TraverseResponse{
		Nodes:          restNodes,
		Edges:          restEdges,
		TotalReachable: len(nodes),
		QueryMs:        float64(elapsed.Microseconds()) / 1000.0,
	}, nil
}

func (w *RESTEngineWrapper) Explain(ctx context.Context, vault string, req *ExplainRequest) (*ExplainResponse, error) {
	data, err := w.engine.Explain(ctx, vault, req.EngramID, req.Query)
	if err != nil {
		return nil, err
	}
	return &ExplainResponse{
		EngramID:    data.EngramID,
		Concept:     data.Concept,
		FinalScore:  data.FinalScore,
		WouldReturn: data.WouldReturn,
		Threshold:   data.Threshold,
	}, nil
}

func (w *RESTEngineWrapper) UpdateState(ctx context.Context, vault, engramID, state, _ string) error {
	return w.engine.UpdateLifecycleState(ctx, vault, engramID, state)
}

func (w *RESTEngineWrapper) UpdateTags(ctx context.Context, vault, engramID string, tags []string) error {
	ulid, err := storage.ParseULID(engramID)
	if err != nil {
		return fmt.Errorf("invalid engram id: %w", err)
	}
	ws := w.engine.Store().ResolveVaultPrefix(vault)
	return w.engine.Store().UpdateTags(ctx, ws, ulid, tags)
}

func (w *RESTEngineWrapper) ListDeleted(ctx context.Context, vault string, limit int) (*ListDeletedResponse, error) {
	engrams, err := w.engine.ListDeleted(ctx, vault, limit)
	if err != nil {
		return nil, err
	}
	items := make([]DeletedEngramItem, len(engrams))
	for i, eng := range engrams {
		deletedAt := eng.UpdatedAt
		items[i] = DeletedEngramItem{
			ID:               eng.ID.String(),
			Concept:          eng.Concept,
			DeletedAt:        deletedAt.Unix(),
			RecoverableUntil: deletedAt.Add(7 * 24 * time.Hour).Unix(),
			Tags:             eng.Tags,
		}
	}
	return &ListDeletedResponse{
		Deleted: items,
		Count:   len(items),
	}, nil
}

func (w *RESTEngineWrapper) RetryEnrich(ctx context.Context, vault, engramID string) (*RetryEnrichResponse, error) {
	if w.enricher == nil {
		return nil, fmt.Errorf("no enrichment plugin configured")
	}
	ulid, err := storage.ParseULID(engramID)
	if err != nil {
		return nil, fmt.Errorf("invalid engram id: %w", err)
	}
	ws := w.engine.Store().ResolveVaultPrefix(vault)
	eng, err := w.engine.Store().GetEngram(ctx, ws, ulid)
	if err != nil {
		return nil, fmt.Errorf("get engram: %w", err)
	}
	result, enrichErr := w.enricher.Enrich(ctx, eng)
	if enrichErr != nil {
		return nil, fmt.Errorf("enrich: %w", enrichErr)
	}
	if result == nil {
		return nil, fmt.Errorf("enrich returned nil result")
	}

	pStore := plugin.NewStoreAdapter(w.engine.Store(), w.hnswReg)
	if err := plugin.PersistEnrichmentResult(ctx, pStore, plugin.ULID(ulid), result); err != nil {
		return nil, err
	}
	return &RetryEnrichResponse{
		EngramID:      engramID,
		PluginsQueued: []string{w.enricher.Name()},
	}, nil
}

func (w *RESTEngineWrapper) ResolveContradiction(ctx context.Context, vault, idA, idB string) error {
	return w.engine.ResolveContradiction(ctx, vault, idA, idB)
}

func (w *RESTEngineWrapper) GetContradictions(ctx context.Context, vault string) (*ContradictionsResponse, error) {
	pairs, err := w.engine.GetContradictions(ctx, vault)
	if err != nil {
		return nil, err
	}
	ws := w.engine.Store().ResolveVaultPrefix(vault)
	items := make([]ContradictionItem, 0, len(pairs))
	for _, pair := range pairs {
		engA, errA := w.engine.Store().GetEngram(ctx, ws, pair[0])
		engB, errB := w.engine.Store().GetEngram(ctx, ws, pair[1])
		item := ContradictionItem{
			IDa: pair[0].String(),
			IDb: pair[1].String(),
		}
		if errA == nil && engA != nil {
			item.ConceptA = engA.Concept
		}
		if errB == nil && engB != nil {
			item.ConceptB = engB.Concept
		}
		items = append(items, item)
	}
	return &ContradictionsResponse{Contradictions: items}, nil
}

func (w *RESTEngineWrapper) GetGuide(ctx context.Context, vault string) (string, error) {
	statResp, err := w.engine.Stat(ctx, &mbp.StatRequest{Vault: vault})
	if err != nil {
		return "", err
	}
	guide := fmt.Sprintf(`MuninnDB Guide for vault %q

This vault has %d memories stored.

Available operations:
- write: Store new memories (engrams) with concept and content
- activate/recall: Semantic search across stored memories
- evolve: Update an existing memory with new content
- consolidate: Merge multiple related memories into one
- decide: Record a decision with rationale and evidence
- link: Create associations between memories
- traverse: Walk the memory graph from a starting node
- explain: Get scoring details for a specific memory vs query
- forget: Soft-delete a memory (recoverable for 7 days)
- restore: Recover a soft-deleted memory
- set-state: Change lifecycle state (planning, active, paused, blocked, completed, cancelled, archived)
- list-deleted: View soft-deleted memories pending permanent removal
- contradictions: View detected contradictions between memories
`, vault, statResp.EngramCount)
	return guide, nil
}

func (w *RESTEngineWrapper) ExportGraph(ctx context.Context, vault string, includeEngrams bool) (*engine.ExportGraph, error) {
	return w.engine.ExportGraph(ctx, vault, includeEngrams)
}

func (w *RESTEngineWrapper) GetBatchEngramLinks(ctx context.Context, req *BatchGetEngramLinksRequest) (*BatchGetEngramLinksResponse, error) {
	vault := req.Vault
	if vault == "" {
		vault = "default"
	}
	maxPerNode := req.MaxPerNode
	if maxPerNode <= 0 {
		maxPerNode = 50
	}
	assocMap, err := w.engine.GetAssociationsBatch(ctx, vault, req.IDs, maxPerNode)
	if err != nil {
		return nil, err
	}
	result := make(map[string][]AssociationItem, len(assocMap))
	for srcID, assocs := range assocMap {
		items := make([]AssociationItem, len(assocs))
		for i, a := range assocs {
			items[i] = AssociationItem{
				TargetID:          a.TargetID.String(),
				RelType:           uint16(a.RelType),
				Weight:            a.Weight,
				CoActivationCount: a.CoActivationCount,
				RestoredAt:        int64(a.RestoredAt),
			}
		}
		result[srcID] = items
	}
	return &BatchGetEngramLinksResponse{Links: result}, nil
}
