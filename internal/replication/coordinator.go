package replication

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vmihailenco/msgpack/v5"

	"github.com/scrypster/muninndb/internal/cognitive"
	"github.com/scrypster/muninndb/internal/config"
	"github.com/scrypster/muninndb/internal/transport/mbp"
	"github.com/scrypster/muninndb/internal/wal"
)

// hebbianSubmitter is the interface the coordinator uses to submit co-activation
// events to the HebbianWorker. Defined as an interface for testability.
type hebbianSubmitter interface {
	Submit(item cognitive.CoActivationEvent) bool
}

// cognitiveFlushable is implemented by cognitive workers that support graceful
// shutdown with queue draining. Used during handoff to flush in-flight work.
type cognitiveFlushable interface {
	Stop()
}

// NodeState tracks the coordinator's current operational state.
type NodeState uint8

const (
	StateNormal   NodeState = 0
	StateDraining NodeState = 1
)

// ErrDraining is returned when a write is attempted while the node is draining.
var ErrDraining = errors.New("node is draining: not accepting new writes")

// ErrSelfRemoval is returned when RemoveNode is called with the local node's own ID.
var ErrSelfRemoval = errors.New("cannot remove self from cluster")

// defaultQuorumLossTimeout is the fallback if the config value is zero.
const defaultQuorumLossTimeout = 5 * time.Second

// ClusterCoordinator is the top-level cluster manager.
// It owns and orchestrates: ConnManager, MSP, Election, JoinHandler, JoinClient,
// ReplicationLog, Applier, EpochStore, and per-Lobe NetworkStreamers.
type ClusterCoordinator struct {
	cfg        *config.ClusterConfig
	repLog     *ReplicationLog
	applier    *Applier
	epochStore *EpochStore
	mgr        *ConnManager
	msp        *MSP
	election   *Election
	joinHandler  *JoinHandler
	joinClient   *JoinClient
	tokenManager *JoinTokenManager
	tls          *ClusterTLS

	role   NodeRole
	roleMu sync.RWMutex

	// Per-Lobe streamers (Cortex only): lobeID -> cancel func
	streamers   map[string]context.CancelFunc
	streamersMu sync.Mutex

	// quorumLostSince is the time when this Cortex first noticed it could not
	// reach a quorum of live peers. Zero value means quorum is healthy.
	quorumLostSince time.Time
	quorumMu        sync.Mutex

	// Cognitive workers (Cortex only): receive forwarded side effects from Lobes.
	hebbianWorker hebbianSubmitter

	// Flushable cognitive workers for graceful handoff. Set via SetCognitiveFlushers.
	hebbianFlusher cognitiveFlushable

	// cogForwardedTotal counts co-activations received via CogForward frames.
	cogForwardedTotal uint64

	// nodeState tracks whether the coordinator is in normal or draining mode.
	nodeState atomic.Uint32

	// ccsProbe measures Cognitive Consistency Score across cluster nodes.
	// Set via SetCCSProbe after the coordinator is created.
	ccsProbe *CCSProbe

	// mol is the Muninn Operation Log, used for periodic SafePrune.
	// Set via SetMOL after the coordinator is created.
	mol *wal.MOL

	// snapshotInProgress tracks how many snapshot transfers are active.
	// SafePrune is skipped while any snapshot is in progress to avoid
	// deleting WAL segments that the snapshot receiver still needs.
	snapshotInProgress atomic.Int32

	// reconciler runs post-partition cognitive reconciliation.
	// Set via SetReconciler after the coordinator is created.
	reconciler *Reconciler

	// reconDelay is how long to wait after a Lobe reconnects before triggering
	// reconciliation, allowing initial WAL catch-up. Defaults to 2s.
	reconDelay time.Duration

	// failoverMu serializes GracefulFailover calls (only one at a time).
	failoverMu sync.Mutex

	// replicaSeqs tracks the last ack'd seq per replica (nodeID -> seq).
	replicaSeqs sync.Map

	// handoffAckCh receives HANDOFF_ACK from the target during graceful failover.
	handoffAckCh chan mbp.HandoffAck
	handoffMu    sync.Mutex

	// started is set to true when Run() begins. Setters called after Run()
	// will panic to prevent unsynchronized field access.
	started atomic.Bool

	// Callbacks for the engine layer
	OnBecameCortex func(epoch uint64) // engine should start cognitive workers
	OnBecameLobe   func()             // engine should stop cognitive workers
}

// Coordinator is a type alias for backward compatibility.
type Coordinator = ClusterCoordinator

// NewClusterCoordinator creates a new ClusterCoordinator wired to all Phase 1 subsystems.
func NewClusterCoordinator(
	cfg *config.ClusterConfig,
	repLog *ReplicationLog,
	applier *Applier,
	epochStore *EpochStore,
) *ClusterCoordinator {
	mgr := NewConnManager(cfg.NodeID)
	msp := NewMSP(cfg.NodeID, cfg.BindAddr, mgr)
	election := NewElection(cfg.NodeID, epochStore, mgr)
	joinHandler := NewJoinHandler(cfg.NodeID, cfg.ClusterSecret, epochStore, repLog, mgr)
	joinClient := NewJoinClient(cfg.NodeID, cfg.BindAddr, cfg.ClusterSecret, epochStore, applier, mgr)

	reconDelay := time.Duration(cfg.ReconDelayMs) * time.Millisecond
	if reconDelay <= 0 {
		reconDelay = 2 * time.Second
	}

	c := &ClusterCoordinator{
		cfg:         cfg,
		repLog:      repLog,
		applier:     applier,
		epochStore:  epochStore,
		mgr:         mgr,
		msp:         msp,
		election:    election,
		joinHandler: joinHandler,
		joinClient:  joinClient,
		role:        RoleUnknown,
		streamers:   make(map[string]context.CancelFunc),
		reconDelay:  reconDelay,
	}

	// Wire token manager when a cluster secret is configured.
	if cfg.ClusterSecret != "" {
		tokenTTL := time.Duration(cfg.JoinTokenTTLMin) * time.Minute
		if tokenTTL <= 0 {
			tokenTTL = 15 * time.Minute
		}
		c.tokenManager = NewJoinTokenManager(cfg.ClusterSecret, tokenTTL)
	}

	// Wire election callbacks
	election.OnPromoted = func(epoch uint64) {
		c.handlePromotion(epoch)
	}
	election.OnDemoted = func() {
		c.handleDemotion()
	}
	election.OnNewLeader = func(leaderID string, epoch uint64) {
		c.handleNewLeader(leaderID, epoch)
	}

	// Wire MSP callbacks: trigger election when Cortex goes ODOWN.
	// Sentinels participate in ODOWN voting but never start elections themselves.
	msp.OnODown = func(nodeID string) {
		slog.Warn("cluster: ODOWN detected, triggering election", "down_node", nodeID)
		if c.IsSentinel() {
			slog.Info("cluster: sentinel node observed ODOWN, skipping election start", "down_node", nodeID)
			return
		}
		if err := c.election.StartElection(context.Background()); err != nil {
			slog.Error("cluster: failed to start election after ODOWN", "err", err)
		}
	}

	// Wire OnSDown to check quorum health — if we are Cortex and lose quorum
	// for 5s, pre-emptively demote to prevent split-brain writes.
	msp.OnSDown = func(nodeID string) {
		c.checkQuorumHealth()
	}

	// Wire OnRecover to restart the streamer when a peer recovers from SDOWN
	// and trigger cognitive reconciliation after initial WAL catch-up.
	msp.OnRecover = func(nodeID string) {
		if !c.IsLeader() {
			return // only restart streamer if we are Cortex
		}
		slog.Info("cluster: peer recovered from SDOWN, restarting streamer", "node", nodeID)
		peers := c.msp.AllPeers()
		for _, p := range peers {
			if p.NodeID == nodeID {
				c.startStreamerForLobe(NodeInfo{
					NodeID: p.NodeID,
					Addr:   p.Addr,
					Role:   p.Role,
				})
				if c.reconciler != nil {
					go func() {
						time.Sleep(c.reconDelay)
						if !c.IsLeader() {
							return
						}
						ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
						defer cancel()
						slog.Info("cluster: triggering post-reconnect reconciliation", "node", nodeID)
						result, err := c.TriggerReconciliation(ctx, []string{nodeID})
						if err != nil {
							slog.Warn("cluster: post-reconnect reconciliation failed",
								"node", nodeID, "err", err)
							return
						}
						slog.Info("cluster: post-reconnect reconciliation complete",
							"node", nodeID,
							"checked", result.EngramsChecked,
							"divergent", result.EngramsDivergent,
							"synced", result.WeightsSynced)
					}()
				}
				return
			}
		}
	}

	// Wire OnAddrChanged to update ConnManager when a peer moves to a new address
	// (e.g., pod restart with dynamic IP). The streamer will reconnect on next use.
	msp.OnAddrChanged = func(nodeID, newAddr string) {
		slog.Info("cluster: peer address changed, updating connection manager",
			"node", nodeID, "new_addr", newAddr)
		c.mgr.AddPeer(nodeID, newAddr)
	}

	// Wire join handler callbacks
	joinHandler.OnLobeJoined = func(info NodeInfo) {
		c.startStreamerForLobe(info)
	}
	joinHandler.OnLobeLeft = func(nodeID string) {
		c.stopStreamerForLobe(nodeID)
	}

	return c
}

// Run starts the coordinator. Blocks until ctx is done.
// If cfg.Role == "primary": bootstraps as Cortex (starts election if epoch==0)
// If cfg.Role == "replica": connects to seed, joins via JoinClient, starts receiving replication
// If cfg.Role == "sentinel": starts MSP only, participates in voting
// If cfg.Role == "observer": joins replication stream, applies locally, no voting
func (c *ClusterCoordinator) Run(ctx context.Context) error {
	c.started.Store(true)

	// Observers do not participate in voting — do not register as voter.
	if c.cfg.Role != "observer" {
		c.election.RegisterVoter(c.cfg.NodeID)
	} else {
		c.election.SetObserver(true)
	}

	// Add seed peers to ConnManager and MSP
	for _, seed := range c.cfg.Seeds {
		seedID := "seed-" + seed
		c.mgr.AddPeer(seedID, seed)
		c.msp.AddPeer(seedID, seed, RoleUnknown)
		// Observers do not register seed peers as voters either (they don't vote).
		if c.cfg.Role != "observer" {
			c.election.RegisterVoter(seedID)
		}
	}

	// Start MSP heartbeat in background
	heartbeat := time.Duration(c.cfg.HeartbeatMS) * time.Millisecond
	if heartbeat <= 0 {
		heartbeat = 1000 * time.Millisecond
	}
	mspCtx, mspCancel := context.WithCancel(ctx)
	defer mspCancel()

	// missedThreshold=3: SDOWN after 3 missed heartbeat intervals.
	const mspMissedThreshold = 3
	go func() {
		if err := c.msp.Run(mspCtx, heartbeat, mspMissedThreshold); err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("cluster: MSP exited with error", "err", err)
		}
	}()

	// Start periodic WAL pruning (only prunes on Cortex when replicas are caught up).
	c.startPeriodicPrune(ctx)

	switch c.cfg.Role {
	case "primary":
		return c.runAsCortex(ctx)
	case "replica":
		return c.runAsLobe(ctx)
	case "sentinel":
		return c.runAsSentinel(ctx)
	case "observer":
		return c.runAsObserver(ctx)
	default: // "auto" or unset
		return c.runAsCortex(ctx)
	}
}

// runAsCortex bootstraps as Cortex: starts election if epoch==0, then blocks.
// On restart after a crash mid-handoff, if the persisted role is "cortex" and
// the epoch is non-zero, the node promotes itself directly without a new election.
// This prevents a leaderless cluster when the old Cortex already demoted itself
// based on the HANDOFF_ACK that was sent before the crash.
func (c *ClusterCoordinator) runAsCortex(ctx context.Context) error {
	currentEpoch := c.epochStore.Load()

	// Check for a persisted role from a previous handoff promotion attempt.
	persistedRole, err := c.epochStore.LoadRole()
	if err != nil {
		slog.Warn("cluster: could not load persisted role, defaulting to election",
			"node", c.cfg.NodeID, "err", err)
	}

	if persistedRole == "cortex" && currentEpoch > 0 {
		// We crashed after persisting the role (and epoch) during HandleHandoff
		// but before completing in-memory promotion. Recover by promoting now.
		slog.Info("cluster: recovering Cortex role from persisted state after crash",
			"node", c.cfg.NodeID, "epoch", currentEpoch)
		// Set election state to Leader so the race-condition guard in handlePromotion
		// passes. The crash-recovery path bypasses the normal election vote sequence.
		c.election.mu.Lock()
		c.election.state = ElectionLeader
		c.election.currentLeader = c.cfg.NodeID
		c.election.mu.Unlock()
		c.handlePromotion(currentEpoch)
		<-ctx.Done()
		return ctx.Err()
	}

	if currentEpoch == 0 {
		slog.Info("cluster: epoch 0, bootstrapping election", "node", c.cfg.NodeID)
		if err := c.election.StartElection(ctx); err != nil {
			return fmt.Errorf("cluster: bootstrap election failed: %w", err)
		}
	}

	<-ctx.Done()
	return ctx.Err()
}

// runAsLobe connects to seed, joins, then blocks while receiving replication.
func (c *ClusterCoordinator) runAsLobe(ctx context.Context) error {
	c.roleMu.Lock()
	c.role = RoleReplica
	c.roleMu.Unlock()

	if len(c.cfg.Seeds) == 0 {
		return errors.New("cluster: lobe requires at least one seed address")
	}

	cortexAddr := c.cfg.Seeds[0]
	resp, err := c.joinClient.Join(ctx, cortexAddr)
	if err != nil {
		return fmt.Errorf("cluster: join failed: %w", err)
	}

	slog.Info("cluster: joined as lobe", "cortex", resp.CortexID, "epoch", resp.Epoch)

	<-ctx.Done()
	return ctx.Err()
}

// runAsSentinel starts MSP only, participates in voting, no data.
func (c *ClusterCoordinator) runAsSentinel(ctx context.Context) error {
	c.roleMu.Lock()
	c.role = RoleSentinel
	c.roleMu.Unlock()

	// Mark the election as sentinel: grants votes but never starts elections.
	c.election.SetSentinel(true)

	<-ctx.Done()
	return ctx.Err()
}

// runAsObserver joins the replication stream (like a Lobe), applies entries
// locally, but does not participate in voting or elections.
// Cognitive side effects received during Apply are silently discarded — the
// Observer has no workers and does not forward effects to the Cortex.
func (c *ClusterCoordinator) runAsObserver(ctx context.Context) error {
	c.roleMu.Lock()
	c.role = RoleObserver
	c.roleMu.Unlock()

	if len(c.cfg.Seeds) == 0 {
		return errors.New("cluster: observer requires at least one seed address")
	}

	cortexAddr := c.cfg.Seeds[0]
	resp, err := c.joinClient.Join(ctx, cortexAddr)
	if err != nil {
		return fmt.Errorf("cluster: observer join failed: %w", err)
	}

	slog.Info("cluster: joined as observer", "cortex", resp.CortexID, "epoch", resp.Epoch)

	<-ctx.Done()
	return ctx.Err()
}

// IsObserver returns true if this node is currently an Observer.
func (c *ClusterCoordinator) IsObserver() bool {
	return c.Role() == RoleObserver
}

// Role returns the current NodeRole (thread-safe).
func (c *ClusterCoordinator) Role() NodeRole {
	c.roleMu.RLock()
	defer c.roleMu.RUnlock()
	return c.role
}

// IsLeader returns true if this node is currently Cortex.
func (c *ClusterCoordinator) IsLeader() bool {
	return c.Role() == RolePrimary
}

// IsSentinel returns true if this node is running as a Sentinel (quorum voter only).
func (c *ClusterCoordinator) IsSentinel() bool {
	return c.Role() == RoleSentinel
}

// CurrentEpoch returns the current cluster epoch.
func (c *ClusterCoordinator) CurrentEpoch() uint64 {
	return c.epochStore.Load()
}

// KnownNodes returns a snapshot of all known nodes (from MSP peer states + members).
func (c *ClusterCoordinator) KnownNodes() []NodeInfo {
	peers := c.msp.AllPeers()
	nodes := make([]NodeInfo, 0, len(peers)+1)

	// Include self
	nodes = append(nodes, NodeInfo{
		NodeID: c.cfg.NodeID,
		Addr:   c.cfg.BindAddr,
		Role:   c.Role(),
	})

	for _, p := range peers {
		nodes = append(nodes, NodeInfo{
			NodeID: p.NodeID,
			Addr:   p.Addr,
			Role:   p.Role,
		})
	}
	return nodes
}

// ReplicationLag returns how far behind this Lobe is (0 if Cortex).
func (c *ClusterCoordinator) ReplicationLag() uint64 {
	if c.IsLeader() {
		return 0
	}
	cortexSeq := c.repLog.CurrentSeq()
	lastApplied := c.applier.LastApplied()
	if cortexSeq <= lastApplied {
		return 0
	}
	return cortexSeq - lastApplied
}

// CortexID returns the node ID of the current Cortex leader, or empty if unknown.
func (c *ClusterCoordinator) CortexID() string {
	return c.election.CurrentLeader()
}

// FencingToken returns the current fencing token (epoch-based).
func (c *ClusterCoordinator) FencingToken() uint64 {
	return c.epochStore.Load()
}

// ClusterMembers returns a snapshot of all known cluster members.
// Alias for KnownNodes for API compatibility.
func (c *ClusterCoordinator) ClusterMembers() []NodeInfo {
	return c.KnownNodes()
}

// checkQuorumHealth is called periodically (from the MSP tick goroutine via
// OnSDown callback) to detect sustained quorum loss. If the Cortex cannot
// reach a quorum of live peers for quorumLossTimeout (5s), it pre-emptively
// demotes itself to prevent split-brain writes.
func (c *ClusterCoordinator) checkQuorumHealth() {
	if !c.IsLeader() {
		return
	}

	livePeers := c.msp.LivePeers()
	quorum := c.election.Quorum()
	// Count: self (1) + live peers
	totalAlive := 1 + len(livePeers)

	c.quorumMu.Lock()

	if totalAlive >= quorum {
		// Quorum restored — reset timer
		c.quorumLostSince = time.Time{}
		c.quorumMu.Unlock()
		return
	}

	now := time.Now()
	if c.quorumLostSince.IsZero() {
		// First detection of quorum loss
		c.quorumLostSince = now
		slog.Warn("cluster: quorum lost, starting demotion timer",
			"alive", totalAlive, "quorum", quorum)
		c.quorumMu.Unlock()
		return
	}

	qTimeout := time.Duration(c.cfg.QuorumLossTimeoutSec) * time.Second
	if qTimeout <= 0 {
		qTimeout = defaultQuorumLossTimeout
	}
	needsDemotion := now.Sub(c.quorumLostSince) >= qTimeout
	if needsDemotion {
		slog.Error("cluster: quorum lost for >5s, pre-emptively demoting",
			"alive", totalAlive, "quorum", quorum)
		c.quorumLostSince = time.Time{} // reset before demotion
	}
	c.quorumMu.Unlock()

	if needsDemotion {
		c.nodeState.Store(uint32(StateDraining))
		go c.handleDemotion()
	}
}

// HandleIncomingFrame dispatches an incoming MBP frame from a peer to the right handler.
// Called by the TCP listener when a frame arrives.
func (c *ClusterCoordinator) HandleIncomingFrame(fromNodeID string, frameType uint8, payload []byte) error {
	switch frameType {
	case mbp.TypePing:
		c.msp.HandlePing(fromNodeID, payload)
		return nil

	case mbp.TypePong:
		c.msp.HandlePong(fromNodeID, payload)
		return nil

	case mbp.TypeVoteRequest:
		var req mbp.VoteRequest
		if err := msgpack.Unmarshal(payload, &req); err != nil {
			return fmt.Errorf("unmarshal VoteRequest: %w", err)
		}
		resp := c.election.HandleVoteRequest(req)
		respPayload, err := msgpack.Marshal(resp)
		if err != nil {
			return fmt.Errorf("marshal VoteResponse: %w", err)
		}
		peer, ok := c.mgr.GetPeer(fromNodeID)
		if ok {
			_ = peer.Send(mbp.TypeVoteResponse, respPayload)
		}
		return nil

	case mbp.TypeVoteResponse:
		var resp mbp.VoteResponse
		if err := msgpack.Unmarshal(payload, &resp); err != nil {
			return fmt.Errorf("unmarshal VoteResponse: %w", err)
		}
		c.election.HandleVoteResponse(resp)
		return nil

	case mbp.TypeCortexClaim:
		var claim mbp.CortexClaim
		if err := msgpack.Unmarshal(payload, &claim); err != nil {
			return fmt.Errorf("unmarshal CortexClaim: %w", err)
		}
		c.election.HandleCortexClaim(claim)
		return nil

	case mbp.TypeJoinRequest:
		var req mbp.JoinRequest
		if err := msgpack.Unmarshal(payload, &req); err != nil {
			return fmt.Errorf("unmarshal JoinRequest: %w", err)
		}
		peer, ok := c.mgr.GetPeer(fromNodeID)
		if !ok {
			// Create a peer for the joining node
			c.mgr.AddPeer(req.NodeID, req.Addr)
			peer, ok = c.mgr.GetPeer(req.NodeID)
			if !ok {
				return errors.New("failed to create peer for joining node")
			}
		}
		resp := c.joinHandler.HandleJoinRequest(req, peer)
		respPayload, err := msgpack.Marshal(resp)
		if err != nil {
			return fmt.Errorf("marshal JoinResponse: %w", err)
		}
		_ = peer.Send(mbp.TypeJoinResponse, respPayload)

		// Phase 2: stream snapshot immediately after JoinResponse on same conn.
		if resp.NeedsSnapshot {
			c.IncrementSnapshotCount()
			go func() {
				defer c.DecrementSnapshotCount()
				ctx := context.Background()
				if _, err := c.joinHandler.StreamSnapshot(ctx, peer); err != nil {
					slog.Error("cluster: snapshot stream failed; closing connection so lobe can reconnect and retry",
						"lobe", req.NodeID, "err", err)
					// Close the connection so the Lobe's blocking reads return an
					// error immediately. Without this, the Lobe waits forever for
					// a snapshot that was never completed.
					_ = peer.Close()
				}
			}()
		}
		return nil

	case mbp.TypeLeave:
		var msg mbp.LeaveMessage
		if err := msgpack.Unmarshal(payload, &msg); err != nil {
			return fmt.Errorf("unmarshal LeaveMessage: %w", err)
		}
		c.joinHandler.HandleLeave(msg)
		return nil

	case mbp.TypeReplEntry:
		// Sentinels do not store data — silently discard replication entries.
		if c.IsSentinel() {
			return nil
		}
		var entry mbp.ReplEntry
		if err := msgpack.Unmarshal(payload, &entry); err != nil {
			return fmt.Errorf("unmarshal ReplEntry: %w", err)
		}
		return c.applier.Apply(ReplicationEntry{
			Seq:         entry.Seq,
			Op:          WALOp(entry.Op),
			Key:         entry.Key,
			Value:       entry.Value,
			TimestampNS: entry.TimestampNS,
		})

	case mbp.TypeReplAck:
		var ack mbp.ReplAck
		if err := msgpack.Unmarshal(payload, &ack); err != nil {
			return fmt.Errorf("unmarshal ReplAck: %w", err)
		}
		c.UpdateReplicaSeq(ack.NodeID, ack.LastSeq)
		return nil

	case mbp.TypeCogForward:
		return c.handleCogForward(fromNodeID, payload)

	case mbp.TypeHandoff:
		return c.HandleHandoff(fromNodeID, payload)

	case mbp.TypeHandoffAck:
		return c.HandleHandoffAck(fromNodeID, payload)

	case mbp.TypeCCSProbe:
		if c.ccsProbe != nil {
			return c.ccsProbe.HandleCCSProbe(fromNodeID, payload)
		}
		return nil

	case mbp.TypeCCSResponse:
		if c.ccsProbe != nil {
			return c.ccsProbe.HandleCCSResponse(fromNodeID, payload)
		}
		return nil

	case mbp.TypeReconProbe:
		if c.reconciler != nil {
			return c.reconciler.HandleReconProbe(fromNodeID, payload)
		}
		return nil

	case mbp.TypeReconReply:
		if c.reconciler != nil {
			return c.reconciler.HandleReconReply(fromNodeID, payload)
		}
		return nil

	case mbp.TypeReconSync:
		if c.reconciler != nil {
			return c.reconciler.HandleReconSync(fromNodeID, payload)
		}
		return nil

	case mbp.TypeReconAck:
		if c.reconciler != nil {
			return c.reconciler.HandleReconAck(fromNodeID, payload)
		}
		return nil

	default:
		return fmt.Errorf("unknown frame type: 0x%02x", frameType)
	}
}

// Stop performs a graceful shutdown: stops streamers, stops MSP, closes connections.
func (c *ClusterCoordinator) Stop() error {
	// Stop all streamers
	c.streamersMu.Lock()
	for id, cancel := range c.streamers {
		cancel()
		delete(c.streamers, id)
	}
	c.streamersMu.Unlock()

	// Close all peer connections
	return c.mgr.Close()
}

// handlePromotion is called when this node wins an election.
func (c *ClusterCoordinator) handlePromotion(epoch uint64) {
	// Acquire the election mutex to atomically check state and set role.
	// This serializes against HandleCortexClaim, which also holds election.mu
	// when demoting. Without this, a concurrent CortexClaim from another node
	// could set state=Follower between tryPromote's lock release and here,
	// causing split role state (election=Follower, coordinator=Primary).
	c.election.mu.Lock()
	if c.election.state != ElectionLeader {
		c.election.mu.Unlock()
		slog.Warn("cluster: election state changed before promotion completed, aborting",
			"epoch", epoch, "node", c.cfg.NodeID)
		return
	}
	c.roleMu.Lock()
	c.role = RolePrimary
	c.roleMu.Unlock()
	c.election.mu.Unlock()

	slog.Info("cluster: promoted to Cortex", "epoch", epoch, "node", c.cfg.NodeID)

	if c.OnBecameCortex != nil {
		c.OnBecameCortex(epoch)
	}
}

// handleDemotion is called when this node loses Cortex status.
func (c *ClusterCoordinator) handleDemotion() {
	c.roleMu.Lock()
	c.role = RoleReplica
	c.roleMu.Unlock()

	// Stop all streamers (no longer primary)
	c.streamersMu.Lock()
	for id, cancel := range c.streamers {
		cancel()
		delete(c.streamers, id)
	}
	c.streamersMu.Unlock()

	// Reset draining state: this node is now a Lobe and accepts no writes anyway.
	c.nodeState.Store(uint32(StateNormal))

	slog.Info("cluster: demoted from Cortex", "node", c.cfg.NodeID)

	// Fire engine callback
	if c.OnBecameLobe != nil {
		c.OnBecameLobe()
	}
}

// handleNewLeader is called when another node becomes Cortex.
func (c *ClusterCoordinator) handleNewLeader(leaderID string, epoch uint64) {
	slog.Info("cluster: new leader detected", "leader", leaderID, "epoch", epoch)
}

// startStreamerForLobe starts a NetworkStreamer for a newly joined Lobe.
func (c *ClusterCoordinator) startStreamerForLobe(info NodeInfo) {
	peer, ok := c.mgr.GetPeer(info.NodeID)
	if !ok {
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	c.streamersMu.Lock()
	// Cancel any existing streamer for this lobe
	if existing, ok := c.streamers[info.NodeID]; ok {
		existing()
	}
	c.streamers[info.NodeID] = cancel
	c.streamersMu.Unlock()

	go func() {
		// Phase 1: always stream from seq=0 (no pruning, full log available)
		s := NewNetworkStreamer(c.repLog, peer, 0)
		if err := s.Stream(ctx); err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("cluster: streamer error for lobe", "lobe", info.NodeID, "err", err)
		}
	}()
}

// stopStreamerForLobe cancels the streamer for a departed Lobe.
func (c *ClusterCoordinator) stopStreamerForLobe(nodeID string) {
	c.streamersMu.Lock()
	if cancel, ok := c.streamers[nodeID]; ok {
		cancel()
		delete(c.streamers, nodeID)
	}
	c.streamersMu.Unlock()
}

// ForwardCognitiveEffects sends a CognitiveSideEffect to the Cortex asynchronously.
// If the Cortex connection is unavailable, the effects are silently dropped.
// Observers discard cognitive effects — they have no workers and do not forward.
// This method never blocks the caller.
func (c *ClusterCoordinator) ForwardCognitiveEffects(effect mbp.CognitiveSideEffect) {
	if c.IsObserver() {
		return // observers discard cognitive side effects
	}
	go func() {
		peer, ok := c.mgr.GetPeer(c.election.CurrentLeader())
		if !ok {
			return // Cortex unreachable, drop
		}
		payload, err := msgpack.Marshal(effect)
		if err != nil {
			return
		}
		_ = peer.Send(mbp.TypeCogForward, payload)
	}()
}

// ConnManager returns the underlying ConnManager (used by server wiring).
func (c *ClusterCoordinator) ConnManager() *ConnManager {
	return c.mgr
}

// MSP returns the underlying MSP (used by server wiring).
func (c *ClusterCoordinator) MSP() *MSP {
	return c.msp
}

// Election returns the underlying Election (used by server wiring).
func (c *ClusterCoordinator) Election() *Election {
	return c.election
}

// RepLog returns the replication log (used for SSE subscription).
func (c *ClusterCoordinator) RepLog() *ReplicationLog {
	return c.repLog
}

// JoinTokenManager returns the join token manager, or nil if no cluster secret is configured.
func (c *ClusterCoordinator) JoinTokenManager() *JoinTokenManager {
	return c.tokenManager
}

// ClusterSecret returns the shared cluster secret used to authenticate
// inter-node requests. Returns "" if no secret is configured.
func (c *ClusterCoordinator) ClusterSecret() string {
	if c.cfg == nil {
		return ""
	}
	return c.cfg.ClusterSecret
}

// TLSManager returns the TLS manager, or nil if TLS is not configured.
func (c *ClusterCoordinator) TLSManager() *ClusterTLS {
	return c.tls
}

// SetTLSManager wires a ClusterTLS into the coordinator.
// Must be called before Run().
func (c *ClusterCoordinator) SetTLSManager(t *ClusterTLS) {
	if c.started.Load() {
		panic("SetTLSManager called after Run()")
	}
	c.tls = t
}

// SetCognitiveWorkers wires the Hebbian worker into the coordinator.
// Must be called before Run().
func (c *ClusterCoordinator) SetCognitiveWorkers(hebbian hebbianSubmitter) {
	if c.started.Load() {
		panic("SetCognitiveWorkers called after Run()")
	}
	c.hebbianWorker = hebbian
}

// CogForwardedTotal returns the total number of co-activations received via
// CogForward frames since startup.
func (c *ClusterCoordinator) CogForwardedTotal() uint64 {
	return atomic.LoadUint64(&c.cogForwardedTotal)
}

// handleCogForward dispatches an incoming CognitiveSideEffect from a Lobe to
// the appropriate local cognitive workers. Never blocks: worker submits are
// non-blocking (select/default). Sends a best-effort CogAck back to the sender.
func (c *ClusterCoordinator) handleCogForward(fromNodeID string, payload []byte) error {
	var effect mbp.CognitiveSideEffect
	if err := msgpack.Unmarshal(payload, &effect); err != nil {
		return fmt.Errorf("handleCogForward: unmarshal: %w", err)
	}

	// Dispatch co-activations to HebbianWorker.
	// Group all co-activations into a single CoActivationEvent using a zero WS
	// (workspace). The Lobe does not send WS — the Cortex's default workspace
	// applies. If WS scoping is needed in future, add it to CognitiveSideEffect.
	if c.hebbianWorker != nil && len(effect.CoActivations) > 0 {
		engrams := make([]cognitive.CoActivatedEngram, len(effect.CoActivations))
		for i, ca := range effect.CoActivations {
			engrams[i] = cognitive.CoActivatedEngram{
				ID:    ca.ID,
				Score: ca.Score,
			}
		}
		ev := cognitive.CoActivationEvent{
			WS:      [8]byte{}, // default workspace
			At:      time.Unix(0, effect.Timestamp),
			Engrams: engrams,
		}
		c.hebbianWorker.Submit(ev) // non-blocking: drops if channel full
	}

	// Increment observability counter by the number of co-activations received.
	if n := uint64(len(effect.CoActivations)); n > 0 {
		atomic.AddUint64(&c.cogForwardedTotal, n)
	}

	// Log restored edges forwarded from Lobe (observability).
	// The Cortex will restore these edges independently on its own next activation.
	if n := uint64(len(effect.RestoredEdges)); n > 0 {
		slog.Debug("cog-forward: restored edges from Lobe", "from", fromNodeID, "count", n)
		atomic.AddUint64(&c.cogForwardedTotal, n)
	}

	// Send CogAck back to Lobe — best-effort, ignore send errors.
	ack := mbp.CogAck{QueryID: effect.QueryID}
	ackPayload, err := msgpack.Marshal(ack)
	if err == nil {
		if peer, ok := c.mgr.GetPeer(fromNodeID); ok {
			_ = peer.Send(mbp.TypeCogAck, ackPayload)
		}
	}

	return nil
}

// IsDraining returns true if the coordinator is in DRAINING state.
// Callers that write to the replication log should check this and return
// ErrDraining if true. The actual write rejection belongs in the engine layer.
func (c *ClusterCoordinator) IsDraining() bool {
	return NodeState(c.nodeState.Load()) == StateDraining
}

// SetCognitiveFlushers wires the cognitive workers' flushable handles for
// graceful handoff. Must be called before Run().
func (c *ClusterCoordinator) SetCognitiveFlushers(hebbian cognitiveFlushable) {
	if c.started.Load() {
		panic("SetCognitiveFlushers called after Run()")
	}
	c.hebbianFlusher = hebbian
}

// SetCCSProbe wires a CCSProbe into the coordinator.
// Must be called before Run().
func (c *ClusterCoordinator) SetCCSProbe(probe *CCSProbe) {
	if c.started.Load() {
		panic("SetCCSProbe called after Run()")
	}
	c.ccsProbe = probe
}

// CognitiveConsistency returns the last computed CCSResult.
// If no CCSProbe has been wired, returns a default excellent result.
func (c *ClusterCoordinator) CognitiveConsistency() CCSResult {
	if c.ccsProbe == nil {
		return CCSResult{
			Score:      1.0,
			Assessment: "excellent",
			NodeScores: map[string]float64{},
			SampledAt:  time.Now(),
		}
	}
	return c.ccsProbe.LastResult()
}

// UpdateReplicaSeq records the latest seq ack'd by a replica.
// Called when TypeReplAck is received.
func (c *ClusterCoordinator) UpdateReplicaSeq(nodeID string, seq uint64) {
	c.replicaSeqs.Store(nodeID, seq)
}

// ReplicaLag returns the lag for a specific nodeID (cortexSeq - replicaSeq).
// Returns 0 if the replica is caught up or unknown.
func (c *ClusterCoordinator) ReplicaLag(nodeID string) uint64 {
	v, ok := c.replicaSeqs.Load(nodeID)
	if !ok {
		return 0
	}
	replicaSeq := v.(uint64)
	cortexSeq := c.repLog.CurrentSeq()
	if cortexSeq <= replicaSeq {
		return 0
	}
	return cortexSeq - replicaSeq
}

// MinReplicatedSeq returns the minimum confirmed sequence number across all
// connected replicas. Returns 0 if no replicas are connected.
func (c *ClusterCoordinator) MinReplicatedSeq() uint64 {
	var minSeq uint64
	hasReplicas := false

	c.replicaSeqs.Range(func(key, value any) bool {
		seq := value.(uint64)
		if !hasReplicas || seq < minSeq {
			minSeq = seq
			hasReplicas = true
		}
		return true
	})

	if !hasReplicas {
		return 0
	}
	return minSeq
}

// GracefulFailover initiates a planned handoff to targetNodeID.
// Blocks until handoff completes or times out.
// targetNodeID must be a known, connected Lobe.
func (c *ClusterCoordinator) GracefulFailover(ctx context.Context, targetNodeID string) error {
	// Serialize: only one handoff at a time.
	c.failoverMu.Lock()
	defer c.failoverMu.Unlock()

	if !c.IsLeader() {
		return errors.New("graceful failover: not the Cortex")
	}

	// Verify target is a known connected peer.
	peer, ok := c.mgr.GetPeer(targetNodeID)
	if !ok {
		return fmt.Errorf("graceful failover: target %q is not a known peer", targetNodeID)
	}

	// Step 1: Enter DRAINING state — rejects new writes.
	c.nodeState.Store(uint32(StateDraining))

	// Ensure we return to StateNormal on any error.
	var handoffSucceeded bool
	defer func() {
		if !handoffSucceeded {
			c.nodeState.Store(uint32(StateNormal))
		}
	}()

	// Step 2: Flush cognitive workers (wait for in-flight batches to finish).
	if c.hebbianFlusher != nil {
		c.hebbianFlusher.Stop()
	}

	// Step 3: Wait for replication convergence (all Lobes caught up).
	cortexSeq := c.repLog.CurrentSeq()
	convergenceTimeout := time.Duration(c.cfg.FailoverConvergenceTimeoutSec) * time.Second
	if convergenceTimeout <= 0 {
		convergenceTimeout = 30 * time.Second
	}
	convergenceCtx, convergenceCancel := context.WithTimeout(ctx, convergenceTimeout)
	defer convergenceCancel()

	if err := c.waitForConvergence(convergenceCtx, cortexSeq); err != nil {
		return fmt.Errorf("graceful failover: convergence failed: %w", err)
	}

	// Step 4: Send HANDOFF frame to target.
	epoch := c.epochStore.Load()
	handoffMsg := mbp.HandoffMessage{
		TargetID:  targetNodeID,
		Epoch:     epoch,
		CortexSeq: cortexSeq,
	}
	payload, err := msgpack.Marshal(handoffMsg)
	if err != nil {
		return fmt.Errorf("graceful failover: marshal handoff: %w", err)
	}

	// Initialize the ack channel before sending (protected by handoffMu).
	c.handoffMu.Lock()
	c.handoffAckCh = make(chan mbp.HandoffAck, 1)
	c.handoffMu.Unlock()

	if err := peer.Send(mbp.TypeHandoff, payload); err != nil {
		return fmt.Errorf("graceful failover: send handoff: %w", err)
	}

	// Step 5: Wait for HANDOFF_ACK.
	ackTimeout := time.Duration(c.cfg.HandoffAckTimeoutSec) * time.Second
	if ackTimeout <= 0 {
		ackTimeout = 5 * time.Second
	}
	ackTimer := time.NewTimer(ackTimeout)
	defer ackTimer.Stop()

	// Nil out the handoff channel on exit so that a late-arriving HandleHandoffAck
	// becomes a no-op instead of sending to a stale, unconsumed channel.
	defer func() {
		c.handoffMu.Lock()
		c.handoffAckCh = nil
		c.handoffMu.Unlock()
	}()

	select {
	case ack := <-c.handoffAckCh:
		if !ack.Success {
			return errors.New("graceful failover: target rejected handoff")
		}
	case <-ackTimer.C:
		return errors.New("graceful failover: HANDOFF_ACK timeout (5s)")
	case <-ctx.Done():
		return ctx.Err()
	}

	// Step 6: Handoff succeeded — demote self and clear draining state.
	handoffSucceeded = true
	c.nodeState.Store(uint32(StateNormal))
	c.handleDemotion()

	slog.Info("cluster: graceful failover complete", "target", targetNodeID, "epoch", epoch)
	return nil
}

// waitForConvergence polls all known Lobes' lastAck seq until all have caught up
// to targetSeq or the context expires.
func (c *ClusterCoordinator) waitForConvergence(ctx context.Context, targetSeq uint64) error {
	// If no entries have been written, convergence is immediate.
	if targetSeq == 0 {
		return nil
	}

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return errors.New("convergence timeout: not all replicas caught up within deadline")
		case <-ticker.C:
			if c.allReplicasConverged(targetSeq) {
				return nil
			}
		}
	}
}

// allReplicasConverged checks if all known Lobes have ack'd at least targetSeq.
func (c *ClusterCoordinator) allReplicasConverged(targetSeq uint64) bool {
	allConverged := true
	hasReplicas := false

	c.replicaSeqs.Range(func(key, value any) bool {
		hasReplicas = true
		seq := value.(uint64)
		if seq < targetSeq {
			allConverged = false
			return false // short-circuit
		}
		return true
	})

	// If no replicas are tracked, convergence is trivially met.
	if !hasReplicas {
		return true
	}
	return allConverged
}

// HandleHandoff processes an incoming HANDOFF frame (target node side).
// The target starts cognitive workers, broadcasts CortexClaim, and sends HANDOFF_ACK.
func (c *ClusterCoordinator) HandleHandoff(fromNodeID string, payload []byte) error {
	var msg mbp.HandoffMessage
	if err := msgpack.Unmarshal(payload, &msg); err != nil {
		return fmt.Errorf("HandleHandoff: unmarshal: %w", err)
	}

	// Verify we are the intended target.
	if msg.TargetID != c.cfg.NodeID {
		return fmt.Errorf("HandleHandoff: handoff target %q does not match local node %q", msg.TargetID, c.cfg.NodeID)
	}

	newEpoch := msg.Epoch + 1

	// Promote self: update epoch, set role, broadcast claim.
	if err := c.epochStore.ForceSet(newEpoch); err != nil {
		return fmt.Errorf("HandleHandoff: ForceSet epoch %d: %w", newEpoch, err)
	}

	// Verify the epoch stuck — if someone else bumped it concurrently,
	// ForceSet is a no-op (returns nil) and we must not promote.
	if actual := c.epochStore.Load(); actual != newEpoch {
		slog.Warn("HandleHandoff: epoch moved past our target, aborting promotion",
			"expected", newEpoch, "actual", actual)
		return fmt.Errorf("HandleHandoff: epoch already advanced to %d, expected %d", actual, newEpoch)
	}

	// Persist intended role BEFORE broadcasting CortexClaim.
	// If the node crashes after broadcasting but before completing in-memory
	// promotion, the persisted role allows the restart path to recover as Cortex
	// rather than rejoining as a Lobe and leaving the cluster leaderless.
	if err := c.epochStore.PersistRole("cortex"); err != nil {
		return fmt.Errorf("HandleHandoff: persist role: %w", err)
	}

	// Broadcast CortexClaim to all peers.
	claim := mbp.CortexClaim{
		CortexID:     c.cfg.NodeID,
		Epoch:        newEpoch,
		FencingToken: newEpoch,
	}
	claimPayload, err := msgpack.Marshal(claim)
	if err != nil {
		slog.Error("HandleHandoff: failed to marshal CortexClaim", "err", err)
		return fmt.Errorf("HandleHandoff: marshal CortexClaim: %w", err)
	}
	c.mgr.Broadcast(mbp.TypeCortexClaim, claimPayload)

	// Set election state to Leader so the race-condition guard in handlePromotion
	// passes. The handoff path bypasses the election vote path, so we must
	// transition the election state here before calling handlePromotion.
	c.election.mu.Lock()
	c.election.state = ElectionLeader
	c.election.currentLeader = c.cfg.NodeID
	c.election.mu.Unlock()

	// Call handlePromotion to set role and fire callbacks (starts cognitive workers).
	c.handlePromotion(newEpoch)

	// Send HANDOFF_ACK back to the old Cortex.
	ack := mbp.HandoffAck{
		TargetID: c.cfg.NodeID,
		Epoch:    newEpoch,
		Success:  true,
	}
	ackPayload, err := msgpack.Marshal(ack)
	if err != nil {
		return fmt.Errorf("HandleHandoff: marshal ack: %w", err)
	}

	peer, ok := c.mgr.GetPeer(fromNodeID)
	if !ok {
		return fmt.Errorf("HandleHandoff: cannot send ack, peer %q not found", fromNodeID)
	}
	return peer.Send(mbp.TypeHandoffAck, ackPayload)
}

// SetReconciler wires a Reconciler into the coordinator.
// Must be called before Run().
func (c *ClusterCoordinator) SetReconciler(rec *Reconciler) {
	if c.started.Load() {
		panic("SetReconciler called after Run()")
	}
	c.reconciler = rec
}

// SetMOL wires the MOL for periodic SafePrune.
// Must be called before Run().
func (c *ClusterCoordinator) SetMOL(mol *wal.MOL) {
	if c.started.Load() {
		panic("SetMOL called after Run()")
	}
	c.mol = mol
}

// IncrementSnapshotCount marks the start of a snapshot transfer.
// SafePrune is skipped while the count is positive.
func (c *ClusterCoordinator) IncrementSnapshotCount() {
	c.snapshotInProgress.Add(1)
}

// DecrementSnapshotCount marks the end of a snapshot transfer.
func (c *ClusterCoordinator) DecrementSnapshotCount() {
	c.snapshotInProgress.Add(-1)
}

// SnapshotInProgress returns true if any snapshot transfer is active.
func (c *ClusterCoordinator) SnapshotInProgress() bool {
	return c.snapshotInProgress.Load() > 0
}

// startPeriodicPrune launches a goroutine that prunes fully-replicated WAL
// segments every 60 seconds. Only runs on the Cortex (leader) node.
// Pruning is skipped while a snapshot transfer is in progress.
func (c *ClusterCoordinator) startPeriodicPrune(ctx context.Context) {
	if c.mol == nil {
		return
	}
	go func() {
		pruneInterval := time.Duration(c.cfg.PruneIntervalSec) * time.Second
		if pruneInterval <= 0 {
			pruneInterval = 60 * time.Second
		}
		ticker := time.NewTicker(pruneInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if !c.IsLeader() {
					continue
				}
				if c.snapshotInProgress.Load() > 0 {
					slog.Warn("cluster: skipping WAL prune — snapshot transfer in progress")
					continue
				}
				minSeq := c.MinReplicatedSeq()
				if minSeq == 0 {
					continue
				}
				pruned, err := c.mol.SafePrune(minSeq)
				if err != nil {
					slog.Warn("cluster: periodic prune failed", "err", err)
				} else if pruned > 0 {
					slog.Info("cluster: pruned WAL segments", "pruned", pruned, "min_replicated_seq", minSeq)
				}
			}
		}
	}()
}

// RemoveNode gracefully removes a node from the cluster.
// Stops the streamer, removes from MSP, cleans up replica tracking, and closes the connection.
// Returns ErrSelfRemoval if nodeID matches the local node.
func (c *ClusterCoordinator) RemoveNode(nodeID string) error {
	if nodeID == c.cfg.NodeID {
		return ErrSelfRemoval
	}

	// Stop the streamer for this node.
	c.streamersMu.Lock()
	if cancel, ok := c.streamers[nodeID]; ok {
		cancel()
		delete(c.streamers, nodeID)
	}
	c.streamersMu.Unlock()

	// Remove from MSP peer tracking.
	c.msp.RemovePeer(nodeID)

	// Clean up replica sequence tracking.
	c.replicaSeqs.Delete(nodeID)

	// Remove from election quorum so removed nodes don't inflate the voter count.
	c.election.UnregisterVoter(nodeID)

	// Close the network connection.
	c.mgr.RemovePeer(nodeID)

	slog.Info("cluster: node removed", "node", nodeID)
	return nil
}

// TriggerReconciliation runs a cognitive reconciliation against the given Lobe node IDs.
// Called by the Cortex when a Lobe reconnects after a partition.
func (c *ClusterCoordinator) TriggerReconciliation(ctx context.Context, lobeNodeIDs []string) (ReconcileResult, error) {
	if c.reconciler == nil {
		return ReconcileResult{}, errors.New("reconciler not configured")
	}
	return c.reconciler.Run(ctx, lobeNodeIDs)
}

// HandleHandoffAck processes an incoming HANDOFF_ACK frame (cortex side).
func (c *ClusterCoordinator) HandleHandoffAck(fromNodeID string, payload []byte) error {
	var ack mbp.HandoffAck
	if err := msgpack.Unmarshal(payload, &ack); err != nil {
		return fmt.Errorf("HandleHandoffAck: unmarshal: %w", err)
	}

	// Deliver to the waiting GracefulFailover goroutine.
	c.handoffMu.Lock()
	ch := c.handoffAckCh
	c.handoffMu.Unlock()

	if ch != nil {
		select {
		case ch <- ack:
		default:
			// Channel full or nobody listening — should not happen in normal flow.
		}
	}
	return nil
}
