//go:build localassets

package engine

import (
	"math"
	"testing"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// ---------------------------------------------------------------------------
// #800 / COG-31 end to end: a symmetric association must lift recall from
// EITHER endpoint, through the real recall pipeline with the real bundled
// embedder — not just inside phase4HebbianBoost.
//
// The deterministic exact-equality proof lives in
// internal/engine/activation/hebbian_symmetry_test.go, where the activation
// log's timestamp can be pinned and the identity is exact to 1e-9. This test
// answers the different question that one cannot: is the new reader actually
// WIRED into the recall path a user hits?
//
// Every fixture is synthetic — invented operational wording for a product that
// does not exist.
// ---------------------------------------------------------------------------

const (
	symIrrigationConcept = "greenhouse irrigation schedule"
	symIrrigationText    = "The greenhouse irrigation schedule waters bed rows at 06:00 and 18:00 using the north cistern."
	symCisternConcept    = "north cistern refill"
	symCisternText       = "The north cistern refills from roof catchment and is topped up by tanker when it drops below a quarter."
)

// symmetryArm links older→newer (the direction the Hebbian worker canonicalises
// to), primes ONE endpoint into the activation log, then recalls and returns
// the hebbian_boost the OTHER endpoint receives.
func symmetryArm(t *testing.T, primeQuery, scoreQuery, scoreConcept string) float64 {
	t.Helper()
	h, cleanup := newRealEmbedHarness(t)
	defer cleanup()

	// Written in this order, so `older` really is the byte-wise-smaller ULID —
	// which is the endpoint canonicalPair makes the association's SOURCE.
	older := h.writeEmbedded(symIrrigationConcept, symIrrigationText)
	newer := h.writeEmbedded(symCisternConcept, symCisternText)
	h.eng.waitWriteTimeIdle()

	// Weight 0.4, not 1.0: phase4HebbianBoost clamps the summed boost at 1.0,
	// and a clamped arm would compare equal to another clamped arm no matter
	// what the underlying read did.
	if _, err := h.eng.Link(h.ctx, &mbp.LinkRequest{
		Vault: "default", SourceID: older, TargetID: newer, Weight: 0.4,
	}); err != nil {
		t.Fatalf("link: %v", err)
	}

	// Prime: recall one endpoint so it lands in the recency-weighted activation
	// log that phase4HebbianBoost reads on the NEXT call. Drained
	// deterministically — WaitLogIdle, never a sleep (#722, #782).
	if _, err := h.eng.Activate(h.ctx, &mbp.ActivateRequest{
		Vault: "default", Context: []string{primeQuery}, MaxResults: 5,
	}); err != nil {
		t.Fatalf("prime activate: %v", err)
	}
	h.eng.activation.WaitLogIdle()
	h.eng.waitWriteTimeIdle()

	resp, err := h.eng.Activate(h.ctx, &mbp.ActivateRequest{
		Vault: "default", Context: []string{scoreQuery}, MaxResults: 5,
	})
	if err != nil {
		t.Fatalf("score activate: %v", err)
	}
	for _, it := range resp.Activations {
		if it.Concept == scoreConcept {
			t.Logf("scored %q: hebbian_boost=%.9g score=%.4f", it.Concept,
				it.ScoreComponents.HebbianBoost, it.Score)
			return float64(it.ScoreComponents.HebbianBoost)
		}
	}
	t.Fatalf("%q did not come back for query %q; rows: %v", scoreConcept, scoreQuery, conceptsOf(resp))
	return 0
}

func conceptsOf(resp *mbp.ActivateResponse) []string {
	out := make([]string, 0, len(resp.Activations))
	for _, it := range resp.Activations {
		out = append(out, it.Concept)
	}
	return out
}

// TestRecall_HebbianBoostSymmetricEndToEnd is the wiring proof for #800.
//
// ARM 1 (worked before this fix): the link's SOURCE is the candidate, so the
// forward 0x03 read finds it.
// ARM 2 (scored exactly 0 before this fix): the link's TARGET is the candidate,
// and the edge is only on 0x04 from its point of view.
//
// The two arms are compared with a RELATIVE tolerance, not 1e-9. That is not
// slack for a wrong answer: phase4HebbianBoost multiplies by exp(-age/3600)
// where age is whole wall-clock seconds since the priming recall, and the two
// arms are separate embedder-backed runs seconds apart. 5e-3 covers ~18 s of
// spread; the EXACT identity is asserted, with the recency term pinned, by
// TestHebbianBoost_IsSymmetricInPairOrder.
func TestRecall_HebbianBoostSymmetricEndToEnd(t *testing.T) {
	// ARM 1: prime the newer memory, score the older one (forward read).
	arm1 := symmetryArm(t,
		"how does the cistern get refilled",
		"when are the beds watered", symIrrigationConcept)

	// ARM 2: prime the older memory, score the newer one (reverse read).
	arm2 := symmetryArm(t,
		"when are the beds watered",
		"how does the cistern get refilled", symCisternConcept)

	t.Logf("arm1 (candidate = link SOURCE) = %.9g", arm1)
	t.Logf("arm2 (candidate = link TARGET) = %.9g", arm2)

	// K4: both arms at zero is UNDERPOWERED, not a pass.
	if arm1 == 0 && arm2 == 0 {
		t.Fatal("UNDERPOWERED, not a pass: both arms scored 0 — the link or the " +
			"activation-log priming did not take effect")
	}
	if arm1 <= 0 {
		t.Fatalf("arm1 (the direction that already worked) = %v, want > 0 — the fixture is broken", arm1)
	}
	if arm2 <= 0 {
		t.Fatalf("#800: a link is invisible from its TARGET. arm2 hebbian_boost = %v, want > 0", arm2)
	}
	if rel := math.Abs(arm1-arm2) / arm1; rel >= 5e-3 {
		t.Fatalf("#800: the same link scores materially differently by endpoint.\n"+
			"  arm1 = %.9g\n  arm2 = %.9g\n  relative diff = %.3g, want < 5e-3", arm1, arm2, rel)
	}
}
