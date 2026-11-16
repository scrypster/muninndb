package replication

import (
	"context"
	"testing"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/storage"
)

// TestApplier_FollowerCacheSeesReplicatedWrite: a follower applies the
// leader's batches straight into Pebble, bypassing its store's write path.
// Without the applier's cache invalidation the follower's L1 cache keeps
// serving the value it held before the replicated write, indefinitely.
func TestApplier_FollowerCacheSeesReplicatedWrite(t *testing.T) {
	var shipped [][]byte
	leaderDB, err := storage.OpenPebble(t.TempDir(), storage.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	leader := storage.NewPebbleStore(leaderDB, storage.PebbleStoreConfig{CacheSize: 100,
		RepLogAppend: func(op uint8, _, value []byte) error {
			shipped = append(shipped, append([]byte(nil), value...))
			return nil
		}})
	defer leader.Close()

	followerDB, err := storage.OpenPebble(t.TempDir(), storage.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	follower := storage.NewPebbleStore(followerDB, storage.PebbleStoreConfig{CacheSize: 100})
	defer follower.Close()
	applier := NewApplier(followerDB)
	applier.SetCommitHooks(follower.InvalidateCommittedRepr, follower.InvalidateAllCaches)

	var seq uint64
	ship := func() {
		for _, repr := range shipped {
			seq++
			if err := applier.Apply(ReplicationEntry{Seq: seq, Op: OpBatch, Value: repr}); err != nil {
				t.Fatal(err)
			}
		}
		shipped = nil
	}

	ctx := context.Background()
	ws := leader.VaultPrefix("depot-roster")
	id, err := leader.WriteEngram(ctx, ws, &storage.Engram{
		Concept: "tender axle", Content: "the tender's rear axle box ran warm on the gala day", Confidence: 0.3, Stability: 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	ship()

	if e, err := follower.GetEngram(ctx, ws, id); err != nil || e.Confidence != 0.3 {
		t.Fatalf("follower precondition: %v %v", e, err) // now cached on the follower
	}
	if err := leader.UpdateConfidence(ctx, ws, id, 0.9); err != nil {
		t.Fatal(err)
	}
	ship()

	e, err := follower.GetEngram(ctx, ws, id)
	if err != nil {
		t.Fatal(err)
	}
	if e.Confidence != 0.9 {
		t.Fatalf("follower served its pre-replication cache entry: confidence %v, leader committed 0.9", e.Confidence)
	}
}

// The commit hook fails safe: it runs even when Commit returns an error, so a
// batch that was applied (or partially visible) before the error can never
// leave the follower's caches stale. A read-only DB makes Commit fail
// deterministically.
func TestApplier_CommitHookRunsWhenCommitFails(t *testing.T) {
	dir := t.TempDir()
	db, err := pebble.Open(dir, &pebble.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	ro, err := pebble.Open(dir, &pebble.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()

	a := NewApplier(ro)
	var calls int
	a.SetCommitHooks(func([]byte) { calls++ }, func() {})

	src := ro.NewBatch()
	src.Set([]byte("k"), []byte("v"), nil)
	repr := append([]byte(nil), src.Repr()...)
	src.Close()

	if err := a.Apply(ReplicationEntry{Seq: 1, Op: OpSet, Key: []byte("k"), Value: []byte("v")}); err == nil {
		t.Fatal("precondition: commit on a read-only DB should fail")
	}
	if err := a.Apply(ReplicationEntry{Seq: 2, Op: OpBatch, Value: repr}); err == nil {
		t.Fatal("precondition: repr commit on a read-only DB should fail")
	}
	if calls != 2 {
		t.Fatalf("commit hook ran %d times across two failed commits, want 2 (fail-open invalidation)", calls)
	}
}
