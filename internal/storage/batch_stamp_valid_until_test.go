package storage

import (
	"bytes"
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/storage/keys"
)

// StoreBatch.StampValidUntil is the batch form of PebbleStore.StampValidUntil:
// the same one-field retirement a supersedes link applies, queued so it commits
// with the engram and the edge that justify it. These pin that it is the SAME
// effect as the direct stamp, and that it is atomic with its batch.
//
// Fixtures are invented (an orchard).

func writeStampFixture(t *testing.T, ps *PebbleStore, ws [8]byte, concept string) ULID {
	t.Helper()
	ctx := context.Background()
	id, err := ps.WriteEngram(ctx, ws, &Engram{
		Concept:    concept,
		Content:    concept + " — noted in the orchard ledger after the autumn pick",
		Summary:    "an orchard ledger entry",
		KeyPoints:  []string{"picked in autumn", "logged by row"},
		Tags:       []string{"orchard", "row:seven"},
		Confidence: 0.7,
		Relevance:  0.4,
		Stability:  21,
		Importance: 0.8,
		MemoryType: TypeDecision,
		TypeLabel:  "orchard_decision",
		Trust:      TrustVerified,
		Embedding:  []float32{0.25, -0.5, 0.75, 0.125},
	})
	if err != nil {
		t.Fatal(err)
	}
	// A committed access bump, so AccessCount/LastAccess are not at their
	// freshly-written values when the stamp reads the record.
	if err := ps.TouchAccess(ctx, ws, id); err != nil {
		t.Fatal(err)
	}
	return id
}

func stampProvenance(t *testing.T, ps *PebbleStore, ws [8]byte, id ULID) []string {
	t.Helper()
	ps.DrainProvenance()
	entries, err := ps.ProvenanceStore().Get(context.Background(), ws, [16]byte(id))
	if err != nil {
		t.Fatal(err)
	}
	var notes []string
	for _, e := range entries {
		if e.Operation == "stamp-valid-until" {
			notes = append(notes, e.AgentID+"|"+e.Note)
		}
	}
	return notes
}

// The batch stamp and the direct stamp must leave byte-for-field identical
// records: ValidUntil set, and NOTHING else moved — not the state, not
// UpdatedAt, not the access metadata, not the embedding dimension. (The batch
// path re-encodes the record where the direct one patches raw bytes, so this is
// a real property to pin, not a tautology.)
func TestBatchStampValidUntil_MatchesTheDirectStamp(t *testing.T) {
	ps := openTestStore(t)
	ctx := context.Background()
	ws := ps.VaultPrefix("orchard-ledger")
	direct := writeStampFixture(t, ps, ws, "russet row replanted")
	batched := writeStampFixture(t, ps, ws, "bramley row grafted")
	until := time.Unix(1_800_000_000, 0).UTC()

	beforeDirect, _ := ps.GetEngramUncached(ctx, ws, direct)
	beforeBatched, _ := ps.GetEngramUncached(ctx, ws, batched)

	if stamped, err := ps.StampValidUntil(ctx, ws, direct, until, true); err != nil || !stamped {
		t.Fatalf("direct stamp: stamped=%v err=%v", stamped, err)
	}
	b := ps.NewBatch()
	defer b.Discard()
	if err := b.StampValidUntil(ctx, ws, batched, until, true); err != nil {
		t.Fatal(err)
	}
	if err := b.Commit(); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		name   string
		id     ULID
		before *Engram
	}{{"direct", direct, beforeDirect}, {"batch", batched, beforeBatched}} {
		after, err := ps.GetEngramUncached(ctx, ws, c.id)
		if err != nil {
			t.Fatal(err)
		}
		if !after.ValidUntil.Equal(until) {
			t.Errorf("%s: ValidUntil = %v, want %v", c.name, after.ValidUntil, until)
		}
		want := *c.before
		want.ValidUntil = after.ValidUntil
		if !reflect.DeepEqual(&want, after) {
			t.Errorf("%s stamp changed more than ValidUntil.\n before: %+v\n after:  %+v", c.name, c.before, after)
		}
		assertLifecycle(t, ps, ws, c.id, StateActive)
		// The cached read agrees with the record (the commit evicted the entry).
		cached, err := ps.GetEngram(ctx, ws, c.id)
		if err != nil || !cached.ValidUntil.Equal(until) {
			t.Errorf("%s: cached read ValidUntil = %v err=%v, want %v", c.name, cached.ValidUntil, err, until)
		}
		metas, err := ps.GetMetadata(ctx, ws, []ULID{c.id})
		if err != nil || metas[0] == nil || !metas[0].ValidUntil.Equal(until) {
			t.Errorf("%s: metadata ValidUntil not stamped (err=%v)", c.name, err)
		}
	}

	pd, pb := stampProvenance(t, ps, ws, direct), stampProvenance(t, ps, ws, batched)
	if len(pd) != 1 || !reflect.DeepEqual(pd, pb) {
		t.Errorf("stamp provenance differs: direct %v, batch %v", pd, pb)
	}
}

// onlyIfOpen: an already-closed window records when the fact actually stopped
// being true and must not be moved — and a stamp that changes nothing writes
// nothing (no rewrite of the record, no provenance entry).
func TestBatchStampValidUntil_OnlyIfOpenPreservesAnEarlierStamp(t *testing.T) {
	ps := openTestStore(t)
	ctx := context.Background()
	ws := ps.VaultPrefix("orchard-ledger")
	id := writeStampFixture(t, ps, ws, "cider press serviced")
	earlier := time.Unix(1_700_000_000, 0).UTC()
	later := time.Unix(1_800_000_000, 0).UTC()
	if _, err := ps.StampValidUntil(ctx, ws, id, earlier, false); err != nil {
		t.Fatal(err)
	}
	rawBefore, _ := Get(ps.db, keys.EngramKey(ws, [16]byte(id)))

	b := ps.NewBatch()
	defer b.Discard()
	if err := b.StampValidUntil(ctx, ws, id, later, true); err != nil {
		t.Fatal(err)
	}
	if err := b.Commit(); err != nil {
		t.Fatal(err)
	}

	rawAfter, _ := Get(ps.db, keys.EngramKey(ws, [16]byte(id)))
	if !bytes.Equal(rawBefore, rawAfter) {
		t.Errorf("a no-op stamp rewrote the record")
	}
	got, _ := ps.GetEngramUncached(ctx, ws, id)
	if !got.ValidUntil.Equal(earlier) {
		t.Errorf("ValidUntil = %v, want the earlier stamp %v preserved", got.ValidUntil, earlier)
	}
	if notes := stampProvenance(t, ps, ws, id); len(notes) != 1 {
		t.Errorf("stamp provenance entries = %v, want only the first stamp's", notes)
	}

	// Without onlyIfOpen the batch stamp overwrites, like the direct one.
	b2 := ps.NewBatch()
	defer b2.Discard()
	if err := b2.StampValidUntil(ctx, ws, id, later, false); err != nil {
		t.Fatal(err)
	}
	if err := b2.Commit(); err != nil {
		t.Fatal(err)
	}
	if got, _ := ps.GetEngramUncached(ctx, ws, id); !got.ValidUntil.Equal(later) {
		t.Errorf("ValidUntil = %v, want %v after an unconditional stamp", got.ValidUntil, later)
	}
}

// supersedeBatch queues what a write that declares a supersession needs to
// commit as one unit: the new engram, its guarded supersedes edge onto target,
// and target's retirement.
func supersedeBatch(t *testing.T, ps *PebbleStore, ws [8]byte, target ULID, until time.Time) (StoreBatch, ULID) {
	t.Helper()
	ctx := context.Background()
	b := ps.NewBatch()
	succ := &Engram{Concept: "replacement", Content: "the row was replanted with a later-flowering variety"}
	if err := b.WriteEngram(ctx, ws, succ); err != nil {
		t.Fatal(err)
	}
	if err := b.WriteAssociation(ctx, ws, succ.ID, target, &Association{
		TargetID: target, RelType: RelSupersedes, Weight: 0.9, Confidence: 1, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.StampValidUntil(ctx, ws, target, until, true); err != nil {
		t.Fatal(err)
	}
	return b, succ.ID
}

func assertNothingLanded(t *testing.T, ps *PebbleStore, ws [8]byte, target, succ ULID, targetGone bool) {
	t.Helper()
	ctx := context.Background()
	if ps.engramExists(ws, [16]byte(succ)) {
		t.Errorf("the successor engram exists although its batch did not commit")
	}
	if got, err := ps.SupersedersOf(ctx, ws, target); err != nil || len(got) != 0 {
		t.Errorf("a supersedes edge onto the target exists although its batch did not commit: %v (err %v)", got, err)
	}
	fwd, err := ps.GetAssociations(ctx, ws, []ULID{succ}, 0)
	if err != nil || len(fwd[succ]) != 0 {
		t.Errorf("forward edges of the uncommitted successor: %v (err %v)", fwd[succ], err)
	}
	if targetGone {
		return
	}
	if eng, err := ps.GetEngramUncached(ctx, ws, target); err != nil || !eng.ValidUntil.IsZero() {
		t.Errorf("the target was retired although its batch did not commit (err %v)", err)
	}
}

// The engram, the edge and the retirement are one commit: a batch that is
// discarded, or whose Commit fails, leaves none of the three — never the edge
// without the stamp, never the stamp without the edge.
func TestBatchStampValidUntil_IsAtomicWithTheEngramAndItsEdge(t *testing.T) {
	ctx := context.Background()
	until := time.Unix(1_800_000_000, 0).UTC()

	t.Run("committed", func(t *testing.T) {
		ps := openTestStore(t)
		ws := ps.VaultPrefix("orchard-ledger")
		target := writeStampFixture(t, ps, ws, "plum row netted")
		b, succ := supersedeBatch(t, ps, ws, target, until)
		defer b.Discard()
		if err := b.Commit(); err != nil {
			t.Fatal(err)
		}
		if got, _ := ps.SupersedersOf(ctx, ws, target); len(got) != 1 || got[0] != succ {
			t.Errorf("successors = %v, want [%s]", got, succ)
		}
		if eng, _ := ps.GetEngramUncached(ctx, ws, target); !eng.ValidUntil.Equal(until) || eng.State != StateActive {
			t.Errorf("target: valid_until=%v state=%v, want stamped and active", eng.ValidUntil, eng.State)
		}
		if !ps.engramExists(ws, [16]byte(succ)) {
			t.Errorf("the successor was not written")
		}
	})

	t.Run("discarded", func(t *testing.T) {
		ps := openTestStore(t)
		ws := ps.VaultPrefix("orchard-ledger")
		target := writeStampFixture(t, ps, ws, "plum row netted")
		b, succ := supersedeBatch(t, ps, ws, target, until)
		b.Discard()
		assertNothingLanded(t, ps, ws, target, succ, false)
	})

	// The retirement cannot be applied (the target was hard-deleted after it
	// was queued): the WHOLE batch fails, including the engram and the edge.
	t.Run("commit fails applying the retirement", func(t *testing.T) {
		ps := openTestStore(t)
		ws := ps.VaultPrefix("orchard-ledger")
		target := writeStampFixture(t, ps, ws, "plum row netted")
		b, succ := supersedeBatch(t, ps, ws, target, until)
		defer b.Discard()
		if err := ps.DeleteEngram(ctx, ws, target); err != nil {
			t.Fatal(err)
		}
		if err := b.Commit(); err == nil {
			t.Fatal("Commit succeeded although the engram it must retire no longer exists")
		}
		assertNothingLanded(t, ps, ws, target, succ, true)
	})

	// The edge cannot be written (a different relation already holds its key,
	// STO-15): again the whole batch fails — the target is not retired.
	t.Run("commit fails on the edge's collision guard", func(t *testing.T) {
		ps := openTestStore(t)
		ws := ps.VaultPrefix("orchard-ledger")
		target := writeStampFixture(t, ps, ws, "plum row netted")
		source := writeStampFixture(t, ps, ws, "plum row replaced by damsons")
		b := ps.NewBatch()
		defer b.Discard()
		if err := b.WriteAssociation(ctx, ws, source, target, &Association{
			TargetID: target, RelType: RelSupersedes, Weight: 0.9, Confidence: 1, CreatedAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		if err := b.StampValidUntil(ctx, ws, target, until, true); err != nil {
			t.Fatal(err)
		}
		// Lands between the queue and the Commit, at the same (src, weight, dst).
		if err := ps.WriteAssociation(ctx, ws, source, target, &Association{
			TargetID: target, RelType: RelContradicts, Weight: 0.9, Confidence: 1, CreatedAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		err := b.Commit()
		if err == nil || !strings.Contains(err.Error(), ErrAssocRelTypeCollision.Error()) {
			t.Fatalf("Commit err = %v, want the STO-15 collision", err)
		}
		if eng, _ := ps.GetEngramUncached(ctx, ws, target); !eng.ValidUntil.IsZero() {
			t.Errorf("the target was retired although the supersedes edge was refused")
		}
		if got, _ := ps.SupersedersOf(ctx, ws, target); len(got) != 0 {
			t.Errorf("successors = %v, want none", got)
		}
	})
}

// A write committed between the queue and the Commit is kept: the stamp is
// applied to the record as stored at Commit, under the engram's stripe lock
// (STO-24), not to the copy read when it was queued.
func TestBatchStampValidUntil_ConcurrentUpdateIsKept(t *testing.T) {
	ps := openTestStore(t)
	ctx := context.Background()
	ws := ps.VaultPrefix("orchard-ledger")
	id := writeStampFixture(t, ps, ws, "quince hedge trimmed")
	before, _ := ps.GetEngramUncached(ctx, ws, id)
	until := time.Unix(1_800_000_000, 0).UTC()

	b := ps.NewBatch()
	defer b.Discard()
	if err := b.StampValidUntil(ctx, ws, id, until, true); err != nil {
		t.Fatal(err)
	}
	if err := ps.TouchAccess(ctx, ws, id); err != nil {
		t.Fatal(err)
	}
	if err := ps.UpdateTrust(ctx, ws, id, TrustExternal); err != nil {
		t.Fatal(err)
	}
	if err := b.Commit(); err != nil {
		t.Fatal(err)
	}
	got, _ := ps.GetEngramUncached(ctx, ws, id)
	if got.AccessCount != before.AccessCount+1 || got.Trust != TrustExternal {
		t.Errorf("the stamp reverted writes committed after it was queued: access=%d (want %d) trust=%v (want external)",
			got.AccessCount, before.AccessCount+1, got.Trust)
	}
	if !got.ValidUntil.Equal(until) {
		t.Errorf("ValidUntil = %v, want %v", got.ValidUntil, until)
	}
}

// Queue-time refusals: a missing engram, and an engram written in the same
// batch (the stamp would be applied to the STORED record and discard the write).
func TestBatchStampValidUntil_QueueRefusals(t *testing.T) {
	ps := openTestStore(t)
	ctx := context.Background()
	ws := ps.VaultPrefix("orchard-ledger")
	b := ps.NewBatch()
	defer b.Discard()

	if err := b.StampValidUntil(ctx, ws, NewULID(), time.Now(), true); err == nil {
		t.Errorf("stamping an engram that does not exist was queued")
	}
	fresh := &Engram{Concept: "new row", Content: "a new row of medlars was staked out"}
	if err := b.WriteEngram(ctx, ws, fresh); err != nil {
		t.Fatal(err)
	}
	if err := b.StampValidUntil(ctx, ws, fresh.ID, time.Now(), true); err == nil {
		t.Errorf("stamping an engram written in the same batch was queued")
	}
}
