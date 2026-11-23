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
// The bundled local provider and the Ollama provider both schedule with it.
type embedLane struct {
	owner string        // the provider named in the busy error: "local" | "ollama"
	name  string        // "query" | "batch" — the metrics label
	slot  chan struct{} // capacity 1: a token in the channel means "held"
	// onWait, when non-nil, is called when a caller finds the lane held and
	// starts to wait for it. Test-only instrumentation: it tells a test that a
	// call is parked here, without sleeping. Set it before the lane is shared.
	onWait func()
}

// newEmbedLane returns a lane of the bundled local provider.
func newEmbedLane(name string) *embedLane {
	return newProviderLane("local", name)
}

// newProviderLane returns a lane owned by the named provider.
func newProviderLane(owner, name string) *embedLane {
	return &embedLane{owner: owner, name: name, slot: make(chan struct{}, 1)}
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
	if l.onWait != nil {
		l.onWait()
	}
	start := time.Now()
	select {
	case l.slot <- struct{}{}:
		metrics.EmbedLaneWaitSeconds.WithLabelValues(l.name).Observe(time.Since(start).Seconds())
		return nil
	case <-ctx.Done():
		metrics.EmbedLaneTimeoutsTotal.WithLabelValues(l.name).Inc()
		return fmt.Errorf("%s embed: %s lane busy, gave up after %s: %w",
			l.owner, l.name, time.Since(start).Round(time.Millisecond), ctx.Err())
	}
}

// release frees the lane. Calling it without a matching acquire is a bug.
func (l *embedLane) release() {
	<-l.slot
}
