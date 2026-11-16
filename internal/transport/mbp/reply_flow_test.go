package mbp

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// returnsWithin reports whether f returns within d.
func returnsWithin(d time.Duration, f func()) bool {
	done := make(chan struct{})
	go func() { f(); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// TestConnFlow_SmallRepliesAreAlwaysQueued: acknowledgements and errors are
// never held back or replaced, whatever else is unread. The answer to a write
// is one of them.
func TestConnFlow_SmallRepliesAreAlwaysQueued(t *testing.T) {
	var total atomic.Int64
	f := newConnFlow(&total)
	if !f.admitReply(connReplyBudget, 0) {
		t.Fatal("a reply the size of the whole budget was refused with nothing else unread")
	}
	if f.admitReply(smallReply+1, 0) {
		t.Error("a large reply was queued with the budget full")
	}
	for i := 0; i < 10; i++ {
		if !f.admitReply(smallReply, 0) {
			t.Fatalf("small reply %d was refused", i)
		}
	}
	if got, want := total.Load(), int64(connReplyBudget+10*smallReply); got != want {
		t.Errorf("unwritten bytes = %d; want %d", got, want)
	}
	f.replyGone(connReplyBudget)
	for i := 0; i < 10; i++ {
		f.replyGone(smallReply)
	}
	if total.Load() != 0 || f.largeBytes != 0 || f.smallBytes != 0 || f.replyFrames != 0 {
		t.Errorf("after everything was written: total %d, large %d, small %d, frames %d; want zeros", total.Load(), f.largeBytes, f.smallBytes, f.replyFrames)
	}
}

// TestConnFlow_ReserveWaitsForRoomAndHoldsTheBudget: room for a reply is taken
// before the reply exists, and a request that would not fit waits. What is
// reserved and what is queued never add up to more than the budget.
func TestConnFlow_ReserveWaitsForRoomAndHoldsTheBudget(t *testing.T) {
	f := newConnFlow(nil)
	const each = connReplyBudget / 4
	for i := 0; i < 4; i++ {
		if waited, ok := f.reserve(each); !ok || waited {
			t.Fatalf("reservation %d: waited %v, ok %v; there was room", i, waited, ok)
		}
	}
	// A fifth has to wait...
	got := make(chan bool, 1)
	go func() { waited, ok := f.reserve(each); got <- waited && ok }()
	select {
	case <-got:
		t.Fatal("a fifth reservation was granted past the budget")
	case <-time.After(100 * time.Millisecond):
	}
	// ...until one of the four becomes a reply smaller than its reservation,
	// and that reply is written.
	if !f.admitReply(each/2, each) {
		t.Fatal("a reply smaller than its reservation was refused")
	}
	select {
	case <-got:
		t.Fatal("the fifth was granted while three reservations and half of one were still held")
	case <-time.After(100 * time.Millisecond):
	}
	f.replyGone(each / 2)
	select {
	case ok := <-got:
		if !ok {
			t.Error("the fifth reservation was not granted after waiting")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the fifth reservation was still waiting after room was given back")
	}
	if f.largeBytes > connReplyBudget {
		t.Errorf("%d bytes reserved or queued; the budget is %d", f.largeBytes, connReplyBudget)
	}
}

// TestConnFlow_AReplyLargerThanItsReservation takes the difference if there is
// room and is refused, without waiting, if there is not: the reservation is a
// bound that is expected to hold, and the budget holds if it does not.
func TestConnFlow_AReplyLargerThanItsReservation(t *testing.T) {
	f := newConnFlow(nil)
	f.reserve(connReplyBudget / 2)
	if !f.admitReply(connReplyBudget, connReplyBudget/2) {
		t.Error("a reply larger than its reservation was refused although the budget had room for it")
	}
	f.replyGone(connReplyBudget)

	f.reserve(connReplyBudget / 2)
	f.reserve(connReplyBudget / 2)
	if !returnsWithin(time.Second, func() {
		if f.admitReply(connReplyBudget, connReplyBudget/2) {
			t.Error("a reply was queued past the budget")
		}
	}) {
		t.Fatal("admitReply waited")
	}
	if f.largeBytes != connReplyBudget/2 {
		t.Errorf("after the refusal %d bytes are held; want only the other reservation, %d", f.largeBytes, connReplyBudget/2)
	}
}

// TestConnFlow_AbandonReleasesEveryone: once nothing more will be written,
// nobody waits: not a handler waiting for room, not the reader.
func TestConnFlow_AbandonReleasesEveryone(t *testing.T) {
	f := newConnFlow(nil)
	f.reserve(connReplyBudget)

	handler := make(chan bool, 1)
	go func() { _, ok := f.reserve(connReplyBudget / 2); handler <- ok }()
	reader := make(chan bool, 1)
	go func() { reader <- f.waitToRead() }()
	select {
	case <-handler:
		t.Fatal("a reservation was granted past the budget")
	case <-reader:
		t.Fatal("the reader was let through with the budget taken")
	case <-time.After(100 * time.Millisecond):
	}

	f.abandon()
	for name, ch := range map[string]chan bool{"the waiting handler": handler, "the reader": reader} {
		select {
		case ok := <-ch:
			if ok {
				t.Errorf("%s was told to carry on after the connection was abandoned", name)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s was still waiting after the connection was abandoned", name)
		}
	}
	if f.admitReply(10, 0) {
		t.Error("a reply was queued on an abandoned connection")
	}
}

// TestConnFlow_TheReaderWaitsForEachOfItsThreeReasons, and for nothing else.
func TestConnFlow_TheReaderWaitsForEachOfItsThreeReasons(t *testing.T) {
	blocked := func(f *connFlow) bool { return !returnsWithin(100*time.Millisecond, func() { f.waitToRead() }) }

	f := newConnFlow(nil)
	if blocked(f) {
		t.Fatal("the reader waits on an idle connection")
	}

	f.admitReply(connReplyBudget/2, 0)
	if !blocked(f) {
		t.Error("the reader reads on with half the reply budget unread")
	}
	f.replyGone(connReplyBudget / 2)

	f = newConnFlow(nil)
	for i := 0; i < replyQueueLen-maxConcurrentFrames; i++ {
		f.admitReply(1, 0)
	}
	if !blocked(f) {
		t.Error("the reader reads on when the queue could not hold an answer for every frame in flight")
	}

	f = newConnFlow(nil)
	f.requestRead(connRequestGate)
	if !blocked(f) {
		t.Error("the reader reads on with the request gate reached")
	}
	f.requestDone(connRequestGate)
	if blocked(f) {
		t.Error("the reader still waits after the requests were handled")
	}

	// stop lets the reader go, and handlers still queue their answers.
	f = newConnFlow(nil)
	f.requestRead(connRequestGate)
	f.stop()
	if f.waitToRead() {
		t.Error("a stopped reader was told to read")
	}
	if !f.admitReply(10, 0) {
		t.Error("a reply was refused after stop: the answers to frames in flight are still owed")
	}
}

// TestReserveReply_IsSettledExactlyOnce: the room a request reserved is given
// back once, by its reply or by the handler returning, never both.
func TestReserveReply_IsSettledExactlyOnce(t *testing.T) {
	s := &Server{}
	writeCh := make(chan *Frame, 4)

	t.Run("by the reply", func(t *testing.T) {
		f := newConnFlow(nil)
		ctx, giveBack, ok := s.reserveReply(withConnFlow(context.Background(), f), writeCh, 1, 1<<20)
		if !ok || f.largeBytes != 1<<20 {
			t.Fatalf("ok %v, held %d", ok, f.largeBytes)
		}
		s.enqueue(ctx, writeCh, &Frame{Payload: make([]byte, 300<<10)})
		giveBack()
		if f.largeBytes != 300<<10 {
			t.Errorf("held %d after a 300 KiB reply used a 1 MiB reservation; want the reply's 300 KiB", f.largeBytes)
		}
	})
	t.Run("by the handler, when the answer was a short error", func(t *testing.T) {
		f := newConnFlow(nil)
		ctx, giveBack, _ := s.reserveReply(withConnFlow(context.Background(), f), writeCh, 2, 1<<20)
		s.queueErrorFrame(ctx, writeCh, 2, ErrEngramNotFound, "not found")
		giveBack()
		giveBack()
		if f.largeBytes != 0 {
			t.Errorf("held %d after the request ended; want nothing", f.largeBytes)
		}
	})
	t.Run("by the handler, when it queued nothing", func(t *testing.T) {
		f := newConnFlow(nil)
		_, giveBack, _ := s.reserveReply(withConnFlow(context.Background(), f), writeCh, 3, 1<<20)
		giveBack()
		if f.largeBytes != 0 {
			t.Errorf("held %d; want nothing", f.largeBytes)
		}
	})
	t.Run("outside a connection nothing is reserved", func(t *testing.T) {
		if _, _, ok := s.reserveReply(context.Background(), writeCh, 4, 1<<20); !ok {
			t.Error("a request outside a connection was told not to run")
		}
	})
}

// TestEnqueue_AReplyTooLargeForAFrameIsRefusedAsThat: the writer cannot send a
// reply larger than a frame may carry. The request is told so, in a frame the
// writer can send, and the connection carries on.
func TestEnqueue_AReplyTooLargeForAFrameIsRefusedAsThat(t *testing.T) {
	s := &Server{}
	f := newConnFlow(nil)
	writeCh := make(chan *Frame, 4)
	ctx, giveBack, _ := s.reserveReply(withConnFlow(context.Background(), f), writeCh, 9, connReplyBudget)
	s.enqueue(ctx, writeCh, &Frame{CorrelationID: 9, Payload: make([]byte, MaxPayloadSize+1)})
	giveBack()

	got := <-writeCh
	var ep ErrorPayload
	if got.Type != TypeError || DecodeMsgpack(got.Payload, &ep) != nil || ep.Code != ErrMaxResultsExceeded || got.CorrelationID != 9 {
		t.Errorf("answered with frame 0x%02X code %d for request %d; want error %d for request 9", got.Type, ep.Code, got.CorrelationID, ErrMaxResultsExceeded)
	}
	if f.largeBytes != 0 {
		t.Errorf("held %d after the refusal; want nothing", f.largeBytes)
	}
}
