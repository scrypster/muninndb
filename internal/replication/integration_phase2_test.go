//go:build integration

package replication

import (
	"context"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/vmihailenco/msgpack/v5"

	"github.com/scrypster/muninndb/internal/config"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// ---------------------------------------------------------------------------
// newTestNodeWithDB creates a testNode whose JoinHandler and JoinClient are
// wired with a Pebble DB, enabling snapshot send/receive.
// ---------------------------------------------------------------------------

func newTestNodeWithDB(t *testing.T, nodeID, role string) *testNode {
	t.Helper()

	dir := t.TempDir()
	db, err := pebble.Open(dir, &pebble.Options{})
	if err != nil {
		t.Fatalf("pebble.Open for %s: %v", nodeID, err)
	}
	t.Cleanup(func() { db.Close() })

	repLog := NewReplicationLog(db)
	applier := NewApplier(db)
	epochStore, err := NewEpochStore(db)
	if err != nil {
		t.Fatalf("NewEpochStore for %s: %v", nodeID, err)
	}

	cfg := &config.ClusterConfig{
		Enabled:     true,
		NodeID:      nodeID,
		BindAddr:    "127.0.0.1:0",
		Seeds:       []string{},
		Role:        role,
		LeaseTTL:    10,
		HeartbeatMS: 1000,
	}

	// Use the standard constructor, then upgrade join handler/client to DB-aware versions.
	coord := NewClusterCoordinator(cfg, repLog, applier, epochStore)

	// Replace joinHandler with DB-aware version.
	coord.joinHandler = NewJoinHandlerWithDB(nodeID, "", epochStore, repLog, db, coord.mgr)
	// Re-wire callbacks.
	coord.joinHandler.OnLobeJoined = func(info NodeInfo) {
		coord.startStreamerForLobe(info)
	}
	coord.joinHandler.OnLobeLeft = func(nodeID string) {
		coord.stopStreamerForLobe(nodeID)
	}

	// Replace joinClient with DB-aware version.
	coord.joinClient = NewJoinClientWithDB(nodeID, cfg.BindAddr, "", epochStore, applier, db, coord.mgr)

	return &testNode{
		id:         nodeID,
		coord:      coord,
		repLog:     repLog,
		applier:    applier,
		epochStore: epochStore,
		db:         db,
		dbDir:      dir,
		t:          t,
	}
}

// p2MockFlusher implements cognitiveFlushable for graceful handoff tests.
type p2MockFlusher struct {
	stopped atomic.Bool
}

func (m *p2MockFlusher) Stop() {
	m.stopped.Store(true)
}

// ---------------------------------------------------------------------------
// Test 1: Snapshot Join -- new node joins via full Pebble snapshot transfer
// ---------------------------------------------------------------------------

func TestP2Integration_SnapshotJoin(t *testing.T) {
	// Setup: node-A is Cortex with 1000 pre-populated entries.
	nodeA := newTestNodeWithDB(t, "node-A", "primary")
	nodeB := newTestNode(t, "node-B", "replica")

	connectNodes(t, nodeA, nodeB)
	registerVoters(nodeA, nodeB)
	electNode(t, nodeA)

	// Write 1000 KV entries to node-A's repLog and apply them directly to Pebble
	// (simulating the Cortex writing real data).
	for i := 1; i <= 1000; i++ {
		key := []byte(fmt.Sprintf("k-snap-%d", i))
		val := []byte(fmt.Sprintf("snap-val-%d", i))
		if _, err := nodeA.repLog.Append(OpSet, key, val); err != nil {
			t.Fatalf("Append entry %d: %v", i, err)
		}
		if err := nodeA.db.Set(key, val, pebble.NoSync); err != nil {
			t.Fatalf("Set key %d: %v", i, err)
		}
	}
	if nodeA.repLog.CurrentSeq() != 1000 {
		t.Fatalf("expected repLog seq 1000, got %d", nodeA.repLog.CurrentSeq())
	}

	// node-D is a fresh Lobe (empty DB) that will join via snapshot.
	nodeD := newTestNodeWithDB(t, "node-D", "replica")

	// Create a net.Pipe() pair for the join + snapshot handshake.
	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() {
		clientConn.Close()
		serverConn.Close()
	})

	// Server side (node-A): read JoinRequest, send JoinResponse, then stream snapshot.
	serverDone := make(chan error, 1)
	go func() {
		defer serverConn.Close()

		frame, err := mbp.ReadFrame(serverConn)
		if err != nil {
			serverDone <- fmt.Errorf("read JoinRequest: %w", err)
			return
		}
		if frame.Type != mbp.TypeJoinRequest {
			serverDone <- fmt.Errorf("expected TypeJoinRequest, got 0x%02x", frame.Type)
			return
		}

		var req mbp.JoinRequest
		if err := msgpack.Unmarshal(frame.Payload, &req); err != nil {
			serverDone <- fmt.Errorf("unmarshal JoinRequest: %w", err)
			return
		}

		resp := nodeA.coord.joinHandler.HandleJoinRequest(req, nil)

		payload, err := msgpack.Marshal(resp)
		if err != nil {
			serverDone <- fmt.Errorf("marshal JoinResponse: %w", err)
			return
		}
		respFrame := &mbp.Frame{
			Version:       0x01,
			Type:          mbp.TypeJoinResponse,
			PayloadLength: uint32(len(payload)),
			Payload:       payload,
		}
		if err := mbp.WriteFrame(serverConn, respFrame); err != nil {
			serverDone <- fmt.Errorf("write JoinResponse: %w", err)
			return
		}

		// Stream snapshot if needed.
		if resp.NeedsSnapshot {
			snapPeer := &PeerConn{conn: serverConn}
			if _, err := nodeA.coord.joinHandler.StreamSnapshot(context.Background(), snapPeer); err != nil {
				serverDone <- fmt.Errorf("StreamSnapshot: %w", err)
				return
			}
		}

		serverDone <- nil
	}()

	// Client side (node-D): perform join handshake (includes snapshot reception).
	result, err := nodeD.coord.joinClient.joinConn(context.Background(), clientConn)
	if err != nil {
		t.Fatalf("joinConn: %v", err)
	}
	if !result.Accepted {
		t.Fatalf("join rejected: %s", result.RejectReason)
	}
	if !result.NeedsSnapshot {
		t.Fatal("expected NeedsSnapshot=true in JoinResponse")
	}

	// Wait for server to finish.
	if err := <-serverDone; err != nil {
		t.Fatalf("server side error: %v", err)
	}

	// Verify node-D has all 1000 keys from the snapshot.
	for i := 1; i <= 1000; i++ {
		key := []byte(fmt.Sprintf("k-snap-%d", i))
		val, closer, err := nodeD.db.Get(key)
		if err != nil {
			t.Errorf("node-D missing k-snap-%d: %v", i, err)
			continue
		}
		want := fmt.Sprintf("snap-val-%d", i)
		if string(val) != want {
			t.Errorf("node-D k-snap-%d: got %q, want %q", i, val, want)
		}
		closer.Close()
	}

	// node-D's epoch should match node-A's.
	if nodeD.coord.CurrentEpoch() != nodeA.coord.CurrentEpoch() {
		t.Errorf("node-D epoch=%d, expected %d", nodeD.coord.CurrentEpoch(), nodeA.coord.CurrentEpoch())
	}

	// Now wire node-D into the cluster for streaming catch-up.
	connectNodes(t, nodeA, nodeD)

	// Start a NetworkStreamer on A targeting D from the snapshot seq.
	snapshotSeq := result.StreamFromSeq
	cancelStream := startStreamer(t, nodeA, nodeD, snapshotSeq)
	defer cancelStream()

	// Write 50 more entries on node-A after the snapshot.
	for i := 1; i <= 50; i++ {
		key := []byte(fmt.Sprintf("k-post-%d", i))
		val := []byte(fmt.Sprintf("post-val-%d", i))
		if _, err := nodeA.repLog.Append(OpSet, key, val); err != nil {
			t.Fatalf("Append post entry %d: %v", i, err)
		}
	}

	expectedSeq := uint64(1050)

	// Wait for node-D to apply all entries up to 1050 (1000 snapshot + 50 streamed).
	waitFor(t, 10*time.Second, func() bool {
		return nodeD.applier.LastApplied() >= expectedSeq
	}, fmt.Sprintf("node-D applier.LastApplied() >= %d", expectedSeq))

	if nodeD.applier.LastApplied() != expectedSeq {
		t.Errorf("node-D lastApplied=%d, expected %d", nodeD.applier.LastApplied(), expectedSeq)
	}
	if nodeD.applier.LastApplied() != nodeA.repLog.CurrentSeq() {
		t.Errorf("node-D lastApplied=%d != node-A repLog.CurrentSeq()=%d",
			nodeD.applier.LastApplied(), nodeA.repLog.CurrentSeq())
	}

	// Verify the 50 post-snapshot entries.
	for i := 1; i <= 50; i++ {
		key := []byte(fmt.Sprintf("k-post-%d", i))
		val, closer, err := nodeD.db.Get(key)
		if err != nil {
			t.Errorf("node-D missing k-post-%d: %v", i, err)
			continue
		}
		want := fmt.Sprintf("post-val-%d", i)
		if string(val) != want {
			t.Errorf("node-D k-post-%d: got %q, want %q", i, val, want)
		}
		closer.Close()
	}

	t.Logf("SnapshotJoin: 1000 keys via snapshot + 50 via streaming = 1050 entries on node-D")
}

// ---------------------------------------------------------------------------
// Test 1b: Wire order -- JoinResponse must precede any ReplEntry on the wire
// ---------------------------------------------------------------------------

// TestP2Integration_JoinResponseBeforeReplEntry is an end-to-end check of the
// wire-ordering invariant the deferred-callback fix protects: on a real
// connection the TypeJoinResponse frame must reach the lobe before any
// TypeReplEntry frame from the streamer. If FireOnLobeJoined fired inline inside
// HandleJoinRequest, the streamer's first ReplEntry could overtake the
// JoinResponse and corrupt the lobe-side handshake parser (#409 follow-up).
//
// Scope note: this test observes the wire only, so it cannot *deterministically*
// fail a hypothetical inline-fire regression — there the streamer goroutine and
// the coordinator's JoinResponse send would race for PeerConn's send mutex, and
// the test would catch the bug only on schedules where ReplEntry wins. The
// deterministic guard against reintroducing the inline fire is the unit test
// TestJoinHandler_HandleJoinRequest_DoesNotFireCallback, which asserts the
// callback is not invoked inside HandleJoinRequest at all. This test complements
// it by proving the assembled coordinator path (HandleIncomingJoin +
// NetworkStreamer) actually puts JoinResponse on the wire first, with a real
// streamer producing ReplEntry frames behind it.
func TestP2Integration_JoinResponseBeforeReplEntry(t *testing.T) {
	cortex := newTestNode(t, "cortex-wo", "primary")
	if err := cortex.epochStore.ForceSet(4); err != nil {
		t.Fatalf("ForceSet: %v", err)
	}
	// Only the leader accepts joins (#533); mark this cortex as leader.
	cortex.coord.roleMu.Lock()
	cortex.coord.role = RolePrimary
	cortex.coord.roleMu.Unlock()
	// Pre-populate the replication log so the streamer has entries to drain and
	// send the instant it starts (NetworkStreamer.Stream drains from seq 0).
	appendEntries(t, cortex, "wo", 20)

	// Cancel streamers/conns on teardown to avoid a leaked streamer goroutine.
	t.Cleanup(func() { _ = cortex.coord.Stop() })

	// net.Pipe stands in for the inbound lobe connection.
	cortexConn, lobeConn := net.Pipe()
	t.Cleanup(func() { cortexConn.Close(); lobeConn.Close() })

	// Lobe side: record the type of every frame in arrival order until the first
	// ReplEntry is seen (or the pipe closes).
	orderCh := make(chan []uint8, 1)
	go func() {
		var order []uint8
		for {
			frame, err := mbp.ReadFrame(lobeConn)
			if err != nil {
				break
			}
			order = append(order, frame.Type)
			if frame.Type == mbp.TypeReplEntry {
				break // enough to assert ordering
			}
		}
		orderCh <- order
	}()

	req := mbp.JoinRequest{
		NodeID:          "lobe-wo",
		Addr:            "127.0.0.1:9700",
		ProtocolVersion: mbp.CurrentProtocolVersion,
	}
	payload, err := msgpack.Marshal(req)
	if err != nil {
		t.Fatalf("marshal JoinRequest: %v", err)
	}

	if _, _, err := cortex.coord.HandleIncomingJoin(cortexConn, payload); err != nil {
		t.Fatalf("HandleIncomingJoin: %v", err)
	}

	var order []uint8
	select {
	case order = <-orderCh:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for frames on lobe side")
	}

	if len(order) == 0 {
		t.Fatal("no frames received on lobe side")
	}
	// The very first frame on the wire must be the JoinResponse.
	if order[0] != mbp.TypeJoinResponse {
		t.Fatalf("first frame = 0x%02x, want TypeJoinResponse 0x%02x", order[0], mbp.TypeJoinResponse)
	}
	// The reader loop stops at the first ReplEntry, so the last recorded frame
	// must be one. Asserting it confirms the streamer actually ran and produced
	// a ReplEntry *after* the JoinResponse — otherwise the test could pass
	// trivially against a coordinator that never starts the streamer at all.
	if order[len(order)-1] != mbp.TypeReplEntry {
		t.Fatalf("expected a TypeReplEntry to follow JoinResponse, got frame sequence %v", order)
	}
	t.Logf("wire order ok: %d frames, first=JoinResponse, ReplEntry seen after", len(order))
}

// ---------------------------------------------------------------------------
// Test 2: Cognitive Forwarding -- Lobe side effects reach Cortex HebbianWorker
// ---------------------------------------------------------------------------

func TestP2Integration_CognitiveForwarding(t *testing.T) {
	// node-A (Cortex) with mock HebbianWorker.
	nodeA := newTestNode(t, "node-A", "primary")
	nodeB := newTestNode(t, "node-B", "replica")

	connectNodes(t, nodeA, nodeB)
	registerVoters(nodeA, nodeB)
	electNode(t, nodeA)

	// Wait for B to recognize A as leader.
	waitFor(t, 5*time.Second, func() bool {
		return nodeB.coord.election.CurrentLeader() == "node-A"
	}, "node-B to recognize node-A as leader")

	// Wire mock cognitive workers into node-A.
	mockHebbian := &mockHebbianSubmitter{}
	nodeA.coord.SetCognitiveWorkers(mockHebbian)

	// Construct a CognitiveSideEffect with 3 CoActivationRef entries.
	engram1 := [16]byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0A, 0x0B, 0x0C, 0x0D, 0x0E, 0x0F, 0x10}
	engram2 := [16]byte{0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18, 0x19, 0x1A, 0x1B, 0x1C, 0x1D, 0x1E, 0x1F, 0x20}
	engram3 := [16]byte{0x21, 0x22, 0x23, 0x24, 0x25, 0x26, 0x27, 0x28, 0x29, 0x2A, 0x2B, 0x2C, 0x2D, 0x2E, 0x2F, 0x30}

	effect := mbp.CognitiveSideEffect{
		QueryID:      "test-query-001",
		OriginNodeID: "node-B",
		Timestamp:    time.Now().UnixNano(),
		CoActivations: []mbp.CoActivationRef{
			{ID: engram1, Score: 0.95},
			{ID: engram2, Score: 0.85},
			{ID: engram3, Score: 0.75},
		},
	}

	// Forward the effect from node-B's coordinator to node-A (Cortex).
	// ForwardCognitiveEffects sends a TypeCogForward frame over the pipe.
	nodeB.coord.ForwardCognitiveEffects(effect)

	// Wait for node-A to process the CogForward.
	waitFor(t, 5*time.Second, func() bool {
		return nodeA.coord.CogForwardedTotal() >= 3
	}, "node-A CogForwardedTotal >= 3")

	if got := nodeA.coord.CogForwardedTotal(); got != 3 {
		t.Errorf("CogForwardedTotal=%d, expected 3", got)
	}

	// Verify the mock HebbianWorker received a Submit call.
	waitFor(t, 5*time.Second, func() bool {
		return len(mockHebbian.Received()) >= 1
	}, "HebbianWorker to receive at least 1 event")

	// Verify the co-activation IDs match.
	hebbianEvents := mockHebbian.Received()
	if len(hebbianEvents) != 1 {
		t.Errorf("expected 1 CoActivationEvent, got %d", len(hebbianEvents))
	} else {
		ev := hebbianEvents[0]
		if len(ev.Engrams) != 3 {
			t.Errorf("expected 3 engrams in CoActivationEvent, got %d", len(ev.Engrams))
		} else {
			sentIDs := map[[16]byte]bool{engram1: true, engram2: true, engram3: true}
			for _, eng := range ev.Engrams {
				if !sentIDs[eng.ID] {
					t.Errorf("unexpected engram ID in CoActivationEvent: %x", eng.ID)
				}
			}
		}
	}

	t.Logf("CognitiveForwarding: 3 co-activations forwarded from Lobe to Cortex, "+
		"CogForwardedTotal=%d, HebbianEvents=%d",
		nodeA.coord.CogForwardedTotal(), len(hebbianEvents))
}

// ---------------------------------------------------------------------------
// Test 3: Graceful Handoff -- Cortex responsibility transfers with zero loss
// ---------------------------------------------------------------------------

func TestP2Integration_GracefulHandoff(t *testing.T) {
	// 3-node cluster: node-A (Cortex), node-B (Lobe), node-C (Lobe).
	nodeA := newTestNode(t, "node-A", "primary")
	nodeB := newTestNode(t, "node-B", "replica")
	nodeC := newTestNode(t, "node-C", "replica")

	connectNodes(t, nodeA, nodeB)
	connectNodes(t, nodeA, nodeC)
	connectNodes(t, nodeB, nodeC)
	registerVoters(nodeA, nodeB, nodeC)

	electNode(t, nodeA)

	// Wait for B and C to recognize A as leader.
	waitFor(t, 5*time.Second, func() bool {
		return nodeB.coord.election.CurrentLeader() == "node-A" &&
			nodeC.coord.election.CurrentLeader() == "node-A"
	}, "B and C to recognize node-A as leader")

	originalEpoch := nodeA.coord.CurrentEpoch()

	// Wire mock cognitive flushers into node-A.
	hebbianFlusher := &p2MockFlusher{}
	nodeA.coord.SetCognitiveFlushers(hebbianFlusher)

	// Write 100 entries on node-A.
	appendEntries(t, nodeA, "h", 100)

	// Simulate replication convergence: update replica seq for B and C.
	// GracefulFailover needs all tracked replicas to have ack'd the current seq.
	cortexSeq := nodeA.repLog.CurrentSeq()
	nodeA.coord.UpdateReplicaSeq("node-B", cortexSeq)
	nodeA.coord.UpdateReplicaSeq("node-C", cortexSeq)

	// Run GracefulFailover: A hands off to B.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err := nodeA.coord.GracefulFailover(ctx, "node-B")
	if err != nil {
		t.Fatalf("GracefulFailover: %v", err)
	}

	// Assert: node-B is now the Cortex.
	if !nodeB.coord.IsLeader() {
		t.Error("node-B should be leader after handoff")
	}
	if nodeA.coord.IsLeader() {
		t.Error("node-A should not be leader after handoff")
	}
	if nodeA.coord.Role() != RoleReplica {
		t.Errorf("node-A role=%v, expected RoleReplica", nodeA.coord.Role())
	}

	// node-B's epoch should be higher than the original.
	if nodeB.coord.CurrentEpoch() <= originalEpoch {
		t.Errorf("node-B epoch=%d, expected > %d", nodeB.coord.CurrentEpoch(), originalEpoch)
	}

	// Verify cognitive flusher was called during handoff.
	if !hebbianFlusher.stopped.Load() {
		t.Error("HebbianFlusher should have been stopped during handoff")
	}

	// Write 50 more entries on new Cortex node-B and deliver them to A and C
	// via HandleIncomingFrame (same pattern as TestIntegration_FullEndToEnd).
	baseSeq := cortexSeq
	for i := 1; i <= 50; i++ {
		key := []byte(fmt.Sprintf("k-h2-%d", i))
		val := []byte(fmt.Sprintf("h2-val-%d", i))
		entry := mbp.ReplEntry{
			Seq:         baseSeq + uint64(i),
			Op:          uint8(OpSet),
			Key:         key,
			Value:       val,
			TimestampNS: time.Now().UnixNano(),
		}
		payload, err := msgpack.Marshal(entry)
		if err != nil {
			t.Fatalf("marshal ReplEntry: %v", err)
		}

		// Deliver to node-A (now Lobe).
		if err := nodeA.coord.HandleIncomingFrame("node-B", mbp.TypeReplEntry, payload); err != nil {
			t.Fatalf("HandleIncomingFrame on node-A entry %d: %v", i, err)
		}
		// Deliver to node-C.
		if err := nodeC.coord.HandleIncomingFrame("node-B", mbp.TypeReplEntry, payload); err != nil {
			t.Fatalf("HandleIncomingFrame on node-C entry %d: %v", i, err)
		}
	}

	expectedSeq := baseSeq + 50

	// Wait for node-A and node-C to apply the new entries.
	waitFor(t, 5*time.Second, func() bool {
		return nodeA.applier.LastApplied() >= expectedSeq
	}, fmt.Sprintf("node-A applier.LastApplied() >= %d", expectedSeq))

	waitFor(t, 5*time.Second, func() bool {
		return nodeC.applier.LastApplied() >= expectedSeq
	}, fmt.Sprintf("node-C applier.LastApplied() >= %d", expectedSeq))

	// Verify the 50 new entries are present on A and C.
	for i := 1; i <= 50; i++ {
		key := []byte(fmt.Sprintf("k-h2-%d", i))
		want := fmt.Sprintf("h2-val-%d", i)

		// Check node-A (demoted to Lobe).
		val, closer, err := nodeA.db.Get(key)
		if err != nil {
			t.Errorf("node-A missing k-h2-%d: %v", i, err)
		} else {
			if string(val) != want {
				t.Errorf("node-A k-h2-%d: got %q, want %q", i, val, want)
			}
			closer.Close()
		}

		// Check node-C.
		val, closer, err = nodeC.db.Get(key)
		if err != nil {
			t.Errorf("node-C missing k-h2-%d: %v", i, err)
		} else {
			if string(val) != want {
				t.Errorf("node-C k-h2-%d: got %q, want %q", i, val, want)
			}
			closer.Close()
		}
	}

	t.Logf("GracefulHandoff: epoch %d -> %d, A demoted to Lobe, "+
		"B promoted to Cortex, 50 new entries replicated to all nodes",
		originalEpoch, nodeB.coord.CurrentEpoch())
}
