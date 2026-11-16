package mbp

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

// budgetCtx returns a connection context with a budget of the given size, the
// budget itself, and the server-wide total it reports into.
func budgetCtx(size int64) (context.Context, *decodeBudget, *atomic.Int64) {
	total := new(atomic.Int64)
	ctx := withDecodeBudget(context.Background(), size, total)
	return ctx, ctx.Value(decodeBudgetKey).(*decodeBudget), total
}

// mustBeEmpty fails unless nothing at all is charged to the budget.
func mustBeEmpty(t *testing.T, when string, b *decodeBudget, total *atomic.Int64) {
	t.Helper()
	if got := b.inUse.Load(); got != 0 {
		t.Errorf("%s: %d bytes are still charged to the connection", when, got)
	}
	if got := total.Load(); got != 0 {
		t.Errorf("%s: the server-wide total is %d, not 0", when, got)
	}
}

// TestDecodeBudget_AFrameThatDoesNotFitIsRefusedAtOnce: what a connection's
// frames decode to is charged to the connection until each handler is done,
// and a frame that would take the connection past its budget is refused — it
// does not wait.
func TestDecodeBudget_AFrameThatDoesNotFitIsRefusedAtOnce(t *testing.T) {
	const size = 1 << 20
	ctx, budget, total := budgetCtx(size)
	frame := zstdDeclared(t, 600*1024) // two of these do not fit together

	first, releaseFirst, err := decodeCharged(ctx, frame)
	if err != nil || len(first) != 600*1024 {
		t.Fatalf("first frame: %d bytes, err %v", len(first), err)
	}
	if got := budget.inUse.Load(); got != 600*1024 {
		t.Errorf("after the first frame %d bytes are charged; want exactly what it decoded to", got)
	}

	done := make(chan error, 1)
	go func() {
		_, _, err := decodeCharged(ctx, frame)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrDecodeBudgetExhausted) {
			t.Fatalf("second frame: err = %v; want ErrDecodeBudgetExhausted", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the second frame is waiting for the budget; it must be refused, not held")
	}
	if got := budget.inUse.Load(); got != 600*1024 {
		t.Errorf("the refused frame changed the charge to %d", got)
	}

	releaseFirst()
	mustBeEmpty(t, "after the first frame was released", budget, total)

	// Room again: the same frame now fits, and so does one the size of the
	// whole budget once that is released too.
	_, release, err := decodeCharged(ctx, frame)
	if err != nil {
		t.Fatalf("a frame after the budget was freed: %v", err)
	}
	release()
	release() // releasing twice gives nothing back twice
	mustBeEmpty(t, "after a double release", budget, total)
	_, release, err = decodeCharged(ctx, zstdDeclared(t, size))
	if err != nil {
		t.Fatalf("a budget-sized frame with nothing else charged: %v", err)
	}
	release()
	mustBeEmpty(t, "at the end", budget, total)
}

// TestDecodeBudget_NothingStaysChargedOnAnyPath walks every way a compressed
// frame can end — served, refused, or failed part way — and requires the
// connection's budget, and the server-wide total, to be back at zero after
// each. A charge that leaked would be room the connection never got back.
func TestDecodeBudget_NothingStaysChargedOnAnyPath(t *testing.T) {
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedFastest))
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	pattern := func(n int) []byte {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte(i*7 + i/251)
		}
		return b
	}
	corrupt := func(frame []byte) []byte {
		out := append([]byte{}, frame...)
		var hdr zstd.Header
		if err := hdr.Decode(out); err != nil {
			t.Fatal(err)
		}
		out[hdr.HeaderSize+3+40] ^= 0xFF // inside the first block's body
		return out
	}
	declared := enc.EncodeAll(pattern(200_000), nil)
	streamedSmall := zstdUndeclared(t, pattern(50_000))
	streamedBig := zstdUndeclared(t, pattern(3*testMiB)) // needs the limit-sized buffer

	const big = 2 * MaxPayloadSize // the production budget
	cases := []struct {
		name   string
		size   int64
		frame  []byte
		wantOK bool
		// wantHeld is what a served frame must be charged while it is handled.
		wantHeld int64
		wantErr  error
	}{
		{"declared, served", big, declared, true, 200_000, nil},
		{"streamed and small, served", big, streamedSmall, true, 50_000, nil},
		{"streamed and large, served", big, streamedBig, true, 3 * testMiB, nil},
		// A budget of exactly one frame: the large charge fits only because the
		// small one is given back before it is taken, never held alongside it.
		{"streamed and large, in a budget of exactly one frame", MaxPayloadSize, streamedBig, true, 3 * testMiB, nil},
		{"declared, corrupt", big, corrupt(declared), false, 0, nil},
		{"streamed and small, corrupt", big, corrupt(streamedSmall), false, 0, nil},
		{"streamed and large, corrupt", big, corrupt(streamedBig), false, 0, nil},
		{"declared over the frame limit", big, zstdDeclared(t, MaxPayloadSize+1), false, 0, ErrDecodedPayloadTooLarge},
		{"streamed past the frame limit", big, zstdUndeclaredZeros(t, MaxPayloadSize+testMiB), false, 0, ErrDecodedPayloadTooLarge},
		{"two frames in one payload", big, append(append([]byte{}, declared...), declared...), false, 0, ErrNotOneZstdFrame},
		{"not zstd at all", big, []byte("this is not a zstd frame at all, it is a sentence"), false, 0, nil},
		{"declared, larger than the whole budget", 100_000, declared, false, 0, ErrDecodedPayloadTooLarge},
		// The budget holds the first scratch buffer but not the large one.
		{"streamed and large, budget too small for the large buffer", testMiB, streamedBig, false, 0, ErrDecodedPayloadTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, budget, total := budgetCtx(tc.size)
			payload, release, err := decodeCharged(ctx, tc.frame)
			switch {
			case tc.wantOK && err != nil:
				t.Fatalf("err = %v; want it served", err)
			case !tc.wantOK && err == nil:
				t.Fatalf("served (%d bytes); want it refused", len(payload))
			case tc.wantErr != nil && !errors.Is(err, tc.wantErr):
				t.Errorf("err = %v; want %v", err, tc.wantErr)
			}
			if tc.wantOK {
				if got := budget.inUse.Load(); got != tc.wantHeld {
					t.Errorf("while handled, %d bytes are charged; want %d, what it decoded to", got, tc.wantHeld)
				}
			}
			release()
			mustBeEmpty(t, "afterwards", budget, total)
		})
	}

	// And when the budget is already taken by another frame: refused, and the
	// other frame's charge is untouched.
	t.Run("refused for want of room", func(t *testing.T) {
		for name, frame := range map[string][]byte{"declared": declared, "streamed and small": streamedSmall, "streamed and large": streamedBig} {
			ctx, budget, total := budgetCtx(big)
			if !budget.tryCharge(big - 10_000) { // another frame holds nearly all of it
				t.Fatal("could not set the budget up")
			}
			_, release, err := decodeCharged(ctx, frame)
			if !errors.Is(err, ErrDecodeBudgetExhausted) {
				t.Errorf("%s: err = %v; want ErrDecodeBudgetExhausted", name, err)
			}
			release()
			if got := budget.inUse.Load(); got != big-10_000 {
				t.Errorf("%s: the other frame's charge is now %d; want it untouched", name, got)
			}
			budget.give(big - 10_000)
			mustBeEmpty(t, name, budget, total)
		}
	})
}

// TestDecodeBudget_ManyFramesAtOnceNeverWedge is the deadlock pin at the level
// of the budget itself: far more frames than fit, all at once, all of the kind
// that takes a small charge first and a large one after. Every one of them
// must come back — served or refused — and when they have all finished the
// budget must be empty.
//
// When a frame could hold its small charge while waiting for the large one,
// this never finished: the small charges filled the budget and no frame could
// take the large one.
func TestDecodeBudget_ManyFramesAtOnceNeverWedge(t *testing.T) {
	ctx, budget, total := budgetCtx(2 * MaxPayloadSize)
	frame := zstdUndeclaredZeros(t, testMiB) // past the scratch buffer: needs the large charge

	const frames = 2 * maxConcurrentFrames
	var served, refused atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < frames; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			payload, release, err := decodeCharged(ctx, frame)
			switch {
			case err == nil && len(payload) == testMiB:
				served.Add(1)
			case errors.Is(err, ErrDecodeBudgetExhausted):
				refused.Add(1)
			default:
				t.Errorf("a frame came back with %d bytes and err %v", len(payload), err)
			}
			release()
		}()
	}
	close(start)

	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(60 * time.Second):
		t.Fatalf("after 60s only %d of %d frames had come back (%d served, %d refused): the rest are wedged",
			served.Load()+refused.Load(), frames, served.Load(), refused.Load())
	}
	t.Logf("%d frames at once: %d served, %d refused for want of room", frames, served.Load(), refused.Load())
	if served.Load() == 0 {
		t.Errorf("no frame was served at all")
	}
	mustBeEmpty(t, "after every frame finished", budget, total)
}

// TestDecodeBudget_ConnectionsDoNotShareOne: each connection has its own
// budget. A connection that has filled its own does nothing to another's.
func TestDecodeBudget_ConnectionsDoNotShareOne(t *testing.T) {
	total := new(atomic.Int64)
	a := withDecodeBudget(context.Background(), 1<<20, total)
	b := withDecodeBudget(context.Background(), 1<<20, total)
	frame := zstdDeclared(t, 600*1024)

	_, releaseA, err := decodeCharged(a, frame)
	if err != nil {
		t.Fatalf("connection A: %v", err)
	}
	if _, _, err := decodeCharged(a, frame); !errors.Is(err, ErrDecodeBudgetExhausted) {
		t.Fatalf("connection A's second frame: err = %v; want it refused", err)
	}
	_, releaseB, err := decodeCharged(b, frame)
	if err != nil {
		t.Fatalf("connection B was refused (%v) because connection A is at its budget", err)
	}
	if got := total.Load(); got != 2*600*1024 {
		t.Errorf("server-wide total = %d; want both connections' charges", got)
	}
	releaseA()
	releaseB()
	if got := total.Load(); got != 0 {
		t.Errorf("server-wide total = %d after both released", got)
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
