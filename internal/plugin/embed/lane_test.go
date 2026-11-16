package embed

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/scrypster/muninndb/internal/metrics"
	"github.com/scrypster/muninndb/internal/plugin"
)

// TestEmbedLane_QueuedAcquireHonoursContext pins the primitive the local
// embedder's lanes rest on: a caller queued behind a holder returns ctx's
// error when ctx ends, without the holder ever releasing — the property a
// sync.Mutex cannot have. Counters for the wait and the timeout move.
func TestEmbedLane_QueuedAcquireHonoursContext(t *testing.T) {
	l := newEmbedLane("query")
	if err := l.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	waits := testutil.ToFloat64(metrics.EmbedLaneWaitsTotal.WithLabelValues("query"))
	timeouts := testutil.ToFloat64(metrics.EmbedLaneTimeoutsTotal.WithLabelValues("query"))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- l.acquire(ctx) }()
	cancel() // the holder never releases; only ctx can end the wait
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("queued acquire returned %v, want an error wrapping context.Canceled", err)
		}
	case <-time.After(10 * time.Second): // watchdog only: failure detector, not a timing assertion
		t.Fatal("queued acquire did not return after its context ended — the lane ignores the caller's context")
	}
	if got := testutil.ToFloat64(metrics.EmbedLaneTimeoutsTotal.WithLabelValues("query")) - timeouts; got != 1 {
		t.Errorf("lane timeouts moved by %v, want 1", got)
	}
	// The wait counter can only have moved if the goroutine found the lane
	// held, which it must have (the holder never released).
	if got := testutil.ToFloat64(metrics.EmbedLaneWaitsTotal.WithLabelValues("query")) - waits; got != 1 {
		t.Errorf("lane waits moved by %v, want 1", got)
	}

	l.release()
	if err := l.acquire(context.Background()); err != nil {
		t.Fatalf("lane not reusable after release: %v", err)
	}
	l.release()
}

// TestCreateRateLimiter_LocalIsUnlimited: the bundled in-process model has no
// remote quota, and a shared token bucket would let background batch calls
// starve recall's query embed — the coupling the lanes exist to remove.
func TestCreateRateLimiter_LocalIsUnlimited(t *testing.T) {
	s := &EmbedService{}
	if lim := s.createRateLimiter(plugin.SchemeLocal, nil); lim != nil {
		t.Fatalf("local embedder got a rate limiter (%+v); want none", lim)
	}
	if lim := s.createRateLimiter(plugin.SchemeOpenAI, nil); lim == nil {
		t.Fatal("remote embedder lost its rate limiter")
	}
}
