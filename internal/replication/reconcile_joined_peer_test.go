package replication

import (
	"strings"
	"testing"
)

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

// Regression (PR #524 review): when the joined node's advertised address does
// NOT match the seed string (DNS seed vs pod IP / 0.0.0.0), the voter count must
// still be conserved — an arbitrary seed placeholder is retired so quorum cannot
// inflate.
func TestReconcileJoinedPeer_ConservesOnAddrMismatch(t *testing.T) {
	coord, _ := newTestCoordinator(t, "primary")
	coord.election.RegisterVoter(coord.cfg.NodeID)
	// Seed keyed by the DNS name the operator configured.
	coord.msp.AddPeer("seed-cortex.svc:8479", "cortex.svc:8479", RoleUnknown)
	coord.election.RegisterVoter("seed-cortex.svc:8479")

	quorumBefore := coord.election.Quorum() // {self, seed} = 2 → 2

	// Node joins advertising a routable IP that does NOT match the seed string.
	coord.reconcileJoinedPeer("lobe1", "10.0.0.5:8479", RoleReplica)

	if got := coord.election.Quorum(); got != quorumBefore {
		t.Errorf("quorum inflated on addr mismatch: before=%d after=%d", quorumBefore, got)
	}
	for _, p := range coord.msp.AllPeers() {
		if strings.HasPrefix(p.NodeID, "seed-") {
			t.Errorf("seed placeholder %q not retired on mismatch (would inflate quorum)", p.NodeID)
		}
	}
}

// A rejoin must not steal another not-yet-joined node's seed slot (deflation).
func TestReconcileJoinedPeer_RejoinDoesNotStealOtherSeed(t *testing.T) {
	coord, _ := newTestCoordinator(t, "primary")
	coord.election.RegisterVoter(coord.cfg.NodeID)
	coord.msp.AddPeer("seed-lobe1:8479", "lobe1:8479", RoleUnknown)
	coord.election.RegisterVoter("seed-lobe1:8479")
	coord.msp.AddPeer("seed-lobe2:8479", "lobe2:8479", RoleUnknown)
	coord.election.RegisterVoter("seed-lobe2:8479")

	coord.reconcileJoinedPeer("lobe1", "lobe1:8479", RoleReplica) // retires seed-lobe1
	qAfterJoin := coord.election.Quorum()

	coord.reconcileJoinedPeer("lobe1", "lobe1:8479", RoleReplica) // rejoin — must retire nothing
	if got := coord.election.Quorum(); got != qAfterJoin {
		t.Errorf("rejoin changed quorum (stole a seed slot?): %d -> %d", qAfterJoin, got)
	}
	// lobe2's seed must survive the lobe1 rejoin.
	var hasLobe2Seed bool
	for _, p := range coord.msp.AllPeers() {
		if p.NodeID == "seed-lobe2:8479" {
			hasLobe2Seed = true
		}
	}
	if !hasLobe2Seed {
		t.Error("lobe1 rejoin wrongly retired lobe2's seed slot")
	}
}
