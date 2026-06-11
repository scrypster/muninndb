package replication

import (
	"testing"
	"time"
)

// #528 / #522 Step 4c: a gossiped down-vote that completes quorum (with self also
// seeing the target SDOWN) fires OnODown exactly once.
func TestRecordDownVote_FiresODownAtQuorum(t *testing.T) {
	c, _ := newTestCoordinator(t, "auto")
	msp := c.msp
	msp.AddPeer("target", "t:1", RoleReplica)
	msp.AddPeer("voter-1", "v:1", RoleReplica)
	// non-observer population = self + target + voter-1 = 3 → quorum 2.

	msp.mu.Lock()
	msp.peers["target"].SDown = true // we also see the target as down
	msp.mu.Unlock()

	fired := make(chan string, 4)
	msp.OnODown = func(id string) { fired <- id }

	// self(1) + voter-1(1) = 2 ≥ quorum 2 → ODOWN.
	msp.RecordDownVote("voter-1", "target")
	select {
	case id := <-fired:
		if id != "target" {
			t.Errorf("ODOWN fired for %q, want target", id)
		}
	case <-time.After(time.Second):
		t.Fatal("expected OnODown to fire at quorum")
	}

	// Latched: a duplicate / repeat vote must not fire again.
	msp.RecordDownVote("voter-1", "target")
	msp.AddPeer("voter-2", "v2:1", RoleReplica)
	msp.RecordDownVote("voter-2", "target")
	select {
	case <-fired:
		t.Error("OnODown must fire only once per SDOWN episode")
	case <-time.After(100 * time.Millisecond):
	}
}

// An observer's gossiped down-vote must NOT count toward ODOWN: observers are
// excluded from the quorum denominator, so counting them could spuriously fail
// over a healthy Cortex (#522 Step 4c review).
func TestRecordDownVote_ObserverSenderDoesNotCount(t *testing.T) {
	c, _ := newTestCoordinator(t, "auto")
	msp := c.msp
	msp.AddPeer("target", "t:1", RoleReplica)
	msp.AddPeer("obs-1", "o:1", RoleObserver)
	// non-observer population = self + target = 2 → quorum 2.

	msp.mu.Lock()
	msp.peers["target"].SDown = true
	msp.mu.Unlock()

	fired := make(chan string, 1)
	msp.OnODown = func(id string) { fired <- id }

	msp.RecordDownVote("obs-1", "target") // observer vote — must be ignored
	select {
	case <-fired:
		t.Error("an observer sender's vote must not count toward the ODOWN quorum")
	case <-time.After(150 * time.Millisecond):
	}
}

// A down-vote must NOT trigger ODOWN if this node does not also see the target
// as SDOWN — ODOWN requires local agreement, not just remote reports.
func TestRecordDownVote_NoODownWithoutLocalSDown(t *testing.T) {
	c, _ := newTestCoordinator(t, "auto")
	msp := c.msp
	msp.AddPeer("target", "t:1", RoleReplica)
	msp.AddPeer("voter-1", "v:1", RoleReplica)
	// target is NOT marked SDOWN locally.

	fired := make(chan string, 1)
	msp.OnODown = func(id string) { fired <- id }

	msp.RecordDownVote("voter-1", "target")
	select {
	case <-fired:
		t.Error("OnODown must not fire when this node does not see the target as SDOWN")
	case <-time.After(150 * time.Millisecond):
	}
}
