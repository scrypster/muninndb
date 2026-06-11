package replication

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// #519 / #522 Step 4: when two nodes both assert leadership at the same epoch
// (two misconfigured role=primary), the lower node-id keeps leadership and the
// higher one yields — instead of both mutually demoting.
func TestHandleCortexClaim_EqualEpoch_LowerIDKeepsLeadership(t *testing.T) {
	el := newTestElection(t, "node-a") // lower id
	if err := el.StartElection(context.Background()); err != nil {
		t.Fatalf("StartElection: %v", err)
	}
	if el.State() != ElectionLeader {
		t.Fatalf("expected leader at epoch 1, got %d", el.State())
	}

	var demoted int32
	el.OnDemoted = func() { atomic.AddInt32(&demoted, 1) }

	// A higher node-id claims leadership at the SAME epoch.
	el.HandleCortexClaim(mbp.CortexClaim{CortexID: "node-z", Epoch: 1})

	if el.State() != ElectionLeader {
		t.Errorf("lower node-id must keep leadership on equal-epoch conflict, got state %d", el.State())
	}
	if el.CurrentLeader() != "node-a" {
		t.Errorf("CurrentLeader = %q, want node-a", el.CurrentLeader())
	}
	if atomic.LoadInt32(&demoted) != 0 {
		t.Errorf("must not demote; OnDemoted called %d times", atomic.LoadInt32(&demoted))
	}
}

func TestHandleCortexClaim_EqualEpoch_HigherIDYields(t *testing.T) {
	el := newTestElection(t, "node-z") // higher id
	if err := el.StartElection(context.Background()); err != nil {
		t.Fatalf("StartElection: %v", err)
	}

	var demoted int32
	el.OnDemoted = func() { atomic.AddInt32(&demoted, 1) }

	// A lower node-id claims leadership at the SAME epoch.
	el.HandleCortexClaim(mbp.CortexClaim{CortexID: "node-a", Epoch: 1})

	if el.State() != ElectionFollower {
		t.Errorf("higher node-id must yield on equal-epoch conflict, got state %d", el.State())
	}
	if el.CurrentLeader() != "node-a" {
		t.Errorf("CurrentLeader = %q, want node-a", el.CurrentLeader())
	}
	if atomic.LoadInt32(&demoted) != 1 {
		t.Errorf("must demote once; OnDemoted called %d times", atomic.LoadInt32(&demoted))
	}
}

// A higher-epoch claim always wins regardless of node-id.
func TestHandleCortexClaim_HigherEpochWins(t *testing.T) {
	el := newTestElection(t, "node-a")
	if err := el.StartElection(context.Background()); err != nil {
		t.Fatalf("StartElection: %v", err)
	}
	el.HandleCortexClaim(mbp.CortexClaim{CortexID: "node-z", Epoch: 5})
	if el.State() != ElectionFollower {
		t.Errorf("a higher-epoch claim must win even from a higher node-id, got state %d", el.State())
	}
}
