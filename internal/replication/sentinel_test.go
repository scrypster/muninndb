package replication

import (
	"errors"
	"sync/atomic"
	"testing"

	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/vfs"
	"github.com/vmihailenco/msgpack/v5"

	"github.com/scrypster/muninndb/internal/config"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// newSentinelCoordinator creates a ClusterCoordinator configured as a Sentinel.
func newSentinelCoordinator(t *testing.T) *ClusterCoordinator {
	t.Helper()
	db, err := pebble.Open("", &pebble.Options{FS: vfs.NewMem()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	repLog := NewReplicationLog(db)
	applier := NewApplier(db)
	epochStore, err := NewEpochStore(db)
	if err != nil {
		t.Fatal(err)
	}

	cfg := &config.ClusterConfig{
		Enabled:     true,
		NodeID:      "sentinel-node",
		BindAddr:    "127.0.0.1:9100",
		Seeds:       []string{},
		Role:        "sentinel",
		LeaseTTL:    10,
		HeartbeatMS: 1000,
	}

	coord := NewClusterCoordinator(cfg, repLog, applier, epochStore)
	// Manually apply sentinel role (normally done by runAsSentinel during Run).
	coord.roleMu.Lock()
	coord.role = RoleSentinel
	coord.roleMu.Unlock()
	coord.election.SetSentinel(true)

	return coord
}

// TestSentinel_NotEligibleForCortex verifies that:
// 1. A Sentinel's election.StartElection returns an error (cannot initiate).
// 2. The Sentinel DOES grant a vote when a VoteRequest arrives (can vote).
// 3. The Sentinel is never promoted even if HandleVoteResponse is called.
func TestSentinel_NotEligibleForCortex(t *testing.T) {
	coord := newSentinelCoordinator(t)

	// Register some voters so quorum > 1 and the election machinery is active.
	coord.election.RegisterVoter("sentinel-node")
	coord.election.RegisterVoter("cortex-node")
	coord.election.RegisterVoter("lobe-node")

	// Track promotion.
	var promotedCalls int32
	coord.election.OnPromoted = func(epoch uint64) {
		atomic.AddInt32(&promotedCalls, 1)
	}

	// --- Sentinel must NOT start an election ---
	err := coord.election.StartElection(nil)
	if err == nil {
		t.Fatal("expected error when Sentinel calls StartElection, got nil")
	}
	if !errors.Is(err, errSentinelCannotElect) {
		t.Errorf("expected errSentinelCannotElect, got: %v", err)
	}

	if coord.election.State() != ElectionIdle {
		t.Errorf("sentinel election state should remain Idle, got %v", coord.election.State())
	}

	// --- Sentinel MUST grant a vote when asked ---
	req := mbp.VoteRequest{
		CandidateID: "cortex-node",
		Epoch:       1,
	}
	resp := coord.election.HandleVoteRequest(req)
	if !resp.Granted {
		t.Error("sentinel should grant vote to a valid candidate")
	}
	if resp.VoterID != "sentinel-node" {
		t.Errorf("VoterID = %q, want %q", resp.VoterID, "sentinel-node")
	}

	// --- Sentinel must NOT be promoted even if HandleVoteResponse is called ---
	// Put election in candidate state manually — this should not happen in practice
	// but we defensively guard tryPromote.
	coord.election.mu.Lock()
	coord.election.state = ElectionCandidate
	coord.election.candidateEpoch = 1
	coord.election.votes[1] = map[string]bool{"sentinel-node": true}
	coord.election.mu.Unlock()

	coord.election.HandleVoteResponse(mbp.VoteResponse{
		VoterID: "cortex-node",
		Epoch:   1,
		Granted: true,
	})

	if atomic.LoadInt32(&promotedCalls) != 0 {
		t.Error("sentinel OnPromoted should never be called")
	}
}

// TestSentinel_ProvidesQuorum verifies that a Sentinel's vote can satisfy quorum
// for a Lobe candidate. Cluster: cortex-a (down), lobe-b (candidate), sentinel-c (voter).
// Quorum = 3/2 + 1 = 2. lobe-b needs 2 votes: self + sentinel-c.
func TestSentinel_ProvidesQuorum(t *testing.T) {
	// Set up the lobe-b election, with 3 registered voters.
	db, err := pebble.Open("", &pebble.Options{FS: vfs.NewMem()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	es, err := NewEpochStore(db)
	if err != nil {
		t.Fatal(err)
	}
	mgr := NewConnManager("lobe-b")
	t.Cleanup(func() { mgr.Close() })

	el := NewElection("lobe-b", es, mgr)
	// Three voters: cortex-a (was Cortex), lobe-b (candidate), sentinel-c.
	el.RegisterVoter("cortex-a")
	el.RegisterVoter("lobe-b")
	el.RegisterVoter("sentinel-c")
	// quorum = 3/2 + 1 = 2

	var promotedEpoch uint64
	el.OnPromoted = func(epoch uint64) {
		atomic.StoreUint64(&promotedEpoch, epoch)
	}

	// lobe-b starts election.
	if err := el.StartElection(nil); err != nil {
		t.Fatalf("lobe-b StartElection: %v", err)
	}

	if el.State() != ElectionCandidate {
		t.Fatalf("expected ElectionCandidate, got %v", el.State())
	}

	// cortex-a is down — no vote from it.
	// sentinel-c grants its vote (lobe-b already has self-vote).
	el.HandleVoteResponse(mbp.VoteResponse{
		VoterID: "sentinel-c",
		Epoch:   1,
		Granted: true,
	})

	if el.State() != ElectionLeader {
		t.Fatalf("lobe-b should win with sentinel quorum, state = %v", el.State())
	}
	if atomic.LoadUint64(&promotedEpoch) != 1 {
		t.Errorf("expected promoted epoch=1, got %d", atomic.LoadUint64(&promotedEpoch))
	}
	if el.CurrentLeader() != "lobe-b" {
		t.Errorf("CurrentLeader = %q, want %q", el.CurrentLeader(), "lobe-b")
	}
}

// TestSentinel_NoDataStorage verifies that a Sentinel coordinator never applies
// replication entries — HandleIncomingFrame with TypeReplEntry returns nil without
// calling Applier.Apply.
func TestSentinel_NoDataStorage(t *testing.T) {
	coord := newSentinelCoordinator(t)

	// Record the lastApplied before sending a replication entry.
	beforeApplied := coord.applier.LastApplied()

	// Build a valid ReplEntry payload.
	entry := mbp.ReplEntry{
		Seq:         1,
		Op:          uint8(OpSet),
		Key:         []byte("sentinel-key"),
		Value:       []byte("sentinel-value"),
		TimestampNS: 999,
	}
	payload, err := msgpack.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}

	// Dispatch — sentinel should silently discard.
	if err := coord.HandleIncomingFrame("cortex-1", mbp.TypeReplEntry, payload); err != nil {
		t.Fatalf("HandleIncomingFrame ReplEntry on sentinel: %v", err)
	}

	// Applier.LastApplied must NOT have changed.
	afterApplied := coord.applier.LastApplied()
	if afterApplied != beforeApplied {
		t.Errorf("sentinel must not apply replication entries: lastApplied before=%d after=%d",
			beforeApplied, afterApplied)
	}
}
