package replication

import (
	"net"
	"testing"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// #531 PR1: ClearLeader forgets a dead leader and returns a follower to Idle so a
// fresh election can start.
func TestElection_ClearLeader(t *testing.T) {
	el := newTestElection(t, "self")
	el.mu.Lock()
	el.state = ElectionFollower
	el.currentLeader = "dead-cortex"
	el.mu.Unlock()

	el.ClearLeader("dead-cortex")

	if el.CurrentLeader() != "" {
		t.Errorf("currentLeader should be cleared, got %q", el.CurrentLeader())
	}
	if el.State() != ElectionIdle {
		t.Errorf("a follower whose leader died should return to Idle, got state %d", el.State())
	}

	// A different (still-live) leader must NOT be cleared.
	el.mu.Lock()
	el.state = ElectionFollower
	el.currentLeader = "other-cortex"
	el.mu.Unlock()
	el.ClearLeader("dead-cortex")
	if el.CurrentLeader() != "other-cortex" {
		t.Errorf("must not clear a different leader, got %q", el.CurrentLeader())
	}
}

// #533: only the current leader accepts joins — a non-leader rejects with a
// redirect to the leader it knows.
func TestHandleJoinRequest_NonLeaderRedirects(t *testing.T) {
	es := newTestEpochStore(t)
	if err := es.ForceSet(3); err != nil {
		t.Fatalf("ForceSet: %v", err)
	}
	mgr := NewConnManager("lobe-2")
	handler := NewJoinHandler("lobe-2", "", es, newTestRepLog(t), mgr)
	// This node is NOT the leader; it knows the real leader's address.
	handler.LeaderInfo = func() (bool, string, string) { return false, "cortex-1", "10.0.0.1:8479" }

	req := mbp.JoinRequest{NodeID: "lobe-1", Addr: "10.0.0.2:8479", ProtocolVersion: mbp.CurrentProtocolVersion}
	cortexConn, lobeConn := net.Pipe()
	t.Cleanup(func() { cortexConn.Close(); lobeConn.Close() })
	peer := mgr.RegisterConn(req.NodeID, req.Addr, cortexConn)

	resp := handler.HandleJoinRequest(req, peer)

	if resp.Accepted {
		t.Fatal("a non-leader must NOT accept a join")
	}
	if resp.CortexID != "cortex-1" || resp.CortexAddr != "10.0.0.1:8479" {
		t.Errorf("expected redirect to cortex-1/10.0.0.1:8479, got %q/%q", resp.CortexID, resp.CortexAddr)
	}
}

// A node that IS the leader accepts the join.
func TestHandleJoinRequest_LeaderAccepts(t *testing.T) {
	es := newTestEpochStore(t)
	if err := es.ForceSet(3); err != nil {
		t.Fatalf("ForceSet: %v", err)
	}
	mgr := NewConnManager("cortex-1")
	handler := NewJoinHandler("cortex-1", "", es, newTestRepLog(t), mgr)
	handler.LeaderInfo = func() (bool, string, string) { return true, "cortex-1", "10.0.0.1:8479" }

	req := mbp.JoinRequest{NodeID: "lobe-1", Addr: "10.0.0.2:8479", ProtocolVersion: mbp.CurrentProtocolVersion}
	cortexConn, lobeConn := net.Pipe()
	t.Cleanup(func() { cortexConn.Close(); lobeConn.Close() })
	peer := mgr.RegisterConn(req.NodeID, req.Addr, cortexConn)

	if resp := handler.HandleJoinRequest(req, peer); !resp.Accepted {
		t.Errorf("the leader must accept the join, got rejected: %s", resp.RejectReason)
	}
}
