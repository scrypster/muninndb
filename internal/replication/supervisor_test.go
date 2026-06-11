package replication

import (
	"context"
	"testing"
	"time"
)

// #522 Step 3: leadTerm routes a demotion to the recovery mode its cause dictates.
func TestLeadTerm_ClaimDemotionGoesToFollowing(t *testing.T) {
	coord, _ := newTestCoordinator(t, "primary")
	coord.pushRoleEvent(roleEvent{promoted: false, cause: causeClaim})
	if mode := coord.leadTerm(context.Background()); mode != modeFollowing {
		t.Errorf("claim demotion → got mode %d, want modeFollowing", mode)
	}
}

func TestLeadTerm_QuorumLossGoesToWaiting(t *testing.T) {
	coord, _ := newTestCoordinator(t, "primary")
	coord.pushRoleEvent(roleEvent{promoted: false, cause: causeQuorumLoss})
	if mode := coord.leadTerm(context.Background()); mode != modeWaitingQuorum {
		t.Errorf("quorum-loss demotion → got mode %d, want modeWaitingQuorum", mode)
	}
}

func TestLeadTerm_ExitsOnCtxCancel(t *testing.T) {
	coord, _ := newTestCoordinator(t, "primary")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if mode := coord.leadTerm(ctx); mode != modeLeading {
		t.Errorf("ctx cancel → got mode %d, want modeLeading (supervisor exits)", mode)
	}
}

// waitQuorumTerm defects to following the moment a foreign leader is known,
// instead of asserting leadership (the "no higher claim" guard).
func TestWaitQuorumTerm_DefectsToFollowingOnForeignLeader(t *testing.T) {
	coord, _ := newTestCoordinator(t, "primary")
	coord.election.mu.Lock()
	coord.election.state = ElectionFollower
	coord.election.currentLeader = "other-cortex"
	coord.election.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// Wake the loop immediately with a (non-promoted) event so it re-derives.
	coord.pushRoleEvent(roleEvent{promoted: false, cause: causeClaim})

	if mode := coord.waitQuorumTerm(ctx); mode != modeFollowing {
		t.Errorf("foreign leader present → got mode %d, want modeFollowing", mode)
	}
}

// waitQuorumTerm re-elects once a live voter quorum is present, transitioning to
// leading. Single-voter coordinator: aliveVoters (self) == quorum (1).
func TestWaitQuorumTerm_ReelectsWhenQuorumPresent(t *testing.T) {
	coord, _ := newTestCoordinator(t, "primary")
	coord.election.RegisterVoter(coord.cfg.NodeID) // quorum = 1, self alive

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	coord.pushRoleEvent(roleEvent{promoted: false, cause: causeQuorumLoss}) // wake immediately

	mode := coord.waitQuorumTerm(ctx)
	if mode != modeLeading {
		t.Fatalf("quorum present → got mode %d, want modeLeading (re-elected)", mode)
	}
	if !coord.IsLeader() {
		t.Error("expected to be leader after re-election")
	}
}
