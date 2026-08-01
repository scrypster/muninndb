package storage

import (
	"context"
	"fmt"
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
			if _, err := store.DecayAssocWeights(ctx, ws, time.Hour, 0.05, 0.0); err != nil {
				t.Fatalf("%s: DecayAssocWeights pass %d: %v", name, i, err)
			}
		}
		w, err := store.GetAssocWeight(ctx, ws, src, dst)
		if err != nil {
			t.Fatalf("%s: GetAssocWeight: %v", name, err)
		}
		return w
	}

	wA := run("fine", 60)  // 60 x 1-minute passes
	wB := run("coarse", 1) // 1 x 60-minute pass

	t.Logf("arm A (60 x 1min) = %v; arm B (1 x 60min) = %v", wA, wB)
	if math.Abs(float64(wA)-float64(wB)) > 1e-6 {
		t.Errorf("decay is cadence-dependent: 60 one-minute passes gave %v, one sixty-minute pass gave %v (delta %v, want < 1e-6)",
			wA, wB, math.Abs(float64(wA)-float64(wB)))
	}
}

// TestDecayAssoc_NeverRaisesWeight pins the min() in the mechanism.
//
// The ceiling is anchored to peakWeight, so an edge whose current weight sits
// BELOW its peak curve — the dream engine writes inferred weights directly, and
// every edge flattened by #762 is in this state — must be left where it is.
// Decay clamps downward or does nothing; it never resurrects.
func TestDecayAssoc_NeverRaisesWeight(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	ws := store.VaultPrefix("decay-never-raises")
	t0 := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)

	// Seed at 0.9 to earn the peak, then rewrite the weight down to 0.2 —
	// UpdateAssocWeight preserves peakWeight, so ceiling ≈ 0.9 >> 0.2.
	src, dst := seedDecayEdge(t, store, ws, 0.9, t0)
	if err := store.UpdateAssocWeight(ctx, ws, src, dst, 0.2, 0); err != nil {
		t.Fatalf("UpdateAssocWeight: %v", err)
	}

	store.decayNow = func() time.Time { return t0.Add(time.Minute) }
	if _, err := store.DecayAssocWeights(ctx, ws, 30*24*time.Hour, 0.05, 0.0); err != nil {
		t.Fatalf("DecayAssocWeights: %v", err)
	}

	w, err := store.GetAssocWeight(ctx, ws, src, dst)
	if err != nil {
		t.Fatalf("GetAssocWeight: %v", err)
	}
	if math.Abs(float64(w)-0.2) > 1e-6 {
		t.Errorf("decay raised a sub-peak weight: got %v, want 0.2 (peak 0.9, ceiling ~0.9)", w)
	}
}

// TestDecayAssoc_SubFloorStaleEdge_NotRaisedToFloor pins the refute finding
// that the floor-clamp branch could RAISE a weight.
//
// An edge whose stored weight sits BELOW its dynamic floor (peak*0.05) is
// reachable in production: the dream engine writes inferred weights through
// UpdateAssocWeight (internal/consolidation/transitive.go), which preserves
// the monotone peak — so peak 0.9 with weight 0.02 is a legal stored state.
// With a stale anchor the ceiling drops under the floor, the drop-guard
// passes (drop = 0.02 - ~0 > epsilon), newW lands under minWeight, and the
// floor branch assigned e.newW = 0.045 — decay RAISING 0.02 to 0.045. Decay
// must clamp downward or do nothing; it must never write an increase.
func TestDecayAssoc_SubFloorStaleEdge_NotRaisedToFloor(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	ws := store.VaultPrefix("decay-subfloor-stale")

	// Earn peak 0.9, then rewrite the weight down to 0.02 — below the dynamic
	// floor 0.9*0.05 = 0.045. UpdateAssocWeight stamps lastActivated = now.
	src, dst := seedDecayEdge(t, store, ws, 0.9, time.Now())
	if err := store.UpdateAssocWeight(ctx, ws, src, dst, 0.02, 0); err != nil {
		t.Fatalf("UpdateAssocWeight: %v", err)
	}

	// Stale anchor: evaluate 300 days after the stamp. ceiling ≈ 0.9*2^-10,
	// far under both the stored weight and the floor.
	store.decayNow = func() time.Time { return time.Now().Add(300 * 24 * time.Hour) }
	if _, err := store.DecayAssocWeights(ctx, ws, 30*24*time.Hour, 0.05, 0.0); err != nil {
		t.Fatalf("DecayAssocWeights: %v", err)
	}

	w, err := store.GetAssocWeight(ctx, ws, src, dst)
	if err != nil {
		t.Fatalf("GetAssocWeight: %v", err)
	}
	if float64(w) > 0.02+1e-6 {
		t.Errorf("decay RAISED a sub-floor weight: got %v, want 0.02 unchanged (dynamic floor 0.045 must not resurrect)", w)
	}
	if float64(w) < 0.02-1e-6 {
		t.Errorf("sub-floor weight changed: got %v, want 0.02 unchanged", w)
	}
}

// TestDecayAssoc_FlooredEdge_NoRewriteChurn pins the write-churn half of the
// same refute finding: an edge sitting AT its dynamic floor re-entered the
// chunk on every pass — 5 keys rewritten and one batch replicated every ~65 s,
// forever, per floored edge (the refuter measured 1429/2000 edges churning
// every pass on a simulated post-#762 vault). After the first clamp to the
// floor, subsequent passes must produce ZERO decay batches.
func TestDecayAssoc_FlooredEdge_NoRewriteChurn(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	ws := store.VaultPrefix("decay-floor-churn")
	t0 := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)

	src, dst := seedDecayEdge(t, store, ws, 0.8, t0) // dynamic floor = 0.04

	var batches int
	store.repLogAppend = func(op uint8, key, value []byte) error {
		batches++
		return nil
	}

	// Pass 1, 300 days stale: legitimately clamps to the floor — one batch.
	store.decayNow = func() time.Time { return t0.Add(300 * 24 * time.Hour) }
	if _, err := store.DecayAssocWeights(ctx, ws, 30*24*time.Hour, 0.05, 0.0); err != nil {
		t.Fatalf("DecayAssocWeights pass 1: %v", err)
	}
	if batches != 1 {
		t.Fatalf("floor clamp should commit exactly one batch: got %d", batches)
	}
	w, err := store.GetAssocWeight(ctx, ws, src, dst)
	if err != nil {
		t.Fatalf("GetAssocWeight: %v", err)
	}
	if math.Abs(float64(w)-0.04) > 1e-6 {
		t.Fatalf("pass 1 should clamp to the dynamic floor: got %v, want 0.04", w)
	}

	// Passes 2..21 at the real ~65 s cadence: the edge is already at its
	// floor, the ceiling stays below it, and nothing may be rewritten.
	batches = 0
	for i := 1; i <= 20; i++ {
		at := t0.Add(300*24*time.Hour + time.Duration(i)*65*time.Second)
		store.decayNow = func() time.Time { return at }
		if _, err := store.DecayAssocWeights(ctx, ws, 30*24*time.Hour, 0.05, 0.0); err != nil {
			t.Fatalf("DecayAssocWeights pass %d: %v", i+1, err)
		}
	}
	if batches != 0 {
		t.Errorf("floored edge rewritten in %d of 20 passes — 5 keys + a replicated batch per pass, forever, want 0", batches)
	}
	w, err = store.GetAssocWeight(ctx, ws, src, dst)
	if err != nil {
		t.Fatalf("GetAssocWeight after churn passes: %v", err)
	}
	if math.Abs(float64(w)-0.04) > 1e-6 {
		t.Errorf("floored edge moved during no-op passes: got %v, want 0.04", w)
	}
}

// TestDecayAssoc_LegacyZeroLastActivated_Adopted covers the guard that keeps
// the naive formula from destroying every legacy edge on the first pass.
//
// lastActivated == 0 means "unknown", not "1970". Read as an epoch stamp it
// would hand the formula ~56 years of elapsed time and floor the edge
// instantly. With neither lastActivated nor createdAt available the edge is
// adopted: stamped with now, weight untouched.
func TestDecayAssoc_LegacyZeroLastActivated_Adopted(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	ws := store.VaultPrefix("decay-legacy-adopt")
	now := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)

	src, dst := NewULID(), NewULID()
	if err := store.WriteAssociation(ctx, ws, src, dst, &Association{
		TargetID:   dst,
		Weight:     0.7,
		RelType:    RelSupports,
		Confidence: 1.0,
		// LastActivated and CreatedAt both intentionally zero: a pre-metadata edge.
	}); err != nil {
		t.Fatalf("WriteAssociation: %v", err)
	}

	store.decayNow = func() time.Time { return now }
	if _, err := store.DecayAssocWeights(ctx, ws, 30*24*time.Hour, 0.05, 0.0); err != nil {
		t.Fatalf("DecayAssocWeights: %v", err)
	}

	w, err := store.GetAssocWeight(ctx, ws, src, dst)
	if err != nil {
		t.Fatalf("GetAssocWeight: %v", err)
	}
	if math.Abs(float64(w)-0.7) > 1e-6 {
		t.Fatalf("legacy zero-lastActivated edge was decayed: got %v, want 0.7 (floor would be 0.035)", w)
	}

	_, _, _, lastActivated, _, _ := store.getAssocValue(ws, src, dst, w)
	if lastActivated != int32(now.Unix()) {
		t.Errorf("adopted edge was not stamped: lastActivated = %d, want %d", lastActivated, now.Unix())
	}
}

// TestDecayAssoc_HalfLifeMatchesTable pins the actual curve against the regime
// the 30-day default was chosen for: noticeable within days, halved in a month,
// floored within a quarter.
func TestDecayAssoc_HalfLifeMatchesTable(t *testing.T) {
	ctx := context.Background()
	const halfLife = 30 * 24 * time.Hour
	t0 := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		elapsedDays float64
		wantRatio   float64
	}{
		{1, 0.9772},
		{7, 0.8511},
		{30, 0.5000},
		{90, 0.1250},
		{129.7, 0.0500}, // the dynamic floor: peak * 0.05
		{365, 0.0500},   // clamped at the floor, never below
	}

	for _, tc := range cases {
		store := newTestStore(t)
		ws := store.VaultPrefix("decay-table")
		src, dst := seedDecayEdge(t, store, ws, 0.8, t0)

		store.decayNow = func() time.Time {
			return t0.Add(time.Duration(tc.elapsedDays * float64(24*time.Hour)))
		}
		if _, err := store.DecayAssocWeights(ctx, ws, halfLife, 0.05, 0.0); err != nil {
			t.Fatalf("%.1fd: DecayAssocWeights: %v", tc.elapsedDays, err)
		}
		w, err := store.GetAssocWeight(ctx, ws, src, dst)
		if err != nil {
			t.Fatalf("%.1fd: GetAssocWeight: %v", tc.elapsedDays, err)
		}
		gotRatio := float64(w) / 0.8
		if math.Abs(gotRatio-tc.wantRatio) > 1e-3 {
			t.Errorf("%.1f days elapsed: weight/peak = %.4f, want %.4f", tc.elapsedDays, gotRatio, tc.wantRatio)
		}
	}
}

// TestDecayAssoc_EpsilonSkipLagIsBounded proves the write-avoidance skip
// forgives no decay, and bounds what it does cost. The skip is NOT lossless —
// the stored weight can sit up to epsilon above the true ceiling — but the
// residual is a bounded lag, not accumulated error.
//
// The precise property — measured, not assumed. The design for #762 predicted
// the stepped and single-pass arms would agree to 1e-6; they do not, and the
// reason is worth writing down. A skipped pass leaves the STORED weight up to
// assocDecayWriteEpsilon above the true ceiling, so an arm that skips its last
// write reads back slightly high. What the absolute ceiling buys is that this
// residual is a bounded LAG, not accumulated ERROR: it does not grow with the
// number of skipped passes, because every pass recomputes from lastActivated
// rather than compounding off the stored weight. Under the old per-pass
// multiplier the same skip would have permanently forgiven a pass's worth of
// decay, 1329 times a day.
//
// So: gap <= epsilon after 24 h of 65-second passes, and STILL <= epsilon after
// 30 days of them — 40x the passes, same bound.
func TestDecayAssoc_EpsilonSkipLagIsBounded(t *testing.T) {
	ctx := context.Background()
	t0 := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	const halfLife = 30 * 24 * time.Hour

	// 65 s is the real prune cadence (60 s + up to 10 s jitter): ~1329
	// passes/day, nearly all of which the epsilon skip elides at H=30 d.
	run := func(name string, stride, horizon time.Duration) float32 {
		store := newTestStore(t)
		ws := store.VaultPrefix("decay-epsilon-" + name)
		src, dst := seedDecayEdge(t, store, ws, 0.8, t0)
		for at := stride; at <= horizon; at += stride {
			store.decayNow = func() time.Time { return t0.Add(at) }
			if _, err := store.DecayAssocWeights(ctx, ws, halfLife, 0.05, 0.0); err != nil {
				t.Fatalf("%s: DecayAssocWeights at %v: %v", name, at, err)
			}
		}
		w, err := store.GetAssocWeight(ctx, ws, src, dst)
		if err != nil {
			t.Fatalf("%s: GetAssocWeight: %v", name, err)
		}
		return w
	}

	for _, horizon := range []time.Duration{24 * time.Hour, 30 * 24 * time.Hour} {
		days := horizon.Hours() / 24
		stepped := run(fmt.Sprintf("stepped-%.0fd", days), 65*time.Second, horizon)
		single := run(fmt.Sprintf("single-%.0fd", days), horizon, horizon)
		gap := float64(stepped) - float64(single)
		passes := int(horizon / (65 * time.Second))

		t.Logf("%.0f days: %d stepped passes = %v; one pass = %v; gap = %.3g",
			days, passes, stepped, single, gap)

		// Never LOW: a skipped write can only leave the weight above the
		// ceiling, never below it. A negative gap means decay was applied twice.
		if gap < -1e-6 {
			t.Errorf("%.0fd: stepped arm decayed BELOW the single-pass ceiling by %.3g — decay is compounding", days, -gap)
		}
		// Never high by more than one epsilon, no matter how many passes.
		if gap > assocDecayWriteEpsilon {
			t.Errorf("%.0fd: epsilon skip forgave decay: gap %.3g after %d passes exceeds epsilon %g — error is accumulating, not lagging",
				days, gap, passes, assocDecayWriteEpsilon)
		}
		// And decay must actually be happening.
		want := 0.8 * math.Exp2(-days/30.0)
		if math.Abs(float64(single)-want) > 1e-4 {
			t.Errorf("%.0fd: single-pass decay wrong: got %v, want %v", days, single, want)
		}
	}
}

// TestDecayAssoc_FutureLastActivated_NeverBoosts covers backwards clock skew
// and future-dated stamps. Negative elapsed time makes 2^(-dt/H) > 1, i.e. a
// ceiling ABOVE the peak. Decay must neither boost the edge nor decay it.
//
// Note this is guarded twice over: explicitly by the elapsed<0 clamp, and
// structurally by the single drop-guard (a ceiling above the stored weight
// yields drop <= 0, which is never written). The clamp is documentation; the
// drop-guard is the invariant.
func TestDecayAssoc_FutureLastActivated_NeverBoosts(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	ws := store.VaultPrefix("decay-future-stamp")
	t0 := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

	// Edge stamped a year in the FUTURE relative to the decay clock.
	src, dst := seedDecayEdge(t, store, ws, 0.6, t0.Add(365*24*time.Hour))

	store.decayNow = func() time.Time { return t0 }
	if _, err := store.DecayAssocWeights(ctx, ws, 30*24*time.Hour, 0.05, 0.0); err != nil {
		t.Fatalf("DecayAssocWeights: %v", err)
	}
	w, err := store.GetAssocWeight(ctx, ws, src, dst)
	if err != nil {
		t.Fatalf("GetAssocWeight: %v", err)
	}
	if math.Abs(float64(w)-0.6) > 1e-6 {
		t.Errorf("future-stamped edge changed: got %v, want 0.6 unchanged", w)
	}
}
