//go:build localassets

package storage

import (
	"context"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// The replay's forgetting half, pinned at the storage layer.
//
// The cognition trial reconstructs the association graph by replaying recorded
// co-activations at their recorded times, INTERLEAVED with decay passes at
// simulated times. Two properties have to hold for that reconstruction to be
// the counterfactual it claims to be, and neither is obvious:
//
//  1. The replayed stamp must actually reach decay. Decay computes
//     elapsed = now - lastActivated (COG-27). If the write stamped time.Now()
//     — the pre-#779 behaviour — elapsed is ~0 for every replayed edge, no
//     forgetting is ever applied, and the result is a "no forgetting ever"
//     graph that never existed. This test drives 12 simulated weeks and
//     requires that the edge actually decays.
//
//  2. COG-27's never-raise guard must survive the new field. A decay pass may
//     only ever clamp downward. Asserted across every pass, not just the last.
//
// PRIVACY: synthetic IDs only.
// ---------------------------------------------------------------------------

func TestCognitionTrial_ReplayedStampsDriveDecayAndNeverRaise(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("replay-decay-vault")
	src, dst := NewULID(), NewULID()

	// t0 is fixed, not time.Now(): the whole point is that nothing here depends
	// on the wall clock.
	t0 := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	const halfLife = 30 * 24 * time.Hour

	seedEndpoints(t, store, ws, src, dst)
	if err := store.WriteAssociation(ctx, ws, src, dst, &Association{
		TargetID: dst, Weight: 0.9, Confidence: 1.0, CreatedAt: t0,
	}); err != nil {
		t.Fatalf("WriteAssociation: %v", err)
	}

	// The replayed co-activation: learned at t0, stamped at t0.
	if err := store.UpdateAssocWeightBatch(ctx, []AssocWeightUpdate{{
		WS: ws, Src: src, Dst: dst, Weight: 0.9, CountDelta: 1,
		LastActivatedAt: int32(t0.Unix()),
	}}); err != nil {
		t.Fatalf("UpdateAssocWeightBatch: %v", err)
	}

	read := func() float32 {
		t.Helper()
		w, err := store.GetAssocWeight(ctx, ws, src, dst)
		if err != nil {
			t.Fatalf("GetAssocWeight: %v", err)
		}
		return w
	}

	start := read()
	if start <= 0 {
		t.Fatalf("edge not written (weight %v) — the fixture proves nothing", start)
	}

	prev := start
	for week := 1; week <= 12; week++ {
		at := t0.Add(time.Duration(week) * 7 * 24 * time.Hour)
		store.decayNow = func() time.Time { return at }
		if _, err := store.DecayAssocWeights(ctx, ws, halfLife, 0.01, 0); err != nil {
			t.Fatalf("DecayAssocWeights(week %d): %v", week, err)
		}
		got := read()
		if got > prev {
			t.Fatalf("week %d: decay RAISED the weight %.9f -> %.9f — COG-27's "+
				"never-raise guard must survive the replayed stamp", week, prev, got)
		}
		prev = got
	}

	if prev >= start {
		t.Errorf("after 12 simulated weeks at a 30-day half-life the weight is still "+
			"%.9f (started %.9f) — the replayed lastActivated did not reach decay, so "+
			"the reconstruction would replay the learning and none of the forgetting",
			prev, start)
	}
}
