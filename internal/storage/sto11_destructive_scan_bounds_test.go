package storage

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/cockroachdb/pebble"

	"github.com/scrypster/muninndb/internal/storage/keys"
)

// tightPrefixUpperBound increments the first sub-0xFF byte from the right AND
// zeroes every trailing 0xFF byte, so the bound covers the prefix and nothing
// beyond it. Since #816 keys.PrefixUpperBound agrees with it.
//
// DO NOT replace this with a call to keys.PrefixUpperBound. The test computes
// its own bound deliberately: the thing under test is whether a destructive
// scan stays inside its prefix, and the scan's bound comes from the shared
// helper. Measuring the survivors with the same helper the subject uses means a
// future regression in the helper moves the ruler and the assertion at the same
// time — the test would keep passing while the code it guards over-deletes.
// That is not hypothetical: this file was written while the helper WAS loose,
// and it is the reason it could measure that at all.
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
// The shared shape it was written against: `keys.PrefixUpperBound` (and
// `storage.PrefixIterator`, which open-coded a byte-identical copy) incremented
// the first sub-0xFF byte from the right and returned WITHOUT clearing the
// trailing 0xFF bytes, so for a prefix whose last byte is 0xFF the bound
// admitted keys belonging to the NEXT id. #816 fixed the helper and
// PrefixIterator now delegates to it, so each loop below is inside its prefix
// twice over: by its bound AND by an explicit `bytes.Equal(k[:25], prefix)`
// break. Both are deliberate. Every loop deletes what its iterator returns, and
// a delete loop should not depend on a helper in another package for the only
// thing holding it in place — so the guards stay and this test keeps measuring
// them with its own ruler.
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
// It is worth guarding anyway, and worth guarding uniformly, even now that #816
// has made the shared helper tight: the compensation is one comparison per key
// at each call site, and any future non-ULID id tail (a counter, a hash
// truncation, a content-addressed key) collapses that 64-bit gap to zero the day
// it lands — silently, on a delete path. The guards are the property this test
// measures; the helper being correct is a second, independent line.
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
			//
			// The FIFTH scan over this same prefix — RestoreArchivedEdges' own
			// candidate loop, twenty lines above the reap — cannot be a row in
			// this table: it is destructive AND creative, and it must leave most
			// of the victim's rows in place. It has its own test, immediately
			// below this one.
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
					"neighbour rows %d -> %d — either this loop's upper bound went loose "+
					"or its bytes.Equal(k[:25], prefix) guard is gone; both must hold",
					before, after)
			}
		})
	}
}

// TestSTO11_RestoreArchivedEdgesCandidateScanStaysInsideItsOwnPrefix pins the
// FIFTH scan over the 25-byte `0x25|ws|src` prefix: RestoreArchivedEdges' own
// candidate loop, twenty lines above the reap that IS in the table.
//
// It is not a row in that table because its shape is different in the way that
// matters. The four tabled loops only delete, and a crossing shows up as the
// neighbour losing rows. This loop deletes (the archive row it consumes) and
// ALSO MINTS live 0x03/0x04/0x14 rows — so a crossing shows up as an edge that
// never existed being fabricated from the victim to a NEIGHBOUR's destination,
// stamped restoredAt, which then permanently exempts it from GCArchivedEdges.
//
// It has no `bytes.Equal(k[:25], prefix)` break, so its upper bound is its ONLY
// protection. RestoreArchivedEdges hand-rolls that bound (increment from the
// right, zero every byte it wraps) rather than calling keys.PrefixUpperBound or
// PrefixIterator like its four neighbours. #816 has since made the shared helper
// agree with it, so the two are now equivalent — but the hand-rolled bound stays,
// because "equivalent today" and "the only thing standing between this loop and
// fabricated edges" is a combination that should not be maintained at a distance.
//
// This test is what catches the loosening, whatever its route: a helper
// regression, or the innocent-looking edit that "unifies" this loop with the reap
// below it and drops the bound in the process. `engramExists(ws, c.dst)` at the
// top of the restore loop will NOT catch it: the dst it reads out of the
// neighbour's key is a real, live engram. The liveness guard is orthogonal to the
// bound.
//
// Reachability is the same ~2^-64 structural-hygiene argument as the table's:
// the ids are constructed by hand. The point is the invariant, not an incident.
func TestSTO11_RestoreArchivedEdgesCandidateScanStaysInsideItsOwnPrefix(t *testing.T) {
	ctx := context.Background()
	ps := newTestStore(t)
	ws := ps.VaultPrefix("sto11-restore-candidate-bounds")

	// Same construction as the table: the victim's id ends in 0xFF, the
	// neighbour sits exactly inside the band a loose bound would admit.
	var victim ULID
	copy(victim[:], []byte{0x71, 0x22, 0x33})
	victim[14] = 0x10
	victim[15] = 0xFF
	neighbour := victim
	neighbour[14] = 0x11
	neighbour[15] = 0x00

	victimDst, neighbourDst := NewULID(), NewULID()
	// Every endpoint is LIVE. The victim has a 0x01 record so the call takes the
	// restore branch rather than the reap branch, and neighbourDst has one so
	// that the restore loop's engramExists guard would happily wave it through.
	seedEndpoints(t, ps, ws, victim, neighbour, victimDst, neighbourDst)
	seedArchiveRow(t, ps, ws, victim, victimDst)
	seedArchiveRow(t, ps, ws, neighbour, neighbourDst)

	neighbourPrefix := keys.ArchiveAssocPrefixForID(ws, [16]byte(neighbour))
	before := countRowsUnder(t, ps, neighbourPrefix)
	if before == 0 {
		t.Fatal("precondition: the neighbour has no archive rows inside the widened band")
	}

	restored, err := ps.RestoreArchivedEdges(ctx, ws, [16]byte(victim), restoreTopN)
	if err != nil {
		t.Fatalf("RestoreArchivedEdges: %v", err)
	}

	// Every assertion below is an Error, not a Fatal: when the bound goes loose
	// each one reports a distinct consequence of the same crossing, and seeing
	// all of them is the difference between "a count is off" and "an edge that
	// never existed is now live and permanently un-collectable".
	for _, got := range restored {
		if got == [16]byte(neighbourDst) {
			t.Error("the candidate scan crossed into the NEIGHBOURING source's archive band and " +
				"restored an edge from the victim to a destination it was never associated with")
		}
	}

	// The decisive row: 0x14 is keyed on (src,dst) with no weight component, so
	// its presence is an exact statement that a victim→neighbourDst edge exists.
	fabricated := keys.AssocWeightIndexKey(ws, [16]byte(victim), [16]byte(neighbourDst))
	if _, closer, gErr := ps.db.Get(fabricated); gErr == nil {
		_ = closer.Close()
		t.Error("FABRICATED EDGE: a live 0x14 weight-index row now exists from the victim to the " +
			"NEIGHBOUR's archived destination. The candidate loop has no prefix check; it is held " +
			"inside its own keyspace solely by the hand-rolled tight upper bound in " +
			"RestoreArchivedEdges, which this run no longer has.")
	}
	if w, _ := ps.GetAssocWeight(ctx, ws, victim, neighbourDst); w != 0 {
		t.Errorf("FABRICATED EDGE: GetAssocWeight(victim, neighbourDst) = %v, want 0", w)
	}

	// Belt-and-braces on the neighbour's own rows. Measured: under a loose bound
	// this one does NOT fire, and the reason is worth recording — the archive
	// delete is keyed ArchiveAssocKey(ws, srcID, c.dst) with srcID = the VICTIM,
	// so the crossing reads the neighbour's row and then deletes a key that does
	// not exist. The neighbour keeps its archive row AND the victim gains a live
	// edge: a duplication, not a move, and therefore invisible to any assertion
	// that only counts the neighbour's rows. Kept so a future variant that does
	// delete under the scanned key is caught here rather than in production.
	if after := countRowsUnder(t, ps, neighbourPrefix); after != before {
		t.Errorf("the candidate scan consumed the NEIGHBOURING id's archive rows: %d -> %d",
			before, after)
	}

	// A bound that is too TIGHT is also a defect: the victim's own edge must
	// still restore, exactly once. Checked last so a loose bound reports its
	// real damage above rather than bailing out here on a count.
	if len(restored) != 1 || restored[0] != [16]byte(victimDst) {
		t.Errorf("the victim's own archived edge did not restore cleanly: got %d restored, "+
			"want exactly [%s]", len(restored), victimDst)
	}
}
