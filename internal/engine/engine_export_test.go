package engine

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/engine/vaultjob"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

func TestEngineExportVault(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()

	ctx := context.Background()

	// Write some engrams (unique content to avoid content-hash dedup).
	for i := 0; i < 3; i++ {
		_, err := eng.Write(ctx, &mbp.WriteRequest{
			Vault:   "export-src",
			Concept: "concept",
			Content: fmt.Sprintf("test content %d", i),
		})
		if err != nil {
			t.Fatalf("Write: %v", err)
		}
	}

	var buf bytes.Buffer
	result, err := eng.ExportVault(ctx, "export-src", "", 0, false, &buf)
	if err != nil {
		t.Fatalf("ExportVault: %v", err)
	}
	if result.EngramCount != 3 {
		t.Errorf("EngramCount: got %d, want 3", result.EngramCount)
	}
	if buf.Len() == 0 {
		t.Error("expected non-empty archive")
	}
}

func TestEngineExportVaultNotFound(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()

	ctx := context.Background()
	var buf bytes.Buffer
	_, err := eng.ExportVault(ctx, "no-such-vault", "", 0, false, &buf)
	if err == nil {
		t.Fatal("expected error for missing vault")
	}
}

func TestEngineStartImport(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()

	ctx := context.Background()

	// Write some engrams to source vault (unique content to avoid content-hash dedup).
	for i := 0; i < 2; i++ {
		_, err := eng.Write(ctx, &mbp.WriteRequest{
			Vault:   "import-src",
			Concept: "concept",
			Content: fmt.Sprintf("test content %d", i),
		})
		if err != nil {
			t.Fatalf("Write: %v", err)
		}
	}

	// Export from source.
	var buf bytes.Buffer
	if _, err := eng.ExportVault(ctx, "import-src", "", 0, false, &buf); err != nil {
		t.Fatalf("ExportVault: %v", err)
	}

	// Import into new vault.
	job, err := eng.StartImport(ctx, "import-dst", "", 0, false, bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("StartImport: %v", err)
	}
	if job.ID == "" {
		t.Error("expected non-empty job ID")
	}
}

// TestEngineExportImportRoundTrip verifies that data exported from one vault
// can be imported into a new vault and that the engram count is preserved.
func TestEngineExportImportRoundTrip(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()

	ctx := context.Background()

	// Write 5 engrams to the source vault (unique content to avoid content-hash dedup).
	const engramCount = 5
	for i := 0; i < engramCount; i++ {
		_, err := eng.Write(ctx, &mbp.WriteRequest{
			Vault:   "rt-src-vault",
			Concept: fmt.Sprintf("concept-%d", i),
			Content: fmt.Sprintf("round-trip test %d", i),
		})
		if err != nil {
			t.Fatalf("Write engram %d: %v", i, err)
		}
	}

	// Export from source vault into a buffer.
	var buf bytes.Buffer
	result, err := eng.ExportVault(ctx, "rt-src-vault", "", 0, false, &buf)
	if err != nil {
		t.Fatalf("ExportVault: %v", err)
	}
	if result.EngramCount != engramCount {
		t.Errorf("ExportVault: EngramCount = %d, want %d", result.EngramCount, engramCount)
	}
	if buf.Len() == 0 {
		t.Fatal("ExportVault: expected non-empty archive")
	}

	// Start import into the destination vault.
	job, err := eng.StartImport(ctx, "rt-dst-vault", "", 0, false, bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("StartImport: %v", err)
	}
	if job.ID == "" {
		t.Fatal("StartImport: expected non-empty job ID")
	}

	// Poll until the job reaches a terminal state.
	deadline := time.Now().Add(10 * time.Second)
	var finalJob *vaultjob.Job
	for time.Now().Before(deadline) {
		j, ok := eng.GetVaultJob(job.ID)
		if !ok {
			t.Fatalf("GetVaultJob(%q): job not found", job.ID)
		}
		if j.GetStatus() == vaultjob.StatusDone || j.GetStatus() == vaultjob.StatusError {
			finalJob = j
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if finalJob == nil {
		t.Fatalf("timed out waiting for import job %q to complete", job.ID)
	}
	if finalJob.GetStatus() != vaultjob.StatusDone {
		t.Fatalf("import job status = %s, want %s; err: %s",
			finalJob.GetStatus(), vaultjob.StatusDone, finalJob.GetErr())
	}

	// Verify the destination vault contains the expected number of engrams.
	wsDst := eng.store.VaultPrefix("rt-dst-vault")
	var dstCount int64
	_ = eng.store.ScanEngrams(ctx, wsDst, func(_ *storage.Engram) error {
		dstCount++
		return nil
	})
	if dstCount != engramCount {
		t.Errorf("dst vault engram count = %d, want %d", dstCount, engramCount)
	}
}
