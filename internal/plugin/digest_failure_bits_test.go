package plugin

import (
	"context"
	"testing"
)

// TestDigestEmbedAndEnrichFailedAreDistinctBits pins the #605 bit-collision
// fix directly: DigestEmbedFailed and DigestEnrichFailed must not alias the
// same bit. Before the fix they were literally the same constant
// (DigestEnrichFailed = DigestEmbedFailed), so this assertion fails outright.
func TestDigestEmbedAndEnrichFailedAreDistinctBits(t *testing.T) {
	if DigestEmbedFailed == DigestEnrichFailed {
		t.Fatalf("DigestEmbedFailed (%#x) and DigestEnrichFailed (%#x) alias the same bit", DigestEmbedFailed, DigestEnrichFailed)
	}
}

// TestEmbedFailureDoesNotBlockEnrichScan pins the #605 cross-stage-blocking
// fix: an engram that failed ONLY the embed stage must still be picked up by
// the enrich retroactive processor. Before the fix, DigestEmbedFailed and
// DigestEnrichFailed shared bit 0x80, so stamping an engram with
// DigestEmbedFailed made the enrich processor's skipFlags() check (which
// excludes DigestEnrichFailed) exclude it too — one transient embed failure
// silently starved that engram of LLM enrichment forever.
func TestEmbedFailureDoesNotBlockEnrichScan(t *testing.T) {
	eng := &Engram{Concept: "embed-failed-only", Content: "content"}
	store := newFlagAwareStore(eng)
	ctx := context.Background()

	// Simulate the embed processor's permanent-failure path: mark ONLY the
	// embed failure, as retroactive.go's flushMicroBatch does today.
	if err := store.SetDigestFlag(ctx, eng.ID, DigestEmbedFailed); err != nil {
		t.Fatalf("SetDigestFlag(DigestEmbedFailed): %v", err)
	}

	enrichPlugin := &enrichMockForRetro{
		mockPlugin:   mockPlugin{name: "enrich-after-embed-failure", tier: TierEnrich},
		enrichResult: &EnrichmentResult{Summary: "enriched"},
	}
	rp := NewRetroactiveProcessor(store, enrichPlugin, DigestEnrich)

	rp.processBatch(ctx)

	if enrichPlugin.callCount != 1 {
		t.Fatalf("enrich processor call count = %d, want 1 — an embed-only failure must not block enrichment (bit collision between DigestEmbedFailed and DigestEnrichFailed)", enrichPlugin.callCount)
	}
	if store.flags[eng.ID]&DigestEnrich == 0 {
		t.Fatalf("engram was never enriched after an embed-only failure; flags=%#x", store.flags[eng.ID])
	}
}
