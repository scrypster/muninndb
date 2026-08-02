package storage

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/cockroachdb/pebble"

	"github.com/scrypster/muninndb/internal/storage/keys"
)

// tightPrefixUpperBound is what keys.PrefixUpperBound SHOULD return: the first
// sub-0xFF byte from the right is incremented AND every trailing 0xFF byte is
// zeroed, so the bound covers the prefix and nothing beyond it.
//
// The test computes its own bound deliberately. Counting the neighbour's
// surviving rows with keys.PrefixUpperBound would count rows outside the
// neighbour's prefix too, which is the very looseness under test — the assertion
// would then be measured with the broken ruler. (Fixing the shared helper is
// #816; this file must keep working either way.)
func tightPrefixUpperBound(prefix []byte) []byte {
	bound := append([]byte{}, prefix...)
	for i := len(bound) - 1; i >= 0; i-- {
		if bound[i] < 0xFF {
			bound[i]++
			return bound
		}
		bound[i] = 0x00
	}
	return append(append([]byte{}, prefix...), 0x00)
}

// countRowsUnder returns the number of keys that live strictly under prefix.
func countRowsUnder(t *testing.T, ps *PebbleStore, prefix []byte) int {
	t.Helper()
	iter, err := ps.db.NewIter(&pebble.IterOptions{
		LowerBound: prefix,
		UpperBound: tightPrefixUpperBound(prefix),
	})
	if err != nil {
		t.Fatalf("count iterator: %v", err)
	}
	defer iter.Close()
	n := 0
	for valid := iter.First(); valid; valid = iter.Next() {
		if !bytes.HasPrefix(iter.Key(), prefix) {
			t.Fatalf("count iterator escaped its own prefix: %x", iter.Key())
		}
		n++
	}
	return n
}

// seedArchiveRow writes one 0x25 archive row directly, bypassing the decay path
// that would normally produce it. The value is a well-formed 26-byte archive
// value so that RestoreArchivedEdges would genuinely consider it a candidate.
func seedArchiveRow(t *testing.T, ps *PebbleStore, ws [8]byte, src, dst ULID) {
	t.Helper()
	v := encodeAssocValue(RelSupports, 0.9, time.Unix(1_700_000_000, 0), int32(time.Now().Unix()), 0.9, 3)
	if err := ps.db.Set(keys.ArchiveAssocKey(ws, [16]byte(src), [16]byte(dst)), v[:], pebble.NoSync); err != nil {
		t.Fatalf("seed archive row %s→%s: %v", src, dst, err)
	}
}

// TestSTO11_EveryDestructivePrefixScanStaysInsideItsOwnPrefix is the machine
// check over EVERY destructive scan bounded by a 25-byte `kind|ws(8)|id(16)`
// prefix. It is a table, not four tests, so that the fifth such scan is a row
// rather than a rediscovery.
//
// The shared shape: `keys.PrefixUpperBound` (and `storage.PrefixIterator`, whose
// bound computation is byte-identical) increments the first sub-0xFF byte from
// the right and returns WITHOUT zeroing the trailing 0xFF bytes. For a prefix
// whose last byte is 0xFF the returned bound therefore admits keys belonging to
// the NEXT id. Every loop below deletes what its iterator returns, so without an
// explicit `bytes.Equal(k[:25], prefix)` break each one can delete a live,
// unrelated engram's rows.
//
// # Reachability, stated honestly
//
// This is STRUCTURAL HYGIENE, not a live data-loss report. To land inside the
// widened band a second engram must share the victim's first 14 bytes — the full
// 48-bit ULID millisecond timestamp AND 8 of the 10 crypto-random entropy bytes,
// i.e. ~2^-64 on top of a same-millisecond collision. With ULID-shaped keys that
// is not operationally reachable, and every arm below has to CONSTRUCT its ids
// by hand to reproduce it. "~1 id in 256" is the rate at which the BOUND IS
// LOOSE, not the rate at which anything is lost; the two differ by about 64
// bits.
//
// It is worth guarding anyway, and worth guarding uniformly: the mandated shared
// helper's contract is wrong, the compensation is a per-key check at each call
// site, and any future non-ULID id tail (a counter, a hash truncation, a
// content-addressed key) collapses that 64-bit gap to zero the day it lands —
// silently, on a delete path. Fixing the helper itself is #816.
func TestSTO11_EveryDestructivePrefixScanStaysInsideItsOwnPrefix(t *testing.T) {
	ctx := context.Background()

	// Constructed, never hoped-for: the victim's id ends in 0xFF (the byte that
	// makes the bound over-inclusive) and the neighbour sits exactly inside the
	// band the loose bound admits — id[14]+1, trailing byte below 0xFF.
	newIDs := func() (victim, neighbour ULID) {
		copy(victim[:], []byte{0x71, 0x22, 0x33})
		victim[14] = 0x10
		victim[15] = 0xFF
		neighbour = victim
		neighbour[14] = 0x11
		neighbour[15] = 0x00
		return victim, neighbour
	}

	cases := []struct {
		name string
		// prefixFor builds the 25-byte destructive scan prefix for an id.
		prefixFor func(ws [8]byte, id ULID) []byte
		// seed places at least one row under prefixFor(victim) and under
		// prefixFor(neighbour), plus whatever run needs to reach its loop.
		seed func(t *testing.T, ps *PebbleStore, ws [8]byte, victim, neighbour ULID)
		// run drives the destructive scan whose subject is the victim.
		run func(t *testing.T, ps *PebbleStore, ws [8]byte, victim ULID)
	}{
		{
			// DeleteEngram's forward pass. Guarded on this branch; kept in the
			// table so the enumeration is complete in one place.
			name:      "DeleteEngram 0x03 forward cascade",
			prefixFor: func(ws [8]byte, id ULID) []byte { return keys.AssocFwdPrefixForID(ws, [16]byte(id)) },
			seed: func(t *testing.T, ps *PebbleStore, ws [8]byte, victim, neighbour ULID) {
				vt, nt := NewULID(), NewULID()
				seedEndpoints(t, ps, ws, victim, neighbour, vt, nt)
				for _, e := range []struct{ src, dst ULID }{{victim, vt}, {neighbour, nt}} {
					if err := ps.WriteAssociation(ctx, ws, e.src, e.dst, danglingProbeAssoc(e.dst)); err != nil {
						t.Fatalf("WriteAssociation: %v", err)
					}
				}
			},
			run: func(t *testing.T, ps *PebbleStore, ws [8]byte, victim ULID) {
				if err := ps.DeleteEngram(ctx, ws, victim); err != nil {
					t.Fatalf("DeleteEngram: %v", err)
				}
			},
		},
		{
			// DeleteEngram's reverse pass.
			name:      "DeleteEngram 0x04 reverse cascade",
			prefixFor: func(ws [8]byte, id ULID) []byte { return keys.AssocRevPrefixForID(ws, [16]byte(id)) },
			seed: func(t *testing.T, ps *PebbleStore, ws [8]byte, victim, neighbour ULID) {
				vs, ns := NewULID(), NewULID()
				seedEndpoints(t, ps, ws, victim, neighbour, vs, ns)
				for _, e := range []struct{ src, dst ULID }{{vs, victim}, {ns, neighbour}} {
					if err := ps.WriteAssociation(ctx, ws, e.src, e.dst, danglingProbeAssoc(e.dst)); err != nil {
						t.Fatalf("WriteAssociation: %v", err)
					}
				}
			},
			run: func(t *testing.T, ps *PebbleStore, ws [8]byte, victim ULID) {
				if err := ps.DeleteEngram(ctx, ws, victim); err != nil {
					t.Fatalf("DeleteEngram: %v", err)
				}
			},
		},
		{
			// DeleteEngram's archived-source cascade, ~15 lines below the two
			// loops above and structurally identical — it uses PrefixIterator,
			// whose bound carries the same defect.
			name:      "DeleteEngram 0x25 archived-source cascade",
			prefixFor: func(ws [8]byte, id ULID) []byte { return keys.ArchiveAssocPrefixForID(ws, [16]byte(id)) },
			seed: func(t *testing.T, ps *PebbleStore, ws [8]byte, victim, neighbour ULID) {
				vt, nt := NewULID(), NewULID()
				seedEndpoints(t, ps, ws, victim, neighbour, vt, nt)
				seedArchiveRow(t, ps, ws, victim, vt)
				seedArchiveRow(t, ps, ws, neighbour, nt)
			},
			run: func(t *testing.T, ps *PebbleStore, ws [8]byte, victim ULID) {
				if err := ps.DeleteEngram(ctx, ws, victim); err != nil {
					t.Fatalf("DeleteEngram: %v", err)
				}
			},
		},
		{
			// reapArchivedEdgesFrom, on the RECALL READ PATH: an ordinary
			// RestoreArchivedEdges for a source with no 0x01 record reaps the
			// whole prefix. Driven through the public entry point rather than
			// called directly, because the reachability is the point.
			name:      "reapArchivedEdgesFrom via RestoreArchivedEdges",
			prefixFor: func(ws [8]byte, id ULID) []byte { return keys.ArchiveAssocPrefixForID(ws, [16]byte(id)) },
			seed: func(t *testing.T, ps *PebbleStore, ws [8]byte, victim, neighbour ULID) {
				vt, nt := NewULID(), NewULID()
				// The victim deliberately has NO 0x01 record — that is what
				// sends RestoreArchivedEdges down the reap branch.
				seedEndpoints(t, ps, ws, neighbour, vt, nt)
				seedArchiveRow(t, ps, ws, victim, vt)
				seedArchiveRow(t, ps, ws, neighbour, nt)
			},
			run: func(t *testing.T, ps *PebbleStore, ws [8]byte, victim ULID) {
				if _, err := ps.RestoreArchivedEdges(ctx, ws, [16]byte(victim), restoreTopN); err != nil {
					t.Fatalf("RestoreArchivedEdges: %v", err)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ps := newTestStore(t)
			ws := ps.VaultPrefix("sto11-destructive-bounds")
			victim, neighbour := newIDs()

			tc.seed(t, ps, ws, victim, neighbour)

			victimPrefix := tc.prefixFor(ws, victim)
			neighbourPrefix := tc.prefixFor(ws, neighbour)
			if got := countRowsUnder(t, ps, victimPrefix); got == 0 {
				t.Fatal("precondition: the victim has no rows under the scanned prefix")
			}
			before := countRowsUnder(t, ps, neighbourPrefix)
			if before == 0 {
				t.Fatal("precondition: the neighbour has no rows inside the widened band")
			}

			tc.run(t, ps, ws, victim)

			if got := countRowsUnder(t, ps, victimPrefix); got != 0 {
				t.Errorf("the scan left %d of the victim's own rows behind", got)
			}
			if after := countRowsUnder(t, ps, neighbourPrefix); after != before {
				t.Errorf("the destructive scan crossed into the NEIGHBOURING id's keyspace: "+
					"neighbour rows %d -> %d (keys.PrefixUpperBound is loose and this loop "+
					"has no bytes.Equal(k[:25], prefix) guard)", before, after)
			}
		})
	}
}
