package storage

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

// replSink is a fake replication log: it records every batch repr the store
// ships, in the order the store ships them.
type replSink struct {
	mu    sync.Mutex
	reprs [][]byte
}

func (s *replSink) append(_ uint8, _, value []byte) error {
	s.mu.Lock()
	s.reprs = append(s.reprs, append([]byte(nil), value...))
	s.mu.Unlock()
	return nil
}

func (s *replSink) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.reprs)
}

// applyToFollower replays the shipped batches, in shipped order, into a fresh
// Pebble DB — what a follower's applier does — and returns that DB.
func (s *replSink) applyToFollower(t *testing.T) *pebble.DB {
	t.Helper()
	fdb := openTestPebble(t)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.reprs {
		b := fdb.NewBatch()
		if err := b.SetRepr(append([]byte(nil), r...)); err != nil {
			t.Fatal(err)
		}
		if err := b.Commit(pebble.Sync); err != nil {
			t.Fatal(err)
		}
		b.Close()
	}
	return fdb
}

func assertFollowerMatchesLeader(t *testing.T, ps *PebbleStore, sink *replSink, ws [8]byte, id ULID) {
	t.Helper()
	fdb := sink.applyToFollower(t)
	for _, k := range [][]byte{keys.EngramKey(ws, [16]byte(id)), keys.MetaKey(ws, [16]byte(id))} {
		lv, err := Get(ps.db, k)
		if err != nil {
			t.Fatal(err)
		}
		fv, err := Get(fdb, k)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(lv, fv) {
			lst, fst := byte(0), byte(0)
			if len(lv) > 64 {
				lst = lv[64]
			}
			if len(fv) > 64 {
				fst = fv[64]
			}
			t.Fatalf("follower diverged from leader on key 0x%02x: leader state %v, follower state %v — "+
				"the replication log received the two writes in the opposite order from their commits",
				k[0], LifecycleState(lst), LifecycleState(fst))
		}
	}
}

// Each case lands a forget (or a trust change) between the writer's commit
// and its replicateBatch. If the writer has released the engram's stripe by
// then, the concurrent write commits AND replicates inside the gap, so the
// follower applies it first and the older batch on top: a forgotten memory
// comes back on followers. Replicating under the stripe makes the concurrent
// writer wait, and the log order is the commit order.
func TestReplication_SameEngramBatchesShipInCommitOrder(t *testing.T) {
	vec := make([]float32, 384)
	for i := range vec {
		vec[i] = float32(i%5) / 5
	}
	cases := []struct {
		name       string
		op         string
		write      func(ctx context.Context, ps *PebbleStore, ws [8]byte, id ULID) error
		concurrent func(ctx context.Context, ps *PebbleStore, ws [8]byte, id ULID) error
	}{
		{"UpdateEmbedding vs forget", "UpdateEmbedding",
			func(ctx context.Context, ps *PebbleStore, ws [8]byte, id ULID) error {
				return ps.UpdateEmbedding(ctx, ws, id, vec)
			},
			func(ctx context.Context, ps *PebbleStore, ws [8]byte, id ULID) error {
				return ps.SoftDelete(ctx, ws, id)
			}},
		{"UpdateConfidence vs forget", "UpdateConfidence",
			func(ctx context.Context, ps *PebbleStore, ws [8]byte, id ULID) error {
				return ps.UpdateConfidence(ctx, ws, id, 0.3)
			},
			func(ctx context.Context, ps *PebbleStore, ws [8]byte, id ULID) error {
				return ps.SoftDelete(ctx, ws, id)
			}},
		{"batch supersede vs trust change", "BatchCommit",
			func(ctx context.Context, ps *PebbleStore, ws [8]byte, id ULID) error {
				b := ps.NewBatch()
				defer b.Discard()
				if err := b.SupersedeEngram(ctx, ws, id, time.Unix(1_800_000_000, 0)); err != nil {
					return err
				}
				return b.Commit()
			},
			func(ctx context.Context, ps *PebbleStore, ws [8]byte, id ULID) error {
				return ps.UpdateTrust(ctx, ws, id, TrustVerified)
			}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ps := openTestStore(t)
			sink := &replSink{}
			ps.repLogAppend = sink.append
			ctx := context.Background()
			ws := ps.VaultPrefix("trail-repl")
			id := writeRMWFixture(t, ps, ws, "trail register box at the col")

			il := &interleaver{ps: ps}
			restore := il.hook(t, c.op+":committed", id, func() {
				if err := c.concurrent(ctx, ps, ws, id); err != nil {
					t.Errorf("concurrent write: %v", err)
				}
			})
			if err := c.write(ctx, ps, ws, id); err != nil {
				t.Fatal(err)
			}
			restore()
			il.wait(t)
			assertFollowerMatchesLeader(t, ps, sink, ws, id)
		})
	}
}

// A WAL sync failure after the commit must not skip replication: the batch
// is already applied on the leader, so not shipping it diverges the
// followers. The error is still returned.
func TestUpdateEmbedding_SyncFailureStillReplicates(t *testing.T) {
	ps := openTestStore(t)
	sink := &replSink{}
	ps.repLogAppend = sink.append
	ctx := context.Background()
	ws := ps.VaultPrefix("trail-repl")
	id := writeRMWFixture(t, ps, ws, "trail register box at the col")
	before := sink.len()

	boom := errors.New("injected wal sync failure")
	h := func() error { return boom }
	walSyncHook.Store(&h)
	defer walSyncHook.Store(nil)

	vec := make([]float32, 384)
	for i := range vec {
		vec[i] = 0.25
	}
	err := ps.UpdateEmbedding(ctx, ws, id, vec)
	if !errors.Is(err, boom) {
		t.Fatalf("UpdateEmbedding err = %v, want the sync failure", err)
	}
	if got := sink.len() - before; got != 1 {
		t.Fatalf("a committed embedding batch whose WAL sync failed was replicated %d times, want 1", got)
	}
	walSyncHook.Store(nil)
	assertFollowerMatchesLeader(t, ps, sink, ws, id)
}
