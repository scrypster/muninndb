package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/scrypster/muninndb/internal/auth"
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
	// BEHAVIOUR CHANGE, deliberate: Explain now runs activation with the
	// negative-threshold diagnostic bypass, so an engram that ANY index can
	// reach gets a real score card even when it is far below the recall bar —
	// that is the whole point of the bypass (a below-bar engram used to be
	// gated out before it could be scored, and its absence was
	// indistinguishable from "never a candidate"). For this fixture the broad
	// candidate sets do reach the engram, so it is SCORED — with honest, tiny
	// numbers — and the honesty this test protects lives in WouldReturn=false
	// plus a real threshold, not in the absence of a card. Scored=false is
	// still possible (an engram outside every candidate set), and the
	// distinct Note for that case is covered by the missing/no-pool paths.
	if !data.Scored {
		t.Fatalf("Scored = false under the diagnostic bypass for an engram the candidate sets reach; Note=%q", data.Note)
	}
	// Everything query-independent is still knowable and must be reported.
	if data.Concept == "" {
		t.Error("Concept is empty even though the engram exists")
	}
	if data.Confidence != 1.0 {
		t.Errorf("Confidence = %v, want 1.0 — stored confidence does not depend on the query", data.Confidence)
	}
	if data.WouldReturn {
		t.Error("WouldReturn = true for an engram this query cannot justify returning — the bypass must not leak into the verdict")
	}
	if data.Threshold <= 0 {
		t.Errorf("Threshold = %v: explain must report the REAL recall bar, never the bypass sentinel", data.Threshold)
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
// reports to the bar recall ACTUALLY applies. Ownership moved: the MCP surface
// no longer pre-fills a default (it forwards 0, like every other transport),
// so the mirror is now the ENGINE's fusion-aware coerce — ACT-R 0.1 (COG-26's
// calibration point on the absolute scale), rrf 0.001 (#590), weighted_sum
// 0.5 (the only bar validated against that formula).
//
// The previous version of this pin asserted 0.5 — and kept passing while the
// mirror it protected was broken, because both sides of the mirror had moved
// and the pin only looked at one (adversarial review of #754, finding 4). If
// a default moves again, ALL THREE cases here must be re-derived together
// with the engine.go COG-6 coerce.
func TestRecallThresholdFor_MirrorsRecallSurface(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	if got := eng.recallThresholdFor("some-vault"); got != 0.1 {
		t.Errorf("recallThresholdFor(ACT-R vault) = %v, want 0.1 (the engine default the MCP surface no longer overrides)", got)
	}
}

// The other two branches of the fusion-aware default, pinned so a future edit
// to one cannot silently drift the mirror for the others. Uses the auth-store
// path (the same one production vault config takes) rather than a setter the
// engine does not have.
func TestRecallThresholdFor_FusionBranches(t *testing.T) {
	eng, as, _, cleanup := testEnvWithAuth(t)
	defer cleanup()
	require.NoError(t, as.SetVaultConfig(auth.VaultConfig{
		Name: "rrf-vault", Public: true,
		Plasticity: &auth.PlasticityConfig{ScoringFusion: ptr("rrf")},
	}))
	require.NoError(t, as.SetVaultConfig(auth.VaultConfig{
		Name: "ws-vault", Public: true,
		Plasticity: &auth.PlasticityConfig{ScoringFusion: ptr("weighted_sum")},
	}))
	if got := eng.recallThresholdFor("rrf-vault"); got != 0.001 {
		t.Errorf("recallThresholdFor(rrf vault) = %v, want 0.001 (#590's mechanism)", got)
	}
	if got := eng.recallThresholdFor("ws-vault"); got != 0.5 {
		t.Errorf("recallThresholdFor(weighted_sum vault) = %v, want 0.5 (the only bar validated against that formula)", got)
	}
}
