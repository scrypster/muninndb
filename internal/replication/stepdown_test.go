package replication

import (
	"context"
	"testing"
)

// #522 Step 3: the quorum-loss demotion path leaves election.state == Leader, so
// StartElection would return errAlreadyCandidate forever (the second lock on the
// zombie). StepDown relinquishes leadership without a successor so a later
// re-election can proceed.
func TestElection_StepDown_AllowsReElection(t *testing.T) {
	el := newTestElection(t, "self") // single voter → StartElection auto-promotes
	if err := el.StartElection(context.Background()); err != nil {
		t.Fatalf("StartElection: %v", err)
	}
	if el.State() != ElectionLeader {
		t.Fatalf("expected leader, got state %d", el.State())
	}

	el.StepDown()

	if el.State() != ElectionIdle {
		t.Errorf("StepDown should set state Idle, got %d", el.State())
	}
	if el.CurrentLeader() != "" {
		t.Errorf("StepDown should clear self-leadership, currentLeader=%q", el.CurrentLeader())
	}
	// A fresh election must now be possible (was blocked by errAlreadyCandidate).
	if err := el.StartElection(context.Background()); err != nil {
		t.Errorf("StartElection after StepDown should succeed, got %v", err)
	}
	if el.State() != ElectionLeader {
		t.Errorf("expected re-election to promote (single voter), got state %d", el.State())
	}
}

// StepDown must NOT clear currentLeader when another node already holds it (a
// concurrent CortexClaim installed a real leader).
func TestElection_StepDown_KeepsForeignLeader(t *testing.T) {
	el := newTestElection(t, "self")
	el.mu.Lock()
	el.state = ElectionFollower
	el.currentLeader = "other"
	el.mu.Unlock()

	el.StepDown()

	if el.CurrentLeader() != "other" {
		t.Errorf("StepDown must not clear a foreign leader, got %q", el.CurrentLeader())
	}
}

// IsVoter reports registered-voter membership (used by aliveVoters()).
func TestElection_IsVoter(t *testing.T) {
	el := newTestElection(t, "self") // harness registers self
	el.RegisterVoter("v1")
	if !el.IsVoter("v1") {
		t.Error("expected v1 to be a voter")
	}
	if el.IsVoter("stranger") {
		t.Error("stranger should not be a voter")
	}
	el.UnregisterVoter("v1")
	if el.IsVoter("v1") {
		t.Error("v1 should no longer be a voter after UnregisterVoter")
	}
}
