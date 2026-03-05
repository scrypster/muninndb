package replication

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/vfs"
	"github.com/vmihailenco/msgpack/v5"

	"github.com/scrypster/muninndb/internal/cognitive"
	"github.com/scrypster/muninndb/internal/config"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// mockHebbianSubmitter records submitted CoActivationEvents for test assertions.
type mockHebbianSubmitter struct {
	mu     sync.Mutex
	events []cognitive.CoActivationEvent
	full   bool // when true, Submit always drops (simulates full channel)
}

func (m *mockHebbianSubmitter) Submit(item cognitive.CoActivationEvent) bool {
	if m.full {
		return false
	}
	m.mu.Lock()
	m.events = append(m.events, item)
	m.mu.Unlock()
	return true
}

func (m *mockHebbianSubmitter) Received() []cognitive.CoActivationEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]cognitive.CoActivationEvent, len(m.events))
	copy(out, m.events)
	return out
}

// newTestCoordinator creates a ClusterCoordinator backed by an in-memory Pebble DB.
func newTestCoordinator(t *testing.T, role string) (*ClusterCoordinator, *pebble.DB) {
	t.Helper()
	db, err := pebble.Open("", &pebble.Options{FS: vfs.NewMem()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	repLog := NewReplicationLog(db)
	applier := NewApplier(db)
	epochStore, err := NewEpochStore(db)
	if err != nil {
		t.Fatal(err)
	}

	cfg := &config.ClusterConfig{
		Enabled:     true,
		NodeID:      "node-test",
		BindAddr:    "127.0.0.1:9001",
		Seeds:       []string{},
		Role:        role,
		LeaseTTL:    10,
		HeartbeatMS: 1000,
	}

	coord := NewClusterCoordinator(cfg, repLog, applier, epochStore)
	return coord, db
}

// simulatePromotion sets election.state = ElectionLeader and then calls
// handlePromotion. This is necessary in tests because the race-condition fix in
// handlePromotion now atomically checks election.state before setting role —
// the same guard that serializes against a concurrent HandleCortexClaim in
// production. Test code that bypasses the election path must use this helper
// instead of calling handlePromotion directly.
func simulatePromotion(c *ClusterCoordinator, epoch uint64) {
	c.election.mu.Lock()
	c.election.state = ElectionLeader
	c.election.currentLeader = c.cfg.NodeID
	c.election.mu.Unlock()
	c.handlePromotion(epoch)
}

func TestClusterCoordinator_NewCoordinator(t *testing.T) {
	coord, _ := newTestCoordinator(t, "auto")

	if coord.Role() != RoleUnknown {
		t.Errorf("expected initial role RoleUnknown, got %v", coord.Role())
	}

	if coord.CurrentEpoch() != 0 {
		t.Errorf("expected initial epoch 0, got %d", coord.CurrentEpoch())
	}

	if coord.IsLeader() {
		t.Error("expected IsLeader false initially")
	}

	if coord.mgr == nil {
		t.Error("expected ConnManager to be initialized")
	}
	if coord.msp == nil {
		t.Error("expected MSP to be initialized")
	}
	if coord.election == nil {
		t.Error("expected Election to be initialized")
	}
	if coord.joinHandler == nil {
		t.Error("expected JoinHandler to be initialized")
	}
	if coord.joinClient == nil {
		t.Error("expected JoinClient to be initialized")
	}
}

func TestClusterCoordinator_Role_ThreadSafe(t *testing.T) {
	coord, _ := newTestCoordinator(t, "auto")

	// Force epoch to 1 so promotion can proceed
	coord.epochStore.ForceSet(1)

	var wg sync.WaitGroup
	const readers = 50

	// Start concurrent readers
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = coord.Role()
			}
		}()
	}

	// Trigger promotion/demotion concurrently
	wg.Add(2)
	go func() {
		defer wg.Done()
		for j := 0; j < 50; j++ {
			simulatePromotion(coord,1)
		}
	}()
	go func() {
		defer wg.Done()
		for j := 0; j < 50; j++ {
			coord.handleDemotion()
		}
	}()

	wg.Wait()
}

func TestClusterCoordinator_IsLeader(t *testing.T) {
	coord, _ := newTestCoordinator(t, "auto")

	if coord.IsLeader() {
		t.Error("expected IsLeader=false initially")
	}

	// Simulate promotion
	simulatePromotion(coord,1)
	if !coord.IsLeader() {
		t.Error("expected IsLeader=true after promotion")
	}

	// Simulate demotion
	coord.handleDemotion()
	if coord.IsLeader() {
		t.Error("expected IsLeader=false after demotion")
	}
}

func TestClusterCoordinator_CurrentEpoch(t *testing.T) {
	coord, _ := newTestCoordinator(t, "auto")

	if epoch := coord.CurrentEpoch(); epoch != 0 {
		t.Errorf("expected epoch 0, got %d", epoch)
	}

	coord.epochStore.ForceSet(5)

	if epoch := coord.CurrentEpoch(); epoch != 5 {
		t.Errorf("expected epoch 5, got %d", epoch)
	}
}

func TestClusterCoordinator_HandleIncomingFrame_Ping(t *testing.T) {
	coord, _ := newTestCoordinator(t, "auto")

	// Register a peer in MSP so HandlePing has someone to update
	peerID := "peer-1"
	coord.msp.AddPeer(peerID, "127.0.0.1:9002", RoleReplica)

	// Mark as SDOWN so we can verify Ping clears it
	coord.msp.mu.Lock()
	if p, ok := coord.msp.peers[peerID]; ok {
		p.SDown = true
		p.MissedBeats = 5
	}
	coord.msp.mu.Unlock()

	if !coord.msp.IsSDown(peerID) {
		t.Fatal("expected peer to be SDOWN before ping")
	}

	// Send Ping frame
	err := coord.HandleIncomingFrame(peerID, mbp.TypePing, nil)
	if err != nil {
		t.Fatalf("HandleIncomingFrame Ping: %v", err)
	}

	if coord.msp.IsSDown(peerID) {
		t.Error("expected peer to recover from SDOWN after ping")
	}
}

func TestClusterCoordinator_HandleIncomingFrame_VoteRequest(t *testing.T) {
	coord, _ := newTestCoordinator(t, "auto")

	// We need a peer connection for sending the response back.
	// Add the peer to ConnManager.
	voterID := "candidate-1"
	coord.mgr.AddPeer(voterID, "127.0.0.1:9003")

	// Create a VoteRequest
	req := mbp.VoteRequest{
		CandidateID: voterID,
		Epoch:       1,
	}
	payload, err := msgpack.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}

	// HandleIncomingFrame should process the vote request without error.
	// The response send will fail (peer not actually connected) but that's OK.
	err = coord.HandleIncomingFrame(voterID, mbp.TypeVoteRequest, payload)
	if err != nil {
		t.Fatalf("HandleIncomingFrame VoteRequest: %v", err)
	}

	// Verify the vote was recorded: election should have votedFor[1] == voterID
	coord.election.mu.Lock()
	voted, ok := coord.election.votedFor[1]
	coord.election.mu.Unlock()

	if !ok {
		t.Error("expected vote to be recorded for epoch 1")
	}
	if voted != voterID {
		t.Errorf("expected votedFor[1]=%q, got %q", voterID, voted)
	}
}

func TestClusterCoordinator_HandleIncomingFrame_VoteResponse(t *testing.T) {
	coord, _ := newTestCoordinator(t, "auto")

	// Register voter-1 so the election machinery counts its vote.
	coord.election.RegisterVoter("voter-1")

	// Manually put election in candidate state for epoch 1
	coord.election.mu.Lock()
	coord.election.state = ElectionCandidate
	coord.election.candidateEpoch = 1
	coord.election.votes[1] = map[string]bool{coord.cfg.NodeID: true}
	coord.election.mu.Unlock()

	resp := mbp.VoteResponse{
		VoterID: "voter-1",
		Epoch:   1,
		Granted: true,
	}
	payload, err := msgpack.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}

	err = coord.HandleIncomingFrame("voter-1", mbp.TypeVoteResponse, payload)
	if err != nil {
		t.Fatalf("HandleIncomingFrame VoteResponse: %v", err)
	}

	// Verify the vote was recorded
	coord.election.mu.Lock()
	count := len(coord.election.votes[1])
	coord.election.mu.Unlock()

	if count != 2 {
		t.Errorf("expected 2 votes for epoch 1, got %d", count)
	}
}

func TestClusterCoordinator_HandleIncomingFrame_CortexClaim(t *testing.T) {
	coord, _ := newTestCoordinator(t, "auto")

	claim := mbp.CortexClaim{
		CortexID:     "leader-1",
		Epoch:        3,
		FencingToken: 3,
	}
	payload, err := msgpack.Marshal(claim)
	if err != nil {
		t.Fatal(err)
	}

	err = coord.HandleIncomingFrame("leader-1", mbp.TypeCortexClaim, payload)
	if err != nil {
		t.Fatalf("HandleIncomingFrame CortexClaim: %v", err)
	}

	// Epoch should have been updated
	if epoch := coord.CurrentEpoch(); epoch != 3 {
		t.Errorf("expected epoch 3 after CortexClaim, got %d", epoch)
	}

	// Election state should be follower
	if state := coord.election.State(); state != ElectionFollower {
		t.Errorf("expected ElectionFollower, got %v", state)
	}
}

func TestClusterCoordinator_HandleIncomingFrame_ReplEntry(t *testing.T) {
	coord, _ := newTestCoordinator(t, "replica")

	entry := mbp.ReplEntry{
		Seq:         1,
		Op:          uint8(OpSet),
		Key:         []byte("key1"),
		Value:       []byte("val1"),
		TimestampNS: 12345,
	}
	payload, err := msgpack.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}

	err = coord.HandleIncomingFrame("cortex-1", mbp.TypeReplEntry, payload)
	if err != nil {
		t.Fatalf("HandleIncomingFrame ReplEntry: %v", err)
	}

	if last := coord.applier.LastApplied(); last != 1 {
		t.Errorf("expected lastApplied=1, got %d", last)
	}
}

func TestClusterCoordinator_HandleIncomingFrame_Leave(t *testing.T) {
	coord, _ := newTestCoordinator(t, "primary")

	// Register a member first
	coord.joinHandler.mu.Lock()
	coord.joinHandler.members["lobe-1"] = NodeInfo{NodeID: "lobe-1", Addr: "127.0.0.1:9004"}
	coord.joinHandler.mu.Unlock()
	coord.mgr.AddPeer("lobe-1", "127.0.0.1:9004")

	msg := mbp.LeaveMessage{
		NodeID: "lobe-1",
		Epoch:  1,
	}
	payload, err := msgpack.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}

	err = coord.HandleIncomingFrame("lobe-1", mbp.TypeLeave, payload)
	if err != nil {
		t.Fatalf("HandleIncomingFrame Leave: %v", err)
	}

	// Member should be removed
	members := coord.joinHandler.Members()
	if len(members) != 0 {
		t.Errorf("expected 0 members after leave, got %d", len(members))
	}
}

func TestClusterCoordinator_HandleIncomingFrame_UnknownType(t *testing.T) {
	coord, _ := newTestCoordinator(t, "auto")

	err := coord.HandleIncomingFrame("peer-1", 0xFE, nil)
	if err == nil {
		t.Error("expected error for unknown frame type")
	}
}

func TestClusterCoordinator_KnownNodes(t *testing.T) {
	coord, _ := newTestCoordinator(t, "auto")

	// Add peers to MSP
	coord.msp.AddPeer("peer-1", "127.0.0.1:9002", RoleReplica)
	coord.msp.AddPeer("peer-2", "127.0.0.1:9003", RoleSentinel)

	nodes := coord.KnownNodes()

	// Should include self + 2 peers
	if len(nodes) != 3 {
		t.Fatalf("expected 3 nodes, got %d", len(nodes))
	}

	// First node should be self
	if nodes[0].NodeID != "node-test" {
		t.Errorf("expected first node to be self, got %q", nodes[0].NodeID)
	}
}

func TestClusterCoordinator_ReplicationLag(t *testing.T) {
	coord, _ := newTestCoordinator(t, "auto")

	// As Cortex (primary), lag should be 0
	simulatePromotion(coord,1)
	if lag := coord.ReplicationLag(); lag != 0 {
		t.Errorf("expected lag=0 on Cortex, got %d", lag)
	}

	// As Lobe (replica), lag = currentSeq - lastApplied
	coord.handleDemotion()

	// Append some entries to the log
	coord.repLog.Append(OpSet, []byte("k1"), []byte("v1"))
	coord.repLog.Append(OpSet, []byte("k2"), []byte("v2"))
	coord.repLog.Append(OpSet, []byte("k3"), []byte("v3"))

	// Applier hasn't applied anything yet, so lag = 3
	lag := coord.ReplicationLag()
	if lag != 3 {
		t.Errorf("expected lag=3, got %d", lag)
	}

	// Apply one entry, lag should drop to 2
	coord.applier.Apply(ReplicationEntry{Seq: 1, Op: OpSet, Key: []byte("k1"), Value: []byte("v1")})
	lag = coord.ReplicationLag()
	if lag != 2 {
		t.Errorf("expected lag=2, got %d", lag)
	}
}

func TestClusterCoordinator_Stop(t *testing.T) {
	coord, _ := newTestCoordinator(t, "auto")

	err := coord.Stop()
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Calling Stop again should be safe
	err = coord.Stop()
	if err != nil {
		t.Fatalf("Stop (second call): %v", err)
	}
}

func TestClusterCoordinator_OnBecameCortex_Callback(t *testing.T) {
	coord, _ := newTestCoordinator(t, "auto")

	var called bool
	var callbackEpoch uint64
	coord.OnBecameCortex = func(epoch uint64) {
		called = true
		callbackEpoch = epoch
	}

	simulatePromotion(coord,7)

	if !called {
		t.Error("expected OnBecameCortex to be called")
	}
	if callbackEpoch != 7 {
		t.Errorf("expected callback epoch=7, got %d", callbackEpoch)
	}
}

func TestClusterCoordinator_OnBecameLobe_Callback(t *testing.T) {
	coord, _ := newTestCoordinator(t, "auto")

	var called bool
	coord.OnBecameLobe = func() {
		called = true
	}

	simulatePromotion(coord,1)
	coord.handleDemotion()

	if !called {
		t.Error("expected OnBecameLobe to be called")
	}
	if coord.Role() != RoleReplica {
		t.Errorf("expected role=RoleReplica after demotion, got %v", coord.Role())
	}
}

func TestClusterCoordinator_NilCallbacksSafe(t *testing.T) {
	coord, _ := newTestCoordinator(t, "auto")

	// Ensure nil callbacks don't panic
	coord.OnBecameCortex = nil
	coord.OnBecameLobe = nil

	simulatePromotion(coord,1)
	coord.handleDemotion()
}

func TestClusterCoordinator_Accessors(t *testing.T) {
	coord, _ := newTestCoordinator(t, "auto")

	if coord.ConnManager() == nil {
		t.Error("expected ConnManager() to return non-nil")
	}
	if coord.MSP() == nil {
		t.Error("expected MSP() to return non-nil")
	}
	if coord.Election() == nil {
		t.Error("expected Election() to return non-nil")
	}
}

func TestClusterCoordinator_CortexID(t *testing.T) {
	coord, _ := newTestCoordinator(t, "auto")

	// Initially no leader
	if id := coord.CortexID(); id != "" {
		t.Errorf("expected empty CortexID initially, got %q", id)
	}

	// Simulate a CortexClaim from another node
	claim := mbp.CortexClaim{
		CortexID:     "leader-x",
		Epoch:        2,
		FencingToken: 2,
	}
	coord.election.HandleCortexClaim(claim)

	if id := coord.CortexID(); id != "leader-x" {
		t.Errorf("expected CortexID=%q, got %q", "leader-x", id)
	}
}

func TestClusterCoordinator_FencingToken(t *testing.T) {
	coord, _ := newTestCoordinator(t, "auto")

	if token := coord.FencingToken(); token != 0 {
		t.Errorf("expected fencing token 0, got %d", token)
	}

	coord.epochStore.ForceSet(42)

	if token := coord.FencingToken(); token != 42 {
		t.Errorf("expected fencing token 42, got %d", token)
	}
}

func TestClusterCoordinator_ClusterMembers(t *testing.T) {
	coord, _ := newTestCoordinator(t, "auto")

	coord.msp.AddPeer("peer-a", "127.0.0.1:9010", RoleReplica)

	members := coord.ClusterMembers()
	if len(members) != 2 {
		t.Fatalf("expected 2 members (self + peer), got %d", len(members))
	}
}

func TestClusterCoordinator_QuorumLoss_PreemptiveDemotion(t *testing.T) {
	coord, _ := newTestCoordinator(t, "auto")

	// Promote to Cortex
	simulatePromotion(coord,1)
	if !coord.IsLeader() {
		t.Fatal("expected to be leader after promotion")
	}

	// Register 2 voters (self + peer-1) so quorum=2, but peer-1 is SDOWN
	coord.election.RegisterVoter(coord.cfg.NodeID)
	coord.election.RegisterVoter("peer-1")
	coord.msp.AddPeer("peer-1", "127.0.0.1:9020", RoleReplica)
	coord.msp.mu.Lock()
	if p, ok := coord.msp.peers["peer-1"]; ok {
		p.SDown = true
	}
	coord.msp.mu.Unlock()

	// First call: sets quorumLostSince
	coord.checkQuorumHealth()
	if coord.IsLeader() {
		// Should still be leader — timeout hasn't elapsed
	}

	// Backdate quorumLostSince to simulate 5s elapsed
	coord.quorumMu.Lock()
	coord.quorumLostSince = time.Now().Add(-6 * time.Second)
	coord.quorumMu.Unlock()

	// This call should trigger demotion
	coord.checkQuorumHealth()

	// Give the async handleDemotion goroutine a moment to complete
	time.Sleep(50 * time.Millisecond)

	if coord.IsLeader() {
		t.Error("expected demotion after sustained quorum loss")
	}
}

func TestClusterCoordinator_QuorumRestored_ResetsTimer(t *testing.T) {
	coord, _ := newTestCoordinator(t, "auto")

	simulatePromotion(coord,1)
	coord.election.RegisterVoter(coord.cfg.NodeID)
	// Only self as voter, so quorum=1, which is always met.

	// Simulate a previous quorum loss timestamp
	coord.quorumMu.Lock()
	coord.quorumLostSince = time.Now().Add(-10 * time.Second)
	coord.quorumMu.Unlock()

	// With only self as voter, quorum is satisfied — should reset
	coord.checkQuorumHealth()

	coord.quorumMu.Lock()
	isZero := coord.quorumLostSince.IsZero()
	coord.quorumMu.Unlock()

	if !isZero {
		t.Error("expected quorumLostSince to be reset when quorum is healthy")
	}
}

func TestClusterCoordinator_HandleCogForward_DispatchesToWorkers(t *testing.T) {
	coord, _ := newTestCoordinator(t, "primary")

	hebbian := &mockHebbianSubmitter{}
	coord.SetCognitiveWorkers(hebbian)

	id1 := [16]byte{1}
	id2 := [16]byte{2}

	effect := mbp.CognitiveSideEffect{
		QueryID:      "q-1",
		OriginNodeID: "lobe-1",
		Timestamp:    time.Now().UnixNano(),
		CoActivations: []mbp.CoActivationRef{
			{ID: id1, Score: 0.8},
			{ID: id2, Score: 0.6},
		},
	}
	payload, err := msgpack.Marshal(effect)
	if err != nil {
		t.Fatal(err)
	}

	err = coord.HandleIncomingFrame("lobe-1", mbp.TypeCogForward, payload)
	if err != nil {
		t.Fatalf("HandleIncomingFrame CogForward: %v", err)
	}

	// HebbianWorker should have received one CoActivationEvent with 2 engrams.
	events := hebbian.Received()
	if len(events) != 1 {
		t.Fatalf("expected 1 CoActivationEvent, got %d", len(events))
	}
	if len(events[0].Engrams) != 2 {
		t.Errorf("expected 2 engrams, got %d", len(events[0].Engrams))
	}
	if events[0].Engrams[0].ID != id1 {
		t.Errorf("expected engram[0].ID=%v, got %v", id1, events[0].Engrams[0].ID)
	}
	if events[0].Engrams[0].Score != 0.8 {
		t.Errorf("expected engram[0].Score=0.8, got %v", events[0].Engrams[0].Score)
	}
}

func TestClusterCoordinator_HandleCogForward_DropsOnFullChannel(t *testing.T) {
	coord, _ := newTestCoordinator(t, "primary")

	// Workers simulate a full channel — Submit always returns false.
	hebbian := &mockHebbianSubmitter{full: true}
	coord.SetCognitiveWorkers(hebbian)

	effect := mbp.CognitiveSideEffect{
		QueryID: "q-drop",
		CoActivations: []mbp.CoActivationRef{
			{ID: [16]byte{9}, Score: 0.5},
		},
	}
	payload, err := msgpack.Marshal(effect)
	if err != nil {
		t.Fatal(err)
	}

	// Must not block or panic even when workers are full.
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = coord.HandleIncomingFrame("lobe-2", mbp.TypeCogForward, payload)
	}()

	select {
	case <-done:
		// good — did not block
	case <-time.After(500 * time.Millisecond):
		t.Fatal("handleCogForward blocked when channel was full")
	}

	// Nothing should have been recorded by the mock worker.
	if got := len(hebbian.Received()); got != 0 {
		t.Errorf("expected 0 hebbian events on full channel, got %d", got)
	}
}

func TestClusterCoordinator_HandleCogForward_SendsAck(t *testing.T) {
	coord, _ := newTestCoordinator(t, "primary")
	coord.SetCognitiveWorkers(&mockHebbianSubmitter{})

	// Create a net.Pipe() to inject a real conn into PeerConn so Send works.
	serverSide, clientSide := net.Pipe()
	t.Cleanup(func() {
		serverSide.Close()
		clientSide.Close()
	})

	peer := &PeerConn{nodeID: "lobe-ack", conn: serverSide}
	coord.mgr.mu.Lock()
	coord.mgr.peers["lobe-ack"] = peer
	coord.mgr.mu.Unlock()

	effect := mbp.CognitiveSideEffect{
		QueryID: "q-ack",
		CoActivations: []mbp.CoActivationRef{
			{ID: [16]byte{5}, Score: 0.9},
		},
	}
	payload, err := msgpack.Marshal(effect)
	if err != nil {
		t.Fatal(err)
	}

	// Read the ack frame from clientSide in a goroutine (Pipe blocks until written).
	type readResult struct {
		frameType uint8
		err       error
	}
	ch := make(chan readResult, 1)
	go func() {
		f, err := mbp.ReadFrame(clientSide)
		if err != nil {
			ch <- readResult{err: err}
			return
		}
		ch <- readResult{frameType: f.Type}
	}()

	err = coord.HandleIncomingFrame("lobe-ack", mbp.TypeCogForward, payload)
	if err != nil {
		t.Fatalf("HandleIncomingFrame CogForward: %v", err)
	}

	select {
	case res := <-ch:
		if res.err != nil {
			t.Fatalf("reading ack frame: %v", res.err)
		}
		if res.frameType != mbp.TypeCogAck {
			t.Errorf("expected TypeCogAck (0x%02x), got 0x%02x", mbp.TypeCogAck, res.frameType)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timed out waiting for CogAck frame")
	}
}

func TestClusterCoordinator_CogForwardedTotal(t *testing.T) {
	coord, _ := newTestCoordinator(t, "primary")
	coord.SetCognitiveWorkers(&mockHebbianSubmitter{})

	if total := coord.CogForwardedTotal(); total != 0 {
		t.Errorf("expected CogForwardedTotal=0 initially, got %d", total)
	}

	// Send a frame with 3 co-activations.
	effect := mbp.CognitiveSideEffect{
		QueryID: "q-count",
		CoActivations: []mbp.CoActivationRef{
			{ID: [16]byte{1}, Score: 0.1},
			{ID: [16]byte{2}, Score: 0.2},
			{ID: [16]byte{3}, Score: 0.3},
		},
	}
	payload, err := msgpack.Marshal(effect)
	if err != nil {
		t.Fatal(err)
	}

	if err := coord.HandleIncomingFrame("lobe-count", mbp.TypeCogForward, payload); err != nil {
		t.Fatalf("HandleIncomingFrame: %v", err)
	}

	if total := coord.CogForwardedTotal(); total != 3 {
		t.Errorf("expected CogForwardedTotal=3, got %d", total)
	}

	// Send another frame with 2 co-activations.
	effect2 := mbp.CognitiveSideEffect{
		QueryID: "q-count-2",
		CoActivations: []mbp.CoActivationRef{
			{ID: [16]byte{4}, Score: 0.4},
			{ID: [16]byte{5}, Score: 0.5},
		},
	}
	payload2, err := msgpack.Marshal(effect2)
	if err != nil {
		t.Fatal(err)
	}

	if err := coord.HandleIncomingFrame("lobe-count", mbp.TypeCogForward, payload2); err != nil {
		t.Fatalf("HandleIncomingFrame: %v", err)
	}

	if total := coord.CogForwardedTotal(); total != 5 {
		t.Errorf("expected CogForwardedTotal=5, got %d", total)
	}
}

func TestClusterCoordinator_HandleCogForward_RestoredEdges(t *testing.T) {
	coord, _ := newTestCoordinator(t, "primary")
	coord.SetCognitiveWorkers(&mockHebbianSubmitter{})

	if total := coord.CogForwardedTotal(); total != 0 {
		t.Errorf("expected CogForwardedTotal=0 initially, got %d", total)
	}

	// Send a frame with only RestoredEdges (no co-activations).
	effect := mbp.CognitiveSideEffect{
		QueryID: "q-restored",
		RestoredEdges: []mbp.EdgeRef{
			{Src: [16]byte{0xA1}, Dst: [16]byte{0xB1}},
			{Src: [16]byte{0xA2}, Dst: [16]byte{0xB2}},
		},
	}
	payload, err := msgpack.Marshal(effect)
	if err != nil {
		t.Fatal(err)
	}

	if err := coord.HandleIncomingFrame("lobe-restored", mbp.TypeCogForward, payload); err != nil {
		t.Fatalf("HandleIncomingFrame with RestoredEdges: %v", err)
	}

	// The coordinator should count restored edges in cogForwardedTotal.
	if total := coord.CogForwardedTotal(); total != 2 {
		t.Errorf("expected CogForwardedTotal=2 after 2 RestoredEdges, got %d", total)
	}

	// Send a second frame that mixes co-activations and RestoredEdges.
	effect2 := mbp.CognitiveSideEffect{
		QueryID: "q-mixed",
		CoActivations: []mbp.CoActivationRef{
			{ID: [16]byte{1}, Score: 0.5},
		},
		RestoredEdges: []mbp.EdgeRef{
			{Src: [16]byte{0xC1}, Dst: [16]byte{0xD1}},
		},
	}
	payload2, err := msgpack.Marshal(effect2)
	if err != nil {
		t.Fatal(err)
	}

	if err := coord.HandleIncomingFrame("lobe-restored", mbp.TypeCogForward, payload2); err != nil {
		t.Fatalf("HandleIncomingFrame mixed frame: %v", err)
	}

	// 2 restored + 1 co-activation + 1 restored = 4 total.
	if total := coord.CogForwardedTotal(); total != 4 {
		t.Errorf("expected CogForwardedTotal=4 after mixed frame, got %d", total)
	}
}

// mockFlushable tracks whether Stop() was called and uses a channel to signal
// completion, allowing tests to verify flush ordering without time.Sleep.
type mockFlushable struct {
	mu      sync.Mutex
	stopped bool
	stopCh  chan struct{} // close to let Stop() return
}

func newMockFlushable() *mockFlushable {
	return &mockFlushable{stopCh: make(chan struct{})}
}

func (m *mockFlushable) Stop() {
	m.mu.Lock()
	m.stopped = true
	m.mu.Unlock()
	<-m.stopCh // block until released
}

func (m *mockFlushable) wasStopped() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stopped
}

func (m *mockFlushable) release() {
	select {
	case <-m.stopCh:
	default:
		close(m.stopCh)
	}
}

// newMockFlushableImmediate returns a flushable whose Stop() returns immediately.
func newMockFlushableImmediate() *mockFlushable {
	m := newMockFlushable()
	close(m.stopCh) // pre-released
	return m
}

func TestGracefulFailover_Success(t *testing.T) {
	coord, _ := newTestCoordinator(t, "primary")

	// Promote to Cortex.
	simulatePromotion(coord,1)
	coord.epochStore.ForceSet(1)

	// Set up a target peer with a net.Pipe so Send works.
	targetID := "target-1"
	serverSide, clientSide := net.Pipe()
	t.Cleanup(func() { serverSide.Close(); clientSide.Close() })

	targetPeer := &PeerConn{nodeID: targetID, conn: serverSide}
	coord.mgr.mu.Lock()
	coord.mgr.peers[targetID] = targetPeer
	coord.mgr.mu.Unlock()

	// Wire mock flushers (immediate return).
	coord.SetCognitiveFlushers(newMockFlushableImmediate())

	// Pre-seed a replica seq so convergence check sees it caught up.
	coord.UpdateReplicaSeq(targetID, coord.repLog.CurrentSeq())

	// In a goroutine, read the HANDOFF frame from clientSide and deliver the ACK
	// via HandleHandoffAck (simulating the frame dispatch loop).
	ackDone := make(chan error, 1)
	go func() {
		f, err := mbp.ReadFrame(clientSide)
		if err != nil {
			ackDone <- err
			return
		}
		if f.Type != mbp.TypeHandoff {
			ackDone <- fmt.Errorf("expected TypeHandoff (0x%02x), got 0x%02x", mbp.TypeHandoff, f.Type)
			return
		}
		var msg mbp.HandoffMessage
		if err := msgpack.Unmarshal(f.Payload, &msg); err != nil {
			ackDone <- err
			return
		}
		if msg.TargetID != targetID {
			ackDone <- fmt.Errorf("expected TargetID=%q, got %q", targetID, msg.TargetID)
			return
		}

		// Simulate target sending HANDOFF_ACK by calling HandleHandoffAck directly.
		ack := mbp.HandoffAck{TargetID: targetID, Epoch: msg.Epoch + 1, Success: true}
		ackPayload, _ := msgpack.Marshal(ack)
		ackDone <- coord.HandleHandoffAck(targetID, ackPayload)
	}()

	// Run GracefulFailover.
	ctx := context.Background()
	err := coord.GracefulFailover(ctx, targetID)

	// Wait for the ack goroutine.
	if ackErr := <-ackDone; ackErr != nil {
		t.Fatalf("ack goroutine error: %v", ackErr)
	}

	if err != nil {
		t.Fatalf("GracefulFailover: %v", err)
	}

	// Cortex should now be demoted.
	if coord.IsLeader() {
		t.Error("expected Cortex to be demoted after successful handoff")
	}

	// Node state should be back to normal (demotion path, not error).
	if coord.IsDraining() {
		t.Error("expected draining=false after successful handoff")
	}
}

func TestGracefulFailover_ConvergenceTimeout(t *testing.T) {
	coord, _ := newTestCoordinator(t, "primary")
	simulatePromotion(coord,1)
	coord.epochStore.ForceSet(1)

	// Add a target peer.
	targetID := "target-conv"
	coord.mgr.AddPeer(targetID, "127.0.0.1:9999")

	// Wire immediate flushers.
	coord.SetCognitiveFlushers(newMockFlushableImmediate())

	// Append an entry so cortexSeq > 0.
	coord.repLog.Append(OpSet, []byte("k"), []byte("v"))

	// Pre-seed replica seq that is behind — never catches up.
	coord.UpdateReplicaSeq(targetID, 0)

	// Use a very short context timeout to simulate the 30s convergence timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	err := coord.GracefulFailover(ctx, targetID)
	if err == nil {
		t.Fatal("expected error from convergence timeout")
	}

	// Cortex should still be leader (handoff aborted).
	if !coord.IsLeader() {
		t.Error("expected Cortex to remain leader after convergence timeout")
	}

	// State should be restored to Normal.
	if coord.IsDraining() {
		t.Error("expected draining=false after aborted handoff")
	}
}

func TestGracefulFailover_AckTimeout(t *testing.T) {
	coord, _ := newTestCoordinator(t, "primary")
	simulatePromotion(coord,1)
	coord.epochStore.ForceSet(1)

	// Set up target peer with a pipe that reads but never sends ACK.
	targetID := "target-noack"
	serverSide, clientSide := net.Pipe()
	t.Cleanup(func() { serverSide.Close(); clientSide.Close() })

	targetPeer := &PeerConn{nodeID: targetID, conn: serverSide}
	coord.mgr.mu.Lock()
	coord.mgr.peers[targetID] = targetPeer
	coord.mgr.mu.Unlock()

	coord.SetCognitiveFlushers(newMockFlushableImmediate())

	// No entries → convergence is immediate.

	// Read the handoff frame but do NOT send an ack.
	go func() {
		_, _ = mbp.ReadFrame(clientSide)
		// intentionally do not respond
	}()

	err := coord.GracefulFailover(context.Background(), targetID)
	if err == nil {
		t.Fatal("expected error from HANDOFF_ACK timeout")
	}

	// Cortex should still be leader.
	if !coord.IsLeader() {
		t.Error("expected Cortex to remain leader after ack timeout")
	}
	if coord.IsDraining() {
		t.Error("expected draining=false after ack timeout")
	}
}

func TestGracefulFailover_DrainRejectsWrites(t *testing.T) {
	coord, _ := newTestCoordinator(t, "primary")
	simulatePromotion(coord,1)
	coord.epochStore.ForceSet(1)

	// Set up target peer (no pipe needed — we use AddPeer for a non-connected peer
	// since the test will cancel before reaching the Send step).
	targetID := "target-drain"
	serverSide, clientSide := net.Pipe()
	t.Cleanup(func() { serverSide.Close(); clientSide.Close() })

	targetPeer := &PeerConn{nodeID: targetID, conn: serverSide}
	coord.mgr.mu.Lock()
	coord.mgr.peers[targetID] = targetPeer
	coord.mgr.mu.Unlock()

	// Use a blocking flusher so we can observe DRAINING state while blocked.
	blockingFlusher := newMockFlushable()
	coord.SetCognitiveFlushers(blockingFlusher)

	// Signal channels for state observation.
	drainingObserved := make(chan bool, 1)

	go func() {
		for {
			if coord.IsDraining() {
				drainingObserved <- true
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()

	// Run failover in background with a short timeout so it doesn't hang.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	failoverDone := make(chan error, 1)
	go func() {
		failoverDone <- coord.GracefulFailover(ctx, targetID)
	}()

	// Wait for draining state.
	select {
	case isDraining := <-drainingObserved:
		if !isDraining {
			t.Fatal("expected IsDraining to be true during handoff")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for DRAINING state")
	}

	// Release the blocking flusher so failover can proceed.
	blockingFlusher.release()

	// Drain the handoff frame from the pipe.
	go func() { _, _ = mbp.ReadFrame(clientSide) }()

	// Failover will timeout on ack (no responder).
	select {
	case <-failoverDone:
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for failover to complete")
	}
}

func TestClusterCoordinator_UpdateReplicaSeq(t *testing.T) {
	coord, _ := newTestCoordinator(t, "primary")

	// Initially no lag (no replicas tracked).
	if lag := coord.ReplicaLag("lobe-1"); lag != 0 {
		t.Errorf("expected lag=0 for unknown replica, got %d", lag)
	}

	// Append some entries.
	coord.repLog.Append(OpSet, []byte("k1"), []byte("v1"))
	coord.repLog.Append(OpSet, []byte("k2"), []byte("v2"))
	coord.repLog.Append(OpSet, []byte("k3"), []byte("v3"))

	cortexSeq := coord.repLog.CurrentSeq()

	// Update replica to seq 1 — lag should be cortexSeq - 1.
	coord.UpdateReplicaSeq("lobe-1", 1)
	lag := coord.ReplicaLag("lobe-1")
	if lag != cortexSeq-1 {
		t.Errorf("expected lag=%d, got %d", cortexSeq-1, lag)
	}

	// Update replica to cortexSeq — lag should be 0.
	coord.UpdateReplicaSeq("lobe-1", cortexSeq)
	lag = coord.ReplicaLag("lobe-1")
	if lag != 0 {
		t.Errorf("expected lag=0 when caught up, got %d", lag)
	}
}

func TestClusterCoordinator_HandleHandoff_PromotesTarget(t *testing.T) {
	coord, _ := newTestCoordinator(t, "replica")
	coord.epochStore.ForceSet(5)

	// Track promotion callback.
	var promotedEpoch uint64
	coord.OnBecameCortex = func(epoch uint64) {
		promotedEpoch = epoch
	}

	// Set up a "from" peer (old cortex) with a pipe so we can read the ack.
	fromID := "old-cortex"
	serverSide, clientSide := net.Pipe()
	t.Cleanup(func() { serverSide.Close(); clientSide.Close() })

	fromPeer := &PeerConn{nodeID: fromID, conn: serverSide}
	coord.mgr.mu.Lock()
	coord.mgr.peers[fromID] = fromPeer
	coord.mgr.mu.Unlock()

	// Build HANDOFF payload.
	msg := mbp.HandoffMessage{
		TargetID:  coord.cfg.NodeID,
		Epoch:     5,
		CortexSeq: 100,
	}
	payload, _ := msgpack.Marshal(msg)

	// Read the ack in background.
	ackCh := make(chan mbp.HandoffAck, 1)
	go func() {
		f, err := mbp.ReadFrame(clientSide)
		if err != nil {
			return
		}
		// There might be a CortexClaim broadcast first — read until we get the ack.
		for f.Type != mbp.TypeHandoffAck {
			f, err = mbp.ReadFrame(clientSide)
			if err != nil {
				return
			}
		}
		var ack mbp.HandoffAck
		msgpack.Unmarshal(f.Payload, &ack)
		ackCh <- ack
	}()

	err := coord.HandleHandoff(fromID, payload)
	if err != nil {
		t.Fatalf("HandleHandoff: %v", err)
	}

	// Verify promotion.
	if coord.Role() != RolePrimary {
		t.Errorf("expected role=RolePrimary after handoff, got %v", coord.Role())
	}
	if promotedEpoch != 6 {
		t.Errorf("expected promotedEpoch=6, got %d", promotedEpoch)
	}

	// Verify epoch was incremented.
	if epoch := coord.CurrentEpoch(); epoch != 6 {
		t.Errorf("expected epoch=6, got %d", epoch)
	}

	// Verify ack.
	select {
	case ack := <-ackCh:
		if !ack.Success {
			t.Error("expected ack.Success=true")
		}
		if ack.Epoch != 6 {
			t.Errorf("expected ack.Epoch=6, got %d", ack.Epoch)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for HANDOFF_ACK")
	}
}
