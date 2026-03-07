package engine

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/scrypster/muninndb/internal/engine/vaultjob"
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

// TestNoBareGoSpawnsWithoutMarker walks all non-test engine package .go files
// and fails if any bare goroutine launch (line matching `^\t+go `) is not
// preceded by a line containing `// engine:spawn-ok`.
func TestNoBareGoSpawnsWithoutMarker(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}

		f, err := os.Open(name)
		if err != nil {
			t.Fatalf("open %s: %v", name, err)
		}

		var lines []string
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			lines = append(lines, scanner.Text())
		}
		f.Close()
		if err := scanner.Err(); err != nil {
			t.Fatalf("scan %s: %v", name, err)
		}

		for i, line := range lines {
			// Match a bare goroutine launch: one or more leading tabs then "go ".
			trimmed := strings.TrimLeft(line, "\t")
			if !strings.HasPrefix(trimmed, "go ") || trimmed == line {
				continue
			}
			// Check that the preceding non-blank line contains the marker.
			markerFound := false
			for j := i - 1; j >= 0; j-- {
				prev := strings.TrimSpace(lines[j])
				if prev == "" {
					continue
				}
				if strings.Contains(prev, "// engine:spawn-ok") {
					markerFound = true
				}
				break
			}
			if !markerFound {
				t.Errorf("%s:%d: bare goroutine launch without '// engine:spawn-ok' marker on preceding line:\n\t%s",
					name, i+1, line)
			}
		}
	}
}

// TestRunJobRecoversPebbleClosed verifies that the recover() block in runClone
// catches a pebble.ErrClosed panic (from a closed DB) and fails the job cleanly
// rather than crashing the process.
//
// Strategy: stop the engine first (draining the FTS worker and all other goroutines
// so no external goroutine is using the DB), then close the DB directly, then invoke
// runClone directly (in-package access). The recover() in runClone must catch the
// pebble.ErrClosed panic and mark the job as failed.
func TestRunJobRecoversPebbleClosed(t *testing.T) {
	ctx := context.Background()

	eng, db, cleanup := testEnvWithDB(t)
	defer cleanup()

	// Write an engram to create a source vault.
	if _, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   "src-closed",
		Concept: "pebble closed test",
		Content: "test engram",
	}); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Stop the engine so all background goroutines (including the FTS worker)
	// finish and release the DB before we close it.
	eng.Stop()

	// Now close the DB; no engine goroutines are using it at this point.
	if err := db.Close(); err != nil {
		t.Fatalf("db.Close: %v", err)
	}

	// Create a synthetic job to track recovery state.
	job, err := eng.jobManager.Create("clone", "src-closed", "dst-closed")
	if err != nil {
		t.Fatalf("jobManager.Create: %v", err)
	}

	wsSource := eng.store.VaultPrefix("src-closed")
	wsTarget := eng.store.VaultPrefix("dst-closed")

	// runClone will attempt to read from the closed DB and panic with pebble.ErrClosed.
	// The recover() block inside runClone must catch this and call jobManager.Fail.
	// Because we are calling it synchronously here (no goroutine), any unhandled
	// panic would propagate and fail the test — confirming the recover() works.
	eng.runClone(job, wsSource, wsTarget, "dst-closed")

	// The job must be in an error state — not still running, not succeeded.
	if got := job.GetStatus(); got != vaultjob.StatusError {
		t.Errorf("expected job status %q after pebble.ErrClosed, got %q", vaultjob.StatusError, got)
	}
}
