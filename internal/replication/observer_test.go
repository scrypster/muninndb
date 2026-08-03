//go:build integration

package replication

import (
	"context"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// assertNeverObs polls cond every 5ms for up to maxWait.
// Fails the test immediately if cond ever returns true.
// (Named assertNeverObs to avoid redeclaration in the same package across build tags.)
func assertNeverObs(t *testing.T, maxWait time.Duration, cond func() bool, msg string) {
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
// Test 1: Observer receives and applies full replication stream
// ---------------------------------------------------------------------------

// TestObserver_ReceivesReplication sets up a Cortex + Observer, writes 10 keys
// to the Cortex replication log, and verifies the Observer applies all 10.
func TestObserver_ReceivesReplication(t *testing.T) {
	cortex := newTestNode(t, "cortex", "primary")
	observer := newTestNode(t, "observer", "observer")

	// Wire bidirectional connection.
	connectNodes(t, cortex, observer)

	// Bootstrap cortex as leader (single-voter cluster).
	registerVoters(cortex)
	if err := cortex.coord.election.StartElection(context.Background()); err != nil {
		t.Fatalf("StartElection: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool {
		return cortex.coord.IsLeader()
	}, "cortex to become leader")

	// Verify observer role is set correctly.
	if cortex.coord.IsObserver() {
		t.Error("cortex should not be observer")
	}

	// Start a NetworkStreamer from cortex -> observer before appending entries.
	cancelStream := startStreamer(t, cortex, observer, 0)
	defer cancelStream()

	// Write 10 keys to the Cortex replication log.
	appendEntries(t, cortex, "obs", 10)

	// Wait for Observer to apply all 10 entries.
	waitFor(t, 5*time.Second, func() bool {
		return observer.applier.LastApplied() >= 10
	}, "observer to apply 10 entries")

	if last := observer.applier.LastApplied(); last != 10 {
		t.Errorf("observer lastApplied=%d, expected 10", last)
	}

	// Verify entries are in observer's Pebble DB.
	verifyEntries(t, observer, "obs", 10)
}

// ---------------------------------------------------------------------------
// Test 2: Observer does not grant election votes
// ---------------------------------------------------------------------------

// TestObserver_NoElectionVote sets up Cortex + Lobe + Observer. Kills Cortex.
// Verifies election proceeds with quorum = 2 out of 2 non-observer nodes
// (Cortex + Lobe). The Observer's vote is not counted.
func TestObserver_NoElectionVote(t *testing.T) {
	cortex := newTestNode(t, "cortex", "primary")
	lobe := newTestNode(t, "lobe", "replica")
	observer := newTestNode(t, "observer", "observer")

	// Wire all pairs.
	connectNodes(t, cortex, lobe)
	connectNodes(t, cortex, observer)
	connectNodes(t, lobe, observer)

	// Only cortex and lobe are voters; observer is intentionally not registered.
	registerVoters(cortex, lobe)
	// observer's election.SetObserver(true) is done internally when role="observer"
	// is passed to coordinator.Run, but in tests we wire it manually.
	observer.coord.election.SetObserver(true)

	// Bootstrap cortex.
	if err := cortex.coord.election.StartElection(context.Background()); err != nil {
		t.Fatalf("StartElection: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool {
		return cortex.coord.IsLeader()
	}, "cortex to become leader")

	initialEpoch := cortex.coord.CurrentEpoch()

	// Simulate cortex failure: mark it SDOWN in lobe's MSP.
	lobe.coord.msp.AddPeer("cortex", "pipe", RolePrimary)
	lobe.coord.msp.mu.Lock()
	if p, ok := lobe.coord.msp.peers["cortex"]; ok {
		p.SDown = true
		p.MissedBeats = 10
	}
	lobe.coord.msp.mu.Unlock()

	// Verify the observer would refuse to grant a vote.
	req := mbpVoteRequest(t, "lobe", initialEpoch+1)
	resp := observer.coord.election.HandleVoteRequest(req)
	if resp.Granted {
		t.Error("observer should not grant a vote")
	}

	// Lobe starts a new election. With quorum=2 (cortex+lobe registered),
	// lobe needs self-vote + any other non-observer vote.
	// In the test we simulate lobe getting its own self-vote => quorum of voters
	// registered in lobe's election = 2 (cortex, lobe). Quorum = 2/2+1 = 2.
	// Lobe starts candidate, votes for self (1 vote). Needs 1 more from cortex.
	// Since cortex is "dead", lobe can't get quorum from cortex alone.
	// However in this test we want to verify election CAN succeed if cortex
	// responds — we simulate a vote from cortex.
	if err := lobe.coord.election.StartElection(context.Background()); err != nil {
		t.Fatalf("lobe StartElection: %v", err)
	}

	// Simulate cortex granting its vote to lobe (epoch = lobe's candidateEpoch).
	lobeEpoch := lobe.coord.election.CurrentEpoch()
	lobe.coord.election.HandleVoteResponse(mbpVoteResponse(t, "cortex", lobeEpoch, true))

	waitFor(t, 5*time.Second, func() bool {
		return lobe.coord.IsLeader()
	}, "lobe to become leader after cortex failure")

	if lobe.coord.CurrentEpoch() <= initialEpoch {
		t.Errorf("lobe epoch=%d, expected > %d", lobe.coord.CurrentEpoch(), initialEpoch)
	}
}

// ---------------------------------------------------------------------------
// Test 3: Observers do not affect quorum count
// ---------------------------------------------------------------------------

// TestObserver_DoesNotAffectQuorum verifies that with 1 Cortex + 1 Lobe +
// 2 Observers in the MSP peer table:
//  1. MSP.NonObserverQuorum() = 2 (self + cortex, excluding the 2 observers).
//  2. Observers refuse to grant votes (HandleVoteRequest returns Granted=false).
//  3. An election with 2 registered voters (cortex+lobe) requires quorum=2.
//     Lobe alone (1 self-vote) cannot reach quorum without cortex's vote.
func TestObserver_DoesNotAffectQuorum(t *testing.T) {
	lobe := newTestNode(t, "lobe", "replica")
	obs1 := newTestNode(t, "obs1", "observer")
	obs2 := newTestNode(t, "obs2", "observer")

	obs1.coord.election.SetObserver(true)
	obs2.coord.election.SetObserver(true)

	// --- Part 1: MSP NonObserverQuorum calculation ---
	// In lobe's MSP, register cortex (Primary) and both observers as peers.
	lobe.coord.msp.AddPeer("cortex", "pipe", RolePrimary)
	lobe.coord.msp.AddPeer("obs1", "pipe", RoleObserver)
	lobe.coord.msp.AddPeer("obs2", "pipe", RoleObserver)

	// NonObserverQuorum: peers = cortex(primary) + obs1(observer) + obs2(observer)
	// non-observer count = self(1) + cortex(1) = 2 => quorum = 2/2+1 = 2
	q := lobe.coord.msp.NonObserverQuorum()
	if q != 2 {
		t.Errorf("expected NonObserverQuorum=2, got %d", q)
	}

	// --- Part 2: Observers refuse to grant votes ---
	// Register lobe and cortex as voters; obs1/obs2 are not voters.
	lobe.coord.election.RegisterVoter("lobe")
	lobe.coord.election.RegisterVoter("cortex")
	obs1.coord.election.RegisterVoter("lobe")
	obs1.coord.election.RegisterVoter("cortex")
	obs2.coord.election.RegisterVoter("lobe")
	obs2.coord.election.RegisterVoter("cortex")

	// Simulate lobe starting an election at epoch 2.
	testEpoch := uint64(2)
	if err := obs1.coord.epochStore.Advance(testEpoch - 1); err != nil {
		t.Fatal(err)
	}
	if err := obs2.coord.epochStore.Advance(testEpoch - 1); err != nil {
		t.Fatal(err)
	}

	obs1VoteResp := obs1.coord.election.HandleVoteRequest(mbpVoteRequest(t, "lobe", testEpoch))
	obs2VoteResp := obs2.coord.election.HandleVoteRequest(mbpVoteRequest(t, "lobe", testEpoch))
	if obs1VoteResp.Granted {
		t.Error("obs1 should not grant a vote (observer)")
	}
	if obs2VoteResp.Granted {
		t.Error("obs2 should not grant a vote (observer)")
	}

	// --- Part 3: Election quorum with cortex dead ---
	// Lobe has 2 registered voters (cortex + lobe), quorum = 2.
	// With cortex dead, lobe can only accumulate 1 vote (self). Election fails.
	electionQuorum := lobe.coord.election.Quorum()
	if electionQuorum != 2 {
		t.Errorf("expected election quorum=2 (cortex+lobe registered), got %d", electionQuorum)
	}

	// Start election: lobe votes for self (1 vote). Needs 1 more from cortex.
	if err := lobe.coord.election.StartElection(context.Background()); err != nil {
		t.Fatalf("lobe StartElection: %v", err)
	}

	// Observer votes must not count. Since observers refuse to grant votes,
	// no additional Granted=true responses come in from non-voters.
	// Poll for 100ms and fail immediately if lobe becomes leader.
	assertNeverObs(t, 100*time.Millisecond, func() bool {
		return lobe.coord.IsLeader()
	}, "lobe should NOT become leader with only self-vote (quorum=2 requires 2 votes)")

	// Confirm that the election remains in candidate state (waiting for cortex vote).
	if lobe.coord.election.State() != ElectionCandidate {
		t.Errorf("expected ElectionCandidate, got %v", lobe.coord.election.State())
	}
}

// ---------------------------------------------------------------------------
// Helpers for constructing MBP vote messages in tests
// ---------------------------------------------------------------------------

func mbpVoteRequest(t *testing.T, candidateID string, epoch uint64) mbp.VoteRequest {
	t.Helper()
	return mbp.VoteRequest{CandidateID: candidateID, Epoch: epoch}
}

func mbpVoteResponse(t *testing.T, voterID string, epoch uint64, granted bool) mbp.VoteResponse {
	t.Helper()
	return mbp.VoteResponse{VoterID: voterID, Epoch: epoch, Granted: granted}
}
