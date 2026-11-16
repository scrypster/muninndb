package engine

import (
	"testing"
	"time"

	"github.com/cockroachdb/pebble/vfs"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// TestWaitWriteTimeIdle_WaitsForTheNoveltyWorker pins that the test drain covers
// the novelty worker. That worker writes a `refines` edge for a newly written
// near-duplicate, off the write path; WaitWriteTimeIdle drained every other
// write-time worker and not this one, so a test asserting on a vault's edges
// after the drain was still racing it.
func TestWaitWriteTimeIdle_WaitsForTheNoveltyWorker(t *testing.T) {
	env := openRCEnv(t, vfs.NewMem())
	defer env.stop()

	first, err := env.eng.Write(env.ctx, &mbp.WriteRequest{Vault: "default", Concept: "kiln log",
		Content: "Bisque firing reached cone six after nine hours in the small kiln."})
	if err != nil {
		t.Fatal(err)
	}
	env.eng.waitWriteTimeIdle()

	// Hold the next novelty job at its start. The worker is idle here, and the
	// job's channel send orders this write before the worker's read.
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	env.eng.noveltyJobHook = func() {
		started <- struct{}{}
		<-release
	}

	second, err := env.eng.Write(env.ctx, &mbp.WriteRequest{Vault: "default", Concept: "kiln log",
		Content: "Bisque firing reached cone six after ten hours in the small kiln."})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("the novelty job for the second write never started")
	}

	drained := make(chan struct{})
	go func() {
		env.eng.waitWriteTimeIdle()
		close(drained)
	}()
	select {
	case <-drained:
		close(release)
		t.Fatal("WaitWriteTimeIdle returned while a novelty job was still running")
	case <-time.After(200 * time.Millisecond):
	}

	close(release)
	select {
	case <-drained:
	case <-time.After(10 * time.Second):
		t.Fatal("WaitWriteTimeIdle did not return after the novelty job finished")
	}

	// The job's effect is visible the moment the drain returns.
	rev, err := env.store.GetReverseAssociations(env.ctx, env.ws, env.mustULID(first.ID), 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range rev {
		if r.RelType == storage.RelRefines && r.TargetID == env.mustULID(second.ID) {
			found = true
		}
	}
	if !found {
		t.Fatalf("no refines edge from the second memory to the first after the drain; incoming edges: %+v", rev)
	}
}

// TestWaitWriteTimeIdle_ReturnsOnceTheNoveltyWorkerHasExited pins that the
// drain cannot hang on jobs nothing will run: after shutdown, and after the
// worker has died of a panic (it does not restart).
func TestWaitWriteTimeIdle_ReturnsOnceTheNoveltyWorkerHasExited(t *testing.T) {
	returnsWithin := func(t *testing.T, e *Engine, d time.Duration) bool {
		t.Helper()
		done := make(chan struct{})
		go func() {
			e.waitWriteTimeIdle()
			close(done)
		}()
		select {
		case <-done:
			return true
		case <-time.After(d):
			return false
		}
	}

	t.Run("after shutdown", func(t *testing.T) {
		env := openRCEnv(t, vfs.NewMem())
		env.stop()
		// A job that was queued when the worker exited: counted, never run.
		env.eng.noveltyPending.Add(1)
		if !returnsWithin(t, env.eng, 5*time.Second) {
			t.Fatal("WaitWriteTimeIdle hangs after shutdown with a novelty job still queued")
		}
	})

	t.Run("after the worker panicked", func(t *testing.T) {
		env := openRCEnv(t, vfs.NewMem())
		defer env.stop()
		env.eng.noveltyJobHook = func() { panic("injected novelty failure") }
		if _, err := env.eng.Write(env.ctx, &mbp.WriteRequest{Vault: "default", Concept: "glaze test",
			Content: "Celadon over porcelain pooled in the carved lines."}); err != nil {
			t.Fatal(err)
		}
		select {
		case <-env.eng.noveltyDone:
		case <-time.After(10 * time.Second):
			t.Fatal("the novelty worker did not exit after its job panicked")
		}
		// Later writes queue jobs no worker will ever take.
		for _, content := range []string{"Shino crawled on the shoulder of the jar.", "Ash glaze ran to the foot ring."} {
			if _, err := env.eng.Write(env.ctx, &mbp.WriteRequest{Vault: "default", Concept: "glaze test", Content: content}); err != nil {
				t.Fatal(err)
			}
		}
		if got := env.eng.noveltyPending.Load(); got == 0 {
			t.Fatal("premise: expected novelty jobs left queued after the worker exited")
		}
		if !returnsWithin(t, env.eng, 5*time.Second) {
			t.Fatal("WaitWriteTimeIdle hangs after the novelty worker panicked")
		}
	})
}
