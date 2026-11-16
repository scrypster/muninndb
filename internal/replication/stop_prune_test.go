package replication

import (
	"bytes"
	"context"
	"runtime/pprof"
	"testing"
)

// pruneLoopRunning reports whether the periodic prune loop's goroutine exists.
func pruneLoopRunning(t *testing.T) bool {
	t.Helper()
	var buf bytes.Buffer
	if err := pprof.Lookup("goroutine").WriteTo(&buf, 2); err != nil {
		t.Fatalf("goroutine dump: %v", err)
	}
	return bytes.Contains(buf.Bytes(), []byte("(*ClusterCoordinator).startPeriodicPrune.func"))
}

// TestStop_EndsAndJoinsThePruneLoop: Stop must not return while the periodic
// prune loop can still run. The caller closes the store next, and a prune tick
// still running then (a range delete followed by a flush and a compaction)
// panics with "pebble: closed" in a goroutine nobody owns, which takes the
// whole process down. It was seen killing a test package run, and in a server
// it is a panic at shutdown.
//
// The loop used to end only when Run's context was cancelled. Stop does not
// own that context, so this test does not cancel it either.
func TestStop_EndsAndJoinsThePruneLoop(t *testing.T) {
	if pruneLoopRunning(t) {
		t.Skip("a prune loop from another test is still running in this process; this check needs a clean slate")
	}
	coord, _ := newTestCoordinator(t, "primary")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); _ = coord.Run(ctx) }()
	waitUntil(t, func() bool { return pruneLoopRunning(t) }, func() string { return "the prune loop never started" })

	if err := coord.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if pruneLoopRunning(t) {
		t.Fatal("Stop returned while the periodic prune loop was still running: a tick after this point runs against a closed store")
	}
	if err := coord.Stop(); err != nil { // a second Stop must not panic on the closed channel
		t.Fatalf("second Stop: %v", err)
	}
	cancel()
	<-done
}
