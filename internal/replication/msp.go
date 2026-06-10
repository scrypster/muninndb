package replication

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vmihailenco/msgpack/v5"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// PeerState tracks liveness for one peer.
type PeerState struct {
	NodeID      string
	Addr        string
	Role        NodeRole
	LastSeen    time.Time
	MissedBeats int
	SDown       bool
}

// MSP manages heartbeats and failure detection for a node.
type MSP struct {
	localNodeID string
	localAddr   string // advertised address included in PING payloads
	mgr         *ConnManager
	peers       map[string]*PeerState
	mu          sync.RWMutex

	// interval stores the current heartbeat interval, updated by SetHeartbeatInterval.
	// Protected by mu. Zero means "use the value passed to Run()".
	interval time.Duration

	// missedThreshold is the SDOWN beat count, hot-reloadable via SetMissedThreshold.
	missedThreshold atomic.Int32

	// votedDown is scaffolded for Phase 2 gossip: maps nodeID → set of voter nodeIDs
	// that have reported it SDOWN. Not yet populated from gossip.
	votedDown map[string]map[string]struct{}

	OnSDown       func(nodeID string)          // called when a peer goes SDOWN
	OnODown       func(nodeID string)          // called when quorum agrees SDOWN
	OnRecover     func(nodeID string)          // called when SDOWN peer recovers
	OnAddrChanged func(nodeID, newAddr string) // called when a peer advertises a new address
}

// pingPayload is the msgpack-encoded payload for PING/PONG frames.
type pingPayload struct {
	NodeID string `msgpack:"node_id"`
	Addr   string `msgpack:"addr,omitempty"` // advertised address; empty for legacy peers
}

// NewMSP creates a new MSP heartbeat engine for the given local node.
// localAddr is the address this node advertises in PING payloads so peers can
// detect and react to address changes (e.g., after a pod restart).
func NewMSP(localNodeID, localAddr string, mgr *ConnManager) *MSP {
	return &MSP{
		localNodeID: localNodeID,
		localAddr:   localAddr,
		mgr:         mgr,
		peers:       make(map[string]*PeerState),
		votedDown:   make(map[string]map[string]struct{}),
	}
}

// AddPeer registers a peer to monitor.
func (m *MSP) AddPeer(nodeID, addr string, role NodeRole) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.peers[nodeID] = &PeerState{
		NodeID:   nodeID,
		Addr:     addr,
		Role:     role,
		LastSeen: time.Now(),
	}
}

// RemovePeer stops monitoring the given peer.
func (m *MSP) RemovePeer(nodeID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.peers, nodeID)
	delete(m.votedDown, nodeID)
}

// HandlePing processes an incoming PING from a peer — updates LastSeen,
// clears MissedBeats, recovers from SDOWN if needed, and updates the peer's
// address if the payload advertises a new one.
// payload may be nil (legacy peers that send no payload are accepted).
func (m *MSP) HandlePing(fromNodeID string, payload []byte) {
	addr := m.decodeAddr(payload)
	m.handleHeartbeat(fromNodeID, addr)
}

// HandlePong processes an incoming PONG from a peer — same as HandlePing.
func (m *MSP) HandlePong(fromNodeID string, payload []byte) {
	addr := m.decodeAddr(payload)
	m.handleHeartbeat(fromNodeID, addr)
}

// decodeAddr decodes the Addr field from a raw msgpack PING/PONG payload.
// Returns "" if payload is nil, empty, or does not contain an addr field.
func (m *MSP) decodeAddr(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	var p pingPayload
	if err := msgpack.Unmarshal(payload, &p); err != nil {
		return ""
	}
	return p.Addr
}

// handleHeartbeat updates peer state on any received heartbeat signal.
// fromAddr is the address the peer advertised; empty string means unknown/legacy.
func (m *MSP) handleHeartbeat(fromNodeID, fromAddr string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	p, ok := m.peers[fromNodeID]
	if !ok {
		return
	}

	wasSDown := p.SDown
	p.LastSeen = time.Now()
	p.MissedBeats = 0
	p.SDown = false

	// Update address if the peer advertises a new one (pod restart, IP change).
	if fromAddr != "" && fromAddr != p.Addr {
		slog.Info("msp: peer address updated",
			"node", fromNodeID, "old", p.Addr, "new", fromAddr)
		p.Addr = fromAddr
		if m.OnAddrChanged != nil {
			go m.OnAddrChanged(fromNodeID, fromAddr)
		}
	}

	if wasSDown && m.OnRecover != nil {
		go m.OnRecover(fromNodeID)
	}
}

// IsSDown reports whether the given node is considered subjectively down.
func (m *MSP) IsSDown(nodeID string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	p, ok := m.peers[nodeID]
	if !ok {
		return false
	}
	return p.SDown
}

// IsODown reports true when this node considers nodeID SDOWN and at least
// quorum-1 other nodes have also voted it down (Phase 1: local view only).
func (m *MSP) IsODown(nodeID string, quorum int) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	p, ok := m.peers[nodeID]
	if !ok {
		return false
	}
	if !p.SDown {
		return false
	}

	// Count votes: this node is 1 vote. Add any gossip votes from votedDown.
	votes := 1
	if voters, ok := m.votedDown[nodeID]; ok {
		votes += len(voters)
	}
	return votes >= quorum
}

// LivePeers returns a snapshot of peers that are not SDOWN.
func (m *MSP) LivePeers() []*PeerState {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]*PeerState, 0, len(m.peers))
	for _, p := range m.peers {
		if !p.SDown {
			copy := *p
			out = append(out, &copy)
		}
	}
	return out
}

// AllPeers returns a snapshot of all registered peers.
func (m *MSP) AllPeers() []*PeerState {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]*PeerState, 0, len(m.peers))
	for _, p := range m.peers {
		copy := *p
		out = append(out, &copy)
	}
	return out
}

// Run is the main heartbeat loop. It sends PINGs every pingInterval and
// uses LastSeen to detect SDOWN when a peer has not been heard from for
// missedThreshold * pingInterval. Blocks until ctx is cancelled.
// The missedThreshold can be updated at runtime via SetMissedThreshold.
func (m *MSP) Run(ctx context.Context, pingInterval time.Duration, missedThreshold int) error {
	m.missedThreshold.Store(int32(missedThreshold))

	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()

	payload, err := msgpack.Marshal(pingPayload{NodeID: m.localNodeID, Addr: m.localAddr})
	if err != nil {
		return err
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			m.tick(payload, pingInterval, int(m.missedThreshold.Load()))
		}
	}
}

// SetMissedThreshold updates the SDOWN missed-beat threshold for future ticks.
// Safe to call after Run(). The change takes effect on the next tick.
func (m *MSP) SetMissedThreshold(n int) {
	m.missedThreshold.Store(int32(n))
}

// nonObserverQuorumLocked computes quorum based only on non-observer peers
// (plus self). Must be called with mu held.
// Quorum = floor(nonObserverCount / 2) + 1
func (m *MSP) nonObserverQuorumLocked() int {
	count := 1 // self is always a non-observer from MSP's perspective
	for _, p := range m.peers {
		if p.Role != RoleObserver {
			count++
		}
	}
	return count/2 + 1
}

// NonObserverQuorum returns the current quorum excluding observer peers.
// Thread-safe.
func (m *MSP) NonObserverQuorum() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.nonObserverQuorumLocked()
}

// SetHeartbeatInterval updates the heartbeat interval for future ticks.
// Safe to call after Run(). The change takes effect on the next ticker reset
// in Run(); existing in-flight ticks are not affected.
func (m *MSP) SetHeartbeatInterval(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.interval = d
}

// tick runs one heartbeat cycle: broadcasts PING and evaluates peer liveness
// using LastSeen. The quorum parameter is the base quorum; ODOWN detection uses
// a dynamically computed quorum that excludes observer peers.
func (m *MSP) tick(pingPayload []byte, pingInterval time.Duration, missedThreshold int) {
	// Broadcast PING to all connected peers.
	m.mgr.Broadcast(mbp.TypePing, pingPayload)

	m.mu.Lock()
	defer m.mu.Unlock()

	// Compute effective quorum excluding observers for ODOWN detection.
	effectiveQuorum := m.nonObserverQuorumLocked()

	sdownThreshold := time.Duration(missedThreshold) * pingInterval

	for _, p := range m.peers {
		// Update MissedBeats for observability only — it is not used for SDOWN
		// decisions. The authoritative liveness check is time-based.
		p.MissedBeats = int(time.Since(p.LastSeen) / pingInterval)

		if time.Since(p.LastSeen) > sdownThreshold && !p.SDown {
			p.SDown = true
			nodeID := p.NodeID
			isObserver := p.Role == RoleObserver

			if m.OnSDown != nil {
				go m.OnSDown(nodeID)
			}

			// Observers do not participate in ODOWN voting: their SDOWN state
			// is not counted toward the quorum needed to declare ODOWN.
			if !isObserver {
				// Check ODOWN under the same lock snapshot.
				votes := 1
				if voters, ok := m.votedDown[nodeID]; ok {
					votes += len(voters)
				}
				if votes >= effectiveQuorum && m.OnODown != nil {
					go m.OnODown(nodeID)
				}
			}
		}
	}
}
