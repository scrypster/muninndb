package replication

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// pacedConn is a net.Conn that models the exact condition behind #627: a peer
// that keeps draining the socket, but slower than the sender pushes. Each Write
// call accepts perWrite bytes and then reports os.ErrDeadlineExceeded together
// with the bytes it did accept — precisely what net.TCPConn does when the
// receive window is full and the write deadline expires. The final Write, which
// needs no more room, succeeds.
//
// Nothing here sleeps or races a wall clock: "slow" is expressed as "the
// deadline fired again", which is the only thing the code under test can
// observe. The only wall-clock quantity in these tests is the outer bound, and
// bounding elapsed time is what that constant is FOR.
type pacedConn struct {
	mu       sync.Mutex
	deadline time.Time
	buf      bytes.Buffer
	frames   int
	cursor   int

	frameTgt   int
	reachedTgt chan struct{}
	tgtOnce    sync.Once

	// perWrite is how many bytes the peer absorbs before the window refills.
	// Zero models a wedged peer that accepts nothing at all.
	perWrite int

	// stall, when non-nil, blocks every Write until it is closed — used to hold
	// the write slot while another goroutine tries to send a control frame.
	stall chan struct{}

	deadlineSets atomic.Int64
	closed       chan struct{}
	closeOnce    sync.Once
}

func newPacedConn(perWrite, frameTarget int) *pacedConn {
	return &pacedConn{
		perWrite:   perWrite,
		frameTgt:   frameTarget,
		reachedTgt: make(chan struct{}),
		closed:     make(chan struct{}),
	}
}

func (c *pacedConn) Read(_ []byte) (int, error)        { <-c.closed; return 0, io.EOF }
func (c *pacedConn) LocalAddr() net.Addr               { return dummyAddr("local") }
func (c *pacedConn) RemoteAddr() net.Addr              { return dummyAddr("remote") }
func (c *pacedConn) SetDeadline(t time.Time) error     { return c.SetWriteDeadline(t) }
func (c *pacedConn) SetReadDeadline(_ time.Time) error { return nil }

func (c *pacedConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	c.deadline = t
	c.mu.Unlock()
	if !t.IsZero() {
		c.deadlineSets.Add(1)
	}
	return nil
}

func (c *pacedConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}

// Reached is closed once frameTgt complete MBP frames have been accepted.
func (c *pacedConn) Reached() <-chan struct{} { return c.reachedTgt }

func (c *pacedConn) Written() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Len()
}

func (c *pacedConn) FrameCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.frames
}

func (c *pacedConn) Write(p []byte) (int, error) {
	if c.stall != nil {
		select {
		case <-c.stall:
		case <-c.closed:
			return 0, net.ErrClosed
		}
	}
	select {
	case <-c.closed:
		return 0, net.ErrClosed
	default:
	}

	n := c.perWrite
	if n > len(p) {
		n = len(p)
	}
	if n > 0 {
		c.accept(p[:n])
	}
	if n == len(p) {
		return n, nil // the tail fits; no more room needed
	}
	c.mu.Lock()
	dl := c.deadline
	c.mu.Unlock()
	if dl.IsZero() {
		// No deadline set at all: a real conn would block indefinitely.
		<-c.closed
		return n, net.ErrClosed
	}
	return n, os.ErrDeadlineExceeded
}

// accept records bytes and counts how many complete MBP frames have landed.
// Frame counting is incremental (a running cursor, not a re-scan).
func (c *pacedConn) accept(b []byte) {
	c.mu.Lock()
	c.buf.Write(b)
	raw := c.buf.Bytes()
	for {
		if len(raw)-c.cursor < mbp.FramePrefixSize {
			break
		}
		payloadLen := int(binary.BigEndian.Uint32(raw[c.cursor+4 : c.cursor+8]))
		if len(raw)-c.cursor < mbp.FramePrefixSize+payloadLen {
			break
		}
		c.cursor += mbp.FramePrefixSize + payloadLen
		c.frames++
	}
	hit := c.frameTgt > 0 && c.frames >= c.frameTgt
	c.mu.Unlock()
	if hit {
		c.tgtOnce.Do(func() { close(c.reachedTgt) })
	}
}

// Frames decodes every complete frame accepted so far.
func (c *pacedConn) Frames() []*mbp.Frame {
	c.mu.Lock()
	raw := append([]byte(nil), c.buf.Bytes()...)
	c.mu.Unlock()
	r := bytes.NewReader(raw)
	var out []*mbp.Frame
	for {
		f, err := mbp.ReadFrame(r)
		if err != nil || f == nil {
			return out
		}
		out = append(out, f)
	}
}

// TestPeerConn_Send_SurvivesSlowButProgressingPeer is the #627 spiral in one
// frame. The receiver drains steadily but cannot absorb the whole frame inside
// one deadline window, so the deadline fires repeatedly with bytes accepted
// each time. That peer is slow, not wedged, and the send must complete.
//
// The pre-fix mechanism — one fixed deadline for the entire frame — fails here
// for any value of the constant, because the frame size, the link rate and the
// constant are three unrelated quantities. Raising 5s to 30s moves the cliff.
func TestPeerConn_Send_SurvivesSlowButProgressingPeer(t *testing.T) {
	const (
		payloadSize = 256 * 1024
		perWrite    = 8 * 1024
	)
	conn := newPacedConn(perWrite, 1)
	defer conn.Close()

	pc := &PeerConn{
		nodeID:   "slow-lobe",
		addr:     "paced",
		conn:     conn,
		sendIdle: 5 * time.Second,
		sendMax:  30 * time.Second,
		slotWait: time.Second,
	}

	wantBytes := mbp.FramePrefixSize + payloadSize
	if err := pc.Send(mbp.TypeReplEntry, bytes.Repeat([]byte{0xAB}, payloadSize)); err != nil {
		t.Fatalf("Send to a slow-but-progressing peer failed after %d of %d bytes: %v",
			conn.Written(), wantBytes, err)
	}

	if got := conn.Written(); got != wantBytes {
		t.Fatalf("bytes delivered: got %d, want %d", got, wantBytes)
	}
	if got := conn.FrameCount(); got != 1 {
		t.Fatalf("complete frames delivered: got %d, want 1", got)
	}
	if !pc.IsConnected() {
		t.Fatal("connection was torn down by a successful send")
	}
	if got := conn.deadlineSets.Load(); got < 2 {
		t.Fatalf("write deadline was set %d times; a progress-based bound must renew it", got)
	}
}

// TestPeerConn_Send_WedgedPeerStillFails guards the other side of the fix: a
// peer that accepts nothing at all must still be detected. Progress-based does
// not mean unbounded.
func TestPeerConn_Send_WedgedPeerStillFails(t *testing.T) {
	conn := newPacedConn(0, 1) // absorbs nothing
	defer conn.Close()

	pc := &PeerConn{
		nodeID:   "wedged-lobe",
		conn:     conn,
		sendIdle: 100 * time.Millisecond,
		sendMax:  30 * time.Second,
		slotWait: time.Second,
	}

	start := time.Now()
	err := pc.Send(mbp.TypeReplEntry, bytes.Repeat([]byte{0x01}, 4096))
	if err == nil {
		t.Fatal("expected a send failure against a peer that accepts nothing")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("wedged peer detected only after %v", elapsed)
	}
	if pc.IsConnected() {
		t.Fatal("a dead connection must be torn down so it can be re-dialed")
	}
}

// TestPeerConn_Send_OuterBoundStopsDribblingPeer is the adversarial case a
// purely progress-based deadline invites: a peer that accepts one token of data
// per window would hold the write slot forever, because every window ends in
// forward progress. sendMaxDuration is the outer bound that makes "forever"
// unrepresentable.
func TestPeerConn_Send_OuterBoundStopsDribblingPeer(t *testing.T) {
	conn := newPacedConn(1, 1) // one byte per window: a dribbler
	defer conn.Close()

	pc := &PeerConn{
		nodeID:   "dribbler",
		conn:     conn,
		sendIdle: time.Minute,
		sendMax:  20 * time.Millisecond,
		slotWait: time.Second,
	}

	start := time.Now()
	err := pc.Send(mbp.TypeReplEntry, bytes.Repeat([]byte{0x02}, 4<<20))
	elapsed := time.Since(start)

	if !errors.Is(err, ErrSendStalled) {
		t.Fatalf("dribbling peer after %v: got err %v, want ErrSendStalled", elapsed, err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("outer bound did not hold: send ran for %v with sendMax=20ms", elapsed)
	}
	if pc.IsConnected() {
		t.Fatal("a stalled connection must be torn down")
	}
}

// TestPeerConn_ControlSendDoesNotBlockBehindBulkWrite pins the reason the old
// fixed 5s bound existed at all: the shared MSP tick broadcasts to every peer
// sequentially, so it must never block for the duration of a bulk transfer.
// Now that a bulk frame may legitimately take minutes, a control frame that
// cannot get the write slot returns ErrPeerBusy — alive, skipped, connection
// untouched — instead of waiting behind it or killing the peer.
func TestPeerConn_ControlSendDoesNotBlockBehindBulkWrite(t *testing.T) {
	conn := newPacedConn(64, 1)
	conn.stall = make(chan struct{})
	defer conn.Close()

	pc := &PeerConn{
		nodeID:   "busy-lobe",
		conn:     conn,
		sendIdle: 30 * time.Second,
		sendMax:  30 * time.Second,
		slotWait: 50 * time.Millisecond,
	}

	inSlot := make(chan struct{})
	bulkDone := make(chan error, 1)
	go func() {
		close(inSlot)
		bulkDone <- pc.Send(mbp.TypeSnapChunk, bytes.Repeat([]byte{0x03}, 1<<20))
	}()
	<-inSlot
	// Wait until the bulk send is provably inside the write slot and blocked on
	// the peer: the first Write has been entered (stall is being waited on).
	waitForBlockedWriter(t, pc)

	start := time.Now()
	err := pc.Send(mbp.TypePing, []byte("hb"))
	elapsed := time.Since(start)

	if !errors.Is(err, ErrPeerBusy) {
		t.Fatalf("heartbeat behind a bulk write: got err %v after %v, want ErrPeerBusy", err, elapsed)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("heartbeat blocked for %v behind a bulk write", elapsed)
	}
	if !pc.IsConnected() {
		t.Fatal("a busy peer must not be marked dead")
	}

	conn.Close()
	close(conn.stall)
	<-bulkDone
}

// waitForBlockedWriter blocks until the peer's write slot is occupied. It polls
// the slot itself rather than a clock, so it cannot pass early on a fast
// machine or flake on a loaded one.
func waitForBlockedWriter(t *testing.T, p *PeerConn) {
	t.Helper()
	slot := p.slot()
	giveUp := time.Now().Add(5 * time.Second)
	for {
		select {
		case slot <- struct{}{}:
			<-slot // slot was free; the bulk send has not taken it yet
			if time.Now().After(giveUp) {
				t.Fatal("bulk send never occupied a write slot — there is no slot to be busy on")
			}
			runtime.Gosched()
		default:
			return // occupied
		}
	}
}

// TestSendBulk_DeliversAfterTheSlotFrees proves a bulk frame survives a busy
// write slot instead of killing the transfer. Exactly one bulk transfer can
// occupy a connection at a time (a post-join snapshot and a replication stream
// share the PeerConn), and treating "busy" as "dead" is how the #627 spiral
// restarted itself from the other end.
func TestSendBulk_DeliversAfterTheSlotFrees(t *testing.T) {
	conn := newPacedConn(1<<20, 1)
	defer conn.Close()

	pc := &PeerConn{
		nodeID:   "contended",
		conn:     conn,
		sendIdle: 5 * time.Second,
		sendMax:  30 * time.Second,
		slotWait: 5 * time.Millisecond,
	}

	// Occupy the write slot as another bulk transfer would.
	slot := pc.slot()
	slot <- struct{}{}

	// Release it once the sender has been rebuffed twice — a retry count, not a
	// clock, so this cannot pass early or flake under load.
	var releaseOnce sync.Once
	onBusyRetry = func(attempt int) {
		if attempt >= 2 {
			releaseOnce.Do(func() { <-slot })
		}
	}
	t.Cleanup(func() { onBusyRetry = nil })

	if err := sendBulk(context.Background(), pc, mbp.TypeReplEntry, []byte("payload"), "test"); err != nil {
		t.Fatalf("sendBulk behind a busy slot: %v", err)
	}
	if got := conn.FrameCount(); got != 1 {
		t.Fatalf("frames delivered: got %d, want 1 — the frame was dropped, not retried", got)
	}
}

// TestSendBulk_GivesUpOnAPermanentlyHeldSlot is the outer bound: retrying
// forever would be its own hang.
func TestSendBulk_GivesUpOnAPermanentlyHeldSlot(t *testing.T) {
	conn := newPacedConn(1<<20, 1)
	defer conn.Close()

	pc := &PeerConn{
		nodeID:   "wedged-slot",
		conn:     conn,
		sendIdle: 5 * time.Second,
		sendMax:  30 * time.Second,
		slotWait: time.Millisecond,
	}
	pc.slot() <- struct{}{} // never released

	var attempts atomic.Int64
	onBusyRetry = func(int) { attempts.Add(1) }
	t.Cleanup(func() { onBusyRetry = nil })

	err := sendBulk(context.Background(), pc, mbp.TypeReplEntry, []byte("payload"), "test")
	if !errors.Is(err, ErrPeerBusy) {
		t.Fatalf("permanently busy slot: got %v, want an error wrapping ErrPeerBusy", err)
	}
	// The bound is the point: it must have retried, and it must have stopped.
	if got := attempts.Load(); got != maxBusyRetries {
		t.Fatalf("busy retries: got %d, want exactly maxBusyRetries=%d (0 means it never retried at all)",
			got, maxBusyRetries)
	}
	if pc.IsConnected() != true {
		t.Fatal("a busy slot must not tear the connection down")
	}
}

// TestSendBulk_StopsOnContextCancel: shutdown must not wait out the retries.
func TestSendBulk_StopsOnContextCancel(t *testing.T) {
	conn := newPacedConn(1<<20, 1)
	defer conn.Close()

	pc := &PeerConn{
		nodeID:   "cancelled",
		conn:     conn,
		sendIdle: 5 * time.Second,
		sendMax:  30 * time.Second,
		slotWait: time.Millisecond,
	}
	pc.slot() <- struct{}{} // never released

	ctx, cancel := context.WithCancel(context.Background())
	onBusyRetry = func(int) { cancel() }
	t.Cleanup(func() { onBusyRetry = nil })

	if err := sendBulk(ctx, pc, mbp.TypeReplEntry, []byte("payload"), "test"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled sendBulk: got %v, want context.Canceled", err)
	}
}
