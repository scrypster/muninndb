package storage

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/scrypster/muninndb/internal/prefix"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

// absenceVsFailureCase describes one PebbleStore entry point that must tell a
// record that is NOT THERE apart from a record it FAILED TO READ.
//
// The two are different facts — one about the vault, one about the disk — and
// in this package the distinction is load-bearing because the tuple a reader
// fabricates on "absent" is the tuple a writer re-encodes a few lines later.
// The association write path did exactly that: a failed 0x03 read reported an
// empty edge, and UpdateAssocWeight then persisted that emptiness over a live
// edge's relType (reclassifying a directional relation as RelType 0, the
// Hebbian co-activation type), its createdAt, and its peakWeight — the COG-27
// decay-ceiling anchor (ceiling = peakWeight * 2^(-dt/halfLife)).
//
// This table is BEHAVIOURAL on purpose. Its predecessor was a source census
// that matched the laundering idiom, and it missed four of the five natural
// ways to rewrite the same bug (see TestPointGetReadersAreCovered). Every one
// of those five rewrites fails this table, because the table never looks at how
// the check is spelled — only at what the method returns and what it leaves on
// disk.
type absenceVsFailureCase struct {
	// method is the PebbleStore method under test. When routesPointGet is set,
	// TestPointGetReadersAreCovered cross-checks this name against the set of
	// methods that actually call pointGet, so a new reader cannot be added
	// behind the seam without a row here.
	method         string
	routesPointGet bool

	// faultPrefix is the keyspace byte whose point reads are made to fail.
	// Faulting ONE namespace is the point: when the 0x14 weight index still
	// reads fine but the 0x03 metadata read fails, the write path believes it
	// knows the old weight (so it deletes the old keys) and believes the edge
	// has no metadata (so it writes defaults) — the maximum-damage
	// interleaving, which a whole-DB outage would never isolate.
	faultPrefix byte

	// seed writes the fixture and returns the key whose stored bytes must be
	// byte-identical after a faulted call. nil means the case writes nothing
	// worth guarding.
	seed func(t *testing.T, s *PebbleStore, ws [8]byte, src, dst ULID) []byte

	// absent invokes the entry point against a pair that was never written,
	// with no fault injected. It must NOT report an error: absence is normal.
	absent func(s *PebbleStore, ws [8]byte, src, dst ULID) error

	// call invokes the entry point against the seeded pair. Under an injected
	// fault it must return an error.
	call func(s *PebbleStore, ws [8]byte, src, dst ULID) error

	// corrupt replaces a fixed-width index record with a present-but-truncated
	// one. A record that exists but is too short to decode is corruption, not
	// absence — reporting it as absence hands the same fabricated tuple to the
	// same writer, with a nil error and no injected fault anywhere.
	corrupt func(t *testing.T, s *PebbleStore, ws [8]byte, src, dst ULID)

	// afterCorrupt asserts any additional damage the corrupt path must not do.
	afterCorrupt func(t *testing.T, s *PebbleStore, ws [8]byte, src, dst ULID)
}

// absenceVsFailureCases is the coverage set. TestPointGetReadersAreCovered
// requires every pointGet-routed reader to appear here.
var absenceVsFailureCases = []absenceVsFailureCase{
	{
		method:         "GetAssocWeight",
		routesPointGet: true,
		faultPrefix:    prefix.AssocWeightIndex,
		seed: func(t *testing.T, s *PebbleStore, ws [8]byte, src, dst ULID) []byte {
			seedEdge(t, s, ws, src, dst, 0.7)
			return keys.AssocWeightIndexKey(ws, [16]byte(src), [16]byte(dst))
		},
		absent: func(s *PebbleStore, ws [8]byte, src, dst ULID) error {
			_, err := s.GetAssocWeight(context.Background(), ws, src, dst)
			return err
		},
		call: func(s *PebbleStore, ws [8]byte, src, dst ULID) error {
			_, err := s.GetAssocWeight(context.Background(), ws, src, dst)
			return err
		},
		corrupt: truncateWeightIndex,
	},
	{
		method:         "getAssocValueFull",
		routesPointGet: true,
		faultPrefix:    prefix.AssocFwd,
		seed: func(t *testing.T, s *PebbleStore, ws [8]byte, src, dst ULID) []byte {
			seedEdge(t, s, ws, src, dst, 0.6)
			return keys.AssocFwdKey(ws, [16]byte(src), 0.6, [16]byte(dst))
		},
		absent: func(s *PebbleStore, ws [8]byte, src, dst ULID) error {
			_, _, _, _, _, _, _, err := s.getAssocValueFull(context.Background(), ws, src, dst)
			return err
		},
		call: func(s *PebbleStore, ws [8]byte, src, dst ULID) error {
			_, _, _, _, _, _, _, err := s.getAssocValueFull(context.Background(), ws, src, dst)
			return err
		},
		corrupt: truncateWeightIndex,
	},
	{
		method:         "ReadOrdinal",
		routesPointGet: true,
		faultPrefix:    prefix.Ordinal,
		seed: func(t *testing.T, s *PebbleStore, ws [8]byte, src, dst ULID) []byte {
			if err := s.WriteOrdinal(context.Background(), ws, src, dst, 7); err != nil {
				t.Fatalf("WriteOrdinal: %v", err)
			}
			return keys.OrdinalKey(ws, [16]byte(src), [16]byte(dst))
		},
		absent: func(s *PebbleStore, ws [8]byte, src, dst ULID) error {
			_, _, err := s.ReadOrdinal(context.Background(), ws, src, dst)
			return err
		},
		call: func(s *PebbleStore, ws [8]byte, src, dst ULID) error {
			_, _, err := s.ReadOrdinal(context.Background(), ws, src, dst)
			return err
		},
		corrupt: func(t *testing.T, s *PebbleStore, ws [8]byte, src, dst ULID) {
			t.Helper()
			setShort(t, s, keys.OrdinalKey(ws, [16]byte(src), [16]byte(dst)))
		},
	},
	{
		// Not a pointGet caller itself — it reads through the two above. It is
		// in the table because it is the WRITER that persists whatever they
		// report, so it is where a laundered read does its damage.
		method:      "UpdateAssocWeight",
		faultPrefix: prefix.AssocFwd,
		seed: func(t *testing.T, s *PebbleStore, ws [8]byte, src, dst ULID) []byte {
			seedEdge(t, s, ws, src, dst, 0.85)
			return keys.AssocFwdKey(ws, [16]byte(src), 0.85, [16]byte(dst))
		},
		absent: func(s *PebbleStore, ws [8]byte, src, dst ULID) error {
			return s.UpdateAssocWeight(context.Background(), ws, src, dst, 0.2, 1)
		},
		call: func(s *PebbleStore, ws [8]byte, src, dst ULID) error {
			return s.UpdateAssocWeight(context.Background(), ws, src, dst, 0.15, 1)
		},
		corrupt:      truncateWeightIndex,
		afterCorrupt: assertSingleLiveEdge,
	},
	{
		method:      "UpdateAssocWeightBatch",
		faultPrefix: prefix.AssocFwd,
		seed: func(t *testing.T, s *PebbleStore, ws [8]byte, src, dst ULID) []byte {
			seedEdge(t, s, ws, src, dst, 0.9)
			return keys.AssocFwdKey(ws, [16]byte(src), 0.9, [16]byte(dst))
		},
		absent: func(s *PebbleStore, ws [8]byte, src, dst ULID) error {
			return s.UpdateAssocWeightBatch(context.Background(), []AssocWeightUpdate{{
				WS: ws, Src: [16]byte(src), Dst: [16]byte(dst), Weight: 0.2, CountDelta: 1,
			}})
		},
		call: func(s *PebbleStore, ws [8]byte, src, dst ULID) error {
			return s.UpdateAssocWeightBatch(context.Background(), []AssocWeightUpdate{{
				WS: ws, Src: [16]byte(src), Dst: [16]byte(dst), Weight: 0.2, CountDelta: 1,
			}})
		},
		corrupt:      truncateWeightIndex,
		afterCorrupt: assertSingleLiveEdge,
	},
}

// truncateWeightIndex replaces the pair's 4-byte 0x14 weight-index record with
// a 2-byte one. Every writer of that key in this package writes exactly 4
// bytes, so a shorter record can only be damage.
func truncateWeightIndex(t *testing.T, s *PebbleStore, ws [8]byte, src, dst ULID) {
	t.Helper()
	setShort(t, s, keys.AssocWeightIndexKey(ws, [16]byte(src), [16]byte(dst)))
}

func setShort(t *testing.T, s *PebbleStore, key []byte) {
	t.Helper()
	if err := s.db.Set(key, []byte{0x00, 0x01}, nil); err != nil {
		t.Fatalf("Set truncated record: %v", err)
	}
}

// assertSingleLiveEdge fails if the pair ended up with more than one live 0x03
// edge. A writer that treated a corrupt weight index as "no edge" reads
// oldWeight as 0, skips the delete of the existing forward/reverse keys, and
// writes a SECOND edge for the same pair at the new weight — which
// GetAssociations, BFS traversal and DecayAssocWeights all then treat as two
// independent relations.
func assertSingleLiveEdge(t *testing.T, s *PebbleStore, ws [8]byte, src, dst ULID) {
	t.Helper()
	if n := countLiveEdges(t, s, ws, src, dst); n != 1 {
		t.Errorf("live forward edges for the pair: got %d, want 1 — a corrupt weight index "+
			"read as absence made the writer duplicate the edge instead of relocating it", n)
	}
}

// countLiveEdges scans the 0x03 namespace directly (not the 0x14 index, which
// can only ever name one weight position) and counts forward keys for the pair.
func countLiveEdges(t *testing.T, s *PebbleStore, ws [8]byte, src, dst ULID) int {
	t.Helper()
	scan := make([]byte, 0, 25)
	scan = append(scan, prefix.AssocFwd)
	scan = append(scan, ws[:]...)
	scan = append(scan, src[:]...)
	iter, err := PrefixIterator(s.db, scan)
	if err != nil {
		t.Fatalf("prefix iterator: %v", err)
	}
	defer iter.Close()
	n := 0
	for iter.First(); iter.Valid(); iter.Next() {
		k := iter.Key()
		if len(k) < 45 || !bytes.Equal(k[29:45], dst[:]) {
			continue
		}
		n++
	}
	if err := iter.Error(); err != nil {
		t.Fatalf("scan forward edges: %v", err)
	}
	return n
}

// TestAbsenceIsNotFailure is the behavioural pin for the laundered-read class.
//
// For every entry point in absenceVsFailureCases it asserts four things:
//
//	absent          — a pair that was never written reports NO error
//	read failure    — an injected point-read fault surfaces as an error
//	no rewrite      — that failed call left the stored record byte-identical
//	short record    — a present-but-truncated fixed-width record is an error,
//	                  not absence, with no fault injected anywhere
//
// It is deliberately indifferent to how the absence check is written. Any
// implementation that swallows the read error fails the second assertion; any
// implementation that swallows it AND writes fails the third as well.
func TestAbsenceIsNotFailure(t *testing.T) {
	for _, tc := range absenceVsFailureCases {
		t.Run(tc.method, func(t *testing.T) {
			t.Run("absence is not an error", func(t *testing.T) {
				store := newTestStore(t)
				ws := store.VaultPrefix("absence-" + tc.method)
				src, dst := NewULID(), NewULID()
				if tc.seed != nil {
					tc.seed(t, store, ws, src, dst)
				}
				// A pair that was never written, on a healthy store.
				// The absent endpoint is a REAL engram with no edge to it:
				// this subtest's variable is EDGE absence. STO-12 makes an
				// edge to a nonexistent engram a different fact (a refused
				// write), and conflating the two would have this row assert
				// that dangling writes are normal.
				absentDst := NewULID()
				seedEndpoints(t, store, ws, absentDst)
				if err := tc.absent(store, ws, src, absentDst); err != nil {
					t.Errorf("%s on an absent pair: got error %v, want nil — absence is a normal fact", tc.method, err)
				}
			})

			t.Run("read failure is an error", func(t *testing.T) {
				store := newTestStore(t)
				ws := store.VaultPrefix("failure-" + tc.method)
				src, dst := NewULID(), NewULID()
				var guard []byte
				var before []byte
				if tc.seed != nil {
					guard = tc.seed(t, store, ws, src, dst)
					var err error
					before, err = Get(store.db, guard)
					if err != nil {
						t.Fatalf("read guarded key: %v", err)
					}
					if before == nil {
						t.Fatalf("guarded key %x missing after seed", guard)
					}
				}

				store.readFault = failReadsWithPrefix(tc.faultPrefix)
				err := tc.call(store, ws, src, dst)
				store.readFault = nil

				if err == nil {
					t.Fatalf("%s under an injected read fault: got nil error, want the failure surfaced. "+
						"Reporting a failed read as absence hands a fabricated tuple to the writer downstream.", tc.method)
				}
				if !errors.Is(err, errInjectedRead) {
					t.Errorf("%s: want the injected read failure wrapped, got %v", tc.method, err)
				}

				if guard != nil {
					after, gerr := Get(store.db, guard)
					if gerr != nil {
						t.Fatalf("re-read guarded key: %v", gerr)
					}
					if !bytes.Equal(after, before) {
						t.Errorf("%s rewrote the stored record on a failed read: got %x, want %x", tc.method, after, before)
					}
				}
			})

			if tc.corrupt == nil {
				return
			}
			t.Run("short record is not absence", func(t *testing.T) {
				store := newTestStore(t)
				ws := store.VaultPrefix("short-" + tc.method)
				src, dst := NewULID(), NewULID()
				if tc.seed != nil {
					tc.seed(t, store, ws, src, dst)
				}
				tc.corrupt(t, store, ws, src, dst)

				// No fault injected: the record is simply too short to decode.
				if err := tc.call(store, ws, src, dst); err == nil {
					t.Errorf("%s on a present-but-truncated fixed-width record: got nil error, want an error. "+
						"Every writer of these index records writes exactly 4 bytes, so a shorter one is "+
						"damage; reading it as absence fabricates the same tuple a failed read would.", tc.method)
				}
				if tc.afterCorrupt != nil {
					tc.afterCorrupt(t, store, ws, src, dst)
				}
			})
		})
	}
}

// TestPointGetSeamIsNilInProductionConstructors asserts what the seam's safety
// actually rests on: no constructor sets readFault, and with it nil pointGet is
// a pass-through for BOTH outcomes it mediates — a present key and a missing
// key — through every reader that routes to it.
//
// The previous version of this test was named ...SeamIsInert but only checked
// that the field defaulted to nil and that one successful read returned the
// right weight. That is a weaker property than the name promised: it never
// exercised the missing-key branch, and it never touched getAssocValueFull or
// ReadOrdinal. The stronger structural fact — that nothing in the production
// code assigns readFault — is asserted by TestReadFaultIsArmedOnlyByTests (read_error_census_test.go).
func TestPointGetSeamIsNilInProductionConstructors(t *testing.T) {
	store := newTestStore(t)
	ws := store.VaultPrefix("seam-nil")
	if store.readFault != nil {
		t.Fatal("readFault must be nil on a store built by the production constructor")
	}

	src, dst := NewULID(), NewULID()
	seedEdge(t, store, ws, src, dst, 0.5)
	if err := store.WriteOrdinal(context.Background(), ws, src, dst, 3); err != nil {
		t.Fatalf("WriteOrdinal: %v", err)
	}
	absent := NewULID()

	// Present key, every pointGet-routed reader.
	if w, err := store.GetAssocWeight(context.Background(), ws, src, dst); err != nil || w < 0.499 || w > 0.501 {
		t.Errorf("GetAssocWeight (present): got %v, %v; want 0.5, nil", w, err)
	}
	if rt, _, _, _, _, _, _, err := store.getAssocValueFull(context.Background(), ws, src, dst); err != nil || rt != RelSupersedes {
		t.Errorf("getAssocValueFull (present): got relType %v, err %v; want RelSupersedes, nil", rt, err)
	}
	if ord, found, err := store.ReadOrdinal(context.Background(), ws, src, dst); err != nil || !found || ord != 3 {
		t.Errorf("ReadOrdinal (present): got %v, %v, %v; want 3, true, nil", ord, found, err)
	}

	// Missing key: the other branch pointGet mediates.
	if w, err := store.GetAssocWeight(context.Background(), ws, src, absent); err != nil || w != 0 {
		t.Errorf("GetAssocWeight (absent): got %v, %v; want 0, nil", w, err)
	}
	if _, _, _, _, _, _, _, err := store.getAssocValueFull(context.Background(), ws, src, absent); err != nil {
		t.Errorf("getAssocValueFull (absent): got err %v, want nil", err)
	}
	if _, found, err := store.ReadOrdinal(context.Background(), ws, src, absent); err != nil || found {
		t.Errorf("ReadOrdinal (absent): got found=%v, err=%v; want false, nil", found, err)
	}
}
