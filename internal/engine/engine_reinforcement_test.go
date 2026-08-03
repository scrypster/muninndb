package engine

import (
	"context"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// pollAccessCount polls Engine.Read until AccessCount reaches at least want,
// or the timeout elapses, returning the last observed count. Reinforcement
// (#682) rides fire-and-forget goroutines off Engine.Read/RecordFeedback, so
// tests must poll rather than assert immediately after the call returns.
func pollAccessCount(t *testing.T, eng *Engine, vault, id string, want uint32, timeout time.Duration) uint32 {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last uint32
	for time.Now().Before(deadline) {
		// ReadOnly: true — polling itself must not reinforce, or it would
		// inflate the very count under test.
		resp, err := eng.Read(context.Background(), &mbp.ReadRequest{Vault: vault, ID: id, ReadOnly: true})
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		last = resp.AccessCount
		if last >= want {
			return last
		}
		time.Sleep(10 * time.Millisecond)
	}
	return last
}

// TestRead_ReinforcesAccess: remember -> engine.Read by id -> AccessCount
// 0->1, LastAccess advances, and its where_left_off position moves ahead of a
// later-written sibling (LastAccess index reordering after read).
func TestRead_ReinforcesAccess(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	// Explicit, distinct CreatedAt stamps (LastAccess defaults to CreatedAt
	// at write time — normalizeEngramTimes) instead of a time.Sleep between
	// the two writes to force clock-tick separation: #695 flagged the sleep
	// as itself fragile under coarse clock resolution / CI load, and it was
	// never addressed. This makes the "older"/"newer" ordering a property of
	// the data, not of wall-clock scheduling.
	base := time.Now()
	older, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault: "reinforce-read", Concept: "older", Content: "older sibling content",
		CreatedAt: timePtr(base.Add(-time.Second)),
	})
	if err != nil {
		t.Fatalf("Write older: %v", err)
	}
	newer, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault: "reinforce-read", Concept: "newer", Content: "newer sibling content",
		CreatedAt: timePtr(base),
	})
	if err != nil {
		t.Fatalf("Write newer: %v", err)
	}

	// Sanity: before reinforcing, "newer" is more recently accessed (it was
	// stamped after "older" and LastAccess defaults to write time).
	entries, err := eng.WhereLeftOff(ctx, "reinforce-read", 10, nil)
	if err != nil {
		t.Fatalf("WhereLeftOff (before): %v", err)
	}
	if len(entries) < 2 || entries[0].ID.String() != newer.ID {
		t.Fatalf("WhereLeftOff (before) = %v, want newer (%s) first", entries, newer.ID)
	}

	before, err := eng.Read(ctx, &mbp.ReadRequest{Vault: "reinforce-read", ID: older.ID})
	if err != nil {
		t.Fatalf("Read (before): %v", err)
	}
	if before.AccessCount != 0 {
		t.Fatalf("AccessCount before any reinforcement = %d, want 0", before.AccessCount)
	}

	// Read "older" again — this spawns the #682 reinforcement (TouchAccess)
	// as a fire-and-forget goroutine tracked by fireAndForgetWG. Drain it
	// deterministically (docs/internals/testing-hermeticity.md, source #2)
	// instead of polling against a wall-clock deadline — the same seam
	// (waitFireAndForgetIdle) every sibling test in this file already uses.
	if _, err := eng.Read(ctx, &mbp.ReadRequest{Vault: "reinforce-read", ID: older.ID}); err != nil {
		t.Fatalf("Read (reinforcing): %v", err)
	}
	eng.waitFireAndForgetIdle()

	after, err := eng.Read(ctx, &mbp.ReadRequest{Vault: "reinforce-read", ID: older.ID, ReadOnly: true})
	if err != nil {
		t.Fatalf("Read (after): %v", err)
	}
	if after.AccessCount == 0 {
		t.Fatalf("TouchAccess did not land after draining fire-and-forget goroutines (AccessCount still 0) — " +
			"a dropped reinforcement, not scheduling noise")
	}

	// LastAccess must now put "older" ahead of "newer" in WhereLeftOff — no
	// polling needed, the drain above already guarantees the 0x22 LastAccess
	// index write (synchronous inside TouchAccess) has landed.
	entries, err = eng.WhereLeftOff(ctx, "reinforce-read", 10, nil)
	if err != nil {
		t.Fatalf("WhereLeftOff (after): %v", err)
	}
	if len(entries) == 0 || entries[0].ID.String() != older.ID {
		t.Fatalf("WhereLeftOff did not reorder 'older' to the front after reinforcing its read: %v", entries)
	}
}

func timePtr(t time.Time) *time.Time { return &t }

// TestRead_ReadOnly_NoReinforce: engine.Read with observe-mode context (or
// req.ReadOnly) must NOT bump AccessCount.
func TestRead_ReadOnly_NoReinforce(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	w, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault: "reinforce-readonly", Concept: "ro", Content: "read-only content",
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	observeCtx := context.WithValue(ctx, auth.ContextMode, auth.ModeObserve)
	for i := 0; i < 5; i++ {
		if _, err := eng.Read(observeCtx, &mbp.ReadRequest{Vault: "reinforce-readonly", ID: w.ID}); err != nil {
			t.Fatalf("Read (observe) iter %d: %v", i, err)
		}
	}
	// Also exercise the explicit req.ReadOnly path under a full-mode context.
	for i := 0; i < 5; i++ {
		if _, err := eng.Read(ctx, &mbp.ReadRequest{Vault: "reinforce-readonly", ID: w.ID, ReadOnly: true}); err != nil {
			t.Fatalf("Read (req.ReadOnly) iter %d: %v", i, err)
		}
	}

	// Give any (incorrectly) spawned fire-and-forget goroutine time to land,
	// deterministically rather than via a fixed sleep (flakes under CPU
	// contention).
	eng.waitFireAndForgetIdle()

	resp, err := eng.Read(ctx, &mbp.ReadRequest{Vault: "reinforce-readonly", ID: w.ID, ReadOnly: true})
	if err != nil {
		t.Fatalf("final Read: %v", err)
	}
	if resp.AccessCount != 0 {
		t.Fatalf("AccessCount = %d after 10 read-only reads, want 0", resp.AccessCount)
	}
}

// TestFeedbackUseful_BumpsAccessCount: RecordFeedback(useful=true) bumps
// AccessCount; useful=false must not.
func TestFeedbackUseful_BumpsAccessCount(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	wTrue, err := eng.Write(ctx, &mbp.WriteRequest{Vault: "feedback-touch", Concept: "useful", Content: "useful content"})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := eng.RecordFeedback(ctx, "feedback-touch", wTrue.ID, true); err != nil {
		t.Fatalf("RecordFeedback(true): %v", err)
	}
	// RecordFeedback's TouchAccess write rides a fire-and-forget goroutine
	// (#682); await it deterministically instead of polling with a fixed
	// deadline, which flakes under CPU contention (constrained CI cores,
	// -race) when the goroutine isn't scheduled in time.
	eng.waitFireAndForgetIdle()
	got := pollAccessCount(t, eng, "feedback-touch", wTrue.ID, 1, 2*time.Second)
	if got != 1 {
		t.Fatalf("AccessCount after RecordFeedback(true) = %d, want 1", got)
	}

	wFalse, err := eng.Write(ctx, &mbp.WriteRequest{Vault: "feedback-touch", Concept: "not useful", Content: "not useful content"})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := eng.RecordFeedback(ctx, "feedback-touch", wFalse.ID, false); err != nil {
		t.Fatalf("RecordFeedback(false): %v", err)
	}
	// useful=false still spawns a fire-and-forget scoring-signal goroutine
	// (just no TouchAccess); await it deterministically rather than sleeping
	// a fixed duration before asserting the negative (AccessCount == 0).
	eng.waitFireAndForgetIdle()
	resp, err := eng.Read(ctx, &mbp.ReadRequest{Vault: "feedback-touch", ID: wFalse.ID, ReadOnly: true})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if resp.AccessCount != 0 {
		t.Fatalf("AccessCount after RecordFeedback(false) = %d, want 0", resp.AccessCount)
	}
}

// TestRecall_DoesNotReinforce pins COG-12: Activate/Recall never bumps
// AccessCount, no matter how many times a memory surfaces in results.
func TestRecall_DoesNotReinforce(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	w, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault: "recall-noreinforce", Concept: "recall target",
		Content: "distinctive recall reinforcement pin content",
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	for i := 0; i < 20; i++ {
		_, err := eng.Activate(ctx, &mbp.ActivateRequest{
			Vault: "recall-noreinforce", Context: []string{"distinctive recall reinforcement pin content"},
		})
		if err != nil {
			t.Fatalf("Activate iter %d: %v", i, err)
		}
	}

	// Deterministic wait for any (incorrectly) spawned fire-and-forget
	// reinforcement goroutine, instead of a fixed sleep (flakes under load).
	eng.waitFireAndForgetIdle()
	resp, err := eng.Read(ctx, &mbp.ReadRequest{Vault: "recall-noreinforce", ID: w.ID, ReadOnly: true})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if resp.AccessCount != 0 {
		t.Fatalf("AccessCount = %d after 20 recalls, want 0 (COG-12: recall never reinforces)", resp.AccessCount)
	}
}

// TestReinforce_DoesNotEscalateTrust reinforces a trust=inferred memory
// repeatedly via both channels (Read and RecordFeedback) and asserts trust
// never moves. Reinforcement touches only AccessCount/LastAccess.
func TestReinforce_DoesNotEscalateTrust(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	w, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault: "trust-noescalate", Concept: "trust pin", Content: "trust must not escalate via reinforcement",
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	// New engrams default to TrustInferred (0x02) / TrustUnset(0x00), both of
	// which display as "inferred". Confirm the starting point.
	initial, err := eng.GetEngram(ctx, "trust-noescalate", mustParseULID(t, w.ID))
	if err != nil {
		t.Fatalf("GetEngram (initial): %v", err)
	}
	if initial.Trust != storage.TrustUnset && initial.Trust != storage.TrustInferred {
		t.Fatalf("initial trust = %v, want TrustUnset or TrustInferred", initial.Trust)
	}
	startTrust := initial.Trust

	for i := 0; i < 10; i++ {
		if _, err := eng.Read(ctx, &mbp.ReadRequest{Vault: "trust-noescalate", ID: w.ID}); err != nil {
			t.Fatalf("Read iter %d: %v", i, err)
		}
		if err := eng.RecordFeedback(ctx, "trust-noescalate", w.ID, true); err != nil {
			t.Fatalf("RecordFeedback iter %d: %v", i, err)
		}
	}

	pollAccessCount(t, eng, "trust-noescalate", w.ID, 1, 2*time.Second)
	// pollAccessCount only guarantees the FIRST reinforcement landed; wait
	// deterministically for all 10 RecordFeedback fire-and-forget goroutines
	// to finish before asserting trust never moved, instead of a fixed sleep.
	eng.waitFireAndForgetIdle()

	final, err := eng.GetEngram(ctx, "trust-noescalate", mustParseULID(t, w.ID))
	if err != nil {
		t.Fatalf("GetEngram (final): %v", err)
	}
	if final.Trust != startTrust {
		t.Fatalf("trust escalated via reinforcement: before=%v after=%v", startTrust, final.Trust)
	}
}

func mustParseULID(t *testing.T, s string) storage.ULID {
	t.Helper()
	id, err := storage.ParseULID(s)
	if err != nil {
		t.Fatalf("ParseULID(%q): %v", s, err)
	}
	return id
}
