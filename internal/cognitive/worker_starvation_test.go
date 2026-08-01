package cognitive

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestWorker_ActiveSessionDoesNotStarveFlush is the RED repro for #764 D1.
//
// Run() used to call ticker.Reset on EVERY received item, which turned "flush
// at least every maxWait" into "flush after maxWait of SILENCE". A caller
// submitting faster than maxWait therefore held the flush off forever: the
// batch only landed once it reached batchSize (50) or the submissions stopped.
// For ContradictWorker that meant a declared contradiction was never detected
// while the agent kept working, and for ConfidenceWorker it meant the #747
// contradiction penalty arrived never rather than "one interval later".
//
// The assertion is deliberately one-sided and generous: with a 200ms interval
// and 2s of traffic there are ~10 flush opportunities, and under the bug there
// are exactly zero (the debounce is absolute, not a scheduling race), so this
// cannot flake in the failing direction.
func TestWorker_ActiveSessionDoesNotStarveFlush(t *testing.T) {
	const (
		interval = 200 * time.Millisecond
		duration = 2 * time.Second
		// Submit well inside the interval so the old unconditional
		// ticker.Reset never lets the timer elapse.
		submitEvery = 20 * time.Millisecond
	)

	// batchSize is far above the number of items this test submits, so a
	// size-triggered flush cannot mask the missing time-triggered one.
	const batchSize = 10000

	var mu sync.Mutex
	var flushed int
	w := NewWorker[int](batchSize, batchSize, interval, func(ctx context.Context, batch []int) error {
		mu.Lock()
		flushed += len(batch)
		mu.Unlock()
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = w.Run(ctx) }()

	deadline := time.Now().Add(duration)
	for i := 0; time.Now().Before(deadline); i++ {
		if i >= batchSize {
			t.Fatalf("test submitted enough items to trigger a size flush; lower the rate")
		}
		w.Submit(i)
		time.Sleep(submitEvery)
	}

	mu.Lock()
	got := flushed
	mu.Unlock()
	if got == 0 {
		t.Fatalf("RED (#764 D1): worker flushed nothing in %v of continuous submissions at a %v interval — the flush ticker is a debounce, not a deadline", duration, interval)
	}

	cancel()
	<-done
}

// TestWorker_IdleFlushStillFires pins the behaviour the ticker reset was
// there for in the first place: a single item on an otherwise silent worker
// must still flush within about one interval.
func TestWorker_IdleFlushStillFires(t *testing.T) {
	const interval = 100 * time.Millisecond

	flushed := make(chan int, 4)
	w := NewWorker[int](8, 50, interval, func(ctx context.Context, batch []int) error {
		flushed <- len(batch)
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = w.Run(ctx) }()

	w.Submit(1)
	select {
	case n := <-flushed:
		if n != 1 {
			t.Fatalf("flushed %d items, want 1", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("idle worker never flushed a single submitted item")
	}

	cancel()
	<-done
}
