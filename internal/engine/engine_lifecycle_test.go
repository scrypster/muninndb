package engine

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// TestEngine_SpawnAfterStop verifies that spawnFireAndForget and spawnJob
// return false and launch no goroutine after Stop() has been called.
func TestEngine_SpawnAfterStop(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()

	eng.Stop()

	var launched bool
	if eng.spawnFireAndForget(func() { launched = true }) {
		t.Error("spawnFireAndForget: returned true after Stop()")
	}
	if launched {
		t.Error("spawnFireAndForget: goroutine was launched after Stop()")
	}

	launched = false
	if eng.spawnJob(func() { launched = true }) {
		t.Error("spawnJob: returned true after Stop()")
	}
	if launched {
		t.Error("spawnJob: goroutine was launched after Stop()")
	}
}

// TestEngine_StopDrainsFireAndForget verifies that stopping the engine while
// a Read-triggered scoring goroutine is in-flight does not panic.
// This is the scenario that produced "panic: pebble: closed" in CI.
func TestEngine_StopDrainsFireAndForget(t *testing.T) {
	for range 50 {
		eng, cleanup := testEnv(t)

		// Write an engram so Read has something to return.
		ctx := context.Background()
		resp, err := eng.Write(ctx, &mbp.WriteRequest{
			Vault:   "test",
			Concept: "lifecycle",
			Content: "goroutine drain test",
		})
		if err != nil {
			cleanup()
			t.Fatal(err)
		}

		// Read triggers a fire-and-forget scoring goroutine.
		_, _ = eng.Read(ctx, &mbp.ReadRequest{
			Vault: "test",
			ID:    resp.ID,
		})

		// Stop immediately — races with the feedback goroutine.
		// spawnFireAndForget must drain it before store.Close().
		cleanup() // calls eng.Stop() then store.Close()
	}
	// Reaching here without panic means the drain worked correctly.
}

// TestEngine_StopDrainsJobs is a stress test that starts a clone job just before
// Stop(), verifying that no panic or hang occurs. Either the job runs to completion
// or spawnJob returns false (engine shutting down) — both outcomes are correct.
func TestEngine_StopDrainsJobs(t *testing.T) {
	ctx := context.Background()
	for i := range 20 {
		eng, cleanup := testEnv(t)

		src := fmt.Sprintf("drain-src-%d", i)
		dst := fmt.Sprintf("drain-dst-%d", i)

		// Create source and target vaults by writing an engram to each.
		if _, err := eng.Write(ctx, &mbp.WriteRequest{
			Vault:   src,
			Concept: "lifecycle drain test",
			Content: "source engram",
		}); err != nil {
			cleanup()
			t.Fatal(err)
		}
		if _, err := eng.Write(ctx, &mbp.WriteRequest{
			Vault:   dst,
			Concept: "lifecycle drain test",
			Content: "target engram",
		}); err != nil {
			cleanup()
			t.Fatal(err)
		}

		// Trigger a clone job just before Stop. Either the job runs or
		// spawnJob returns false — both are correct. Must not panic or hang.
		go func() {
			eng.StartClone(ctx, src, src+"_clone")
		}()
		cleanup()
	}
}

// TestEngine_StopIdempotent verifies that Stop() can be called multiple times
// concurrently without deadlock or double-drain.
func TestEngine_StopIdempotent(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()

	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			eng.Stop()
		}()
	}
	wg.Wait() // must complete without deadlock
}
