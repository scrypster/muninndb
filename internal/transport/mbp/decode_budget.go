package mbp

import (
	"context"
	"errors"
	"sync/atomic"
)

// connDecodeBudget is how many bytes of DECOMPRESSED payload one connection
// may have in flight at once: decoded and not yet handled.
//
// A connection runs up to maxConcurrentFrames frames at a time, and a
// compressed frame of a few kilobytes can decode to MaxPayloadSize, so without
// a budget one connection could hold maxConcurrentFrames × MaxPayloadSize —
// a gigabyte — for the price of a few hundred kilobytes sent.
//
// Two frames' worth: a single frame at the limit always fits, and ordinary
// traffic (requests of a few kilobytes) never comes near it.
//
// # Nothing waits for the budget
//
// A frame that does not fit is refused at once with an error frame; it does
// not queue. That is deliberate, and it is what an earlier version of this got
// wrong: there, a frame could hold a small first charge while it waited for
// room to take a larger one. Enough frames doing that at once — one burst from
// one connection — held the whole budget between them in small charges, none
// could take the large one, and none gave anything back. Every frame of the
// connection hung, and since the reader was waiting on them, the connection
// and its goroutines were never released, even after the client had gone.
//
// With no waiting there is nothing to deadlock, and a frame holds a charge
// only while it is running. The price is that a client which really does have
// more than the budget in flight is told to retry rather than held back; no
// request the server accepts is large enough for that to happen.
//
// # Per connection, not global
//
// Each connection has its own budget, so what one connection does to its
// budget is invisible to every other: it can get only its own frames refused.
// One shared budget would cap the server's total, but it would also hand any
// single connection — an unauthenticated one included — the means to have
// everyone else's compressed frames refused by keeping the budget full. The
// total across connections is therefore bounded only by the number of
// connections, which nothing limits today (SEC-21 residual).
const connDecodeBudget = 2 * MaxPayloadSize

// ErrDecodeBudgetExhausted is returned when a compressed frame's decoded
// payload does not fit in what its connection has left.
var ErrDecodeBudgetExhausted = errors.New("too much decompressed payload in flight on this connection")

// decodeBudget is one connection's budget. All it holds is a count; it never
// blocks.
type decodeBudget struct {
	size  int64
	inUse atomic.Int64
	// total, when set, is kept equal to the sum of inUse over every connection
	// of a server.
	total *atomic.Int64
}

// tryCharge takes n bytes if they fit, and reports whether they did.
func (b *decodeBudget) tryCharge(n int64) bool {
	for {
		cur := b.inUse.Load()
		if cur+n > b.size {
			return false
		}
		if b.inUse.CompareAndSwap(cur, cur+n) {
			if b.total != nil {
				b.total.Add(n)
			}
			return true
		}
	}
}

func (b *decodeBudget) give(n int64) {
	b.inUse.Add(-n)
	if b.total != nil {
		b.total.Add(-n)
	}
}

// frameCharge is what ONE frame holds of its connection's budget. It
// implements decodeCharger. A nil budget charges nothing.
//
// A frameCharge is used by one goroutine, the frame's own.
type frameCharge struct {
	budget *decodeBudget
	held   int64
}

// hold makes the frame's charge exactly n bytes. What the frame held is given
// back FIRST and n is then taken in one step, so a frame never holds part of
// the budget while it asks for more.
func (c *frameCharge) hold(n int) error {
	c.release()
	if c.budget == nil {
		return nil
	}
	if int64(n) > c.budget.size {
		return ErrDecodedPayloadTooLarge // it could never fit
	}
	if !c.budget.tryCharge(int64(n)) {
		return ErrDecodeBudgetExhausted
	}
	c.held = int64(n)
	return nil
}

// keep lowers the charge to n bytes. It only ever gives back, so it cannot
// fail.
func (c *frameCharge) keep(n int) {
	if c.budget == nil || int64(n) >= c.held {
		return
	}
	c.budget.give(c.held - int64(n))
	c.held = int64(n)
}

// release gives back everything the frame holds. It is safe to call again.
func (c *frameCharge) release() {
	if c.budget != nil && c.held > 0 {
		c.budget.give(c.held)
	}
	c.held = 0
}

type decodeBudgetKeyType struct{}

var decodeBudgetKey = decodeBudgetKeyType{}

func withDecodeBudget(ctx context.Context, size int64, total *atomic.Int64) context.Context {
	return context.WithValue(ctx, decodeBudgetKey, &decodeBudget{size: size, total: total})
}

func (s *Server) connDecodeBudget() int64 {
	if s.decodeBudget > 0 {
		return s.decodeBudget
	}
	return connDecodeBudget
}

// decodeCharged decompresses one frame payload and charges what it decodes to
// against the connection's budget. release gives the charge back; the caller
// calls it when the handler has finished with the payload. On an error nothing
// is left charged.
//
// A context with no budget (a frame dispatched outside a connection, as unit
// tests do) is decoded uncharged — still within the per-frame limit.
func decodeCharged(ctx context.Context, data []byte) (payload []byte, release func(), err error) {
	budget, _ := ctx.Value(decodeBudgetKey).(*decodeBudget)
	charge := &frameCharge{budget: budget}
	payload, err = decompressBounded(data, charge)
	if err != nil {
		charge.release()
		return nil, func() {}, err
	}
	return payload, charge.release, nil
}

// decompressionFailure is what the client is told when a compressed frame is
// not served: the error code and the message. It names the refusals a
// well-behaved client can act on and says nothing else about the payload.
func decompressionFailure(err error) (ErrorCode, string) {
	switch {
	case errors.Is(err, ErrDecodeBudgetExhausted):
		return ErrRateLimited, "too much compressed payload in flight on this connection: retry once earlier requests have been answered"
	case errors.Is(err, ErrDecodedPayloadTooLarge):
		return ErrInternal, "decompression failed: the payload decodes to more than a frame may carry"
	case errors.Is(err, ErrNotOneZstdFrame):
		return ErrInternal, "decompression failed: a compressed payload must be exactly one zstd frame"
	default:
		return ErrInternal, "decompression failed"
	}
}
