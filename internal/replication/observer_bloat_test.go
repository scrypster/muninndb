package replication

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/cockroachdb/pebble"

	"github.com/scrypster/muninndb/internal/prefix"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

// countReplicationEntries counts log-entry keys in db.
func countReplicationEntries(t *testing.T, db *pebble.DB) int {
	t.Helper()
	iter, err := db.NewIter(&pebble.IterOptions{
		LowerBound: entryRangeLower(),
		UpperBound: entryRangeUpper(),
	})
	if err != nil {
		t.Fatalf("new iter: %v", err)
	}
	defer iter.Close()
	n := 0
	for valid := iter.First(); valid; valid = iter.Next() {
		if _, ok := entrySeqFromKey(iter.Key()); ok {
			n++
		}
	}
	return n
}

// TestSnapshot_DoesNotShipTheCortexLogToALobe is #826's first mechanism.
//
// SnapshotSender.streamChunks iterated the WHOLE database, so a joining Lobe
// received a byte-for-byte copy of the Cortex's replication log — entries that
// carry the full key and value of every replicated write. Nothing on a Lobe
// reads them (only a Cortex serves a stream) and nothing prunes them (the
// periodic prune is leader-gated), so they sat there forever. Measured on
// production lobes: 22 GB and 7.5 GB.
//
// The metadata must still ship — in particular the seq counter, so a promoted
// Lobe continues the cluster's numbering rather than restarting at 1 and having
// every entry it emits skipped as "already applied".
func TestSnapshot_DoesNotShipTheCortexLogToALobe(t *testing.T) {
	cortexDB := newTestDB(t)
	lobeDB := newTestDB(t)

	repLog := NewReplicationLog(cortexDB)
	for i := 0; i < 25; i++ {
		if _, err := repLog.Append(OpBatch, nil, bytes.Repeat([]byte("payload"), 32)); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	// User data, an idempotency receipt, and cluster metadata must all arrive.
	userKey := append([]byte{prefix.Engram}, []byte("user-record")...)
	if err := cortexDB.Set(userKey, []byte("engram bytes"), pebble.Sync); err != nil {
		t.Fatalf("seed user key: %v", err)
	}
	receiptKey := keys.IdempotencyKey("op-alpha")
	if err := cortexDB.Set(receiptKey, []byte(`{"engram_id":"e1","created_at":1}`), pebble.Sync); err != nil {
		t.Fatalf("seed receipt: %v", err)
	}
	if err := cortexDB.Set(clusterEpochKey(), []byte{0, 0, 0, 0, 0, 0, 0, 4}, pebble.Sync); err != nil {
		t.Fatalf("seed epoch: %v", err)
	}

	if got := countReplicationEntries(t, cortexDB); got != 25 {
		t.Fatalf("fixture: cortex has %d log entries, want 25", got)
	}

	sender := NewSnapshotSender(cortexDB, repLog)
	receiver := NewSnapshotReceiver(lobeDB)
	senderConn, receiverConn := snapPipeConn(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sendErr := make(chan error, 1)
	go func() {
		_, err := sender.Send(ctx, senderConn)
		sendErr <- err
	}()

	if _, err := receiver.Receive(ctx, receiverConn); err != nil {
		t.Fatalf("receive: %v", err)
	}
	if err := <-sendErr; err != nil {
		t.Fatalf("send: %v", err)
	}

	if got := countReplicationEntries(t, lobeDB); got != 0 {
		t.Errorf("lobe received %d replication log entries from the Cortex snapshot; want 0 (#826)", got)
	}
	// The Cortex kept its own log — the filter is send-side only.
	if got := countReplicationEntries(t, cortexDB); got != 25 {
		t.Errorf("cortex lost log entries to its own snapshot: %d, want 25", got)
	}
	// Everything else arrived.
	for name, k := range map[string][]byte{
		"user record":         userKey,
		"idempotency receipt": receiptKey,
		"cluster epoch":       clusterEpochKey(),
		"seq counter":         seqCounterKey(),
	} {
		v, closer, err := lobeDB.Get(k)
		if err != nil {
			t.Errorf("%s (% x) did not reach the lobe: %v", name, k, err)
			continue
		}
		if len(v) == 0 {
			t.Errorf("%s arrived empty", name)
		}
		closer.Close()
	}
}

// TestLocalAppendFunc_FollowerDoesNotGrowALogNobodyReads is #826's second
// mechanism. RepLogAppend was wired unconditionally on every cluster node, so a
// Lobe appended a full-size entry for each of its OWN local writes (recall's
// last-access touches, Hebbian updates, decay, dream). That is why the measured
// lobe held MORE entries than the Cortex it followed.
//
// Suppression must be fail-open: RoleUnknown is the startup window, and a
// leader that stopped logging there would silently drop writes out of the
// stream its followers read.
func TestLocalAppendFunc_FollowerDoesNotGrowALogNobodyReads(t *testing.T) {
	cases := []struct {
		role       NodeRole
		wantAppend bool
	}{
		{RolePrimary, true},
		{RoleUnknown, true},
		{RoleSentinel, true},
		{RoleReplica, false},
		{RoleObserver, false},
	}
	for _, tc := range cases {
		t.Run(tc.role.String(), func(t *testing.T) {
			db := newTestDB(t)
			log := NewReplicationLog(db)
			c := &ClusterCoordinator{role: tc.role}

			appendFn := LocalAppendFunc(c, log)
			for i := 0; i < 3; i++ {
				if err := appendFn(uint8(OpBatch), nil, []byte("local write")); err != nil {
					t.Fatalf("append: %v", err)
				}
			}

			got := countReplicationEntries(t, db)
			want := 0
			if tc.wantAppend {
				want = 3
			}
			if got != want {
				t.Errorf("role=%s: log holds %d entries; want %d", tc.role, got, want)
			}
			if !tc.wantAppend && log.CurrentSeq() != 0 {
				t.Errorf("role=%s: sequence advanced to %d on a suppressed append", tc.role, log.CurrentSeq())
			}
		})
	}
}

// TestLocalAppendFunc_PromotedFollowerResumesFromItsPersistedSequence: a Lobe
// that stopped appending must not restart numbering at 1 when it is promoted —
// a follower whose watermark is higher would skip every entry as already
// applied.
func TestLocalAppendFunc_PromotedFollowerResumesFromItsPersistedSequence(t *testing.T) {
	db := newTestDB(t)
	log := NewReplicationLog(db)
	c := &ClusterCoordinator{role: RolePrimary}
	appendFn := LocalAppendFunc(c, log)

	for i := 0; i < 5; i++ {
		if err := appendFn(uint8(OpBatch), nil, []byte("v")); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	c.roleMu.Lock()
	c.role = RoleObserver
	c.roleMu.Unlock()
	for i := 0; i < 10; i++ {
		if err := appendFn(uint8(OpBatch), nil, []byte("v")); err != nil {
			t.Fatalf("suppressed append: %v", err)
		}
	}

	c.roleMu.Lock()
	c.role = RolePrimary
	c.roleMu.Unlock()
	if err := appendFn(uint8(OpBatch), nil, []byte("v")); err != nil {
		t.Fatalf("append after promotion: %v", err)
	}
	if got := log.CurrentSeq(); got != 6 {
		t.Errorf("sequence after promotion = %d; want 6 (5 appended, 10 suppressed, 1 more)", got)
	}
}

// TestLocalAppendFunc_NilCoordinatorAlwaysAppends — cluster mode without a
// coordinator must not silently stop replicating.
func TestLocalAppendFunc_NilCoordinatorAlwaysAppends(t *testing.T) {
	db := newTestDB(t)
	log := NewReplicationLog(db)
	if err := LocalAppendFunc(nil, log)(uint8(OpSet), []byte("k"), []byte("v")); err != nil {
		t.Fatalf("append: %v", err)
	}
	if got := countReplicationEntries(t, db); got != 1 {
		t.Errorf("nil coordinator: %d entries; want 1", got)
	}
}
