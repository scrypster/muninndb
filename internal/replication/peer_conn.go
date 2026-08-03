package replication

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// ErrNotConnected is returned when Send or Receive is called before Connect.
var ErrNotConnected = errors.New("peer not connected")

// Write-path liveness bounds (#627).
//
// The original design bounded a whole frame write with one fixed 5s deadline.
// That is a constant applied to a quantity measured somewhere else entirely:
// the frame may be a 40-byte heartbeat or a 1 MB snapshot chunk, and the link
// may be loopback or a sleepy laptop on a WAN tunnel. A Lobe that fell behind
// therefore could not catch up — the receiver applied entries slower than the
// sender pushed, the socket buffer filled, one frame write crossed 5s, the
// stream died, the Lobe rejoined further behind, and the loop tightened.
// Raising the constant only moves the cliff.
//
// The replacement separates the two things the old constant was conflating:
//
//   - sendIdleTimeout is a *liveness* bound: how long the peer may accept no
//     bytes at all. It resets on every byte of forward progress, so it is
//     independent of frame size and link speed. A peer that keeps draining the
//     socket is slow, not wedged, and slow is not a failure.
//   - sendMaxDuration is the *outer* bound: how long any single frame may hold
//     the peer's write slot even while dribbling. Without it a malicious or
//     pathologically slow peer could hold a stream open forever by accepting
//     one byte per idle period. It is deliberately far larger than any
//     legitimate single-frame transfer (the largest bulk frame in the system is
//     a 1 MB snapshot chunk, ~10s at 100 KB/s) — it exists to make "forever"
//     impossible, not to police throughput.
//   - sendSlotWait bounds how long a *different* caller waits for the write
//     slot. Frames on one connection must be serialized, but the shared MSP
//     tick goroutine broadcasts to every peer sequentially and must not block
//     behind a multi-megabyte transfer. A peer that is busy streaming is alive,
//     so that wait expires with ErrPeerBusy and leaves the connection intact.
const (
	sendIdleTimeout = 5 * time.Second
	sendMaxDuration = 2 * time.Minute
	sendSlotWait    = 5 * time.Second

	// sendWriteChunk is the granularity at which the idle deadline is renewed
	// and the outer bound re-checked.
	sendWriteChunk = 64 * 1024
)

// ErrPeerBusy is returned when the peer's write slot was held by another frame
// for longer than sendSlotWait. It is explicitly NOT a connection failure: the
// connection is left open and the peer is still considered alive. Callers on
// the shared broadcast path treat it as "skip this tick"; bulk senders retry.
var ErrPeerBusy = errors.New("peer write slot busy")

// ErrSendStalled is returned when a single frame write exceeded sendMaxDuration
// while still making token forward progress — a dribbling peer.
var ErrSendStalled = errors.New("peer write exceeded maximum single-frame duration")

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

	// writeSlot serializes frame writes on this connection. It is separate from
	// mu so that Close, IsConnected and Receive are not blocked for the duration
	// of a bulk write. Lazily created because tests and join.go construct
	// PeerConn literals; the zero value must stay usable.
	slotOnce  sync.Once
	writeSlot chan struct{}

	// Test seams: zero means "use the package default".
	sendIdle time.Duration
	sendMax  time.Duration
	slotWait time.Duration
}

func (p *PeerConn) slot() chan struct{} {
	p.slotOnce.Do(func() { p.writeSlot = make(chan struct{}, 1) })
	return p.writeSlot
}

func (p *PeerConn) idleTimeout() time.Duration {
	if p.sendIdle > 0 {
		return p.sendIdle
	}
	return sendIdleTimeout
}

func (p *PeerConn) maxDuration() time.Duration {
	if p.sendMax > 0 {
		return p.sendMax
	}
	return sendMaxDuration
}

func (p *PeerConn) slotTimeout() time.Duration {
	if p.slotWait > 0 {
		return p.slotWait
	}
	return sendSlotWait
}

// progressWriter writes to conn in bounded chunks, renewing the write deadline
// on every byte of forward progress. A deadline that fires after some bytes
// were accepted is not an error — it is the definition of a slow-but-live peer.
type progressWriter struct {
	conn         net.Conn
	idle         time.Duration
	hardDeadline time.Time
}

func (w *progressWriter) Write(b []byte) (int, error) {
	total := 0
	for total < len(b) {
		if time.Now().After(w.hardDeadline) {
			return total, fmt.Errorf("%w: %d of %d bytes written", ErrSendStalled, total, len(b))
		}
		end := total + sendWriteChunk
		if end > len(b) {
			end = len(b)
		}
		if err := w.conn.SetWriteDeadline(time.Now().Add(w.idle)); err != nil {
			return total, err
		}
		n, err := w.conn.Write(b[total:end])
		total += n
		if err != nil {
			var ne net.Error
			if n > 0 && errors.As(err, &ne) && ne.Timeout() {
				// Forward progress was made before the idle deadline fired.
				// Renew and continue — this is the whole point of #627.
				continue
			}
			return total, err
		}
	}
	return total, nil
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
	// TCP keepalive is a cheap backstop for detecting a silently-dead peer (the
	// primary detection is read/write errors + MSP SDOWN, #534).
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetKeepAlive(true)
		_ = tc.SetKeepAlivePeriod(15 * time.Second)
	}
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
//
// Send returns ErrPeerBusy — leaving the connection open and healthy — when
// another frame held the write slot for longer than the slot timeout. Callers
// on the shared broadcast tick must treat that as "alive, skip"; bulk senders
// must retry rather than tear the stream down (#627).
func (p *PeerConn) Send(frameType uint8, payload []byte) error {
	slot := p.slot()
	timer := time.NewTimer(p.slotTimeout())
	defer timer.Stop()
	select {
	case slot <- struct{}{}:
	case <-timer.C:
		return ErrPeerBusy
	}
	defer func() { <-slot }()

	p.mu.Lock()
	conn := p.conn
	closed := p.closed
	p.mu.Unlock()

	if conn == nil || closed {
		return ErrNotConnected
	}

	f := &mbp.Frame{
		Version:       0x01,
		Type:          frameType,
		PayloadLength: uint32(len(payload)),
		Payload:       payload,
	}
	w := &progressWriter{
		conn:         conn,
		idle:         p.idleTimeout(),
		hardDeadline: time.Now().Add(p.maxDuration()),
	}
	if err := mbp.WriteFrame(w, f); err != nil {
		// A stalled or failed write means the conn is dead or wedged — mark it
		// closed so IsConnected reports false and a restarted peer's new conn can
		// replace it via RegisterConnKind / discovery re-dial (#534). Only tear
		// down the conn we actually wrote to: Close or a re-register may have
		// swapped it while we held the slot.
		p.mu.Lock()
		if p.conn == conn {
			_ = p.conn.Close()
			p.conn = nil
			p.closed = true
		}
		p.mu.Unlock()
		return err
	}
	_ = conn.SetWriteDeadline(time.Time{}) // clear for subsequent reads/writes
	return nil
}

// maxBusyRetries bounds how many times a bulk frame waits behind another bulk
// transfer on the same connection. Each retry costs one slot timeout, so this
// is an outer bound of ~1 minute at the default. Past that the peer is not
// merely busy and the caller gives up so its supervisor can rebuild the stream.
const maxBusyRetries = 12

// onBusyRetry, when non-nil, is invoked after each rebuffed bulk send. Test
// seam: it lets a test release the write slot at a known retry count instead of
// racing a clock.
var onBusyRetry func(attempt int)

// sendBulk writes a bulk frame, tolerating a busy write slot.
//
// Exactly one bulk transfer can occupy a connection at a time (a post-join
// snapshot and a replication stream target the same PeerConn), and ErrPeerBusy
// means the other one holds it — the peer is alive. Tearing the transfer down
// there is the move that turned a lagging Lobe into a permanently-lagging one
// (#627), so retry rather than fail.
func sendBulk(ctx context.Context, p *PeerConn, frameType uint8, payload []byte, what string) error {
	for attempt := 0; ; attempt++ {
		err := p.Send(frameType, payload)
		if !errors.Is(err, ErrPeerBusy) {
			return err
		}
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if attempt >= maxBusyRetries {
			return fmt.Errorf("%s: peer %s write slot busy for %d attempts: %w",
				what, p.NodeID(), attempt+1, err)
		}
		slog.Warn("cluster: peer write slot busy, retrying",
			"peer", p.NodeID(), "what", what, "attempt", attempt+1)
		if onBusyRetry != nil {
			onBusyRetry(attempt + 1)
		}
	}
}

// Is reports whether this PeerConn currently wraps the given net.Conn (identity).
func (p *PeerConn) Is(conn net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.conn == conn
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
