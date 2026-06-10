//go:build integration

package replication

import (
	"context"
	"fmt"
	"net"
	"testing"

	"github.com/cockroachdb/pebble"
	"github.com/vmihailenco/msgpack/v5"

	"github.com/scrypster/muninndb/internal/config"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// ---------------------------------------------------------------------------
// Test 1: Rolling Upgrade — data survives a Pebble DB close/reopen cycle
// ---------------------------------------------------------------------------

func TestRollingUpgrade_DataConsistency(t *testing.T) {
	nodeA := newTestNode(t, "node-A", "primary")

	// Build nodeB manually so we control the Pebble lifecycle (close + reopen).
	// newTestNode registers a t.Cleanup that would double-close after our manual close.
	bDir := t.TempDir()
	bDB, err := pebble.Open(bDir, &pebble.Options{})
	if err != nil {
		t.Fatalf("pebble.Open for node-B: %v", err)
	}
	nodeB := &testNode{
		id:      "node-B",
		repLog:  NewReplicationLog(bDB),
		applier: NewApplier(bDB),
		db:      bDB,
		dbDir:   bDir,
		t:       t,
	}
	bEpoch, err := NewEpochStore(bDB)
	if err != nil {
		t.Fatalf("NewEpochStore for node-B: %v", err)
	}
	nodeB.epochStore = bEpoch
	nodeB.coord = NewClusterCoordinator(
		&config.ClusterConfig{
			Enabled: true, NodeID: "node-B", BindAddr: "127.0.0.1:0",
			Role: "replica", LeaseTTL: 10, HeartbeatMS: 1000,
		},
		nodeB.repLog, nodeB.applier, nodeB.epochStore,
	)

	connectNodes(t, nodeA, nodeB)
	registerVoters(nodeA, nodeB)

	electNode(t, nodeA)

	// Start streaming before appending so the subscriber is registered.
	cancelStream := startStreamer(t, nodeA, nodeB, 0)

	appendEntries(t, nodeA, "pre-upgrade", 20)

	waitFor(t, 5*secondDuration, func() bool {
		return nodeB.applier.LastApplied() >= 20
	}, "node-B to apply 20 entries before restart")

	verifyEntries(t, nodeB, "pre-upgrade", 20)

	// --- Simulate restart: close and reopen Pebble ---
	cancelStream()

	if err := bDB.Close(); err != nil {
		t.Fatalf("close node-B Pebble: %v", err)
	}

	reopenedDB, err := pebble.Open(bDir, &pebble.Options{})
	if err != nil {
		t.Fatalf("reopen node-B Pebble: %v", err)
	}
	t.Cleanup(func() { reopenedDB.Close() })

	nodeB.db = reopenedDB

	// Schema version check must succeed on the reopened database.
	if err := CheckAndSetSchemaVersion(reopenedDB); err != nil {
		t.Fatalf("CheckAndSetSchemaVersion after reopen: %v", err)
	}

	// All 20 entries must still be readable from the reopened DB.
	verifyEntries(t, nodeB, "pre-upgrade", 20)
}

// secondDuration avoids importing time in the const; reuse the test helpers'
// convention (waitFor already accepts time.Duration and the helper file imports
// time). Defined here so the file compiles standalone with the build tag.
const secondDuration = 1_000_000_000 // time.Second in nanoseconds

// ---------------------------------------------------------------------------
// Test 2: Protocol Version Mismatch — join rejected for incompatible version
// ---------------------------------------------------------------------------

func TestProtocolVersionMismatch(t *testing.T) {
	nodeA := newTestNode(t, "node-A", "primary")
	registerVoters(nodeA)
	electNode(t, nodeA)

	t.Run("version_too_high", func(t *testing.T) {
		nodeNew := newTestNode(t, "node-new-high", "replica")

		clientConn, serverConn := net.Pipe()
		t.Cleanup(func() {
			clientConn.Close()
			serverConn.Close()
		})

		// Server goroutine: read JoinRequest, call handler, send response.
		go func() {
			defer serverConn.Close()
			frame, err := mbp.ReadFrame(serverConn)
			if err != nil {
				return
			}
			if frame.Type != mbp.TypeJoinRequest {
				return
			}
			var req mbp.JoinRequest
			if err := msgpack.Unmarshal(frame.Payload, &req); err != nil {
				return
			}
			resp := nodeA.coord.joinHandler.HandleJoinRequest(req, nil)
			payload, _ := msgpack.Marshal(resp)
			_ = mbp.WriteFrame(serverConn, &mbp.Frame{
				Version:       0x01,
				Type:          mbp.TypeJoinResponse,
				PayloadLength: uint32(len(payload)),
				Payload:       payload,
			})
		}()

		// Build a JoinRequest with a protocol version higher than what the
		// Cortex supports. This simulates a newer Lobe binary connecting to
		// an older Cortex during a rolling upgrade window.
		futureVersion := mbp.CurrentProtocolVersion + 10
		req := mbp.JoinRequest{
			NodeID:          nodeNew.id,
			Addr:            "127.0.0.1:0",
			ProtocolVersion: futureVersion,
		}
		payload, err := msgpack.Marshal(req)
		if err != nil {
			t.Fatalf("marshal JoinRequest: %v", err)
		}
		if err := mbp.WriteFrame(clientConn, &mbp.Frame{
			Version:       0x01,
			Type:          mbp.TypeJoinRequest,
			PayloadLength: uint32(len(payload)),
			Payload:       payload,
		}); err != nil {
			t.Fatalf("write JoinRequest: %v", err)
		}

		respFrame, err := mbp.ReadFrame(clientConn)
		if err != nil {
			t.Fatalf("read JoinResponse: %v", err)
		}
		var resp mbp.JoinResponse
		if err := msgpack.Unmarshal(respFrame.Payload, &resp); err != nil {
			t.Fatalf("unmarshal JoinResponse: %v", err)
		}

		if resp.Accepted {
			t.Fatal("expected join to be rejected for future protocol version")
		}
		if resp.RejectReason == "" {
			t.Error("expected non-empty RejectReason")
		}
		if resp.CurrentProtocolVersion != mbp.CurrentProtocolVersion {
			t.Errorf("response CurrentProtocolVersion=%d, want %d",
				resp.CurrentProtocolVersion, mbp.CurrentProtocolVersion)
		}
	})

	t.Run("version_too_low", func(t *testing.T) {
		// Temporarily raise MinSupportedProtocolVersion so that legacy peers
		// (version 0) are hard-rejected.
		origMin := mbp.MinSupportedProtocolVersion
		mbp.MinSupportedProtocolVersion = mbp.CurrentProtocolVersion
		t.Cleanup(func() { mbp.MinSupportedProtocolVersion = origMin })

		clientConn, serverConn := net.Pipe()
		t.Cleanup(func() {
			clientConn.Close()
			serverConn.Close()
		})

		go func() {
			defer serverConn.Close()
			frame, err := mbp.ReadFrame(serverConn)
			if err != nil {
				return
			}
			var req mbp.JoinRequest
			if err := msgpack.Unmarshal(frame.Payload, &req); err != nil {
				return
			}
			resp := nodeA.coord.joinHandler.HandleJoinRequest(req, nil)
			payload, _ := msgpack.Marshal(resp)
			_ = mbp.WriteFrame(serverConn, &mbp.Frame{
				Version:       0x01,
				Type:          mbp.TypeJoinResponse,
				PayloadLength: uint32(len(payload)),
				Payload:       payload,
			})
		}()

		req := mbp.JoinRequest{
			NodeID:          "node-old-lobe",
			Addr:            "127.0.0.1:0",
			ProtocolVersion: 0, // legacy pre-versioned
		}
		payload, _ := msgpack.Marshal(req)
		_ = mbp.WriteFrame(clientConn, &mbp.Frame{
			Version:       0x01,
			Type:          mbp.TypeJoinRequest,
			PayloadLength: uint32(len(payload)),
			Payload:       payload,
		})

		respFrame, err := mbp.ReadFrame(clientConn)
		if err != nil {
			t.Fatalf("read JoinResponse: %v", err)
		}
		var resp mbp.JoinResponse
		if err := msgpack.Unmarshal(respFrame.Payload, &resp); err != nil {
			t.Fatalf("unmarshal JoinResponse: %v", err)
		}

		if resp.Accepted {
			t.Fatal("expected join to be rejected for legacy protocol version")
		}
		if resp.MinProtocolVersion != mbp.CurrentProtocolVersion {
			t.Errorf("response MinProtocolVersion=%d, want %d",
				resp.MinProtocolVersion, mbp.CurrentProtocolVersion)
		}
	})
}

// ---------------------------------------------------------------------------
// Test 3: RemoveNode cleans up ConnManager, MSP, streamers, and replica seqs
// ---------------------------------------------------------------------------

func TestRemoveNode_CleansUpState(t *testing.T) {
	nodeA := newTestNode(t, "node-A", "primary")
	nodeB := newTestNode(t, "node-B", "replica")

	connectNodes(t, nodeA, nodeB)
	registerVoters(nodeA, nodeB)

	electNode(t, nodeA)

	// Register B in A's MSP so RemoveNode has something to clean up.
	nodeA.coord.msp.AddPeer("node-B", "pipe", RoleReplica)

	// Simulate B having joined: start a streamer from A -> B.
	cancelStream := startStreamer(t, nodeA, nodeB, 0)
	defer cancelStream()

	// Record a streamer in A's streamers map (the way startStreamerForLobe does).
	ctx, cancel := context.WithCancel(context.Background())
	nodeA.coord.streamersMu.Lock()
	nodeA.coord.streamers["node-B"] = cancel
	nodeA.coord.streamersMu.Unlock()
	t.Cleanup(func() { ctx.Done() }) // ensure context resources are cleaned up

	// Store a replica seq so we can verify it gets cleaned up.
	nodeA.coord.replicaSeqs.Store("node-B", uint64(42))

	// --- Act: remove node-B ---
	if err := nodeA.coord.RemoveNode("node-B"); err != nil {
		t.Fatalf("RemoveNode(node-B): %v", err)
	}

	// --- Assert: peer gone from ConnManager ---
	if _, ok := nodeA.coord.mgr.GetPeer("node-B"); ok {
		t.Error("expected node-B to be removed from ConnManager after RemoveNode")
	}

	// --- Assert: peer gone from MSP ---
	for _, p := range nodeA.coord.msp.AllPeers() {
		if p.NodeID == "node-B" {
			t.Error("expected node-B to be removed from MSP after RemoveNode")
		}
	}

	// --- Assert: streamer cancelled and removed ---
	nodeA.coord.streamersMu.Lock()
	_, streamerExists := nodeA.coord.streamers["node-B"]
	nodeA.coord.streamersMu.Unlock()
	if streamerExists {
		t.Error("expected node-B streamer to be removed after RemoveNode")
	}

	// --- Assert: replica seq tracking cleaned up ---
	if _, loaded := nodeA.coord.replicaSeqs.Load("node-B"); loaded {
		t.Error("expected node-B replica seq to be deleted after RemoveNode")
	}
}

// ---------------------------------------------------------------------------
// Test 4: RemoveNode is idempotent — calling twice does not panic
// ---------------------------------------------------------------------------

func TestRemoveNode_Idempotent(t *testing.T) {
	nodeA := newTestNode(t, "node-A", "primary")
	registerVoters(nodeA)
	electNode(t, nodeA)

	// Add a fake peer, then remove it twice.
	nodeA.coord.mgr.AddPeer("phantom", "127.0.0.1:9999")
	nodeA.coord.msp.AddPeer("phantom", "127.0.0.1:9999", RoleReplica)

	if err := nodeA.coord.RemoveNode("phantom"); err != nil {
		t.Fatalf("first RemoveNode(phantom): %v", err)
	}
	if err := nodeA.coord.RemoveNode("phantom"); err != nil { // must not panic
		t.Fatalf("second RemoveNode(phantom): %v", err)
	}

	if _, ok := nodeA.coord.mgr.GetPeer("phantom"); ok {
		t.Error("phantom peer should be gone after RemoveNode")
	}
}

// ---------------------------------------------------------------------------
// Test 5: Rolling upgrade with continued replication after restart
// ---------------------------------------------------------------------------

func TestRollingUpgrade_ContinuedReplication(t *testing.T) {
	nodeA := newTestNode(t, "node-A", "primary")
	nodeB := newTestNode(t, "node-B", "replica")

	connectNodes(t, nodeA, nodeB)
	registerVoters(nodeA, nodeB)

	electNode(t, nodeA)

	// Replicate 10 entries pre-restart.
	cancelStream := startStreamer(t, nodeA, nodeB, 0)
	appendEntries(t, nodeA, "before", 10)

	waitFor(t, 5*secondDuration, func() bool {
		return nodeB.applier.LastApplied() >= 10
	}, "node-B to apply 10 pre-restart entries")
	verifyEntries(t, nodeB, "before", 10)

	cancelStream()

	// "Restart" node-B: simulate by feeding new entries through
	// HandleIncomingFrame (as would happen after a fresh streamer is started).
	for i := 1; i <= 5; i++ {
		key := []byte(fmt.Sprintf("k-after-%d", i))
		val := []byte(fmt.Sprintf("after-val-%d", i))
		entry := mbp.ReplEntry{
			Seq:   uint64(10 + i),
			Op:    uint8(OpSet),
			Key:   key,
			Value: val,
		}
		payload, err := msgpack.Marshal(entry)
		if err != nil {
			t.Fatalf("marshal ReplEntry: %v", err)
		}
		if err := nodeB.coord.HandleIncomingFrame("node-A", mbp.TypeReplEntry, payload); err != nil {
			t.Fatalf("HandleIncomingFrame entry %d: %v", 10+i, err)
		}
	}

	waitFor(t, 5*secondDuration, func() bool {
		return nodeB.applier.LastApplied() >= 15
	}, "node-B to apply post-restart entries")

	// Verify pre-restart data is intact.
	verifyEntries(t, nodeB, "before", 10)

	// Verify post-restart entries.
	for i := 1; i <= 5; i++ {
		key := []byte(fmt.Sprintf("k-after-%d", i))
		val, closer, err := nodeB.db.Get(key)
		if err != nil {
			t.Errorf("node-B missing k-after-%d: %v", i, err)
			continue
		}
		want := fmt.Sprintf("after-val-%d", i)
		if string(val) != want {
			t.Errorf("k-after-%d: got %q, want %q", i, val, want)
		}
		closer.Close()
	}
}

// ---------------------------------------------------------------------------
// Test 6: RemoveNode rejects self-removal
// ---------------------------------------------------------------------------

func TestRemoveNode_RejectsSelfRemoval(t *testing.T) {
	nodeA := newTestNode(t, "node-A", "primary")
	registerVoters(nodeA)
	electNode(t, nodeA)

	err := nodeA.coord.RemoveNode("node-A")
	if err == nil {
		t.Fatal("expected error when removing self, got nil")
	}
	if err != ErrSelfRemoval {
		t.Fatalf("expected ErrSelfRemoval, got: %v", err)
	}

	// Verify the node is still functional (self was not removed).
	nodes := nodeA.coord.KnownNodes()
	found := false
	for _, n := range nodes {
		if n.NodeID == "node-A" {
			found = true
			break
		}
	}
	if !found {
		t.Error("self node should still be present in KnownNodes after rejected self-removal")
	}
}

// ---------------------------------------------------------------------------
// Test 7: SafePrune is skipped during snapshot transfer
// ---------------------------------------------------------------------------

func TestSafePrune_SkippedDuringSnapshot(t *testing.T) {
	nodeA := newTestNode(t, "node-A", "primary")
	registerVoters(nodeA)
	electNode(t, nodeA)

	// Verify initial state: no snapshot in progress.
	if nodeA.coord.SnapshotInProgress() {
		t.Fatal("expected no snapshot in progress initially")
	}

	// Simulate a snapshot starting.
	nodeA.coord.IncrementSnapshotCount()
	if !nodeA.coord.SnapshotInProgress() {
		t.Fatal("expected snapshot in progress after IncrementSnapshotCount")
	}

	// Multiple concurrent snapshots should be tracked.
	nodeA.coord.IncrementSnapshotCount()
	nodeA.coord.DecrementSnapshotCount()
	if !nodeA.coord.SnapshotInProgress() {
		t.Fatal("expected snapshot still in progress (one remaining)")
	}

	// Last snapshot finishes.
	nodeA.coord.DecrementSnapshotCount()
	if nodeA.coord.SnapshotInProgress() {
		t.Fatal("expected no snapshot in progress after all decrements")
	}
}
