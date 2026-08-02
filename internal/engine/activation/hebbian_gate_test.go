package activation_test

import (
	"context"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/storage"
)

// ---------------------------------------------------------------------------
// COG-32: HebbianEnabled is SYMMETRIC — it gates the read-side boost, not only
// the learning submission and the edge decay.
//
// Before this test, `phase4HebbianBoost` ran unconditionally in Run() while its
// neighbour `phase4_5TransitionBoost` was gated on `req.PASEnabled`. A vault
// with `hebbian_enabled: false` (the `scratchpad` preset, which also sets
// `assoc_decay_factor: 0`) therefore had every recall scored by association
// edges it would never update and never decay — a frozen prior nothing in the
// vault's configuration could turn off.
//
// PRIVACY: every string and identifier below is synthetic and authored here.
// ---------------------------------------------------------------------------

// hebGateFixture builds a two-engram corpus where `partner` is strongly
// associated with `probe`, and seeds the activation log so phase 4 has a
// "recently activated" partner to spread from. Both halves are required: the
// boost is `Σ edgeWeight * recencyWeight(partner)` over the candidate's
// associations, so a strong edge alone contributes nothing.
type hebGateFixture struct {
	store  *stubStore
	fts    *stubFTS
	probe  storage.ULID
	parter storage.ULID
}

func newHebGateFixture(t *testing.T) *hebGateFixture {
	t.Helper()
	store := newStubStore()

	probe := &storage.Engram{
		Concept:    "kiln firing schedule",
		Content:    "the bisque kiln ramps at eighty degrees an hour to cone zero four",
		Embedding:  []float32{0.1, 0.1, 0.1, 0.1, 0.1, 0.1, 0.1, 0.1},
		Confidence: 1.0,
		CreatedAt:  time.Now().Add(-72 * time.Hour),
	}
	partner := &storage.Engram{
		Concept:    "glaze mixing ratio",
		Content:    "the celadon glaze is mixed at one part ash to two parts feldspar",
		Embedding:  []float32{0.1, 0.1, 0.1, 0.1, 0.1, 0.1, 0.1, 0.1},
		Confidence: 1.0,
		CreatedAt:  time.Now().Add(-72 * time.Hour),
	}
	store.writeEngram(probe)
	store.writeEngram(partner)

	// A saturated edge probe -> partner. Written directly so the test does not
	// depend on the Hebbian worker having run.
	store.assocs[probe.ID] = []storage.Association{
		{TargetID: partner.ID, Weight: 1.0, RelType: storage.RelRelatesTo},
	}

	return &hebGateFixture{
		store:  store,
		fts:    &stubFTS{results: []activation.ScoredID{{ID: probe.ID, Score: 1.0}}},
		probe:  probe.ID,
		parter: partner.ID,
	}
}

// run executes one activation on a FRESH engine (Run mutates the activation
// log, so a second pass over one engine would not compare like with like) with
// the partner pre-recorded as just co-activated, and returns the probe row's
// reported hebbian_boost.
func (f *hebGateFixture) run(t *testing.T, hebbianEnabled bool) float64 {
	t.Helper()
	eng := newTestEngine(f.store, f.fts, &emptyHNSW{})
	t.Cleanup(eng.Close)

	// Deterministic seam: write the log entry directly instead of issuing a
	// prior Activate() and racing the drainLog goroutine.
	eng.AssocLog().Record(activation.LogEntry{
		VaultID:   1,
		At:        time.Now(),
		EngramIDs: []storage.ULID{f.parter},
		Scores:    []float64{1.0},
	})

	res, err := eng.Run(context.Background(), &activation.ActivateRequest{
		VaultID:        1,
		Context:        []string{"kiln firing schedule"},
		Threshold:      -1, // diagnostic bypass: score every candidate
		MaxResults:     10,
		HebbianEnabled: hebbianEnabled,
		Weights: &activation.Weights{
			SemanticSimilarity: 0.6,
			FullTextRelevance:  0.4,
			UseACTR:            true,
		},
	})
	if err != nil {
		t.Fatalf("Run(hebbianEnabled=%v): %v", hebbianEnabled, err)
	}
	for _, a := range res.Activations {
		if a.Engram.ID == f.probe {
			return a.Components.HebbianBoost
		}
	}
	t.Fatalf("Run(hebbianEnabled=%v): probe engram not in %d results", hebbianEnabled, len(res.Activations))
	return 0
}

// TestPhase4Hebbian_RespectsHebbianEnabled is the RED test for COG-32.
// hebbian_enabled:false must produce a zero read-side boost.
func TestPhase4Hebbian_RespectsHebbianEnabled(t *testing.T) {
	f := newHebGateFixture(t)
	if got := f.run(t, false); got != 0 {
		t.Errorf("HebbianEnabled=false: hebbian_boost = %v, want 0 — the read-side "+
			"boost must be gated symmetrically with learning and decay (COG-32)", got)
	}
}

// TestPhase4Hebbian_EnabledStillBoosts is the converse control: the gate must
// turn the boost OFF, not delete the mechanism. Without it, `return` at the top
// of phase4HebbianBoost would pass the test above.
func TestPhase4Hebbian_EnabledStillBoosts(t *testing.T) {
	f := newHebGateFixture(t)
	if got := f.run(t, true); got <= 0 {
		t.Errorf("HebbianEnabled=true: hebbian_boost = %v, want > 0", got)
	}
}
