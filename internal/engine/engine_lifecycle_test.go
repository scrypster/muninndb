package engine

import (
	"sync"
	"testing"
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
