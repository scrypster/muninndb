package storage

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestWriteAssociation_RelTypeCollisionRefused is the #771 reproduction: two
// link() calls between the same (src, dst) pair at the same weight collide on
// the same 0x03/0x04 key (RelType lives only in the value), so the second
// write silently REPLACES the first RelType instead of coexisting with it.
// The "honest first increment" (#771) is a write-path guard that refuses the
// second write instead of destroying the first declaration silently.
func TestWriteAssociation_RelTypeCollisionRefused(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("reltype-collision")

	src, dst := NewULID(), NewULID()
	seedEndpoints(t, store, ws, src, dst)

	now := time.Now()
	contradicts := &Association{
		TargetID: dst, RelType: RelContradicts, Weight: 1.0,
		Confidence: 1.0, CreatedAt: now,
	}
	if err := store.WriteAssociation(ctx, ws, src, dst, contradicts); err != nil {
		t.Fatalf("write contradicts: %v", err)
	}

	supersedes := &Association{
		TargetID: dst, RelType: RelSupersedes, Weight: 1.0, // SAME weight
		Confidence: 1.0, CreatedAt: now,
	}
	err := store.WriteAssociation(ctx, ws, src, dst, supersedes)
	if err == nil {
		t.Fatal("WriteAssociation: want an error refusing the collision, got nil — " +
			"the contradicts edge was silently replaced by supersedes (#771)")
	}
	if !errors.Is(err, ErrAssocRelTypeCollision) {
		t.Fatalf("WriteAssociation err = %v, want ErrAssocRelTypeCollision", err)
	}

	// The original declaration must survive untouched.
	assocs, gerr := store.GetAssociations(ctx, ws, []ULID{src}, 0)
	if gerr != nil {
		t.Fatalf("GetAssociations: %v", gerr)
	}
	got := assocs[src]
	if len(got) != 1 || got[0].RelType != RelContradicts {
		t.Fatalf("edges after refused collision: got %+v, want exactly one RelContradicts edge", got)
	}
}

// TestWriteAssociation_SameRelTypeSameWeightIsIdempotent: re-writing the SAME
// RelType at the SAME weight (e.g. to bump confidence) is not a collision and
// must still succeed.
func TestWriteAssociation_SameRelTypeSameWeightIsIdempotent(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("reltype-idempotent")

	src, dst := NewULID(), NewULID()
	seedEndpoints(t, store, ws, src, dst)

	now := time.Now()
	first := &Association{TargetID: dst, RelType: RelSupports, Weight: 0.7, Confidence: 0.5, CreatedAt: now}
	if err := store.WriteAssociation(ctx, ws, src, dst, first); err != nil {
		t.Fatalf("first write: %v", err)
	}

	second := &Association{TargetID: dst, RelType: RelSupports, Weight: 0.7, Confidence: 0.9, CreatedAt: now}
	if err := store.WriteAssociation(ctx, ws, src, dst, second); err != nil {
		t.Fatalf("same-RelType re-write at the same weight must not be refused: %v", err)
	}
}

// TestWriteAssociation_DifferentWeightNoCollision: the classic case of two
// distinct RelTypes at DIFFERENT weights never collides — this is what the
// #764 conflict-corpus tests already rely on (their own comment names this as
// the workaround for #771).
func TestWriteAssociation_DifferentWeightNoCollision(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("reltype-distinct-weight")

	src, dst := NewULID(), NewULID()
	seedEndpoints(t, store, ws, src, dst)

	now := time.Now()
	contradicts := &Association{TargetID: dst, RelType: RelContradicts, Weight: 1.0, Confidence: 1.0, CreatedAt: now}
	if err := store.WriteAssociation(ctx, ws, src, dst, contradicts); err != nil {
		t.Fatalf("write contradicts: %v", err)
	}
	supersedes := &Association{TargetID: dst, RelType: RelSupersedes, Weight: 0.9, Confidence: 1.0, CreatedAt: now}
	if err := store.WriteAssociation(ctx, ws, src, dst, supersedes); err != nil {
		t.Fatalf("distinct-weight write must not be refused: %v", err)
	}
}
