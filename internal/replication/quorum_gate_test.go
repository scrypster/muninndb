package replication

import (
	"testing"
	"time"
)

// #520 / #522 Step 3: a leader that has NEVER held a live quorum this term (a
// designated primary still awaiting its lobes at bootstrap) must not be demoted
// by the periodic quorum check — otherwise it would tear down a healthy bootstrap.
func TestCheckQuorumHealth_NeverHadQuorum_DoesNotDemote(t *testing.T) {
	coord, _ := newTestCoordinator(t, "auto")
	simulatePromotion(coord, 1)

	// quorum=2 (self + peer-1), peer-1 SDOWN from the start — never a live quorum.
	coord.election.RegisterVoter(coord.cfg.NodeID)
	coord.election.RegisterVoter("peer-1")
	coord.msp.AddPeer("peer-1", "127.0.0.1:9031", RoleReplica)
	coord.msp.mu.Lock()
	coord.msp.peers["peer-1"].SDown = true
	coord.msp.mu.Unlock()

	coord.checkQuorumHealth()
	coord.quorumMu.Lock()
	coord.quorumLostSince = time.Now().Add(-6 * time.Second)
	coord.quorumMu.Unlock()
	coord.checkQuorumHealth()

	time.Sleep(50 * time.Millisecond)
	if !coord.IsLeader() {
		t.Error("must NOT demote a leader that never held a live quorum (bootstrap exemption)")
	}
}

// aliveVoters counts only registered voters: a live observer must not keep a
// leader that has lost its voter quorum from demoting.
func TestCheckQuorumHealth_CountsVotersNotObservers(t *testing.T) {
	coord, _ := newTestCoordinator(t, "auto")
	simulatePromotion(coord, 1)

	coord.election.RegisterVoter(coord.cfg.NodeID)
	coord.election.RegisterVoter("voter-1")
	coord.msp.AddPeer("voter-1", "127.0.0.1:9032", RoleReplica)
	// An observer peer that is alive but NOT a registered voter.
	coord.msp.AddPeer("obs-1", "127.0.0.1:9033", RoleObserver)

	// voter-1 alive → live quorum observed (hadQuorum latches).
	coord.checkQuorumHealth()
	coord.quorumMu.Lock()
	had := coord.hadQuorum
	coord.quorumMu.Unlock()
	if !had {
		t.Fatal("expected hadQuorum after a live voter quorum")
	}

	// voter-1 SDOWN; obs-1 stays alive but must not count toward quorum.
	coord.msp.mu.Lock()
	coord.msp.peers["voter-1"].SDown = true
	coord.msp.mu.Unlock()

	coord.checkQuorumHealth()
	coord.quorumMu.Lock()
	coord.quorumLostSince = time.Now().Add(-6 * time.Second)
	coord.quorumMu.Unlock()
	coord.checkQuorumHealth()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !coord.IsLeader() {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if coord.IsLeader() {
		t.Error("expected demotion: a live observer must not count toward voter quorum")
	}
}
