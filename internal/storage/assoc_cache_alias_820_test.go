package storage

import (
	"context"
	"testing"
)

// ---------------------------------------------------------------------------
// #820 — GetAssociations' own doc comment states the rule ("Return copies of
// cached slices to prevent callers from mutating the cache") and the cache-HIT
// path honours it. The cache-MISS path handed back the very slice it had just
// stored, so the first caller after a miss owned a live view of the cache entry
// for the rest of the 2s TTL.
//
// The identical defect on the reverse cache was fixed at the source in #800
// (TestRankingReverseEdges_MissPathDoesNotAliasTheCache); this is its forward
// twin, plus the under-serve trap the same ticket names.
// ---------------------------------------------------------------------------

// TestGetAssociations_MissPathDoesNotAliasTheCache mutates what the miss path
// returned and then reads through the cache. Nothing in the tree mutates a
// returned association today, which is exactly why this is worth pinning: the
// ownership contract is a property of today's callers, not of the function.
func TestGetAssociations_MissPathDoesNotAliasTheCache(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("assoc-miss-alias")

	src, dst := NewULID(), NewULID()
	mustWriteAssoc(t, store, ws, src, dst, 0.7, RelCoActivated)

	fresh := newFreshStore(t, store.db)

	miss, err := fresh.GetAssociations(ctx, ws, []ULID{src}, 10)
	if err != nil {
		t.Fatalf("cold GetAssociations: %v", err)
	}
	if len(miss[src]) != 1 {
		t.Fatalf("fixture broken: %d edges on the miss path, want 1", len(miss[src]))
	}

	// A caller doing what the returned value's type allows.
	miss[src][0].Weight = 42.0
	miss[src][0].TargetID = NewULID()

	hit, err := fresh.GetAssociations(ctx, ws, []ULID{src}, 10)
	if err != nil {
		t.Fatalf("warm GetAssociations: %v", err)
	}
	if len(hit[src]) != 1 {
		t.Fatalf("warm read returned %d edges, want 1", len(hit[src]))
	}
	if hit[src][0].Weight == 42.0 || hit[src][0].TargetID != dst {
		t.Errorf("the miss path published the cache's backing array: warm read returns target=%v weight=%v, want target=%v weight=0.7",
			hit[src][0].TargetID, hit[src][0].Weight, dst)
	}
}

// TestGetAssociations_CacheDoesNotUnderServeALargerCap is the trap the same
// ticket names: the forward cache stored the list it built UNDER the caller's
// maxPerNode with no record that it was truncated, so a later caller asking for
// MORE was silently served the shorter list. The reverse cache already records
// `truncated` and re-scans (TestGetRankingNeighbors_CacheDoesNotUnderServeALargerCap).
func TestGetAssociations_CacheDoesNotUnderServeALargerCap(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("assoc-underserve")

	src := NewULID()
	for _, w := range []float32{0.9, 0.6, 0.3} {
		mustWriteAssoc(t, store, ws, src, NewULID(), w, RelCoActivated)
	}

	fresh := newFreshStore(t, store.db)

	small, err := fresh.GetAssociations(ctx, ws, []ULID{src}, 1)
	if err != nil {
		t.Fatalf("capped GetAssociations: %v", err)
	}
	if len(small[src]) != 1 {
		t.Fatalf("fixture broken: cap 1 returned %d edges", len(small[src]))
	}

	large, err := fresh.GetAssociations(ctx, ws, []ULID{src}, 10)
	if err != nil {
		t.Fatalf("uncapped GetAssociations: %v", err)
	}
	if len(large[src]) != 3 {
		t.Errorf("a warm entry cached under a SMALLER cap under-served a larger one: got %d edges, want 3", len(large[src]))
	}

	// maxPerNode <= 0 means uncapped, and must not be served from a capped entry either.
	unbounded, err := fresh.GetAssociations(ctx, ws, []ULID{src}, 0)
	if err != nil {
		t.Fatalf("uncapped GetAssociations: %v", err)
	}
	if len(unbounded[src]) != 3 {
		t.Errorf("an uncapped read was served a capped cache entry: got %d edges, want 3", len(unbounded[src]))
	}
}

// TestAssociationsForOne_CacheDoesNotUnderServeALargerCap pins the same
// property on the single-source reader, which shares assocCache with
// GetAssociations — so either one can plant the short entry the other serves.
func TestAssociationsForOne_CacheDoesNotUnderServeALargerCap(t *testing.T) {
	store := newTestStore(t)
	ws := store.VaultPrefix("assoc-one-underserve")

	src := NewULID()
	for _, w := range []float32{0.9, 0.6, 0.3} {
		mustWriteAssoc(t, store, ws, src, NewULID(), w, RelCoActivated)
	}

	fresh := newFreshStore(t, store.db)

	small, err := fresh.associationsForOne(ws, src, 1)
	if err != nil {
		t.Fatalf("capped associationsForOne: %v", err)
	}
	if len(small) != 1 {
		t.Fatalf("fixture broken: cap 1 returned %d edges", len(small))
	}

	large, err := fresh.associationsForOne(ws, src, 10)
	if err != nil {
		t.Fatalf("uncapped associationsForOne: %v", err)
	}
	if len(large) != 3 {
		t.Errorf("a warm entry cached under a SMALLER cap under-served a larger one: got %d edges, want 3", len(large))
	}
}
