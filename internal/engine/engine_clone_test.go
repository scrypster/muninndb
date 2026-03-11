package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/engine/vaultjob"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// waitForJob polls a job until it reaches a terminal state (done or error),
// or until the timeout elapses. Returns the final job.
func waitForJob(t *testing.T, eng *Engine, jobID string, timeout time.Duration) *vaultjob.Job {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		job, ok := eng.GetVaultJob(jobID)
		if !ok {
			t.Fatalf("job %s not found", jobID)
		}
		if job.GetStatus() == vaultjob.StatusDone || job.GetStatus() == vaultjob.StatusError {
			return job
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for job %s to complete after %s", jobID, timeout)
	return nil
}

// ---------------------------------------------------------------------------
// Validation tests: StartClone error paths
// ---------------------------------------------------------------------------

// TestStartClone_SourceNotFound verifies that StartClone with a nonexistent
// source vault returns an error wrapping ErrVaultNotFound.
func TestStartClone_SourceNotFound(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	_, err := eng.StartClone(ctx, "does-not-exist", "new-vault")
	if err == nil {
		t.Fatal("expected error for nonexistent source vault, got nil")
	}
	if !errors.Is(err, ErrVaultNotFound) {
		t.Errorf("expected error to wrap ErrVaultNotFound, got: %v", err)
	}
}

// TestStartClone_TargetAlreadyExists verifies that StartClone returns an error
// when the target vault already exists.
func TestStartClone_TargetAlreadyExists(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	// Create both source and target vaults by writing an engram to each.
	if _, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   "sc-source",
		Concept: "source concept",
		Content: "source content",
	}); err != nil {
		t.Fatalf("Write source: %v", err)
	}
	if _, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   "sc-existing-target",
		Concept: "target concept",
		Content: "target content",
	}); err != nil {
		t.Fatalf("Write target: %v", err)
	}

	_, err := eng.StartClone(ctx, "sc-source", "sc-existing-target")
	if err == nil {
		t.Fatal("expected error when target vault already exists, got nil")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("expected error to mention 'already exists', got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Validation tests: StartMerge error paths
// ---------------------------------------------------------------------------

// TestStartMerge_SameSourceTarget verifies that StartMerge with the same source
// and target vault returns a "must be different" error.
func TestStartMerge_SameSourceTarget(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	// Create vault-a so it exists.
	if _, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   "sm-same-vault",
		Concept: "concept a",
		Content: "content a",
	}); err != nil {
		t.Fatalf("Write vault: %v", err)
	}

	_, err := eng.StartMerge(ctx, "sm-same-vault", "sm-same-vault", false)
	if err == nil {
		t.Fatal("expected error when source and target are the same, got nil")
	}
	if !strings.Contains(err.Error(), "different") {
		t.Errorf("expected error to mention 'different', got: %v", err)
	}
}

// TestStartMerge_SourceNotFound verifies that StartMerge with a nonexistent
// source vault returns an error wrapping ErrVaultNotFound.
func TestStartMerge_SourceNotFound(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	// Create only target vault.
	if _, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   "sm-target-only",
		Concept: "target concept",
		Content: "target content",
	}); err != nil {
		t.Fatalf("Write target-vault: %v", err)
	}

	_, err := eng.StartMerge(ctx, "sm-nonexistent-source", "sm-target-only", false)
	if err == nil {
		t.Fatal("expected error for nonexistent source vault, got nil")
	}
	if !errors.Is(err, ErrVaultNotFound) {
		t.Errorf("expected error to wrap ErrVaultNotFound, got: %v", err)
	}
}

// TestStartMerge_TargetNotFound verifies that StartMerge with a nonexistent
// target vault returns an error wrapping ErrVaultNotFound.
func TestStartMerge_TargetNotFound(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	// Create only source vault.
	if _, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   "sm-source-only",
		Concept: "source concept",
		Content: "source content",
	}); err != nil {
		t.Fatalf("Write source-vault: %v", err)
	}

	_, err := eng.StartMerge(ctx, "sm-source-only", "sm-nonexistent-target", false)
	if err == nil {
		t.Fatal("expected error for nonexistent target vault, got nil")
	}
	if !errors.Is(err, ErrVaultNotFound) {
		t.Errorf("expected error to wrap ErrVaultNotFound, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Integration tests: clone pipeline
// ---------------------------------------------------------------------------

// TestClone_FullPipeline writes engrams to a source vault, clones it, waits
// for the job to complete, and verifies the target vault has the same engrams.
func TestClone_FullPipeline(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	// Write engrams to source vault.
	concepts := []string{"alpha", "beta", "gamma"}
	for _, c := range concepts {
		if _, err := eng.Write(ctx, &mbp.WriteRequest{
			Vault:   "clone-source",
			Concept: c,
			Content: "content for " + c,
		}); err != nil {
			t.Fatalf("Write(%q): %v", c, err)
		}
	}

	// Start the clone.
	job, err := eng.StartClone(ctx, "clone-source", "clone-target")
	if err != nil {
		t.Fatalf("StartClone: %v", err)
	}
	if job == nil {
		t.Fatal("StartClone returned nil job")
	}

	// Poll until done.
	finalJob := waitForJob(t, eng, job.ID, 5*time.Second)
	if finalJob.GetStatus() != vaultjob.StatusDone {
		t.Fatalf("clone job status = %s, want %s; err: %s",
			finalJob.GetStatus(), vaultjob.StatusDone, finalJob.GetErr())
	}

	// Verify target vault contains the cloned engrams by scanning actual stored data.
	wsTarget := eng.store.VaultPrefix("clone-target")
	var targetCount int64
	_ = eng.store.ScanEngrams(ctx, wsTarget, func(_ *storage.Engram) error {
		targetCount++
		return nil
	})
	if targetCount != int64(len(concepts)) {
		t.Errorf("target vault engram count = %d, want %d", targetCount, len(concepts))
	}

	// Verify source vault is unchanged by scanning actual engrams.
	wsSource := eng.store.VaultPrefix("clone-source")
	var srcCount int64
	_ = eng.store.ScanEngrams(ctx, wsSource, func(_ *storage.Engram) error {
		srcCount++
		return nil
	})
	if srcCount != int64(len(concepts)) {
		t.Errorf("source vault engram count = %d after clone, want %d", srcCount, len(concepts))
	}
}

// ---------------------------------------------------------------------------
// Integration tests: merge pipeline
// ---------------------------------------------------------------------------

// TestMerge_FullPipeline writes engrams to both source and target, merges
// source into target, and verifies behavior for both deleteSource=false and
// deleteSource=true.
func TestMerge_FullPipeline(t *testing.T) {
	t.Run("KeepSource", func(t *testing.T) {
		eng, cleanup := testEnv(t)
		defer cleanup()
		ctx := context.Background()

		// Write 2 engrams to source vault.
		for i, c := range []string{"merge-src-one", "merge-src-two"} {
			if _, err := eng.Write(ctx, &mbp.WriteRequest{
				Vault:   "fp-merge-src",
				Concept: c,
				Content: "source content " + string(rune('0'+i)),
			}); err != nil {
				t.Fatalf("Write source engram %q: %v", c, err)
			}
		}

		// Write 3 engrams to target vault.
		for i, c := range []string{"merge-dst-one", "merge-dst-two", "merge-dst-three"} {
			if _, err := eng.Write(ctx, &mbp.WriteRequest{
				Vault:   "fp-merge-dst",
				Concept: c,
				Content: "target content " + string(rune('0'+i)),
			}); err != nil {
				t.Fatalf("Write target engram %q: %v", c, err)
			}
		}

		// Start merge with deleteSource=false.
		job, err := eng.StartMerge(ctx, "fp-merge-src", "fp-merge-dst", false)
		if err != nil {
			t.Fatalf("StartMerge: %v", err)
		}
		if job == nil {
			t.Fatal("StartMerge returned nil job")
		}

		// Poll until done.
		finalJob := waitForJob(t, eng, job.ID, 5*time.Second)
		if finalJob.GetStatus() != vaultjob.StatusDone {
			t.Fatalf("merge job status = %s, want %s; err: %s",
				finalJob.GetStatus(), vaultjob.StatusDone, finalJob.GetErr())
		}

		// Target should now have 2+3=5 engrams. Use ScanEngrams for ground-truth count.
		wsTarget := eng.store.VaultPrefix("fp-merge-dst")
		var dstCount int64
		_ = eng.store.ScanEngrams(ctx, wsTarget, func(_ *storage.Engram) error {
			dstCount++
			return nil
		})
		if dstCount != 5 {
			t.Errorf("target vault engram count = %d, want 5", dstCount)
		}

		// Source should still exist with its original 2 engrams.
		wsSource := eng.store.VaultPrefix("fp-merge-src")
		var srcCount int64
		_ = eng.store.ScanEngrams(ctx, wsSource, func(_ *storage.Engram) error {
			srcCount++
			return nil
		})
		if srcCount != 2 {
			t.Errorf("source vault engram count = %d, want 2 (deleteSource=false)", srcCount)
		}
	})

	t.Run("DeleteSource", func(t *testing.T) {
		eng, cleanup := testEnv(t)
		defer cleanup()
		ctx := context.Background()

		// Write 2 engrams to source vault.
		for i, c := range []string{"del-src-one", "del-src-two"} {
			if _, err := eng.Write(ctx, &mbp.WriteRequest{
				Vault:   "fp-del-src",
				Concept: c,
				Content: "del source content " + string(rune('0'+i)),
			}); err != nil {
				t.Fatalf("Write source engram %q: %v", c, err)
			}
		}

		// Write 1 engram to target vault.
		if _, err := eng.Write(ctx, &mbp.WriteRequest{
			Vault:   "fp-del-dst",
			Concept: "del-target-one",
			Content: "del target content 0",
		}); err != nil {
			t.Fatalf("Write target engram: %v", err)
		}

		// Start merge with deleteSource=true.
		job, err := eng.StartMerge(ctx, "fp-del-src", "fp-del-dst", true)
		if err != nil {
			t.Fatalf("StartMerge(deleteSource=true): %v", err)
		}
		if job == nil {
			t.Fatal("StartMerge returned nil job")
		}

		// Poll until done.
		finalJob := waitForJob(t, eng, job.ID, 5*time.Second)
		if finalJob.GetStatus() != vaultjob.StatusDone {
			t.Fatalf("merge job status = %s, want %s; err: %s",
				finalJob.GetStatus(), vaultjob.StatusDone, finalJob.GetErr())
		}

		// Target should now have 2+1=3 engrams. Use ScanEngrams for ground-truth count.
		wsTarget := eng.store.VaultPrefix("fp-del-dst")
		var dstCount int64
		_ = eng.store.ScanEngrams(ctx, wsTarget, func(_ *storage.Engram) error {
			dstCount++
			return nil
		})
		if dstCount != 3 {
			t.Errorf("target vault engram count = %d, want 3", dstCount)
		}

		// Source vault should have been deleted: scanning should yield 0 engrams.
		wsSource := eng.store.VaultPrefix("fp-del-src")
		var srcCount int64
		_ = eng.store.ScanEngrams(ctx, wsSource, func(_ *storage.Engram) error {
			srcCount++
			return nil
		})
		if srcCount != 0 {
			t.Errorf("source vault engram count = %d, want 0 (deleted after merge)", srcCount)
		}
	})
}

// ---------------------------------------------------------------------------
// Additional existing tests preserved below
// ---------------------------------------------------------------------------

func TestEngineStartClone_JobCreated(t *testing.T) {
	eng, cleanup := testEnv(t)
	ctx := context.Background()

	// Write an engram to source vault.
	_, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   "jc-source",
		Concept: "test concept",
		Content: "test content",
	})
	if err != nil {
		cleanup()
		t.Fatalf("Write: %v", err)
	}

	job, err := eng.StartClone(ctx, "jc-source", "jc-dest")
	if err != nil {
		cleanup()
		t.Fatalf("StartClone: %v", err)
	}

	if job == nil {
		cleanup()
		t.Fatal("expected non-nil job")
	}
	if job.ID == "" {
		t.Errorf("expected non-empty job ID")
	}
	if job.Operation != "clone" {
		t.Errorf("expected operation 'clone', got %q", job.Operation)
	}
	if job.Source != "jc-source" {
		t.Errorf("expected source 'jc-source', got %q", job.Source)
	}
	if job.Target != "jc-dest" {
		t.Errorf("expected target 'jc-dest', got %q", job.Target)
	}

	// Wait for the goroutine to finish before cleanup closes the DB.
	waitForJob(t, eng, job.ID, 10*time.Second)
	cleanup()
}

func TestEngineStartClone_MemoriesAccessible(t *testing.T) {
	eng, cleanup := testEnv(t)
	ctx := context.Background()

	concepts := []string{"golang language", "database systems", "distributed computing"}
	for _, c := range concepts {
		_, err := eng.Write(ctx, &mbp.WriteRequest{
			Vault:   "ma-source",
			Concept: c,
			Content: "content for " + c,
			Tags:    []string{"test"},
		})
		if err != nil {
			cleanup()
			t.Fatalf("Write %q: %v", c, err)
		}
	}

	// Wait for FTS worker to process jobs.
	awaitFTS(t, eng)

	job, err := eng.StartClone(ctx, "ma-source", "ma-cloned")
	if err != nil {
		cleanup()
		t.Fatalf("StartClone: %v", err)
	}

	finalJob := waitForJob(t, eng, job.ID, 10*time.Second)
	if finalJob.GetStatus() != vaultjob.StatusDone {
		cleanup()
		t.Fatalf("expected job status done, got %q (err: %s)", finalJob.GetStatus(), finalJob.GetErr())
	}

	resp, err := eng.Activate(ctx, &mbp.ActivateRequest{
		Vault:      "ma-cloned",
		Context:    []string{"golang"},
		MaxResults: 10,
		Threshold:  0.0,
	})
	if err != nil {
		cleanup()
		t.Fatalf("Activate on cloned vault: %v", err)
	}
	if len(resp.Activations) == 0 {
		t.Error("expected activations in cloned vault, got none")
	}

	cleanup()
}

func TestEngineStartMerge_AllMemoriesInTarget(t *testing.T) {
	eng, cleanup := testEnv(t)
	ctx := context.Background()

	_, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   "am-vault-a",
		Concept: "source concept alpha",
		Content: "from vault-a",
	})
	if err != nil {
		cleanup()
		t.Fatalf("Write vault-a: %v", err)
	}
	_, err = eng.Write(ctx, &mbp.WriteRequest{
		Vault:   "am-vault-b",
		Concept: "target concept beta",
		Content: "from vault-b",
	})
	if err != nil {
		cleanup()
		t.Fatalf("Write vault-b: %v", err)
	}

	awaitFTS(t, eng)

	job, err := eng.StartMerge(ctx, "am-vault-a", "am-vault-b", false)
	if err != nil {
		cleanup()
		t.Fatalf("StartMerge: %v", err)
	}

	finalJob := waitForJob(t, eng, job.ID, 10*time.Second)
	if finalJob.GetStatus() != vaultjob.StatusDone {
		cleanup()
		t.Fatalf("merge failed: %s", finalJob.GetErr())
	}

	wsB := eng.store.VaultPrefix("am-vault-b")
	count := eng.store.GetVaultCount(ctx, wsB)
	if count < 2 {
		t.Errorf("expected at least 2 engrams in vault-b after merge, got %d", count)
	}

	cleanup()
}

func TestEngineGetVaultJob_ReturnsJob(t *testing.T) {
	eng, cleanup := testEnv(t)
	ctx := context.Background()

	_, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   "gvj-src",
		Concept: "concept",
		Content: "content",
	})
	if err != nil {
		cleanup()
		t.Fatalf("Write: %v", err)
	}

	job, err := eng.StartClone(ctx, "gvj-src", "gvj-dst")
	if err != nil {
		cleanup()
		t.Fatalf("StartClone: %v", err)
	}

	retrieved, ok := eng.GetVaultJob(job.ID)
	if !ok {
		cleanup()
		t.Fatal("expected job to be found")
	}
	if retrieved.ID != job.ID {
		t.Errorf("expected job ID %q, got %q", job.ID, retrieved.ID)
	}
	if retrieved.Operation != "clone" {
		t.Errorf("expected operation 'clone', got %q", retrieved.Operation)
	}

	finalJob := waitForJob(t, eng, job.ID, 10*time.Second)
	if finalJob.GetStatus() != vaultjob.StatusDone {
		cleanup()
		t.Fatalf("expected done, got %q (err: %s)", finalJob.GetStatus(), finalJob.GetErr())
	}

	cleanup()
}
