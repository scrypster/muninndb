package engine

import (
	"context"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/engine/vaultjob"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// TestWhereLeftOff_SurvivesClone and TestWhereLeftOff_SurvivesMerge are #811's
// RED/GREEN pin: prefix.LastAccess (0x22), the index backing
// muninn_where_left_off, is not in clone.go's vaultScopedSwapPrefixes and
// neither CloneVaultData nor MergeVaultData copies it — but reindexVault
// already scans every engram in the target vault after both operations to
// rebuild FTS/HNSW, and that is the correct place to also rebuild 0x22, since
// clone's own per-engram LastAccess is (post-#810/#835) real, non-sentinel
// data by the time reindexVault runs. Without the fix, WhereLeftOff on a
// freshly cloned or merged vault returns nothing for engrams that predate the
// operation, no matter how recently they were genuinely accessed in the
// source.
func TestWhereLeftOff_SurvivesClone(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	if _, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault: "wlo-clone-source", Concept: "quarterly retro", Content: "notes from the retro",
	}); err != nil {
		t.Fatalf("write source: %v", err)
	}
	eng.WaitWriteTimeIdle()

	job, err := eng.StartClone(ctx, "wlo-clone-source", "wlo-clone-target")
	if err != nil {
		t.Fatalf("StartClone: %v", err)
	}
	finalJob := waitForJob(t, eng, job.ID, 5*time.Second)
	if finalJob.GetStatus() != vaultjob.StatusDone {
		t.Fatalf("clone job status = %s, want done; err: %s", finalJob.GetStatus(), finalJob.GetErr())
	}

	results, err := eng.WhereLeftOff(ctx, "wlo-clone-target", 10, nil)
	if err != nil {
		t.Fatalf("WhereLeftOff(clone target): %v", err)
	}
	if len(results) == 0 {
		t.Fatal("#811: WhereLeftOff returned nothing for a freshly cloned vault — the 0x22 index was not carried or rebuilt")
	}
}

func TestWhereLeftOff_SurvivesMerge(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	if _, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault: "wlo-merge-source", Concept: "handover notes", Content: "who is covering the shift",
	}); err != nil {
		t.Fatalf("write source: %v", err)
	}
	if _, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault: "wlo-merge-dest", Concept: "pre-existing memory", Content: "already lived in the destination vault",
	}); err != nil {
		t.Fatalf("write dest: %v", err)
	}
	eng.WaitWriteTimeIdle()

	job, err := eng.StartMerge(ctx, "wlo-merge-source", "wlo-merge-dest", false)
	if err != nil {
		t.Fatalf("StartMerge: %v", err)
	}
	finalJob := waitForJob(t, eng, job.ID, 5*time.Second)
	if finalJob.GetStatus() != vaultjob.StatusDone {
		t.Fatalf("merge job status = %s, want done; err: %s", finalJob.GetStatus(), finalJob.GetErr())
	}

	results, err := eng.WhereLeftOff(ctx, "wlo-merge-dest", 10, nil)
	if err != nil {
		t.Fatalf("WhereLeftOff(merge dest): %v", err)
	}
	if len(results) < 2 {
		t.Fatalf("#811: WhereLeftOff returned %d result(s) after a merge, want at least 2 (the pre-existing memory and the merged-in one) — the 0x22 index was not carried or rebuilt for the merged-in engram", len(results))
	}
}
