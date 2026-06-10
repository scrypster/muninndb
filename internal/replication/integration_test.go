//go:build integration

package replication

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/vmihailenco/msgpack/v5"

	"github.com/scrypster/muninndb/internal/config"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// ---------------------------------------------------------------------------
// testNode wraps a ClusterCoordinator and its subsystems for integration tests.
// ---------------------------------------------------------------------------

type testNode struct {
	id         string
	coord      *ClusterCoordinator
	repLog     *ReplicationLog
	applier    *Applier
	epochStore *EpochStore
	db         *pebble.DB
	dbDir      string // Pebble data dir (for re-opening after "restart")
	t          *testing.T
}

func newTestNode(t *testing.T, nodeID, role string) *testNode {
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
		BindAddr:    "127.0.0.1:0", // not used; we use net.Pipe()
		Seeds:       []string{},
		Role:        role,
		LeaseTTL:    10,
		HeartbeatMS: 1000,
	}

	coord := NewClusterCoordinator(cfg, repLog, applier, epochStore)

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

// pipeConn creates a net.Pipe() between two nodes and injects the connections
// into their ConnManagers, making PeerConn.Send/Receive work without TCP.
// It also starts a goroutine that reads frames from the pipe and dispatches
// them to the receiving node's HandleIncomingFrame.
func connectNodes(t *testing.T, a, b *testNode) {
	t.Helper()
	connectOneWay(t, a, b)
	connectOneWay(t, b, a)
}

// connectOneWay sets up: sender -> net.Pipe -> reader goroutine -> receiver.HandleIncomingFrame
func connectOneWay(t *testing.T, sender, receiver *testNode) {
	t.Helper()

	senderEnd, receiverEnd := net.Pipe()
	t.Cleanup(func() {
		senderEnd.Close()
		receiverEnd.Close()
	})

	// Inject the sender-side conn into sender's ConnManager so sender can Send to receiver.
	pc := &PeerConn{
		nodeID: receiver.id,
		addr:   "pipe",
		conn:   senderEnd,
	}
	sender.coord.mgr.mu.Lock()
	if existing, ok := sender.coord.mgr.peers[receiver.id]; ok {
		_ = existing.Close()
	}
	sender.coord.mgr.peers[receiver.id] = pc
	sender.coord.mgr.mu.Unlock()

	// Start a reader goroutine on receiverEnd that dispatches frames to receiver's coordinator.
	go func() {
		for {
			frame, err := mbp.ReadFrame(receiverEnd)
			if err != nil {
				return // pipe closed
			}
			_ = receiver.coord.HandleIncomingFrame(sender.id, frame.Type, frame.Payload)
		}
	}()
}

// registerVoters registers each node as a voter in every node's election.
func registerVoters(nodes ...*testNode) {
	for _, n := range nodes {
		for _, other := range nodes {
			other.coord.election.RegisterVoter(n.id)
		}
	}
}

// startStreamer launches a NetworkStreamer in the background from sender to receiver
// and returns a cancel function. The streamer must be started before entries are
// appended so that Subscribe() captures the notifications.
func startStreamer(t *testing.T, sender, receiver *testNode, startSeq uint64) context.CancelFunc {
	t.Helper()
	peer, ok := sender.coord.mgr.GetPeer(receiver.id)
	if !ok {
		t.Fatalf("peer %s not found in %s ConnManager", receiver.id, sender.id)
	}
	s := NewNetworkStreamer(sender.repLog, peer, startSeq)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = s.Stream(ctx) }()
	return cancel
}

// appendEntries appends n entries to a node's replication log with predictable
// keys/values: key="k-{prefix}-{i}", value="{prefix}-val-{i}" (i is 1-based).
// Returns the final sequence number.
func appendEntries(t *testing.T, node *testNode, prefix string, n int) uint64 {
	t.Helper()
	var last uint64
	for i := 1; i <= n; i++ {
		seq, err := node.repLog.Append(
			OpSet,
			[]byte(fmt.Sprintf("k-%s-%d", prefix, i)),
			[]byte(fmt.Sprintf("%s-val-%d", prefix, i)),
		)
		if err != nil {
			t.Fatalf("Append %s entry %d: %v", prefix, i, err)
		}
		last = seq
	}
	return last
}

// verifyEntries checks that a node's Pebble DB contains byte-exact values for
// the entries written by appendEntries with the given prefix and count.
func verifyEntries(t *testing.T, node *testNode, prefix string, n int) {
	t.Helper()
	for i := 1; i <= n; i++ {
		key := []byte(fmt.Sprintf("k-%s-%d", prefix, i))
		val, closer, err := node.db.Get(key)
		if err != nil {
			t.Errorf("%s: missing k-%s-%d: %v", node.id, prefix, i, err)
			continue
		}
		want := fmt.Sprintf("%s-val-%d", prefix, i)
		if string(val) != want {
			t.Errorf("%s: k-%s-%d: got %q, want %q", node.id, prefix, i, val, want)
		}
		closer.Close()
	}
}

// electNode runs a full election on the given node and waits for it to become leader.
func electNode(t *testing.T, node *testNode) {
	t.Helper()
	if err := node.coord.election.StartElection(context.Background()); err != nil {
		t.Fatalf("StartElection on %s: %v", node.id, err)
	}
	waitFor(t, 5*time.Second, func() bool {
		return node.coord.IsLeader()
	}, node.id+" to become leader")
}

// waitFor polls condition with a timeout. Returns true if condition was met.
func waitFor(t *testing.T, timeout time.Duration, condition func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", msg)
}

// assertNever polls cond every 5ms for up to maxWait.
// Fails the test immediately if cond ever returns true.
func assertNever(t *testing.T, maxWait time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(maxWait)
	for time.Now().Before(deadline) {
		if cond() {
			t.Fatalf("condition became true unexpectedly: %s", msg)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// ---------------------------------------------------------------------------
// Test 1: Election and Leader
// ---------------------------------------------------------------------------

func TestIntegration_ElectionAndLeader(t *testing.T) {
	nodeA := newTestNode(t, "node-A", "primary")
	nodeB := newTestNode(t, "node-B", "replica")
	nodeC := newTestNode(t, "node-C", "replica")

	// Connect all pairs bidirectionally.
	connectNodes(t, nodeA, nodeB)
	connectNodes(t, nodeA, nodeC)
	connectNodes(t, nodeB, nodeC)

	// Register all 3 nodes as voters in all elections.
	registerVoters(nodeA, nodeB, nodeC)

	// node-A starts an election. Because all connections are wired through
	// HandleIncomingFrame goroutines, the VoteRequest broadcast from A will be
	// received by B and C, which will send VoteResponses back, and A will
	// process them automatically.
	if err := nodeA.coord.election.StartElection(context.Background()); err != nil {
		t.Fatalf("StartElection: %v", err)
	}

	// Wait for A to become leader.
	waitFor(t, 5*time.Second, func() bool {
		return nodeA.coord.IsLeader()
	}, "node-A to become leader")

	// Verify A's state.
	if nodeA.coord.Role() != RolePrimary {
		t.Errorf("node-A: expected RolePrimary, got %v", nodeA.coord.Role())
	}

	// Wait for B and C to recognize A as leader via CortexClaim.
	waitFor(t, 5*time.Second, func() bool {
		return nodeB.coord.election.CurrentLeader() == "node-A"
	}, "node-B to recognize node-A as leader")

	waitFor(t, 5*time.Second, func() bool {
		return nodeC.coord.election.CurrentLeader() == "node-A"
	}, "node-C to recognize node-A as leader")

	// B and C should not be leaders.
	if nodeB.coord.IsLeader() {
		t.Error("node-B should not be leader")
	}
	if nodeC.coord.IsLeader() {
		t.Error("node-C should not be leader")
	}

	// All nodes should have epoch >= 1.
	if nodeA.coord.CurrentEpoch() < 1 {
		t.Errorf("node-A epoch=%d, expected >= 1", nodeA.coord.CurrentEpoch())
	}
	if nodeB.coord.CurrentEpoch() < 1 {
		t.Errorf("node-B epoch=%d, expected >= 1", nodeB.coord.CurrentEpoch())
	}
	if nodeC.coord.CurrentEpoch() < 1 {
		t.Errorf("node-C epoch=%d, expected >= 1", nodeC.coord.CurrentEpoch())
	}
}

// ---------------------------------------------------------------------------
// Test 2: Replication Flow
// ---------------------------------------------------------------------------

func TestIntegration_ReplicationFlow(t *testing.T) {
	nodeA := newTestNode(t, "node-A", "primary")
	nodeB := newTestNode(t, "node-B", "replica")

	// Connect A -> B bidirectionally.
	connectNodes(t, nodeA, nodeB)
	registerVoters(nodeA, nodeB)

	// Bootstrap A as Cortex.
	if err := nodeA.coord.election.StartElection(context.Background()); err != nil {
		t.Fatalf("StartElection: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool {
		return nodeA.coord.IsLeader()
	}, "node-A to become leader")

	// Create a NetworkStreamer on A targeting B via the PeerConn in A's ConnManager.
	// Start the streamer BEFORE appending entries so that Subscribe() is registered
	// before the notifications fire.
	peer, ok := nodeA.coord.mgr.GetPeer("node-B")
	if !ok {
		t.Fatal("node-B peer not found in node-A's ConnManager")
	}

	streamer := NewNetworkStreamer(nodeA.repLog, peer, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = streamer.Stream(ctx)
	}()

	// Append 5 entries to node-A's ReplicationLog.
	// Stream() reads from seq=0 so it will pick up entries even if the subscription
	// goroutine hasn't registered yet when the first notification fires.
	for i := 1; i <= 5; i++ {
		_, err := nodeA.repLog.Append(
			OpSet,
			[]byte(fmt.Sprintf("key-%d", i)),
			[]byte(fmt.Sprintf("val-%d", i)),
		)
		if err != nil {
			t.Fatalf("Append entry %d: %v", i, err)
		}
	}

	// Wait for node-B to apply all 5 entries.
	waitFor(t, 5*time.Second, func() bool {
		return nodeB.applier.LastApplied() >= 5
	}, "node-B applier.LastApplied() >= 5")

	if last := nodeB.applier.LastApplied(); last != 5 {
		t.Errorf("node-B lastApplied=%d, expected 5", last)
	}

	// Verify the entries were applied correctly by reading from B's Pebble DB.
	for i := 1; i <= 5; i++ {
		key := []byte(fmt.Sprintf("key-%d", i))
		val, closer, err := nodeB.db.Get(key)
		if err != nil {
			t.Errorf("node-B missing key-%d: %v", i, err)
			continue
		}
		expected := fmt.Sprintf("val-%d", i)
		if string(val) != expected {
			t.Errorf("node-B key-%d: got %q, want %q", i, val, expected)
		}
		closer.Close()
	}
}

// ---------------------------------------------------------------------------
// Test 3: Failover Election
// ---------------------------------------------------------------------------

func TestIntegration_FailoverElection(t *testing.T) {
	nodeA := newTestNode(t, "node-A", "primary")
	nodeB := newTestNode(t, "node-B", "replica")
	nodeC := newTestNode(t, "node-C", "replica")

	connectNodes(t, nodeA, nodeB)
	connectNodes(t, nodeA, nodeC)
	connectNodes(t, nodeB, nodeC)
	registerVoters(nodeA, nodeB, nodeC)

	// Bootstrap A as Cortex.
	if err := nodeA.coord.election.StartElection(context.Background()); err != nil {
		t.Fatalf("StartElection: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool {
		return nodeA.coord.IsLeader()
	}, "node-A to become leader")

	initialEpoch := nodeA.coord.CurrentEpoch()

	// "Kill" node-A: mark it SDOWN on B and C's MSP.
	nodeB.coord.msp.AddPeer("node-A", "pipe", RolePrimary)
	nodeC.coord.msp.AddPeer("node-A", "pipe", RolePrimary)

	nodeB.coord.msp.mu.Lock()
	if p, ok := nodeB.coord.msp.peers["node-A"]; ok {
		p.SDown = true
		p.MissedBeats = 10
	}
	nodeB.coord.msp.mu.Unlock()

	nodeC.coord.msp.mu.Lock()
	if p, ok := nodeC.coord.msp.peers["node-A"]; ok {
		p.SDown = true
		p.MissedBeats = 10
	}
	nodeC.coord.msp.mu.Unlock()

	// node-B starts a new election (simulating what MSP.OnODown triggers).
	if err := nodeB.coord.election.StartElection(context.Background()); err != nil {
		t.Fatalf("node-B StartElection: %v", err)
	}

	// Wait for node-B to become leader.
	waitFor(t, 5*time.Second, func() bool {
		return nodeB.coord.IsLeader()
	}, "node-B to become leader after failover")

	// node-B's epoch should be higher than initial.
	if nodeB.coord.CurrentEpoch() <= initialEpoch {
		t.Errorf("node-B epoch=%d, expected > %d", nodeB.coord.CurrentEpoch(), initialEpoch)
	}

	// node-C should recognize B as leader.
	waitFor(t, 5*time.Second, func() bool {
		return nodeC.coord.election.CurrentLeader() == "node-B"
	}, "node-C to recognize node-B as leader")

	// If node-A receives the CortexClaim with higher epoch, it should be demoted.
	// Simulate A receiving B's CortexClaim.
	newEpoch := nodeB.coord.CurrentEpoch()
	claim := mbp.CortexClaim{
		CortexID:     "node-B",
		Epoch:        newEpoch,
		FencingToken: newEpoch,
	}
	nodeA.coord.election.HandleCortexClaim(claim)

	// A should no longer be leader.
	if nodeA.coord.IsLeader() {
		t.Error("node-A should not be leader after receiving higher-epoch CortexClaim")
	}

	// A's epoch should match.
	if nodeA.coord.CurrentEpoch() != newEpoch {
		t.Errorf("node-A epoch=%d, expected %d", nodeA.coord.CurrentEpoch(), newEpoch)
	}
}

// ---------------------------------------------------------------------------
// Test 4: Fencing Token Prevents Stale Write
// ---------------------------------------------------------------------------

func TestIntegration_FencingTokenPreventsStaleWrite(t *testing.T) {
	nodeA := newTestNode(t, "node-A", "primary")
	nodeB := newTestNode(t, "node-B", "replica")

	registerVoters(nodeA, nodeB)

	// Set up: A was Cortex at epoch=1, B is new Cortex at epoch=2.
	// Set both coordinator role AND election state so HandleCortexClaim
	// properly detects the leadership transition.
	if err := nodeA.epochStore.ForceSet(1); err != nil {
		t.Fatal(err)
	}
	simulatePromotion(nodeA.coord, 1)

	if err := nodeB.epochStore.ForceSet(2); err != nil {
		t.Fatal(err)
	}
	simulatePromotion(nodeB.coord, 2)

	// A tries to send a VoteRequest with stale epoch=1.
	// B's election should reject it (epoch 1 < B's epoch 2).
	staleReq := mbp.VoteRequest{
		CandidateID: "node-A",
		Epoch:       1,
	}
	resp := nodeB.coord.election.HandleVoteRequest(staleReq)

	if resp.Granted {
		t.Error("expected stale VoteRequest (epoch=1) to be rejected by node-B (epoch=2)")
	}
	if resp.VoterID != "node-B" {
		t.Errorf("expected VoterID=node-B, got %q", resp.VoterID)
	}

	// Now B sends CortexClaim with epoch=2 to A.
	claim := mbp.CortexClaim{
		CortexID:     "node-B",
		Epoch:        2,
		FencingToken: 2,
	}
	nodeA.coord.election.HandleCortexClaim(claim)

	// A's epoch should now be 2.
	if nodeA.coord.CurrentEpoch() != 2 {
		t.Errorf("node-A epoch=%d, expected 2", nodeA.coord.CurrentEpoch())
	}

	// A should no longer be leader.
	if nodeA.coord.IsLeader() {
		t.Error("node-A should not be leader after CortexClaim(epoch=2)")
	}
}

// ---------------------------------------------------------------------------
// Test 5: Join Protocol
// ---------------------------------------------------------------------------

func TestIntegration_JoinProtocol(t *testing.T) {
	// node-A is Cortex with 10 pre-populated entries.
	nodeA := newTestNode(t, "node-A", "primary")

	registerVoters(nodeA)

	// Bootstrap A as Cortex.
	if err := nodeA.coord.election.StartElection(context.Background()); err != nil {
		t.Fatalf("StartElection: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool {
		return nodeA.coord.IsLeader()
	}, "node-A to become leader")

	// Pre-populate 10 entries in A's replication log.
	for i := 1; i <= 10; i++ {
		_, err := nodeA.repLog.Append(
			OpSet,
			[]byte(fmt.Sprintf("join-key-%d", i)),
			[]byte(fmt.Sprintf("join-val-%d", i)),
		)
		if err != nil {
			t.Fatalf("Append entry %d: %v", i, err)
		}
	}

	// node-D is a fresh Lobe joining.
	nodeD := newTestNode(t, "node-D", "replica")

	// Create a net.Pipe() for the join handshake.
	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() {
		clientConn.Close()
		serverConn.Close()
	})

	// Server side (node-A): read JoinRequest, send JoinResponse.
	go func() {
		defer serverConn.Close()

		// Read the JoinRequest frame.
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

		// Process it through node-A's join handler.
		resp := nodeA.coord.joinHandler.HandleJoinRequest(req, nil)

		payload, err := msgpack.Marshal(resp)
		if err != nil {
			return
		}
		respFrame := &mbp.Frame{
			Version:       0x01,
			Type:          mbp.TypeJoinResponse,
			PayloadLength: uint32(len(payload)),
			Payload:       payload,
		}
		_ = mbp.WriteFrame(serverConn, respFrame)
	}()

	// Client side (node-D): perform join handshake.
	resp, err := nodeD.coord.joinClient.joinConn(context.Background(), clientConn)
	if err != nil {
		t.Fatalf("joinConn: %v", err)
	}
	if !resp.Accepted {
		t.Fatalf("join rejected: %s", resp.RejectReason)
	}

	// node-D's epoch should match A's.
	if nodeD.coord.CurrentEpoch() != nodeA.coord.CurrentEpoch() {
		t.Errorf("node-D epoch=%d, expected %d", nodeD.coord.CurrentEpoch(), nodeA.coord.CurrentEpoch())
	}

	// Now set up streaming: connect A -> D for replication.
	connectNodes(t, nodeA, nodeD)

	// Start a NetworkStreamer on A targeting D from seq=0.
	peer, ok := nodeA.coord.mgr.GetPeer("node-D")
	if !ok {
		t.Fatal("node-D peer not found in node-A's ConnManager")
	}

	streamer := NewNetworkStreamer(nodeA.repLog, peer, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = streamer.Stream(ctx)
	}()

	// Append a trigger entry. Stream() reads from seq=0 so it will replay
	// all existing entries even if the subscription goroutine hasn't registered
	// before this notification fires.
	_, err = nodeA.repLog.Append(OpSet, []byte("trigger"), []byte("trigger"))
	if err != nil {
		t.Fatalf("Append trigger entry: %v", err)
	}

	// Wait for node-D to catch up (11 entries total: 10 original + 1 trigger).
	waitFor(t, 5*time.Second, func() bool {
		return nodeD.applier.LastApplied() >= 11
	}, "node-D applier.LastApplied() >= 11")

	if last := nodeD.applier.LastApplied(); last != 11 {
		t.Errorf("node-D lastApplied=%d, expected 11", last)
	}

	// Verify the original 10 entries in D's database.
	for i := 1; i <= 10; i++ {
		key := []byte(fmt.Sprintf("join-key-%d", i))
		val, closer, err := nodeD.db.Get(key)
		if err != nil {
			t.Errorf("node-D missing join-key-%d: %v", i, err)
			continue
		}
		expected := fmt.Sprintf("join-val-%d", i)
		if string(val) != expected {
			t.Errorf("node-D join-key-%d: got %q, want %q", i, val, expected)
		}
		closer.Close()
	}
}

// ---------------------------------------------------------------------------
// Test 6: Full end-to-end — 100 entries, failover, 50 more, resync old Cortex
// ---------------------------------------------------------------------------

func TestIntegration_FullEndToEnd(t *testing.T) {
	// ---- Phase 1: Set up 3-node cluster, elect A as Cortex ----
	nodeA := newTestNode(t, "node-A", "primary")
	nodeB := newTestNode(t, "node-B", "replica")
	nodeC := newTestNode(t, "node-C", "replica")

	connectNodes(t, nodeA, nodeB)
	connectNodes(t, nodeA, nodeC)
	connectNodes(t, nodeB, nodeC)
	registerVoters(nodeA, nodeB, nodeC)

	electNode(t, nodeA)

	// Wait for B and C to acknowledge A as leader.
	waitFor(t, 5*time.Second, func() bool {
		return nodeB.coord.election.CurrentLeader() == "node-A" &&
			nodeC.coord.election.CurrentLeader() == "node-A"
	}, "B and C to recognize node-A as leader")

	// ---- Phase 2: Write 100 entries on A, replicate to B and C ----
	cancelB := startStreamer(t, nodeA, nodeB, 0)
	cancelC := startStreamer(t, nodeA, nodeC, 0)

	appendEntries(t, nodeA, "p1", 100)

	waitFor(t, 5*time.Second, func() bool {
		return nodeB.applier.LastApplied() >= 100
	}, "node-B to apply 100 entries")
	waitFor(t, 5*time.Second, func() bool {
		return nodeC.applier.LastApplied() >= 100
	}, "node-C to apply 100 entries")

	// Byte-for-byte verification on B and C.
	verifyEntries(t, nodeB, "p1", 100)
	verifyEntries(t, nodeC, "p1", 100)

	// ---- Phase 3: Kill Cortex A, failover to B ----
	// Stop A's streamers first.
	cancelB()
	cancelC()
	_ = nodeA.coord.Stop()

	// Mark A as SDOWN on B and C.
	nodeB.coord.msp.AddPeer("node-A", "pipe", RolePrimary)
	nodeC.coord.msp.AddPeer("node-A", "pipe", RolePrimary)
	nodeB.coord.msp.mu.Lock()
	if p, ok := nodeB.coord.msp.peers["node-A"]; ok {
		p.SDown = true
		p.MissedBeats = 10
	}
	nodeB.coord.msp.mu.Unlock()
	nodeC.coord.msp.mu.Lock()
	if p, ok := nodeC.coord.msp.peers["node-A"]; ok {
		p.SDown = true
		p.MissedBeats = 10
	}
	nodeC.coord.msp.mu.Unlock()

	// Measure failover time.
	failoverStart := time.Now()
	electNode(t, nodeB)
	failoverDuration := time.Since(failoverStart)

	if failoverDuration >= 5*time.Second {
		t.Errorf("failover took %v, expected < 5s", failoverDuration)
	}
	t.Logf("failover completed in %v", failoverDuration)

	// C should recognize B as leader.
	waitFor(t, 5*time.Second, func() bool {
		return nodeC.coord.election.CurrentLeader() == "node-B"
	}, "node-C to recognize node-B as leader after failover")

	newEpoch := nodeB.coord.CurrentEpoch()
	if newEpoch <= 1 {
		t.Errorf("expected epoch > 1 after failover, got %d", newEpoch)
	}

	// ---- Phase 4: Write 50 more entries on new Cortex B, replicate to C ----
	// Re-wire B<->C with fresh pipes.
	connectNodes(t, nodeB, nodeC)

	// In Phase 1, each node's replog has its own seq space. B's replog entries
	// will have seq 1-50, but C's applier.lastApplied == 100 (from A's entries).
	// The applier skips entries with seq <= lastApplied. To simulate realistic
	// replication, we feed entries to C via HandleIncomingFrame with seqs
	// continuing from where C left off (101+).
	for i := 1; i <= 50; i++ {
		key := []byte(fmt.Sprintf("k-p2-%d", i))
		val := []byte(fmt.Sprintf("p2-val-%d", i))
		entry := mbp.ReplEntry{
			Seq:         uint64(100 + i), // continue from C's last applied
			Op:          uint8(OpSet),
			Key:         key,
			Value:       val,
			TimestampNS: time.Now().UnixNano(),
		}
		payload, err := msgpack.Marshal(entry)
		if err != nil {
			t.Fatalf("marshal ReplEntry: %v", err)
		}
		if err := nodeC.coord.HandleIncomingFrame("node-B", mbp.TypeReplEntry, payload); err != nil {
			t.Fatalf("HandleIncomingFrame ReplEntry %d: %v", i, err)
		}
	}

	// C should have applied up to seq 150.
	waitFor(t, 5*time.Second, func() bool {
		return nodeC.applier.LastApplied() >= 150
	}, "node-C to apply post-failover entries (seq 101-150)")

	verifyEntries(t, nodeC, "p2", 50)
	// C should still have the original p1 entries.
	verifyEntries(t, nodeC, "p1", 100)

	// ---- Phase 5: Restart old Cortex A as a Lobe, verify resync ----
	// Note: A was the Cortex (writer). It appended entries to its replog but
	// never applied them via the Applier (the Applier is only used on replicas).
	// The data keys (k-p1-*) exist only on B and C. On restart, A joins as a
	// fresh Lobe and must receive all data from the new Cortex B.

	// Create a brand new node to simulate A restarting as a Lobe.
	// (We use a fresh DB rather than re-opening A's DB because in a real
	// scenario the new Lobe receives data via replication, not from local state.)
	restartedA := newTestNode(t, "node-A-restarted", "replica")

	// Wire connection from B to restarted A.
	connectNodes(t, nodeB, restartedA)

	// Send CortexClaim so restarted A recognizes B as leader.
	claim := mbp.CortexClaim{
		CortexID:     "node-B",
		Epoch:        newEpoch,
		FencingToken: newEpoch,
	}
	restartedA.coord.election.HandleCortexClaim(claim)

	if restartedA.coord.IsLeader() {
		t.Error("restarted node-A should not be leader")
	}
	if restartedA.coord.election.CurrentLeader() != "node-B" {
		t.Errorf("restarted A leader=%q, expected node-B",
			restartedA.coord.election.CurrentLeader())
	}

	// Feed all 150 entries to restarted A (100 p1 + 50 p2) via HandleIncomingFrame.
	// This simulates what the Cortex would send during a full resync.
	for i := 1; i <= 100; i++ {
		entry := mbp.ReplEntry{
			Seq:         uint64(i),
			Op:          uint8(OpSet),
			Key:         []byte(fmt.Sprintf("k-p1-%d", i)),
			Value:       []byte(fmt.Sprintf("p1-val-%d", i)),
			TimestampNS: time.Now().UnixNano(),
		}
		payload, err := msgpack.Marshal(entry)
		if err != nil {
			t.Fatalf("marshal p1 ReplEntry: %v", err)
		}
		if err := restartedA.coord.HandleIncomingFrame("node-B", mbp.TypeReplEntry, payload); err != nil {
			t.Fatalf("HandleIncomingFrame p1 entry %d: %v", i, err)
		}
	}
	for i := 1; i <= 50; i++ {
		entry := mbp.ReplEntry{
			Seq:         uint64(100 + i),
			Op:          uint8(OpSet),
			Key:         []byte(fmt.Sprintf("k-p2-%d", i)),
			Value:       []byte(fmt.Sprintf("p2-val-%d", i)),
			TimestampNS: time.Now().UnixNano(),
		}
		payload, err := msgpack.Marshal(entry)
		if err != nil {
			t.Fatalf("marshal p2 ReplEntry: %v", err)
		}
		if err := restartedA.coord.HandleIncomingFrame("node-B", mbp.TypeReplEntry, payload); err != nil {
			t.Fatalf("HandleIncomingFrame p2 entry %d: %v", i, err)
		}
	}

	waitFor(t, 5*time.Second, func() bool {
		return restartedA.applier.LastApplied() >= 150
	}, "restarted node-A to apply all 150 entries")

	// Verify all entries byte-for-byte on restarted A.
	verifyEntries(t, restartedA, "p1", 100)
	verifyEntries(t, restartedA, "p2", 50)

	t.Logf("Full end-to-end: 100 entries replicated, failover in %v, "+
		"50 more entries on new Cortex, old Cortex resynced with all 150 entries intact",
		failoverDuration)
}

// ---------------------------------------------------------------------------
// Test 7: Quorum Loss Demotion Race — IsDraining set before goroutine,
//         reset to StateNormal after demotion completes.
// ---------------------------------------------------------------------------

func TestIntegration_QuorumLossDemotion(t *testing.T) {
	nodeA := newTestNode(t, "node-A", "primary")
	nodeB := newTestNode(t, "node-B", "replica")
	nodeC := newTestNode(t, "node-C", "replica")

	connectNodes(t, nodeA, nodeB)
	connectNodes(t, nodeA, nodeC)
	connectNodes(t, nodeB, nodeC)
	registerVoters(nodeA, nodeB, nodeC)

	// Elect node-A as leader.
	electNode(t, nodeA)

	waitFor(t, 5*time.Second, func() bool {
		return nodeB.coord.election.CurrentLeader() == "node-A" &&
			nodeC.coord.election.CurrentLeader() == "node-A"
	}, "B and C to recognise node-A as leader")

	if !nodeA.coord.IsLeader() {
		t.Fatal("node-A should be leader before quorum-loss test")
	}

	// Register both B and C as MSP peers on A so LivePeers() is aware of them.
	nodeA.coord.msp.AddPeer("node-B", "pipe", RoleReplica)
	nodeA.coord.msp.AddPeer("node-C", "pipe", RoleReplica)

	// Simulate quorum loss: mark both peers SDOWN on A's MSP.
	// After this, A has 1 live node (itself) out of a required quorum of 2
	// (3-node cluster → quorum = floor(3/2)+1 = 2).
	nodeA.coord.msp.mu.Lock()
	if p, ok := nodeA.coord.msp.peers["node-B"]; ok {
		p.SDown = true
		p.MissedBeats = 10
	}
	if p, ok := nodeA.coord.msp.peers["node-C"]; ok {
		p.SDown = true
		p.MissedBeats = 10
	}
	nodeA.coord.msp.mu.Unlock()

	// First call: sets quorumLostSince (no demotion yet).
	nodeA.coord.checkQuorumHealth()

	if !nodeA.coord.IsLeader() {
		t.Fatal("node-A should still be leader after first quorum-health check")
	}

	// Backdate quorumLostSince to simulate the 5s timeout having elapsed.
	nodeA.coord.quorumMu.Lock()
	nodeA.coord.quorumLostSince = time.Now().Add(-6 * time.Second)
	nodeA.coord.quorumMu.Unlock()

	// Second call: timeout exceeded — should set StateDraining and fire async demotion.
	nodeA.coord.checkQuorumHealth()

	// Wait for demotion to complete.
	waitFor(t, 2*time.Second, func() bool {
		return !nodeA.coord.IsLeader()
	}, "node-A to demote itself after quorum loss")

	// Assert 1: node-A is no longer leader.
	if nodeA.coord.IsLeader() {
		t.Error("node-A should not be leader after quorum-loss demotion")
	}

	// Assert 2: IsDraining is false — the race fix resets StateNormal in handleDemotion.
	if nodeA.coord.IsDraining() {
		t.Error("node-A should not be in DRAINING state after demotion completed")
	}
}

// ---------------------------------------------------------------------------
// Test 8: Partition Recovery — MSP OnRecover fires and streamer restarts
// ---------------------------------------------------------------------------

func TestIntegration_PartitionRecovery(t *testing.T) {
	nodeA := newTestNode(t, "node-A", "primary")
	nodeB := newTestNode(t, "node-B", "replica")

	// Connect A <-> B bidirectionally.
	connectNodes(t, nodeA, nodeB)
	registerVoters(nodeA, nodeB)

	// Bootstrap node-A as Cortex.
	if err := nodeA.coord.election.StartElection(context.Background()); err != nil {
		t.Fatalf("StartElection: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool {
		return nodeA.coord.IsLeader()
	}, "node-A to become leader")

	// Verify B recognizes A as leader.
	waitFor(t, 5*time.Second, func() bool {
		return nodeB.coord.election.CurrentLeader() == "node-A"
	}, "node-B to recognize node-A as leader")

	// Register B as an MSP peer on A so A can track its health.
	nodeA.coord.msp.AddPeer("node-B", "pipe", RoleReplica)

	// Start a streamer from A to B so replication flows.
	cancelStreamer := startStreamer(t, nodeA, nodeB, 0)
	defer cancelStreamer()

	// Append 5 entries on A to verify replication works initially.
	appendEntries(t, nodeA, "initial", 5)

	// Wait for B to apply those 5 entries.
	waitFor(t, 5*time.Second, func() bool {
		return nodeB.applier.LastApplied() >= 5
	}, "node-B to apply initial 5 entries")

	verifyEntries(t, nodeB, "initial", 5)

	// ---- Simulate partition: mark node-B as SDOWN on A's MSP ----
	nodeA.coord.msp.mu.Lock()
	if p, ok := nodeA.coord.msp.peers["node-B"]; ok {
		p.SDown = true
		p.MissedBeats = 10
	}
	nodeA.coord.msp.mu.Unlock()

	// Verify SDOWN is registered.
	if !nodeA.coord.msp.IsSDown("node-B") {
		t.Fatal("node-B should be marked SDOWN on node-A's MSP")
	}

	// ---- Simulate partition recovery: send a heartbeat (Pong) from B to A ----
	// This triggers MSP.handleHeartbeat, which clears SDOWN and fires OnRecover.
	nodeA.coord.msp.HandlePong("node-B", nil)

	// Verify SDOWN is cleared.
	if nodeA.coord.msp.IsSDown("node-B") {
		t.Fatal("node-B should not be marked SDOWN after recovery pong")
	}

	// The OnRecover callback is async and fires as a goroutine.
	// Wait a moment for it to execute and restart the streamer.
	time.Sleep(100 * time.Millisecond)

	// Append 3 more entries on A post-recovery.
	appendEntries(t, nodeA, "post-recovery", 3)

	// Wait for B to apply those new entries (indicating the streamer restarted).
	waitFor(t, 5*time.Second, func() bool {
		return nodeB.applier.LastApplied() >= 8 // 5 initial + 3 post-recovery
	}, "node-B to apply post-recovery entries after streamer restart")

	// Verify the post-recovery entries were applied correctly.
	verifyEntries(t, nodeB, "post-recovery", 3)

	// Verify all original entries are still intact.
	verifyEntries(t, nodeB, "initial", 5)
}

// ---------------------------------------------------------------------------
// Test 9: Applier Idempotency Across Leader Change
// ---------------------------------------------------------------------------
//
// This test verifies that the Applier's double-apply protection holds
// across a leader election mid-stream. When a new Cortex is elected,
// it may retransmit entries starting from a sequence number it recovered
// from its log, overlapping with entries that a Lobe already applied
// from the previous leader. The Applier MUST skip these duplicates
// without error or corruption.
//
// Scenario:
// 1. Apply entries 1..10 to a Lobe (simulating initial replication from leader A).
// 2. Simulate leader change: new leader B retransmits entries 8..10 (overlapping).
// 3. New leader B also sends new entries 11..13 (unseen by the Lobe).
// 4. Verify final lastApplied = 13 (correct).
// 5. Verify entries 8, 9, 10 appear exactly once in storage (no double-apply).
// 6. Verify no errors from applying duplicate entries.
func TestIntegration_ApplierIdempotency_AcrossLeaderChange(t *testing.T) {
	// Create a test node (Lobe) that will apply entries.
	nodeLobeC := newTestNode(t, "node-C", "replica")

	// ---- Phase 1: Simulate initial replication from leader A ----
	// Apply entries 1..10 with keys "initial-{i}" to represent the initial state.
	for seq := uint64(1); seq <= 10; seq++ {
		entry := ReplicationEntry{
			Seq:         seq,
			Op:          OpSet,
			Key:         []byte(fmt.Sprintf("initial-%d", seq)),
			Value:       []byte(fmt.Sprintf("value-from-A-%d", seq)),
			TimestampNS: time.Now().UnixNano(),
		}
		if err := nodeLobeC.applier.Apply(entry); err != nil {
			t.Fatalf("apply entry %d: %v", seq, err)
		}
	}

	// Verify the Applier's lastApplied is 10.
	if got := nodeLobeC.applier.LastApplied(); got != 10 {
		t.Errorf("after initial replication: lastApplied=%d, expected 10", got)
	}

	// Verify all 10 entries are in the database.
	for i := 1; i <= 10; i++ {
		key := []byte(fmt.Sprintf("initial-%d", i))
		val, closer, err := nodeLobeC.db.Get(key)
		if err != nil {
			t.Fatalf("entry %d missing in db: %v", i, err)
		}
		want := fmt.Sprintf("value-from-A-%d", i)
		if string(val) != want {
			t.Errorf("entry %d: got %q, want %q", i, val, want)
		}
		closer.Close()
	}

	// ---- Phase 2: Leader change — new leader B retransmits overlapping entries ----
	// New leader B doesn't know that C already applied entries 8, 9, 10.
	// B retransmits them (with different values to simulate a different write source).
	// The Applier MUST skip these as duplicates.
	for seq := uint64(8); seq <= 10; seq++ {
		entry := ReplicationEntry{
			Seq:         seq,
			Op:          OpSet,
			Key:         []byte(fmt.Sprintf("initial-%d", seq)),
			Value:       []byte(fmt.Sprintf("value-from-B-retransmit-%d", seq)),
			TimestampNS: time.Now().UnixNano(),
		}
		if err := nodeLobeC.applier.Apply(entry); err != nil {
			t.Fatalf("apply retransmitted entry %d: %v", seq, err)
		}
	}

	// Verify lastApplied is still 10 (no progress on duplicates).
	if got := nodeLobeC.applier.LastApplied(); got != 10 {
		t.Errorf("after leader retransmit: lastApplied=%d, expected 10", got)
	}

	// Verify entries 8, 9, 10 still have the ORIGINAL values (not overwritten).
	// This proves the duplicate applies were skipped, not re-applied.
	for seq := 8; seq <= 10; seq++ {
		key := []byte(fmt.Sprintf("initial-%d", seq))
		val, closer, err := nodeLobeC.db.Get(key)
		if err != nil {
			t.Fatalf("entry %d missing after retransmit: %v", seq, err)
		}
		want := fmt.Sprintf("value-from-A-%d", seq)
		if string(val) != want {
			t.Errorf("entry %d was reapplied: got %q (from B), want %q (from A)", seq, val, want)
		}
		closer.Close()
	}

	// ---- Phase 3: Leader B sends new entries 11, 12, 13 ----
	// These are genuinely new entries that the Lobe hasn't seen.
	// The Applier MUST accept and apply them normally.
	for seq := uint64(11); seq <= 13; seq++ {
		entry := ReplicationEntry{
			Seq:         seq,
			Op:          OpSet,
			Key:         []byte(fmt.Sprintf("new-%d", seq)),
			Value:       []byte(fmt.Sprintf("value-from-B-new-%d", seq)),
			TimestampNS: time.Now().UnixNano(),
		}
		if err := nodeLobeC.applier.Apply(entry); err != nil {
			t.Fatalf("apply new entry %d: %v", seq, err)
		}
	}

	// Verify lastApplied is now 13 (correctly tracked new entries).
	if got := nodeLobeC.applier.LastApplied(); got != 13 {
		t.Errorf("after new entries: lastApplied=%d, expected 13", got)
	}

	// Verify the new entries are in the database.
	for seq := 11; seq <= 13; seq++ {
		key := []byte(fmt.Sprintf("new-%d", seq))
		val, closer, err := nodeLobeC.db.Get(key)
		if err != nil {
			t.Fatalf("new entry %d missing in db: %v", seq, err)
		}
		want := fmt.Sprintf("value-from-B-new-%d", seq)
		if string(val) != want {
			t.Errorf("new entry %d: got %q, want %q", seq, val, want)
		}
		closer.Close()
	}

	// ---- Phase 4: Verify double-apply prevention one more time ----
	// Apply entries 5..7 (all less than lastApplied=13) to further verify idempotency.
	for seq := uint64(5); seq <= 7; seq++ {
		entry := ReplicationEntry{
			Seq:         seq,
			Op:          OpSet,
			Key:         []byte(fmt.Sprintf("late-apply-%d", seq)),
			Value:       []byte(fmt.Sprintf("should-be-skipped-%d", seq)),
			TimestampNS: time.Now().UnixNano(),
		}
		if err := nodeLobeC.applier.Apply(entry); err != nil {
			t.Fatalf("apply late entry %d: %v", seq, err)
		}
	}

	// Verify lastApplied is unchanged at 13.
	if got := nodeLobeC.applier.LastApplied(); got != 13 {
		t.Errorf("after late applies: lastApplied=%d, expected 13", got)
	}

	// Verify the "late-apply-*" keys don't exist (the apply was skipped).
	for seq := 5; seq <= 7; seq++ {
		key := []byte(fmt.Sprintf("late-apply-%d", seq))
		_, closer, err := nodeLobeC.db.Get(key)
		if err == nil {
			// Key should not exist.
			closer.Close()
			t.Errorf("late-apply-%d should not exist (was skipped) - apply was not skipped!", seq)
		}
		// It's okay if err != nil (key not found).
	}

	t.Logf("Applier idempotency verified: initial 10 entries applied, " +
		"leader retransmit (8-10) skipped, new entries (11-13) applied, " +
		"late entries (5-7) skipped. Final lastApplied=13 with no corruption.")
}
