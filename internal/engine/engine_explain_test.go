package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// seedExplainVault writes one engram and flushes FTS so activation can see it.
func seedExplainVault(t *testing.T, eng *Engine, vault string) string {
	t.Helper()
	resp, err := eng.Write(context.Background(), &mbp.WriteRequest{
		Vault:      vault,
		Concept:    "Postgres connection pool sizing",
		Content:    "The pool is capped at 40 connections per node.",
		Confidence: 1.0,
	})
	if err != nil {
		t.Fatalf("seed write: %v", err)
	}
	awaitFTS(t, eng)
	return resp.ID
}

// TestExplain_ScoredEngramReportsConfidenceAndThreshold pins the two values an
// operator debugging "why didn't my memory come back" cannot work without:
// the engram's stored confidence and the threshold would_return is measured
// against. Both were structurally unreachable before this fix — ExplainData
// carried no Confidence field at all (mbp.ScoreComponents has none), and
// Threshold was a hardcoded 0.0 that made would_return mean "was a candidate"
// rather than "clears recall's bar".
//
// RED without the fix: ExplainData has no Confidence/Found/Scored fields
// (compile failure) and Threshold is 0.
func TestExplain_ScoredEngramReportsConfidenceAndThreshold(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()
	id := seedExplainVault(t, eng, "explain-scored")

	data, err := eng.Explain(ctx, "explain-scored", id, []string{"Postgres", "connection", "pool"}, nil)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if !data.Found {
		t.Fatal("Found = false for an engram that exists")
	}
	if !data.Scored {
		t.Fatal("Scored = false for an engram the query demonstrably matches")
	}
	if data.Confidence != 1.0 {
		t.Errorf("Confidence = %v, want 1.0 (the stored value)", data.Confidence)
	}
	if data.Concept == "" {
		t.Error("Concept is empty for a scored engram")
	}
	if data.Components.FullTextRelevance == 0 {
		t.Error("FullTextRelevance = 0 for an engram matched on its own words")
	}
	if data.Threshold != defaultRecallThreshold {
		t.Errorf("Threshold = %v, want the default recall threshold %v", data.Threshold, defaultRecallThreshold)
	}
	// would_return must mean "clears the threshold", not "was a candidate".
	want := data.FinalScore >= data.Threshold
	if data.WouldReturn != want {
		t.Errorf("WouldReturn = %v, want %v (score %v vs threshold %v)",
			data.WouldReturn, want, data.FinalScore, data.Threshold)
	}
}

// TestExplain_UnscoredEngramSaysSoRatherThanReturningZeros is the core of the
// bug an evaluator hit: an engram that the query's activation run never scored
// came back as an all-zero ExplainData — zero components, empty concept, zero
// confidence — with nothing distinguishing "this signal measured zero" from
// "nothing computed this signal". That is the silent-substitution failure class
// (CLAUDE.md §2.1/§2.2) inside the one tool built for debugging recall.
//
// RED without the fix: Concept == "", Confidence unavailable, Note == "" and
// there is no Scored flag — the response is indistinguishable from a genuine
// all-zero score.
func TestExplain_UnscoredEngramSaysSoRatherThanReturningZeros(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()
	id := seedExplainVault(t, eng, "explain-unscored")

	data, err := eng.Explain(ctx, "explain-unscored", id, []string{"kangaroo", "zeppelin"}, nil)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if !data.Found {
		t.Fatal("Found = false for an engram that exists in the vault")
	}
	if data.Scored {
		t.Fatal("Scored = true for a query that never reached this engram")
	}
	if data.Note == "" {
		t.Error("Note is empty: an unscored explain must say why its components are absent")
	}
	// Everything query-independent is still knowable and must be reported.
	if data.Concept == "" {
		t.Error("Concept is empty even though the engram exists")
	}
	if data.Confidence != 1.0 {
		t.Errorf("Confidence = %v, want 1.0 — stored confidence does not depend on the query", data.Confidence)
	}
	if data.WouldReturn {
		t.Error("WouldReturn = true for an unscored engram")
	}
}

// TestExplain_MissingEngramReportsNotFound: an id that was never written must
// report found=false with an explanation, not a zeroed score card that looks
// like a real (bad) score.
func TestExplain_MissingEngramReportsNotFound(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	data, err := eng.Explain(ctx, "explain-missing", "01HNKZ5F0K0000000000000000", []string{"anything"}, nil)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if data.Found {
		t.Error("Found = true for an engram that was never written")
	}
	if data.Scored || data.WouldReturn {
		t.Error("Scored/WouldReturn must be false for a missing engram")
	}
	if !strings.Contains(strings.ToLower(data.Note), "not found") {
		t.Errorf("Note = %q, want it to say the engram was not found", data.Note)
	}
}

// TestExplain_MalformedIDIsExplained: a non-ULID id previously produced the
// same all-zero card as a genuine miss. It must name the problem.
func TestExplain_MalformedIDIsExplained(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()

	data, err := eng.Explain(context.Background(), "explain-bad-id", "not-a-ulid", []string{"anything"}, nil)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if data.Found || data.Scored {
		t.Error("a malformed id must not report Found/Scored")
	}
	if !strings.Contains(data.Note, "ULID") {
		t.Errorf("Note = %q, want it to name the malformed ULID", data.Note)
	}
}

// TestRecallThresholdFor_MirrorsRecallSurface pins the threshold Explain
// reports to the value muninn_recall actually defaults to (internal/mcp/
// handlers.go handleRecall): 0.5 normally, 0 when the vault fuses with RRF
// (RRF scores are not calibrated to that scale). If the recall default ever
// moves, this pin and the comment in handleRecall have to move with it.
func TestRecallThresholdFor_MirrorsRecallSurface(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	if got := eng.recallThresholdFor("some-vault"); got != 0.5 {
		t.Errorf("recallThresholdFor = %v, want 0.5", got)
	}
}
