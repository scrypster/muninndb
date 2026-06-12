package replication

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"net"
	"sync"

	"github.com/cockroachdb/pebble"
	"github.com/vmihailenco/msgpack/v5"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// JoinHandler handles incoming JoinRequests on the Cortex side.
// It is safe for concurrent access.
type JoinHandler struct {
	localNodeID   string
	localAddr     string // this Cortex's advertised address (for JoinResponse.CortexAddr)
	clusterSecret string
	epochStore    *EpochStore
	repLog        *ReplicationLog
	db            *pebble.DB // non-nil when snapshot streaming is supported
	mgr           *ConnManager

	members map[string]NodeInfo
	mu      sync.RWMutex

	// OnLobeJoined is called (without mu held) when a Lobe successfully joins.
	OnLobeJoined func(info NodeInfo)
	// OnLobeLeft is called (without mu held) when a Lobe leaves.
	OnLobeLeft func(nodeID string)
	// LeaderInfo reports whether this node is the current leader, and if not, the
	// leader it knows (for join redirects). Only the leader accepts joins (#533).
	LeaderInfo func() (isLeader bool, leaderID, leaderAddr string)
}

// NewJoinHandler creates a JoinHandler for the Cortex.
// db may be nil; if non-nil, the handler will stream snapshots to joining Lobes.
func NewJoinHandler(localNodeID, clusterSecret string, epochStore *EpochStore, repLog *ReplicationLog, mgr *ConnManager) *JoinHandler {
	return &JoinHandler{
		localNodeID:   localNodeID,
		clusterSecret: clusterSecret,
		epochStore:    epochStore,
		repLog:        repLog,
		mgr:           mgr,
		members:       make(map[string]NodeInfo),
	}
}

// NewJoinHandlerWithDB creates a JoinHandler that can stream snapshots.
func NewJoinHandlerWithDB(localNodeID, clusterSecret string, epochStore *EpochStore, repLog *ReplicationLog, db *pebble.DB, mgr *ConnManager) *JoinHandler {
	h := NewJoinHandler(localNodeID, clusterSecret, epochStore, repLog, mgr)
	h.db = db
	return h
}

// ValidSecret reports whether secretHash is a valid HMAC of nodeID (+ role for
// protocol v2+) under the cluster secret. Always true in open mode.
// Used to authenticate probes (#531 PR3). The role+protoVer gate matches the
// logic in HandleJoinRequest so probe and join auth stay in parity (#538).
func (h *JoinHandler) ValidSecret(nodeID string, role uint8, protoVer uint16, secretHash []byte) bool {
	if h.clusterSecret == "" {
		return true
	}
	expected := hmac.New(sha256.New, []byte(h.clusterSecret))
	expected.Write([]byte(nodeID))
	if protoVer >= 2 {
		expected.Write([]byte{role})
	}
	return hmac.Equal(secretHash, expected.Sum(nil))
}

// HandleJoinRequest processes a JoinRequest from a connecting Lobe.
// On success it adds the Lobe to the members map and returns an accepted
// JoinResponse. It deliberately does NOT register the conn in mgr (the
// coordinator already did so via RegisterConn before calling this) and does
// NOT fire OnLobeJoined. The caller must invoke FireOnLobeJoined(req.NodeID)
// only after the JoinResponse — and any post-join snapshot — has been written
// to the wire. See FireOnLobeJoined for the handshake race this split avoids.
//
// Epoch validation: we reject a JoinRequest only when epoch == 0, meaning the
// cluster has not yet elected a Cortex and cannot safely accept new members.
// In Phase 1 there is no other epoch-based check on JoinRequest; the joining
// Lobe's LastApplied field carries its last replication seq (not an epoch), and
// any stale lobe is brought up to date via NetworkStreamer starting from seq=0.
func (h *JoinHandler) HandleJoinRequest(req mbp.JoinRequest, conn *PeerConn) mbp.JoinResponse {
	currentEpoch := h.epochStore.Load()

	if req.NodeID == "" {
		return mbp.JoinResponse{
			Accepted:     false,
			RejectReason: "empty node ID",
			Epoch:        currentEpoch,
			CortexID:     h.localNodeID,
		}
	}

	// Validate cluster secret if configured. Protocol v2+ covers Role in the HMAC
	// to prevent role spoofing (#538); v1 senders use nodeID-only for rolling-upgrade compat.
	if h.clusterSecret != "" {
		expectedHash := hmac.New(sha256.New, []byte(h.clusterSecret))
		expectedHash.Write([]byte(req.NodeID))
		if req.ProtocolVersion >= 2 {
			expectedHash.Write([]byte{req.Role})
		}
		if !hmac.Equal(req.SecretHash, expectedHash.Sum(nil)) {
			return mbp.JoinResponse{
				Accepted:     false,
				RejectReason: "invalid cluster secret",
				Epoch:        currentEpoch,
				CortexID:     h.localNodeID,
			}
		}
	}

	// Protocol version check
	if req.ProtocolVersion > mbp.CurrentProtocolVersion {
		return mbp.JoinResponse{
			Accepted: false,
			RejectReason: fmt.Sprintf(
				"protocol version %d is not supported by this Cortex (max supported: %d). "+
					"This Lobe binary is newer than the Cortex — upgrade the Cortex first.",
				req.ProtocolVersion, mbp.CurrentProtocolVersion,
			),
			MinProtocolVersion:     mbp.MinSupportedProtocolVersion,
			CurrentProtocolVersion: mbp.CurrentProtocolVersion,
		}
	}
	if req.ProtocolVersion < mbp.MinSupportedProtocolVersion {
		return mbp.JoinResponse{
			Accepted: false,
			RejectReason: fmt.Sprintf(
				"protocol version %d is no longer supported (minimum: %d, current: %d). "+
					"Upgrade this Lobe to a binary that speaks protocol version >= %d.",
				req.ProtocolVersion, mbp.MinSupportedProtocolVersion,
				mbp.CurrentProtocolVersion, mbp.MinSupportedProtocolVersion,
			),
			MinProtocolVersion:     mbp.MinSupportedProtocolVersion,
			CurrentProtocolVersion: mbp.CurrentProtocolVersion,
		}
	}
	// Deprecation window: accepted but warn so operators can schedule upgrades
	// before MinSupportedProtocolVersion is raised in a future release.
	if req.ProtocolVersion < mbp.DeprecatedProtocolVersion {
		slog.Warn("join: lobe using deprecated protocol version; will be rejected in a future release",
			"lobe", req.NodeID,
			"lobe_version", req.ProtocolVersion,
			"deprecated_below", mbp.DeprecatedProtocolVersion,
			"current", mbp.CurrentProtocolVersion,
		)
	} else if req.ProtocolVersion == 0 {
		slog.Warn("join: legacy (pre-versioned) lobe connecting, consider upgrading",
			"lobe", req.NodeID)
	}

	// Epoch guard: epoch 0 means the cluster has not yet bootstrapped — reject.
	if currentEpoch == 0 {
		return mbp.JoinResponse{
			Accepted:     false,
			RejectReason: "cluster not yet bootstrapped (epoch 0)",
			Epoch:        currentEpoch,
			CortexID:     h.localNodeID,
		}
	}

	// Leadership gate (#533): only the current leader may accept a join. A
	// non-leader rejects with a redirect to the leader it knows, so a Lobe that
	// dialed the wrong node (e.g. another Lobe during a failover) doesn't get a
	// bogus "you joined me" and start following a non-leader — the root cause of
	// the mutual-join split-brain.
	if h.LeaderInfo != nil {
		isLeader, leaderID, leaderAddr := h.LeaderInfo()
		if !isLeader {
			return mbp.JoinResponse{
				Accepted:     false,
				RejectReason: "not cortex",
				Epoch:        currentEpoch,
				CortexID:     leaderID,
				CortexAddr:   leaderAddr,
			}
		}
	}

	info := NodeInfo{
		NodeID:  req.NodeID,
		Addr:    req.Addr,
		Role:    RoleReplica,
		LastSeq: req.LastApplied,
	}

	h.mu.Lock()
	h.members[req.NodeID] = info
	h.mu.Unlock()

	// NOTE: do NOT call h.mgr.AddPeer here. The coordinator already called
	// mgr.RegisterConn(nodeID, addr, conn) with the live inbound TCP connection
	// before invoking HandleJoinRequest. Calling AddPeer would close that live
	// connection and replace it with a disconnected PeerConn{conn: nil},
	// causing the immediately-following peer.Send(JoinResponse) to return
	// ErrNotConnected.
	//
	// NOTE: OnLobeJoined is NOT fired here. Firing it inline would spawn the
	// NetworkStreamer goroutine before the caller has sent the JoinResponse
	// frame on the shared PeerConn — racing the streamer's first ReplEntry
	// frame against the JoinResponse frame and corrupting the lobe-side
	// handshake parser (issue: cortex join-race / #409 follow-up). The caller
	// must invoke FireOnLobeJoined(nodeID) after JoinResponse (+ Snapshot)
	// have been fully written to the wire.

	resp := mbp.JoinResponse{
		Accepted:   true,
		CortexID:   h.localNodeID,
		CortexAddr: h.localAddr,
		Epoch:      currentEpoch,
	}

	// Phase 2: if this handler has a DB, every joining Lobe gets a snapshot.
	// SnapshotSeq is captured here (before the snapshot is actually sent) so
	// the Lobe knows which seq to start NetworkStreamer catch-up from.
	if h.db != nil {
		resp.NeedsSnapshot = true
		resp.SnapshotSeq = h.repLog.CurrentSeq()
	}

	return resp
}

// FireOnLobeJoined invokes the OnLobeJoined callback for a previously-registered
// lobe. Callers must invoke this only AFTER the JoinResponse (and, when
// applicable, the post-join snapshot stream) has been fully written to the
// shared PeerConn — otherwise the streamer's first ReplEntry frame can race
// the JoinResponse frame and break the lobe-side handshake parser.
func (h *JoinHandler) FireOnLobeJoined(nodeID string) {
	h.mu.RLock()
	info, ok := h.members[nodeID]
	cb := h.OnLobeJoined
	h.mu.RUnlock()
	if !ok {
		// The lobe is not (or no longer) a member. This means a mis-ordered or
		// duplicate FireOnLobeJoined call — never legitimate. Warn so the bug
		// surfaces instead of disappearing as a silent no-op.
		slog.Warn("join: FireOnLobeJoined called for unregistered node; skipping callback",
			"node", nodeID)
		return
	}
	if cb == nil {
		// No callback wired (e.g. handler used without a coordinator). Legitimate
		// no-op — nothing to warn about.
		return
	}
	cb(info)
}

// StreamSnapshot sends a full Pebble snapshot to the peer over conn.
// Called by the coordinator immediately after sending the JoinResponse when
// resp.NeedsSnapshot is true.
func (h *JoinHandler) StreamSnapshot(ctx context.Context, conn *PeerConn) (uint64, error) {
	if h.db == nil {
		return 0, fmt.Errorf("join handler: snapshot streaming not configured (no db)")
	}
	sender := NewSnapshotSender(h.db, h.repLog)
	return sender.Send(ctx, conn)
}

// HandleLeave processes a LeaveMessage from a departing Lobe.
func (h *JoinHandler) HandleLeave(msg mbp.LeaveMessage) {
	h.mu.Lock()
	_, exists := h.members[msg.NodeID]
	if exists {
		delete(h.members, msg.NodeID)
	}
	cb := h.OnLobeLeft
	h.mu.Unlock()

	// RemovePeer is called after releasing h.mu to avoid holding the lock
	// during TCP close (which can block for the connection timeout duration).
	if exists {
		h.mgr.RemovePeer(msg.NodeID)
	}

	if exists && cb != nil {
		cb(msg.NodeID)
	}
}

// Members returns a snapshot of currently joined lobes.
func (h *JoinHandler) Members() []NodeInfo {
	h.mu.RLock()
	defer h.mu.RUnlock()

	out := make([]NodeInfo, 0, len(h.members))
	for _, info := range h.members {
		out = append(out, info)
	}
	return out
}

// JoinResult is returned by JoinClient.Join() and JoinClient.joinConn().
// It extends JoinResponse with the seq the Lobe should start NetworkStreamer from.
type JoinResult struct {
	mbp.JoinResponse
	// StreamFromSeq is the seq the Lobe must pass to NewNetworkStreamer as
	// startSeq. If NeedsSnapshot was true, this equals the SnapshotSeq received
	// in the snapshot header (authoritative). Otherwise it equals the Lobe's
	// own LastApplied at join time.
	StreamFromSeq uint64

	// Conn is the live connection to the Cortex. On a successful Join it is
	// returned OPEN — the Cortex streams replication frames over this same
	// connection, so the Lobe keeps it and reads from it (see runAsLobe). The
	// caller owns closing it. Nil for joinConn callers that pass their own conn.
	Conn net.Conn
}

// JoinClient handles the Lobe-side join handshake.
type JoinClient struct {
	localNodeID   string
	localAddr     string
	localRole     NodeRole // this node's configured role, advertised in JoinRequest (#529)
	clusterSecret string
	epochStore    *EpochStore
	applier       *Applier
	db            *pebble.DB // non-nil to enable snapshot reception
	mgr           *ConnManager
}

// NewJoinClient creates a JoinClient for a Lobe node.
// applier may be nil if the caller manages apply state separately.
func NewJoinClient(localNodeID, localAddr, clusterSecret string, epochStore *EpochStore, applier *Applier, mgr *ConnManager) *JoinClient {
	return &JoinClient{
		localNodeID:   localNodeID,
		localAddr:     localAddr,
		clusterSecret: clusterSecret,
		epochStore:    epochStore,
		applier:       applier,
		mgr:           mgr,
	}
}

// NewJoinClientWithDB creates a JoinClient that can receive snapshots.
func NewJoinClientWithDB(localNodeID, localAddr, clusterSecret string, epochStore *EpochStore, applier *Applier, db *pebble.DB, mgr *ConnManager) *JoinClient {
	c := NewJoinClient(localNodeID, localAddr, clusterSecret, epochStore, applier, mgr)
	c.db = db
	return c
}

// Join connects to cortexAddr, sends a JoinRequest, and receives a JoinResponse.
// On success it updates the local epoch via epochStore.ForceSet(resp.Epoch) and
// returns a JoinResult. When the Cortex signals NeedsSnapshot, the snapshot is
// received and written to the local DB before this method returns.
// The caller should start a NetworkStreamer from JoinResult.StreamFromSeq+1.
// On failure it returns an error; the caller should retry with backoff.
// Probe sends a side-effect-free leader-discovery JoinRequest{Probe:true} to
// cortexAddr and returns the response (who the leader is). It does not register,
// snapshot, or mutate any state — used by a returning designated primary to find
// the current leader before deciding whether to assert or defer (#531 PR3).
func (c *JoinClient) Probe(ctx context.Context, cortexAddr string) (mbp.JoinResponse, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", cortexAddr)
	if err != nil {
		return mbp.JoinResponse{}, fmt.Errorf("probe: dial %s: %w", cortexAddr, err)
	}
	defer conn.Close()

	var secretHash []byte
	if c.clusterSecret != "" {
		h := hmac.New(sha256.New, []byte(c.clusterSecret))
		h.Write([]byte(c.localNodeID))
		h.Write([]byte{uint8(c.localRole)})
		secretHash = h.Sum(nil)
	}
	req := mbp.JoinRequest{
		NodeID:          c.localNodeID,
		Addr:            c.localAddr,
		SecretHash:      secretHash,
		Role:            uint8(c.localRole),
		Probe:           true,
		ProtocolVersion: mbp.CurrentProtocolVersion,
	}
	payload, err := msgpack.Marshal(req)
	if err != nil {
		return mbp.JoinResponse{}, fmt.Errorf("probe: marshal request: %w", err)
	}
	if err := mbp.WriteFrame(conn, &mbp.Frame{
		Version:       0x01,
		Type:          mbp.TypeJoinRequest,
		PayloadLength: uint32(len(payload)),
		Payload:       payload,
	}); err != nil {
		return mbp.JoinResponse{}, fmt.Errorf("probe: send request: %w", err)
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetReadDeadline(dl)
	}
	respFrame, err := mbp.ReadFrame(conn)
	if err != nil {
		return mbp.JoinResponse{}, fmt.Errorf("probe: read response: %w", err)
	}
	if respFrame.Type != mbp.TypeJoinResponse {
		return mbp.JoinResponse{}, fmt.Errorf("probe: unexpected frame type 0x%02x", respFrame.Type)
	}
	var resp mbp.JoinResponse
	if err := msgpack.Unmarshal(respFrame.Payload, &resp); err != nil {
		return mbp.JoinResponse{}, fmt.Errorf("probe: unmarshal response: %w", err)
	}
	return resp, nil
}

func (c *JoinClient) Join(ctx context.Context, cortexAddr string) (JoinResult, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", cortexAddr)
	if err != nil {
		return JoinResult{}, fmt.Errorf("join: dial %s: %w", cortexAddr, err)
	}

	result, err := c.joinConn(ctx, conn)
	if err != nil {
		// The handshake failed — close the conn here. On success we deliberately
		// leave it OPEN: the Cortex streams replication frames over this same
		// connection (#448 Bug 2), so the caller (runAsLobe) keeps and reads it.
		conn.Close()
		return result, err
	}
	result.Conn = conn
	return result, nil
}

// joinConn performs the join handshake over an already-established net.Conn.
// Separated from Join() to allow net.Pipe()-based tests without real TCP.
func (c *JoinClient) joinConn(ctx context.Context, conn net.Conn) (JoinResult, error) {
	// Check for incomplete snapshot from a previous failed attempt.
	// Only flag an error if: DB exists, snapshot is incomplete, AND there is data present.
	// If DB is brand new (empty), IsSnapshotComplete will return false but that is expected.
	if c.db != nil {
		recv := NewSnapshotReceiver(c.db)
		isComplete := recv.IsSnapshotComplete()
		if !isComplete {
			// Check if DB has any data at all. If truly empty, snapshot is expected to be incomplete.
			iter, err := c.db.NewIter(nil)
			hasData := false
			if err == nil {
				hasData = iter.First()
				iter.Close()
			}
			if hasData {
				// DB has data but snapshot is incomplete — self-heal by wiping partial state.
				slog.Warn("join: incomplete snapshot detected — wiping partial state for clean rejoin",
					"node", c.localNodeID)
				if err := recv.WipeForResnapshot(); err != nil {
					return JoinResult{}, fmt.Errorf("join: wipe partial snapshot: %w", err)
				}
				slog.Info("join: partial snapshot wiped, proceeding with fresh snapshot request",
					"node", c.localNodeID)
			}
		}
	}

	var lastApplied uint64
	if c.applier != nil {
		lastApplied = c.applier.LastApplied()
	}

	// Compute HMAC-SHA256 of nodeID+role (#538: role covered at proto v2+).
	var secretHash []byte
	if c.clusterSecret != "" {
		h := hmac.New(sha256.New, []byte(c.clusterSecret))
		h.Write([]byte(c.localNodeID))
		h.Write([]byte{uint8(c.localRole)})
		secretHash = h.Sum(nil)
	}

	req := mbp.JoinRequest{
		NodeID:          c.localNodeID,
		Addr:            c.localAddr,
		LastApplied:     lastApplied,
		SecretHash:      secretHash,
		Role:            uint8(c.localRole),
		ProtocolVersion: mbp.CurrentProtocolVersion,
	}

	payload, err := msgpack.Marshal(req)
	if err != nil {
		return JoinResult{}, fmt.Errorf("join: marshal request: %w", err)
	}

	frame := &mbp.Frame{
		Version:       0x01,
		Type:          mbp.TypeJoinRequest,
		PayloadLength: uint32(len(payload)),
		Payload:       payload,
	}
	if err := mbp.WriteFrame(conn, frame); err != nil {
		return JoinResult{}, fmt.Errorf("join: send request: %w", err)
	}

	// Read response — honour ctx cancellation by closing conn if ctx fires.
	respDone := make(chan struct{})
	var respFrame *mbp.Frame
	var readErr error
	go func() {
		defer close(respDone)
		respFrame, readErr = mbp.ReadFrame(conn)
	}()

	select {
	case <-ctx.Done():
		conn.Close()
		return JoinResult{}, ctx.Err()
	case <-respDone:
	}

	if readErr != nil {
		return JoinResult{}, fmt.Errorf("join: read response: %w", readErr)
	}
	if respFrame.Type != mbp.TypeJoinResponse {
		return JoinResult{}, fmt.Errorf("join: unexpected frame type 0x%02x", respFrame.Type)
	}

	var resp mbp.JoinResponse
	if err := msgpack.Unmarshal(respFrame.Payload, &resp); err != nil {
		return JoinResult{}, fmt.Errorf("join: unmarshal response: %w", err)
	}

	if !resp.Accepted {
		slog.Error("join: rejected by Cortex",
			"reason", resp.RejectReason,
			"cortex_min_version", resp.MinProtocolVersion,
			"cortex_current_version", resp.CurrentProtocolVersion,
			"our_version", mbp.CurrentProtocolVersion,
		)
		return JoinResult{JoinResponse: resp}, fmt.Errorf("join: rejected by cortex: %s", resp.RejectReason)
	}

	// Update local epoch to match the Cortex's epoch.
	if err := c.epochStore.ForceSet(resp.Epoch); err != nil {
		return JoinResult{JoinResponse: resp}, fmt.Errorf("join: update epoch: %w", err)
	}

	// Register Cortex as a known peer in ConnManager.
	if resp.CortexID != "" && resp.CortexAddr != "" {
		c.mgr.AddPeer(resp.CortexID, resp.CortexAddr)
	}

	result := JoinResult{
		JoinResponse:  resp,
		StreamFromSeq: lastApplied, // Phase 1 fallback: stream from current lastApplied
	}

	// Phase 2: receive snapshot if Cortex signals NeedsSnapshot.
	if resp.NeedsSnapshot {
		snapshotSeq, err := c.receiveSnapshot(ctx, conn)
		if err != nil {
			return result, fmt.Errorf("join: receive snapshot: %w", err)
		}
		result.StreamFromSeq = snapshotSeq
	}

	return result, nil
}

// receiveSnapshot reads a snapshot stream from conn and applies it to the local DB.
// Returns the SnapshotSeq from the snapshot header.
// Falls back gracefully: if no db is configured, returns resp.SnapshotSeq without
// writing any data (for testing / old-node compatibility).
func (c *JoinClient) receiveSnapshot(ctx context.Context, conn net.Conn) (uint64, error) {
	if c.db == nil {
		return 0, fmt.Errorf("join: snapshot reception requires a DB")
	}

	peer := &PeerConn{conn: conn}
	recv := NewSnapshotReceiver(c.db)
	return recv.Receive(ctx, peer)
}

// Leave sends a LeaveMessage to the Cortex before graceful shutdown.
func (c *JoinClient) Leave(ctx context.Context, cortexAddr string) error {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", cortexAddr)
	if err != nil {
		return fmt.Errorf("leave: dial %s: %w", cortexAddr, err)
	}
	defer conn.Close()

	return c.leaveConn(conn)
}

// leaveConn sends a LeaveMessage over an existing net.Conn.
func (c *JoinClient) leaveConn(conn net.Conn) error {
	msg := mbp.LeaveMessage{
		NodeID: c.localNodeID,
		Epoch:  c.epochStore.Load(),
	}

	payload, err := msgpack.Marshal(msg)
	if err != nil {
		return fmt.Errorf("leave: marshal message: %w", err)
	}

	frame := &mbp.Frame{
		Version:       0x01,
		Type:          mbp.TypeLeave,
		PayloadLength: uint32(len(payload)),
		Payload:       payload,
	}
	return mbp.WriteFrame(conn, frame)
}
