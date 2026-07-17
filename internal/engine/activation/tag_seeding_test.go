package activation_test

import (
	"context"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/storage"
)

// rrfTagWeights returns Weights that select the rank-based RRF scoring path.
// Tag-seeded candidates surface via their RRF tag-pool rank regardless of
// vector/FTS score, isolating "does seeding work" from the non-RRF scoring
// concern exercised by TestTagSeeding_NonRRFCosine.
func rrfTagWeights() *activation.Weights {
	return &activation.Weights{UseRRFFusion: true, DisableACTR: true}
}

// TestTagSeeding_TagsAll_SurfacesEngramOutsideOtherPools is the bug repro for
// scrypster/muninndb#607: a tags_all recall must surface an engram that carries
// the tag even when it appears in none of the FTS/HNSW/decay candidate pools.
func TestTagSeeding_TagsAll_SurfacesEngramOutsideOtherPools(t *testing.T) {
	store := newStubStore()

	target := &storage.Engram{Concept: "target", Content: "carries the tag", Confidence: 1.0, Stability: 30.0, Relevance: 0.5, Tags: []string{"special"}}
	decoy := &storage.Engram{Concept: "decoy", Content: "in the other pools", Confidence: 1.0, Stability: 30.0, Relevance: 0.5}
	store.writeEngram(target)
	store.writeEngram(decoy)

	// Only decoy surfaces via FTS/HNSW/decay; target reaches the pipeline
	// exclusively through the tag index.
	fts := &stubFTS{results: []activation.ScoredID{{ID: decoy.ID, Score: 0.6}}}
	hnsw := &stubHNSW{results: []activation.ScoredID{{ID: decoy.ID, Score: 0.6}}}
	store.recent = []storage.ULID{decoy.ID}

	eng := newTestEngine(store, fts, hnsw)
	result, err := eng.Run(context.Background(), &activation.ActivateRequest{
		Context:    []string{"anything"},
		Threshold:  0.0,
		MaxResults: 10,
		Weights:    rrfTagWeights(),
		Filters:    []activation.Filter{{Field: "tags_all", Value: []string{"special"}}},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !containsID(result.Activations, target.ID) {
		t.Errorf("tags_all recall did not surface the tagged engram (bug #607): got %d activations", len(result.Activations))
	}
	// decoy lacks the tag, so the post-filter must exclude it.
	if containsID(result.Activations, decoy.ID) {
		t.Errorf("decoy without the tag leaked through tags_all post-filter")
	}
}

// TestTagSeeding_TagsAny_SurfacesEngramOutsideOtherPools mirrors the tags_all
// case for the OR-semantics tags_any filter.
func TestTagSeeding_TagsAny_SurfacesEngramOutsideOtherPools(t *testing.T) {
	store := newStubStore()

	target := &storage.Engram{Concept: "target", Content: "carries one of the tags", Confidence: 1.0, Stability: 30.0, Relevance: 0.5, Tags: []string{"blue"}}
	decoy := &storage.Engram{Concept: "decoy", Content: "in the other pools", Confidence: 1.0, Stability: 30.0, Relevance: 0.5}
	store.writeEngram(target)
	store.writeEngram(decoy)

	fts := &stubFTS{results: []activation.ScoredID{{ID: decoy.ID, Score: 0.6}}}
	hnsw := &stubHNSW{results: []activation.ScoredID{{ID: decoy.ID, Score: 0.6}}}
	store.recent = []storage.ULID{decoy.ID}

	eng := newTestEngine(store, fts, hnsw)
	result, err := eng.Run(context.Background(), &activation.ActivateRequest{
		Context:    []string{"anything"},
		Threshold:  0.0,
		MaxResults: 10,
		Weights:    rrfTagWeights(),
		Filters:    []activation.Filter{{Field: "tags_any", Value: []string{"blue", "green"}}},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !containsID(result.Activations, target.ID) {
		t.Errorf("tags_any recall did not surface the tagged engram (bug #607): got %d activations", len(result.Activations))
	}
	if containsID(result.Activations, decoy.ID) {
		t.Errorf("decoy without any requested tag leaked through tags_any post-filter")
	}
}

// TestTagSeeding_DuplicateTagsEquivalent pins that duplicated tag values in a
// filter yield the same recall result as the distinct form (no double-counting,
// no redundant scan-driven change). This is a resource-safety guard: duplicates
// must not amplify work or alter output.
func TestTagSeeding_DuplicateTagsEquivalent(t *testing.T) {
	store := newStubStore()

	target := &storage.Engram{Concept: "target", Content: "both tags", Confidence: 1.0, Stability: 30.0, Relevance: 0.5, Tags: []string{"blue", "red"}}
	store.writeEngram(target)
	store.recent = nil

	run := func(filters []activation.Filter) []storage.ULID {
		result, err := newTestEngine(store, &stubFTS{}, &emptyHNSW{}).Run(context.Background(), &activation.ActivateRequest{
			Context:    []string{"anything"},
			Threshold:  0.0,
			MaxResults: 10,
			Weights:    rrfTagWeights(),
			Filters:    filters,
		})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		var ids []storage.ULID
		for _, a := range result.Activations {
			ids = append(ids, a.Engram.ID)
		}
		return ids
	}

	// tags_all with a duplicated tag equals the distinct form.
	allDistinct := run([]activation.Filter{{Field: "tags_all", Value: []string{"blue", "red"}}})
	allDup := run([]activation.Filter{{Field: "tags_all", Value: []string{"blue", "blue", "red"}}})
	if len(allDistinct) != 1 || len(allDup) != len(allDistinct) || allDup[0] != allDistinct[0] {
		t.Errorf("tags_all dup = %v, want == distinct %v", allDup, allDistinct)
	}

	// tags_any with a duplicated tag equals the distinct form (surfaces target once).
	anyDistinct := run([]activation.Filter{{Field: "tags_any", Value: []string{"blue"}}})
	anyDup := run([]activation.Filter{{Field: "tags_any", Value: []string{"blue", "blue"}}})
	if len(anyDistinct) != 1 || len(anyDup) != len(anyDistinct) || anyDup[0] != anyDistinct[0] {
		t.Errorf("tags_any dup = %v, want == distinct %v", anyDup, anyDistinct)
	}
}

// TestTagSeeding_TagsAll_ANDSemanticsPreserved guards the post-filter: an engram
// carrying only one of two requested tags must NOT be returned.
func TestTagSeeding_TagsAll_ANDSemanticsPreserved(t *testing.T) {
	store := newStubStore()

	// partial carries only tagA; full carries both.
	partial := &storage.Engram{Concept: "partial", Content: "one tag only", Confidence: 1.0, Stability: 30.0, Relevance: 0.5, Tags: []string{"tagA"}}
	full := &storage.Engram{Concept: "full", Content: "both tags", Confidence: 1.0, Stability: 30.0, Relevance: 0.5, Tags: []string{"tagA", "tagB"}}
	store.writeEngram(partial)
	store.writeEngram(full)
	store.recent = nil

	eng := newTestEngine(store, &stubFTS{}, &emptyHNSW{})
	result, err := eng.Run(context.Background(), &activation.ActivateRequest{
		Context:    []string{"anything"},
		Threshold:  0.0,
		MaxResults: 10,
		Weights:    rrfTagWeights(),
		Filters:    []activation.Filter{{Field: "tags_all", Value: []string{"tagA", "tagB"}}},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !containsID(result.Activations, full.ID) {
		t.Errorf("engram carrying both tags was not returned")
	}
	if containsID(result.Activations, partial.ID) {
		t.Errorf("engram carrying only one of two tags_all tags was returned (AND semantics broken)")
	}
}

// TestTagSeeding_TagsAll_FindsTargetWhenAllTagWindowsTruncate is the caveat-2
// repro in its hardest form: with tags_all=[A,B] and a small scan limit, BOTH
// tags have more newer single-tag decoys than the limit, so EVERY per-tag
// newest-first window truncates before reaching the target. The target — the
// oldest engram, carrying both tags — sits outside all windows. A per-tag-window
// union (or intersection) loses it; only a stream intersection that bounds output
// (not input) at the limit surfaces it end-to-end.
func TestTagSeeding_TagsAll_FindsTargetWhenAllTagWindowsTruncate(t *testing.T) {
	store := newStubStore()

	base := time.Now().Add(-24 * time.Hour)
	// Target: oldest engram, carries both tags.
	target := &storage.Engram{
		Concept: "target", Content: "old, both tags", Confidence: 1.0, Stability: 30.0, Relevance: 0.5,
		Tags: []string{"A", "B"}, CreatedAt: base,
	}
	store.writeEngram(target)
	// Three newer A-only AND three newer B-only decoys. With the tag scan limit
	// = CandidatesPerIndex*3 = 3, each tag's newest-first window is entirely
	// decoys and never reaches the target.
	for i := 0; i < 3; i++ {
		store.writeEngram(&storage.Engram{
			Concept: "a-decoy", Content: "newer, A only", Confidence: 1.0, Stability: 30.0, Relevance: 0.5,
			Tags: []string{"A"}, CreatedAt: base.Add(time.Duration(i+1) * time.Hour),
		})
		store.writeEngram(&storage.Engram{
			Concept: "b-decoy", Content: "newer, B only", Confidence: 1.0, Stability: 30.0, Relevance: 0.5,
			Tags: []string{"B"}, CreatedAt: base.Add(time.Duration(i+1) * time.Hour),
		})
	}

	// No FTS/HNSW/decay contribution: the target can only arrive via tag seeding.
	store.recent = nil
	eng := newTestEngine(store, &stubFTS{}, &emptyHNSW{})
	result, err := eng.Run(context.Background(), &activation.ActivateRequest{
		Context:            []string{"anything"},
		Threshold:          0.0,
		MaxResults:         10,
		CandidatesPerIndex: 1, // tag scan limit becomes k*3 = 3, forcing both windows to truncate
		Weights:            rrfTagWeights(),
		Filters:            []activation.Filter{{Field: "tags_all", Value: []string{"A", "B"}}},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !containsID(result.Activations, target.ID) {
		t.Errorf("target (both tags, outside every truncated per-tag window) not surfaced — seeding must intersect the tag streams, not union truncated windows")
	}
	// The single-tag decoys must be excluded by the tags_all post-filter.
	if len(result.Activations) != 1 {
		t.Errorf("expected exactly the target to survive tags_all, got %d activations", len(result.Activations))
	}
}

// TestTagSeeding_NonRRFCosine is the caveat-1 repro: under a non-RRF scorer
// (ACT-R here), a tag-seeded candidate that is in no other pool has vectorScore=0
// and would be threshold-dropped because ContentMatch is zero. Phase 6 must give
// such tag-pool candidates a post-hoc cosine similarity against the query
// embedding so they score above zero and survive.
func TestTagSeeding_NonRRFCosine(t *testing.T) {
	store := newStubStore()

	// Embedding aligned with the stub embedder's fixed query vector ([0.1]*8),
	// so cosine similarity is ~1.0.
	aligned := make([]float32, 8)
	for i := range aligned {
		aligned[i] = 0.1
	}
	target := &storage.Engram{
		Concept: "target", Content: "tag only, no pool hit", Confidence: 1.0, Stability: 30.0, Relevance: 0.5,
		Tags: []string{"special"}, Embedding: aligned,
	}
	store.writeEngram(target)
	store.recent = nil // keep target out of the decay pool

	// emptyHNSW is non-nil, so phase1 computes the query embedding, but returns no
	// vector hits — target's vectorScore is 0 until the post-hoc cosine fix runs.
	eng := newTestEngine(store, &stubFTS{}, &emptyHNSW{})
	result, err := eng.Run(context.Background(), &activation.ActivateRequest{
		Context:    []string{"query text"},
		Threshold:  0.0,
		MaxResults: 10,
		Weights: &activation.Weights{
			SemanticSimilarity: 0.6,
			FullTextRelevance:  0.4,
			UseACTR:            true,
			ACTRDecay:          0.5,
			ACTRHebScale:       4.0,
		},
		Filters: []activation.Filter{{Field: "tags_all", Value: []string{"special"}}},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !containsID(result.Activations, target.ID) {
		t.Fatalf("tag-seeded candidate was threshold-dropped in the non-RRF path (caveat 1): got %d activations", len(result.Activations))
	}
	for _, a := range result.Activations {
		if a.Engram.ID == target.ID {
			if a.Score <= 0 {
				t.Errorf("target score = %v, want > 0 (post-hoc cosine should lift it)", a.Score)
			}
			if a.Components.SemanticSimilarity <= 0 {
				t.Errorf("target SemanticSimilarity = %v, want > 0 (post-hoc cosine)", a.Components.SemanticSimilarity)
			}
		}
	}
}

// TestTagSeeding_ComposesWithCreatedAfter verifies that a tags_all filter
// combined with created_after composes correctly: the tag scan runs within the
// time window, so a recent tagged engram (absent from other pools) surfaces while
// an older tagged engram outside the window does not.
func TestTagSeeding_ComposesWithCreatedAfter(t *testing.T) {
	store := newStubStore()

	now := time.Now()
	threshold := now.Add(-2 * time.Hour)

	recent := &storage.Engram{
		Concept: "recent", Content: "in window", Confidence: 1.0, Stability: 30.0, Relevance: 0.5,
		Tags: []string{"special"}, CreatedAt: now.Add(-1 * time.Hour),
	}
	old := &storage.Engram{
		Concept: "old", Content: "before window", Confidence: 1.0, Stability: 30.0, Relevance: 0.5,
		Tags: []string{"special"}, CreatedAt: now.Add(-5 * time.Hour),
	}
	store.writeEngram(recent)
	store.writeEngram(old)
	store.recent = nil

	eng := newTestEngine(store, &stubFTS{}, &emptyHNSW{})
	result, err := eng.Run(context.Background(), &activation.ActivateRequest{
		Context:    []string{"anything"},
		Threshold:  0.0,
		MaxResults: 10,
		Weights:    rrfTagWeights(),
		Filters: []activation.Filter{
			{Field: "tags_all", Value: []string{"special"}},
			{Field: "created_after", Op: "gt", Value: threshold},
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !containsID(result.Activations, recent.ID) {
		t.Errorf("recent in-window tagged engram was not surfaced")
	}
	if containsID(result.Activations, old.ID) {
		t.Errorf("old out-of-window tagged engram leaked through created_after")
	}
}

func containsID(acts []activation.ScoredEngram, id storage.ULID) bool {
	for _, a := range acts {
		if a.Engram != nil && a.Engram.ID == id {
			return true
		}
	}
	return false
}
