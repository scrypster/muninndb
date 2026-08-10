package storage

import (
	"context"
	"fmt"
	"testing"
)

// TestGetAssociations_VaultPrefixEndingIn0xFF is the forward-side equivalent of
// TestGetRankingNeighbors_VaultPrefixEndingIn0xFF that #819 asked for.
//
// Honest labelling: this arm passes both before and after #819. The old
// open-coded carry loop in AssocFwdRangeEnd handled the ~1-in-256
// 0xFF-terminated vault prefix correctly — it stopped at index 1, which is one
// index too early only for an ALL-0xFF prefix. It is here as a REGRESSION pin
// on the delegation, so that swapping the hand-rolled loop for
// keys.PrefixUpperBound is shown not to have broken the case the reverse side
// is pinned for. The arm that actually fails without the fix is
// TestGetAssociations_AllFFWorkspacePrefixIsNotSilentlyEmpty below.
func TestGetAssociations_VaultPrefixEndingIn0xFF(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Derive a 0xFF-terminated vault prefix deterministically rather than
	// hoping one turns up (~1 name in 256).
	var vault string
	var ws [8]byte
	found := false
	for i := 0; i < 100000; i++ {
		name := fmt.Sprintf("sto11-fwd-probe-%d", i)
		p := store.VaultPrefix(name)
		if p[7] == 0xFF {
			vault, ws, found = name, p, true
			break
		}
	}
	if !found {
		t.Skip("no 0xFF-terminated vault prefix found in the probe space")
	}
	t.Logf("vault %q has prefix ending in 0xFF", vault)

	src, dst := NewULID(), NewULID()
	mustWriteAssoc(t, store, ws, src, dst, 0.8, RelRelatesTo)

	got, err := store.GetAssociations(ctx, ws, []ULID{src}, 10)
	if err != nil {
		t.Fatalf("GetAssociations: %v", err)
	}
	if !containsTarget(got[src], dst) {
		t.Fatalf("STO-11: forward scan returned nothing for a 0xFF-terminated vault prefix (got %v)", targetsOf(got[src]))
	}
}

// TestGetAssociations_AllFFWorkspacePrefixIsNotSilentlyEmpty is the #819 RED
// case at the behavioural level.
//
// AssocFwdRangeEnd's open-coded carry loop stopped at index 1 so it could never
// touch the 0x03 type byte. For an all-0xFF workspace prefix every workspace
// byte wrapped to 0x00 and the loop ran off the end, producing 0x03|00..00 — an
// upper bound BELOW the lower bound. Pebble returns nothing for an inverted
// range, so every forward association in that vault became invisible: silently,
// permanently, and only for that vault.
//
// The workspace prefix is supplied directly rather than derived from a vault
// name because SipHash will not produce an all-0xFF prefix in any reachable
// amount of probing (2^-64). That is the honest severity: this is keyspace
// hygiene — one bound rule instead of two — not a reachable defect.
func TestGetAssociations_AllFFWorkspacePrefixIsNotSilentlyEmpty(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	ws := [8]byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}

	src, dst := NewULID(), NewULID()
	mustWriteAssoc(t, store, ws, src, dst, 0.8, RelRelatesTo)

	got, err := store.GetAssociations(ctx, ws, []ULID{src}, 10)
	if err != nil {
		t.Fatalf("GetAssociations: %v", err)
	}
	if !containsTarget(got[src], dst) {
		t.Fatalf("STO-11: forward scan returned nothing for an all-0xFF workspace prefix — "+
			"AssocFwdRangeEnd produced an upper bound below the lower bound (got %v)", targetsOf(got[src]))
	}
}
