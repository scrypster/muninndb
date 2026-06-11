package replication

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// ErrNotConnected is returned when Send or Receive is called before Connect.
var ErrNotConnected = errors.New("peer not connected")

// sendWriteTimeout bounds a single frame write so one peer with a full TCP
// buffer cannot stall the shared MSP tick goroutine (which broadcasts heartbeats
// to every peer sequentially) and cascade false SDOWNs.
const sendWriteTimeout = 5 * time.Second

// connKind records how a PeerConn was established, which drives the
// simultaneous-dial / hello-vs-join precedence in RegisterConnKind (#522 Step 4).
type connKind uint8

const (
	kindSeed  connKind = iota // disconnected placeholder from the seed list
	kindJoin                  // established via the join/replication handshake
	kindHello                 // established via the PeerHello discovery handshake
)

// PeerConn is a single persistent TCP connection to one remote peer.
// It is safe for concurrent Send calls.
type PeerConn struct {
	nodeID string
	addr   string
	conn   net.Conn
	kind   connKind
	mu     sync.Mutex
	closed bool
}

// NewPeerConn creates a new PeerConn for the given remote node.
func NewPeerConn(nodeID, addr string) *PeerConn {
	return &PeerConn{
		nodeID: nodeID,
		addr:   addr,
	}
}

// NewPeerConnFromConn wraps an already-established inbound TCP connection as a
// PeerConn. Used on the Cortex side when a Lobe dials in: the conn exists but
// the Lobe's stable listen address (addr) comes from the JoinRequest payload.
func NewPeerConnFromConn(nodeID, addr string, conn net.Conn) *PeerConn {
	return &PeerConn{
		nodeID: nodeID,
		addr:   addr,
		conn:   conn,
	}
}

// NodeID returns the remote node ID.
func (p *PeerConn) NodeID() string { return p.nodeID }

// Addr returns the remote address ("host:port").
func (p *PeerConn) Addr() string { return p.addr }

// IsConnected reports whether the connection is currently open.
func (p *PeerConn) IsConnected() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.conn != nil && !p.closed
}

// Connect dials the remote peer over TCP. The provided context controls the
// dial timeout. Connect does not start any background goroutines; the caller
// is responsible for reconnect logic.
func (p *PeerConn) Connect(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return errors.New("peer conn is closed")
	}

	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", p.addr)
	if err != nil {
		return fmt.Errorf("dial %s: %w", p.addr, err)
	}

	// Close any stale connection before replacing it.
	if p.conn != nil {
		_ = p.conn.Close()
	}
	p.conn = conn
	return nil
}

// Send writes a single MBP frame to the connection.
// It is safe to call from multiple goroutines concurrently.
func (p *PeerConn) Send(frameType uint8, payload []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.conn == nil || p.closed {
		return ErrNotConnected
	}

	f := &mbp.Frame{
		Version:       0x01,
		Type:          frameType,
		PayloadLength: uint32(len(payload)),
		Payload:       payload,
	}
	// Bound the write so a wedged peer can't block the shared heartbeat tick.
	_ = p.conn.SetWriteDeadline(time.Now().Add(sendWriteTimeout))
	err := mbp.WriteFrame(p.conn, f)
	_ = p.conn.SetWriteDeadline(time.Time{}) // clear for subsequent reads/writes
	return err
}

// Receive reads one MBP frame from the connection.
// It is safe to call from a single reader goroutine.
func (p *PeerConn) Receive() (frameType uint8, payload []byte, err error) {
	p.mu.Lock()
	conn := p.conn
	closed := p.closed
	p.mu.Unlock()

	if conn == nil || closed {
		return 0, nil, ErrNotConnected
	}

	// There is a window between the unlock above and ReadFrame below where a
	// concurrent Close() can close the underlying connection. If that happens,
	// ReadFrame returns a net.OpError (use of closed network connection), which
	// is the correct error to surface to the caller — they should treat it as a
	// disconnect and stop using this PeerConn.
	f, err := mbp.ReadFrame(conn)
	if err != nil {
		// If the connection was closed concurrently, return ErrNotConnected so
		// callers get a consistent sentinel rather than a raw net error.
		p.mu.Lock()
		isClosed := p.closed
		p.mu.Unlock()
		if isClosed {
			return 0, nil, ErrNotConnected
		}
		return 0, nil, err
	}
	return f.Type, f.Payload, nil
}

// Close closes the connection idempotently. Calling Close more than once is safe.
func (p *PeerConn) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return nil
	}
	p.closed = true
	if p.conn != nil {
		err := p.conn.Close()
		p.conn = nil
		return err
	}
	return nil
}
