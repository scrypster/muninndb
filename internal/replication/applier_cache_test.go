package replication

import (
	"context"
	"testing"

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
