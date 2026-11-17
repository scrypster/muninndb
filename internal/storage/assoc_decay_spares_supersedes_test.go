package storage

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/cockroachdb/pebble"

	"github.com/scrypster/muninndb/internal/storage/keys"
)

// Association decay (COG-27) and its archive (0x25) treated every relation the
// same. A supersedes edge is a declaration, not a co-activation: when decay
// relocated it, archived it, or deleted it, the memory it retired looked free,
// the fork guard admitted a rival, and the lazy restore then gave the memory a
// second successor for good. And decay's relocation wrote its new key with no
// STO-15 check, so two relations on one pair that decayed to the same weight
// erased one of them.
//
// Fixtures are invented (an orchard).

// defaultDecay is the default preset: half-life 30 days, min weight 0.05,
// archive threshold 0.05.
func defaultDecay(t *testing.T, ps *PebbleStore, ws [8]byte, at time.Time) {
	t.Helper()
	ps.decayNow = func() time.Time { return at }
	defer func() { ps.decayNow = nil }()
	if _, err := ps.DecayAssocWeights(context.Background(), ws, 30*24*time.Hour, 0.05, 0.05); err != nil {
		t.Fatalf("decay: %v", err)
	}
}

// rawEdge writes an association's forward, reverse and weight-index records as
// a store holds them after months of use: an old lastActivated, a peak, a
// co-activation count. (WriteAssociation seeds those from the write itself.)
func rawEdge(t *testing.T, ps *PebbleStore, ws [8]byte, src, dst ULID, rel RelType, w float32, lastAct time.Time, peak float32, coAct uint32) {
	t.Helper()
	val := encodeAssocValue(rel, 1, lastAct.Add(-time.Hour), int32(lastAct.Unix()), peak, coAct)
	for _, k := range [][]byte{keys.AssocFwdKey(ws, [16]byte(src), w, [16]byte(dst)), keys.AssocRevKey(ws, [16]byte(dst), w, [16]byte(src))} {
		if err := ps.db.Set(k, val[:], pebble.Sync); err != nil {
			t.Fatal(err)
		}
	}
	var wb [4]byte
	binary.BigEndian.PutUint32(wb[:], math.Float32bits(w))
	if err := ps.db.Set(keys.AssocWeightIndexKey(ws, [16]byte(src), [16]byte(dst)), wb[:], pebble.Sync); err != nil {
		t.Fatal(err)
	}
	ps.assocCache.Remove(assocCacheKey(ws, src))
	ps.revAssocCache.Remove(assocCacheKey(ws, dst))
}

// rawArchived writes a 0x25 archive row as decay leaves one.
func rawArchived(t *testing.T, ps *PebbleStore, ws [8]byte, src, dst ULID, rel RelType, lastAct time.Time, peak float32, coAct uint32) {
	t.Helper()
	val := encodeArchiveValue(rel, 1, lastAct.Add(-time.Hour), int32(lastAct.Unix()), peak, coAct, 0)
	if err := ps.db.Set(keys.ArchiveAssocKey(ws, [16]byte(src), [16]byte(dst)), val[:], pebble.Sync); err != nil {
		t.Fatal(err)
	}
	ps.AddToArchiveBloom([16]byte(src))
}

// pairEdges reads the live forward edges from src to dst straight from the
// keys: relation → weight.
func pairEdges(t *testing.T, ps *PebbleStore, ws [8]byte, src, dst ULID) map[RelType]float32 {
	t.Helper()
	out := map[RelType]float32{}
	it, err := PrefixIterator(ps.db, keys.AssocFwdPrefixForID(ws, [16]byte(src)))
	if err != nil {
		t.Fatal(err)
	}
	defer it.Close()
	for it.First(); it.Valid(); it.Next() {
		k := it.Key()
		if len(k) != 45 || !bytes.Equal(k[29:45], dst[:]) {
			continue
		}
		var wc [4]byte
		copy(wc[:], k[25:29])
		rel, _, _, _, _, _, _ := decodeAssocValue(it.Value())
		out[rel] = keys.WeightFromComplement(wc)
	}
	return out
}

// floorOf is decay's dynamic floor for a peak, computed as decay computes it
// (float32 arithmetic), so a fixture lands on the very key decay writes.
func floorOf(peak float32) float32 { return peak * 0.05 }

func archivedRel(t *testing.T, ps *PebbleStore, ws [8]byte, src, dst ULID) (RelType, bool) {
	t.Helper()
	v, err := Get(ps.db, keys.ArchiveAssocKey(ws, [16]byte(src), [16]byte(dst)))
	if err != nil {
		t.Fatal(err)
	}
	if v == nil {
		return 0, false
	}
	rel, _, _, _, _, _, _ := decodeAssocValue(v)
	return rel, true
}

// TestDecayAssocWeights_NeverTouchesASupersedesEdge: decay leaves a supersedes
// edge's 0x03, 0x04 and 0x14 records byte for byte as they were, and writes no
// 0x25 row, in every shape decay used to move it: co-activated and idle (it was
// ARCHIVED), declared just above the floor (archived after one day), declared
// with a negative weight (DELETED by the first pass), and idle to the floor
// (relocated).
//
// RED on develop: each case moves, archives or deletes the edge.
func TestDecayAssocWeights_NeverTouchesASupersedesEdge(t *testing.T) {
	now := time.Now()
	for _, c := range []struct {
		name    string
		w, peak float32
		coAct   uint32
		idle    time.Duration
	}{
		{"co-activated and idle for 200 days", 0.8, 0.8, 20, 200 * 24 * time.Hour},
		{"declared just above the floor, idle a day", 0.051, 0.051, 1, 24*time.Hour + time.Minute},
		{"declared with a negative weight", -0.5, -0.5, 1, time.Minute},
		{"idle to the floor", 0.8, 0.8, 1, 200 * 24 * time.Hour},
	} {
		t.Run(c.name, func(t *testing.T) {
			ps := openTestStore(t)
			ws := ps.VaultPrefix("orchard-ledger")
			target := writeStampFixture(t, ps, ws, "plum row netted in May")
			succ := writeStampFixture(t, ps, ws, "plum row netted in April from now on")
			rawEdge(t, ps, ws, succ, target, RelSupersedes, c.w, now.Add(-c.idle), c.peak, c.coAct)
			recs := [][]byte{
				keys.AssocFwdKey(ws, [16]byte(succ), c.w, [16]byte(target)),
				keys.AssocRevKey(ws, [16]byte(target), c.w, [16]byte(succ)),
				keys.AssocWeightIndexKey(ws, [16]byte(succ), [16]byte(target)),
			}
			var before [][]byte
			for _, k := range recs {
				v, err := Get(ps.db, k)
				if err != nil || v == nil {
					t.Fatalf("fixture: %x missing (%v)", k, err)
				}
				before = append(before, append([]byte(nil), v...))
			}

			defaultDecay(t, ps, ws, now)
			defaultDecay(t, ps, ws, now.Add(365*24*time.Hour))

			for i, k := range recs {
				v, err := Get(ps.db, k)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(v, before[i]) {
					t.Errorf("record %d of the supersedes edge changed: %x -> %x", i, before[i], v)
				}
			}
			if rel, ok := archivedRel(t, ps, ws, succ, target); ok {
				t.Errorf("decay archived the supersedes edge (0x25 row, relation %d)", rel)
			}
			if got, _ := ps.SupersedersOf(context.Background(), ws, target); len(got) != 1 || got[0] != succ {
				t.Errorf("SupersedersOf(target) = %v, want [%s]", got, succ)
			}
		})
	}
}

// TestDecayAssocWeights_NeverOverwritesAnotherRelation: a relation's weight is
// in the key and its type is not, so two relations on one pair that decay to
// the same weight (the floor, peak*0.05, is the same for equal peaks) meet at
// one key. Decay used to Set the second over the first with no STO-15 check,
// erasing it. Now the second stays at the weight it has: both relations
// survive, in either order, and against a key an earlier pass already left at
// the floor.
//
// RED on develop: one of the two relations is gone.
func TestDecayAssocWeights_NeverOverwritesAnotherRelation(t *testing.T) {
	now := time.Now()
	idle := now.Add(-200 * 24 * time.Hour)
	for _, c := range []struct {
		name  string
		edges []struct {
			rel  RelType
			w    float32
			last time.Time
		}
	}{
		{"both reach the floor in one pass, relates_to above", []struct {
			rel  RelType
			w    float32
			last time.Time
		}{{RelRelatesTo, 0.8, idle}, {RelContradicts, 0.7998, idle}}},
		{"both reach the floor in one pass, contradicts above", []struct {
			rel  RelType
			w    float32
			last time.Time
		}{{RelContradicts, 0.8, idle}, {RelRelatesTo, 0.7998, idle}}},
		{"a supersedes edge above (the refute's probe G: relates_to was erased)", []struct {
			rel  RelType
			w    float32
			last time.Time
		}{{RelSupersedes, 0.8, idle}, {RelRelatesTo, 0.7998, idle}}},
		{"a supersedes edge below (probe H: the supersedes edge was erased)", []struct {
			rel  RelType
			w    float32
			last time.Time
		}{{RelRelatesTo, 0.8, idle}, {RelSupersedes, 0.7998, idle}}},
		{"one is already at the floor", []struct {
			rel  RelType
			w    float32
			last time.Time
		}{{RelRelatesTo, floorOf(0.8), idle}, {RelContradicts, 0.06, idle}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			ps := openTestStore(t)
			ws := ps.VaultPrefix("orchard-ledger")
			a := writeStampFixture(t, ps, ws, "cider press cleaned weekly")
			b := writeStampFixture(t, ps, ws, "cider press cleaned after every pressing")
			for _, e := range c.edges {
				rawEdge(t, ps, ws, a, b, e.rel, e.w, e.last, 0.8, 1)
			}
			defaultDecay(t, ps, ws, now)
			got := pairEdges(t, ps, ws, a, b)
			for _, e := range c.edges {
				if _, ok := got[e.rel]; !ok {
					t.Errorf("decay erased the %d edge of the pair: left %v", e.rel, got)
				}
			}
		})
	}
}

// TestDecayAssocWeights_NeverOverwritesAnotherRelationsArchiveRow: the archive
// row is keyed by (src, dst) alone. A pair whose relates_to edge was archived
// earlier, and whose contradicts edge is now archive-eligible, kept only the
// later row. Now the contradicts edge is clamped to its floor instead, and the
// archived relates_to row is kept.
//
// RED on develop: the archive row is overwritten with the contradicts edge.
func TestDecayAssocWeights_NeverOverwritesAnotherRelationsArchiveRow(t *testing.T) {
	now := time.Now()
	ps := openTestStore(t)
	ws := ps.VaultPrefix("orchard-ledger")
	a := writeStampFixture(t, ps, ws, "graft union wrapped")
	b := writeStampFixture(t, ps, ws, "graft union wrapped with tape")
	rawArchived(t, ps, ws, a, b, RelRelatesTo, now.Add(-300*24*time.Hour), 0.8, 20)
	rawEdge(t, ps, ws, a, b, RelContradicts, 0.8, now.Add(-200*24*time.Hour), 0.8, 20)

	defaultDecay(t, ps, ws, now)

	if rel, ok := archivedRel(t, ps, ws, a, b); !ok || rel != RelRelatesTo {
		t.Errorf("the archived relates_to row = (%d, %v), want it kept", rel, ok)
	}
	if _, ok := pairEdges(t, ps, ws, a, b)[RelContradicts]; !ok {
		t.Errorf("the contradicts edge is gone: neither live nor in its own archive row")
	}
}

// TestRestoreArchivedEdges_LeavesASupersedesRowArchived: the lazy restore runs
// on recall and holds no lock that orders it against a supersedes link of the
// same target, so it cannot tell whether a rival took the target since the row
// was archived. Restoring then made a fork. An archived supersedes row is now
// restored only by the startup association repair, under the target's lock;
// recall leaves it where it is.
//
// RED on develop: the row is restored by a recall.
func TestRestoreArchivedEdges_LeavesASupersedesRowArchived(t *testing.T) {
	now := time.Now()
	ps := openTestStore(t)
	ws := ps.VaultPrefix("orchard-ledger")
	target := writeStampFixture(t, ps, ws, "apple thinning by hand")
	succ := writeStampFixture(t, ps, ws, "apple thinning with a spray")
	rawArchived(t, ps, ws, succ, target, RelSupersedes, now.Add(-10*24*time.Hour), 0.8, 13)

	restored, err := ps.RestoreArchivedEdges(context.Background(), ws, [16]byte(succ), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(restored) != 0 || len(pairEdges(t, ps, ws, succ, target)) != 0 {
		t.Errorf("recall restored an archived supersedes row: restored %v, live %v", restored, pairEdges(t, ps, ws, succ, target))
	}
	if rel, ok := archivedRel(t, ps, ws, succ, target); !ok || rel != RelSupersedes {
		t.Errorf("the archived supersedes row is gone")
	}
}

// TestRestoreArchivedEdges_NeverOverwritesAnotherRelation: a restore writes at
// peak*0.25. A live edge of another relation already at that key used to be
// overwritten; the row now stays archived and the live edge is kept.
//
// RED on develop: the live contradicts edge is replaced.
func TestRestoreArchivedEdges_NeverOverwritesAnotherRelation(t *testing.T) {
	now := time.Now()
	ps := openTestStore(t)
	ws := ps.VaultPrefix("orchard-ledger")
	a := writeStampFixture(t, ps, ws, "bird netting stored dry")
	b := writeStampFixture(t, ps, ws, "bird netting stored folded")
	rawArchived(t, ps, ws, a, b, RelRelatesTo, now.Add(-10*24*time.Hour), 0.8, 13)
	rawEdge(t, ps, ws, a, b, RelContradicts, 0.8*restoreWeightFactor, now, 0.2, 1)

	if _, err := ps.RestoreArchivedEdges(context.Background(), ws, [16]byte(a), 10); err != nil {
		t.Fatal(err)
	}
	if got := pairEdges(t, ps, ws, a, b); len(got) != 1 || got[RelContradicts] == 0 {
		t.Errorf("live edges after the restore = %v, want the contradicts edge kept", got)
	}
	if rel, ok := archivedRel(t, ps, ws, a, b); !ok || rel != RelRelatesTo {
		t.Errorf("the archived relates_to row = (%d, %v), want it kept archived", rel, ok)
	}
}

// TestGCArchivedEdges_KeepsSupersedesRows: archive GC reaps rows that are
// weak, rarely co-activated and three years idle. A supersedes row is a
// declaration, not a weak association: reaping it would erase the claim for
// good and leave whichever rival took the target as the only successor. The
// control row (relates_to, same values) is still reaped.
//
// RED on develop: the supersedes row is reaped too.
func TestGCArchivedEdges_KeepsSupersedesRows(t *testing.T) {
	old := time.Now().Add(-4 * 365 * 24 * time.Hour)
	ps := openTestStore(t)
	ws := ps.VaultPrefix("orchard-ledger")
	a := writeStampFixture(t, ps, ws, "pear blossom frost alarm")
	b := writeStampFixture(t, ps, ws, "pear blossom frost alarm at two degrees")
	c := writeStampFixture(t, ps, ws, "pear blossom frost alarm siren")
	rawArchived(t, ps, ws, b, a, RelSupersedes, old, 0.05, 1)
	rawArchived(t, ps, ws, c, a, RelRelatesTo, old, 0.05, 1)

	if _, err := ps.GCArchivedEdges(context.Background(), ws); err != nil {
		t.Fatal(err)
	}
	if _, ok := archivedRel(t, ps, ws, b, a); !ok {
		t.Errorf("GC reaped an archived supersedes row")
	}
	if _, ok := archivedRel(t, ps, ws, c, a); ok {
		t.Errorf("control: GC kept a relates_to row it should reap")
	}
}

// TestDecayAssocWeights_HoldsThePairGuardThroughCommit: between decay's
// re-check of a destination key and its commit, a Link of another relation
// onto that key must wait, and then be refused by its own STO-15 check, never
// land first and be overwritten. The seam fires inside that window and starts
// the Link.
//
// RED without the lock held through the commit: the Link lands inside the
// window and decay's write erases it, or its edge erases decay's.
func TestDecayAssocWeights_HoldsThePairGuardThroughCommit(t *testing.T) {
	now := time.Now()
	ps := openTestStore(t)
	ws := ps.VaultPrefix("orchard-ledger")
	a := writeStampFixture(t, ps, ws, "irrigation runs at dawn")
	b := writeStampFixture(t, ps, ws, "irrigation runs at dusk")
	rawEdge(t, ps, ws, a, b, RelRelatesTo, 0.8, now.Add(-200*24*time.Hour), 0.8, 1)
	floor := floorOf(0.8)

	linked := make(chan error, 1)
	waited := false
	decayPreCommitHook = func() {
		go func() {
			linked <- ps.WriteAssociation(context.Background(), ws, a, b, &Association{TargetID: b, RelType: RelContradicts, Weight: floor, Confidence: 1, CreatedAt: now})
		}()
		select {
		case err := <-linked:
			t.Errorf("a Link onto the destination did not wait for decay's commit (err %v)", err)
			linked <- err
		case <-time.After(50 * time.Millisecond):
			waited = true
		}
	}
	defer func() { decayPreCommitHook = nil }()
	defaultDecay(t, ps, ws, now)
	decayPreCommitHook = nil

	err := <-linked
	if !waited {
		t.Fatal("the decay commit window was never reached")
	}
	got := pairEdges(t, ps, ws, a, b)
	if err == nil {
		t.Errorf("the Link onto decay's destination succeeded: edges %v", got)
	} else if !errors.Is(err, ErrAssocRelTypeCollision) {
		t.Errorf("the Link's error = %v, want ErrAssocRelTypeCollision", err)
	}
	if _, ok := got[RelRelatesTo]; !ok {
		t.Errorf("the decayed relates_to edge is gone: %v", got)
	}
}
