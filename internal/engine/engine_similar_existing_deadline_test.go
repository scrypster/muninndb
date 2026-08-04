package engine

import (
	"context"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// ---------------------------------------------------------------------------
// F3 (712-currency fix round): EmbedBudgetFraction is a FRACTION of the
// caller's remaining ctx deadline and a passthrough when ctx carries none —
// which REST/MBP writes commonly do not. A wedged embed backend therefore
// stalled the whole write response unboundedly. The fix bounds the
// self-query's own ctx with an absolute ceiling (similarExistingSelfQueryDeadline,
// derived from the design record's own pre-registered SHIP/KILL latency
// bar) regardless of what the caller's ctx looks like.
//
// hangingEmbedder is context-aware (mirrors a real HTTP-backed embedder,
// which activation's own EmbedBudgetFraction mechanism already assumes) —
// it blocks on ctx.Done() or a fixed multi-second delay, whichever comes
// first, so the fix's bounded ctx is what actually returns it early.
// ---------------------------------------------------------------------------

type hangingEmbedder struct{ dim int }

func (h hangingEmbedder) Embed(ctx context.Context, texts []string) ([]float32, error) {
	select {
	case <-time.After(3 * time.Second):
		return make([]float32, h.dim*len(texts)), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (h hangingEmbedder) Tokenize(text string) []string { return []string{text} }

func TestSimilarExisting_F3_WedgedEmbedder_BoundedByAbsoluteDeadline(t *testing.T) {
	eng, cleanup := testEnvWithHNSW(t, hangingEmbedder{dim: 8})
	defer cleanup()
	// context.Background() carries NO deadline — the exact REST/MBP shape
	// F3 reproduces.
	ctx := context.Background()
	const vault = "similar-existing-deadline-probe"

	start := time.Now()
	resp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault: vault, Concept: "wedged embedder probe",
		Content: "This write's self-query must not wait for the embed backend's 3-second hang.",
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	eng.WaitWriteTimeIdle()

	if elapsed >= 2*time.Second {
		t.Fatalf("F3 violated: write took %v — the self-query's Activate() call was not bounded "+
			"by an absolute ceiling and waited out the embedder's 3s hang", elapsed)
	}
	// The self-query's embed sub-call hits its own budget (derived from the
	// F3 ceiling) and activation gracefully degrades to BM25-only — the
	// SAME response-wide "semantic_degraded" basis an unreachable embed
	// backend produces elsewhere (activation/engine.go's phase1). That is
	// the expected, more informative outcome: F5's zero-row fix is what
	// makes it visible here rather than silent, since this vault has no
	// pre-existing candidates for the self-query to find.
	if resp.SimilarExistingBasis == "" {
		t.Fatalf("F3 violated: expected a non-empty SimilarExistingBasis on self-query "+
			"deadline pressure (elapsed %v) — an omitted advisory must name why, not go silent", elapsed)
	}
	if resp.SimilarExistingBasis != "semantic_degraded" {
		t.Errorf("expected SimilarExistingBasis %q, got %q (elapsed %v)",
			"semantic_degraded", resp.SimilarExistingBasis, elapsed)
	}
}
