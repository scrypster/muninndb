package mbp

import (
	"context"
	"encoding/binary"
	"errors"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// What one connection can make the server hold in replies and in requests,
// after HELLO (SEC-30).
//
// A reply is queued for its connection and written to the socket by one
// goroutine. Until this existed the queue held 256 frames of any size, the
// write had no deadline, and a reply that found the queue full was dropped
// without a word. So a client that sent requests and did not read the answers
// kept every reply in memory for as long as it stayed connected: one connection
// with no key held 192 MiB with sixteen frames, 183 MiB with two hundred
// ordinary recalls, and the arithmetic limit was (256 queued + 1 being written
// + 64 being handled) x 16 MiB, about 5 GiB. And a client that pipelined more
// than 256 requests before reading lost answers: 75 to 134 of 1,000.
//
// Four things bound it now.
//
// # Unread replies are counted, and bounded
//
// Every reply's payload is counted against its connection from the moment it
// is queued until it has been written, and replies larger than smallReply share
// connReplyBudget. Only two requests are answered with more than a few hundred
// bytes, READ and ACTIVATE, and those take their place in the budget BEFORE
// they are carried out: the handler reserves what the reply can be at most
// (replyBound) and waits for that much room. So the room is known to exist
// before any work is done, a reply is never built to be thrown away, and a
// connection's replies in memory, built or queued, never exceed the budget.
// The wait is where a slow client is held back: its requests are carried out
// as fast as it takes the answers, and every one is answered in full. A handler
// that waits holds nothing but its request, and what it waits for is given
// back by the writer alone, so nothing here can deadlock.
//
// Replies up to smallReply are always queued, without waiting. That is every
// acknowledgement and every error, so the answer to a write is never held
// back or replaced. Their number is bounded by the queue's length.
//
// A reply that turns out larger than was reserved for it, or a large reply
// from a request that reserved nothing, takes the extra room if it is there
// and is otherwise replaced by a short refusal. Neither happens with the
// bounds as they are; it is what keeps the budget true if a bound is wrong.
//
// # A connection that is not reading stops being read from
//
// The reader does not take the next frame off the socket while half the reply
// budget is taken, or while the queue could not hold an answer for every frame
// already being handled. Its requests then wait in the socket and TCP holds
// its sender back. The reader holds nothing while it waits.
//
// # Requests being handled are counted too
//
// The reader also waits while connRequestGate bytes of request payload are
// being handled. Without it a connection that really sends them can have
// maxConcurrentFrames frames of MaxPayloadSize in flight at once, a gigabyte.
//
// # A connection that takes nothing is closed
//
// The writer gives the peer replyStallTimeout to accept SOME bytes, and renews
// it every time any are accepted. It is a bound on making no progress, not on
// how long a frame takes: a reply of megabytes over a slow link is a slow
// client, and slow is not a failure. A fixed deadline per frame is the mistake
// the replication transport made and unmade (#627, peer_conn.go, whose
// progressWriter this follows): it cut every peer that fell behind, which then
// reconnected further behind. There is deliberately no outer bound per frame
// here. A peer that accepts a byte every few seconds keeps its connection, as
// an idle peer does, and what it holds meanwhile is bounded by the above.
const (
	// replyQueueLen is how many reply frames a connection's queue holds.
	replyQueueLen = 256

	// smallReply is the largest reply that is always queued, whatever is
	// unread. Acknowledgements and error frames are a few hundred bytes.
	smallReply = 4 * 1024

	// connReplyBudget is how many bytes of replies larger than smallReply one
	// connection may have queued and unwritten. One frame's worth: any single
	// reply the protocol can carry fits when nothing else is waiting. The
	// largest reply this server produces is a recall of 100 memories, under
	// 2 MiB before compression.
	connReplyBudget = MaxPayloadSize

	// connRequestGate is how many bytes of request payload may be being
	// handled before the reader waits. A frame is read whole before it is
	// counted, so what is in flight can exceed this by one frame. Ordinary
	// requests are a few kilobytes; maxConcurrentFrames of the largest write
	// the storage layer accepts is about one megabyte.
	connRequestGate = MaxPayloadSize / 4

	// replyStallTimeout is how long a peer may accept no bytes of a reply. It
	// is the REST server's WriteTimeout, which gives a client that long to take
	// a whole response; here it is that long to take any of one.
	replyStallTimeout = 15 * time.Second

	// replyWriteChunk is the granularity at which the stall deadline is renewed.
	replyWriteChunk = 64 * 1024

	// maxPingPayload is the largest PING payload answered. PONG repeats it, and
	// nothing needs more than a token or a timestamp repeated. It was the one
	// frame whose reply was as large as its sender chose, needing no key and
	// nothing stored.
	maxPingPayload = 1024

	// readReplyBound and activateItemBound are what a READ reply, and each
	// result of an ACTIVATE reply, are taken to be at most when room is
	// reserved for them. The storage layer holds a memory's content to 16 KiB
	// and its concept to 512 bytes (erf.MaxContentBytes, erf.MaxConceptBytes);
	// the rest of a result is ids, scores and short strings. These are
	// reservations, not limits: a reply that is larger is still sent if there
	// is room for the difference.
	readReplyBound    = 32 * 1024
	activateItemBound = 24 * 1024

	// defaultActivateResults is how many results an ACTIVATE that does not say
	// gets (the activation engine's default).
	defaultActivateResults = 20

	// connMemoryBound is the payload one established connection can make the
	// server account for at once: requests being handled, decompressed
	// payloads, and unread replies. It is what the listener's default
	// connection cap is derived from (connection_cap.go).
	connMemoryBound = (connRequestGate + MaxPayloadSize) + connDecodeBudget + (connReplyBudget + replyQueueLen*smallReply)

	// stallWarnInterval is how often, at most, closing a stalled connection is
	// logged.
	stallWarnInterval = 10 * time.Second
)

// msgUnreadReplies is what a request is told when its reply could not be kept.
const msgUnreadReplies = "the request was carried out, but its reply was not kept: this connection has too many replies it has not read. Read them, then ask again"

// msgReplyTooLarge is what a request is told when its reply is larger than a
// frame may carry.
const msgReplyTooLarge = "the reply is larger than a frame may carry: ask for fewer results"

// activateReplyBound is the room reserved for the reply to an ACTIVATE asking
// for maxResults results. It is never more than the whole budget: a request
// for more results than that would hold waits for the budget to be empty, and
// then runs alone.
func activateReplyBound(maxResults int) int {
	if maxResults <= 0 {
		maxResults = defaultActivateResults
	}
	if maxResults > connReplyBudget/activateItemBound {
		return connReplyBudget
	}
	return smallReply + maxResults*activateItemBound
}

// errReplyStalled is returned by the writer when the peer accepted nothing for
// replyStallTimeout.
var errReplyStalled = errors.New("peer accepted no reply bytes within the stall timeout")

// connFlow is one connection's account of what it is making the server hold.
type connFlow struct {
	mu   sync.Mutex
	wake *sync.Cond

	requestBytes int64 // payloads of frames read and still being handled
	largeBytes   int64 // replies over smallReply, unwritten, plus room reserved for replies not built yet
	smallBytes   int64 // replies up to smallReply, unwritten
	replyFrames  int   // replies queued and not yet written
	stopped      bool  // the reader must not wait any longer
	dead         bool  // nothing more will be written: nobody should wait, or queue

	// total, when set, is kept equal to the unwritten reply bytes of every
	// connection of a server (reservations are not in it).
	total *atomic.Int64
}

func newConnFlow(total *atomic.Int64) *connFlow {
	f := &connFlow{total: total}
	f.wake = sync.NewCond(&f.mu)
	return f
}

// waitToRead blocks until the connection may be read from again, and reports
// false if it was told to stop instead.
func (f *connFlow) waitToRead() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for !f.stopped &&
		(f.largeBytes >= connReplyBudget/2 ||
			f.replyFrames >= replyQueueLen-maxConcurrentFrames ||
			f.requestBytes >= connRequestGate) {
		f.wake.Wait()
	}
	return !f.stopped
}

// stop releases the reader if it is waiting, for good. Handlers still queue
// their answers.
func (f *connFlow) stop() {
	f.mu.Lock()
	f.stopped = true
	f.mu.Unlock()
	f.wake.Broadcast()
}

// abandon is stop for a connection nothing more will be written to: handlers
// waiting for room are released, and replies are no longer queued.
func (f *connFlow) abandon() {
	f.mu.Lock()
	f.stopped, f.dead = true, true
	f.mu.Unlock()
	f.wake.Broadcast()
}

// requestRead counts a frame that has been read and is about to be handled.
func (f *connFlow) requestRead(n int) {
	f.mu.Lock()
	f.requestBytes += int64(n)
	f.mu.Unlock()
}

// requestDone gives back what requestRead counted.
func (f *connFlow) requestDone(n int) {
	f.mu.Lock()
	f.requestBytes -= int64(n)
	f.mu.Unlock()
	f.wake.Broadcast()
}

// reserve takes room for a reply of at most n bytes that has not been built
// yet, waiting until there is that much, and reports whether it had to wait.
// ok is false if the connection was abandoned first, in which case nothing is
// reserved and the request need not be carried out for a reply's sake.
func (f *connFlow) reserve(n int) (waited, ok bool) {
	if n <= smallReply {
		return false, true
	}
	if n > connReplyBudget {
		n = connReplyBudget
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for !f.dead && f.largeBytes+int64(n) > connReplyBudget {
		waited = true
		f.wake.Wait()
	}
	if f.dead {
		return waited, false
	}
	f.largeBytes += int64(n)
	return waited, true
}

// unreserve gives back room that reserve took and no reply used.
func (f *connFlow) unreserve(n int) {
	if n <= smallReply {
		return
	}
	if n > connReplyBudget {
		n = connReplyBudget
	}
	f.mu.Lock()
	f.largeBytes -= int64(n)
	f.mu.Unlock()
	f.wake.Broadcast()
}

// admitReply counts a reply of n payload bytes as queued, and reports whether
// it may be. reserved is the room reserve took for it, or zero; all of it is
// settled here, whatever the answer. A reply no larger than smallReply always
// may be queued. A larger one may if it fits what was reserved for it, or the
// budget has room for the difference. It never waits.
func (f *connFlow) admitReply(n, reserved int) bool {
	if reserved <= smallReply {
		reserved = 0
	} else if reserved > connReplyBudget {
		reserved = connReplyBudget
	}
	f.mu.Lock()
	defer f.wake.Broadcast()
	defer f.mu.Unlock()
	f.largeBytes -= int64(reserved)
	if f.dead {
		return false
	}
	if n > smallReply {
		if f.largeBytes+int64(n) > connReplyBudget {
			return false
		}
		f.largeBytes += int64(n)
	} else {
		f.smallBytes += int64(n)
	}
	f.replyFrames++
	if f.total != nil {
		f.total.Add(int64(n))
	}
	return true
}

// replyGone gives back what admitReply counted: the reply has been written, or
// will never be.
func (f *connFlow) replyGone(n int) {
	f.mu.Lock()
	if n > smallReply {
		f.largeBytes -= int64(n)
	} else {
		f.smallBytes -= int64(n)
	}
	f.replyFrames--
	f.mu.Unlock()
	if f.total != nil {
		f.total.Add(-int64(n))
	}
	f.wake.Broadcast()
}

// replyRoom is the room one request reserved for its reply. It is carried in
// the request's context from the handler to enqueue, and settled exactly once:
// by the reply, or by the handler returning without one of that size.
type replyRoom struct {
	flow *connFlow
	n    int
	done atomic.Bool
}

type replyRoomKeyType struct{}

var replyRoomKey = replyRoomKeyType{}

// reserveReply takes room on the connection for a reply of at most n bytes,
// waiting for it, before the request is carried out. It returns the context to
// answer under and a function the handler defers to give the room back if no
// reply used it. It reports false when the request is not to be carried out:
// the connection was abandoned while it waited, so there is nobody to answer,
// or the session ended while it waited, which is answered here.
//
// The second matters because the wait can be long. A frame is checked against
// its session when it is dispatched (SEC-23); a READ that then waited for a
// slow client to take earlier replies would otherwise be carried out, and
// delivered, however long ago its key was revoked. So a request that waited is
// checked again before it runs.
//
// A request outside a connection reserves nothing.
func (s *Server) reserveReply(ctx context.Context, writeCh chan *Frame, corrID uint64, n int) (context.Context, func(), bool) {
	flow := flowOf(ctx)
	if flow == nil {
		return ctx, func() {}, true
	}
	waited, ok := flow.reserve(n)
	if !ok {
		return ctx, func() {}, false
	}
	if waited {
		if ended := s.sessionEnded(ctx); ended != "" {
			flow.unreserve(n)
			s.queueErrorFrame(ctx, writeCh, corrID, ErrAuthFailed, ended)
			endSession(ctx)
			return ctx, func() {}, false
		}
	}
	room := &replyRoom{flow: flow, n: n}
	return context.WithValue(ctx, replyRoomKey, room), func() {
		if room.done.CompareAndSwap(false, true) {
			flow.unreserve(n)
		}
	}, true
}

// reservedFor returns the room reserved for the reply being queued under ctx,
// and marks it settled. It is zero when nothing was reserved or it has been
// settled already (a request queues one reply).
func reservedFor(ctx context.Context) int {
	room, _ := ctx.Value(replyRoomKey).(*replyRoom)
	if room == nil || !room.done.CompareAndSwap(false, true) {
		return 0
	}
	return room.n
}

type connFlowKeyType struct{}

var connFlowKey = connFlowKeyType{}

func withConnFlow(ctx context.Context, f *connFlow) context.Context {
	return context.WithValue(ctx, connFlowKey, f)
}

// flowOf returns the connection's account, or nil for a frame dispatched
// outside a connection (as unit tests do), which is then not accounted.
func flowOf(ctx context.Context) *connFlow {
	f, _ := ctx.Value(connFlowKey).(*connFlow)
	return f
}

func (s *Server) stallTimeout() time.Duration {
	if s.replyStall > 0 {
		return s.replyStall
	}
	return replyStallTimeout
}

// progressWriter writes to a connection in bounded chunks and renews the write
// deadline whenever any bytes are accepted. A deadline that passes after some
// bytes went through is a slow peer and the write carries on; one that passes
// with none is a peer that is not reading.
//
// On a TLS connection a write that timed out cannot be resumed (crypto/tls
// makes the error permanent), so there a peer that takes less than one chunk in
// the stall timeout is closed where a plain connection would carry on.
type progressWriter struct {
	conn  net.Conn
	stall time.Duration
}

func (w *progressWriter) Write(b []byte) (int, error) {
	total := 0
	for total < len(b) {
		end := total + replyWriteChunk
		if end > len(b) {
			end = len(b)
		}
		if err := w.conn.SetWriteDeadline(time.Now().Add(w.stall)); err != nil {
			return total, err
		}
		n, err := w.conn.Write(b[total:end])
		total += n
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				if n > 0 {
					continue // some went through: slow, not gone
				}
				return total, errReplyStalled
			}
			return total, err
		}
	}
	return total, nil
}

// writeReply writes one frame. A small frame goes out in one write, built in
// scratch; a large one is written header first and then straight from its
// payload, so the writer does not hold a second copy of it.
func writeReply(w *progressWriter, scratch []byte, f *Frame) ([]byte, error) {
	if len(f.Payload) > MaxPayloadSize {
		return scratch, ErrPayloadTooLarge
	}
	var prefix [FramePrefixSize]byte
	prefix[0] = f.Version
	prefix[1] = f.Type
	binary.BigEndian.PutUint16(prefix[2:4], f.Flags)
	binary.BigEndian.PutUint32(prefix[4:8], uint32(len(f.Payload)))
	binary.BigEndian.PutUint64(prefix[8:16], f.CorrelationID)

	if len(f.Payload) <= replyWriteChunk-FramePrefixSize {
		scratch = append(scratch[:0], prefix[:]...)
		scratch = append(scratch, f.Payload...)
		_, err := w.Write(scratch)
		return scratch, err
	}
	if _, err := w.Write(prefix[:]); err != nil {
		return scratch, err
	}
	_, err := w.Write(f.Payload)
	return scratch, err
}

// writerLoop writes the connection's queued replies, in order, until the queue
// is closed. When a write fails, or the peer takes nothing for the stall
// timeout, it closes the connection, which stops the reader as well, and from
// then on only takes replies off the queue to let go of them.
func (s *Server) writerLoop(conn net.Conn, writeCh chan *Frame, errCh chan error, flow *connFlow) {
	w := &progressWriter{conn: conn, stall: s.stallTimeout()}
	var scratch []byte
	var failed error
	for frame := range writeCh {
		if failed == nil {
			var err error
			if scratch, err = writeReply(w, scratch, frame); err != nil {
				failed = err
				if errors.Is(err, errReplyStalled) {
					s.noteStalled(conn, w.stall)
				}
				_ = conn.Close()
				if flow != nil {
					flow.abandon()
				}
			}
		}
		if flow != nil {
			flow.replyGone(len(frame.Payload))
		}
	}
	errCh <- failed
}

// noteStalled counts a connection closed for taking none of its replies, and
// logs it, at most once per stallWarnInterval.
func (s *Server) noteStalled(conn net.Conn, stall time.Duration) {
	closed := s.stalledClosed.Add(1)
	now := time.Now().UnixNano()
	last := s.stallLastWarn.Load()
	if now-last < int64(stallWarnInterval) || !s.stallLastWarn.CompareAndSwap(last, now) {
		return
	}
	slog.Warn("mbp: closed a connection that accepted none of its replies",
		"remote", conn.RemoteAddr().String(), "for", stall.String(), "closed_so_far", closed)
}

// enqueue queues one reply frame for the connection's writer, counted against
// the connection (see the top of this file). A reply that may not be queued is
// replaced by a short refusal carrying the same correlation id.
func (s *Server) enqueue(ctx context.Context, writeCh chan *Frame, frame *Frame) {
	flow := flowOf(ctx)
	if flow == nil {
		// A frame dispatched outside a connection: the queue is whatever the
		// caller made it, and nothing is accounted.
		select {
		case writeCh <- frame:
		default:
		}
		return
	}
	reserved := reservedFor(ctx)
	if len(frame.Payload) > MaxPayloadSize {
		// The writer could not send it. Say so, in a frame it can send.
		flow.unreserve(reserved)
		s.enqueueRefusal(flow, writeCh, frame.CorrelationID, ErrMaxResultsExceeded, msgReplyTooLarge)
		return
	}
	if !flow.admitReply(len(frame.Payload), reserved) {
		s.enqueueRefusal(flow, writeCh, frame.CorrelationID, ErrRateLimited, msgUnreadReplies)
		return
	}
	s.send(flow, writeCh, frame)
}

// enqueueRefusal queues a short error frame in place of a reply that could not
// be. It is small, so it is always admitted unless the connection is abandoned.
func (s *Server) enqueueRefusal(flow *connFlow, writeCh chan *Frame, corrID uint64, code ErrorCode, message string) {
	frame, ok := errorFrame(corrID, code, message)
	if !ok || !flow.admitReply(len(frame.Payload), 0) {
		return
	}
	s.send(flow, writeCh, frame)
}

// send hands an admitted frame to the writer. The reader does not take a frame
// off the socket while the queue could not hold its answer, so the queue has
// room; if it ever did not, the frame is let go of and its count given back.
func (s *Server) send(flow *connFlow, writeCh chan *Frame, frame *Frame) {
	select {
	case writeCh <- frame:
	default:
		flow.replyGone(len(frame.Payload))
	}
}
