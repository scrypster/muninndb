package embed

import (
	"context"
	"fmt"
	"time"

	"github.com/scrypster/muninndb/internal/metrics"
)

// embedLane admits one call at a time and lets a queued caller give up when
// its context ends. It replaces a sync.Mutex because a mutex cannot be
// abandoned: a caller blocked in Lock() waits for the holder however long the
// holder takes, whatever its own deadline says — which is how a 5 ms-deadline
// query embed used to return, successfully, 2.4 s later behind a batch.
type embedLane struct {
	name string        // "query" | "batch" — the metrics label
	slot chan struct{} // capacity 1: a token in the channel means "held"
}

func newEmbedLane(name string) *embedLane {
	return &embedLane{name: name, slot: make(chan struct{}, 1)}
}

// acquire takes the lane, or returns an error wrapping ctx.Err() as soon as
// ctx ends while waiting. A free lane is taken without consulting ctx (the
// caller checks ctx before deciding to embed at all).
func (l *embedLane) acquire(ctx context.Context) error {
	select {
	case l.slot <- struct{}{}:
		return nil
	default:
	}
	metrics.EmbedLaneWaitsTotal.WithLabelValues(l.name).Inc()
	start := time.Now()
	select {
	case l.slot <- struct{}{}:
		metrics.EmbedLaneWaitSeconds.WithLabelValues(l.name).Observe(time.Since(start).Seconds())
		return nil
	case <-ctx.Done():
		metrics.EmbedLaneTimeoutsTotal.WithLabelValues(l.name).Inc()
		return fmt.Errorf("local embed: %s lane busy, gave up after %s: %w",
			l.name, time.Since(start).Round(time.Millisecond), ctx.Err())
	}
}

// release frees the lane. Calling it without a matching acquire is a bug.
func (l *embedLane) release() {
	<-l.slot
}
