package replication

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// #522 Step 2: HandleVoteResponse must count a vote only from a REGISTERED voter,
// so the numerator (votes) is drawn from the same population as the denominator
// (quorum). Otherwise a vote from an unknown/stale node could complete quorum and
// wrongly promote — a real hazard now that real-id reply paths actually flow.
func TestHandleVoteResponse_IgnoresUnregisteredVoter(t *testing.T) {
	el := newTestElection(t, "self") // harness registers self as a voter
	el.RegisterVoter("voter-1")      // voters {self, voter-1} → quorum 2

	if err := el.StartElection(context.Background()); err != nil {
		t.Fatalf("StartElection: %v", err)
	}
	if el.State() != ElectionCandidate {
		t.Fatalf("expected candidate (quorum 2, self-vote 1), got state %d", el.State())
	}

	var promoted int32
	el.OnPromoted = func(uint64) { atomic.AddInt32(&promoted, 1) }

	// A granted vote from a node that is NOT a registered voter must not count.
	el.HandleVoteResponse(mbp.VoteResponse{VoterID: "stranger", Epoch: 1, Granted: true})
	if el.State() != ElectionCandidate {
		t.Errorf("an unregistered vote must not reach quorum; state=%d", el.State())
	}
	if atomic.LoadInt32(&promoted) != 0 {
		t.Error("must not promote on an unregistered vote")
	}

	// A vote from the registered voter DOES complete quorum and promotes.
	el.HandleVoteResponse(mbp.VoteResponse{VoterID: "voter-1", Epoch: 1, Granted: true})
	if el.State() != ElectionLeader {
		t.Errorf("registered voter should complete quorum and promote, got state %d", el.State())
	}
}
