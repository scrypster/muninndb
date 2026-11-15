//go:build localassets

package embed

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/embedlane"
)

// Invented fixture domain: trail maintenance logs.
const laneQuery = "which switchback needs a new water bar"

func laneTexts(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("Trail log %d: the volunteer crew cleared two blowdowns past the third switchback, "+
			"rebuilt a rock water bar near the ridge spur, and flagged a washout on the lower loop for the "+
			"next work party; the junction sign at marker %d still needs a new post.", i, 100+i)
	}
	return out
}

func newLaneProvider(t *testing.T) *LocalProvider {
	t.Helper()
	p := &LocalProvider{}
	if _, err := p.Init(context.Background(), ProviderHTTPConfig{DataDir: t.TempDir()}); err != nil {
		t.Skipf("local provider init failed (assets missing? run make fetch-assets): %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// watchdog is a FAILURE DETECTOR, never a timing assertion: it only turns a
// hang (the defect — a queued call that never gives up) into a test failure
// instead of a stuck CI job.
const watchdog = 30 * time.Second

type embedResult struct {
	vec     []float32
	err     error
	elapsed time.Duration
}

func embedAsync(ctx context.Context, p *LocalProvider, texts []string) <-chan embedResult {
	ch := make(chan embedResult, 1)
	go func() {
		s := time.Now()
		v, err := p.EmbedBatch(ctx, texts)
		ch <- embedResult{v, err, time.Since(s)}
	}()
	return ch
}

// gateBatch installs hooks that signal when the first batch-lane call has
// entered its lane and hold that call INSIDE the lane (after its inference,
// before it releases) until the returned release func runs. The lane is
// therefore provably occupied for as long as the test needs, with no sleeps.
func gateBatch(p *LocalProvider) (entered <-chan struct{}, release func()) {
	in := make(chan struct{})
	out := make(chan struct{})
	var inOnce, outOnce sync.Once
	p.onLaneAcquired = func(lane string) {
		if lane == "batch" {
			inOnce.Do(func() { close(in) })
		}
	}
	p.onLaneReleased = func(lane string) {
		if lane == "batch" {
			outOnce.Do(func() { <-out })
		}
	}
	var relOnce sync.Once
	return in, func() { relOnce.Do(func() { close(out) }) }
}

// TestLocalProvider_QueuedEmbedHonoursDeadline: a call queued behind an
// in-flight batch returns its deadline error promptly — it does not wait for
// the batch to finish. On the pre-lane code (one sync.Mutex) the 5 ms call
// returned err=nil after the whole batch, 1.5-2.4 s later.
func TestLocalProvider_QueuedEmbedHonoursDeadline(t *testing.T) {
	p := newLaneProvider(t)
	entered, release := gateBatch(p)
	defer release()
	batch := embedAsync(context.Background(), p, laneTexts(8))
	<-entered // a real batch is in the batch lane and will not leave until release()

	// Unmarked = background work: queues for the busy batch lane.
	t.Run("batch lane", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
		defer cancel()
		select {
		case r := <-embedAsync(ctx, p, []string{laneQuery}):
			if !errors.Is(r.err, context.DeadlineExceeded) {
				t.Fatalf("queued call returned err=%v vecLen=%d after %s; want an error wrapping context.DeadlineExceeded",
					r.err, len(r.vec), r.elapsed)
			}
			t.Logf("5ms-deadline call queued behind a batch returned after %s: %v", r.elapsed.Round(time.Microsecond), r.err)
		case <-time.After(watchdog):
			t.Fatal("queued call did not return while the batch held its lane — the deadline is ignored while queued")
		}
	})

	// The query lane honours deadlines the same way when IT is the busy one.
	t.Run("query lane", func(t *testing.T) {
		rel := p.HoldLaneForTest(true)
		defer rel()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
		defer cancel()
		select {
		case r := <-embedAsync(embedlane.WithQuery(ctx), p, []string{laneQuery}):
			if !errors.Is(r.err, context.DeadlineExceeded) {
				t.Fatalf("queued query call returned err=%v after %s; want context.DeadlineExceeded", r.err, r.elapsed)
			}
			t.Logf("5ms-deadline query queued behind a held query lane returned after %s", r.elapsed.Round(time.Microsecond))
		case <-time.After(watchdog):
			t.Fatal("queued query call did not return while its lane was held")
		}
	})

	release()
	if r := <-batch; r.err != nil {
		t.Fatalf("the gated batch itself failed: %v", r.err)
	}
}

// TestLocalProvider_QueryLaneNotBlockedByBatch: a query embed issued while a
// large batch is in flight completes without waiting for it, in roughly idle
// time. The batch is held in its lane until the query returns, so "the query
// finished first" is guaranteed by construction rather than by a race; on the
// pre-lane code the query cannot finish at all (it waits on the batch, which
// waits on the query) and the watchdog fails the test. The latency bound is
// relative to idle latency measured in the same process, not a wall-clock
// constant.
func TestLocalProvider_QueryLaneNotBlockedByBatch(t *testing.T) {
	p := newLaneProvider(t)
	qctx := embedlane.WithQuery(context.Background())

	// Idle query latency, this process, this machine, right now.
	if _, err := p.EmbedBatch(qctx, []string{laneQuery}); err != nil { // warm
		t.Fatal(err)
	}
	var idle []time.Duration
	for i := 0; i < 7; i++ {
		s := time.Now()
		if _, err := p.EmbedBatch(qctx, []string{laneQuery}); err != nil {
			t.Fatal(err)
		}
		idle = append(idle, time.Since(s))
	}
	sort.Slice(idle, func(i, j int) bool { return idle[i] < idle[j] })
	idleMedian := idle[len(idle)/2]

	entered, release := gateBatch(p)
	defer release()
	// 64 texts in ONE provider call (above localMaxBatch, which only
	// BatchEmbedder enforces): ~1.5 s of real inference for the query to land in.
	batch := embedAsync(context.Background(), p, laneTexts(64))
	<-entered

	var q embedResult
	select {
	case q = <-embedAsync(qctx, p, []string{laneQuery}):
	case <-time.After(watchdog):
		t.Fatal("query embed did not complete while a batch was in flight — it is queued behind the batch")
	}
	release()
	if q.err != nil {
		t.Fatalf("query embed under batch failed: %v", q.err)
	}
	b := <-batch
	if b.err != nil {
		t.Fatalf("batch failed: %v", b.err)
	}
	// Generous: concurrent inference shares CPU with the batch, so some
	// slowdown is expected; queueing behind a 64-text batch would be ~50x.
	bound := 10 * idleMedian
	t.Logf("idle query median=%s, query under 64-text batch=%s (bound %s), batch took %s",
		idleMedian.Round(time.Millisecond), q.elapsed.Round(time.Millisecond), bound.Round(time.Millisecond), b.elapsed.Round(time.Millisecond))
	if q.elapsed > bound {
		t.Fatalf("query under batch took %s, more than 10x idle median %s", q.elapsed, idleMedian)
	}
}

// TestLocalProvider_LanesProduceIdenticalVectors: the lanes change scheduling
// only. The same text yields bit-identical vectors from the query lane, the
// batch lane, and the query lane while a batch is running concurrently on the
// shared ORT session.
func TestLocalProvider_LanesProduceIdenticalVectors(t *testing.T) {
	p := newLaneProvider(t)
	ctx := context.Background()
	viaBatch, err := p.EmbedBatch(ctx, []string{laneQuery})
	if err != nil {
		t.Fatal(err)
	}
	viaQuery, err := p.EmbedBatch(embedlane.WithQuery(ctx), []string{laneQuery})
	if err != nil {
		t.Fatal(err)
	}
	assertBitIdentical(t, "query lane vs batch lane (idle)", viaBatch, viaQuery)

	entered, release := gateBatch(p)
	defer release()
	batch := embedAsync(ctx, p, laneTexts(64))
	<-entered
	var underBatch embedResult
	select {
	case underBatch = <-embedAsync(embedlane.WithQuery(ctx), p, []string{laneQuery}):
	case <-time.After(watchdog):
		t.Fatal("query embed did not complete while a batch was in flight")
	}
	release()
	if underBatch.err != nil {
		t.Fatal(underBatch.err)
	}
	assertBitIdentical(t, "query lane concurrent with a batch vs idle", viaBatch, underBatch.vec)

	// And the batch computed concurrently with the query is itself unchanged.
	b := <-batch
	if b.err != nil {
		t.Fatal(b.err)
	}
	idleBatch, err := p.EmbedBatch(ctx, laneTexts(64))
	if err != nil {
		t.Fatal(err)
	}
	assertBitIdentical(t, "64-batch run concurrently with a query vs the same batch idle", idleBatch, b.vec)
	// Not asserted here, recorded for the next reader: a text's vector depends
	// on its BATCH-MATES on the bundled int8 model (alone vs inside any batch:
	// cosine ~0.9993, max component diff ~6e-3; deterministic for an identical
	// batch). That predates the lanes and is why this test compares like with
	// like — same text, same batch composition — rather than alone vs batched.
}

func assertBitIdentical(t *testing.T, label string, a, b []float32) {
	t.Helper()
	if len(a) != len(b) || len(a) == 0 {
		t.Fatalf("%s: lengths %d vs %d", label, len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("%s: component %d differs: %v vs %v", label, i, a[i], b[i])
		}
	}
}

// TestLocalProvider_NeverTwoBatchesInFlight pins the memory bound the old
// mutex provided and the lanes must keep: across many concurrent background
// callers there is never more than one batch-lane inference at once (one
// 64-text inference costs ~1 GB of RSS; the production host has 2 GB), and
// never more than one query-lane inference. A marked call too large for the
// query lane is sent to the batch lane, not run beside it. Asserted on the
// instrumented provider's own in-flight count, not on RSS.
func TestLocalProvider_NeverTwoBatchesInFlight(t *testing.T) {
	p := newLaneProvider(t)
	var inFlight, maxInFlight [2]atomic.Int32 // [0]=batch, [1]=query
	idx := func(lane string) int {
		if lane == "query" {
			return 1
		}
		return 0
	}
	p.onLaneAcquired = func(lane string) {
		i := idx(lane)
		n := inFlight[i].Add(1)
		for {
			m := maxInFlight[i].Load()
			if n <= m || maxInFlight[i].CompareAndSwap(m, n) {
				break
			}
		}
	}
	p.onLaneReleased = func(lane string) { inFlight[idx(lane)].Add(-1) }

	// Through the service layer too, as the retroactive processor calls it.
	svc := NewBatchEmbedder(p, nil, nil)
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	run := func(f func() error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := f(); err != nil {
				errs <- err
			}
		}()
	}
	for g := 0; g < 4; g++ {
		run(func() error { _, err := p.EmbedBatch(context.Background(), laneTexts(4)); return err })
		run(func() error { _, err := svc.Embed(context.Background(), laneTexts(20)); return err })
		run(func() error {
			_, err := p.EmbedBatch(embedlane.WithQuery(context.Background()), []string{laneQuery})
			return err
		})
	}
	// A marked call above the query lane's cap.
	big := localQueryLaneMaxTexts + 1
	p2 := func() error {
		_, err := p.EmbedBatch(embedlane.WithQuery(context.Background()), laneTexts(big))
		return err
	}
	run(p2)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if got := maxInFlight[0].Load(); got != 1 {
		t.Fatalf("max concurrent batch-lane inferences = %d, want exactly 1 (the memory cap)", got)
	}
	if got := maxInFlight[1].Load(); got != 1 {
		t.Fatalf("max concurrent query-lane inferences = %d, want exactly 1", got)
	}

	// The oversized marked call took the batch lane.
	var lane string
	p.onLaneAcquired = func(l string) { lane = l }
	p.onLaneReleased = nil
	if err := p2(); err != nil {
		t.Fatal(err)
	}
	if lane != "batch" {
		t.Fatalf("a marked call of %d texts (> query cap %d) took lane %q, want batch", big, localQueryLaneMaxTexts, lane)
	}
	// And an unmarked single text is background work, never the query lane.
	if _, err := p.EmbedBatch(context.Background(), []string{laneQuery}); err != nil {
		t.Fatal(err)
	}
	if lane != "batch" {
		t.Fatalf("an unmarked single-text call took lane %q, want batch (the lane is chosen by the caller's mark, not by text count)", lane)
	}
}

// TestLocalProvider_ConcurrentLanesRace exercises query + batch + service
// (retroactive-shaped) callers together and Close afterwards; its value is
// under -race.
func TestLocalProvider_ConcurrentLanesRace(t *testing.T) {
	p := newLaneProvider(t)
	svc := NewBatchEmbedder(p, nil, nil)
	var wg sync.WaitGroup
	for g := 0; g < 3; g++ {
		wg.Add(3)
		go func() {
			defer wg.Done()
			if _, err := svc.Embed(context.Background(), laneTexts(12)); err != nil {
				t.Error(err)
			}
		}()
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 3; i++ {
				if _, err := p.EmbedBatch(embedlane.WithQuery(context.Background()), []string{fmt.Sprintf("%s %d %d", laneQuery, g, i)}); err != nil {
					t.Error(err)
				}
			}
		}(g)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
			defer cancel()
			_, _ = p.EmbedBatch(embedlane.WithQuery(ctx), []string{laneQuery}) // may time out; must not race
		}()
	}
	wg.Wait()
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := p.EmbedBatch(context.Background(), []string{laneQuery}); err == nil {
		t.Fatal("EmbedBatch after Close succeeded; want not-initialized error")
	}
}
