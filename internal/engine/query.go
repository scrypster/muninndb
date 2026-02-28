package engine

import (
	"context"
	"fmt"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// TraversalNode is a single engram returned during graph traversal.
type TraversalNode struct {
	ID      storage.ULID
	Concept string
	HopDist int
	Summary string
}

// TraversalEdge is an association edge returned during graph traversal.
type TraversalEdge struct {
	From   storage.ULID
	To     storage.ULID
	Weight float32
}

// ExplainData is the engine-level score explanation for a specific engram + query.
type ExplainData struct {
	EngramID    string
	Concept     string
	FinalScore  float64
	WouldReturn bool
	Threshold   float64
	Components  mbp.ScoreComponents
}

// GetAssociations returns the forward associations for a single engram by string ID.
func (e *Engine) GetAssociations(ctx context.Context, vault, engramID string, maxN int) ([]storage.Association, error) {
	ws := e.store.ResolveVaultPrefix(vault)
	id, err := storage.ParseULID(engramID)
	if err != nil {
		return nil, fmt.Errorf("parse id: %w", err)
	}
	assocMap, err := e.store.GetAssociations(ctx, ws, []storage.ULID{id}, maxN)
	if err != nil {
		return nil, err
	}
	return assocMap[id], nil
}

// GetContradictions returns all contradiction pairs stored in a vault.
func (e *Engine) GetContradictions(ctx context.Context, vault string) ([][2]storage.ULID, error) {
	ws := e.store.ResolveVaultPrefix(vault)
	return e.store.GetContradictions(ctx, ws)
}

// ResolveContradiction removes the contradiction marker for the pair (idA, idB)
// and updates the vault coherence counters.
func (e *Engine) ResolveContradiction(ctx context.Context, vault, idA, idB string) error {
	a, err := storage.ParseULID(idA)
	if err != nil {
		return fmt.Errorf("parse id_a: %w", err)
	}
	b, err := storage.ParseULID(idB)
	if err != nil {
		return fmt.Errorf("parse id_b: %w", err)
	}
	ws := e.store.ResolveVaultPrefix(vault)
	if err := e.store.ResolveContradiction(ctx, ws, a, b); err != nil {
		return err
	}
	if e.coherence != nil {
		e.coherence.GetOrCreate(vault).RecordContradictionResolved()
	}
	return nil
}

// Traverse performs a bounded BFS from startID, following association edges.
func (e *Engine) Traverse(ctx context.Context, vault, startID string, maxHops, maxNodes int) ([]TraversalNode, []TraversalEdge, error) {
	ws := e.store.ResolveVaultPrefix(vault)
	start, err := storage.ParseULID(startID)
	if err != nil {
		return nil, nil, fmt.Errorf("parse start id: %w", err)
	}

	visited := map[storage.ULID]struct{}{start: {}}
	queue := []storage.ULID{start}
	hopMap := map[storage.ULID]int{start: 0}

	var nodes []TraversalNode
	var edges []TraversalEdge

	for hop := 0; hop <= maxHops && len(queue) > 0 && len(nodes) < maxNodes; hop++ {
		assocMap, err := e.store.GetAssociations(ctx, ws, queue, 20)
		if err != nil {
			return nil, nil, err
		}
		engrams, err := e.store.GetEngrams(ctx, ws, queue)
		if err != nil {
			return nil, nil, err
		}
		var next []storage.ULID
		for i, src := range queue {
			if len(nodes) >= maxNodes {
				break
			}
			eng := engrams[i]
			if eng != nil {
				nodes = append(nodes, TraversalNode{
					ID:      eng.ID,
					Concept: eng.Concept,
					HopDist: hopMap[src],
					Summary: eng.Summary,
				})
			}
			for _, assoc := range assocMap[src] {
				edges = append(edges, TraversalEdge{From: src, To: assoc.TargetID, Weight: assoc.Weight})
				if _, seen := visited[assoc.TargetID]; !seen {
					visited[assoc.TargetID] = struct{}{}
					hopMap[assoc.TargetID] = hop + 1
					next = append(next, assoc.TargetID)
				}
			}
		}
		queue = next
	}
	return nodes, edges, nil
}

// Explain runs activation with the given query and returns score details for engramID.
func (e *Engine) Explain(ctx context.Context, vault, engramID string, query []string) (*ExplainData, error) {
	const threshold = 0.0
	// Run activation in observe mode so we get accurate scores without
	// triggering Hebbian co-activation, activity tracking, or PAS transitions.
	// Explain is a diagnostic read — it should not mutate cognitive state.
	ctx = context.WithValue(ctx, auth.ContextMode, "observe")
	resp, err := e.Activate(ctx, &mbp.ActivateRequest{
		Vault:      vault,
		Context:    query,
		MaxResults: 100,
		Threshold:  threshold,
	})
	if err != nil {
		return nil, fmt.Errorf("explain activation: %w", err)
	}
	result := &ExplainData{
		EngramID:    engramID,
		WouldReturn: false,
		Threshold:   threshold,
	}
	for _, item := range resp.Activations {
		if item.ID == engramID {
			result.WouldReturn = true
			result.FinalScore = float64(item.Score)
			result.Concept = item.Concept
			result.Components = item.ScoreComponents
			break
		}
	}
	return result, nil
}
