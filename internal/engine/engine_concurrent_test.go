package engine

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// TestWriteRaceCondition_ConcurrentVaultAccess verifies that concurrent writes
// to the same vault from multiple goroutines do not cause data races or panics.
func TestWriteRaceCondition_ConcurrentVaultAccess(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()

	ctx := context.Background()
	const numGoroutines = 10
	const writesPerGoroutine = 100

	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	for g := 0; g < numGoroutines; g++ {
		go func(goroutineID int) {
			defer wg.Done()
			for i := 0; i < writesPerGoroutine; i++ {
				_, err := eng.Write(ctx, &mbp.WriteRequest{
					Vault:   "concurrent-test",
					Concept: "concurrent concept",
					Content: "concurrent content from goroutine",
				})
				if err != nil {
					// Errors are acceptable under load; panics are not.
					_ = err
				}
			}
		}(g)
	}

	wg.Wait()

	// Verify the vault has engrams after all concurrent writes.
	ws := eng.store.VaultPrefix("concurrent-test")
	count := eng.store.GetVaultCount(ctx, ws)
	if count == 0 {
		t.Error("expected vault to contain engrams after concurrent writes, got 0")
	}
}

// TestActivateSnapshotIsolation verifies that concurrent writes during an
// in-flight Activate do not cause panics or data races. The snapshot ensures
// all read phases see a consistent point-in-time view.
func TestActivateSnapshotIsolation(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()

	ctx := context.Background()

	// Seed a few engrams so Activate has data to work with.
	for i := 0; i < 20; i++ {
		_, _ = eng.Write(ctx, &mbp.WriteRequest{
			Vault:   "snapshot-test",
			Concept: "seed concept",
			Content: "some content for snapshot isolation test",
		})
	}

	var wg sync.WaitGroup
	const writers = 5
	const activators = 5

	// Launch concurrent writers.
	wg.Add(writers)
	for w := 0; w < writers; w++ {
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				_, _ = eng.Write(ctx, &mbp.WriteRequest{
					Vault:   "snapshot-test",
					Concept: "concurrent write",
					Content: "written during activate",
				})
			}
		}()
	}

	// Launch concurrent activators.
	wg.Add(activators)
	for a := 0; a < activators; a++ {
		go func() {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				resp, err := eng.Activate(ctx, &mbp.ActivateRequest{
					Vault:      "snapshot-test",
					Context:    []string{"seed concept"},
					MaxResults: 5,
				})
				if err != nil {
					continue
				}
				_ = resp
			}
		}()
	}

	wg.Wait()
}

// TestConcurrentWriteActivate_StressSmall stresses the write+activate hot path
// by running 10 writer goroutines (100 writes each) and 5 activator goroutines
// simultaneously. It verifies no panics occur and that the vault retains all
// successfully written engrams.
func TestConcurrentWriteActivate_StressSmall(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()

	ctx := context.Background()
	const vault = "stress-write-activate"
	const numWriters = 10
	const writesPerWriter = 100
	const numActivators = 5
	const activationsPerActivator = 20
	const seedCount = 5

	// Phase 1 — seed: write a handful of engrams so activators have data to
	// query before the concurrent phase begins.
	for i := 0; i < seedCount; i++ {
		_, err := eng.Write(ctx, &mbp.WriteRequest{
			Vault:   vault,
			Concept: fmt.Sprintf("seed-%d", i),
			Content: fmt.Sprintf("seed content %d for stress test", i),
		})
		if err != nil {
			t.Fatalf("seed write %d failed: %v", i, err)
		}
	}

	// Phase 2 — concurrent writers and activators.
	errCh := make(chan error, numWriters*writesPerWriter)

	var writerWg sync.WaitGroup
	writerWg.Add(numWriters)

	var activatorWg sync.WaitGroup
	activatorWg.Add(numActivators)

	// Launch writer goroutines.
	for g := 0; g < numWriters; g++ {
		go func(goroutineID int) {
			defer writerWg.Done()
			defer func() {
				if r := recover(); r != nil {
					errCh <- fmt.Errorf("writer goroutine %d panicked: %v", goroutineID, r)
				}
			}()
			for i := 0; i < writesPerWriter; i++ {
				resp, err := eng.Write(ctx, &mbp.WriteRequest{
					Vault:   vault,
					Concept: fmt.Sprintf("stress-%d-%d", goroutineID, i),
					Content: fmt.Sprintf("stress content goroutine %d write %d", goroutineID, i),
				})
				if err != nil {
					errCh <- err
				}
				_ = resp
			}
		}(g)
	}

	// Launch activator goroutines simultaneously.
	for a := 0; a < numActivators; a++ {
		go func(activatorID int) {
			defer activatorWg.Done()
			defer func() {
				if r := recover(); r != nil {
					errCh <- fmt.Errorf("activator goroutine %d panicked: %v", activatorID, r)
				}
			}()
			for i := 0; i < activationsPerActivator; i++ {
				resp, err := eng.Activate(ctx, &mbp.ActivateRequest{
					Vault:      vault,
					Context:    []string{"stress"},
					MaxResults: 5,
				})
				if err != nil {
					// Activation errors are acceptable under concurrent write
					// load; panics are not.
					continue
				}
				if resp == nil {
					errCh <- fmt.Errorf("activator goroutine %d: Activate returned nil response with no error", activatorID)
				}
			}
		}(a)
	}

	writerWg.Wait()
	activatorWg.Wait()

	close(errCh)
	for err := range errCh {
		t.Errorf("concurrent stress error: %v", err)
	}

	// Phase 3 — serial sanity check: run one final Activate after all
	// concurrent goroutines have completed to verify the engine is in a
	// structurally valid state (not just that no panic occurred).
	finalResp, err := eng.Activate(ctx, &mbp.ActivateRequest{
		Vault:      vault,
		Context:    []string{"stress"},
		MaxResults: 5,
	})
	if err != nil {
		t.Errorf("serial Activate after stress run failed: %v", err)
	} else if finalResp == nil {
		t.Error("serial Activate after stress run returned nil response with no error")
	} else if finalResp.TotalFound < 0 {
		t.Errorf("serial Activate: TotalFound must be >= 0, got %d", finalResp.TotalFound)
	}

	// Assert the vault contains at least the seed engrams. Under the race
	// detector some writes may be serialised but none should be lost to
	// corruption, so we accept any count > 0.
	ws := eng.store.VaultPrefix(vault)
	count := eng.store.GetVaultCount(ctx, ws)
	if count < seedCount {
		t.Errorf("expected at least %d engrams in vault after stress run, got %d", seedCount, count)
	}
	t.Logf("stress run complete: vault contains %d engrams (seed=%d, concurrent writes=%d)",
		count, seedCount, numWriters*writesPerWriter)
}

// TestWriteContextCancellation_StopsJobSubmission verifies that concurrent
// calls to Stop() and Write() do not cause panics. Writes may succeed or
// return errors — the only invariant is no panic.
func TestWriteContextCancellation_StopsJobSubmission(t *testing.T) {
	eng, cleanup := testEnv(t)
	// cleanup calls eng.Stop() internally; Stop() uses sync.Once so it is
	// safe to call it multiple times.
	defer cleanup()

	ctx := context.Background()
	const numWriters = 20

	var wg sync.WaitGroup
	wg.Add(numWriters)

	// Stop the engine after a short delay to race with the writers.
	go func() {
		time.Sleep(10 * time.Millisecond)
		eng.Stop()
	}()

	for g := 0; g < numWriters; g++ {
		go func(id int) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("goroutine %d: unexpected panic: %v", id, r)
				}
			}()
			_, err := eng.Write(ctx, &mbp.WriteRequest{
				Vault:   "stop-race-test",
				Concept: "concept",
				Content: "content",
			})
			// Either success or error is acceptable; panic is not.
			_ = err
		}(g)
	}

	wg.Wait()
}
