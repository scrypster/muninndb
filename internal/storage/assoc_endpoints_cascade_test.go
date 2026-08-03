package storage

import (
	"context"
	"encoding/binary"
	"math"
	"testing"
	"time"

	"github.com/cockroachdb/pebble"

	"github.com/scrypster/muninndb/internal/storage/keys"
)

// assocRowsFor returns the labels of the three live association keys that still
// exist for the (src, dst) pair at the given weight.
func assocRowsFor(t *testing.T, ps *PebbleStore, ws [8]byte, src, dst ULID, w float32) []string {
	t.Helper()
	var survivors []string
	for _, probe := range []struct {
		label string
		key   []byte
	}{
		{"0x03 fwd", keys.AssocFwdKey(ws, [16]byte(src), w, [16]byte(dst))},
		{"0x04 rev", keys.AssocRevKey(ws, [16]byte(dst), w, [16]byte(src))},
		{"0x14 idx", keys.AssocWeightIndexKey(ws, [16]byte(src), [16]byte(dst))},
	} {
		if _, closer, err := ps.db.Get(probe.key); err == nil {
			_ = closer.Close()
			survivors = append(survivors, probe.label)
		}
	}
	return survivors
}

// TestSTO12_DeleteEngramCascadeReachesEveryWeightPosition pins STO-11 inside
// DeleteEngram's 0x03/0x04 cascade.
//
// A 0x03 key is prefix(25) | weightComplement(4) | dst(16), and
// keys.WeightComplement is MaxUint32 - uint32(w*MaxUint32) — so complement[0]
// is 0xFF for EVERY weight at or below ~1/256, and is 0xFFFFFFFF exactly for
// weight 0 (which is also the byte position a pre-fix weight-1.0 edge was
// written at, see legacyFullWeightComplement). A naive `append(prefix, 0xFF)`
// upper bound is 26 bytes and sorts at or below every one of those keys, so the
// cascade never sees them: the edge outlives its hard-deleted endpoint forever,
// because DecayAssocWeights never reads 0x01 either.
//
// Nothing clamps Weight on the way in — Engine.Link and the inline-association
// loop pass the client's float straight through — so `weight: 0.001` plus a
// later hard delete is an ordinary client-reachable sequence.
func TestSTO12_DeleteEngramCascadeReachesEveryWeightPosition(t *testing.T) {
	ctx := context.Background()

	weights := []struct {
		name string
		w    float32
	}{
		// Control: complement[0] == 0x33, comfortably below the naive bound.
		{"ordinary weight 0.8", 0.8},
		// complement 0xFFBE76C8 — first byte 0xFF.
		{"sub-1/256 weight 0.001", 0.001},
		// complement 0xFFFFFFFF — the top of the keyspace, and byte-for-byte
		// the legacy full-weight position.
		{"weight 0.0 (the legacy full-weight byte position)", 0},
	}

	for _, tc := range weights {
		t.Run(tc.name+"/target hard-deleted", func(t *testing.T) {
			ps := newTestStore(t)
			ws := ps.VaultPrefix("sto12-cascade-dst")
			src, dst := NewULID(), NewULID()
			seedEndpoints(t, ps, ws, src, dst)

			a := danglingProbeAssoc(dst)
			a.Weight = tc.w
			if err := ps.WriteAssociation(ctx, ws, src, dst, a); err != nil {
				t.Fatalf("WriteAssociation: %v", err)
			}
			if got := assocRowsFor(t, ps, ps.VaultPrefix("sto12-cascade-dst"), src, dst, tc.w); len(got) != 3 {
				t.Fatalf("precondition: expected 3 rows, got %v", got)
			}
			if err := ps.DeleteEngram(ctx, ws, dst); err != nil {
				t.Fatalf("DeleteEngram: %v", err)
			}
			if got := assocRowsFor(t, ps, ws, src, dst, tc.w); len(got) > 0 {
				t.Errorf("STO-12: %v survived the hard delete of the TARGET at weight %v", got, tc.w)
			}
		})

		t.Run(tc.name+"/source hard-deleted", func(t *testing.T) {
			ps := newTestStore(t)
			ws := ps.VaultPrefix("sto12-cascade-src")
			src, dst := NewULID(), NewULID()
			seedEndpoints(t, ps, ws, src, dst)

			a := danglingProbeAssoc(dst)
			a.Weight = tc.w
			if err := ps.WriteAssociation(ctx, ws, src, dst, a); err != nil {
				t.Fatalf("WriteAssociation: %v", err)
			}
			if err := ps.DeleteEngram(ctx, ws, src); err != nil {
				t.Fatalf("DeleteEngram: %v", err)
			}
			if got := assocRowsFor(t, ps, ws, src, dst, tc.w); len(got) > 0 {
				t.Errorf("STO-12: %v survived the hard delete of the SOURCE at weight %v", got, tc.w)
			}
		})
	}
}

// seedLegacyFullWeightPair writes the three keys a PRE-FIX weight-1.0 edge left
// on disk: 0x03 and 0x04 at complement 0xFFFFFFFF, and a 0x14 index holding a
// true 1.0. That disagreement is exactly what RepairLegacyFullWeightAssocKeys
// scans for.
func seedLegacyFullWeightPair(t *testing.T, ps *PebbleStore, ws [8]byte, src, dst ULID) {
	t.Helper()
	val := encodeAssocValue(RelRelatesTo, 1, time.Now(), int32(time.Now().Unix()), 1.0, 1)
	var idxBuf [4]byte
	binary.BigEndian.PutUint32(idxBuf[:], math.Float32bits(1.0))
	b := ps.db.NewBatch()
	defer b.Close()
	_ = b.Set(legacyFullWeightFwdKey(ws, [16]byte(src), [16]byte(dst)), val[:], nil)
	_ = b.Set(legacyFullWeightRevKey(ws, [16]byte(dst), [16]byte(src)), val[:], nil)
	_ = b.Set(keys.AssocWeightIndexKey(ws, [16]byte(src), [16]byte(dst)), idxBuf[:], nil)
	if err := b.Commit(pebble.NoSync); err != nil {
		t.Fatalf("seed legacy pair: %v", err)
	}
}

// TestSTO12_LegacyFullWeightRepairNeverCreatesADanglingEdge pins the two ways
// the pre-fix full-weight repair pass could turn a stranded legacy row into a
// live, correctly-positioned, fully-reachable dangling edge.
//
// The repair scans with PrefixIterator (a correct carry-propagating bound), so
// it sees rows the cascade misses. That makes it a CREATOR of dangling edges
// whenever a legacy row survives its endpoint — the exact combination this
// branch's own writer census originally exempted it on.
func TestSTO12_LegacyFullWeightRepairNeverCreatesADanglingEdge(t *testing.T) {
	ctx := context.Background()

	t.Run("DeleteEngram removes the legacy row, so the repair never sees it", func(t *testing.T) {
		ps := newTestStore(t)
		ws := ps.VaultPrefix("sto12-legacy-cascade")
		src, dst := NewULID(), NewULID()
		seedEndpoints(t, ps, ws, src, dst)
		seedLegacyFullWeightPair(t, ps, ws, src, dst)

		if err := ps.DeleteEngram(ctx, ws, dst); err != nil {
			t.Fatalf("DeleteEngram: %v", err)
		}
		if _, closer, err := ps.db.Get(legacyFullWeightFwdKey(ws, [16]byte(src), [16]byte(dst))); err == nil {
			_ = closer.Close()
			t.Error("the legacy full-weight 0x03 row survived the hard delete of its target")
		}

		n, err := ps.RepairLegacyFullWeightAssocKeys(ctx, ws)
		if err != nil {
			t.Fatalf("repair: %v", err)
		}
		if n != 0 {
			t.Errorf("the repair relocated %d pair(s) whose target has no engram", n)
		}
		if _, closer, err := ps.db.Get(keys.AssocFwdKey(ws, [16]byte(src), 1.0, [16]byte(dst))); err == nil {
			_ = closer.Close()
			t.Error("the repair CREATED a live 0x03 row at the true 1.0 position pointing at a hard-deleted engram")
		}
	})

	t.Run("a legacy row stranded by a pre-fix delete is skipped, not relocated", func(t *testing.T) {
		ps := newTestStore(t)
		ws := ps.VaultPrefix("sto12-legacy-stranded")
		src, dst := NewULID(), NewULID()
		seedEndpoints(t, ps, ws, src, dst)
		seedLegacyFullWeightPair(t, ps, ws, src, dst)

		// A vault hard-deleted by a PRE-FIX binary: the 0x01 record is gone and
		// the legacy association rows were left behind. This is the shape the
		// repair pass exists to meet, so it is the shape it must not amplify.
		if err := ps.db.Delete(keys.EngramKey(ws, [16]byte(dst)), pebble.NoSync); err != nil {
			t.Fatalf("strand the legacy row: %v", err)
		}

		n, err := ps.RepairLegacyFullWeightAssocKeys(ctx, ws)
		if err != nil {
			t.Fatalf("repair: %v", err)
		}
		if n != 0 {
			t.Errorf("the repair relocated %d pair(s) whose target has no engram", n)
		}
		if _, closer, err := ps.db.Get(keys.AssocFwdKey(ws, [16]byte(src), 1.0, [16]byte(dst))); err == nil {
			_ = closer.Close()
			t.Error("the repair relocated a stranded legacy row into a live, correctly-positioned dangling 0x03 edge")
		}
		if _, closer, err := ps.db.Get(keys.AssocRevKey(ws, [16]byte(dst), 1.0, [16]byte(src))); err == nil {
			_ = closer.Close()
			t.Error("the repair relocated a stranded legacy row into a live, correctly-positioned dangling 0x04 edge")
		}
	})
}

// TestSTO12_DeleteEngramCascadeStaysInsideItsOwnPrefix pins the OTHER side of
// the cascade's scan bounds, and the reason the explicit prefix guard in both
// loops is load-bearing rather than belt-and-braces.
//
// keys.PrefixUpperBound is LOOSE: it increments the first sub-0xFF byte from the
// right and returns without zeroing the trailing 0xFF bytes, so for a prefix
// ending in 0xFF — about 1 engram ID in 256 — the bound it returns spans into
// the NEXT engram's association keyspace. (Tracked as #816; it is mandated by
// STO-11 and has several callers, so it is not changed here.)
//
// About-1-in-256 is the rate at which the BOUND IS LOOSE, and not the rate at
// which anything is lost. Landing a second engram inside the widened band takes
// a shared first 14 ID bytes — the full 48-bit ULID millisecond AND 8 of the 10
// crypto-random entropy bytes, ~2^-64 on top of a same-millisecond collision —
// which is why the IDs below are CONSTRUCTED and not hoped for. So the guard is
// structural hygiene rather than a live data-loss fix: the shared helper's
// contract is wrong, each call site compensates per key, and any future
// non-ULID ID tail closes that 64-bit gap immediately, on a delete path.
//
// Both loops therefore keep SeekGE plus an explicit bytes.Equal(k[:25], prefix)
// break — which is also why they must NOT be converted to PrefixIterator, whose
// First/Valid shape changes the break-vs-continue semantics on short keys.
// TestSTO11_EveryDestructivePrefixScanStaysInsideItsOwnPrefix is the table over
// all four destructive scans of this shape; this test keeps the cascade's own
// end-to-end form.
//
// The victim's ID ends in 0xFF by construction, never by hoping a random ULID
// lands there, and the neighbour is placed exactly inside the over-inclusive
// band (victim[14]+1, trailing byte below 0xFF).
func TestSTO12_DeleteEngramCascadeStaysInsideItsOwnPrefix(t *testing.T) {
	ctx := context.Background()
	ps := newTestStore(t)
	ws := ps.VaultPrefix("sto12-cascade-neighbour")

	var victim, neighbour ULID
	copy(victim[:], []byte{0x71, 0x22, 0x33})
	victim[14] = 0x10
	victim[15] = 0xFF // the prefix byte that makes PrefixUpperBound over-inclusive
	neighbour = victim
	neighbour[14] = 0x11 // == victim[14]+1
	neighbour[15] = 0x00 // strictly inside the band the loose bound admits

	victimTarget, neighbourTarget := NewULID(), NewULID()
	seedEndpoints(t, ps, ws, victim, neighbour, victimTarget, neighbourTarget)

	// Cover both scan directions: the neighbour is a SOURCE of one edge (0x03
	// band) and a TARGET of another (0x04 band).
	for _, e := range []struct{ src, dst ULID }{
		{victim, victimTarget},
		{neighbour, neighbourTarget},
		{neighbourTarget, neighbour},
		{victimTarget, victim},
	} {
		if err := ps.WriteAssociation(ctx, ws, e.src, e.dst, danglingProbeAssoc(e.dst)); err != nil {
			t.Fatalf("WriteAssociation %s→%s: %v", e.src, e.dst, err)
		}
	}

	if err := ps.DeleteEngram(ctx, ws, victim); err != nil {
		t.Fatalf("DeleteEngram: %v", err)
	}

	// The victim's own edges are gone in both directions.
	if got := assocRowsFor(t, ps, ws, victim, victimTarget, 0.8); len(got) > 0 {
		t.Errorf("the victim's forward edge survived: %v", got)
	}
	if got := assocRowsFor(t, ps, ws, victimTarget, victim, 0.8); len(got) > 0 {
		t.Errorf("the victim's reverse edge survived: %v", got)
	}
	// The neighbour, whose keys the loose upper bound admits into the scan, is
	// untouched — rows AND engram.
	if got := assocRowsFor(t, ps, ws, neighbour, neighbourTarget, 0.8); len(got) != 3 {
		t.Errorf("the cascade crossed into the NEIGHBOURING engram's keyspace and deleted live rows: "+
			"only %v of 3 forward rows survive", got)
	}
	if got := assocRowsFor(t, ps, ws, neighbourTarget, neighbour, 0.8); len(got) != 3 {
		t.Errorf("the cascade crossed into the NEIGHBOURING engram's keyspace and deleted live rows: "+
			"only %v of 3 reverse rows survive", got)
	}
	if eng, err := ps.GetEngram(ctx, ws, neighbour); err != nil || eng == nil {
		t.Errorf("the neighbouring engram itself was disturbed: %v", err)
	}
}
