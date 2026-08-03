package storage

import (
	"context"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// #818 — three edge-mutating sites evicted neither association cache, so an
// edge could be invisible (or visible at a stale weight) from one endpoint for
// up to the 2s TTL. Same observable shape as COG-31, at 2 seconds instead of
// forever.
//
// Each test warms the cache it is about, mutates through the site under test,
// and reads back through the SAME store — the eviction is the only mechanism
// that can make the read correct, because the TTL has not expired.
// ---------------------------------------------------------------------------

// TestDeleteEngram_ReverseCacheInvalidated: DeleteEngram evicts assocCache for
// every source that named the dead engram (#803), but never touched
// revAssocCache — so the dead engram stayed reachable INTO its former targets.
func TestDeleteEngram_ReverseCacheInvalidated(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("delete-evicts-reverse")

	a, b := NewULID(), NewULID()
	mustWriteAssoc(t, store, ws, a, b, 0.7, RelCoActivated)

	// Warm the REVERSE cache for B: it holds the inbound edge from A.
	warm, err := store.GetRankingNeighbors(ctx, ws, []ULID{b}, 10)
	if err != nil {
		t.Fatalf("warm GetRankingNeighbors: %v", err)
	}
	if !containsTarget(warm[b], a) {
		t.Fatalf("fixture broken: reverse edge a->b not visible from b: %v", targetsOf(warm[b]))
	}

	if err := store.DeleteEngram(ctx, ws, a); err != nil {
		t.Fatalf("DeleteEngram: %v", err)
	}

	got, err := store.GetRankingNeighbors(ctx, ws, []ULID{b}, 10)
	if err != nil {
		t.Fatalf("GetRankingNeighbors: %v", err)
	}
	if containsTarget(got[b], a) {
		t.Error("revAssocCache still names the hard-deleted engram: traversal from b hops to an id that can never materialise")
	}
}

// TestDecayAssocWeights_BothCachesInvalidated: decay rewrites every edge it
// touches at a new weight and archives others, and evicted neither cache — so
// ranking kept scoring on pre-decay weights for the TTL.
func TestDecayAssocWeights_BothCachesInvalidated(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("decay-evicts-caches")

	a, b := NewULID(), NewULID()
	seedEndpoints(t, store, ws, a, b)

	// Anchor the edge explicitly: with LastActivated == 0 and a zero CreatedAt
	// the first decay pass only ADOPTS the edge (stamps the anchor, leaves the
	// weight), which would make this test assert nothing.
	base := time.Unix(1750000000, 0)
	if err := store.WriteAssociation(ctx, ws, a, b, &Association{
		TargetID:      b,
		Weight:        0.8,
		PeakWeight:    0.8,
		RelType:       RelCoActivated,
		CreatedAt:     base,
		LastActivated: int32(base.Unix()),
	}); err != nil {
		t.Fatalf("WriteAssociation: %v", err)
	}

	// Warm FORWARD (keyed on src=a) and REVERSE (keyed on dst=b).
	if got, err := store.GetAssociations(ctx, ws, []ULID{a}, 10); err != nil {
		t.Fatalf("warm GetAssociations: %v", err)
	} else if w := weightOfTarget(t, got[a], b); w != 0.8 {
		t.Fatalf("fixture broken: warm forward weight %v, want 0.8", w)
	}
	if got, err := store.GetRankingNeighbors(ctx, ws, []ULID{b}, 10); err != nil {
		t.Fatalf("warm GetRankingNeighbors: %v", err)
	} else if w := weightOfTarget(t, got[b], a); w != 0.8 {
		t.Fatalf("fixture broken: warm reverse weight %v, want 0.8", w)
	}

	// Two half-lives of simulated elapsed time: ceiling = 0.8 * 2^-2 = 0.2.
	// Injected clock, not a sleep — decay is a pure function of elapsed time.
	halfLife := time.Hour
	store.decayNow = func() time.Time { return base.Add(2 * halfLife) }
	if _, err := store.DecayAssocWeights(ctx, ws, halfLife, 0.05, 0); err != nil {
		t.Fatalf("DecayAssocWeights: %v", err)
	}
	store.decayNow = nil

	// On-disk truth, read through neither cache.
	onDisk, err := store.GetAssocWeight(ctx, ws, a, b)
	if err != nil {
		t.Fatalf("GetAssocWeight: %v", err)
	}
	if onDisk > 0.25 || onDisk < 0.15 {
		t.Fatalf("fixture broken: decay did not move the on-disk weight (%v), so neither cache can be stale", onDisk)
	}

	got, err := store.GetAssociations(ctx, ws, []ULID{a}, 10)
	if err != nil {
		t.Fatalf("GetAssociations: %v", err)
	}
	if w := weightOfTarget(t, got[a], b); w != onDisk {
		t.Errorf("forward cache served a pre-decay weight: got %v, want %v", w, onDisk)
	}

	rev, err := store.GetRankingNeighbors(ctx, ws, []ULID{b}, 10)
	if err != nil {
		t.Fatalf("GetRankingNeighbors: %v", err)
	}
	if w := weightOfTarget(t, rev[b], a); w != onDisk {
		t.Errorf("reverse cache served a pre-decay weight: got %v, want %v", w, onDisk)
	}
}

// TestBatchWriteAssociation_BothCachesInvalidated: the batch writer sets 0x03
// and 0x04 and evicted neither cache, so an edge written through a batch was
// invisible from BOTH endpoints for the TTL. Eviction must be deferred to
// Commit — a discarded batch must evict nothing.
func TestBatchWriteAssociation_BothCachesInvalidated(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("batch-write-evicts")

	a, b := NewULID(), NewULID()
	seedEndpoints(t, store, ws, a, b)

	// Warm both caches with the pre-write (empty) truth.
	if got, err := store.GetAssociations(ctx, ws, []ULID{a}, 10); err != nil {
		t.Fatalf("warm GetAssociations: %v", err)
	} else if len(got[a]) != 0 {
		t.Fatalf("fixture broken: a already has %d edges", len(got[a]))
	}
	if got, err := store.GetRankingNeighbors(ctx, ws, []ULID{b}, 10); err != nil {
		t.Fatalf("warm GetRankingNeighbors: %v", err)
	} else if len(got[b]) != 0 {
		t.Fatalf("fixture broken: b already has %d neighbors", len(got[b]))
	}

	batch := store.NewBatch()
	if err := batch.WriteAssociation(ctx, ws, a, b, &Association{
		TargetID: b, Weight: 0.6, RelType: RelCoActivated,
	}); err != nil {
		t.Fatalf("batch WriteAssociation: %v", err)
	}
	if err := batch.Commit(); err != nil {
		t.Fatalf("batch Commit: %v", err)
	}

	fwd, err := store.GetAssociations(ctx, ws, []ULID{a}, 10)
	if err != nil {
		t.Fatalf("GetAssociations: %v", err)
	}
	if !containsTarget(fwd[a], b) {
		t.Error("forward cache still says a has no edges after a committed batch write")
	}

	rev, err := store.GetRankingNeighbors(ctx, ws, []ULID{b}, 10)
	if err != nil {
		t.Fatalf("GetRankingNeighbors: %v", err)
	}
	if !containsTarget(rev[b], a) {
		t.Error("reverse cache still says b has no inbound edges after a committed batch write")
	}
}

// TestBatchWriteAssociation_DiscardEvictsNothing is the other half of the
// deferred-eviction contract: a batch that never lands must not invalidate a
// correct cache entry. Cheap to get wrong by evicting at queue time.
func TestBatchWriteAssociation_DiscardEvictsNothing(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("batch-discard-evicts")

	a, b := NewULID(), NewULID()
	seedEndpoints(t, store, ws, a, b)

	if _, err := store.GetAssociations(ctx, ws, []ULID{a}, 10); err != nil {
		t.Fatalf("warm GetAssociations: %v", err)
	}
	if _, cached := store.assocCache.Get(assocCacheKey(ws, a)); !cached {
		t.Fatal("fixture broken: the warming read cached nothing")
	}

	batch := store.NewBatch()
	if err := batch.WriteAssociation(ctx, ws, a, b, &Association{
		TargetID: b, Weight: 0.6, RelType: RelCoActivated,
	}); err != nil {
		t.Fatalf("batch WriteAssociation: %v", err)
	}
	batch.Discard()

	if _, cached := store.assocCache.Get(assocCacheKey(ws, a)); !cached {
		t.Error("a discarded batch evicted the forward cache — eviction must be deferred to Commit")
	}
}
