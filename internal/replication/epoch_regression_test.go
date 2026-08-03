package replication

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/vmihailenco/msgpack/v5"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// ---------------------------------------------------------------------------
// #631 claim 1 — a rebuilt/rolled-back Cortex is adopted silently, and the
// Lobe's apply cursor survives the resnapshot that was supposed to replace it.
//
// Reported symptom: a Lobe holding epoch 49 / ack seq 36557 joined a rebuilt
// Cortex (epoch 1, seq ~0), reported "lag: 0", and received nothing.
//
// Mechanism (verified in-tree, not taken on faith):
//   - SnapshotReceiver.WipeForResnapshot preserves ONLY cluster_epoch, so the
//     PERSISTED lastApplied key is wiped — but Applier.lastApplied is an
//     in-memory field with no reset path anywhere in the tree, so it stays at
//     36557 after the snapshot lands.
//   - ReplicationLag() = cortexSeq - lastApplied, guarded by
//     "if cortexSeq <= lastApplied { return 0 }" → reports 0.
//   - Applier.Apply() skips every entry with Seq <= lastApplied → receives
//     nothing, forever, silently.
//
// These tests pin both halves: the apply cursor must be reset to the snapshot
// baseline, and a backwards epoch must only ever be adopted together with the
// full resnapshot that replaces the local state it belongs to.
// ---------------------------------------------------------------------------

// seedLastApplied writes a persisted apply cursor into db so a freshly
// constructed Applier loads it, exactly as a long-running Lobe would.
func seedLastApplied(t *testing.T, db *pebble.DB, seq uint64) {
	t.Helper()
	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, seq)
	if err := db.Set(lastAppliedKey(), buf, pebble.Sync); err != nil {
		t.Fatalf("seed lastApplied: %v", err)
	}
}

// TestJoin_ResnapshotResetsApplyCursor is the #631 claim-1 core pin. A Lobe far
// ahead of a rebuilt Cortex takes a full snapshot; after it lands, the apply
// cursor MUST be the snapshot baseline. If it keeps the old, higher value the
// node is permanently deaf to the new Cortex's stream while reporting lag 0.
func TestJoin_ResnapshotResetsApplyCursor(t *testing.T) {
	cortexDB := newTestDB(t)
	cortexRepLog := NewReplicationLog(cortexDB)
	if err := cortexDB.Set([]byte("key1"), []byte("val1"), pebble.Sync); err != nil {
		t.Fatalf("cortex db set: %v", err)
	}
	if _, err := cortexRepLog.Append(OpSet, []byte("key1"), []byte("val1")); err != nil {
		t.Fatalf("replog append: %v", err)
	}
	snapshotSeq := cortexRepLog.CurrentSeq()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Cortex was rebuilt: epoch 1, head seq == snapshotSeq (small).
	clientConn, done := mockCortexJoinWithSnapshot(t, cortexDB, cortexRepLog, snapshotSeq, 1)

	// Lobe is far ahead of the rebuilt Cortex on BOTH counters.
	lobeDB := newTestDB(t)
	const staleCursor = 36557
	seedLastApplied(t, lobeDB, staleCursor)
	lobeES := newTestEpochStore(t)
	if _, err := lobeES.Advance(49); err != nil {
		t.Fatalf("seed epoch: %v", err)
	}
	lobeApplier := NewApplier(lobeDB)
	if got := lobeApplier.LastApplied(); got != staleCursor {
		t.Fatalf("precondition: applier LastApplied = %d, want %d", got, staleCursor)
	}
	lobeMgr := NewConnManager("lobe-regress")
	client := NewJoinClientWithDB("lobe-regress", "127.0.0.1:9500", "", lobeES, lobeApplier, lobeDB, lobeMgr)

	result, err := client.joinConn(ctx, clientConn)
	if err != nil {
		t.Fatalf("joinConn: %v", err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("mock cortex goroutine did not complete in time")
	}
	if result.StreamFromSeq != snapshotSeq {
		t.Errorf("StreamFromSeq = %d, want %d", result.StreamFromSeq, snapshotSeq)
	}

	// THE PIN: the apply cursor is the snapshot baseline, not the stale value.
	if got := lobeApplier.LastApplied(); got != snapshotSeq {
		t.Errorf("apply cursor after resnapshot = %d, want %d (snapshot baseline). "+
			"A cursor left at the pre-rebuild value makes Applier.Apply skip every "+
			"entry the new Cortex ships while ReplicationLag reports 0 — #631 claim 1.",
			got, snapshotSeq)
	}

	// And the very next entry the new Cortex ships must actually apply.
	if err := lobeApplier.Apply(ReplicationEntry{
		Seq: snapshotSeq + 1, Op: OpSet, Key: []byte("post-snap"), Value: []byte("v"),
	}); err != nil {
		t.Fatalf("apply post-snapshot entry: %v", err)
	}
	if _, closer, err := lobeDB.Get([]byte("post-snap")); err != nil {
		t.Errorf("post-snapshot entry was silently skipped by the applier: %v", err)
	} else {
		closer.Close()
	}
}

// TestJoin_AdoptsLowerEpochAfterResnapshot pins the other half: once the local
// state HAS been wholesale replaced by the Cortex's snapshot, the local epoch
// must follow it down. Keeping the higher pre-rebuild epoch leaves the node
// holding a fencing token no live Cortex can match.
func TestJoin_AdoptsLowerEpochAfterResnapshot(t *testing.T) {
	cortexDB := newTestDB(t)
	cortexRepLog := NewReplicationLog(cortexDB)
	if _, err := cortexRepLog.Append(OpSet, []byte("k"), []byte("v")); err != nil {
		t.Fatalf("replog append: %v", err)
	}
	snapshotSeq := cortexRepLog.CurrentSeq()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	clientConn, done := mockCortexJoinWithSnapshot(t, cortexDB, cortexRepLog, snapshotSeq, 1)

	lobeDB := newTestDB(t)
	lobeES := newTestEpochStore(t)
	if _, err := lobeES.Advance(49); err != nil {
		t.Fatalf("seed epoch: %v", err)
	}
	lobeMgr := NewConnManager("lobe-adopt")
	client := NewJoinClientWithDB("lobe-adopt", "127.0.0.1:9501", "", lobeES, NewApplier(lobeDB), lobeDB, lobeMgr)

	if _, err := client.joinConn(ctx, clientConn); err != nil {
		t.Fatalf("joinConn: %v", err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("mock cortex goroutine did not complete in time")
	}

	if got := lobeES.Load(); got != 1 {
		t.Errorf("epoch after resnapshot = %d, want 1 (the Cortex's epoch). "+
			"WipeForResnapshot deliberately preserves cluster_epoch, so without an "+
			"explicit adoption the node keeps a fencing token from a cluster history "+
			"that no longer exists — #631 claim 1.", got)
	}
}

// TestJoin_RefusesEpochRegressionWithoutResnapshot: a backwards epoch with NO
// snapshot offered means the Cortex has been rebuilt or restored beneath us and
// nothing is going to reconcile the local state. Continuing produces exactly the
// reported silent divergence, so the join must fail loudly instead.
func TestJoin_RefusesEpochRegressionWithoutResnapshot(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cortexConn, lobeConn := net.Pipe()
	t.Cleanup(func() { cortexConn.Close(); lobeConn.Close() })

	go func() {
		if _, err := mbp.ReadFrame(cortexConn); err != nil {
			return
		}
		resp := mbp.JoinResponse{
			Accepted: true,
			CortexID: "cortex-rebuilt",
			Epoch:    1, // rebuilt data dir
			// NeedsSnapshot deliberately false: nothing will replace local state.
		}
		payload, _ := msgpack.Marshal(resp)
		_ = mbp.WriteFrame(cortexConn, &mbp.Frame{
			Version:       0x01,
			Type:          mbp.TypeJoinResponse,
			PayloadLength: uint32(len(payload)),
			Payload:       payload,
		})
	}()

	lobeES := newTestEpochStore(t)
	if _, err := lobeES.Advance(49); err != nil {
		t.Fatalf("seed epoch: %v", err)
	}
	mgr := NewConnManager("lobe-refuse")
	client := NewJoinClient("lobe-refuse", "127.0.0.1:9502", "", lobeES, nil, mgr)

	_, err := client.joinConn(ctx, lobeConn)
	if err == nil {
		t.Fatal("joinConn accepted a Cortex whose epoch (1) is BEHIND the local epoch (49) " +
			"with no resnapshot offered — that is the silent divergence of #631 claim 1")
	}
	if !errors.Is(err, ErrEpochRegression) {
		t.Errorf("err = %v, want ErrEpochRegression", err)
	}
	if !strings.Contains(err.Error(), "49") || !strings.Contains(err.Error(), "1") {
		t.Errorf("error must name both epochs so an operator can act on it: %v", err)
	}
	// The local epoch must be untouched by a refused join.
	if got := lobeES.Load(); got != 49 {
		t.Errorf("epoch after refused join = %d, want 49 (unchanged)", got)
	}
}

// TestEpochStore_AdvanceReportsRegression pins the structural change: the
// monotonic setter REPORTS whether it moved, so a caller cannot silently
// swallow a regression the way ForceSet's bare error return allowed.
func TestEpochStore_AdvanceReportsRegression(t *testing.T) {
	s := newTestEpochStore(t)

	advanced, err := s.Advance(10)
	if err != nil || !advanced {
		t.Fatalf("Advance(10) = (%v, %v), want (true, nil)", advanced, err)
	}
	advanced, err = s.Advance(5)
	if err != nil {
		t.Fatalf("Advance(5): %v", err)
	}
	if advanced {
		t.Error("Advance(5) reported advanced=true from 10 — the epoch must never go backwards here")
	}
	if got := s.Load(); got != 10 {
		t.Errorf("Load() = %d, want 10", got)
	}
	advanced, err = s.Advance(10)
	if err != nil {
		t.Fatalf("Advance(10) again: %v", err)
	}
	if advanced {
		t.Error("Advance(10) from 10 reported advanced=true; equal is not an advance")
	}
}

// TestEpochStore_AdoptForSnapshot is the ONLY backwards path, and it is named
// for its precondition: the local state has just been replaced wholesale.
func TestEpochStore_AdoptForSnapshot(t *testing.T) {
	s := newTestEpochStore(t)
	if _, err := s.Advance(49); err != nil {
		t.Fatalf("Advance(49): %v", err)
	}
	if err := s.AdoptForSnapshot(1); err != nil {
		t.Fatalf("AdoptForSnapshot(1): %v", err)
	}
	if got := s.Load(); got != 1 {
		t.Errorf("Load() = %d, want 1", got)
	}
}
