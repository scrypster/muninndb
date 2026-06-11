package replication

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/vmihailenco/msgpack/v5"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// ElectionState tracks what this node knows about the current election.
type ElectionState uint8

const (
	ElectionIdle      ElectionState = 0
	ElectionCandidate ElectionState = 1
	ElectionLeader    ElectionState = 2 // this node is Cortex
	ElectionFollower  ElectionState = 3 // another node is Cortex
)

// Election manages the MSP leader election protocol.
//
// Safety invariant: at most one Cortex may hold a given epoch. This is
// guaranteed by (1) monotonically increasing epoch numbers persisted via
// EpochStore, (2) strict majority quorum requirement, and (3) the pigeonhole
// principle -- two candidates cannot simultaneously get >N/2 votes from N voters.
type Election struct {
	localNodeID string
	epochStore  *EpochStore
	mgr         *ConnManager

	// isSentinel marks this node as a Sentinel: it may grant votes but never
	// starts elections or claims Cortex status.
	isSentinel bool

	// isObserver marks this node as an Observer. Observers never grant votes
	// and are not counted toward election quorum.
	isObserver bool

	state         ElectionState
	currentLeader string // empty if unknown

	// votes tracks received votes: epoch -> voterID -> granted.
	votes map[uint64]map[string]bool

	// votedFor tracks this node's votes: epoch -> candidateID.
	// Invariant: once votedFor[epoch] is set, it is immutable for that epoch.
	// This ensures at most one vote is granted per epoch.
	votedFor map[uint64]string

	// candidateEpoch is the epoch for which this node is currently a candidate.
	// Only valid when state == ElectionCandidate.
	candidateEpoch uint64

	mu sync.Mutex

	// Callbacks -- nil-checked before invocation. Called without mu held.
	OnPromoted  func(epoch uint64)                  // this node became Cortex
	OnDemoted   func()                              // this node lost Cortex status
	OnNewLeader func(leaderID string, epoch uint64) // another node became Cortex

	// voters tracks which nodeIDs are eligible to vote (Cortex, Lobe, Sentinel).
	voters map[string]struct{}
}

var (
	errAlreadyCandidate    = errors.New("election: already a candidate or leader")
	errEpochCASFailed      = errors.New("election: epoch compare-and-set failed (concurrent election)")
	errSentinelCannotElect = errors.New("election: sentinel nodes cannot initiate elections")
	errObserverCannotElect = errors.New("election: observer nodes cannot initiate elections")
)

// NewElection creates an Election for the given local node.
func NewElection(localNodeID string, epochStore *EpochStore, mgr *ConnManager) *Election {
	return &Election{
		localNodeID: localNodeID,
		epochStore:  epochStore,
		mgr:         mgr,
		state:       ElectionIdle,
		votes:       make(map[uint64]map[string]bool),
		votedFor:    make(map[uint64]string),
		voters:      make(map[string]struct{}),
	}
}

// SetSentinel marks this Election as belonging to a Sentinel node. Sentinels
// grant votes to candidates but never initiate elections or claim Cortex status.
func (e *Election) SetSentinel(sentinel bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.isSentinel = sentinel
}

// SetObserver marks this Election as belonging to an Observer node. Observers
// never grant votes and are not counted toward election quorum.
func (e *Election) SetObserver(observer bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.isObserver = observer
}

// RegisterVoter marks nodeID as eligible to vote (Cortex, Lobe, or Sentinel).
func (e *Election) RegisterVoter(nodeID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.voters[nodeID] = struct{}{}
}

// UnregisterVoter removes a voter from the registry.
func (e *Election) UnregisterVoter(nodeID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.voters, nodeID)
}

// IsVoter reports whether nodeID is a registered voter.
func (e *Election) IsVoter(nodeID string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, ok := e.voters[nodeID]
	return ok
}

// StepDown relinquishes leadership without a successor (used by the pre-emptive
// quorum-loss demotion, which has no claimant). state→Idle so a later
// StartElection is not blocked by errAlreadyCandidate; currentLeader is cleared
// only if it still points at us — a concurrent HandleCortexClaim that already
// installed another leader wins (#522 Step 3).
func (e *Election) StepDown() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.state == ElectionLeader {
		e.state = ElectionIdle
	}
	if e.currentLeader == e.localNodeID {
		e.currentLeader = ""
	}
}

// StartElection initiates a new election from this node.
// Returns an error if this node is already a candidate or leader.
//
// Steps:
//  1. Increment epoch via epochStore.CompareAndSet
//  2. Set state = ElectionCandidate
//  3. Vote for self
//  4. Broadcast VoteRequest to all peers
//  5. HandleVoteResponse processes incoming votes asynchronously
func (e *Election) StartElection(ctx context.Context) error {
	e.mu.Lock()

	// Sentinels provide quorum votes but never initiate elections.
	if e.isSentinel {
		e.mu.Unlock()
		return errSentinelCannotElect
	}

	// Observers receive replication but never initiate elections.
	if e.isObserver {
		e.mu.Unlock()
		return errObserverCannotElect
	}

	if e.state == ElectionCandidate || e.state == ElectionLeader {
		e.mu.Unlock()
		return errAlreadyCandidate
	}

	// Step 1: Increment epoch atomically.
	currentEpoch := e.epochStore.Load()
	newEpoch := currentEpoch + 1

	ok, err := e.epochStore.CompareAndSet(currentEpoch, newEpoch)
	if err != nil {
		e.mu.Unlock()
		return err
	}
	if !ok {
		e.mu.Unlock()
		return errEpochCASFailed
	}

	// Step 2: Transition to candidate state.
	e.state = ElectionCandidate
	e.candidateEpoch = newEpoch

	// Step 3: Vote for self.
	e.votedFor[newEpoch] = e.localNodeID
	if e.votes[newEpoch] == nil {
		e.votes[newEpoch] = make(map[string]bool)
	}
	e.votes[newEpoch][e.localNodeID] = true

	// Check if self-vote alone reaches quorum (single-voter cluster).
	quorum := e.quorumLocked()
	voteCount := len(e.votes[newEpoch])

	e.mu.Unlock()

	// Step 4: Broadcast VoteRequest to all peers.
	req := mbp.VoteRequest{
		CandidateID: e.localNodeID,
		Epoch:       newEpoch,
	}
	payload, err := msgpack.Marshal(req)
	if err != nil {
		return err
	}
	e.mgr.Broadcast(mbp.TypeVoteRequest, payload)

	// If self-vote alone is enough (single-node quorum), promote immediately.
	if voteCount >= quorum {
		e.tryPromote(newEpoch)
	}

	return nil
}

// HandleVoteRequest processes an incoming VoteRequest from another candidate.
//
// Grant the vote if and only if:
//   - The candidate's epoch >= our current epoch (epoch monotonicity).
//   - We have not already voted for a different candidate in this epoch.
//
// If the request epoch is higher than ours, we update our epoch store
// (ForceSet) to track the latest epoch we've seen.
func (e *Election) HandleVoteRequest(req mbp.VoteRequest) mbp.VoteResponse {
	e.mu.Lock()
	defer e.mu.Unlock()

	// Observers never grant votes.
	if e.isObserver {
		return mbp.VoteResponse{
			VoterID: e.localNodeID,
			Epoch:   req.Epoch,
			Granted: false,
		}
	}

	currentEpoch := e.epochStore.Load()

	// Reject stale requests: candidate's epoch must be >= our current epoch.
	if req.Epoch < currentEpoch {
		return mbp.VoteResponse{
			VoterID: e.localNodeID,
			Epoch:   req.Epoch,
			Granted: false,
		}
	}

	// If the request epoch is higher, advance our epoch.
	if req.Epoch > currentEpoch {
		_ = e.epochStore.ForceSet(req.Epoch)
	}

	// Check if we already voted in this epoch.
	if prev, ok := e.votedFor[req.Epoch]; ok {
		// Already voted -- grant only if it was for the same candidate (idempotent).
		granted := prev == req.CandidateID
		return mbp.VoteResponse{
			VoterID: e.localNodeID,
			Epoch:   req.Epoch,
			Granted: granted,
		}
	}

	// Grant vote and record it.
	e.votedFor[req.Epoch] = req.CandidateID

	// If we were a candidate for a different (older) epoch, step down.
	if e.state == ElectionCandidate && e.candidateEpoch < req.Epoch {
		e.state = ElectionIdle
	}

	return mbp.VoteResponse{
		VoterID: e.localNodeID,
		Epoch:   req.Epoch,
		Granted: true,
	}
}

// HandleVoteResponse processes an incoming VoteResponse.
//
// If we collect strict majority votes in our candidate epoch, we become Cortex:
//  1. CompareAndSet epoch to confirm we still own it (belt-and-suspenders)
//  2. Broadcast CortexClaim to all peers
//  3. Set state = ElectionLeader
//  4. Call OnPromoted(epoch)
func (e *Election) HandleVoteResponse(resp mbp.VoteResponse) {
	e.mu.Lock()

	// Only process votes if we are currently a candidate for this epoch.
	if e.state != ElectionCandidate || resp.Epoch != e.candidateEpoch {
		e.mu.Unlock()
		return
	}

	if !resp.Granted {
		e.mu.Unlock()
		return
	}

	// Count votes only from registered voters, so the vote tally (numerator) is
	// drawn from the same population as the quorum (denominator). A vote from an
	// unknown/stale node must never help complete quorum (#522 Step 2).
	if _, ok := e.voters[resp.VoterID]; !ok {
		e.mu.Unlock()
		return
	}

	// Record the vote.
	if e.votes[resp.Epoch] == nil {
		e.votes[resp.Epoch] = make(map[string]bool)
	}
	e.votes[resp.Epoch][resp.VoterID] = true

	quorum := e.quorumLocked()
	voteCount := len(e.votes[resp.Epoch])

	e.mu.Unlock()

	if voteCount >= quorum {
		e.tryPromote(resp.Epoch)
	}
}

// tryPromote attempts to transition from candidate to leader for the given epoch.
// It confirms the epoch via CAS before broadcasting the claim.
// Sentinels are never eligible for promotion.
func (e *Election) tryPromote(epoch uint64) {
	e.mu.Lock()

	// Sentinels are never eligible to become Cortex.
	if e.isSentinel {
		e.mu.Unlock()
		return
	}

	// Double-check we are still a candidate for this epoch.
	if e.state != ElectionCandidate || e.candidateEpoch != epoch {
		e.mu.Unlock()
		return
	}

	// Belt-and-suspenders: confirm we own this epoch via CAS.
	// We CAS from epoch to epoch (no-op if we already set it). This catches
	// the impossible-but-guarded case of a concurrent epoch bump.
	ok, err := e.epochStore.CompareAndSet(epoch, epoch)
	if err != nil || !ok {
		// Another node bumped the epoch -- we lost the race.
		e.state = ElectionIdle
		e.mu.Unlock()
		return
	}

	// Promote to leader.
	e.state = ElectionLeader
	e.currentLeader = e.localNodeID
	onPromoted := e.OnPromoted

	// Clean up old election state for epochs < epoch
	for ep := range e.votes {
		if ep < epoch {
			delete(e.votes, ep)
		}
	}
	for ep := range e.votedFor {
		if ep < epoch {
			delete(e.votedFor, ep)
		}
	}

	e.mu.Unlock()

	// Broadcast CortexClaim to all peers.
	e.broadcastClaim(epoch)

	// Invoke callback without lock held.
	if onPromoted != nil {
		onPromoted(epoch)
	}
}

// broadcastClaim marshals and broadcasts a CortexClaim for the given epoch to
// all connected peers. Factored out so the equal-epoch tie-break can re-assert.
func (e *Election) broadcastClaim(epoch uint64) {
	claim := mbp.CortexClaim{
		CortexID:     e.localNodeID,
		Epoch:        epoch,
		FencingToken: epoch, // epoch is the fencing token
	}
	payload, err := msgpack.Marshal(claim)
	if err != nil {
		slog.Error("election: failed to marshal CortexClaim", "err", err)
		return
	}
	e.mgr.Broadcast(mbp.TypeCortexClaim, payload)
}

// HandleCortexClaim processes an incoming CortexClaim from another node.
//
// Accept the claim if claim.Epoch >= our current epoch:
//  1. Update our epoch store via ForceSet
//  2. Set currentLeader = claim.CortexID
//  3. Set state = ElectionFollower
//  4. If we were leader (split scenario), call OnDemoted
//  5. Call OnNewLeader(claim.CortexID, claim.Epoch)
func (e *Election) HandleCortexClaim(claim mbp.CortexClaim) {
	e.mu.Lock()

	currentEpoch := e.epochStore.Load()

	// Reject stale claims.
	if claim.Epoch < currentEpoch {
		e.mu.Unlock()
		return
	}

	// Equal-epoch dueling-leaders tie-break (#519, #522 Step 4): two nodes both
	// asserted leadership at the same epoch — only reachable when more than one is
	// misconfigured role=primary (each force-promotes with a single self-vote).
	// Resolve deterministically by lowest node-id: the lower id keeps leadership
	// (ignores the conflicting claim and re-asserts), the higher id falls through
	// to the demotion path below. Without this, two equal-epoch leaders mutually
	// demote and the cluster can end up leaderless or flapping.
	if claim.Epoch == currentEpoch && e.state == ElectionLeader &&
		claim.CortexID != e.localNodeID && e.localNodeID < claim.CortexID {
		e.mu.Unlock()
		slog.Warn("cluster: equal-epoch CortexClaim from a higher node-id; keeping leadership (lowest id wins), re-asserting. Ensure exactly one node is configured role=primary.",
			"claimant", claim.CortexID, "epoch", claim.Epoch, "self", e.localNodeID)
		e.broadcastClaim(claim.Epoch)
		return
	}

	// Update epoch store to track the claim's epoch.
	_ = e.epochStore.ForceSet(claim.Epoch)

	wasLeader := e.state == ElectionLeader
	e.state = ElectionFollower
	e.currentLeader = claim.CortexID

	onDemoted := e.OnDemoted
	onNewLeader := e.OnNewLeader

	// Clean up old election state for epochs < claim.Epoch
	for ep := range e.votes {
		if ep < claim.Epoch {
			delete(e.votes, ep)
		}
	}
	for ep := range e.votedFor {
		if ep < claim.Epoch {
			delete(e.votedFor, ep)
		}
	}

	e.mu.Unlock()

	// Invoke callbacks without lock held.
	if wasLeader && onDemoted != nil {
		onDemoted()
	}
	if onNewLeader != nil {
		onNewLeader(claim.CortexID, claim.Epoch)
	}
}

// State returns the current ElectionState (thread-safe).
func (e *Election) State() ElectionState {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.state
}

// CurrentLeader returns the current leader's nodeID, or empty if unknown.
func (e *Election) CurrentLeader() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.currentLeader
}

// CurrentEpoch returns the current epoch from the store.
func (e *Election) CurrentEpoch() uint64 {
	return e.epochStore.Load()
}

// Quorum returns the minimum number of votes needed for a majority.
// Quorum = len(voters)/2 + 1
func (e *Election) Quorum() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.quorumLocked()
}

// quorumLocked returns the quorum count. Must be called with mu held.
func (e *Election) quorumLocked() int {
	return len(e.voters)/2 + 1
}
