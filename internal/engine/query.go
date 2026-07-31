package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

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

// defaultRecallThreshold is the score bar a default recall applies on an
// ACT-R vault. The ENGINE owns this default now (the MCP surface forwards 0;
// see the COG-6 coerce in engine.go), so Explain mirrors the engine, not a
// surface. 0.1 is the value COG-26's b=0.520 was calibrated against, on the
// absolute-score scale the ACT-R gate compares. Pinned by
// TestRecallThresholdFor_MirrorsRecallSurface.
const defaultRecallThreshold = 0.1

// weightedSumRecallThreshold mirrors the engine default for legacy
// weighted_sum vaults — the only bar ever validated against that formula.
const weightedSumRecallThreshold = 0.5

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
	switch e.ResolveVaultPlasticity(vault).ScoringFusion {
	case "rrf":
		// activation.Run's rrf default (#590).
		return 0.001
	case "weighted_sum":
		return weightedSumRecallThreshold
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

// Contradiction status values. A caller that cannot tell these apart cannot
// tell "this vault has no contradictions" from "the detector has not run yet".
const (
	// ContradictionDetected — the durable 0x0A marker exists. The confidence
	// penalty (if any) has been applied exactly once for this pair.
	ContradictionDetected = "detected"
	// ContradictionPending — an explicit "contradicts" association exists but
	// the batch detector has not flagged the pair yet. The contradiction is
	// real and durable; only its detection is outstanding.
	ContradictionPending = "pending_detection"
)

// ContradictionDetail is one contradiction pair, resolved for presentation.
//
// DetectedAt is zero when Status is ContradictionPending (nothing has detected
// it yet) and also for legacy markers written before the timestamp existed.
// Zero means UNKNOWN and must be rendered as absent, never as an instant.
type ContradictionDetail struct {
	IDa        string
	IDb        string
	ConceptA   string
	ConceptB   string
	Status     string
	DetectedAt time.Time
	DeclaredAt time.Time
}

// ContradictionReport is the full answer to "what contradicts what in this
// vault?", including how much of it is still awaiting detection.
//
// ScanComplete reports whether the search for declared-but-undetected links
// examined the whole association keyspace. When it is false, PendingCount is a
// lower bound and the caller must say so rather than implying the list is
// exhaustive.
type ContradictionReport struct {
	Pairs         []ContradictionDetail
	DetectedCount int
	PendingCount  int
	ScanComplete  bool
	Scanned       int
}

// GetContradictionReport returns every contradiction in a vault — both the
// pairs the detector has flagged and the pairs an agent has explicitly linked
// but the batch detector has not reached yet.
//
// The second half is the point. The ContradictWorker runs on a 30s batch
// interval, so for up to half a minute after muninn_link(relation="contradicts")
// the 0x0A marker does not exist. Returning only markers meant an agent that
// linked a contradiction and immediately checked its work got an empty list —
// the same answer a vault with no contradictions gives. Three independent
// evaluators concluded from that empty list that the feature was dead. An
// unknown state must never be reported as a known one (CLAUDE.md §2.1/§2.2), so
// the declared-but-unflagged pairs are reported explicitly, labelled pending.
//
// Detection is left entirely to the worker: this is a read path and it neither
// flags markers nor submits confidence updates. The contradiction confidence
// penalty must stay asynchronous and fire exactly once per pair.
func (e *Engine) GetContradictionReport(ctx context.Context, vault string) (*ContradictionReport, error) {
	ws := e.store.ResolveVaultPrefix(vault)

	detected, err := e.store.GetContradictionRecords(ctx, ws)
	if err != nil {
		return nil, err
	}
	declared, err := e.store.DeclaredContradictions(ctx, ws, 0)
	if err != nil {
		return nil, err
	}

	report := &ContradictionReport{
		ScanComplete: declared.Complete,
		Scanned:      declared.Scanned,
	}

	type pairKey [32]byte
	key := func(r storage.ContradictionRecord) pairKey {
		var k pairKey
		copy(k[:16], r.A[:])
		copy(k[16:], r.B[:])
		return k
	}

	index := make(map[pairKey]int, len(detected)+len(declared.Records))
	for _, r := range detected {
		index[key(r)] = len(report.Pairs)
		report.Pairs = append(report.Pairs, ContradictionDetail{
			IDa:        r.A.String(),
			IDb:        r.B.String(),
			Status:     ContradictionDetected,
			DetectedAt: r.DetectedAt,
		})
		report.DetectedCount++
	}
	for _, r := range declared.Records {
		if idx, ok := index[key(r)]; ok {
			// Already detected — keep one entry, but record when it was declared.
			report.Pairs[idx].DeclaredAt = r.DeclaredAt
			continue
		}
		index[key(r)] = len(report.Pairs)
		report.Pairs = append(report.Pairs, ContradictionDetail{
			IDa:        r.A.String(),
			IDb:        r.B.String(),
			Status:     ContradictionPending,
			DeclaredAt: r.DeclaredAt,
		})
		report.PendingCount++
	}

	if err := e.fillContradictionConcepts(ctx, ws, report.Pairs); err != nil {
		return nil, err
	}
	return report, nil
}

// fillContradictionConcepts resolves the concept of every engram named in the
// report. Concepts are looked up on read rather than duplicated into the 0x0A
// index: the index would then need invalidating on every rename, and a stale
// concept is exactly the kind of plausible-but-wrong value this surface exists
// to stop producing. The lookup is one batched GetEngrams over the distinct
// IDs — contradiction sets are small, and the pairs were already deduplicated.
//
// A concept that cannot be resolved (deleted or unreadable engram) is left
// empty rather than guessed at.
func (e *Engine) fillContradictionConcepts(ctx context.Context, ws [8]byte, pairs []ContradictionDetail) error {
	if len(pairs) == 0 {
		return nil
	}
	order := make([]storage.ULID, 0, len(pairs)*2)
	seen := make(map[string]bool, len(pairs)*2)
	for _, p := range pairs {
		for _, s := range [2]string{p.IDa, p.IDb} {
			if seen[s] {
				continue
			}
			seen[s] = true
			id, err := storage.ParseULID(s)
			if err != nil {
				continue
			}
			order = append(order, id)
		}
	}
	engrams, err := e.store.GetEngrams(ctx, ws, order)
	if err != nil {
		return fmt.Errorf("resolve contradiction concepts: %w", err)
	}
	concepts := make(map[string]string, len(order))
	for i, eng := range engrams {
		if eng == nil || i >= len(order) {
			continue
		}
		concepts[order[i].String()] = eng.Concept
	}
	for i := range pairs {
		pairs[i].ConceptA = concepts[pairs[i].IDa]
		pairs[i].ConceptB = concepts[pairs[i].IDb]
	}
	return nil
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
	// Threshold -1 is the explicit diagnostic bypass (activation.Run treats a
	// negative threshold as "gate nothing"): a below-bar engram must still get a
	// full score card, or Explain cannot explain the exact case it exists for —
	// "why didn't my memory come back". Passing 0 does NOT work: the COG-6
	// coerce turns it into the live default and the absolute gate then drops the
	// engram before it is ever scored.
	resp, err := e.Activate(ctx, &mbp.ActivateRequest{
		Vault:      vault,
		Context:    query,
		Embedding:  embedding,
		MaxResults: explainMaxCandidates,
		Threshold:  -1,
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
	// would_return compares the SAME quantity recall gates. On ACT-R vaults
	// that is AbsoluteScore — Final is max-normalized per query, so comparing
	// it against the bar reproduced the argmax-exemption lie explain existed to
	// debug. rrf and weighted_sum still gate on Final.
	gated := result.FinalScore
	if fusion := e.ResolveVaultPlasticity(vault).ScoringFusion; fusion != "rrf" && fusion != "weighted_sum" {
		gated = float64(result.Components.AbsoluteScore)
	}
	result.WouldReturn = result.Scored && gated >= result.Threshold
	return result, nil
}
