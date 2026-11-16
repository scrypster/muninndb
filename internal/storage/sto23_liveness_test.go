package storage

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/scrypster/muninndb/internal/storage/erf"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

// rawState returns the lifecycle state byte held by a record value, or -1.
func rawState(t *testing.T, s *PebbleStore, key []byte) int {
	t.Helper()
	v, err := Get(s.db, key)
	if err != nil {
		t.Fatal(err)
	}
	if len(v) <= erf.OffsetState {
		return -1
	}
	return int(v[erf.OffsetState])
}

// tearMeta overwrites id's 0x02 metadata record so its state byte says st,
// leaving the 0x01 engram record alone — a copy torn between its passes.
func tearMeta(t *testing.T, s *PebbleStore, ws [8]byte, id ULID, st LifecycleState) {
	t.Helper()
	k := keys.MetaKey(ws, [16]byte(id))
	v, err := Get(s.db, k)
	if err != nil || len(v) <= erf.OffsetState {
		t.Fatalf("read meta: %v", err)
	}
	v = append([]byte(nil), v...)
	v[erf.OffsetState] = byte(st)
	if err := s.db.Set(k, v, nil); err != nil {
		t.Fatal(err)
	}
}

func writeLive(t *testing.T, s *PebbleStore, ws [8]byte, concept string) ULID {
	t.Helper()
	id, err := s.WriteEngram(context.Background(), ws, &Engram{Concept: concept, Content: concept + " noted on the ridge trail", Confidence: 0.8, Stability: 30})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// copyTornByConcurrentForget runs a vault copy (clone or merge) and forgets X
// on the SOURCE from inside the copy's progress hook, i.e. after the 0x01
// pass and before the 0x02 pass. The copy must still agree with itself, and
// its retired set must agree with its engram records.
func copyTornByConcurrentForget(t *testing.T, merge bool) {
	s := openTestStore(t)
	ctx := context.Background()
	src, dst := s.VaultPrefix("trail-src"), s.VaultPrefix("trail-dst")
	x := writeLive(t, s, src, "cairn X rebuilt")
	for i := 0; i < 5; i++ {
		writeLive(t, s, src, fmt.Sprintf("waymark %d", i))
	}
	var once atomic.Bool
	hook := func(int64) {
		if once.CompareAndSwap(false, true) {
			if err := s.SoftDelete(ctx, src, x); err != nil {
				t.Errorf("SoftDelete: %v", err)
			}
		}
	}
	var err error
	if merge {
		_, err = s.MergeVaultData(ctx, src, dst, hook)
	} else {
		_, err = s.CloneVaultData(ctx, src, dst, hook)
	}
	if err != nil {
		t.Fatal(err)
	}
	if !once.Load() {
		t.Fatal("fixture: the copy hook never ran")
	}
	eng := rawState(t, s, keys.EngramKey(dst, [16]byte(x)))
	meta := rawState(t, s, keys.MetaKey(dst, [16]byte(x)))
	view, err := s.RetiredIDs(ctx, dst)
	if err != nil {
		t.Fatal(err)
	}
	inSet := view.Contains([16]byte(x))
	engRetired := isRetiredState(LifecycleState(eng))
	if eng != meta || inSet != engRetired {
		t.Fatalf("torn copy of X: 0x01 state=%d 0x02 state=%d inRetiredSet=%v (the copy must read one moment, and the set must follow 0x01)", eng, meta, inSet)
	}
}

func TestSTO23_CloneTornByConcurrentForget(t *testing.T) { copyTornByConcurrentForget(t, false) }
func TestSTO23_MergeTornByConcurrentForget(t *testing.T) { copyTornByConcurrentForget(t, true) }

// The lazy build derives liveness from the 0x01 engram record, counts a 0x02
// disagreement, and never lets a torn metadata record hide a live memory.
func TestSTO23_RetiredBuildTrustsEngramRecordOverTornMeta(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	ws := s.VaultPrefix("torn-build")
	live := writeLive(t, s, ws, "footbridge deck")
	gone := writeLive(t, s, ws, "old stile")
	if err := s.SoftDelete(ctx, ws, gone); err != nil {
		t.Fatal(err)
	}
	tearMeta(t, s, ws, live, StateSoftDeleted) // meta says retired, record says live
	tearMeta(t, s, ws, gone, StateActive)      // meta says live, record says retired
	s.ResetAllRetired()

	before := s.RetiredLivenessDisagreements()
	view, err := s.RetiredIDs(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	if view.Contains([16]byte(live)) {
		t.Fatal("retired set hides a memory whose engram record is live (built from the torn 0x02 record)")
	}
	if !view.Contains([16]byte(gone)) {
		t.Fatal("retired set misses a memory whose engram record is retired")
	}
	if got := s.RetiredLivenessDisagreements() - before; got != 2 {
		t.Fatalf("disagreements counted = %d, want 2", got)
	}
}

// The per-commit re-derive reads the same source as the build: a committed
// batch that touches only a (torn) 0x02 record must not retire a memory whose
// engram record is live.
func TestSTO23_RetiredRederiveTrustsEngramRecord(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	ws := s.VaultPrefix("torn-rederive")
	live := writeLive(t, s, ws, "puncheon steps")
	if _, err := s.RetiredIDs(ctx, ws); err != nil {
		t.Fatal(err)
	}
	k := keys.MetaKey(ws, [16]byte(live))
	v, _ := Get(s.db, k)
	v = append([]byte(nil), v...)
	v[erf.OffsetState] = byte(StateSoftDeleted)
	b := s.db.NewBatch()
	b.Set(k, v, nil)
	if err := s.commitBatch(b, nil); err != nil {
		t.Fatal(err)
	}
	b.Close()
	view, _ := s.RetiredIDs(ctx, ws)
	if view.Contains([16]byte(live)) {
		t.Fatal("re-derive retired a memory whose engram record is live (read the 0x02 record)")
	}
}

// ---------------------------------------------------------------------------
// STO-22 guards that previously had no test pinning them.
// ---------------------------------------------------------------------------

// NewSnapshotContext must take the fill token BEFORE pinning the snapshot. A
// write landing between the two is then either visible to the snapshot (token
// first) or — if the order is reversed — invisible to the snapshot yet below
// the token, and its stale fill would be accepted.
func TestSTO22_SnapshotContextTakesTokenBeforePin(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	ws, id := writeProbe(t, s, "token-before-pin", 0.20)
	var once atomic.Bool
	fn := func() {
		if once.CompareAndSwap(false, true) {
			if err := s.UpdateRelevance(ctx, ws, id, 0.90, 30); err != nil {
				t.Errorf("UpdateRelevance: %v", err)
			}
		}
	}
	snapshotPinHook.Store(&fn)
	sctx, snap := s.NewSnapshotContext(ctx)
	snapshotPinHook.Store(nil)
	defer snap.Close()
	if !once.Load() {
		t.Fatal("fixture: pin hook did not run")
	}
	if _, err := s.GetEngrams(sctx, ws, []ULID{id}); err != nil {
		t.Fatal(err)
	}
	e, err := s.GetEngram(ctx, ws, id)
	if err != nil {
		t.Fatal(err)
	}
	if e.Relevance != 0.90 {
		t.Fatalf("stale fill survived: relevance %v after a committed 0.90 (token taken after the pin?)", e.Relevance)
	}
}

// A follower learns of a vault clear only through the range tombstone in the
// leader's batch: InvalidateCommittedRepr must drop the cleared vault's
// cached entries.
func TestSTO22_RangeDeleteReprInvalidatesFollowerCache(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	ws, id := writeProbe(t, s, "follower-clear", 0.5)
	if _, err := s.GetEngram(ctx, ws, id); err != nil { // cached
		t.Fatal(err)
	}
	next, _ := incrementWS(ws)
	b := s.db.NewBatch()
	for _, p := range []byte{0x01, 0x02} {
		lo := append([]byte{p}, ws[:]...)
		hi := append([]byte{p}, next[:]...)
		if err := b.DeleteRange(lo, hi, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.Commit(nil); err != nil { // the applier's raw commit
		t.Fatal(err)
	}
	s.InvalidateCommittedRepr(b.Repr())
	b.Close()
	if e, err := s.GetEngram(ctx, ws, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after a replicated vault clear the follower still serves the engram: %v %v", e, err)
	}
}

// A fill that started before ClearVault (token taken, value read) and lands
// after it must be rejected: clearing bumps the generations, not only the
// entries.
func TestSTO22_ClearVaultRejectsInFlightFill(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	ws, id := writeProbe(t, s, "clear-inflight", 0.5)
	tok := s.cache.guard.token()
	raw, _ := Get(s.db, keys.EngramKey(ws, [16]byte(id)))
	dec, err := erf.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClearVault(ctx, ws); err != nil {
		t.Fatal(err)
	}
	s.cache.fill(ws, id, fromERFEngram(dec), tok, true)
	if e, err := s.GetEngram(ctx, ws, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a pre-clear fill re-installed a cleared engram: %v %v", e, err)
	}
}

// The reverse-association cache is filled from snapshot-aware scans like the
// forward one, and must be guarded the same way.
func TestSTO22_SnapshotReverseAssociationFillDoesNotOverwriteNewerCommit(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	ws := s.VaultPrefix("stale-rev-assoc")
	src, dst := NewULID(), NewULID()
	seedEndpoints(t, s, ws, src, dst)
	if err := s.WriteAssociation(ctx, ws, src, dst, &Association{TargetID: dst, Weight: 0.2, RelType: RelRelatesTo, Confidence: 1}); err != nil {
		t.Fatal(err)
	}
	s.revAssocCache.Purge()
	snapCtx, snap := s.NewSnapshotContext(ctx)
	defer snap.Close()
	if err := s.UpdateAssocWeight(ctx, ws, src, dst, 0.9, 0); err != nil {
		t.Fatal(err)
	}
	old, err := s.rankingReverseEdges(snapCtx, ws, []ULID{dst}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(old[dst]) != 1 {
		t.Fatalf("fixture: reverse edge not ranking-visible: %+v", old[dst])
	}
	got, err := s.rankingReverseEdges(ctx, ws, []ULID{dst}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got[dst]) != 1 || got[dst][0].Weight < 0.89 {
		t.Fatalf("stale reverse-association fill: live read returned %+v after a committed weight of 0.9", got[dst])
	}
}

// Clearing one vault resets only that vault's retired set.
func TestSTO23_ClearVaultResetsOnlyItsOwnRetiredSet(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	a, b := s.VaultPrefix("clear-a"), s.VaultPrefix("clear-b")
	writeLive(t, s, a, "gate latch")
	writeLive(t, s, b, "gate hinge")
	for _, ws := range [][8]byte{a, b} {
		if _, err := s.RetiredIDs(ctx, ws); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.ClearVault(ctx, a); err != nil {
		t.Fatal(err)
	}
	v, ok := s.retired.Load(b)
	if !ok {
		t.Fatal("clearing vault a dropped vault b's retired set")
	}
	set := v.(*retiredSet)
	set.mu.RLock()
	built := set.built
	set.mu.RUnlock()
	if !built {
		t.Fatal("clearing vault a reset vault b's retired set")
	}
}
