package storage_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/vfs"
	"github.com/scrypster/muninndb/internal/replication"
	"github.com/scrypster/muninndb/internal/storage"
)

func followerRetiredMismatch(t *testing.T, ps *storage.PebbleStore, ws [8]byte) (setLen, records, hiddenLive, missing int) {
	t.Helper()
	ctx := context.Background()
	want := map[[16]byte]bool{}
	all := map[[16]byte]bool{}
	if err := ps.ScanEngrams(ctx, ws, func(e *storage.Engram) error {
		all[[16]byte(e.ID)] = true
		if e.State == storage.StateSoftDeleted || e.State == storage.StateArchived {
			want[[16]byte(e.ID)] = true
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	view, err := ps.RetiredIDs(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	for id := range all {
		in := view.Contains(id)
		if in && !want[id] {
			hiddenLive++
		}
		if !in && want[id] {
			missing++
		}
	}
	return view.Len(), len(want), hiddenLive, missing
}

// A replication follower applies the leader's batches straight to Pebble. With
// the applier's commit hooks wired to the follower store (as cmd/muninn does),
// the follower's set follows the leader's lifecycle changes — including a
// RESTORE, the direction in which a stale set hides a live memory. (That a
// resnapshot rebase drops the set is pinned at the applier:
// TestApplier_CommitHooks in internal/replication.)
func TestRetiredSet_FollowerAppliedBatchesDriveTheSet(t *testing.T) {
	ctx := context.Background()
	fdb, err := pebble.Open("follower", &pebble.Options{FS: vfs.NewMem()})
	if err != nil {
		t.Fatal(err)
	}
	applier := replication.NewApplier(fdb)
	follower := storage.NewPebbleStore(fdb, storage.PebbleStoreConfig{CacheSize: 100, NoSyncEngrams: true})
	t.Cleanup(func() { follower.Close() })
	applier.SetCommitHooks(follower.NoteCommittedRepr, follower.ResetAllRetired)

	var seq uint64
	ldb, err := pebble.Open("leader", &pebble.Options{FS: vfs.NewMem()})
	if err != nil {
		t.Fatal(err)
	}
	leader := storage.NewPebbleStore(ldb, storage.PebbleStoreConfig{CacheSize: 100, NoSyncEngrams: true,
		RepLogAppend: func(op uint8, key, value []byte) error {
			seq++
			return applier.Apply(replication.ReplicationEntry{Seq: seq, Op: replication.WALOp(op),
				Key: append([]byte(nil), key...), Value: append([]byte(nil), value...)})
		}})
	t.Cleanup(func() { leader.Close() })
	ws := leader.VaultPrefix("cluster")

	ids := make([]storage.ULID, 20)
	for i := range ids {
		id, err := leader.WriteEngram(ctx, ws, &storage.Engram{Concept: fmt.Sprintf("post %d", i), Content: "trail post"})
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = id
	}
	for _, id := range ids[:10] {
		if err := leader.SoftDelete(ctx, ws, id); err != nil {
			t.Fatal(err)
		}
	}
	// The follower builds its set now, then only replication tells it more.
	if v, err := follower.RetiredIDs(ctx, ws); err != nil || v.Len() != 10 {
		t.Fatalf("follower set after 10 forgets: len=%d err=%v", v.Len(), err)
	}
	for _, id := range ids[:5] {
		if err := leader.UpdateMetadata(ctx, ws, id, &storage.EngramMeta{State: storage.StateActive, Confidence: 1, Stability: 30, UpdatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range ids[15:] {
		if err := leader.SoftDelete(ctx, ws, id); err != nil {
			t.Fatal(err)
		}
	}
	if n, rec, hidden, missing := followerRetiredMismatch(t, follower, ws); hidden != 0 || missing != 0 {
		t.Errorf("follower after replicated restores/forgets: set=%d records=%d — %d live memories hidden, %d retired missing", n, rec, hidden, missing)
	}

}
