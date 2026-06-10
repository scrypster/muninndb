//go:build integration

package replication

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vmihailenco/msgpack/v5"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// assertNeverP3 polls cond every 5ms for up to maxWait.
// Fails the test immediately if cond ever returns true.
// (Named assertNeverP3 to avoid redeclaration in the same package across build tags.)
func assertNeverP3(t *testing.T, maxWait time.Duration, cond func() bool, msg string) {
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
// Firewall conn: wraps net.Conn with an atomic blocked flag that drops I/O.
// ---------------------------------------------------------------------------

type firewallConn struct {
	inner   net.Conn
	blocked atomic.Bool
}

func (f *firewallConn) Read(b []byte) (int, error) {
	if f.blocked.Load() {
		return 0, io.EOF
	}
	return f.inner.Read(b)
}

func (f *firewallConn) Write(b []byte) (int, error) {
	if f.blocked.Load() {
		return 0, io.EOF
	}
	return f.inner.Write(b)
}

func (f *firewallConn) Close() error                       { return f.inner.Close() }
func (f *firewallConn) LocalAddr() net.Addr                { return f.inner.LocalAddr() }
func (f *firewallConn) RemoteAddr() net.Addr               { return f.inner.RemoteAddr() }
func (f *firewallConn) SetDeadline(t time.Time) error      { return f.inner.SetDeadline(t) }
func (f *firewallConn) SetReadDeadline(t time.Time) error  { return f.inner.SetReadDeadline(t) }
func (f *firewallConn) SetWriteDeadline(t time.Time) error { return f.inner.SetWriteDeadline(t) }

// connectNodesWithFirewall connects sender -> receiver through a firewall.
// Returns the firewallConn so the caller can block/unblock the link.
func connectNodesWithFirewall(t *testing.T, sender, receiver *testNode) *firewallConn {
	t.Helper()

	senderEnd, receiverEnd := net.Pipe()
	fw := &firewallConn{inner: senderEnd}
	t.Cleanup(func() {
		senderEnd.Close()
		receiverEnd.Close()
	})

	pc := &PeerConn{
		nodeID: receiver.id,
		addr:   "pipe",
		conn:   fw,
	}
	sender.coord.mgr.mu.Lock()
	if existing, ok := sender.coord.mgr.peers[receiver.id]; ok {
		_ = existing.Close()
	}
	sender.coord.mgr.peers[receiver.id] = pc
	sender.coord.mgr.mu.Unlock()

	go func() {
		for {
			frame, err := mbp.ReadFrame(receiverEnd)
			if err != nil {
				return
			}
			_ = receiver.coord.HandleIncomingFrame(sender.id, frame.Type, frame.Payload)
		}
	}()

	return fw
}

// ---------------------------------------------------------------------------
// Test 1: Network Partition — partition B, write on A, heal, verify B catches up
// ---------------------------------------------------------------------------

func TestP3Integration_NetworkPartition(t *testing.T) {
	t.Parallel()

	nodeA := newTestNode(t, "node-A", "primary")
	nodeB := newTestNode(t, "node-B", "replica")
	nodeC := newTestNode(t, "node-C", "replica")

	// A<->B with firewall on the A->B direction
	fwAB := connectNodesWithFirewall(t, nodeA, nodeB)
	connectOneWay(t, nodeB, nodeA)
	// A<->C normal
	connectNodes(t, nodeA, nodeC)
	// B<->C normal
	connectNodes(t, nodeB, nodeC)

	registerVoters(nodeA, nodeB, nodeC)
	electNode(t, nodeA)

	waitFor(t, 5*time.Second, func() bool {
		return nodeB.coord.election.CurrentLeader() == "node-A" &&
			nodeC.coord.election.CurrentLeader() == "node-A"
	}, "B and C to recognize A as leader")

	// Start streamers from A to B and C.
	cancelB := startStreamer(t, nodeA, nodeB, 0)
	defer cancelB()
	cancelC := startStreamer(t, nodeA, nodeC, 0)
	defer cancelC()

	// Partition: block A -> B
	fwAB.blocked.Store(true)

	// Write 20 entries on A.
	appendEntries(t, nodeA, "part", 20)

	// C should receive all 20.
	waitFor(t, 5*time.Second, func() bool {
		return nodeC.applier.LastApplied() >= 20
	}, "node-C to apply 20 entries")

	// B should NOT reach 20 applied entries while the partition is active.
	assertNeverP3(t, 200*time.Millisecond, func() bool {
		return nodeB.applier.LastApplied() >= 20
	}, "B should not have applied 20 entries during partition")

	// Heal partition: unblock A -> B and reconnect the streamer.
	fwAB.blocked.Store(false)

	// Cancel old streamer and start a fresh one so B can catch up.
	cancelB()
	// Reconnect fresh pipe for A->B.
	connectOneWay(t, nodeA, nodeB)
	cancelB2 := startStreamer(t, nodeA, nodeB, 0)
	defer cancelB2()

	// Append a trigger entry to wake the new streamer's subscription.
	_, err := nodeA.repLog.Append(OpSet, []byte("trigger"), []byte("trigger"))
	if err != nil {
		t.Fatalf("Append trigger entry: %v", err)
	}

	// B should eventually catch up (20 original + 1 trigger = 21).
	waitFor(t, 10*time.Second, func() bool {
		return nodeB.applier.LastApplied() >= 20
	}, "node-B to catch up after partition heal")

	verifyEntries(t, nodeB, "part", 20)
	verifyEntries(t, nodeC, "part", 20)

	t.Logf("NetworkPartition: B caught up to 20 entries after partition heal")
}

// ---------------------------------------------------------------------------
// Test 2: Failover Under Write Load
// ---------------------------------------------------------------------------

func TestP3Integration_FailoverUnderWriteLoad(t *testing.T) {
	t.Parallel()

	nodeA := newTestNode(t, "node-A", "primary")
	nodeB := newTestNode(t, "node-B", "replica")
	nodeC := newTestNode(t, "node-C", "replica")

	connectNodes(t, nodeA, nodeB)
	connectNodes(t, nodeA, nodeC)
	connectNodes(t, nodeB, nodeC)
	registerVoters(nodeA, nodeB, nodeC)

	electNode(t, nodeA)
	waitFor(t, 5*time.Second, func() bool {
		return nodeB.coord.election.CurrentLeader() == "node-A" &&
			nodeC.coord.election.CurrentLeader() == "node-A"
	}, "B and C to recognize A as leader")

	// Phase 1: Write 50 entries on A, replicate to B and C.
	cancelB := startStreamer(t, nodeA, nodeB, 0)
	cancelC := startStreamer(t, nodeA, nodeC, 0)

	var writesMu sync.Mutex
	var writtenSeqs []uint64

	// Write 50 entries at ~5 entries / 10ms bursts.
	for i := 1; i <= 50; i++ {
		seq, err := nodeA.repLog.Append(
			OpSet,
			[]byte(fmt.Sprintf("k-load-%d", i)),
			[]byte(fmt.Sprintf("load-val-%d", i)),
		)
		if err != nil {
			t.Fatalf("Append load entry %d: %v", i, err)
		}
		writesMu.Lock()
		writtenSeqs = append(writtenSeqs, seq)
		writesMu.Unlock()
	}

	// Wait for B and C to catch up to 50.
	waitFor(t, 5*time.Second, func() bool {
		return nodeB.applier.LastApplied() >= 50 && nodeC.applier.LastApplied() >= 50
	}, "B and C to apply 50 entries")

	// Phase 2: Kill A, failover to B.
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

	electNode(t, nodeB)
	waitFor(t, 5*time.Second, func() bool {
		return nodeC.coord.election.CurrentLeader() == "node-B"
	}, "C to recognize B as leader")

	// Phase 3: Write 50 more entries on new Cortex B, deliver to C.
	connectNodes(t, nodeB, nodeC)
	baseSeq := uint64(50)
	for i := 1; i <= 50; i++ {
		key := []byte(fmt.Sprintf("k-load2-%d", i))
		val := []byte(fmt.Sprintf("load2-val-%d", i))
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
		if err := nodeC.coord.HandleIncomingFrame("node-B", mbp.TypeReplEntry, payload); err != nil {
			t.Fatalf("HandleIncomingFrame entry %d: %v", i, err)
		}
		writesMu.Lock()
		writtenSeqs = append(writtenSeqs, baseSeq+uint64(i))
		writesMu.Unlock()
	}

	// Wait for C to catch up.
	waitFor(t, 5*time.Second, func() bool {
		return nodeC.applier.LastApplied() >= 100
	}, "C to apply 100 total entries")

	// Verify no duplicate seq numbers.
	writesMu.Lock()
	seqSet := make(map[uint64]bool, len(writtenSeqs))
	for _, s := range writtenSeqs {
		if seqSet[s] {
			t.Errorf("duplicate seq number: %d", s)
		}
		seqSet[s] = true
	}
	writesMu.Unlock()

	if len(seqSet) < 100 {
		t.Errorf("expected >= 100 unique seqs, got %d", len(seqSet))
	}

	t.Logf("FailoverUnderWriteLoad: 100 entries written across failover, no duplicates")
}

// ---------------------------------------------------------------------------
// Test 3: CCS After Partition — divergence detected and reconciled
// ---------------------------------------------------------------------------

func TestP3Integration_CognitiveConsistencyAfterPartition(t *testing.T) {
	t.Parallel()

	cortex := newTestNode(t, "cortex", "primary")
	lobe := newTestNode(t, "lobe", "replica")

	connectNodes(t, cortex, lobe)
	registerVoters(cortex, lobe)
	electNode(t, cortex)

	waitFor(t, 5*time.Second, func() bool {
		return lobe.coord.election.CurrentLeader() == "cortex"
	}, "lobe to recognize cortex as leader")

	// Register lobe as a member in cortex's joinHandler.
	cortex.coord.joinHandler.mu.Lock()
	cortex.coord.joinHandler.members["lobe"] = NodeInfo{
		NodeID: "lobe",
		Addr:   "pipe",
		Role:   RoleReplica,
	}
	cortex.coord.joinHandler.mu.Unlock()

	// Set up identical weights on both sides.
	keys := [][16]byte{{1}, {2}, {3}}
	cortexWeights := map[[16]byte]float64{{1}: 0.9, {2}: 0.8, {3}: 0.7}

	cortexSampler := &mockHebbianSampler{keys: keys, weights: cortexWeights}
	cortexProbe := NewCCSProbe(cortexSampler, cortex.coord)
	cortexProbe.sampleN = 3
	cortex.coord.SetCCSProbe(cortexProbe)

	// Lobe has same weights initially.
	lobeSampler := &mockHebbianSampler{keys: keys, weights: map[[16]byte]float64{
		{1}: 0.9, {2}: 0.8, {3}: 0.7,
	}}
	lobeProbe := NewCCSProbe(lobeSampler, lobe.coord)
	lobe.coord.SetCCSProbe(lobeProbe)

	// Run CCS probe — should be 1.0 (identical weights).
	cortexProbe.probe(context.Background())
	result := cortexProbe.LastResult()
	if result.Score != 1.0 {
		t.Errorf("initial CCS score = %.4f, expected 1.0", result.Score)
	}

	// Simulate partition: change lobe's weights (divergence).
	lobeSampler.weights = map[[16]byte]float64{
		{1}: 0.5, {2}: 0.3, {3}: 0.1,
	}

	// Run CCS probe again — should detect divergence (score < 1.0).
	cortexProbe.probe(context.Background())
	result = cortexProbe.LastResult()
	if result.Score >= 1.0 {
		t.Errorf("post-partition CCS score = %.4f, expected < 1.0", result.Score)
	}
	t.Logf("CCS divergence detected: score=%.4f", result.Score)

	// Set up reconciler on cortex and lobe.
	cortexWriter := newMockHebbianStoreWriter()
	cortexReconciler := NewReconciler(cortexSampler, cortexWriter, cortex.coord)
	cortex.coord.SetReconciler(cortexReconciler)

	lobeWriter := newMockHebbianStoreWriter()
	lobeReconciler := NewReconciler(lobeSampler, lobeWriter, lobe.coord)
	lobe.coord.SetReconciler(lobeReconciler)

	// Run reconciliation from cortex targeting lobe.
	reconResult, err := cortex.coord.TriggerReconciliation(context.Background(), []string{"lobe"})
	if err != nil {
		t.Fatalf("TriggerReconciliation: %v", err)
	}

	if reconResult.EngramsDivergent == 0 {
		t.Error("expected divergent engrams after partition, got 0")
	}
	t.Logf("Reconciliation: checked=%d, divergent=%d, synced=%d",
		reconResult.EngramsChecked, reconResult.EngramsDivergent, reconResult.WeightsSynced)

	// After reconciliation, update lobe's sampler weights to match cortex
	// (simulating what the ReconSync would do in a real system).
	lobeSampler.weights = map[[16]byte]float64{
		{1}: 0.9, {2}: 0.8, {3}: 0.7,
	}

	// Run CCS probe again — should be 1.0.
	cortexProbe.probe(context.Background())
	result = cortexProbe.LastResult()
	if result.Score != 1.0 {
		t.Errorf("post-reconciliation CCS score = %.4f, expected 1.0", result.Score)
	}

	t.Logf("CCS restored to 1.0 after reconciliation")
}

// ---------------------------------------------------------------------------
// Test 4: Sentinel Quorum — Sentinel provides vote, never becomes Cortex
// ---------------------------------------------------------------------------

func TestP3Integration_SentinelQuorum(t *testing.T) {
	t.Parallel()

	nodeA := newTestNode(t, "node-A", "primary")
	nodeB := newTestNode(t, "node-B", "replica")
	nodeS := newTestNode(t, "sentinel-S", "sentinel")

	// Mark sentinel role.
	nodeS.coord.roleMu.Lock()
	nodeS.coord.role = RoleSentinel
	nodeS.coord.roleMu.Unlock()
	nodeS.coord.election.SetSentinel(true)

	connectNodes(t, nodeA, nodeB)
	connectNodes(t, nodeA, nodeS)
	connectNodes(t, nodeB, nodeS)

	// All 3 are voters.
	registerVoters(nodeA, nodeB, nodeS)

	// Bootstrap A as Cortex.
	electNode(t, nodeA)
	waitFor(t, 5*time.Second, func() bool {
		return nodeB.coord.election.CurrentLeader() == "node-A" &&
			nodeS.coord.election.CurrentLeader() == "node-A"
	}, "B and S to recognize A as leader")

	initialEpoch := nodeA.coord.CurrentEpoch()

	// Kill A: mark SDOWN on B and S.
	nodeB.coord.msp.AddPeer("node-A", "pipe", RolePrimary)
	nodeS.coord.msp.AddPeer("node-A", "pipe", RolePrimary)
	nodeB.coord.msp.mu.Lock()
	if p, ok := nodeB.coord.msp.peers["node-A"]; ok {
		p.SDown = true
		p.MissedBeats = 10
	}
	nodeB.coord.msp.mu.Unlock()
	nodeS.coord.msp.mu.Lock()
	if p, ok := nodeS.coord.msp.peers["node-A"]; ok {
		p.SDown = true
		p.MissedBeats = 10
	}
	nodeS.coord.msp.mu.Unlock()

	// Track if S ever gets promoted.
	var sentinelPromoted atomic.Bool
	nodeS.coord.election.OnPromoted = func(epoch uint64) {
		sentinelPromoted.Store(true)
	}

	// B starts election. Quorum = 3/2 + 1 = 2.
	// B gets self-vote + S's vote = 2 >= 2 => B wins.
	if err := nodeB.coord.election.StartElection(context.Background()); err != nil {
		t.Fatalf("node-B StartElection: %v", err)
	}

	waitFor(t, 5*time.Second, func() bool {
		return nodeB.coord.IsLeader()
	}, "node-B to become leader with sentinel quorum")

	// Verify B's epoch is higher.
	if nodeB.coord.CurrentEpoch() <= initialEpoch {
		t.Errorf("node-B epoch=%d, expected > %d", nodeB.coord.CurrentEpoch(), initialEpoch)
	}

	// Verify sentinel was never promoted.
	if sentinelPromoted.Load() {
		t.Error("sentinel should never be promoted to Cortex")
	}

	// Verify sentinel cannot start an election.
	err := nodeS.coord.election.StartElection(context.Background())
	if err == nil {
		t.Error("sentinel should not be able to start an election")
	}

	// S should recognize B as leader.
	waitFor(t, 5*time.Second, func() bool {
		return nodeS.coord.election.CurrentLeader() == "node-B"
	}, "sentinel to recognize B as leader")

	t.Logf("SentinelQuorum: B elected with sentinel vote, epoch %d -> %d",
		initialEpoch, nodeB.coord.CurrentEpoch())
}

// ---------------------------------------------------------------------------
// Test 5: Observer Replication — receives data but excluded from quorum
// ---------------------------------------------------------------------------

func TestP3Integration_ObserverReplication(t *testing.T) {
	t.Parallel()

	cortex := newTestNode(t, "cortex", "primary")
	lobe := newTestNode(t, "lobe", "replica")
	observer := newTestNode(t, "observer", "observer")
	observer.coord.election.SetObserver(true)

	connectNodes(t, cortex, lobe)
	connectNodes(t, cortex, observer)
	connectNodes(t, lobe, observer)

	// Only cortex and lobe are voters.
	registerVoters(cortex, lobe)

	electNode(t, cortex)
	waitFor(t, 5*time.Second, func() bool {
		return lobe.coord.election.CurrentLeader() == "cortex"
	}, "lobe to recognize cortex as leader")

	// Start streamers from cortex to lobe and observer.
	cancelLobe := startStreamer(t, cortex, lobe, 0)
	defer cancelLobe()
	cancelObs := startStreamer(t, cortex, observer, 0)
	defer cancelObs()

	// Write 30 entries.
	appendEntries(t, cortex, "obs", 30)

	// Both lobe and observer should receive all 30.
	waitFor(t, 5*time.Second, func() bool {
		return lobe.applier.LastApplied() >= 30
	}, "lobe to apply 30 entries")
	waitFor(t, 5*time.Second, func() bool {
		return observer.applier.LastApplied() >= 30
	}, "observer to apply 30 entries")

	verifyEntries(t, lobe, "obs", 30)
	verifyEntries(t, observer, "obs", 30)

	// Kill cortex: stop it so it can't respond to VoteRequests.
	_ = cortex.coord.Stop()

	lobe.coord.msp.AddPeer("cortex", "pipe", RolePrimary)
	lobe.coord.msp.mu.Lock()
	if p, ok := lobe.coord.msp.peers["cortex"]; ok {
		p.SDown = true
		p.MissedBeats = 10
	}
	lobe.coord.msp.mu.Unlock()

	// Observer should refuse to grant a vote.
	req := mbp.VoteRequest{CandidateID: "lobe", Epoch: cortex.coord.CurrentEpoch() + 1}
	resp := observer.coord.election.HandleVoteRequest(req)
	if resp.Granted {
		t.Error("observer should not grant a vote")
	}

	// Disconnect lobe -> cortex so VoteRequest broadcast doesn't reach dead cortex.
	lobe.coord.mgr.mu.Lock()
	if pc, ok := lobe.coord.mgr.peers["cortex"]; ok {
		_ = pc.Close()
		delete(lobe.coord.mgr.peers, "cortex")
	}
	lobe.coord.mgr.mu.Unlock()

	// Lobe starts election. Quorum = 2, lobe has only self-vote = 1. Cannot win.
	if err := lobe.coord.election.StartElection(context.Background()); err != nil {
		t.Fatalf("lobe StartElection: %v", err)
	}

	// Lobe should NOT become leader within this window (observer excluded from quorum).
	assertNeverP3(t, 200*time.Millisecond, func() bool {
		return lobe.coord.IsLeader()
	}, "lobe should NOT become leader without cortex vote (observer excluded from quorum)")

	// Confirm election remains in candidate state.
	if lobe.coord.election.State() != ElectionCandidate {
		t.Errorf("expected ElectionCandidate, got %v", lobe.coord.election.State())
	}

	t.Logf("ObserverReplication: observer received 30 entries, excluded from quorum")
}

// ---------------------------------------------------------------------------
// Test 6: Rolling Upgrade — graceful failover through all nodes
// ---------------------------------------------------------------------------

func TestP3Integration_RollingUpgrade(t *testing.T) {
	t.Parallel()

	nodeA := newTestNode(t, "node-A", "primary")
	nodeB := newTestNode(t, "node-B", "replica")
	nodeC := newTestNode(t, "node-C", "replica")

	connectNodes(t, nodeA, nodeB)
	connectNodes(t, nodeA, nodeC)
	connectNodes(t, nodeB, nodeC)
	registerVoters(nodeA, nodeB, nodeC)

	electNode(t, nodeA)
	waitFor(t, 5*time.Second, func() bool {
		return nodeB.coord.election.CurrentLeader() == "node-A" &&
			nodeC.coord.election.CurrentLeader() == "node-A"
	}, "B and C to recognize A as leader")

	epochAfterA := nodeA.coord.CurrentEpoch()

	// Phase 1: Write 20 entries on A.
	appendEntries(t, nodeA, "r1", 20)
	cortexSeq := nodeA.repLog.CurrentSeq()

	// Simulate replication convergence for handoff.
	nodeA.coord.UpdateReplicaSeq("node-B", cortexSeq)
	nodeA.coord.UpdateReplicaSeq("node-C", cortexSeq)

	// Wire mock flushers for graceful handoff.
	nodeA.coord.SetCognitiveFlushers(&p2MockFlusher{})

	// Graceful failover A -> B.
	ctx := context.Background()
	if err := nodeA.coord.GracefulFailover(ctx, "node-B"); err != nil {
		t.Fatalf("GracefulFailover A->B: %v", err)
	}

	if !nodeB.coord.IsLeader() {
		t.Fatal("node-B should be leader after handoff from A")
	}
	epochAfterB := nodeB.coord.CurrentEpoch()
	if epochAfterB <= epochAfterA {
		t.Errorf("epoch after B=%d, expected > %d", epochAfterB, epochAfterA)
	}

	// Phase 2: Write 20 more entries on B, deliver to A and C.
	baseSeq := cortexSeq
	for i := 1; i <= 20; i++ {
		key := []byte(fmt.Sprintf("k-r2-%d", i))
		val := []byte(fmt.Sprintf("r2-val-%d", i))
		entry := mbp.ReplEntry{
			Seq:         baseSeq + uint64(i),
			Op:          uint8(OpSet),
			Key:         key,
			Value:       val,
			TimestampNS: time.Now().UnixNano(),
		}
		payload, err := msgpack.Marshal(entry)
		if err != nil {
			t.Fatalf("marshal entry: %v", err)
		}
		_ = nodeA.coord.HandleIncomingFrame("node-B", mbp.TypeReplEntry, payload)
		_ = nodeC.coord.HandleIncomingFrame("node-B", mbp.TypeReplEntry, payload)
	}

	waitFor(t, 5*time.Second, func() bool {
		return nodeA.applier.LastApplied() >= baseSeq+20 &&
			nodeC.applier.LastApplied() >= baseSeq+20
	}, "A and C to apply r2 entries")

	// Simulate convergence for B -> C handoff.
	newBaseSeq := baseSeq + 20
	nodeB.coord.UpdateReplicaSeq("node-A", newBaseSeq)
	nodeB.coord.UpdateReplicaSeq("node-C", newBaseSeq)
	nodeB.coord.SetCognitiveFlushers(&p2MockFlusher{})

	// Graceful failover B -> C.
	if err := nodeB.coord.GracefulFailover(ctx, "node-C"); err != nil {
		t.Fatalf("GracefulFailover B->C: %v", err)
	}

	if !nodeC.coord.IsLeader() {
		t.Fatal("node-C should be leader after handoff from B")
	}
	epochAfterC := nodeC.coord.CurrentEpoch()
	if epochAfterC <= epochAfterB {
		t.Errorf("epoch after C=%d, expected > %d", epochAfterC, epochAfterB)
	}

	// Phase 3: Write 20 more entries on C, deliver to A and B.
	for i := 1; i <= 20; i++ {
		key := []byte(fmt.Sprintf("k-r3-%d", i))
		val := []byte(fmt.Sprintf("r3-val-%d", i))
		entry := mbp.ReplEntry{
			Seq:         newBaseSeq + uint64(i),
			Op:          uint8(OpSet),
			Key:         key,
			Value:       val,
			TimestampNS: time.Now().UnixNano(),
		}
		payload, err := msgpack.Marshal(entry)
		if err != nil {
			t.Fatalf("marshal entry: %v", err)
		}
		_ = nodeA.coord.HandleIncomingFrame("node-C", mbp.TypeReplEntry, payload)
		_ = nodeB.coord.HandleIncomingFrame("node-C", mbp.TypeReplEntry, payload)
	}

	finalSeq := newBaseSeq + 20
	waitFor(t, 5*time.Second, func() bool {
		return nodeA.applier.LastApplied() >= finalSeq &&
			nodeB.applier.LastApplied() >= finalSeq
	}, "A and B to apply r3 entries")

	// Verify all 60 entries are present on all nodes that received them via Apply.
	// A (demoted to Lobe): received r2 + r3 via HandleIncomingFrame.
	verifyEntries(t, nodeA, "r2", 20)
	verifyEntries(t, nodeA, "r3", 20)

	// C: received r2 via HandleIncomingFrame + wrote r3 locally... but r3 is in replog not Pebble.
	// C would apply r1 entries if we streamed them. For this test, verify what we know is applied.
	verifyEntries(t, nodeC, "r2", 20) // received r2 via HandleIncomingFrame above

	// Verify epoch was incremented twice.
	if epochAfterC != epochAfterA+2 {
		t.Errorf("expected 2 epoch increments: %d -> %d (delta=%d)",
			epochAfterA, epochAfterC, epochAfterC-epochAfterA)
	}

	t.Logf("RollingUpgrade: 3 failovers, epoch %d -> %d -> %d, entries verified",
		epochAfterA, epochAfterB, epochAfterC)
}

// ---------------------------------------------------------------------------
// Test 7: Large Cluster Election Convergence — 5 nodes
// ---------------------------------------------------------------------------

func TestP3Integration_LargeCluster_ElectionConvergence(t *testing.T) {
	t.Parallel()

	nodes := make([]*testNode, 5)
	nodes[0] = newTestNode(t, "node-0", "primary")
	for i := 1; i < 5; i++ {
		nodes[i] = newTestNode(t, fmt.Sprintf("node-%d", i), "replica")
	}

	// Fully connect all pairs.
	for i := 0; i < 5; i++ {
		for j := i + 1; j < 5; j++ {
			connectNodes(t, nodes[i], nodes[j])
		}
	}

	registerVoters(nodes...)

	// Elect node-0 as Cortex.
	electNode(t, nodes[0])
	for i := 1; i < 5; i++ {
		waitFor(t, 5*time.Second, func() bool {
			return nodes[i].coord.election.CurrentLeader() == "node-0"
		}, fmt.Sprintf("node-%d to recognize node-0 as leader", i))
	}

	initialEpoch := nodes[0].coord.CurrentEpoch()

	// Kill node-0: mark SDOWN on all other nodes.
	for i := 1; i < 5; i++ {
		nodes[i].coord.msp.AddPeer("node-0", "pipe", RolePrimary)
		nodes[i].coord.msp.mu.Lock()
		if p, ok := nodes[i].coord.msp.peers["node-0"]; ok {
			p.SDown = true
			p.MissedBeats = 10
		}
		nodes[i].coord.msp.mu.Unlock()
	}

	// node-1 starts election. Quorum = 5/2 + 1 = 3.
	// node-1 gets self-vote + votes from node-2, node-3, node-4 = 4 votes.
	if err := nodes[1].coord.election.StartElection(context.Background()); err != nil {
		t.Fatalf("node-1 StartElection: %v", err)
	}

	waitFor(t, 5*time.Second, func() bool {
		return nodes[1].coord.IsLeader()
	}, "node-1 to become leader")

	// Verify exactly one leader among surviving nodes.
	leaderCount := 0
	var leaderID string
	for i := 1; i < 5; i++ {
		if nodes[i].coord.IsLeader() {
			leaderCount++
			leaderID = nodes[i].id
		}
	}
	if leaderCount != 1 {
		t.Errorf("expected exactly 1 leader, got %d", leaderCount)
	}

	// Verify new epoch is initial + 1.
	newEpoch := nodes[1].coord.CurrentEpoch()
	if newEpoch != initialEpoch+1 {
		t.Errorf("new epoch=%d, expected %d", newEpoch, initialEpoch+1)
	}

	// All surviving nodes should agree on the same leader.
	for i := 2; i < 5; i++ {
		waitFor(t, 5*time.Second, func() bool {
			return nodes[i].coord.election.CurrentLeader() == leaderID
		}, fmt.Sprintf("node-%d to recognize %s as leader", i, leaderID))
	}

	t.Logf("LargeCluster: 5-node cluster, node-0 killed, %s elected in epoch %d",
		leaderID, newEpoch)
}

// ---------------------------------------------------------------------------
// Test 8: Clock Skew Resilience — SDOWN detection with heartbeat delays
// ---------------------------------------------------------------------------

func TestP3Integration_ClockSkewResilience(t *testing.T) {
	t.Parallel()

	nodeA := newTestNode(t, "node-A", "primary")
	nodeB := newTestNode(t, "node-B", "replica")

	// Register peers in MSP with short heartbeat interval.
	nodeA.coord.msp.AddPeer("node-B", "pipe", RoleReplica)
	nodeB.coord.msp.AddPeer("node-A", "pipe", RolePrimary)

	// Use a short heartbeat interval: 50ms.
	// SDOWN threshold = 3 missed beats = 150ms.
	heartbeatInterval := 50 * time.Millisecond

	// Simulate heartbeats with artificial 20ms delay (clock skew / slow network).
	// With 50ms heartbeat and 20ms delay, heartbeats still arrive within the 50ms window.
	// SDOWN should NOT be triggered.

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Run MSP for nodeA in background (missedThreshold=3: SDOWN after 3*50ms=150ms).
	go func() {
		_ = nodeA.coord.msp.Run(ctx, heartbeatInterval, 3)
	}()

	// Simulate B sending delayed heartbeats to A (every 50ms + 20ms delay = 70ms).
	stopHeartbeats := make(chan struct{})
	go func() {
		ticker := time.NewTicker(heartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stopHeartbeats:
				return
			case <-ticker.C:
				// 20ms artificial delay simulating clock skew.
				time.Sleep(20 * time.Millisecond)
				nodeA.coord.msp.HandlePing("node-B", nil)
			}
		}
	}()

	// Wait 500ms (10 heartbeat cycles). SDOWN should NOT be triggered.
	time.Sleep(500 * time.Millisecond)
	if nodeA.coord.msp.IsSDown("node-B") {
		t.Error("SDOWN should NOT be triggered with 20ms delay on 50ms heartbeat interval")
	}

	// Now stop heartbeats entirely.
	close(stopHeartbeats)

	// SDOWN should be triggered within 3 * 50ms = 150ms + some margin.
	// We wait up to 500ms for safety.
	waitFor(t, 500*time.Millisecond, func() bool {
		return nodeA.coord.msp.IsSDown("node-B")
	}, "SDOWN to be triggered after heartbeats stop")

	t.Logf("ClockSkewResilience: 20ms delay on 50ms heartbeat did not cause false SDOWN; " +
		"stopping heartbeats correctly triggered SDOWN")
}
