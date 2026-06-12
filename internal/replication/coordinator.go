package replication

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"net"
	"strings"
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

// demotionCause distinguishes why a Cortex stepped down, which determines its
// recovery path in the supervisor loop (#522 Step 3).
type demotionCause uint8

const (
	causeClaim      demotionCause = iota // another node legitimately leads (claim/handoff)
	causeQuorumLoss                      // pre-emptive self-demotion; no other leader exists
)

// runMode is a state of the post-leadership supervisor loop (#522 Step 3).
type runMode uint8

const (
	modeLeading       runMode = iota // hold leadership until demoted
	modeFollowing                    // join/stream from a known leader until promoted
	modeWaitingQuorum                // no leader; wait for quorum to return, then re-elect
)

// roleEvent is pushed to roleCh on every promotion/demotion so the supervisor
// loop can transition promptly. Every term also re-derives authoritative state
// (IsLeader / CurrentLeader) on entry/tick, so a dropped or duplicate event only
// delays a transition by at most one heartbeat — it can never wedge the loop.
type roleEvent struct {
	promoted bool
	cause    demotionCause // valid when !promoted
}

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
	cfg          *config.ClusterConfig
	repLog       *ReplicationLog
	applier      *Applier
	epochStore   *EpochStore
	mgr          *ConnManager
	msp          *MSP
	election     *Election
	joinHandler  *JoinHandler
	joinClient   *JoinClient
	tokenManager *JoinTokenManager
	tls          *ClusterTLS

	role   NodeRole
	roleMu sync.RWMutex

	// roleCh carries promotion/demotion events to the supervisor loop (#522 Step 3).
	roleCh chan roleEvent

	// advertiseAddr is this node's routable address (used in PeerHello, #522 Step 4).
	advertiseAddr string
	// electing single-flights the ODOWN-triggered failover election driver so
	// concurrent ODOWN events don't spawn racing election loops (#522 Step 4c).
	electing atomic.Bool
	// runCtx is the lifetime context captured at Run() start, so callbacks wired
	// before Run (e.g. OnODown) can launch ctx-aware loops (#531 PR1).
	runCtx context.Context

	// Per-Lobe streamers (Cortex only): lobeID -> cancel func
	streamers   map[string]context.CancelFunc
	streamersMu sync.Mutex

	// quorumLostSince is the time when this Cortex first noticed it could not
	// reach a quorum of live peers. Zero value means quorum is healthy.
	quorumLostSince time.Time
	// hadQuorum latches true once a leadership term has observed a live quorum,
	// gating pre-emptive quorum-loss demotion so a node still forming its first
	// quorum at bootstrap is exempt. Cleared on demotion (#522 Step 3; the
	// periodic demotion that consumes it lands in PR B / #520).
	hadQuorum bool
	quorumMu  sync.Mutex

	// Cognitive workers (Cortex only): receive forwarded side effects from Lobes.
	hebbianWorker hebbianSubmitter

	// Flushable cognitive workers for graceful handoff. Set via SetCognitiveFlushers.
	hebbianFlusher cognitiveFlushable

	// cogForwardedTotal counts co-activations received via CogForward frames.
	cogForwardedTotal uint64

	// nodeState tracks whether the coordinator is in normal or draining mode.
	nodeState atomic.Uint32

	// reconcileOnHeal controls whether post-partition reconciliation fires when a peer
	// recovers from SDOWN. Hot-reloadable via SetReconcileOnHeal.
	// 1 = enabled (default), 0 = disabled.
	reconcileOnHeal atomic.Uint32

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
	// The address other nodes use to reach this one. Defaults to BindAddr (the
	// config loader applies the same default; this fallback covers callers that
	// build ClusterConfig directly, e.g. tests).
	advertiseAddr := cfg.AdvertiseAddr
	if advertiseAddr == "" {
		advertiseAddr = cfg.BindAddr
	}

	mgr := NewConnManager(cfg.NodeID)
	msp := NewMSP(cfg.NodeID, advertiseAddr, mgr)
	election := NewElection(cfg.NodeID, epochStore, mgr)
	joinHandler := NewJoinHandler(cfg.NodeID, cfg.ClusterSecret, epochStore, repLog, mgr)
	joinHandler.localAddr = advertiseAddr
	joinClient := NewJoinClient(cfg.NodeID, advertiseAddr, cfg.ClusterSecret, epochStore, applier, mgr)
	joinClient.localRole = roleFromConfigString(cfg.Role)

	reconDelay := time.Duration(cfg.ReconDelayMs) * time.Millisecond
	if reconDelay <= 0 {
		reconDelay = 2 * time.Second
	}

	c := &ClusterCoordinator{
		cfg:           cfg,
		repLog:        repLog,
		applier:       applier,
		epochStore:    epochStore,
		mgr:           mgr,
		msp:           msp,
		election:      election,
		joinHandler:   joinHandler,
		joinClient:    joinClient,
		role:          RoleUnknown,
		streamers:     make(map[string]context.CancelFunc),
		roleCh:        make(chan roleEvent, 4),
		advertiseAddr: advertiseAddr,
		reconDelay:    reconDelay,
	}
	// Default reconcile-on-heal to enabled; matches config default (ReconcileHeal=true).
	c.reconcileOnHeal.Store(1)

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
		// OnDemoted is fired only by HandleCortexClaim, which has already set
		// currentLeader to the claimant — a known leader exists.
		c.handleDemotion(causeClaim)
	}
	election.OnNewLeader = func(leaderID string, epoch uint64) {
		c.handleNewLeader(leaderID, epoch)
	}

	// Wire MSP callbacks: trigger a failover election only when the LEADER goes
	// ODOWN. A non-leader (lobe) dying while a healthy leader is known must not
	// start an election (#531 PR1). Sentinels never start elections.
	msp.OnODown = func(nodeID string) {
		if c.IsSentinel() {
			return
		}
		if ldr := c.election.CurrentLeader(); ldr != "" && ldr != nodeID && !c.msp.IsSDown(ldr) {
			return // a different, live leader is known — this is a lobe death, not a failover
		}
		slog.Warn("cluster: ODOWN detected, triggering failover election", "down_node", nodeID)
		c.election.ClearLeader(nodeID) // forget the dead leader so we can re-elect
		c.startElectionWithJitter(c.runCtx)
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
				if c.reconciler != nil && c.reconcileOnHeal.Load() == 1 {
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
		// Metadata-only: must NOT close the live connection (a benign addr
		// readvertisement would otherwise tear down the replication stream). A
		// genuinely dead conn is detected via MSP SDOWN / read errors (#522).
		c.mgr.UpdatePeerAddr(nodeID, newAddr)
	}

	// Wire join handler callbacks
	joinHandler.OnLobeJoined = func(info NodeInfo) {
		c.startStreamerForLobe(info)
	}
	joinHandler.OnLobeLeft = func(nodeID string) {
		c.stopStreamerForLobe(nodeID)
	}
	// Only the leader accepts joins; a non-leader redirects to the leader it knows (#533).
	joinHandler.LeaderInfo = func() (bool, string, string) {
		if c.IsLeader() {
			return true, c.cfg.NodeID, c.advertiseAddr
		}
		ldr := c.election.CurrentLeader()
		var addr string
		if ldr != "" && ldr != c.cfg.NodeID {
			if p, ok := c.mgr.GetPeer(ldr); ok {
				addr = p.Addr()
			}
		}
		return false, ldr, addr
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
	c.runCtx = ctx

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

	// mspMissedThreshold: SDOWN after N missed heartbeat intervals (from config, default 3).
	mspMissedThreshold := c.cfg.SDOWNBeats
	if mspMissedThreshold <= 0 {
		mspMissedThreshold = 3
	}
	go func() {
		if err := c.msp.Run(mspCtx, heartbeat, mspMissedThreshold); err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("cluster: MSP exited with error", "err", err)
		}
	}()

	// Periodically re-evaluate quorum health so a Cortex that loses quorum
	// actually self-demotes within the timeout (#520). Previously this ran only
	// on MSP SDOWN transitions, so sustained quorum loss set the timer once and
	// never re-checked. Safe now that liveness is accurate (Step 1) and demotion
	// recovers via the supervisor (PR A); the hadQuorum gate exempts bootstrap.
	go func() {
		ticker := time.NewTicker(heartbeat)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				c.checkQuorumHealth()
			}
		}
	}()

	// Start peer discovery so nodes with no join relationship (two primaries,
	// sentinels, lobe↔lobe) establish identified connections (#522 Step 4).
	if len(c.cfg.Seeds) > 0 {
		go c.runPeerDiscovery(ctx)
	}

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
		c.election.broadcastClaim(currentEpoch) // announce so peers learn the leader (#531 PR1)
		// Clear the crash-recovery breadcrumb now that in-memory promotion is
		// complete. Without this, every subsequent clean restart re-enters this
		// path instead of going through a normal election. (#176)
		if err := c.epochStore.PersistRole(""); err != nil {
			slog.Warn("cluster: failed to clear persisted role after crash-recovery", "err", err)
		}
		return c.supervise(ctx, modeLeading)
	}

	// A designated primary with seeds first probes for an existing leader BEFORE
	// starting an election — a failover may have promoted another node while we
	// were down. If a leader at epoch ≥ ours exists, follow it (no epoch bump, no
	// assert) so we don't flap the cluster or lose the failover leader's writes
	// (#531). The probe is reliable (synchronous request/response, immune to stale
	// conns / claim timing). A fresh cluster (no seed reachable) skips this fast.
	if c.cfg.Role == "primary" && len(c.cfg.Seeds) > 0 {
		ldrID, ldrAddr, ldrEpoch, found, maxEpoch := c.probeForLeader(ctx)
		if found && ldrEpoch >= currentEpoch {
			slog.Info("cluster: existing cortex found at startup; deferring instead of retaking leadership",
				"node", c.cfg.NodeID, "cortex", ldrID, "epoch", ldrEpoch)
			c.roleMu.Lock()
			c.role = RoleReplica // we're following, not leading — report as a replica
			c.roleMu.Unlock()
			c.election.HandleCortexClaim(mbp.CortexClaim{
				CortexID: ldrID, CortexAddr: ldrAddr, Epoch: ldrEpoch, FencingToken: ldrEpoch,
			})
			return c.supervise(ctx, modeFollowing)
		}
		if maxEpoch > currentEpoch {
			// The cluster moved on (an election is in flight) but no leader has
			// settled. Elect normally — NEVER force-assert into someone else's
			// term — and let the supervisor's waitQuorumTerm re-elect as needed.
			slog.Info("cluster: peers ahead of our epoch but no leader yet; electing without force-assert",
				"node", c.cfg.NodeID, "our_epoch", currentEpoch, "max_peer_epoch", maxEpoch)
			if err := c.election.StartElection(ctx); err != nil {
				return fmt.Errorf("cluster: election failed: %w", err)
			}
			return c.supervise(ctx, c.initialCortexMode())
		}
	}

	// Always start an election on normal startup (epoch 0 = first boot,
	// epoch > 0 = restart after clean shutdown or crash before handoff).
	// The crash-mid-handoff recovery path above handles the only case where we
	// promote without a new election.
	slog.Info("cluster: starting election", "node", c.cfg.NodeID, "epoch", currentEpoch)
	if err := c.election.StartElection(ctx); err != nil {
		return fmt.Errorf("cluster: election failed: %w", err)
	}

	// A node explicitly configured role=primary is the designated leader. If the
	// bootstrap election did not immediately promote it (its seeds are registered
	// as voters but are not up yet to vote, so self-vote is short of quorum),
	// assert leadership directly so it reports as Cortex instead of "unknown".
	c.bootstrapPromoteIfDesignatedPrimary()

	return c.supervise(ctx, c.initialCortexMode())
}

// bootstrapPromoteIfDesignatedPrimary asserts leadership for a node explicitly
// configured role=primary when the bootstrap election left it a candidate —
// typically because its seeds count toward quorum but have not voted yet (#516
// part 1). It is guarded two ways: only for an explicit "primary" (auto-role
// nodes use the normal election), and only while still a candidate — if another
// node had claimed the epoch we would be a follower, so we never override a real
// leader. This mirrors the existing crash-recovery promotion path.
func (c *ClusterCoordinator) bootstrapPromoteIfDesignatedPrimary() {
	if c.cfg.Role != "primary" || c.IsLeader() {
		return
	}
	c.election.mu.Lock()
	if c.election.state != ElectionCandidate {
		c.election.mu.Unlock()
		return
	}
	epoch := c.election.candidateEpoch
	c.election.state = ElectionLeader
	c.election.currentLeader = c.cfg.NodeID
	c.election.mu.Unlock()

	slog.Info("cluster: designated primary asserting leadership at bootstrap",
		"node", c.cfg.NodeID, "epoch", epoch)
	c.handlePromotion(epoch)
	// Announce leadership so followers learn currentLeader through the normal
	// channel (bootstrap-assert bypasses tryPromote, which is the only other
	// broadcaster). Without this, peers never see a claim from a designated
	// primary and can't redirect joins to it (#531 PR1).
	c.election.broadcastClaim(epoch)
}

// probeForLeader polls the seeds for the current leader using side-effect-free
// probes (#531 PR3). It returns the leader's id/addr/epoch if one is found, plus
// the highest epoch observed across all responses (so the caller can avoid
// force-asserting into an in-flight election). Up to 3 passes (one per heartbeat);
// returns early if a pass reaches no seed at all (a fresh cluster — assert fast).
func (c *ClusterCoordinator) probeForLeader(ctx context.Context) (ldrID, ldrAddr string, ldrEpoch uint64, found bool, maxEpoch uint64) {
	hb := c.heartbeatInterval()
	maxEpoch = c.epochStore.Load()
	for pass := 0; pass < 3; pass++ {
		anyResponse := false
		for _, seed := range c.cfg.Seeds {
			if seed == c.advertiseAddr {
				continue
			}
			pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			resp, err := c.joinClient.Probe(pctx, seed)
			cancel()
			if err != nil {
				continue
			}
			anyResponse = true
			if resp.Epoch > maxEpoch {
				maxEpoch = resp.Epoch
			}
			if resp.Accepted && resp.CortexID != "" {
				return resp.CortexID, seed, resp.Epoch, true, maxEpoch // responder is the leader
			}
			// Redirect: probe the named leader to confirm + get its authoritative epoch.
			if resp.CortexID != "" && resp.CortexID != c.cfg.NodeID && resp.CortexAddr != "" {
				rctx, rcancel := context.WithTimeout(ctx, 2*time.Second)
				rresp, rerr := c.joinClient.Probe(rctx, resp.CortexAddr)
				rcancel()
				if rerr == nil && rresp.Accepted && rresp.CortexID != "" {
					if rresp.Epoch > maxEpoch {
						maxEpoch = rresp.Epoch
					}
					return rresp.CortexID, resp.CortexAddr, rresp.Epoch, true, maxEpoch
				}
			}
		}
		if !anyResponse {
			return "", "", 0, false, maxEpoch // no seed reachable → fresh cluster
		}
		select {
		case <-ctx.Done():
			return "", "", 0, false, maxEpoch
		case <-time.After(hb):
		}
	}
	return "", "", 0, false, maxEpoch
}

// joinWithRetry attempts to join the Cortex, cycling through all seeds on each
// attempt and retrying with equal-jitter exponential backoff until success or
// ctx is canceled. Each attempt uses its own 30 s timeout so a canceled startup
// context does not abort in-flight dials.
func (c *ClusterCoordinator) joinWithRetry(ctx context.Context, seeds []string, role string) (JoinResult, error) {
	const maxAttempts = 10
	const joinTimeout = 30 * time.Second
	const maxBackoff = 30 * time.Second

	backoff := time.Second
	var redirect string // a leader address learned from a "not cortex" redirect (#533)
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		cortexAddr := redirect
		if cortexAddr == "" {
			cortexAddr = seeds[(attempt-1)%len(seeds)]
		}
		redirect = ""
		joinCtx, cancel := context.WithTimeout(context.Background(), joinTimeout)
		resp, err := c.joinClient.Join(joinCtx, cortexAddr)
		cancel()
		if err == nil {
			return resp, nil
		}
		// Redirect: a non-leader rejected us and pointed at the leader it knows.
		// Try that address immediately (no backoff) instead of cycling seeds, so
		// a Lobe finds the real Cortex fast after a failover (#533).
		if resp.CortexAddr != "" && resp.CortexAddr != cortexAddr {
			if ctx.Err() != nil {
				return JoinResult{}, ctx.Err()
			}
			redirect = resp.CortexAddr
			continue
		}
		slog.Warn("cluster: join attempt failed, will retry",
			"role", role, "attempt", attempt, "max", maxAttempts,
			"cortex", cortexAddr, "backoff", backoff, "err", err)
		jitter := time.Duration(rand.Int63n(int64(backoff / 2)))
		select {
		case <-ctx.Done():
			return JoinResult{}, ctx.Err()
		case <-time.After(backoff/2 + jitter):
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
	return JoinResult{}, fmt.Errorf("failed to join cortex after %d attempts across %d seed(s)", maxAttempts, len(seeds))
}

// runAsLobe connects to seed, joins, then blocks while receiving replication.
// Join attempts use a dedicated per-attempt context (not the parent) so a
// canceled startup context does not kill in-flight dials. Retries use
// exponential backoff capped at 30 s.
func (c *ClusterCoordinator) runAsLobe(ctx context.Context) error {
	c.roleMu.Lock()
	c.role = RoleReplica
	c.roleMu.Unlock()

	if len(c.cfg.Seeds) == 0 {
		return errors.New("cluster: lobe requires at least one seed address")
	}

	// Run through the supervisor starting as a follower, so that a Lobe which
	// wins an election (e.g. on Cortex ODOWN) transitions to leadership instead
	// of staying wedged inside the stream loop (#522 Step 3 inverse zombie).
	return c.supervise(ctx, modeFollowing)
}

// streamReplication runs the join → read-stream → reconnect loop shared by the
// Lobe and Observer roles. The Cortex streams replication frames over the SAME
// connection used for the join handshake (#448 Bug 2), so the role keeps that
// connection open and reads from it; if the stream drops, it rejoins.
func (c *ClusterCoordinator) streamReplication(ctx context.Context, role string, seeds []string) error {
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		resp, err := c.joinWithRetry(ctx, seeds, role)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// Non-fatal: the leader may not be up yet (or this node is a demoted
			// Cortex whose seeds are its now-down lobes). Back off and keep trying
			// rather than exiting the cluster goroutine (#522 Step 3).
			slog.Warn("cluster: join attempts exhausted, will retry", "role", role, "err", err)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(5 * time.Second):
			}
			continue
		}
		slog.Info("cluster: joined", "role", role, "cortex", resp.CortexID, "epoch", resp.Epoch)

		// Register the Cortex connection and reconcile its identity so the Lobe's
		// MSP heartbeats reach it (keeping it a live voter) and so the GetPeer-based
		// reply paths — VoteResponse, ReplAck, CogForward, HandoffAck — actually
		// send (#522 Step 1). CortexAddr comes from the JoinResponse (Step 0);
		// fall back to the dialed seed for an older Cortex that didn't send it.
		cortexAddr := resp.CortexAddr
		if cortexAddr == "" && len(seeds) > 0 {
			cortexAddr = seeds[0]
		}
		if resp.CortexID != "" && resp.Conn != nil {
			c.mgr.RegisterConn(resp.CortexID, cortexAddr, resp.Conn)
			c.reconcileJoinedPeer(resp.CortexID, cortexAddr, RolePrimary)
			// Adopt the Cortex named by the JoinResponse as our leader, so the
			// election layer knows currentLeader even if its CortexClaim broadcast
			// raced ahead of our join (reliable leader discovery, #531 PR1). This is
			// what lets OnODown tell a leader's death from a mere lobe's.
			if resp.CortexID != c.cfg.NodeID {
				c.election.HandleCortexClaim(mbp.CortexClaim{
					CortexID:     resp.CortexID,
					CortexAddr:   cortexAddr,
					Epoch:        resp.Epoch,
					FencingToken: resp.Epoch,
				})
			}
		}

		streamErr := c.streamFromCortex(ctx, resp.Conn, resp.CortexID)

		// Tear down the connection registration on stream end (RemovePeer closes
		// the conn); the next loop iteration rejoins and re-registers.
		if resp.CortexID != "" {
			c.mgr.RemovePeer(resp.CortexID)
		}
		if resp.Conn != nil {
			resp.Conn.Close()
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		slog.Warn("cluster: replication stream ended, rejoining", "role", role, "err", streamErr)

		// Brief backoff before rejoin so a flapping Cortex doesn't spin us.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// streamFromCortex reads replication frames from the Cortex over the live join
// connection and dispatches each via HandleIncomingFrame, until the connection
// errors or ctx is cancelled. Returns nil on ctx cancellation; the read error
// otherwise. Closing conn on ctx cancellation unblocks the in-flight ReadFrame.
func (c *ClusterCoordinator) streamFromCortex(ctx context.Context, conn net.Conn, cortexID string) error {
	if conn == nil {
		return fmt.Errorf("cluster: no connection to cortex")
	}
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-stop:
		}
	}()

	for {
		frame, err := mbp.ReadFrame(conn)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if err := c.HandleIncomingFrame(cortexID, frame.Type, frame.Payload); err != nil {
			// A single malformed/failed frame must not tear down the stream.
			slog.Warn("cluster: failed to apply streamed frame", "type", frame.Type, "err", err)
		}
		// After applying a replication entry, report our progress so the Cortex
		// can track replica lag (#516 part 3). The Cortex reads these acks via
		// handleClusterConn → HandleIncomingFrame → UpdateReplicaSeq.
		if frame.Type == mbp.TypeReplEntry {
			c.sendReplAck(cortexID, conn)
		}
	}
}

// sendReplAck reports this node's last-applied seq to the Cortex over the
// replication connection. Best-effort: a write error is non-fatal — the next
// entry's ack carries the latest seq, or the stream reconnects.
// reconcileJoinedPeer replaces a seed-<addr> placeholder with the real joined
// node-id in MSP and the voter set (#522 Step 1). Without this the Cortex's MSP
// and votes are keyed by seed-ids while heartbeats/votes arrive under real ids
// and get dropped, and the never-heartbeated seed placeholder drives a false
// SDOWN.
//
// It CONSERVES the voter count by construction so quorum can never inflate, even
// when the dialed/seed address string differs from the advertised address (DNS
// seed vs pod IP, 0.0.0.0 binds, etc.): on a genuinely new join it retires
// exactly one seed placeholder — the address-matching one if present, otherwise
// an arbitrary remaining seed (logged loudly). A rejoin (node already known)
// retires nothing, so it cannot deflate the set or steal another node's seed
// slot. The real identity is added BEFORE the seed is retired so a concurrent
// quorum read can only momentarily over-count (safe), never under-count.
func (c *ClusterCoordinator) reconcileJoinedPeer(nodeID, addr string, role NodeRole) {
	if nodeID == "" {
		return
	}
	// A voter only if neither this node nor the peer is an observer (#529).
	isVoter := c.cfg.Role != "observer" && role != RoleObserver

	alreadyKnown := false
	for _, p := range c.msp.AllPeers() {
		if p.NodeID == nodeID {
			alreadyKnown = true
			break
		}
	}

	c.msp.AddPeer(nodeID, addr, role)
	if isVoter {
		c.election.RegisterVoter(nodeID)
	}
	if alreadyKnown {
		return // rejoin — already counted; retire nothing.
	}

	// New join: retire exactly one seed placeholder to conserve the voter count.
	var matched, anySeed string
	for _, p := range c.msp.AllPeers() {
		if p.NodeID == nodeID || !strings.HasPrefix(p.NodeID, "seed-") {
			continue
		}
		if p.Addr == addr {
			matched = p.NodeID
			break
		}
		if anySeed == "" {
			anySeed = p.NodeID
		}
	}
	retire := matched
	if retire == "" && isVoter {
		// Only a voter join may retire an arbitrary (non-address-matching) seed —
		// an observer that matches no seed must not retire and unregister an
		// unrelated voter placeholder (#529).
		retire = anySeed
		if retire != "" {
			slog.Warn("cluster: joined peer address did not match any seed; retiring a seed placeholder to conserve quorum (set advertise_addr to match the seed addresses)",
				"node", nodeID, "addr", addr, "retired_seed", retire)
		}
	}
	if retire != "" {
		c.msp.RemovePeer(retire)
		c.mgr.RemovePeer(retire)
		// A seed placeholder is always registered as a voter at startup, so a
		// retired seed must always be unregistered — even on an observer join,
		// where the "expected voter" turned out to be a non-voting observer.
		c.election.UnregisterVoter(retire)
	}
}

func (c *ClusterCoordinator) sendReplAck(cortexID string, conn net.Conn) {
	if c.applier == nil {
		return
	}
	payload, err := msgpack.Marshal(mbp.ReplAck{NodeID: c.cfg.NodeID, LastSeq: c.applier.LastApplied()})
	if err != nil {
		return
	}
	// Prefer the registered PeerConn so this write serializes with MSP heartbeats
	// on the same connection (#522 Step 1). Fall back to the raw conn when the
	// Cortex isn't registered (e.g. an older path or a test harness).
	if cortexID != "" {
		if peer, ok := c.mgr.GetPeer(cortexID); ok && peer.IsConnected() {
			_ = peer.Send(mbp.TypeReplAck, payload)
			return
		}
	}
	if conn != nil {
		_ = mbp.WriteFrame(conn, &mbp.Frame{
			Version:       0x01,
			Type:          mbp.TypeReplAck,
			PayloadLength: uint32(len(payload)),
			Payload:       payload,
		})
	}
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

	// Observers never lead, so they don't need the supervisor — just stream.
	return c.streamReplication(ctx, "observer", c.cfg.Seeds)
}

// supervise runs the post-leadership state machine that replaces the terminal
// `<-ctx.Done()` parks. It transitions between leading, following, and waiting-
// for-quorum on promotion/demotion events and never exits except on ctx cancel,
// so a demoted Cortex (or a promoted Lobe) can never become a zombie (#522 Step 3).
func (c *ClusterCoordinator) supervise(ctx context.Context, mode runMode) error {
	for ctx.Err() == nil {
		switch mode {
		case modeLeading:
			mode = c.leadTerm(ctx)
		case modeFollowing:
			mode = c.followTerm(ctx)
		case modeWaitingQuorum:
			mode = c.waitQuorumTerm(ctx)
		}
	}
	return ctx.Err()
}

// initialCortexMode derives the supervisor's starting mode after runAsCortex's
// election preamble: leading if we won, following if a claim was already seen,
// else waiting for quorum (an auto/primary node that hasn't reached quorum yet).
func (c *ClusterCoordinator) initialCortexMode() runMode {
	if c.IsLeader() {
		return modeLeading
	}
	if ldr := c.election.CurrentLeader(); ldr != "" && ldr != c.cfg.NodeID {
		return modeFollowing
	}
	return modeWaitingQuorum
}

// leadTerm holds leadership until a demotion event arrives, then routes to the
// recovery mode the demotion cause dictates. A backstop ticker re-derives role
// from authoritative state so that even if a demotion event were ever dropped
// (unreachable in practice — the loop drains roleCh while leading), the node
// still recovers instead of staying parked as a leader-but-Replica zombie.
func (c *ClusterCoordinator) leadTerm(ctx context.Context) runMode {
	ticker := time.NewTicker(c.heartbeatInterval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return modeLeading
		case <-ticker.C:
			if !c.IsLeader() {
				if ldr := c.election.CurrentLeader(); ldr != "" && ldr != c.cfg.NodeID {
					return modeFollowing
				}
				return modeWaitingQuorum
			}
		case ev := <-c.roleCh:
			if ev.promoted {
				continue // already leading; ignore a duplicate promote
			}
			if ev.cause == causeQuorumLoss {
				return modeWaitingQuorum
			}
			return modeFollowing
		}
	}
}

// followTerm joins and streams from a known leader until this node is promoted.
// streamReplication runs under a per-term context so a promotion cleanly tears
// down the stream before this node starts leading.
func (c *ClusterCoordinator) followTerm(ctx context.Context) runMode {
	termCtx, termCancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = c.streamReplication(termCtx, "lobe", c.followSeeds())
	}()
	defer func() { termCancel(); <-done }()

	for {
		select {
		case <-ctx.Done():
			return modeFollowing
		case <-done:
			return modeFollowing // streamReplication only returns on ctx cancel
		case ev := <-c.roleCh:
			if ev.promoted {
				return modeLeading
			}
			// A further demotion while following: stay following if a leader is
			// still known, else wait for quorum and re-elect.
			if ldr := c.election.CurrentLeader(); ldr == "" || ldr == c.cfg.NodeID {
				return modeWaitingQuorum
			}
		}
	}
}

// waitQuorumTerm holds, leaderless, until a live voter quorum returns, then
// starts a fresh election (rate-limited). Defects to following if a higher claim
// appears. This is how a quorum-loss-demoted node resumes leadership when its
// lobes rejoin — without thrashing (re-election needs a real vote quorum).
func (c *ClusterCoordinator) waitQuorumTerm(ctx context.Context) runMode {
	hb := c.heartbeatInterval()
	ticker := time.NewTicker(hb)
	defer ticker.Stop()
	backoff := hb
	var nextElection time.Time // zero = eligible now

	for {
		select {
		case <-ctx.Done():
			return modeWaitingQuorum
		case ev := <-c.roleCh:
			if ev.promoted {
				return modeLeading
			}
		case <-ticker.C:
		}

		if c.IsLeader() {
			return modeLeading
		}
		if ldr := c.election.CurrentLeader(); ldr != "" && ldr != c.cfg.NodeID {
			return modeFollowing // a higher claim appeared — follow, never assert
		}
		if !c.electing.Load() && c.aliveVoters() >= c.election.Quorum() && (nextElection.IsZero() || !time.Now().Before(nextElection)) {
			// Yield if the ODOWN-triggered failover driver is already electing, so
			// the two drivers don't race the same node into split candidacies (#531 PR1).
			if err := c.election.StartElection(ctx); err != nil {
				slog.Warn("cluster: re-election attempt failed", "node", c.cfg.NodeID, "err", err)
			}
			nextElection = time.Now().Add(backoff)
			if backoff < 8*hb {
				backoff *= 2
			}
		}
	}
}

// followSeeds returns the seed list to dial as a follower, with the known
// leader's address tried first (a demoted Cortex's configured seeds are its
// lobes; the actual leader must be tried before them).
func (c *ClusterCoordinator) followSeeds() []string {
	seeds := append([]string(nil), c.cfg.Seeds...)
	if ldr := c.election.CurrentLeader(); ldr != "" && ldr != c.cfg.NodeID {
		if p, ok := c.mgr.GetPeer(ldr); ok && p.Addr() != "" {
			seeds = append([]string{p.Addr()}, seeds...)
		}
	}
	return seeds
}

// aliveVoters counts self plus live MSP peers that are registered voters — the
// correct population for quorum decisions (LivePeers alone includes observers).
func (c *ClusterCoordinator) aliveVoters() int {
	count := 1 // self
	for _, p := range c.msp.LivePeers() {
		if c.election.IsVoter(p.NodeID) {
			count++
		}
	}
	return count
}

func (c *ClusterCoordinator) heartbeatInterval() time.Duration {
	if c.cfg.HeartbeatMS > 0 {
		return time.Duration(c.cfg.HeartbeatMS) * time.Millisecond
	}
	return time.Second
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

// ReconciledMembers returns the cluster member list with joined Lobes shown by
// their real join-handshake identity (node-id, role, last applied seq) instead
// of the synthetic seed-<addr> MSP placeholders (#516). Used for status
// reporting; the raw KnownNodes()/MSP view is left untouched for internal logic.
func (c *ClusterCoordinator) ReconciledMembers() []NodeInfo {
	base := c.KnownNodes()
	members := base
	if c.joinHandler != nil {
		members = reconcileMembers(base, c.joinHandler.Members())
	}
	// Overlay the latest acked seq per replica (#516 part 3) so the reported
	// last_seq reflects live progress, not the value captured at join time.
	for i := range members {
		if v, ok := c.replicaSeqs.Load(members[i].NodeID); ok {
			members[i].LastSeq = v.(uint64)
		}
	}
	return members
}

// reconcileMembers replaces a base (MSP) entry with the matching joined member
// when their addresses match, and appends any joined member not present in base.
func reconcileMembers(base, joined []NodeInfo) []NodeInfo {
	joinedByAddr := make(map[string]NodeInfo, len(joined))
	for _, m := range joined {
		joinedByAddr[m.Addr] = m
	}
	out := make([]NodeInfo, 0, len(base)+len(joined))
	seen := make(map[string]bool, len(joined))
	for _, n := range base {
		if m, ok := joinedByAddr[n.Addr]; ok {
			out = append(out, m)
			seen[m.Addr] = true
		} else {
			out = append(out, n)
		}
	}
	for _, m := range joined {
		if !seen[m.Addr] {
			out = append(out, m)
			seen[m.Addr] = true
		}
	}
	return out
}

// checkQuorumHealth is called periodically (from the MSP tick goroutine via
// OnSDown callback) to detect sustained quorum loss. If the Cortex cannot
// reach a quorum of live peers for quorumLossTimeout (5s), it pre-emptively
// demotes itself to prevent split-brain writes.
func (c *ClusterCoordinator) checkQuorumHealth() {
	if !c.IsLeader() {
		return
	}

	quorum := c.election.Quorum()
	// Count self + live REGISTERED voters — the correct quorum population (plain
	// LivePeers would also count non-voting observers, #522 Step 3).
	totalAlive := c.aliveVoters()

	c.quorumMu.Lock()

	if totalAlive >= quorum {
		// Quorum healthy: latch hadQuorum for this term and reset the loss timer.
		c.hadQuorum = true
		c.quorumLostSince = time.Time{}
		c.quorumMu.Unlock()
		return
	}

	// Quorum is not met. Only a leader that has HELD a live quorum this term
	// pre-emptively demotes — a node still forming its first quorum (a designated
	// primary awaiting lobe joins at bootstrap) is exempt, so the periodic check
	// can never tear down a healthy bootstrap (#522 Step 3 / #520).
	if !c.hadQuorum {
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
		// Pre-emptive quorum-loss demotion: no successor exists, so release the
		// election's leader state (else StartElection stays blocked) and demote
		// with the quorum-loss cause so the supervisor enters WAITING_QUORUM and
		// resumes leadership when quorum returns (#522 Step 3).
		c.election.StepDown()
		c.handleDemotion(causeQuorumLoss)
	}
}

// EvictConn disconnects the registered peer for nodeID if it still wraps conn —
// called by the connection handler when an inbound conn's read loop exits, so a
// killed peer's stale PeerConn doesn't block its restart (#534).
func (c *ClusterCoordinator) EvictConn(nodeID string, conn net.Conn) {
	c.mgr.EvictIfConn(nodeID, conn)
}

// HandleIncomingJoin processes a TypeJoinRequest frame on a raw inbound conn
// whose node ID is not yet known. It registers the live conn under req.NodeID
// so that peer.Send works immediately (no dial required), processes the join
// request, and returns the joining node's stable ID so that handleClusterConn
// can use it for all subsequent frames on the same connection.
func (c *ClusterCoordinator) HandleIncomingJoin(conn net.Conn, payload []byte) (string, bool, error) {
	var req mbp.JoinRequest
	if err := msgpack.Unmarshal(payload, &req); err != nil {
		return "", false, fmt.Errorf("unmarshal JoinRequest: %w", err)
	}

	// Side-effect-free leader-discovery probe (#531 PR3): answer who the leader is
	// on the raw conn, WITHOUT registering the conn, adding a member, or streaming
	// a snapshot. A returning designated primary uses this to find the current
	// leader before deciding whether to assert or defer.
	if req.Probe {
		// Authenticate the probe so an unauthenticated party can't learn cluster
		// topology (parity with the normal join's secret check, #531 PR3 review).
		if !c.joinHandler.ValidSecret(req.NodeID, req.SecretHash) {
			return req.NodeID, false, nil
		}
		isLeader := c.IsLeader()
		ldrID, ldrAddr := c.cfg.NodeID, c.advertiseAddr
		if !isLeader {
			ldrID = c.election.CurrentLeader()
			ldrAddr = ""
			if ldrID != "" && ldrID != c.cfg.NodeID {
				if p, ok := c.mgr.GetPeer(ldrID); ok {
					ldrAddr = p.Addr()
				}
			}
		}
		resp := mbp.JoinResponse{Accepted: isLeader, CortexID: ldrID, CortexAddr: ldrAddr, Epoch: c.epochStore.Load()}
		if respPayload, err := msgpack.Marshal(resp); err == nil {
			_ = mbp.WriteFrame(conn, &mbp.Frame{
				Version:       0x01,
				Type:          mbp.TypeJoinResponse,
				PayloadLength: uint32(len(respPayload)),
				Payload:       respPayload,
			})
		}
		return req.NodeID, false, nil
	}

	// Leadership gate BEFORE registering the conn (#533): only the leader accepts
	// joins. A non-leader must NOT RegisterConn the joiner here — that would evict
	// an existing live conn (e.g. the lobe↔lobe hello conn carrying SDOWN gossip)
	// and replace it with a throwaway rejected-join conn, silently breaking
	// failover. Reply with a redirect on the raw conn and let the caller close it.
	if !c.IsLeader() {
		ldr := c.election.CurrentLeader()
		var ldrAddr string
		if ldr != "" && ldr != c.cfg.NodeID {
			if p, ok := c.mgr.GetPeer(ldr); ok {
				ldrAddr = p.Addr()
			}
		}
		resp := mbp.JoinResponse{
			Accepted:     false,
			RejectReason: "not cortex",
			Epoch:        c.epochStore.Load(),
			CortexID:     ldr,
			CortexAddr:   ldrAddr,
		}
		if respPayload, err := msgpack.Marshal(resp); err == nil {
			_ = mbp.WriteFrame(conn, &mbp.Frame{
				Version:       0x01,
				Type:          mbp.TypeJoinResponse,
				PayloadLength: uint32(len(respPayload)),
				Payload:       respPayload,
			})
		}
		return req.NodeID, false, nil
	}

	// Register the live inbound conn so peer.Send succeeds immediately.
	// RegisterConn returns the PeerConn it created under the write lock,
	// eliminating the TOCTOU gap of a separate GetPeer call.
	peer := c.mgr.RegisterConn(req.NodeID, req.Addr, conn)

	resp := c.joinHandler.HandleJoinRequest(req, peer)
	respPayload, err := msgpack.Marshal(resp)
	if err != nil {
		return req.NodeID, false, fmt.Errorf("marshal JoinResponse: %w", err)
	}
	if err := peer.Send(mbp.TypeJoinResponse, respPayload); err != nil {
		return req.NodeID, false, fmt.Errorf("cluster: send JoinResponse to %s: %w", req.NodeID, err)
	}

	// Replace the seed placeholder with this joiner's real identity in MSP + voters
	// so heartbeats and votes keyed by its node-id are no longer dropped (#522). Use
	// the joiner's advertised role (0 = legacy = replica) so a joining Observer is
	// not registered as a voter (#529).
	if resp.Accepted {
		joinerRole := NodeRole(req.Role)
		if joinerRole == RoleUnknown {
			joinerRole = RoleReplica
		}
		c.reconcileJoinedPeer(req.NodeID, req.Addr, joinerRole)
	}

	if resp.NeedsSnapshot {
		c.IncrementSnapshotCount()
		go func() {
			defer c.DecrementSnapshotCount()
			ctx := context.Background()
			if _, err := c.joinHandler.StreamSnapshot(ctx, peer); err != nil {
				slog.Error("cluster: snapshot stream failed; closing connection so lobe can reconnect and retry",
					"lobe", req.NodeID, "err", err)
				_ = peer.Close()
				return
			}
			// Snapshot complete — only NOW is it safe to start the streamer.
			// See JoinHandler.FireOnLobeJoined doc for the race this avoids.
			c.joinHandler.FireOnLobeJoined(req.NodeID)
		}()
	} else {
		// No snapshot path: JoinResponse already on the wire, safe to start
		// the streamer immediately.
		c.joinHandler.FireOnLobeJoined(req.NodeID)
	}
	return req.NodeID, true, nil
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

	case mbp.TypeSDown:
		var msg mbp.SDownNotification
		if err := msgpack.Unmarshal(payload, &msg); err != nil {
			return fmt.Errorf("unmarshal SDownNotification: %w", err)
		}
		// Key the vote by the authenticated frame source, not the payload's
		// SenderID — otherwise one peer could forge many sender ids to drive ODOWN
		// unilaterally (#522 Step 4c). Gossip is one-hop, so fromNodeID is the voter.
		c.msp.RecordDownVote(fromNodeID, msg.TargetID)
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

	c.pushRoleEvent(roleEvent{promoted: true})

	if c.OnBecameCortex != nil {
		c.OnBecameCortex(epoch)
	}
}

// pushRoleEvent delivers a role transition to the supervisor loop without
// blocking. If the buffer is full the event is dropped — terms re-derive
// authoritative state each tick, so a dropped event only delays a transition.
func (c *ClusterCoordinator) pushRoleEvent(ev roleEvent) {
	select {
	case c.roleCh <- ev:
	default:
	}
}

// handleDemotion is called when this node loses Cortex status.
func (c *ClusterCoordinator) handleDemotion(cause demotionCause) {
	c.roleMu.Lock()
	c.role = RoleReplica
	c.roleMu.Unlock()

	// A new leadership term is over: clear the "observed a live quorum" latch so
	// the next term's pre-emptive quorum-loss demotion is gated afresh (#522).
	c.quorumMu.Lock()
	c.hadQuorum = false
	c.quorumLostSince = time.Time{}
	c.quorumMu.Unlock()

	// Stop all streamers (no longer primary)
	c.streamersMu.Lock()
	for id, cancel := range c.streamers {
		cancel()
		delete(c.streamers, id)
	}
	c.streamersMu.Unlock()

	// Reset draining state: this node is now a Lobe and accepts no writes anyway.
	c.nodeState.Store(uint32(StateNormal))

	slog.Info("cluster: demoted from Cortex", "node", c.cfg.NodeID, "cause", cause)

	// Hand the supervisor loop the recovery cue (#522 Step 3).
	c.pushRoleEvent(roleEvent{promoted: false, cause: cause})

	// Fire the engine callback asynchronously: this path runs on the MSP tick
	// goroutine (via checkQuorumHealth), and OnBecameLobe stops cognitive workers
	// which may block — it must not stall heartbeat processing. The role flip
	// above already gates writes, so the worker teardown can complete out of band.
	if c.OnBecameLobe != nil {
		go c.OnBecameLobe()
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

	// Step 6: Handoff succeeded — demote self and clear draining state. The
	// handoff target is the new leader, so follow it (causeClaim).
	handoffSucceeded = true
	c.nodeState.Store(uint32(StateNormal))
	c.handleDemotion(causeClaim)

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
	if err := peer.Send(mbp.TypeHandoffAck, ackPayload); err != nil {
		return fmt.Errorf("HandleHandoff: send ack: %w", err)
	}

	// ACK delivered. The old Cortex will now demote itself. Clear the crash-recovery
	// breadcrumb so that a subsequent clean restart goes through a normal election
	// rather than incorrectly re-entering the crash-recovery path. (#176)
	// If this clear fails, the breadcrumb remains. On the next restart, crash-recovery
	// fires again — which is safe (idempotent re-promotion) rather than leaving the
	// cluster without a leader.
	if err := c.epochStore.PersistRole(""); err != nil {
		slog.Warn("cluster: failed to clear persisted role after handoff ack", "err", err)
	}
	return nil
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

// SetReconcileOnHeal controls whether post-partition reconciliation fires when a
// peer recovers from SDOWN. Safe to call at any time (hot-reloadable).
func (c *ClusterCoordinator) SetReconcileOnHeal(enabled bool) {
	if enabled {
		c.reconcileOnHeal.Store(1)
	} else {
		c.reconcileOnHeal.Store(0)
	}
}

// GetClusterConfig returns the in-memory cluster config.
func (c *ClusterCoordinator) GetClusterConfig() *config.ClusterConfig {
	return c.cfg
}

// CCSProbe returns the CCSProbe, or nil if not configured.
func (c *ClusterCoordinator) CCSProbe() *CCSProbe {
	return c.ccsProbe
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
