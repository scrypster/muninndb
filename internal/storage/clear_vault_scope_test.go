package storage

import (
	"context"
	"testing"
	"time"
)

// TestClearVault_KeepsOtherVaultsAssociationCaches: the commit-time
// invalidation of a vault clear's range tombstones purged the forward and
// reverse association caches of EVERY vault, so clearing one vault cost every
// other vault a cold traversal. Both caches are keyed by (vault, engram), so
// a vault-scoped tombstone now drops only that vault's entries. The cleared
// vault's own entries must still go.
func TestClearVault_KeepsOtherVaultsAssociationCaches(t *testing.T) {
	ps := openTestStore(t)
	ctx := context.Background()
	kept := ps.VaultPrefix("ridge-trail")
	cleared := ps.VaultPrefix("valley-trail")

	pair := func(ws [8]byte) (ULID, ULID) {
		a := writeRMWFixture(t, ps, ws, "cairn at the saddle")
		b := writeRMWFixture(t, ps, ws, "spring below the saddle")
		if err := ps.WriteAssociation(ctx, ws, a, b, &Association{
			TargetID: b, RelType: RelSupports, Weight: 0.6, Confidence: 1, CreatedAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := ps.GetAssociations(ctx, ws, []ULID{a}, 10); err != nil {
			t.Fatal(err)
		}
		if _, err := ps.rankingReverseEdges(ctx, ws, []ULID{b}, 10); err != nil {
			t.Fatal(err)
		}
		return a, b
	}
	ka, kb := pair(kept)
	ca, cb := pair(cleared)
	for _, c := range []struct {
		name string
		ok   bool
	}{
		{"kept forward", ps.assocCache.Contains(assocCacheKey(kept, ka))},
		{"kept reverse", ps.revAssocCache.Contains(assocCacheKey(kept, kb))},
		{"cleared forward", ps.assocCache.Contains(assocCacheKey(cleared, ca))},
		{"cleared reverse", ps.revAssocCache.Contains(assocCacheKey(cleared, cb))},
	} {
		if !c.ok {
			t.Fatalf("fixture: %s association cache entry was never filled", c.name)
		}
	}

	if _, err := ps.ClearVault(ctx, cleared); err != nil {
		t.Fatal(err)
	}

	if !ps.assocCache.Contains(assocCacheKey(kept, ka)) {
		t.Error("clearing one vault evicted ANOTHER vault's forward association cache entry")
	}
	if !ps.revAssocCache.Contains(assocCacheKey(kept, kb)) {
		t.Error("clearing one vault evicted ANOTHER vault's reverse association cache entry")
	}
	if ps.assocCache.Contains(assocCacheKey(cleared, ca)) || ps.revAssocCache.Contains(assocCacheKey(cleared, cb)) {
		t.Error("the cleared vault's own association cache entries survived the clear")
	}
}
