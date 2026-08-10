package storage

import (
	"context"
	"testing"
)

// ---------------------------------------------------------------------------
// COG-31 follow-up: UpdateAssocWeightBatch must evict the FORWARD and the
// REVERSE association cache independently.
//
// The batch invalidator originally used ONE dedup set for BOTH caches:
//
//	seen := make(map[[24]byte]struct{}, len(updates))
//	... if !seen[key(Src)] { seen[...]; assocCache.Remove(...) }
//	... if !seen[key(Dst)] { seen[...]; revAssocCache.Remove(...) }
//
// so an engram appearing in BOTH roles inside one batch had its second
// eviction suppressed: whichever cache was reached second for that engram kept
// serving pre-batch weights for the rest of the 2s TTL.
//
// This is not a hypothetical shape. HebbianWorker.processBatch emits every
// C(n,2) pair of a co-activated set through canonicalPair, so for any three
// co-activated engrams X<Y<Z one batch carries (X,Y) and (Y,Z) — Y is a Dst in
// the first and a Src in the second. Every recall returning >=3 results
// produces it.
//
// The forward half is the sharper finding: GetAssociations is untouched by the
// COG-31 increment and was CORRECT before it, so a shared `seen` set made an
// unrelated read path stale. Both directions are pinned below.
// ---------------------------------------------------------------------------

// batchCacheFixture writes a->b and b->c at seedWeight and returns the ids.
// RelCoActivated is used because it is BidirectionalForRanking, so the reverse
// edge is visible to GetRankingNeighbors — and it is what the Hebbian worker
// actually writes.
func batchCacheFixture(t *testing.T, store *PebbleStore, ws [8]byte, seedWeight float32) (a, b, c ULID) {
	t.Helper()
	a, b, c = NewULID(), NewULID(), NewULID()
	mustWriteAssoc(t, store, ws, a, b, seedWeight, RelCoActivated)
	mustWriteAssoc(t, store, ws, b, c, seedWeight, RelCoActivated)
	return a, b, c
}

func weightOfTarget(t *testing.T, assocs []Association, target ULID) float32 {
	t.Helper()
	for _, as := range assocs {
		if as.TargetID == target {
			return as.Weight
		}
	}
	t.Fatalf("target %s not present among %v", target, targetsOf(assocs))
	return 0
}

// TestUpdateAssocWeightBatch_ForwardCacheEvictedWhenEngramIsAlsoDst pins the
// regression against base: B is the Dst of update 0 and the Src of update 1,
// and GetAssociations(B) — a path this increment does not otherwise touch —
// must not keep serving the pre-batch weight.
//
// The update ORDER is load-bearing and deterministic: `updates` is a slice, so
// (a->b) is processed first and claims key(B) for the reverse cache; with one
// shared dedup set the forward eviction for key(B) in update 1 is skipped.
func TestUpdateAssocWeightBatch_ForwardCacheEvictedWhenEngramIsAlsoDst(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("batch-evict-fwd")

	a, b, c := batchCacheFixture(t, store, ws, 0.2)

	// Warm the FORWARD cache for B (holds b->c at 0.2).
	if got, err := store.GetAssociations(ctx, ws, []ULID{b}, 10); err != nil {
		t.Fatalf("warm GetAssociations: %v", err)
	} else if w := weightOfTarget(t, got[b], c); w != 0.2 {
		t.Fatalf("fixture: b->c warm weight = %v, want 0.2", w)
	}

	if err := store.UpdateAssocWeightBatch(ctx, []AssocWeightUpdate{
		{WS: ws, Src: a, Dst: b, Weight: 0.9}, // claims key(B) on the REVERSE cache
		{WS: ws, Src: b, Dst: c, Weight: 0.9}, // needs key(B) on the FORWARD cache
	}); err != nil {
		t.Fatalf("UpdateAssocWeightBatch: %v", err)
	}

	// On-disk truth, read through a path that consults neither cache.
	if w, err := store.GetAssocWeight(ctx, ws, b, c); err != nil || w != 0.9 {
		t.Fatalf("on-disk b->c = %v (err %v), want 0.9 — fixture broken, not a cache bug", w, err)
	}

	got, err := store.GetAssociations(ctx, ws, []ULID{b}, 10)
	if err != nil {
		t.Fatalf("GetAssociations: %v", err)
	}
	if w := weightOfTarget(t, got[b], c); w != 0.9 {
		t.Fatalf("STALE FORWARD CACHE: GetAssociations(B) reports b->c weight %v, on-disk is 0.90. "+
			"assocCache[B] was not evicted because the reverse eviction for (a->b) already "+
			"claimed key(B) in a shared dedup set.", w)
	}
}

// TestUpdateAssocWeightBatch_ReverseCacheEvictedWhenEngramIsAlsoSrc is the
// mirror: the same batch with the two updates in the other order suppresses the
// REVERSE eviction for B instead, so GetRankingNeighbors(B) keeps reporting the
// inbound a->b edge at its pre-batch weight.
//
// Which half is suppressed depends only on the order the pairs arrive in, and
// the Hebbian worker's pair order is not fixed — so in production both halves
// occur.
func TestUpdateAssocWeightBatch_ReverseCacheEvictedWhenEngramIsAlsoSrc(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("batch-evict-rev")

	a, b, c := batchCacheFixture(t, store, ws, 0.2)

	// Warm the REVERSE cache for B (holds the inbound a->b edge at 0.2).
	if got, err := store.GetRankingNeighbors(ctx, ws, []ULID{b}, 10); err != nil {
		t.Fatalf("warm GetRankingNeighbors: %v", err)
	} else if w := weightOfTarget(t, got[b], a); w != 0.2 {
		t.Fatalf("fixture: inbound a->b warm weight = %v, want 0.2", w)
	}

	if err := store.UpdateAssocWeightBatch(ctx, []AssocWeightUpdate{
		{WS: ws, Src: b, Dst: c, Weight: 0.9}, // claims key(B) on the FORWARD cache
		{WS: ws, Src: a, Dst: b, Weight: 0.9}, // needs key(B) on the REVERSE cache
	}); err != nil {
		t.Fatalf("UpdateAssocWeightBatch: %v", err)
	}

	if w, err := store.GetAssocWeight(ctx, ws, a, b); err != nil || w != 0.9 {
		t.Fatalf("on-disk a->b = %v (err %v), want 0.9 — fixture broken, not a cache bug", w, err)
	}

	got, err := store.GetRankingNeighbors(ctx, ws, []ULID{b}, 10)
	if err != nil {
		t.Fatalf("GetRankingNeighbors: %v", err)
	}
	if w := weightOfTarget(t, got[b], a); w != 0.9 {
		t.Fatalf("STALE REVERSE CACHE: GetRankingNeighbors(B) reports the inbound a->b edge at "+
			"weight %v, on-disk is 0.90. revAssocCache[B] was not evicted because the forward "+
			"eviction for (b->c) already claimed key(B) in a shared dedup set.", w)
	}
}
