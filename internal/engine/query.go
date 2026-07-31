package engine

import (
	"context"
	"errors"
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
	From    storage.ULID
	To      storage.ULID
	RelType storage.RelType // zero for synthetic entity-hop edges
	Weight  float32
}

// ExplainData is the engine-level score explanation for a specific engram + query.
//
// Found and Scored exist so a caller can never mistake "nothing computed this"
// for "this measured zero". Components is meaningful ONLY when Scored is true;
// when Scored is false every field in it is an uninitialized zero and must be
// rendered as unknown, never as a score (CLAUDE.md §2.1/§2.2 — a plausible
// wrong number is the project's worst failure class).
type ExplainData struct {
	EngramID string
	Concept  string
	// Found reports whether the engram exists in the vault at all. When false,
	// Concept/Confidence/Components are all unset and Note says why.
	Found bool
	// Scored reports whether this query's activation run produced a score for
	// this engram. False means the query never reached it — the components do
	// not exist rather than being zero.
	Scored bool
	// Confidence is the engram's STORED confidence. It does not depend on the
	// query, so it is populated whenever Found is true — including for an
	// engram this query never scored.
	Confidence  float64
	FinalScore  float64
	WouldReturn bool
	Threshold   float64
	Components  mbp.ScoreComponents
	// Note is a plain-language explanation, set whenever Found or Scored is
	// false. Empty on the fully-scored happy path.
	Note string
}

// defaultRecallThreshold is the score bar a default muninn_recall applies —
// mirrored from internal/mcp/handlers.go handleRecall so Explain's
// would_return answers the question the caller is actually asking ("would
// recall have returned this?") instead of the meaningless "was it a
// candidate at threshold 0?". Pinned by
// TestRecallThresholdFor_MirrorsRecallSurface.
const defaultRecallThreshold = 0.5

// explainMaxCandidates bounds the activation run Explain inspects. Larger than
// any real recall limit so "not scored" almost always means "the query's
// indexes never produced this engram", not "it fell off the end of a short
// result list" — and the Note says which claim is being made.
const explainMaxCandidates = 1000

// recallThresholdFor returns the threshold a default recall applies in
// vault. RRF fusion produces reciprocal-rank scores that are not calibrated to
// the 0..1 relevance scale, so that surface drops the bar to 0 — Explain has to
// report the same number or would_return lies.
func (e *Engine) recallThresholdFor(vault string) float64 {
	if e.ResolveVaultPlasticity(vault).ScoringFusion == "rrf" {
		return 0
	}
	return defaultRecallThreshold
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

// GetAssociationsBatch returns forward associations for multiple engrams.
// The storage layer already supports batching with a single Pebble iterator.
func (e *Engine) GetAssociationsBatch(ctx context.Context, vault string, engramIDs []string, maxN int) (map[string][]storage.Association, error) {
	ws := e.store.ResolveVaultPrefix(vault)
	ids := make([]storage.ULID, len(engramIDs))
	for i, s := range engramIDs {
		id, err := storage.ParseULID(s)
		if err != nil {
			return nil, fmt.Errorf("parse id at index %d: %w", i, err)
		}
		ids[i] = id
	}
	assocMap, err := e.store.GetAssociations(ctx, ws, ids, maxN)
	if err != nil {
		return nil, err
	}
	result := make(map[string][]storage.Association, len(assocMap))
	for id, assocs := range assocMap {
		result[id.String()] = assocs
	}
	// Guarantee every requested ID appears in the result map.
	for _, s := range engramIDs {
		if _, ok := result[s]; !ok {
			result[s] = nil
		}
	}
	return result, nil
}

// GetContradictions returns all contradiction pairs stored in a vault.
func (e *Engine) GetContradictions(ctx context.Context, vault string) ([][2]storage.ULID, error) {
	ws := e.store.ResolveVaultPrefix(vault)
	return e.store.GetContradictions(ctx, ws)
}

// ResolveContradiction removes the contradiction marker for the pair (idA, idB)
// and updates the vault coherence counters.
func (e *Engine) ResolveContradiction(ctx context.Context, vault, idA, idB string) error {
	if err := e.refuseAppend(ctx); err != nil {
		return err
	}
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

// entityHopWeight is the edge weight assigned to engrams reached via a shared
// entity link rather than a direct association edge. It is intentionally lower
// than a typical direct-association weight (0.3–1.0) so that entity-reached
// neighbours are surfaced but ranked below structurally adjacent memories.
const entityHopWeight = 0.1

// Traverse performs a bounded BFS from startID, following association edges.
// When followEntities is true the BFS additionally traverses through shared
// entity links: for each engram dequeued at depth d, all entities it mentions
// are looked up, and every other engram in the same vault that also mentions
// those entities is enqueued at depth d+1 (with entityHopWeight).
func (e *Engine) Traverse(ctx context.Context, vault, startID string, maxHops, maxNodes int, followEntities bool) ([]TraversalNode, []TraversalEdge, error) {
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
				if eng.State != storage.StateSoftDeleted && eng.State != storage.StateArchived {
					nodes = append(nodes, TraversalNode{
						ID:      eng.ID,
						Concept: eng.Concept,
						HopDist: hopMap[src],
						Summary: eng.Summary,
					})
				}
			}
			for _, assoc := range assocMap[src] {
				edges = append(edges, TraversalEdge{From: src, To: assoc.TargetID, RelType: assoc.RelType, Weight: assoc.Weight})
				if _, seen := visited[assoc.TargetID]; !seen {
					visited[assoc.TargetID] = struct{}{}
					hopMap[assoc.TargetID] = hop + 1
					next = append(next, assoc.TargetID)
				}
			}

			// Entity hop: find neighbours reachable via shared entity names.
			if followEntities && hop < maxHops {
				_ = e.store.ScanEngramEntities(ctx, ws, src, func(entityName string) error {
					return e.store.ScanEntityEngrams(ctx, entityName, func(entityWS [8]byte, neighborID storage.ULID) error {
						if entityWS != ws {
							return nil // cross-vault — skip
						}
						if _, seen := visited[neighborID]; seen {
							return nil
						}
						visited[neighborID] = struct{}{}
						hopMap[neighborID] = hop + 1
						next = append(next, neighborID)
						edges = append(edges, TraversalEdge{From: src, To: neighborID, Weight: entityHopWeight})
						return nil
					})
				})
			}
		}
		queue = next
	}
	return nodes, edges, nil
}

// Explain runs activation with the given query and returns score details for
// engramID.
//
// The activation run itself uses a threshold of 0 so a below-bar engram still
// gets a real score card; the threshold REPORTED (and the one would_return is
// measured against) is the vault's default recall threshold. Everything that
// does not depend on the query — existence, concept, stored confidence — is
// read straight from the engram, so a query that never reached it still gets
// those facts plus an explicit Note, instead of an all-zero card that reads
// like a genuine score of zero.
func (e *Engine) Explain(ctx context.Context, vault, engramID string, query []string, embedding []float32) (*ExplainData, error) {
	result := &ExplainData{
		EngramID:  engramID,
		Threshold: e.recallThresholdFor(vault),
	}

	id, err := storage.ParseULID(engramID)
	if err != nil {
		result.Note = fmt.Sprintf("engram_id %q is not a valid ULID, so no engram could be looked up: %v", engramID, err)
		return result, nil
	}
	eng, err := e.GetEngram(ctx, vault, id)
	switch {
	case errors.Is(err, storage.ErrNotFound):
		result.Note = fmt.Sprintf("engram %s was not found in vault %q — check the id and the vault", engramID, vault)
		return result, nil
	case err != nil:
		return nil, fmt.Errorf("explain: load engram %s: %w", engramID, err)
	}
	result.Found = true
	result.Concept = eng.Concept
	result.Confidence = float64(eng.Confidence)

	// Run activation in observe mode so we get accurate scores without
	// triggering Hebbian co-activation, activity tracking, or PAS transitions.
	// Explain is a diagnostic read — it should not mutate cognitive state.
	ctx = context.WithValue(ctx, auth.ContextMode, "observe")
	resp, err := e.Activate(ctx, &mbp.ActivateRequest{
		Vault:      vault,
		Context:    query,
		Embedding:  embedding,
		MaxResults: explainMaxCandidates,
		Threshold:  0,
	})
	if err != nil {
		return nil, fmt.Errorf("explain activation: %w", err)
	}
	for _, item := range resp.Activations {
		if item.ID == engramID {
			result.Scored = true
			result.FinalScore = float64(item.Score)
			result.Concept = item.Concept
			result.Components = item.ScoreComponents
			break
		}
	}
	if !result.Scored {
		result.Note = fmt.Sprintf("this query produced no score for engram %s: it was not among the %d candidates activation ranked, "+
			"so no per-component scores exist for it — any component value shown is 'not computed', never 'measured zero'. "+
			"Its full-text, vector and entity signals did not put it in the candidate pool for these terms.",
			engramID, explainMaxCandidates)
	}
	result.WouldReturn = result.Scored && result.FinalScore >= result.Threshold
	return result, nil
}
