package replication

import (
	"net"
	"testing"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/vmihailenco/msgpack/v5"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// #531 PR3: a probe to the leader returns the leader's identity WITHOUT
// registering the prober's conn or adding a member (side-effect-free discovery).
func TestHandleIncomingJoin_Probe(t *testing.T) {
	coord, _ := newTestCoordinator(t, "primary")
	if err := coord.epochStore.ForceSet(5); err != nil {
		t.Fatal(err)
	}
	coord.roleMu.Lock()
	coord.role = RolePrimary // this node is the leader
	coord.roleMu.Unlock()

	cortexConn, proberConn := net.Pipe()
	t.Cleanup(func() { cortexConn.Close(); proberConn.Close() })

	req := mbp.JoinRequest{NodeID: "returning-cortex", Probe: true, ProtocolVersion: mbp.CurrentProtocolVersion}
	payload, err := msgpack.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}

	go func() { _, _, _ = coord.HandleIncomingJoin(cortexConn, payload) }()

	_ = proberConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	frame, err := mbp.ReadFrame(proberConn)
	if err != nil {
		t.Fatalf("read probe response: %v", err)
	}
	var resp mbp.JoinResponse
	if err := msgpack.Unmarshal(frame.Payload, &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Accepted || resp.CortexID != coord.cfg.NodeID || resp.Epoch != 5 {
		t.Errorf("leader probe should return Accepted + self@5, got accepted=%v cortex=%q epoch=%d",
			resp.Accepted, resp.CortexID, resp.Epoch)
	}
	// Side-effect-free: the prober must NOT have been registered.
	if _, ok := coord.mgr.GetPeer("returning-cortex"); ok {
		t.Error("a probe must not register the prober's conn")
	}
}

// #531 PR3: WipeForResnapshot must preserve the cluster_epoch (fencing state),
// else a deferring ex-primary that re-snapshots would restart at epoch 0.
func TestWipeForResnapshot_PreservesClusterEpoch(t *testing.T) {
	dir := t.TempDir()
	db, err := pebble.Open(dir, &pebble.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	es, err := NewEpochStore(db)
	if err != nil {
		t.Fatal(err)
	}
	if err := es.ForceSet(7); err != nil {
		t.Fatal(err)
	}
	if err := db.Set([]byte("engram:1"), []byte("data"), pebble.Sync); err != nil {
		t.Fatal(err)
	}
	// A non-meta key that merely shares the 0x19 0x03 prefix (e.g. an idempotency
	// receipt) MUST still be wiped — the preservation is exact-key, not prefix.
	collidingKey := []byte{0x19, 0x03, 0xAB, 0xCD}
	if err := db.Set(collidingKey, []byte("receipt"), pebble.Sync); err != nil {
		t.Fatal(err)
	}

	if err := NewSnapshotReceiver(db).WipeForResnapshot(); err != nil {
		t.Fatal(err)
	}

	if _, closer, err := db.Get(collidingKey); err == nil {
		closer.Close()
		t.Error("a key sharing the 0x19 0x03 prefix (not the exact epoch key) must be wiped")
	}

	// Epoch survives (re-read from the DB).
	es2, err := NewEpochStore(db)
	if err != nil {
		t.Fatal(err)
	}
	if es2.Load() != 7 {
		t.Errorf("cluster_epoch must survive WipeForResnapshot, got %d", es2.Load())
	}
	// Non-meta data was wiped.
	if _, closer, err := db.Get([]byte("engram:1")); err == nil {
		closer.Close()
		t.Error("non-meta data should be wiped by WipeForResnapshot")
	}
}
