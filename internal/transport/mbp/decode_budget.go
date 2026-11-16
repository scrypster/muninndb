package mbp

import (
	"context"
	"errors"

	"golang.org/x/sync/semaphore"
)

// connDecodeBudget is how many bytes of DECOMPRESSED payload one connection
// may have in flight at once: decoded and not yet handled.
//
// A connection runs up to maxConcurrentFrames frames at a time, and a
// compressed frame of a few kilobytes can decode to MaxPayloadSize, so without
// a budget one connection could hold maxConcurrentFrames × MaxPayloadSize —
// a gigabyte — for the price of a few hundred kilobytes sent. With it, a frame
// that would take the connection past the budget waits for the frames ahead of
// it to finish; the reader stops reading while they wait, and the peer is held
// back by TCP.
//
// Two frames' worth: a single frame at the limit always fits, and ordinary
// traffic (requests of a few kilobytes) never comes near it.
const connDecodeBudget = 2 * MaxPayloadSize

// decodeBudget is one connection's budget.
type decodeBudget struct {
	sem  *semaphore.Weighted
	size int64
}

type decodeBudgetKeyType struct{}

var decodeBudgetKey = decodeBudgetKeyType{}

func withDecodeBudget(ctx context.Context, size int64) context.Context {
	return context.WithValue(ctx, decodeBudgetKey, &decodeBudget{sem: semaphore.NewWeighted(size), size: size})
}

func (s *Server) connDecodeBudget() int64 {
	if s.decodeBudget > 0 {
		return s.decodeBudget
	}
	return connDecodeBudget
}

// decodeCharged decompresses one frame payload and charges what it decodes to
// against the connection's budget, waiting for room if the connection is at
// its budget. release gives the charge back; the caller calls it when the
// handler has finished with the payload.
//
// A context with no budget (a frame dispatched outside a connection, as unit
// tests do) is decoded uncharged — still within the per-frame limit.
func decodeCharged(ctx context.Context, data []byte) (payload []byte, release func(), err error) {
	budget, _ := ctx.Value(decodeBudgetKey).(*decodeBudget)
	if budget == nil {
		payload, err = decompressBounded(data, nil)
		return payload, func() {}, err
	}

	var held int64
	release = func() {
		if held > 0 {
			budget.sem.Release(held)
			held = 0
		}
	}
	reserve := func(n int) error {
		if held+int64(n) > budget.size {
			// It could never be admitted, however long it waited.
			return ErrDecodedPayloadTooLarge
		}
		if err := budget.sem.Acquire(ctx, int64(n)); err != nil {
			return err // the connection is going away
		}
		held += int64(n)
		return nil
	}
	payload, err = decompressBounded(data, reserve)
	if err != nil {
		release()
		return nil, func() {}, err
	}
	return payload, release, nil
}

// decompressionFailure is what the client is told. It names the two refusals
// a well-behaved client can act on and nothing else about the payload.
func decompressionFailure(err error) string {
	switch {
	case errors.Is(err, ErrDecodedPayloadTooLarge):
		return "decompression failed: the payload decodes to more than a frame may carry"
	case errors.Is(err, ErrNotOneZstdFrame):
		return "decompression failed: a compressed payload must be exactly one zstd frame"
	default:
		return "decompression failed"
	}
}
