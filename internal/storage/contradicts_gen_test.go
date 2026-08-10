package storage

import (
	"context"
	"testing"
	"time"
)

// ContradictsWriteGen is the invalidation signal behind the COG-29 debt
// readout's scan cache. Its completeness is what keeps a declared conflict from
// being silently unreported, so it is pinned BEHAVIOURALLY — one arm per public
// path that can write an association value — rather than by trusting a list of
// call sites. All fixtures are invented (trail maintenance).

func newGenStore(t *testing.T) *PebbleStore {
	t.Helper()
	db, err := OpenPebble(t.TempDir(), DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	store := NewPebbleStore(db, PebbleStoreConfig{CacheSize: 128})
	t.Cleanup(func() { store.Close() })
	return store
}

func TestContradictsWriteGen_BumpsOnEveryAssociationWritePath(t *testing.T) {
	ctx := context.Background()

	t.Run("WriteAssociation", func(t *testing.T) {
		store := newGenStore(t)
		ws := store.VaultPrefix("gen-write-assoc")
		a, b := NewULID(), NewULID()
		seedEndpoints(t, store, ws, a, b)
		before := store.ContradictsWriteGen(ws)
		if err := store.WriteAssociation(ctx, ws, a, b, &Association{
			TargetID: b, RelType: RelContradicts, Weight: 0.8, Confidence: 1, CreatedAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		if got := store.ContradictsWriteGen(ws); got == before {
			t.Errorf("WriteAssociation(contradicts) did not bump the generation (%d) — a debt cache would never see this declaration", got)
		}
	})

	t.Run("batch WriteAssociation", func(t *testing.T) {
		store := newGenStore(t)
		ws := store.VaultPrefix("gen-batch-assoc")
		a, b := NewULID(), NewULID()
		seedEndpoints(t, store, ws, a, b)
		before := store.ContradictsWriteGen(ws)
		batch := store.NewBatch()
		if err := batch.WriteAssociation(ctx, ws, a, b, &Association{
			TargetID: b, RelType: RelContradicts, Weight: 0.8, Confidence: 1, CreatedAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		if err := batch.Commit(); err != nil {
			t.Fatal(err)
		}
		if got := store.ContradictsWriteGen(ws); got == before {
			t.Errorf("batch WriteAssociation(contradicts) did not bump the generation (%d)", got)
		}
	})

	t.Run("inline associations on WriteEngram", func(t *testing.T) {
		store := newGenStore(t)
		ws := store.VaultPrefix("gen-inline-assoc")
		target, err := store.WriteEngram(ctx, ws, &Engram{
			Concept: "waterbar spacing", Content: "waterbars sit 8 metres apart", Confidence: 1, Stability: 30})
		if err != nil {
			t.Fatal(err)
		}
		before := store.ContradictsWriteGen(ws)
		if _, err := store.WriteEngram(ctx, ws, &Engram{
			Concept: "waterbar spacing revised", Content: "waterbars sit 12 metres apart",
			Confidence: 1, Stability: 30,
			Associations: []Association{{TargetID: target, RelType: RelContradicts, Weight: 0.8, Confidence: 1, CreatedAt: time.Now()}},
		}); err != nil {
			t.Fatal(err)
		}
		if got := store.ContradictsWriteGen(ws); got == before {
			t.Errorf("an inline contradicts association on WriteEngram did not bump the generation (%d)", got)
		}
	})
}

// TestContradictsWriteGen_IgnoresOtherRelations is the other half, and it is the
// reason the counter is usable at all: the Hebbian co-activation worker writes
// RelCoActivated on a hot path, so a counter that bumped on ANY association
// write would invalidate the debt cache on essentially every recall and the
// cache would never hold.
func TestContradictsWriteGen_IgnoresOtherRelations(t *testing.T) {
	ctx := context.Background()
	store := newGenStore(t)
	ws := store.VaultPrefix("gen-other-relations")
	a, b, c := NewULID(), NewULID(), NewULID()
	seedEndpoints(t, store, ws, a, b, c)

	before := store.ContradictsWriteGen(ws)
	for _, rel := range []RelType{RelSupports, RelCoActivated, RelRelatesTo, RelSupersedes} {
		dst := NewULID()
		seedEndpoints(t, store, ws, dst)
		if err := store.WriteAssociation(ctx, ws, a, dst, &Association{
			TargetID: dst, RelType: rel, Weight: 0.5, Confidence: 1, CreatedAt: time.Now(),
		}); err != nil {
			t.Fatalf("rel %v: %v", rel, err)
		}
	}
	if got := store.ContradictsWriteGen(ws); got != before {
		t.Errorf("non-contradicts association writes bumped the generation (%d -> %d) — the debt scan cache would be invalidated by ordinary Hebbian churn", before, got)
	}
}

// TestContradictsWriteGen_IsPerVault — one vault's declaration must not
// invalidate another's cached scan.
func TestContradictsWriteGen_IsPerVault(t *testing.T) {
	ctx := context.Background()
	store := newGenStore(t)
	wsA := store.VaultPrefix("gen-vault-one")
	wsB := store.VaultPrefix("gen-vault-two")
	a, b := NewULID(), NewULID()
	seedEndpoints(t, store, wsA, a, b)

	beforeB := store.ContradictsWriteGen(wsB)
	if err := store.WriteAssociation(ctx, wsA, a, b, &Association{
		TargetID: b, RelType: RelContradicts, Weight: 0.8, Confidence: 1, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if store.ContradictsWriteGen(wsA) == 0 {
		t.Error("the declaring vault's generation did not move")
	}
	if got := store.ContradictsWriteGen(wsB); got != beforeB {
		t.Errorf("an unrelated vault's generation moved (%d -> %d)", beforeB, got)
	}
}
