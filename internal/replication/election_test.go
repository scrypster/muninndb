package replication

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// newTestElection creates an Election with a Pebble-backed EpochStore and
// a ConnManager. The local node is registered as a voter by default.
func newTestElection(t *testing.T, localNodeID string) *Election {
	t.Helper()
	db := openTestDB(t, t.TempDir())
	t.Cleanup(func() { db.Close() })

	es, err := NewEpochStore(db)
	if err != nil {
		t.Fatalf("NewEpochStore: %v", err)
	}

	mgr := NewConnManager(localNodeID)
	t.Cleanup(func() { mgr.Close() })

	el := NewElection(localNodeID, es, mgr)
	el.RegisterVoter(localNodeID)
	return el
}

func TestElection_StartElection_BecomesCortex(t *testing.T) {
	el := newTestElection(t, "node-A")
	el.RegisterVoter("node-B")
	el.RegisterVoter("node-C")
	// quorum = 3/2 + 1 = 2

	var promotedEpoch uint64
	var promotedCalls int32
	el.OnPromoted = func(epoch uint64) {
		atomic.StoreUint64(&promotedEpoch, epoch)
		atomic.AddInt32(&promotedCalls, 1)
	}

	ctx := context.Background()
	if err := el.StartElection(ctx); err != nil {
		t.Fatalf("StartElection: %v", err)
	}

	if el.State() != ElectionCandidate {
		t.Fatalf("expected ElectionCandidate, got %d", el.State())
	}

	// Simulate vote from node-B (grants).
	el.HandleVoteResponse(mbp.VoteResponse{
		VoterID: "node-B",
		Epoch:   1,
		Granted: true,
	})

	if el.State() != ElectionLeader {
		t.Fatalf("expected ElectionLeader after quorum, got %d", el.State())
	}
	if atomic.LoadInt32(&promotedCalls) != 1 {
		t.Errorf("OnPromoted called %d times, want 1", atomic.LoadInt32(&promotedCalls))
	}
	if atomic.LoadUint64(&promotedEpoch) != 1 {
		t.Errorf("OnPromoted epoch = %d, want 1", atomic.LoadUint64(&promotedEpoch))
	}
	if el.CurrentLeader() != "node-A" {
		t.Errorf("CurrentLeader = %q, want %q", el.CurrentLeader(), "node-A")
	}
}

func TestElection_StartElection_QuorumNotReached(t *testing.T) {
	el := newTestElection(t, "node-A")
	el.RegisterVoter("node-B")
	el.RegisterVoter("node-C")
	// quorum = 2, self-vote = 1

	var promoted bool
	el.OnPromoted = func(epoch uint64) {
		promoted = true
	}

	ctx := context.Background()
	if err := el.StartElection(ctx); err != nil {
		t.Fatalf("StartElection: %v", err)
	}

	// Only one external vote that rejects.
	el.HandleVoteResponse(mbp.VoteResponse{
		VoterID: "node-B",
		Epoch:   1,
		Granted: false,
	})

	if el.State() != ElectionCandidate {
		t.Fatalf("expected ElectionCandidate (quorum not reached), got %d", el.State())
	}
	if promoted {
		t.Error("OnPromoted should not have been called")
	}
}

func TestElection_HandleVoteRequest_GrantsOnlyOncePerEpoch(t *testing.T) {
	el := newTestElection(t, "voter-1")

	// Two candidates request votes for the same epoch.
	resp1 := el.HandleVoteRequest(mbp.VoteRequest{
		CandidateID: "candidate-A",
		Epoch:       5,
	})
	resp2 := el.HandleVoteRequest(mbp.VoteRequest{
		CandidateID: "candidate-B",
		Epoch:       5,
	})

	if !resp1.Granted {
		t.Error("first VoteRequest should be granted")
	}
	if resp2.Granted {
		t.Error("second VoteRequest for same epoch from different candidate must be rejected")
	}

	// Idempotent: same candidate again should still be granted.
	resp3 := el.HandleVoteRequest(mbp.VoteRequest{
		CandidateID: "candidate-A",
		Epoch:       5,
	})
	if !resp3.Granted {
		t.Error("idempotent vote for same candidate should be granted")
	}
}

func TestElection_HandleCortexClaim_DemotesLeader(t *testing.T) {
	el := newTestElection(t, "node-A")
	// quorum = 1 (only self), so StartElection auto-promotes.

	var demotedCalls int32
	var newLeaderID string
	var newLeaderEpoch uint64
	el.OnDemoted = func() {
		atomic.AddInt32(&demotedCalls, 1)
	}
	el.OnNewLeader = func(leaderID string, epoch uint64) {
		newLeaderID = leaderID
		newLeaderEpoch = epoch
	}

	ctx := context.Background()
	if err := el.StartElection(ctx); err != nil {
		t.Fatalf("StartElection: %v", err)
	}
	if el.State() != ElectionLeader {
		t.Fatalf("expected ElectionLeader (single voter), got %d", el.State())
	}

	// Another node claims cortex with a higher epoch.
	el.HandleCortexClaim(mbp.CortexClaim{
		CortexID: "node-B",
		Epoch:    5,
	})

	if el.State() != ElectionFollower {
		t.Fatalf("expected ElectionFollower after CortexClaim, got %d", el.State())
	}
	if atomic.LoadInt32(&demotedCalls) != 1 {
		t.Errorf("OnDemoted called %d times, want 1", atomic.LoadInt32(&demotedCalls))
	}
	if newLeaderID != "node-B" {
		t.Errorf("OnNewLeader leaderID = %q, want %q", newLeaderID, "node-B")
	}
	if newLeaderEpoch != 5 {
		t.Errorf("OnNewLeader epoch = %d, want 5", newLeaderEpoch)
	}
	if el.CurrentLeader() != "node-B" {
		t.Errorf("CurrentLeader = %q, want %q", el.CurrentLeader(), "node-B")
	}
}

func TestElection_HandleCortexClaim_RejectsStaleEpoch(t *testing.T) {
	el := newTestElection(t, "node-A")

	// Advance epoch to 10.
	if _, err := el.epochStore.Advance(10); err != nil {
		t.Fatalf("Advance: %v", err)
	}

	var newLeaderCalled bool
	el.OnNewLeader = func(leaderID string, epoch uint64) {
		newLeaderCalled = true
	}

	// Stale claim with epoch 5 (< current 10) should be ignored.
	el.HandleCortexClaim(mbp.CortexClaim{
		CortexID: "node-B",
		Epoch:    5,
	})

	if el.State() != ElectionIdle {
		t.Errorf("state should remain ElectionIdle, got %d", el.State())
	}
	if newLeaderCalled {
		t.Error("OnNewLeader should not be called for stale CortexClaim")
	}
}

func TestElection_Quorum(t *testing.T) {
	tests := []struct {
		voters int
		want   int
	}{
		{1, 1},
		{2, 2},
		{3, 2},
		{4, 3},
		{5, 3},
		{6, 4},
		{7, 4},
		{10, 6},
	}

	for _, tc := range tests {
		db := openTestDB(t, t.TempDir())
		es, err := NewEpochStore(db)
		if err != nil {
			t.Fatalf("NewEpochStore: %v", err)
		}
		mgr := NewConnManager("local")

		el := NewElection("local", es, mgr)
		for i := 0; i < tc.voters; i++ {
			el.RegisterVoter(string(rune('A' + i)))
		}

		got := el.Quorum()
		if got != tc.want {
			t.Errorf("Quorum() with %d voters = %d, want %d", tc.voters, got, tc.want)
		}

		mgr.Close()
		db.Close()
	}
}

func TestElection_SplitVote_NoWinner(t *testing.T) {
	// 4 voters: node-A, node-B, node-C, node-D. quorum = 3.
	// Two candidates each get 2 votes -> neither wins.

	elA := newTestElection(t, "node-A")
	elA.RegisterVoter("node-B")
	elA.RegisterVoter("node-C")
	elA.RegisterVoter("node-D")

	elB := newTestElection(t, "node-B")
	elB.RegisterVoter("node-A")
	elB.RegisterVoter("node-C")
	elB.RegisterVoter("node-D")

	var promotedA, promotedB bool
	elA.OnPromoted = func(epoch uint64) { promotedA = true }
	elB.OnPromoted = func(epoch uint64) { promotedB = true }

	ctx := context.Background()

	// Both start elections (they'll get different epochs: A gets 1, B gets 1 from its own store).
	if err := elA.StartElection(ctx); err != nil {
		t.Fatalf("elA.StartElection: %v", err)
	}
	if err := elB.StartElection(ctx); err != nil {
		t.Fatalf("elB.StartElection: %v", err)
	}

	// node-A gets vote from node-C (epoch 1): total 2 (self + C). quorum = 3. Not enough.
	elA.HandleVoteResponse(mbp.VoteResponse{
		VoterID: "node-C",
		Epoch:   1,
		Granted: true,
	})

	// node-B gets vote from node-D (epoch 1): total 2 (self + D). quorum = 3. Not enough.
	elB.HandleVoteResponse(mbp.VoteResponse{
		VoterID: "node-D",
		Epoch:   1,
		Granted: true,
	})

	if elA.State() != ElectionCandidate {
		t.Errorf("elA state = %d, want ElectionCandidate", elA.State())
	}
	if elB.State() != ElectionCandidate {
		t.Errorf("elB state = %d, want ElectionCandidate", elB.State())
	}
	if promotedA {
		t.Error("node-A should not be promoted (split vote)")
	}
	if promotedB {
		t.Error("node-B should not be promoted (split vote)")
	}
}

func TestElection_EpochMonotonicity(t *testing.T) {
	el := newTestElection(t, "voter-1")

	// Node starts at epoch 0. Receives VoteRequest with epoch 5.
	if el.CurrentEpoch() != 0 {
		t.Fatalf("initial epoch = %d, want 0", el.CurrentEpoch())
	}

	resp := el.HandleVoteRequest(mbp.VoteRequest{
		CandidateID: "candidate-X",
		Epoch:       5,
	})

	if !resp.Granted {
		t.Error("should grant vote for higher epoch")
	}

	// Epoch should now be 5.
	if el.CurrentEpoch() != 5 {
		t.Errorf("epoch after VoteRequest = %d, want 5", el.CurrentEpoch())
	}

	// A VoteRequest with epoch 3 (< 5) should be rejected.
	resp2 := el.HandleVoteRequest(mbp.VoteRequest{
		CandidateID: "candidate-Y",
		Epoch:       3,
	})
	if resp2.Granted {
		t.Error("should reject vote for stale epoch")
	}
}

func TestElection_StartElection_RejectsWhenAlreadyCandidate(t *testing.T) {
	el := newTestElection(t, "node-A")
	el.RegisterVoter("node-B")
	el.RegisterVoter("node-C")

	ctx := context.Background()
	if err := el.StartElection(ctx); err != nil {
		t.Fatalf("first StartElection: %v", err)
	}

	// Second call should fail.
	if err := el.StartElection(ctx); err == nil {
		t.Error("expected error on second StartElection while candidate")
	}
}

func TestElection_StartElection_SingleNodeCluster(t *testing.T) {
	el := newTestElection(t, "solo")
	// quorum = 1 (only self)

	var promotedEpoch uint64
	el.OnPromoted = func(epoch uint64) {
		promotedEpoch = epoch
	}

	ctx := context.Background()
	if err := el.StartElection(ctx); err != nil {
		t.Fatalf("StartElection: %v", err)
	}

	if el.State() != ElectionLeader {
		t.Fatalf("single-node should auto-promote, got state %d", el.State())
	}
	if promotedEpoch != 1 {
		t.Errorf("promoted epoch = %d, want 1", promotedEpoch)
	}
}

func TestElection_HandleVoteRequest_ConcurrentSameEpoch(t *testing.T) {
	// Verify that under concurrent VoteRequests for the same epoch from
	// different candidates, exactly one gets granted.
	el := newTestElection(t, "voter")

	const goroutines = 20
	var wg sync.WaitGroup
	granted := make(chan string, goroutines)

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		candidateID := string(rune('A' + i))
		go func(cid string) {
			defer wg.Done()
			resp := el.HandleVoteRequest(mbp.VoteRequest{
				CandidateID: cid,
				Epoch:       10,
			})
			if resp.Granted {
				granted <- cid
			}
		}(candidateID)
	}

	wg.Wait()
	close(granted)

	// All granted votes must be for the same candidate.
	var winner string
	count := 0
	for cid := range granted {
		count++
		if winner == "" {
			winner = cid
		} else if cid != winner {
			t.Errorf("granted vote to %q and %q in same epoch -- safety violation", winner, cid)
		}
	}

	if count == 0 {
		t.Error("expected at least one granted vote")
	}
}

func TestElection_NilCallbacks(t *testing.T) {
	// Ensure no panic when callbacks are nil.
	el := newTestElection(t, "node-A")

	ctx := context.Background()
	if err := el.StartElection(ctx); err != nil {
		t.Fatalf("StartElection: %v", err)
	}

	// Should not panic with nil OnPromoted.
	if el.State() != ElectionLeader {
		t.Fatalf("single-voter should auto-promote, got %d", el.State())
	}

	// CortexClaim with nil OnDemoted / OnNewLeader.
	el.HandleCortexClaim(mbp.CortexClaim{
		CortexID: "other",
		Epoch:    10,
	})
	if el.State() != ElectionFollower {
		t.Errorf("expected ElectionFollower, got %d", el.State())
	}
}

// TestObserver_CannotStartElection verifies that an Election marked as Observer
// returns errObserverCannotElect when StartElection is called.
func TestObserver_CannotStartElection(t *testing.T) {
	el := newTestElection(t, "observer-node")
	el.SetObserver(true)

	err := el.StartElection(context.Background())
	if err == nil {
		t.Fatal("expected error from observer StartElection, got nil")
	}
	if err != errObserverCannotElect {
		t.Errorf("expected errObserverCannotElect, got: %v", err)
	}

	// State must remain Idle — the observer never transitions to candidate.
	if el.State() != ElectionIdle {
		t.Errorf("expected ElectionIdle after failed observer StartElection, got %v", el.State())
	}
}
