package fts

import (
	"context"
	"math"
	"testing"

	"github.com/scrypster/muninndb/internal/storage"
)

// Calibration pins for issue #711: Search.Score is an absolute, IDF-weighted
// query-coverage score in [0,1] — not raw BM25, and never normalized against
// the result set. See .claude/deep-review/2026-07-29-fts-abstention-design.md
// and docs/internals/invariants.md COG-24.

// TestCalibration_SingleCommonTokenLowRelevance pins the sourdough-shape bug:
// a multi-term query where only ONE common term matches and the rest are
// corpus-absent must score low, not saturate to ~1.0.
func TestCalibration_SingleCommonTokenLowRelevance(t *testing.T) {
	db, cleanup := openTestDB(t)
	defer cleanup()

	idx := New(db)
	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 100})
	ws := store.VaultPrefix("cal")
	ctx := context.Background()

	// "system" is common: put it in most of a 20-doc corpus so its IDF is low.
	for i := 0; i < 19; i++ {
		id := [16]byte{byte(i + 1)}
		_ = idx.IndexEngram(ws, id, "some topic", "", "this system handles requests reliably", nil)
	}
	// Target doc: contains "system" once, nothing else query-relevant.
	targetID := [16]byte{20}
	_ = idx.IndexEngram(ws, targetID, "unrelated topic", "", "the system logs an event", nil)

	// Query has 1 common term ("system") + 2 corpus-absent terms.
	results, err := idx.Search(ctx, ws, "system zyxquux plarfnog", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	var got float64
	found := false
	for _, r := range results {
		if r.ID == targetID {
			got = r.Score
			found = true
		}
	}
	if !found {
		t.Fatal("target doc not found in results")
	}
	if got >= 0.15 {
		t.Errorf("single-common-token match Score = %v, want < 0.15 (corpus-absent terms must penalize)", got)
	}
	t.Logf("single-common-token, 2-absent-term query: Score = %.4f", got)
}

// TestCalibration_FullCoverageHighRelevance pins the positive side: a query
// whose every term is covered by the doc (prominently, e.g. in the concept
// field) must score high.
func TestCalibration_FullCoverageHighRelevance(t *testing.T) {
	db, cleanup := openTestDB(t)
	defer cleanup()

	idx := New(db)
	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 100})
	ws := store.VaultPrefix("cal")
	ctx := context.Background()

	// Filler corpus so idf isn't degenerate (N=1).
	for i := 0; i < 19; i++ {
		id := [16]byte{byte(i + 1)}
		_ = idx.IndexEngram(ws, id, "filler doc", "", "generic unrelated filler content here", nil)
	}
	targetID := [16]byte{20}
	_ = idx.IndexEngram(ws, targetID, "alpha beta gamma", "", "alpha beta gamma appear here prominently", nil)

	results, err := idx.Search(ctx, ws, "alpha beta gamma", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) == 0 || results[0].ID != targetID {
		t.Fatalf("expected target doc as top result, got %+v", results)
	}
	if results[0].Score <= 0.7 {
		t.Errorf("full 3-term coverage Score = %v, want > 0.7", results[0].Score)
	}
	if results[0].Score > 1.0 {
		t.Errorf("Score = %v, must never exceed 1.0", results[0].Score)
	}
	t.Logf("full 3-term coverage: Score = %.4f", results[0].Score)
}

// TestCalibration_AbsentTermUsesIdfMax pins the exact idfMax substitution: a
// query term with NO corpus term-stats entry must be charged the maximum-
// rarity IDF (df=0 evaluated at the corpus's own N), not skipped (idf=0).
func TestCalibration_AbsentTermUsesIdfMax(t *testing.T) {
	db, cleanup := openTestDB(t)
	defer cleanup()

	idx := New(db)
	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 100})
	ws := store.VaultPrefix("cal")
	ctx := context.Background()

	const nDocs = 9
	for i := 0; i < nDocs; i++ {
		id := [16]byte{byte(i + 1)}
		_ = idx.IndexEngram(ws, id, "widget", "", "a widget appears in every doc", nil)
	}
	// 10th doc distinguishes nothing new; N should be 9 after the loop above.
	N := float64(nDocs)
	idfMax := math.Log((N+0.5)/0.5 + 1)

	// Single-term query with the absent term alone: every doc scores 0
	// (denominator is idfMax, numerator is 0 for all docs since nothing
	// contains it) rather than being skipped/undefined.
	results, err := idx.Search(ctx, ws, "zyxquuxplarf", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("wholly-absent single-term query returned %d results, want 0 (numerator always 0)", len(results))
	}

	// Two-term query: "widget" (present, real low idf since it's in every
	// doc) + the absent term. Score = idf_widget*cov / (idf_widget + idfMax).
	// Verify the denominator actually reflects idfMax by checking the score
	// is close to idf_widget*cov/(idf_widget+idfMax) computed independently.
	idfWidget := idx.getIDF(ws, "widget", N)
	results2, err := idx.Search(ctx, ws, "widget zyxquuxplarf", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results2) == 0 {
		t.Fatal("expected results for widget+absent-term query")
	}
	// "widget" appears in BOTH the concept (weight 3.0) and content (weight
	// 1.0) fields of every filler doc, tf=1 each, dl==avgdl so tfNorm==1:
	// raw coverage = 1*3.0 + 1*1.0 = 4.0, capped at 1 by the per-term cap.
	expectedCov := 1.0
	expectedScore := idfWidget * expectedCov / (idfWidget + idfMax)
	got := results2[0].Score
	if math.Abs(got-expectedScore) > 1e-6 {
		t.Errorf("Score = %.8f, want %.8f (idfMax=%.6f, idfWidget=%.6f) — absent term must denominate at idfMax",
			got, expectedScore, idfMax, idfWidget)
	}
}

// TestCalibration_ScoreIndependentOfOtherResults is the anti-per-query-max
// pin: a document's Score must not depend on what else scored in the same
// search. We compute the expected Score for a low-scoring doc purely from its
// own posting/idf data (never referencing the other doc), then verify the
// actual Search result for that doc matches — even though a much
// higher-scoring doc is present in the same result set. If Search applied any
// per-query-max normalization (dividing every score by the top score), this
// pin would fail.
func TestCalibration_ScoreIndependentOfOtherResults(t *testing.T) {
	db, cleanup := openTestDB(t)
	defer cleanup()

	idx := New(db)
	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 100})
	ws := store.VaultPrefix("cal")
	ctx := context.Background()

	// Filler so idf isn't degenerate.
	for i := 0; i < 8; i++ {
		id := [16]byte{byte(i + 1)}
		_ = idx.IndexEngram(ws, id, "filler", "", "generic unrelated filler content here", nil)
	}
	lowID := [16]byte{9}
	_ = idx.IndexEngram(ws, lowID, "unrelated", "", "widget mentioned once in passing", nil)
	highID := [16]byte{10}
	_ = idx.IndexEngram(ws, highID, "widget widget widget", "", "widget widget widget widget widget", nil)

	results, err := idx.Search(ctx, ws, "widget", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	var lowScore, highScore float64
	var lowFound, highFound bool
	for _, r := range results {
		if r.ID == lowID {
			lowScore, lowFound = r.Score, true
		}
		if r.ID == highID {
			highScore, highFound = r.Score, true
		}
	}
	if !lowFound || !highFound {
		t.Fatalf("expected both docs in results, got %+v", results)
	}
	if highScore <= lowScore {
		t.Fatalf("test setup: expected highID to outscore lowID (%v vs %v)", highScore, lowScore)
	}
	// highScore must NOT be exactly 1.0 (which would indicate a per-query-max
	// rescale pinning the top hit to 1.0 regardless of absolute quality).
	// A single term's coverage caps at 1.0 by design (the per-term cap), so
	// equal-to-1.0 alone isn't conclusive — the real proof is lowScore below,
	// computed independently of highScore's existence.
	N := float64(10)
	idfWidget := idx.getIDF(ws, "widget", N)
	if idfWidget <= 0 {
		t.Fatal("test setup: expected widget to have a real idf")
	}
	// lowID: tf=1 in content, dl==avgdl (all docs same length) => tfNorm==1,
	// cov = 1*1.0/(k1+1); single-term query => Score = cov (idf cancels).
	expectedLow := (1.0 * fieldWeightContent) / (k1 + 1)
	if expectedLow > 1 {
		expectedLow = 1
	}
	if math.Abs(lowScore-expectedLow) > 0.05 {
		t.Errorf("lowID Score = %.4f, want ~%.4f computed independently of highID's presence — "+
			"a per-query-max normalization would have rescaled this", lowScore, expectedLow)
	}
}
