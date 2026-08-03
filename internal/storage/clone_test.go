package storage

import (
	"context"
	"encoding/binary"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/storage/keys"
)

// TestCloneVaultData_AllPrefixesCopied writes an engram to the source vault,
// clones it, and verifies the engram exists in the target vault.
func TestCloneVaultData_AllPrefixesCopied(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	wsSource := store.VaultPrefix("source-vault")
	wsTarget := store.VaultPrefix("target-vault")

	// Write engram to source.
	eng := &Engram{
		Concept: "test concept",
		Content: "test content body",
		Tags:    []string{"alpha"},
	}
	id, err := store.WriteEngram(ctx, wsSource, eng)
	if err != nil {
		t.Fatalf("WriteEngram: %v", err)
	}

	// Register vault name (now the caller's responsibility, under vaultOpsMu in engine).
	if err := store.WriteVaultName(wsTarget, "target-vault"); err != nil {
		t.Fatalf("WriteVaultName: %v", err)
	}
	// Clone source → target.
	copied, err := store.CloneVaultData(ctx, wsSource, wsTarget, nil)
	if err != nil {
		t.Fatalf("CloneVaultData: %v", err)
	}
	if copied != 1 {
		t.Errorf("expected 1 engram copied, got %d", copied)
	}

	// Verify target contains the engram.
	got, err := store.GetEngram(ctx, wsTarget, id)
	if err != nil {
		t.Fatalf("GetEngram in target: %v", err)
	}
	if got.Concept != eng.Concept {
		t.Errorf("concept mismatch: got %q, want %q", got.Concept, eng.Concept)
	}
	if got.Content != eng.Content {
		t.Errorf("content mismatch: got %q, want %q", got.Content, eng.Content)
	}
}

// TestCloneVaultData_AccessCountReset verifies the asymmetry: AccessCount is
// zeroed in the clone, LastAccess is CARRIED OVER from the source.
func TestCloneVaultData_AccessCountReset(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	wsSource := store.VaultPrefix("src-ac")
	wsTarget := store.VaultPrefix("dst-ac")

	now := time.Now()
	created := now.Add(-48 * time.Hour)
	eng := &Engram{
		Concept:     "access count test",
		Content:     "some content",
		AccessCount: 42,
		LastAccess:  now,
		CreatedAt:   created,
		UpdatedAt:   created,
	}
	id, err := store.WriteEngram(ctx, wsSource, eng)
	if err != nil {
		t.Fatalf("WriteEngram: %v", err)
	}

	if err := store.WriteVaultName(wsTarget, "dst-ac"); err != nil {
		t.Fatalf("WriteVaultName: %v", err)
	}
	if _, err := store.CloneVaultData(ctx, wsSource, wsTarget, nil); err != nil {
		t.Fatalf("CloneVaultData: %v", err)
	}

	got, err := store.GetEngram(ctx, wsTarget, id)
	if err != nil {
		t.Fatalf("GetEngram target: %v", err)
	}

	if got.AccessCount != 0 {
		t.Errorf("AccessCount should be 0 after clone, got %d", got.AccessCount)
	}
	// #810, twice over. The FIRST version of this assertion required LastAccess
	// to be BEFORE the Unix epoch — it PINNED the defect: clone wrote
	// time.Time{}, ERF stored uint64(time.Time{}.UnixNano()), it decoded as
	// 1754-08-30, and the test asserted on that garbage as if it were the
	// intended reset value. The SECOND required LastAccess == CreatedAt, which
	// removed the sentinel but rewound an actively-used memory's recency to its
	// creation date — the same silently-empty recall by another route (see
	// TestClone_AgedActivelyUsedSource_RecallEquivalent in internal/engine).
	//
	// The rule now: the counter resets, the timestamp survives.
	if !got.LastAccess.Equal(now) {
		t.Errorf("LastAccess should be carried over from the source, got %v want %v",
			got.LastAccess.UTC(), now.UTC())
	}
	if !got.LastAccess.After(got.CreatedAt) {
		t.Errorf("LastAccess (%v) should still be after CreatedAt (%v) — the source's relative recency must survive the clone",
			got.LastAccess.UTC(), got.CreatedAt.UTC())
	}
}

// TestCloneVaultData_VaultCountComputedNotCopied verifies that the target vault's
// count equals the number of engrams actually copied, not the source count key.
func TestCloneVaultData_VaultCountComputedNotCopied(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	wsSource := store.VaultPrefix("src-cnt")
	wsTarget := store.VaultPrefix("dst-cnt")

	// Write 3 engrams to source.
	for i := 0; i < 3; i++ {
		_, err := store.WriteEngram(ctx, wsSource, &Engram{
			Concept: "concept",
			Content: "content",
		})
		if err != nil {
			t.Fatalf("WriteEngram[%d]: %v", i, err)
		}
	}

	if err := store.WriteVaultName(wsTarget, "dst-cnt"); err != nil {
		t.Fatalf("WriteVaultName: %v", err)
	}
	copied, err := store.CloneVaultData(ctx, wsSource, wsTarget, nil)
	if err != nil {
		t.Fatalf("CloneVaultData: %v", err)
	}
	if copied != 3 {
		t.Errorf("expected 3 engrams copied, got %d", copied)
	}

	// Verify the stored VaultCountKey value equals 3.
	vaultCountKey := keys.VaultCountKey(wsTarget)
	val, err := Get(store.db, vaultCountKey)
	if err != nil {
		t.Fatalf("Get VaultCountKey: %v", err)
	}
	if len(val) != 8 {
		t.Fatalf("VaultCountKey value has unexpected length %d", len(val))
	}
	storedCount := int64(binary.BigEndian.Uint64(val))
	if storedCount != 3 {
		t.Errorf("stored vault count = %d, want 3", storedCount)
	}

	// Also verify in-memory counter.
	inMemCount := store.GetVaultCount(ctx, wsTarget)
	if inMemCount != 3 {
		t.Errorf("in-memory vault count = %d, want 3", inMemCount)
	}
}

// TestCloneVaultData_0x15VaultCountKeySkipped verifies that the 9-byte
// VaultCountKey from the source is NOT directly copied into the target keyspace.
// The test writes extra 0x15-prefix data (a count key) and checks that the target
// only has the newly computed count key value.
func TestCloneVaultData_0x15VaultCountKeySkipped(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	wsSource := store.VaultPrefix("src-skip15")
	wsTarget := store.VaultPrefix("dst-skip15")

	// Manually write an inflated VaultCountKey in source (simulates a corrupt/old count).
	srcCountKey := keys.VaultCountKey(wsSource)
	inflatedBuf := make([]byte, 8)
	binary.BigEndian.PutUint64(inflatedBuf, 9999)
	if err := store.db.Set(srcCountKey, inflatedBuf, nil); err != nil {
		t.Fatalf("write inflated count: %v", err)
	}

	// Write 1 real engram.
	_, err := store.WriteEngram(ctx, wsSource, &Engram{Concept: "c", Content: "x"})
	if err != nil {
		t.Fatalf("WriteEngram: %v", err)
	}

	if err := store.WriteVaultName(wsTarget, "dst-skip15"); err != nil {
		t.Fatalf("WriteVaultName: %v", err)
	}
	copied, err := store.CloneVaultData(ctx, wsSource, wsTarget, nil)
	if err != nil {
		t.Fatalf("CloneVaultData: %v", err)
	}

	// Target count must equal the real engram count (1), not the inflated source value.
	dstCountKey := keys.VaultCountKey(wsTarget)
	val, err := Get(store.db, dstCountKey)
	if err != nil {
		t.Fatalf("Get dst VaultCountKey: %v", err)
	}
	if len(val) != 8 {
		t.Fatalf("dst count key length %d, want 8", len(val))
	}
	dstCount := int64(binary.BigEndian.Uint64(val))
	if dstCount != copied {
		t.Errorf("dst vault count = %d, want %d (copied engrams)", dstCount, copied)
	}
	if dstCount == 9999 {
		t.Error("dst vault count is the inflated source value — VaultCountKey was not skipped/recomputed")
	}
}

// TestMergeVaultData_AllMemoriesInTarget writes distinct engrams to source and
// target, merges, and verifies all engrams are present in target afterward.
func TestMergeVaultData_AllMemoriesInTarget(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	wsSrc := store.VaultPrefix("merge-src")
	wsDst := store.VaultPrefix("merge-dst")

	// Write 2 engrams to source.
	srcIDs := make([]ULID, 2)
	for i := 0; i < 2; i++ {
		id, err := store.WriteEngram(ctx, wsSrc, &Engram{
			Concept: "src concept",
			Content: "src content",
		})
		if err != nil {
			t.Fatalf("WriteEngram src[%d]: %v", i, err)
		}
		srcIDs[i] = id
	}

	// Write 1 engram to target.
	dstID, err := store.WriteEngram(ctx, wsDst, &Engram{
		Concept: "dst concept",
		Content: "dst content",
	})
	if err != nil {
		t.Fatalf("WriteEngram dst: %v", err)
	}

	merged, err := store.MergeVaultData(ctx, wsSrc, wsDst, nil)
	if err != nil {
		t.Fatalf("MergeVaultData: %v", err)
	}
	if merged != 2 {
		t.Errorf("expected 2 engrams merged, got %d", merged)
	}

	// Verify all 3 engrams exist in target.
	allIDs := append(srcIDs, dstID)
	for _, id := range allIDs {
		got, err := store.GetEngram(ctx, wsDst, id)
		if err != nil {
			t.Fatalf("GetEngram target id=%v: %v", id, err)
		}
		if got == nil {
			t.Errorf("engram %v not found in target after merge", id)
		}
	}
}

// TestMergeVaultData_AccessCountPreserved verifies that merge does NOT reset
// AccessCount or LastAccess (unlike clone).
func TestMergeVaultData_AccessCountPreserved(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	wsSrc := store.VaultPrefix("merge-ac-src")
	wsDst := store.VaultPrefix("merge-ac-dst")

	now := time.Now()
	eng := &Engram{
		Concept:     "preserve access",
		Content:     "content",
		AccessCount: 17,
		LastAccess:  now,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	id, err := store.WriteEngram(ctx, wsSrc, eng)
	if err != nil {
		t.Fatalf("WriteEngram: %v", err)
	}

	if _, err := store.MergeVaultData(ctx, wsSrc, wsDst, nil); err != nil {
		t.Fatalf("MergeVaultData: %v", err)
	}

	got, err := store.GetEngram(ctx, wsDst, id)
	if err != nil {
		t.Fatalf("GetEngram: %v", err)
	}

	if got.AccessCount != 17 {
		t.Errorf("AccessCount should be preserved (17) after merge, got %d", got.AccessCount)
	}
	if got.LastAccess.IsZero() {
		t.Error("LastAccess should not be zero after merge")
	}
}

// TestMergeVaultData_ULIDCollisionSkipsSource verifies that when the same ULID
// exists in both source and target, the merge keeps the target version and logs
// a WARN (skips the source engram).
func TestMergeVaultData_ULIDCollisionSkipsSource(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	wsSrc := store.VaultPrefix("coll-src")
	wsDst := store.VaultPrefix("coll-dst")

	// Write the collision engram to source with a specific concept.
	srcEng := &Engram{
		Concept: "SOURCE concept",
		Content: "source content",
	}
	id, err := store.WriteEngram(ctx, wsSrc, srcEng)
	if err != nil {
		t.Fatalf("WriteEngram src: %v", err)
	}

	// Write the same ULID to target with a different concept.
	// We do this by writing to the target vault using the same ID.
	dstEng := &Engram{
		ID:      id,
		Concept: "TARGET concept",
		Content: "target content",
	}
	_, err = store.WriteEngram(ctx, wsDst, dstEng)
	if err != nil {
		t.Fatalf("WriteEngram dst: %v", err)
	}

	merged, err := store.MergeVaultData(ctx, wsSrc, wsDst, nil)
	if err != nil {
		t.Fatalf("MergeVaultData: %v", err)
	}

	// The collision engram must not have been counted as merged.
	if merged != 0 {
		t.Errorf("expected 0 engrams merged (all collisions), got %d", merged)
	}

	// Target must still have its own version (TARGET concept).
	got, err := store.GetEngram(ctx, wsDst, id)
	if err != nil {
		t.Fatalf("GetEngram: %v", err)
	}
	if got.Concept != "TARGET concept" {
		t.Errorf("expected target concept to be preserved, got %q", got.Concept)
	}
}

// TestCloneVaultData_CrossVaultIsolation verifies that after a clone the source
// vault remains unchanged.
func TestCloneVaultData_CrossVaultIsolation(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	wsSrc := store.VaultPrefix("iso-src")
	wsDst := store.VaultPrefix("iso-dst")

	// Write 2 engrams to source.
	var srcIDs []ULID
	for i := 0; i < 2; i++ {
		id, err := store.WriteEngram(ctx, wsSrc, &Engram{
			Concept: "source concept",
			Content: "source content",
		})
		if err != nil {
			t.Fatalf("WriteEngram src[%d]: %v", i, err)
		}
		srcIDs = append(srcIDs, id)
	}

	srcCountBefore := store.GetVaultCount(ctx, wsSrc)

	if err := store.WriteVaultName(wsDst, "iso-dst"); err != nil {
		t.Fatalf("WriteVaultName: %v", err)
	}
	if _, err := store.CloneVaultData(ctx, wsSrc, wsDst, nil); err != nil {
		t.Fatalf("CloneVaultData: %v", err)
	}

	// Source engrams must still be readable.
	for _, id := range srcIDs {
		got, err := store.GetEngram(ctx, wsSrc, id)
		if err != nil {
			t.Fatalf("GetEngram src %v: %v", id, err)
		}
		if got == nil {
			t.Errorf("source engram %v was deleted by clone", id)
		}
	}

	// Source count must be unchanged.
	srcCountAfter := store.GetVaultCount(ctx, wsSrc)
	if srcCountAfter != srcCountBefore {
		t.Errorf("source vault count changed from %d to %d after clone", srcCountBefore, srcCountAfter)
	}
}
