package mbp

import (
	"context"
	"errors"
	"testing"
	"time"
)

// decodeResult is what one decodeCharged call came back with.
type decodeResult struct {
	n       int
	release func()
	err     error
}

func decodeAsync(ctx context.Context, data []byte) <-chan decodeResult {
	done := make(chan decodeResult, 1)
	go func() {
		payload, release, err := decodeCharged(ctx, data)
		done <- decodeResult{len(payload), release, err}
	}()
	return done
}

// TestDecodeCharged_WaitsForTheConnectionBudget: what a connection's frames
// decode to is charged to the connection until each handler is done, and a
// frame that would take the connection past its budget waits for room instead
// of being decoded alongside the others.
func TestDecodeCharged_WaitsForTheConnectionBudget(t *testing.T) {
	const budget = 1 << 20
	ctx := withDecodeBudget(context.Background(), budget)
	frame := zstdDeclared(t, 600*1024) // two of these do not fit in the budget together

	first := <-decodeAsync(ctx, frame)
	if first.err != nil || first.n != 600*1024 {
		t.Fatalf("first frame: %d bytes, err %v", first.n, first.err)
	}

	second := decodeAsync(ctx, frame)
	select {
	case r := <-second:
		t.Fatalf("the second frame was decoded (%d bytes, err %v) while the first still held the budget", r.n, r.err)
	case <-time.After(150 * time.Millisecond):
		// still waiting, as it should be
	}

	first.release()
	select {
	case r := <-second:
		if r.err != nil || r.n != 600*1024 {
			t.Fatalf("second frame after the first released: %d bytes, err %v", r.n, r.err)
		}
		r.release()
	case <-time.After(10 * time.Second):
		t.Fatal("the second frame never got the budget the first released")
	}

	// Everything was given back: a frame that needs the whole budget fits.
	whole := <-decodeAsync(ctx, zstdDeclared(t, budget))
	if whole.err != nil {
		t.Fatalf("a budget-sized frame after everything was released: %v", whole.err)
	}
	whole.release()
	whole.release() // releasing twice gives nothing back twice
	again := <-decodeAsync(ctx, zstdDeclared(t, budget))
	if again.err != nil {
		t.Fatalf("after a double release: %v", again.err)
	}
	again.release()
}

// TestDecodeCharged_MoreThanTheWholeBudgetIsRefused: a frame the budget could
// never admit is refused at once rather than left waiting for room that will
// not come.
func TestDecodeCharged_MoreThanTheWholeBudgetIsRefused(t *testing.T) {
	ctx := withDecodeBudget(context.Background(), 256*1024)
	select {
	case r := <-decodeAsync(ctx, zstdDeclared(t, 300*1024)):
		if !errors.Is(r.err, ErrDecodedPayloadTooLarge) {
			t.Fatalf("err = %v; want ErrDecodedPayloadTooLarge", r.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a frame larger than the whole budget is waiting for it")
	}
}

// TestDecodeCharged_AClosingConnectionFreesItsWaiters: a frame waiting for the
// budget is waiting on the connection's context, so it does not outlive the
// connection.
func TestDecodeCharged_AClosingConnectionFreesItsWaiters(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ctx = withDecodeBudget(ctx, 1<<20)
	frame := zstdDeclared(t, 600*1024)

	first := <-decodeAsync(ctx, frame)
	if first.err != nil {
		t.Fatalf("first frame: %v", first.err)
	}
	defer first.release()

	waiting := decodeAsync(ctx, frame)
	cancel()
	select {
	case r := <-waiting:
		if !errors.Is(r.err, context.Canceled) {
			t.Fatalf("err = %v; want the connection's cancellation", r.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the waiting frame outlived its connection")
	}
}

// TestDecodeCharged_NoBudgetStillHasTheFrameLimit: a frame dispatched with no
// connection budget in its context is uncharged, not unbounded.
func TestDecodeCharged_NoBudgetStillHasTheFrameLimit(t *testing.T) {
	_, _, err := decodeCharged(context.Background(), zstdDeclared(t, MaxPayloadSize+1))
	if !errors.Is(err, ErrDecodedPayloadTooLarge) {
		t.Fatalf("err = %v; want ErrDecodedPayloadTooLarge", err)
	}
}
