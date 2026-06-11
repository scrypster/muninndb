package replication

import "testing"

// #522 Step 1: when a Lobe joins, the Cortex must replace the seed-<addr>
// placeholder (in MSP and the voter set) with the real joined node-id —
// CONSERVING the voter count (rename, never add) so quorum semantics don't shift,
// and so MSP heartbeats / votes keyed by the real id are no longer dropped.
func TestReconcileJoinedPeer_RenamesSeedConservingVoters(t *testing.T) {
	coord, _ := newTestCoordinator(t, "primary")
	coord.election.RegisterVoter(coord.cfg.NodeID) // self
	// Seed placeholder, as added at construction for a configured seed.
	coord.msp.AddPeer("seed-lobe1:8479", "lobe1:8479", RoleUnknown)
	coord.election.RegisterVoter("seed-lobe1:8479")

	quorumBefore := coord.election.Quorum() // voters {self, seed-lobe1} = 2 → 2

	coord.reconcileJoinedPeer("lobe1", "lobe1:8479", RoleReplica)

	if got := coord.election.Quorum(); got != quorumBefore {
		t.Errorf("voter count not conserved across join: quorum before=%d after=%d", quorumBefore, got)
	}

	var hasReal, hasSeed bool
	for _, p := range coord.msp.AllPeers() {
		switch p.NodeID {
		case "lobe1":
			hasReal = true
			if p.Role != RoleReplica {
				t.Errorf("reconciled peer role = %v, want RoleReplica", p.Role)
			}
		case "seed-lobe1:8479":
			hasSeed = true
		}
	}
	if !hasReal {
		t.Error("expected real node-id lobe1 in MSP after reconciliation")
	}
	if hasSeed {
		t.Error("seed-lobe1:8479 placeholder should be removed from MSP")
	}
}

// Reconciling the same node twice (e.g. a rejoin) is idempotent and does not
// inflate the voter set.
func TestReconcileJoinedPeer_Idempotent(t *testing.T) {
	coord, _ := newTestCoordinator(t, "primary")
	coord.election.RegisterVoter(coord.cfg.NodeID)
	coord.msp.AddPeer("seed-lobe1:8479", "lobe1:8479", RoleUnknown)
	coord.election.RegisterVoter("seed-lobe1:8479")

	coord.reconcileJoinedPeer("lobe1", "lobe1:8479", RoleReplica)
	q1 := coord.election.Quorum()
	coord.reconcileJoinedPeer("lobe1", "lobe1:8479", RoleReplica)
	q2 := coord.election.Quorum()

	if q1 != q2 {
		t.Errorf("reconcile not idempotent: quorum %d then %d", q1, q2)
	}
}
