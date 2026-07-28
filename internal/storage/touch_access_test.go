package storage

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestTouchAccess_ConcurrentWithCAS races PebbleStore.TouchAccess against
// CompareAndSet (active→completed) on the same engram id, under -race. Both
// take the same per-engram stripe lock (casLocks.For(id)) that
// CompareAndSet/DeleteEngram/UpdateConfidence already use, so they must
// serialize: whichever runs second observes the first's committed state.
//
// The invariant under test: after both goroutines return, the CAS's state
// transition (active→completed) MUST have applied (TouchAccess must not
// silently undo it via a stale-state UpdateMetadata write), and TouchAccess's
// AccessCount bump must not be lost (no lost update between the two RMWs).
func TestTouchAccess_ConcurrentWithCAS(t *testing.T) {
	store, cleanup := newTestStoreHelper(t)
	defer cleanup()
	ctx := context.Background()
	ws := store.VaultPrefix("touch-cas")

	for i := 0; i < 200; i++ {
		id := writeLeaseTestEngram(t, store, ws)

		active := StateActive
		completed := StateCompleted

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = store.CompareAndSet(ctx, ws, id, CASCondition{State: &active}, CASMutation{State: &completed})
		}()
		go func() {
			defer wg.Done()
			_ = store.TouchAccess(ctx, ws, id)
		}()
		wg.Wait()

		eng, err := store.GetEngram(ctx, ws, id)
		if err != nil {
			t.Fatalf("iter %d: GetEngram: %v", i, err)
		}
		if eng.State != StateCompleted {
			t.Fatalf("iter %d: final state = %v, want StateCompleted (%v); TouchAccess raced/clobbered the CAS transition",
				i, eng.State, StateCompleted)
		}
		if eng.AccessCount == 0 {
			t.Fatalf("iter %d: AccessCount = 0, want > 0; TouchAccess's bump was lost", i)
		}
	}
}

// TestTouchAccess_PreservesOtherFields verifies TouchAccess bumps ONLY
// AccessCount, LastAccess, and (when the 1/day gate allows) Stability —
// Confidence/Relevance/State/Trust/Importance must be read fresh under the
// lock and passed through unchanged. Pins the "never escalates
// trust/confidence via reinforcement" invariant (COG-10) at the storage layer.
// Here the engram was written moments ago (LastAccess < 24h), so the stability
// gain is also skipped — Stability must be bit-identical.
func TestTouchAccess_PreservesOtherFields(t *testing.T) {
	store, cleanup := newTestStoreHelper(t)
	defer cleanup()
	ctx := context.Background()
	ws := store.VaultPrefix("touch-preserve")
	id := writeLeaseTestEngram(t, store, ws)

	if err := store.UpdateConfidence(ctx, ws, id, 0.42); err != nil {
		t.Fatalf("UpdateConfidence: %v", err)
	}

	before, err := store.GetEngram(ctx, ws, id)
	if err != nil {
		t.Fatalf("GetEngram before: %v", err)
	}

	time.Sleep(time.Millisecond)
	if err := store.TouchAccess(ctx, ws, id); err != nil {
		t.Fatalf("TouchAccess: %v", err)
	}

	after, err := store.GetEngram(ctx, ws, id)
	if err != nil {
		t.Fatalf("GetEngram after: %v", err)
	}

	if after.AccessCount != before.AccessCount+1 {
		t.Errorf("AccessCount = %d, want %d", after.AccessCount, before.AccessCount+1)
	}
	if !after.LastAccess.After(before.LastAccess) {
		t.Errorf("LastAccess did not advance: before=%v after=%v", before.LastAccess, after.LastAccess)
	}
	if after.Confidence != before.Confidence {
		t.Errorf("Confidence changed: before=%v after=%v (TouchAccess must not touch confidence)", before.Confidence, after.Confidence)
	}
	if after.State != before.State {
		t.Errorf("State changed: before=%v after=%v (TouchAccess must not touch state)", before.State, after.State)
	}
	if after.Trust != before.Trust {
		t.Errorf("Trust changed: before=%v after=%v (TouchAccess must not touch trust)", before.Trust, after.Trust)
	}
	// LastAccess < 24h (just written): the 1/day gate must skip the stability gain.
	if after.Stability != before.Stability {
		t.Errorf("Stability changed on a <24h re-touch: before=%v after=%v (1/day gain cap violated)", before.Stability, after.Stability)
	}
	if after.Importance != before.Importance {
		t.Errorf("Importance changed: before=%v after=%v (TouchAccess must never move importance — COG-10)", before.Importance, after.Importance)
	}
}

// writeAgedEngram stores an engram whose CreatedAt/LastAccess are `age` in the
// past with the given prior access count and importance, bypassing WriteEngram's
// "LastAccess defaults to CreatedAt=now" freshness.
func writeAgedEngram(t *testing.T, store *PebbleStore, ws [8]byte, age time.Duration, accessCount uint32, importance float32) ULID {
	t.Helper()
	past := time.Now().Add(-age)
	id, err := store.WriteEngram(context.Background(), ws, &Engram{
		Concept:     "aged",
		Content:     "aged content " + past.String(),
		CreatedAt:   past,
		UpdatedAt:   past,
		LastAccess:  past,
		AccessCount: accessCount,
		Importance:  importance,
	})
	if err != nil {
		t.Fatalf("WriteEngram: %v", err)
	}
	return id
}

// TestTouchStabilityGain_InverseInRetrievalStrength pins the desirable-
// difficulty shape of the raw gain function: a cold memory (old, rarely
// accessed → low B(M)) earns close to the 7-day max, a hot memory (frequently
// accessed → high B(M)) earns little, and the gain is monotone and bounded.
func TestTouchStabilityGain_InverseInRetrievalStrength(t *testing.T) {
	cold := touchStabilityGain(0, 60) // never accessed, 60 days dormant
	hot := touchStabilityGain(50, 1)  // 50 accesses, touched yesterday

	if cold <= hot {
		t.Fatalf("inverted reinforcement broken: cold gain %v <= hot gain %v", cold, hot)
	}
	if cold < 6.0 || cold >= touchGainMaxDays {
		t.Errorf("cold gain = %v, want in [6.0, %v) (design: hard retrieval earns ~+6.9d)", cold, touchGainMaxDays)
	}
	if hot > 1.5 {
		t.Errorf("hot gain = %v, want <= 1.5 (design: hot-set re-touch earns ~+1.2d or less)", hot)
	}
	// Monotone in access count at fixed age.
	prev := touchStabilityGain(0, 30)
	for _, n := range []uint32{1, 2, 5, 10, 50, 500} {
		g := touchStabilityGain(n, 30)
		if g > prev {
			t.Errorf("gain not monotone: n=%d gain %v > previous %v", n, g, prev)
		}
		prev = g
	}
	// Bounded (0, max].
	if g := touchStabilityGain(0, 100000); g <= 0 || g > touchGainMaxDays {
		t.Errorf("gain out of bounds: %v", g)
	}
}

// TestTouchAccess_ColdGainsMoreDurabilityThanHot is the end-to-end pin for
// inverted reinforcement (finding #7, rich-get-richer): touching a cold,
// rarely-used engram must add MORE Stability than touching a hot one.
func TestTouchAccess_ColdGainsMoreDurabilityThanHot(t *testing.T) {
	store, cleanup := newTestStoreHelper(t)
	defer cleanup()
	ctx := context.Background()
	ws := store.VaultPrefix("touch-inverted")

	coldID := writeAgedEngram(t, store, ws, 60*24*time.Hour, 0, 0) // dormant 60d, never accessed
	hotID := writeAgedEngram(t, store, ws, 30*time.Hour, 50, 0)    // heavily used, last touch 30h ago (past the 1/day gate)

	coldBefore, _ := store.GetEngram(ctx, ws, coldID)
	hotBefore, _ := store.GetEngram(ctx, ws, hotID)

	if err := store.TouchAccess(ctx, ws, coldID); err != nil {
		t.Fatalf("TouchAccess cold: %v", err)
	}
	if err := store.TouchAccess(ctx, ws, hotID); err != nil {
		t.Fatalf("TouchAccess hot: %v", err)
	}

	coldAfter, _ := store.GetEngram(ctx, ws, coldID)
	hotAfter, _ := store.GetEngram(ctx, ws, hotID)

	coldGain := coldAfter.Stability - coldBefore.Stability
	hotGain := hotAfter.Stability - hotBefore.Stability
	if coldGain <= 0 {
		t.Fatalf("cold engram earned no stability: gain=%v", coldGain)
	}
	if hotGain < 0 {
		t.Fatalf("hot engram lost stability: gain=%v", hotGain)
	}
	if coldGain <= hotGain {
		t.Errorf("rich-get-richer not inverted: cold gain %v <= hot gain %v", coldGain, hotGain)
	}
}

// TestTouchAccess_GainRateLimitedOncePerDay verifies the second touch within
// 24h bumps AccessCount but NOT Stability (skip the gain, never the count).
func TestTouchAccess_GainRateLimitedOncePerDay(t *testing.T) {
	store, cleanup := newTestStoreHelper(t)
	defer cleanup()
	ctx := context.Background()
	ws := store.VaultPrefix("touch-ratelimit")

	id := writeAgedEngram(t, store, ws, 48*time.Hour, 0, 0)

	if err := store.TouchAccess(ctx, ws, id); err != nil { // qualifies: 48h dormant
		t.Fatalf("TouchAccess #1: %v", err)
	}
	first, _ := store.GetEngram(ctx, ws, id)
	if err := store.TouchAccess(ctx, ws, id); err != nil { // within 24h of #1
		t.Fatalf("TouchAccess #2: %v", err)
	}
	second, _ := store.GetEngram(ctx, ws, id)

	if second.AccessCount != first.AccessCount+1 {
		t.Errorf("AccessCount = %d, want %d (the count is never rate-limited)", second.AccessCount, first.AccessCount+1)
	}
	if second.Stability != first.Stability {
		t.Errorf("Stability moved on a same-day re-touch: %v -> %v (gain must be 1/day)", first.Stability, second.Stability)
	}
}

// TestTouchAccess_ImportanceBoostsGainAndCaps verifies the (1 + 0.5*imp)
// importance multiplier on the gain and the 365-day Stability ceiling.
func TestTouchAccess_ImportanceBoostsGainAndCaps(t *testing.T) {
	store, cleanup := newTestStoreHelper(t)
	defer cleanup()
	ctx := context.Background()
	ws := store.VaultPrefix("touch-imp")

	// Same coldness, different explicit importance.
	plainID := writeAgedEngram(t, store, ws, 60*24*time.Hour, 0, 0.01)
	impID := writeAgedEngram(t, store, ws, 60*24*time.Hour, 0, 1.0)

	plainBefore, _ := store.GetEngram(ctx, ws, plainID)
	impBefore, _ := store.GetEngram(ctx, ws, impID)
	if err := store.TouchAccess(ctx, ws, plainID); err != nil {
		t.Fatal(err)
	}
	if err := store.TouchAccess(ctx, ws, impID); err != nil {
		t.Fatal(err)
	}
	plainAfter, _ := store.GetEngram(ctx, ws, plainID)
	impAfter, _ := store.GetEngram(ctx, ws, impID)

	plainGain := float64(plainAfter.Stability - plainBefore.Stability)
	impGain := float64(impAfter.Stability - impBefore.Stability)
	// imp=1.0 → ×1.5; imp=0.01 → ×1.005. Ratio ≈ 1.49.
	if impGain <= plainGain*1.3 {
		t.Errorf("importance multiplier missing: impGain=%v plainGain=%v (want ~1.5x)", impGain, plainGain)
	}

	// Ceiling: an engram already at 364 days must clamp to 365, not overshoot.
	nearCapID := writeAgedEngram(t, store, ws, 60*24*time.Hour, 0, 1.0)
	// Raise its stability near the cap via UpdateRelevance (relevance unchanged).
	nc, _ := store.GetEngram(ctx, ws, nearCapID)
	if err := store.UpdateRelevance(ctx, ws, nearCapID, nc.Relevance, 364); err != nil {
		t.Fatalf("UpdateRelevance: %v", err)
	}
	if err := store.TouchAccess(ctx, ws, nearCapID); err != nil {
		t.Fatal(err)
	}
	capped, _ := store.GetEngram(ctx, ws, nearCapID)
	if capped.Stability > float32(maxStabilityDays) {
		t.Errorf("Stability %v exceeds the %v-day cap", capped.Stability, maxStabilityDays)
	}
}

// TestTouchAccess_ConcurrentWithDelete races TouchAccess against DeleteEngram
// on the same id under -race. TouchAccess holds the same stripe lock and drops
// the L1 cache before its authoritative read, so it must never resurrect a
// deleted engram's keys (#594 class): after both return, the engram must be
// gone.
func TestTouchAccess_ConcurrentWithDelete(t *testing.T) {
	store, cleanup := newTestStoreHelper(t)
	defer cleanup()
	ctx := context.Background()
	ws := store.VaultPrefix("touch-delete")

	for i := 0; i < 200; i++ {
		id := writeAgedEngram(t, store, ws, 48*time.Hour, 0, 0.5)

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = store.DeleteEngram(ctx, ws, id)
		}()
		go func() {
			defer wg.Done()
			_ = store.TouchAccess(ctx, ws, id)
		}()
		wg.Wait()

		if _, err := store.GetEngram(ctx, ws, id); err == nil {
			t.Fatalf("iter %d: engram still readable after DeleteEngram — TouchAccess resurrected it", i)
		}
	}
}
