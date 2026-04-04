package replication

import (
	"net"
	"sync"
)

// FrameHandler is called when a frame of a registered type is received.
type FrameHandler func(fromNodeID string, payload []byte) error

// ConnManager manages a set of PeerConn instances keyed by node ID.
// Get and list operations use a read lock; mutations use a write lock.
type ConnManager struct {
	localNodeID string
	peers       map[string]*PeerConn
	mu          sync.RWMutex

	handlersMu sync.RWMutex
	handlers   map[uint8]FrameHandler
}

// NewConnManager returns a ConnManager for the given local node.
func NewConnManager(localNodeID string) *ConnManager {
	return &ConnManager{
		localNodeID: localNodeID,
		peers:       make(map[string]*PeerConn),
		handlers:    make(map[uint8]FrameHandler),
	}
}

// RegisterHandler registers a handler for a specific MBP frame type.
// Only one handler per frame type. Overwrites previous registration.
func (m *ConnManager) RegisterHandler(frameType uint8, h FrameHandler) {
	m.handlersMu.Lock()
	defer m.handlersMu.Unlock()
	m.handlers[frameType] = h
}

// Dispatch calls the registered handler for frameType, if any.
// Returns nil if no handler is registered (not an error).
func (m *ConnManager) Dispatch(fromNodeID string, frameType uint8, payload []byte) error {
	m.handlersMu.RLock()
	h := m.handlers[frameType]
	m.handlersMu.RUnlock()

	if h == nil {
		return nil
	}
	return h(fromNodeID, payload)
}

// AddPeer adds a new PeerConn for nodeID/addr. If a connection for nodeID
// already exists it is closed before being replaced.
func (m *ConnManager) AddPeer(nodeID, addr string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if existing, ok := m.peers[nodeID]; ok {
		_ = existing.Close()
	}
	m.peers[nodeID] = NewPeerConn(nodeID, addr)
}

// RegisterConn registers an already-established inbound connection as a peer.
// Unlike AddPeer, the PeerConn wraps the live conn so Send works immediately
// without a separate Connect call. If a peer for nodeID already exists it is
// closed and replaced.
func (m *ConnManager) RegisterConn(nodeID, addr string, conn net.Conn) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if existing, ok := m.peers[nodeID]; ok {
		_ = existing.Close()
	}
	m.peers[nodeID] = NewPeerConnFromConn(nodeID, addr, conn)
}

// RemovePeer closes and removes the peer identified by nodeID.
// It is a no-op if the peer does not exist.
func (m *ConnManager) RemovePeer(nodeID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if p, ok := m.peers[nodeID]; ok {
		_ = p.Close()
		delete(m.peers, nodeID)
	}
}

// GetPeer returns the PeerConn for nodeID, or (nil, false) if not found.
func (m *ConnManager) GetPeer(nodeID string) (*PeerConn, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	p, ok := m.peers[nodeID]
	return p, ok
}

// Peers returns a snapshot slice of all managed PeerConns.
func (m *ConnManager) Peers() []*PeerConn {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]*PeerConn, 0, len(m.peers))
	for _, p := range m.peers {
		out = append(out, p)
	}
	return out
}

// Broadcast sends frameType/payload to every connected peer.
// It returns a map of nodeID → error for any peer whose Send failed.
// Peers that are not yet connected are skipped (no entry in the error map).
func (m *ConnManager) Broadcast(frameType uint8, payload []byte) map[string]error {
	m.mu.RLock()
	// Snapshot the map so we can release the read lock before doing I/O.
	snapshot := make([]*PeerConn, 0, len(m.peers))
	for _, p := range m.peers {
		snapshot = append(snapshot, p)
	}
	m.mu.RUnlock()

	errs := make(map[string]error)
	for _, p := range snapshot {
		if !p.IsConnected() {
			continue
		}
		if err := p.Send(frameType, payload); err != nil {
			errs[p.NodeID()] = err
		}
	}
	return errs
}

// Close closes all managed peer connections.
func (m *ConnManager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	for id, p := range m.peers {
		_ = p.Close()
		delete(m.peers, id)
	}
	return nil
}
