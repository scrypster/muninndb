package storage

import (
	"context"
	"math"
	"testing"
	"time"
)

// seedDecayEdge writes a single edge with a known weight/peak and an explicit
// lastActivated stamp, and returns the pair. Synthetic IDs and vault names only.
func seedDecayEdge(t *testing.T, store *PebbleStore, ws [8]byte, weight float32, lastActivated time.Time) (ULID, ULID) {
	t.Helper()
	src, dst := NewULID(), NewULID()
	var la int32
	if !lastActivated.IsZero() {
		la = int32(lastActivated.Unix())
	}
	if err := store.WriteAssociation(context.Background(), ws, src, dst, &Association{
		TargetID:      dst,
		Weight:        weight,
		RelType:       RelSupports,
		Confidence:    1.0,
		LastActivated: la,
	}); err != nil {
		t.Fatalf("WriteAssociation: %v", err)
	}
	return src, dst
}

// TestDecayAssoc_CadenceIndependent is the RED pin for #762.
//
// Association decay must be a function of elapsed wall-clock time since an
// edge's last activation, not of how many times the prune worker happened to
// tick. Two arms cover the SAME simulated hour:
//
//	arm A — 60 decay evaluations, one per simulated minute
//	arm B —  1 decay evaluation, at +60 simulated minutes
//
// Under a cadence-independent mechanism these are bit-identical. Under the
// pre-fix per-pass multiplier they diverge by ~17x, which is exactly the
// mechanism by which #762 ground every edge in the store to the 0.05 floor:
// the 60s prune cadence is a hidden multiplier on the configured rate.
func TestDecayAssoc_CadenceIndependent(t *testing.T) {
	ctx := context.Background()
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	run := func(name string, steps int) float32 {
		store := newTestStore(t)
		ws := store.VaultPrefix("decay-cadence-" + name)
		src, dst := seedDecayEdge(t, store, ws, 0.8, t0)

		stride := time.Hour / time.Duration(steps)
		for i := 1; i <= steps; i++ {
			store.decayNow = func() time.Time { return t0.Add(time.Duration(i) * stride) }
			if _, err := store.DecayAssocWeights(ctx, ws, 0.95, 0.05, 0.0); err != nil {
				t.Fatalf("%s: DecayAssocWeights pass %d: %v", name, i, err)
			}
		}
		w, err := store.GetAssocWeight(ctx, ws, src, dst)
		if err != nil {
			t.Fatalf("%s: GetAssocWeight: %v", name, err)
		}
		return w
	}

	wA := run("fine", 60) // 60 x 1-minute passes
	wB := run("coarse", 1) // 1 x 60-minute pass

	t.Logf("arm A (60 x 1min) = %v; arm B (1 x 60min) = %v", wA, wB)
	if math.Abs(float64(wA)-float64(wB)) > 1e-6 {
		t.Errorf("decay is cadence-dependent: 60 one-minute passes gave %v, one sixty-minute pass gave %v (delta %v, want < 1e-6)",
			wA, wB, math.Abs(float64(wA)-float64(wB)))
	}
}
