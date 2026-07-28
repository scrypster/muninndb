package activation_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/storage"
)

// noopFTS is an activation.FTSIndex that never matches anything — used so
// this test's candidate pool comes ONLY from RecentActive (decay) and the
// tag/tag_prefix seeding paths, never from full-text overlap.
type noopFTS struct{}

func (noopFTS) Search(_ context.Context, _ [8]byte, _ string, _ int) ([]activation.ScoredID, error) {
	return nil, nil
}

// noopEmbedder returns nil — this test never exercises HNSW/vector search
// (the engine is constructed with hnsw=nil), so no embedding is needed.
type noopEmbedder struct{}

func (noopEmbedder) Embed(_ context.Context, _ []string) ([]float32, error) { return nil, nil }
func (noopEmbedder) Tokenize(text string) []string                          { return []string{text} }

// openRealActivationEnv wires a real PebbleStore + real activation.ActivationEngine
// in a temp dir (no FTS/HNSW matching — see noopFTS/noopEmbedder) so this test
// exercises the actual production seeding path end to end, not a stub.
func openRealActivationEnv(t *testing.T) (*storage.PebbleStore, *activation.ActivationEngine, func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "muninndb-s1-test-*")
	if err != nil {
		t.Fatal(err)
	}
	db, err := storage.OpenPebble(dir, storage.DefaultOptions())
	if err != nil {
		os.RemoveAll(dir)
		t.Fatal(err)
	}
	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 1000})
	actEngine := activation.New(store, noopFTS{}, nil, noopEmbedder{})
	return store, actEngine, func() {
		actEngine.Close()
		store.Close()
		os.RemoveAll(dir)
	}
}

// TestRawTagRange_SeedsDueItems is the S1 acceptance case (RED before the
// change, GREEN after): N engrams are written with a "due:<date>" tag whose
// date is <= today, with content that shares no words with the query context
// (so FTS cannot find them), backdated 30 days and never accessed (so they
// rank at the bottom of the decay/RecentActive pool, well outside a small
// CandidatesPerIndex window). recall(tag_filter{prefix:"due:", lte:today})
// must SEED these as candidates via the S1 raw-tag-range index (0x2B) and
// return ALL N of them — not rely on them already being a candidate from
// FTS/HNSW/decay. Before S1, tag_prefix was only checked in phase 6's
// passesMetaFilter (a post-hoc filter on whatever candidates other indices
// happened to surface), so these never-touched, low-relevance, content-
// unrelated engrams were never candidates in the first place and this test
// failed with 0 results.
//
// Uses RRF fusion scoring (UseRRFFusion): a pure tag-pool hit's RRF score is
// non-zero purely from being in the tag pool (1/(rrfK_Tag+rank+1)) — it does
// not require ALSO matching on content, which is the entire point of a
// content-unrelated reminder-style tag. This is the scoring mode this
// candidate-injection design targets (see seedTagCandidates' doc comment);
// the legacy/ACT-R content-gated paths intentionally require some non-zero
// content signal (FTS or a real HNSW vector hit) before a candidate can
// score above zero, which a purely tag-seeded, content-unrelated engram will
// never have in this deliberately FTS/HNSW-starved test setup.
func TestRawTagRange_SeedsDueItems(t *testing.T) {
	store, actEngine, cleanup := openRealActivationEnv(t)
	defer cleanup()

	ctx := context.Background()
	ws := store.VaultPrefix("s1-due-vault")
	backdated := time.Now().Add(-30 * 24 * time.Hour)

	const n = 12
	dueIDs := make(map[storage.ULID]bool, n)
	for i := 0; i < n; i++ {
		eng := &storage.Engram{
			Concept:    fmt.Sprintf("unrelated-topic-%d", i),
			Content:    fmt.Sprintf("zqxw glorbnak fluvventious %d prendicular skoombat", i),
			Tags:       []string{"due:2026-01-01"}, // well before "today" per the lte bound below
			Confidence: 1.0,
			Stability:  30.0,
			CreatedAt:  backdated,
			LastAccess: backdated, // never accessed since creation
			Relevance:  0,         // lowest relevance bucket — excluded from any small decay window
		}
		id, err := store.WriteEngram(ctx, ws, eng)
		if err != nil {
			t.Fatalf("WriteEngram (due item %d): %v", i, err)
		}
		dueIDs[id] = true
	}

	// Decoys: recent, high relevance, occupy the entire (tiny) decay window so
	// the due items cannot ride along in sets.decay by chance.
	const decoys = 10
	for i := 0; i < decoys; i++ {
		_, err := store.WriteEngram(ctx, ws, &storage.Engram{
			Concept:    fmt.Sprintf("decoy-%d", i),
			Content:    fmt.Sprintf("completely different filler sentence number %d", i),
			Confidence: 1.0,
			Stability:  30.0,
			CreatedAt:  time.Now(),
			LastAccess: time.Now(),
			Relevance:  1.0,
		})
		if err != nil {
			t.Fatalf("WriteEngram (decoy %d): %v", i, err)
		}
	}

	req := &activation.ActivateRequest{
		VaultPrefix: ws,
		Context:     []string{"totally different query about nothing related"},
		MaxResults:  500,
		Weights:     &activation.Weights{UseRRFFusion: true},
		// Deliberately small (well below n=12 decoys' count of RecentActive-
		// eligible items): without S1 seeding, this bounds RecentActive/FTS
		// pools far below n, so the due items would be truncated away. The tag
		// seeding path itself scans with limit=CandidatesPerIndex*3, so this
		// must stay >= ceil(n/3) to avoid truncating the raw-tag-range scan.
		CandidatesPerIndex: 10,
		Filters: []activation.Filter{
			{Field: "tag_prefix", Op: "lte", Value: [2]string{"due:", "2026-07-27"}},
		},
	}

	result, err := actEngine.Run(ctx, req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := make(map[storage.ULID]bool, len(result.Activations))
	for _, a := range result.Activations {
		got[a.Engram.ID] = true
	}

	missing := 0
	for id := range dueIDs {
		if !got[id] {
			missing++
		}
	}
	if missing > 0 {
		t.Errorf("expected all %d due-tagged engrams to be seeded and returned, missing %d (got %d total activations)",
			n, missing, len(result.Activations))
	}
}

// TestRawTagRange_SeedsDueItems_DefaultScoring is the S1 gap-closure case
// (RED before the threshold-bypass change, GREEN after): the SAME scenario as
// TestRawTagRange_SeedsDueItems above (content-unrelated, backdated,
// never-accessed due-tagged engrams, decoys occupying the decay window), but
// run under DEFAULT scoring (Weights: nil -> ACT-R, NOT RRF) AND at the
// production default thresholds users actually run — ACT-R's resolved 0.05
// and the MCP surface default 0.5.
//
// Under ACT-R, contentMatch = SemanticSimilarity*vectorScore +
// FullTextRelevance*normalizedFTS. This test's engrams deliberately share no
// words with the query and there is no HNSW index (embedder returns nil), so
// contentMatch is ~0 for every due item. The tagMatchFloor makes the score
// strictly positive, but a floored tag-only hit against a 30-day-decayed,
// never-accessed engram scores far below either production threshold
// (floor 0.1 * softplus(B~0.17) ~= 0.017 < 0.05 < 0.5) — so the floor ALONE
// still silently dropped every due item at the thresholds users run.
//
// The real fix is the threshold BYPASS in Phase 6 scoring: a candidate that
// matched an explicit tag filter (inTagPool, already verified by
// passesMetaFilter) is never dropped for scoring below the relevance
// threshold. A filter DEFINES the candidate set; the relevance threshold only
// RANKS within it. The floor is retained purely to give tag-only hits a
// sensible ordering relative to one another and below genuine content matches
// (see TestComputeACTR_TagPoolFloor_GenuineMatchOutranksFlooredTagOnly).
//
// This test asserts the property that actually matters to reminders: under
// default scoring, at BOTH production default thresholds, every explicitly
// tag-filtered due item surfaces regardless of how far its content relevance
// or base-level activation has decayed.
func TestRawTagRange_SeedsDueItems_DefaultScoring(t *testing.T) {
	for _, threshold := range []float64{0.05, 0.5} {
		t.Run(fmt.Sprintf("threshold=%g", threshold), func(t *testing.T) {
			store, actEngine, cleanup := openRealActivationEnv(t)
			defer cleanup()

			ctx := context.Background()
			ws := store.VaultPrefix(fmt.Sprintf("s1-due-vault-default-scoring-%g", threshold))
			backdated := time.Now().Add(-30 * 24 * time.Hour)

			const n = 12
			dueIDs := make(map[storage.ULID]bool, n)
			for i := 0; i < n; i++ {
				eng := &storage.Engram{
					Concept:    fmt.Sprintf("unrelated-topic-%d", i),
					Content:    fmt.Sprintf("zqxw glorbnak fluvventious %d prendicular skoombat", i),
					Tags:       []string{"due:2026-01-01"},
					Confidence: 1.0,
					Stability:  30.0,
					CreatedAt:  backdated,
					LastAccess: backdated,
					Relevance:  0,
				}
				id, err := store.WriteEngram(ctx, ws, eng)
				if err != nil {
					t.Fatalf("WriteEngram (due item %d): %v", i, err)
				}
				dueIDs[id] = true
			}

			const decoys = 10
			for i := 0; i < decoys; i++ {
				_, err := store.WriteEngram(ctx, ws, &storage.Engram{
					Concept:    fmt.Sprintf("decoy-%d", i),
					Content:    fmt.Sprintf("completely different filler sentence number %d", i),
					Confidence: 1.0,
					Stability:  30.0,
					CreatedAt:  time.Now(),
					LastAccess: time.Now(),
					Relevance:  1.0,
				})
				if err != nil {
					t.Fatalf("WriteEngram (decoy %d): %v", i, err)
				}
			}

			req := &activation.ActivateRequest{
				VaultPrefix:        ws,
				Context:            []string{"totally different query about nothing related"},
				MaxResults:         500,
				Weights:            nil, // DEFAULT scoring: resolveWeights falls back to ACT-R, not RRF.
				Threshold:          threshold,
				CandidatesPerIndex: 10,
				Filters: []activation.Filter{
					{Field: "tag_prefix", Op: "lte", Value: [2]string{"due:", "2026-07-27"}},
				},
			}

			result, err := actEngine.Run(ctx, req)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}

			got := make(map[storage.ULID]bool, len(result.Activations))
			for _, a := range result.Activations {
				got[a.Engram.ID] = true
			}

			missing := 0
			for id := range dueIDs {
				if !got[id] {
					missing++
				}
			}
			if missing > 0 {
				t.Errorf("expected all %d due-tagged engrams to surface under DEFAULT (ACT-R) scoring at production threshold %g, missing %d (got %d total activations)",
					n, threshold, missing, len(result.Activations))
			}
		})
	}
}
