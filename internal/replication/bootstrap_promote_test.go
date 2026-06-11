package replication

import "testing"

// #516 part 1: a node explicitly configured role=primary never formally promotes
// when its seeds are registered as voters but are not up yet to vote — self-vote
// alone is short of quorum, so it stays a candidate (role "unknown") even though
// it serves joins and streams. bootstrapPromoteIfDesignatedPrimary asserts
// leadership in that case.
func TestBootstrapPromote_DesignatedPrimaryPromotes(t *testing.T) {
	coord, _ := newTestCoordinator(t, "primary")

	// Simulate StartElection having bumped the epoch and self-voted, but not
	// reached quorum (seeds not up): still a candidate for epoch 1.
	coord.election.mu.Lock()
	coord.election.state = ElectionCandidate
	coord.election.candidateEpoch = 1
	coord.election.mu.Unlock()

	coord.bootstrapPromoteIfDesignatedPrimary()

	if coord.Role() != RolePrimary {
		t.Errorf("designated primary did not promote: role=%v", coord.Role())
	}
	if !coord.IsLeader() {
		t.Error("expected IsLeader() == true after bootstrap promotion")
	}
}

// An auto-role node must NOT force-promote — it goes through the normal election.
func TestBootstrapPromote_AutoRoleDoesNotForcePromote(t *testing.T) {
	coord, _ := newTestCoordinator(t, "")

	coord.election.mu.Lock()
	coord.election.state = ElectionCandidate
	coord.election.candidateEpoch = 1
	coord.election.mu.Unlock()

	coord.bootstrapPromoteIfDesignatedPrimary()

	if coord.Role() == RolePrimary {
		t.Error("auto-role node should not bootstrap-promote")
	}
}

// If another leader already claimed the epoch (we are no longer a candidate),
// the designated primary must not override it.
func TestBootstrapPromote_DoesNotOverrideExistingLeader(t *testing.T) {
	coord, _ := newTestCoordinator(t, "primary")

	coord.election.mu.Lock()
	coord.election.state = ElectionFollower // a CortexClaim demoted us
	coord.election.mu.Unlock()

	coord.bootstrapPromoteIfDesignatedPrimary()

	if coord.Role() == RolePrimary {
		t.Error("must not promote when no longer a candidate (another leader exists)")
	}
}
