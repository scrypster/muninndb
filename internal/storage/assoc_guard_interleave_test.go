package storage

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/storage/keys"
)

// The guards behind "decay spares supersedes edges and never writes over
// another relation", one test each. Every test here fails with its guard
// removed. The concurrent ones land the other writer at a test seam inside the
// window the guard closes, never behind a sleep: when the writer under test
// holds the pair's stripe the other writer runs on a goroutine and waits,
// and when it does not, the other writer runs inline, inside the window.
//
// Fixtures are invented (an orchard).

type liveKey struct {
	w   float32
	rel RelType
}

// liveKeys lists every live forward key from src to dst, duplicates included
// (pairEdges folds them by relation).
func liveKeys(t *testing.T, ps *PebbleStore, ws [8]byte, src, dst ULID) []liveKey {
	t.Helper()
	it, err := PrefixIterator(ps.db, keys.AssocFwdPrefixForID(ws, [16]byte(src)))
	if err != nil {
		t.Fatal(err)
	}
	defer it.Close()
	var out []liveKey
	for it.First(); it.Valid(); it.Next() {
		k := it.Key()
		if len(k) != 45 || !bytes.Equal(k[29:45], dst[:]) {
			continue
		}
		var wc [4]byte
		copy(wc[:], k[25:29])
		rel, _, _, _, _, _, _ := decodeAssocValue(it.Value())
		out = append(out, liveKey{keys.WeightFromComplement(wc), rel})
	}
	return out
}

// interleave runs other at the guard seam for (op, src, dst), once: inline
// when the pair's stripe is free (the window is open), on a goroutine when it
// is held (other waits for the commit). wait joins the goroutine.
type interleave struct {
	once sync.Once
	wg   sync.WaitGroup
	err  error
	ran  bool
}

func (il *interleave) install(t *testing.T, ps *PebbleStore, op string, src, dst ULID, other func() error) (restore func()) {
	t.Helper()
	return SetAssocGuardHookForTesting(func(gotOp string, ws [8]byte, s, d ULID) {
		if gotOp != op || s != src || d != dst {
			return
		}
		il.once.Do(func() {
			il.ran = true
			if !ps.AssocPairStripeHeldForTesting(ws, s, d) {
				il.err = other()
				return
			}
			il.wg.Add(1)
			go func() { defer il.wg.Done(); il.err = other() }()
		})
	})
}

func (il *interleave) wait(t *testing.T) {
	t.Helper()
	il.wg.Wait()
	if !il.ran {
		t.Fatal("the seam never fired: the test is not exercising the window")
	}
}

// TestUpdateAssocWeightBatch_NeverMovesOntoAnotherRelation (the refute's probe
// I, at the store): a supersedes edge declared at 1.0 and a relates_to edge at
// 0.95 on the same directed pair. The Hebbian worker reads the pair's weight
// index (0.95, the relates_to edge) and moves that edge up to 1.0, the key the
// supersedes edge holds. The move used to Set over it: the supersedes edge
// was erased, the target looked free, and a rival was linked.
//
// RED without the destination check in UpdateAssocWeightBatch and
// UpdateAssocWeight: the supersedes edge is gone.
func TestUpdateAssocWeightBatch_NeverMovesOntoAnotherRelation(t *testing.T) {
	for _, single := range []bool{false, true} {
		name := "batch"
		if single {
			name = "single"
		}
		t.Run(name, func(t *testing.T) {
			ps := openTestStore(t)
			ws := ps.VaultPrefix("guard-orchard")
			older := writeStampFixture(t, ps, ws, "cherries are netted in May")
			newer := writeStampFixture(t, ps, ws, "cherries are netted in June")
			now := time.Now()
			rawEdge(t, ps, ws, older, newer, RelSupersedes, 1.0, now, 1.0, 0)
			rawEdge(t, ps, ws, older, newer, RelRelatesTo, 0.95, now, 0.95, 4)

			var err error
			if single {
				err = ps.UpdateAssocWeight(context.Background(), ws, older, newer, 1.0, 1)
			} else {
				err = ps.UpdateAssocWeightBatch(context.Background(), []AssocWeightUpdate{{WS: ws, Src: older, Dst: newer, Weight: 1.0, CountDelta: 1}})
			}
			if !errors.Is(err, ErrAssocRelTypeCollision) {
				t.Fatalf("the move onto the supersedes key: err = %v, want ErrAssocRelTypeCollision", err)
			}
			if !single {
				var skip *AssocBatchSkipError
				if !errors.As(err, &skip) {
					t.Fatalf("err = %T, want *AssocBatchSkipError so the worker suppresses the callback", err)
				}
				if got := skip.CollisionSkips(); len(got) != 1 || got[0] != 0 {
					t.Errorf("CollisionSkips() = %v, want [0]", got)
				}
			}
			got := pairEdges(t, ps, ws, older, newer)
			if got[RelSupersedes] != 1.0 || got[RelRelatesTo] != 0.95 {
				t.Errorf("edges after the refused move = %v, want supersedes at 1.0 and relates_to at 0.95 (it keeps its weight)", got)
			}
			if s, _ := ps.SupersedersOf(context.Background(), ws, newer); len(s) != 1 || s[0] != older {
				t.Errorf("SupersedersOf(newer) = %v, want [older]", s)
			}
		})
	}
}

// TestUpdateAssocWeightBatch_HoldsThePairGuardThroughCommit: the destination
// check is only sound if nothing can land on the destination between it and
// the commit. Two writers can.
//
//   - A Link of another relation at the weight the edge moves to: its own
//     check sees the key free (the move has not committed), it commits, and
//     the move then Sets over it.
//   - A decay chunk: it re-checks the edge at its old key (still there), moves
//     it, and the weight update then deletes the old key (gone) and Sets its
//     own: two live keys for one edge.
//
// RED without the lockMany in UpdateAssocWeightBatch: the Link returns nil and
// its edge is gone, and decay leaves two live keys.
func TestUpdateAssocWeightBatch_HoldsThePairGuardThroughCommit(t *testing.T) {
	t.Run("a link of another relation at the destination", func(t *testing.T) {
		ps := openTestStore(t)
		ws := ps.VaultPrefix("guard-orchard")
		a := writeStampFixture(t, ps, ws, "plums are thinned by hand")
		b := writeStampFixture(t, ps, ws, "plums are thinned with a pole")
		rawEdge(t, ps, ws, a, b, RelRelatesTo, 0.8, time.Now(), 0.8, 2)

		il := &interleave{}
		defer il.install(t, ps, "weight-update", a, b, func() error {
			return ps.WriteAssociation(context.Background(), ws, a, b, &Association{TargetID: b, RelType: RelSupersedes, Weight: 0.81, Confidence: 1, CreatedAt: time.Now()})
		})()
		if err := ps.UpdateAssocWeightBatch(context.Background(), []AssocWeightUpdate{{WS: ws, Src: a, Dst: b, Weight: 0.81, CountDelta: 1}}); err != nil {
			t.Fatalf("weight update: %v", err)
		}
		il.wait(t)

		got := pairEdges(t, ps, ws, a, b)
		if il.err == nil {
			if _, ok := got[RelSupersedes]; !ok {
				t.Errorf("the Link returned nil but its supersedes edge is gone (edges %v): the weight update wrote over it", got)
			}
		} else if !errors.Is(il.err, ErrAssocRelTypeCollision) {
			t.Errorf("the Link failed with %v, want ErrAssocRelTypeCollision", il.err)
		}
		if got[RelRelatesTo] != 0.81 {
			t.Errorf("relates_to = %v, want moved to 0.81", got[RelRelatesTo])
		}
	})

	t.Run("a decay chunk", func(t *testing.T) {
		ps := openTestStore(t)
		ws := ps.VaultPrefix("guard-orchard")
		a := writeStampFixture(t, ps, ws, "figs ripen on old wood")
		b := writeStampFixture(t, ps, ws, "figs ripen on new wood")
		// Idle 200 days with one co-activation: decay clamps it to its floor.
		rawEdge(t, ps, ws, a, b, RelRelatesTo, 0.8, time.Now().Add(-200*24*time.Hour), 0.8, 1)

		il := &interleave{}
		defer il.install(t, ps, "weight-update", a, b, func() error {
			_, err := ps.DecayAssocWeights(context.Background(), ws, 30*24*time.Hour, 0.05, 0.05)
			return err
		})()
		if err := ps.UpdateAssocWeightBatch(context.Background(), []AssocWeightUpdate{{WS: ws, Src: a, Dst: b, Weight: 0.81, CountDelta: 1}}); err != nil {
			t.Fatalf("weight update: %v", err)
		}
		il.wait(t)
		if il.err != nil {
			t.Fatalf("decay: %v", il.err)
		}
		if got := liveKeys(t, ps, ws, a, b); len(got) != 1 {
			t.Errorf("live a->b keys = %v, want exactly one", got)
		}
	})
}

// TestDecayAssocWeights_SkipsAnEdgeThatMovedSinceTheScan: decay scans a
// snapshot, so an edge can move between the scan and the chunk's commit. The
// re-check drops it. Without the re-check the chunk deletes the old key (gone
// already) and Sets the decayed weight from the stale value: two live keys
// for one edge, and the weight index pointing at the stale one.
//
// RED without the "edge moved or went since the scan" check in
// recheckDecayMovesLocked.
func TestDecayAssocWeights_SkipsAnEdgeThatMovedSinceTheScan(t *testing.T) {
	ps := openTestStore(t)
	ws := ps.VaultPrefix("guard-orchard")
	a := writeStampFixture(t, ps, ws, "walnuts fall in October")
	b := writeStampFixture(t, ps, ws, "walnuts are shaken down in October")
	rawEdge(t, ps, ws, a, b, RelRelatesTo, 0.8, time.Now().Add(-200*24*time.Hour), 0.8, 1)

	var once sync.Once
	decayPreLockHook = func() {
		once.Do(func() {
			if err := ps.UpdateAssocWeightBatch(context.Background(), []AssocWeightUpdate{{WS: ws, Src: a, Dst: b, Weight: 0.81, CountDelta: 1}}); err != nil {
				t.Errorf("weight update: %v", err)
			}
		})
	}
	defer func() { decayPreLockHook = nil }()
	if _, err := ps.DecayAssocWeights(context.Background(), ws, 30*24*time.Hour, 0.05, 0.05); err != nil {
		t.Fatal(err)
	}
	decayPreLockHook = nil

	got := liveKeys(t, ps, ws, a, b)
	if len(got) != 1 || got[0].w != 0.81 {
		t.Errorf("live a->b keys = %v, want only the moved edge at 0.81", got)
	}
	if w, _ := ps.GetAssocWeight(context.Background(), ws, a, b); w != 0.81 {
		t.Errorf("weight index = %v, want 0.81", w)
	}
}

// TestDecayAssocWeights_TwoArchivesInOneChunkNeverShareARow: the 0x25 key is
// (src, dst) with no relation in it. Two relations on one pair that both
// qualify for the archive in one pass would write the same row, and the later
// Set would erase the first relation from the store entirely (neither live nor
// archived). The second is clamped to its floor instead.
//
// RED without pendingArch in recheckDecayMovesLocked (the check only sees
// committed rows, and neither is committed yet).
func TestDecayAssocWeights_TwoArchivesInOneChunkNeverShareARow(t *testing.T) {
	ps := openTestStore(t)
	ws := ps.VaultPrefix("guard-orchard")
	a := writeStampFixture(t, ps, ws, "frost damages open blossom")
	b := writeStampFixture(t, ps, ws, "late frost costs the pear crop")
	idle := time.Now().Add(-130 * 24 * time.Hour)
	// Both archive-eligible: ceiling below the min weight, score 0.8*13/130.
	rawEdge(t, ps, ws, a, b, RelCauses, 0.3, idle, 0.8, 13)
	rawEdge(t, ps, ws, a, b, RelDependsOn, 0.2, idle, 0.8, 13)

	defaultDecay(t, ps, ws, time.Now())

	live := pairEdges(t, ps, ws, a, b)
	arch, archived := archivedRel(t, ps, ws, a, b)
	for _, rel := range []RelType{RelCauses, RelDependsOn} {
		_, isLive := live[rel]
		if !isLive && !(archived && arch == rel) {
			t.Errorf("relation %v is neither live (%v) nor the archived row (%v, %v): one archive erased the other", rel, live, arch, archived)
		}
	}
}

// TestDecayAssocWeights_NeverOverwritesAMirroredRowOfAnotherRelation: a
// symmetric edge is archived under (src, dst) and mirrored under (dst, src).
// The mirrored key can hold another edge's row, here a supersedes declaration
// of dst onto src that an older binary archived. Archiving the symmetric edge
// would replace it. The edge is clamped to its floor instead.
//
// RED without the mirrored-row check in recheckDecayMovesLocked.
func TestDecayAssocWeights_NeverOverwritesAMirroredRowOfAnotherRelation(t *testing.T) {
	ps := openTestStore(t)
	ws := ps.VaultPrefix("guard-orchard")
	a := writeStampFixture(t, ps, ws, "hazels are coppiced every seven years")
	b := writeStampFixture(t, ps, ws, "hazels are coppiced every eight years")
	idle := time.Now().Add(-130 * 24 * time.Hour)
	rawEdge(t, ps, ws, a, b, RelRelatesTo, 0.3, idle, 0.8, 13)
	rawArchived(t, ps, ws, b, a, RelSupersedes, idle, 0.8, 13)

	defaultDecay(t, ps, ws, time.Now())

	if rel, ok := archivedRel(t, ps, ws, b, a); !ok || rel != RelSupersedes {
		t.Errorf("the mirrored row = (%v, %v), want the archived supersedes declaration untouched", rel, ok)
	}
	if got := pairEdges(t, ps, ws, a, b)[RelRelatesTo]; got != floorOf(0.8) {
		t.Errorf("relates_to = %v, want clamped to its floor %v instead of archived", got, floorOf(0.8))
	}
}

// TestRestoreArchivedEdges_HoldsThePairGuardThroughCommit: the lazy restore
// checks its restore key under the pair's guard lock and must hold it through
// the commit. Otherwise a Link of another relation at the restore weight
// passes its own check (the restore has not committed), commits, and the
// restore then Sets over it.
//
// RED without the lockMany in RestoreArchivedEdges.
func TestRestoreArchivedEdges_HoldsThePairGuardThroughCommit(t *testing.T) {
	ps := openTestStore(t)
	ws := ps.VaultPrefix("guard-orchard")
	s := writeStampFixture(t, ps, ws, "rootstock sets the tree's size")
	d := writeStampFixture(t, ps, ws, "dwarfing rootstock needs staking")
	rawArchived(t, ps, ws, s, d, RelCauses, time.Now().Add(-10*24*time.Hour), 0.8, 13)
	restoreW := float32(0.8) * restoreWeightFactor

	il := &interleave{}
	defer il.install(t, ps, "lazy-restore", s, d, func() error {
		return ps.WriteAssociation(context.Background(), ws, s, d, &Association{TargetID: d, RelType: RelDependsOn, Weight: restoreW, Confidence: 1, CreatedAt: time.Now()})
	})()
	if _, err := ps.RestoreArchivedEdges(context.Background(), ws, [16]byte(s), 10); err != nil {
		t.Fatal(err)
	}
	il.wait(t)

	got := pairEdges(t, ps, ws, s, d)
	if il.err == nil {
		if _, ok := got[RelDependsOn]; !ok {
			t.Errorf("the Link returned nil but its edge is gone (edges %v): the restore wrote over it", got)
		}
	} else if !errors.Is(il.err, ErrAssocRelTypeCollision) {
		t.Errorf("the Link failed with %v, want ErrAssocRelTypeCollision", il.err)
	}
}

// TestRestoreArchivedEdges_LeavesAMirroredRowOfAnotherRelation (the refute's
// 8a): restoring a symmetric edge also retires its mirrored row, but the
// mirrored key (dst, src) can hold another edge's row. An older binary's decay
// archived across relations, so a store can hold relates_to under (S, D) and a
// supersedes declaration of D onto S under (D, S). A recall touching S used to
// delete that declaration.
//
// RED without the relation check on the mirrored row.
func TestRestoreArchivedEdges_LeavesAMirroredRowOfAnotherRelation(t *testing.T) {
	ps := openTestStore(t)
	ws := ps.VaultPrefix("guard-orchard")
	s := writeStampFixture(t, ps, ws, "walnuts hulled by hand")
	d := writeStampFixture(t, ps, ws, "walnuts hulled with the drum")
	rawArchived(t, ps, ws, s, d, RelRelatesTo, time.Now().Add(-10*24*time.Hour), 0.8, 13)
	rawArchived(t, ps, ws, d, s, RelSupersedes, time.Now().Add(-130*24*time.Hour), 0.8, 13)

	got, err := ps.RestoreArchivedEdges(context.Background(), ws, [16]byte(s), 10)
	if err != nil || len(got) != 1 {
		t.Fatalf("restore: %v, %v, want the relates_to edge back", got, err)
	}
	if rel, ok := archivedRel(t, ps, ws, d, s); !ok || rel != RelSupersedes {
		t.Errorf("the (D, S) row = (%v, %v), want the archived supersedes declaration kept", rel, ok)
	}

	// The same relation on the mirrored key is the restored edge's own
	// sibling, and it still goes.
	ps2 := openTestStore(t)
	ws2 := ps2.VaultPrefix("guard-orchard")
	s2 := writeStampFixture(t, ps2, ws2, "chestnuts roast in the shell")
	d2 := writeStampFixture(t, ps2, ws2, "chestnuts are scored before roasting")
	rawArchived(t, ps2, ws2, s2, d2, RelRelatesTo, time.Now().Add(-10*24*time.Hour), 0.8, 13)
	rawArchived(t, ps2, ws2, d2, s2, RelRelatesTo, time.Now().Add(-10*24*time.Hour), 0.8, 13)
	if _, err := ps2.RestoreArchivedEdges(context.Background(), ws2, [16]byte(s2), 10); err != nil {
		t.Fatal(err)
	}
	if _, ok := archivedRel(t, ps2, ws2, d2, s2); ok {
		t.Error("the restored edge's own mirrored row is still archived")
	}
}

// archivedSupersedesFixture: an older binary's decay archived a supersedes
// edge S -> T declared at 0.8.
func archivedSupersedesFixture(t *testing.T) (ps *PebbleStore, ws [8]byte, s, tg ULID) {
	t.Helper()
	ps = openTestStore(t)
	ws = ps.VaultPrefix("guard-orchard")
	tg = writeStampFixture(t, ps, ws, "quince picked in October")
	s = writeStampFixture(t, ps, ws, "quince picked when it turns gold")
	rawArchived(t, ps, ws, s, tg, RelSupersedes, time.Now().Add(-125*24*time.Hour), 0.8, 13)
	return ps, ws, s, tg
}

// TestRestoreArchivedSupersedes_HoldsThePairGuardThroughCommit: as the lazy
// restore, for the one-off supersedes restore: a Link of another relation at
// the declared weight must not land between the collision check and the write.
//
// RED without the pair lock in RestoreArchivedSupersedes.
func TestRestoreArchivedSupersedes_HoldsThePairGuardThroughCommit(t *testing.T) {
	ps, ws, s, tg := archivedSupersedesFixture(t)
	il := &interleave{}
	defer il.install(t, ps, "supersedes-restore", s, tg, func() error {
		return ps.WriteAssociation(context.Background(), ws, s, tg, &Association{TargetID: tg, RelType: RelRelatesTo, Weight: 0.8, Confidence: 1, CreatedAt: time.Now()})
	})()
	out, _, err := ps.RestoreArchivedSupersedes(context.Background(), ws, s, tg)
	if err != nil {
		t.Fatal(err)
	}
	il.wait(t)
	if out != SupersedesRestored {
		t.Fatalf("outcome = %v, want restored", out)
	}
	got := pairEdges(t, ps, ws, s, tg)
	if il.err == nil {
		if _, ok := got[RelRelatesTo]; !ok {
			t.Errorf("the Link returned nil but its edge is gone (edges %v): the restore wrote over it", got)
		}
	} else if !errors.Is(il.err, ErrAssocRelTypeCollision) {
		t.Errorf("the Link failed with %v, want ErrAssocRelTypeCollision", il.err)
	}
}

// TestRestoreArchivedSupersedes_LeavesARowAnotherRelationsKeyHolds: the key a
// supersedes edge goes back at (its declared weight) holds a live edge of
// another relation. Restoring would erase it, so the row stays archived.
//
// RED without the collision check in RestoreArchivedSupersedes.
func TestRestoreArchivedSupersedes_LeavesARowAnotherRelationsKeyHolds(t *testing.T) {
	ps, ws, s, tg := archivedSupersedesFixture(t)
	if err := ps.WriteAssociation(context.Background(), ws, s, tg, &Association{TargetID: tg, RelType: RelRelatesTo, Weight: 0.8, Confidence: 1, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	out, _, err := ps.RestoreArchivedSupersedes(context.Background(), ws, s, tg)
	if err != nil {
		t.Fatal(err)
	}
	if out != SupersedesLeftCollision {
		t.Errorf("outcome = %v, want left (collision)", out)
	}
	if got := pairEdges(t, ps, ws, s, tg); got[RelRelatesTo] != 0.8 {
		t.Errorf("edges = %v, want relates_to still at 0.8", got)
	}
	if rel, ok := archivedRel(t, ps, ws, s, tg); !ok || rel != RelSupersedes {
		t.Errorf("the row = (%v, %v), want it still archived", rel, ok)
	}
}

// TestRestoreArchivedSupersedes_LeavesARowWhoseEndpointIsGone: a memory hard
// deleted by a binary whose delete did not cascade 0x25 leaves its archive
// rows behind. Restoring one would write a live edge onto a memory that does
// not exist (a dangling edge, STO-12).
//
// RED without the endpoint check in RestoreArchivedSupersedes.
func TestRestoreArchivedSupersedes_LeavesARowWhoseEndpointIsGone(t *testing.T) {
	for _, goneIsSource := range []bool{false, true} {
		ps, ws, s, tg := archivedSupersedesFixture(t)
		gone := tg
		if goneIsSource {
			gone = s
		}
		if err := ps.db.Delete(keys.EngramKey(ws, [16]byte(gone)), nil); err != nil {
			t.Fatal(err)
		}
		out, _, err := ps.RestoreArchivedSupersedes(context.Background(), ws, s, tg)
		if err != nil {
			t.Fatal(err)
		}
		if out != SupersedesLeftEndpointGone {
			t.Errorf("source gone %v: outcome = %v, want left (endpoint gone)", goneIsSource, out)
		}
		if got := liveKeys(t, ps, ws, s, tg); len(got) != 0 {
			t.Errorf("source gone %v: live S->T keys = %v, want none", goneIsSource, got)
		}
	}
}

// TestRestoreArchivedSupersedes_SourceAlreadyLiveDropsTheRow (the refute's
// 4a): the archived successor was linked onto the target again since, at
// another weight (Hebbian had raised the archived peak to 0.8111, the new link
// used 0.8). The declaration is in effect. Restoring it wrote a second live
// supersedes key for one declaration. The row is dropped and nothing written.
//
// RED before the fix: two live S->T keys, [{0.8111 4} {0.8 4}].
func TestRestoreArchivedSupersedes_SourceAlreadyLiveDropsTheRow(t *testing.T) {
	ps := openTestStore(t)
	ws := ps.VaultPrefix("guard-orchard")
	tg := writeStampFixture(t, ps, ws, "medlars are bletted before eating")
	s := writeStampFixture(t, ps, ws, "medlars are bletted for three weeks")
	rawArchived(t, ps, ws, s, tg, RelSupersedes, time.Now().Add(-125*24*time.Hour), 0.8111, 13)
	if err := ps.WriteAssociation(context.Background(), ws, s, tg, &Association{TargetID: tg, RelType: RelSupersedes, Weight: 0.8, Confidence: 1, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	out, holders, err := ps.RestoreArchivedSupersedes(context.Background(), ws, s, tg)
	if err != nil {
		t.Fatal(err)
	}
	if out != SupersedesAlreadyLive || len(holders) != 0 {
		t.Errorf("outcome = %v, holders %v, want already live with no other successor", out, holders)
	}
	if got := liveKeys(t, ps, ws, s, tg); len(got) != 1 || got[0] != (liveKey{0.8, RelSupersedes}) {
		t.Errorf("live S->T keys = %v, want only the live link at 0.8", got)
	}
	if _, ok := archivedRel(t, ps, ws, s, tg); ok {
		t.Error("the archive row is still there: an in-effect declaration keeps a stale copy")
	}
}
