package engine

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/engine/trigger"
	"github.com/scrypster/muninndb/internal/index/fts"
	"github.com/scrypster/muninndb/internal/storage"
)

// ptr is a generic helper to take the address of any value.
func ptr[T any](v T) *T { return &v }

// testEnvWithAuth wires up a fully functional Engine with a real auth.Store,
// real storage and FTS, using a temporary directory cleaned up after the test.
// Returns the engine, the auth.Store, the pebble store, and a cleanup func.
func testEnvWithAuth(t *testing.T) (*Engine, *auth.Store, *storage.PebbleStore, func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "muninndb-pruning-test-*")
	if err != nil {
		t.Fatal(err)
	}

	db, err := storage.OpenPebble(dir, storage.DefaultOptions())
	if err != nil {
		os.RemoveAll(dir)
		t.Fatal(err)
	}

	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 1000})
	ftsIdx := fts.New(db)

	embedder := &noopEmbedder{}
	actEngine := activation.New(store, &ftsAdapter{ftsIdx}, nil, embedder)
	trigSystem := trigger.New(store, &ftsTrigAdapter{ftsIdx}, nil, embedder)

	as := auth.NewStore(db)
	eng := NewEngine(EngineConfig{Store: store, AuthStore: as, FTSIndex: ftsIdx, ActivationEngine: actEngine, TriggerSystem: trigSystem, Embedder: embedder})

	return eng, as, store, func() {
		eng.Stop()
		store.Close()
		os.RemoveAll(dir)
	}
}

// TestPruneVault_MaxEngrams verifies that PruneVault removes the lowest-relevance
// engrams when the vault count exceeds MaxEngrams.
func TestPruneVault_MaxEngrams(t *testing.T) {
	eng, as, store, cleanup := testEnvWithAuth(t)
	defer cleanup()
	ctx := context.Background()

	const vaultName = "prunetest"
	ws := store.VaultPrefix(vaultName)

	// Register the vault name so PruneVault and ReindexFTSVault can find it.
	if err := store.WriteVaultName(ws, vaultName); err != nil {
		t.Fatalf("WriteVaultName: %v", err)
	}

	// Write 5 engrams to the vault.
	for i := 0; i < 5; i++ {
		engram := &storage.Engram{
			Concept: "test concept",
			Content: "test content for pruning",
		}
		if _, err := store.WriteEngram(ctx, ws, engram); err != nil {
			t.Fatalf("WriteEngram[%d]: %v", i, err)
		}
	}

	// Capture the actual count before pruning (may be >= 5 due to any
	// background initialization; we use it to verify invariants hold after prune).
	totalBefore := store.GetVaultCount(ctx, ws)
	if totalBefore < 5 {
		t.Fatalf("expected at least 5 engrams before prune, got %d", totalBefore)
	}

	// Set PlasticityConfig with MaxEngrams=3 for the vault.
	if err := as.SetVaultConfig(auth.VaultConfig{
		Name:   vaultName,
		Public: true,
		Plasticity: &auth.PlasticityConfig{
			MaxEngrams: ptr(3),
		},
	}); err != nil {
		t.Fatalf("SetVaultConfig: %v", err)
	}

	pruned, err := eng.PruneVault(ctx, vaultName)
	if err != nil {
		t.Fatalf("PruneVault: %v", err)
	}

	// pruned + remaining must equal total before pruning.
	countAfter := store.GetVaultCount(ctx, ws)
	if pruned+countAfter != totalBefore {
		t.Errorf("pruned(%d) + remaining(%d) != totalBefore(%d)", pruned, countAfter, totalBefore)
	}

	// Vault must have at most MaxEngrams (3) after pruning.
	if countAfter != 3 {
		t.Errorf("expected 3 engrams after prune, got %d", countAfter)
	}

	// At least totalBefore-3 engrams must have been pruned.
	wantPruned := totalBefore - 3
	if pruned < wantPruned {
		t.Errorf("expected at least %d pruned, got %d", wantPruned, pruned)
	}
}

// TestPruneVault_COG20_HighImportanceExempt is the COG-20 pin: an engram with
// EffectiveImportance >= HighImportanceFloor is never hard-deleted by the
// MaxEngrams (retrieval-strength) prune path, even when it has the LOWEST
// ACT-R base-level of every candidate — the next-worst engrams are deleted
// instead. The important engram is also EXPIRED on the valid-time axis
// (closed ValidUntil in the past), pinning the composition rule: the
// exemption performs no validity check, so an expired-but-important fact
// stays reachable via as_of forever (modulo RetentionDays).
func TestPruneVault_COG20_HighImportanceExempt(t *testing.T) {
	eng, as, store, cleanup := testEnvWithAuth(t)
	defer cleanup()
	ctx := context.Background()

	const vaultName = "cog20test"
	ws := store.VaultPrefix(vaultName)
	if err := store.WriteVaultName(ws, vaultName); err != nil {
		t.Fatalf("WriteVaultName: %v", err)
	}

	now := time.Now()
	write := func(concept string, relevance float32, lastAccess time.Time, importance float32, validUntil time.Time) storage.ULID {
		t.Helper()
		id, err := store.WriteEngram(ctx, ws, &storage.Engram{
			Concept:    concept,
			Content:    "cog20 " + concept,
			Relevance:  relevance,
			CreatedAt:  now.Add(-120 * 24 * time.Hour),
			LastAccess: lastAccess,
			Importance: importance,
			ValidUntil: validUntil,
		})
		if err != nil {
			t.Fatalf("WriteEngram %s: %v", concept, err)
		}
		return id
	}

	// The important engram is the WORST candidate on every axis the pruner
	// looks at: lowest relevance (first out of the bucket index prefilter) and
	// oldest LastAccess (lowest B(M) in the rescoring phase). Without the
	// COG-20 exemption it is deleted first. It is also already expired.
	importantID := write("important-but-cold", 0.0, now.Add(-100*24*time.Hour), 0.9, now.Add(-24*time.Hour))
	staleA := write("stale-a", 0.1, now.Add(-90*24*time.Hour), 0, time.Time{})
	staleB := write("stale-b", 0.2, now.Add(-80*24*time.Hour), 0, time.Time{})
	write("fresh-a", 0.3, now, 0, time.Time{})
	write("fresh-b", 0.4, now, 0, time.Time{})

	// Snapshot the counter (it may run ahead of the write count — see
	// TestPruneVault_MaxEngrams) and configure MaxEngrams for an excess of
	// exactly 2, so the two worst NON-exempt candidates are pruned.
	totalBefore := store.GetVaultCount(ctx, ws)
	if err := as.SetVaultConfig(auth.VaultConfig{
		Name: vaultName, Public: true,
		Plasticity: &auth.PlasticityConfig{MaxEngrams: ptr(int(totalBefore) - 2)},
	}); err != nil {
		t.Fatalf("SetVaultConfig: %v", err)
	}

	pruned, err := eng.PruneVault(ctx, vaultName)
	if err != nil {
		t.Fatalf("PruneVault: %v", err)
	}
	if pruned != 2 {
		t.Errorf("pruned = %d, want 2 (excess with enough non-exempt candidates)", pruned)
	}

	// The important engram must survive despite being the lowest-B(M) candidate.
	if _, err := store.GetEngram(ctx, ws, importantID); err != nil {
		t.Fatalf("COG-20 violated: high-importance engram was pruned: %v", err)
	}
	// The next-worst (stale, unimportant) engrams are the ones deleted.
	if _, err := store.GetEngram(ctx, ws, staleA); err == nil {
		t.Errorf("stale-a should have been pruned in the important engram's place")
	}
	if _, err := store.GetEngram(ctx, ws, staleB); err == nil {
		t.Errorf("stale-b should have been pruned in the important engram's place")
	}
	if count := store.GetVaultCount(ctx, ws); count != totalBefore-2 {
		t.Errorf("vault count after prune = %d, want %d", count, totalBefore-2)
	}
}

// TestPruneVault_ImportanceExemptionWarns verifies the degrade-loudly rule:
// when high-importance exemptions leave the vault over MaxEngrams
// (pruned < excess), PruneVault logs a WARN naming the vault — and does NOT
// loop or delete protected engrams to force the target.
func TestPruneVault_ImportanceExemptionWarns(t *testing.T) {
	eng, as, store, cleanup := testEnvWithAuth(t)
	defer cleanup()
	ctx := context.Background()

	const vaultName = "cog20warn"
	ws := store.VaultPrefix(vaultName)
	if err := store.WriteVaultName(ws, vaultName); err != nil {
		t.Fatalf("WriteVaultName: %v", err)
	}

	now := time.Now()
	// 4 protected engrams + 1 prunable; MaxEngrams below 4 → excess exists but only
	// 1 non-exempt candidate → pruned < excess → WARN, and no protected deletion.
	var protectedIDs [4]storage.ULID
	for i := 0; i < 4; i++ {
		id, err := store.WriteEngram(ctx, ws, &storage.Engram{
			Concept:    fmt.Sprintf("protected-%d", i),
			Content:    fmt.Sprintf("cog20warn protected %d", i),
			CreatedAt:  now.Add(-120 * 24 * time.Hour),
			LastAccess: now.Add(-100 * 24 * time.Hour),
			// Higher relevance than the prunable so the importance-blind
			// LowestRelevanceIDs prefilter never selects a protected engram ahead
			// of the prunable one (which would leave the prunable out of the
			// candidate pool and prune nothing — the source of the old flake).
			Relevance:  0.5,
			Importance: 0.9,
		})
		if err != nil {
			t.Fatalf("WriteEngram protected-%d: %v", i, err)
		}
		protectedIDs[i] = id
	}
	prunableID, err := store.WriteEngram(ctx, ws, &storage.Engram{
		Concept:    "prunable",
		Content:    "cog20warn prunable",
		CreatedAt:  now.Add(-120 * 24 * time.Hour),
		LastAccess: now.Add(-100 * 24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("WriteEngram prunable: %v", err)
	}

	// Wait for the vault counter to settle before configuring MaxEngrams. The
	// counter is eventually-consistent under load (a first-write can transiently
	// double-count), so a raw GetVaultCount snapshot here is racy — poll until two
	// consecutive reads agree. MaxEngrams is then set below the 4 exempt engrams so
	// the vault necessarily ends over the target regardless of the exact count.
	stableCount := func() int64 {
		last := store.GetVaultCount(ctx, ws)
		for i := 0; i < 100; i++ {
			time.Sleep(5 * time.Millisecond)
			cur := store.GetVaultCount(ctx, ws)
			if cur == last {
				return cur
			}
			last = cur
		}
		return last
	}()
	if err := as.SetVaultConfig(auth.VaultConfig{
		Name: vaultName, Public: true,
		Plasticity: &auth.PlasticityConfig{MaxEngrams: ptr(int(stableCount) - 2)},
	}); err != nil {
		t.Fatalf("SetVaultConfig: %v", err)
	}

	// Capture WARN records emitted during the prune.
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	pruned, err := eng.PruneVault(ctx, vaultName)
	slog.SetDefault(prev)
	if err != nil {
		t.Fatalf("PruneVault: %v", err)
	}

	// Deterministic guarantees, independent of the exact counter: the single
	// non-exempt candidate is pruned, every high-importance engram SURVIVES
	// (COG-20), and the vault is left over MaxEngrams so the WARN fires.
	if pruned < 1 {
		t.Errorf("pruned = %d, want >= 1 (the non-exempt candidate must be removed)", pruned)
	}
	for i := 0; i < 4; i++ {
		if eng, _ := store.GetEngram(ctx, ws, protectedIDs[i]); eng == nil || eng.State == storage.StateArchived || eng.State == storage.StateSoftDeleted {
			t.Errorf("protected-%d was pruned; COG-20 exemption must never delete a high-importance engram", i)
		}
	}
	if eng, _ := store.GetEngram(ctx, ws, prunableID); eng != nil && eng.State != storage.StateArchived && eng.State != storage.StateSoftDeleted {
		t.Errorf("the non-exempt prunable engram should have been pruned")
	}
	logged := buf.String()
	if !strings.Contains(logged, "high-importance exemptions left vault over MaxEngrams") ||
		!strings.Contains(logged, vaultName) {
		t.Errorf("expected WARN about exemptions leaving vault over MaxEngrams; got logs:\n%s", logged)
	}
}

// TestPruneVault_RetentionDays verifies that PruneVault removes engrams older
// than the configured RetentionDays threshold and keeps recent ones.
func TestPruneVault_RetentionDays(t *testing.T) {
	eng, as, store, cleanup := testEnvWithAuth(t)
	defer cleanup()
	ctx := context.Background()

	const vaultName = "retentiontest"
	ws := store.VaultPrefix(vaultName)

	if err := store.WriteVaultName(ws, vaultName); err != nil {
		t.Fatalf("WriteVaultName: %v", err)
	}

	// Write 2 engrams with CreatedAt = 30 days ago.
	oldTime := time.Now().Add(-30 * 24 * time.Hour)
	for i := 0; i < 2; i++ {
		engram := &storage.Engram{
			CreatedAt: oldTime,
			Concept:   "old thing",
			Content:   "test content old",
		}
		if _, err := store.WriteEngram(ctx, ws, engram); err != nil {
			t.Fatalf("WriteEngram old[%d]: %v", i, err)
		}
	}

	// Write 2 engrams with CreatedAt = now.
	for i := 0; i < 2; i++ {
		engram := &storage.Engram{
			Concept: "recent thing",
			Content: "test content recent",
		}
		if _, err := store.WriteEngram(ctx, ws, engram); err != nil {
			t.Fatalf("WriteEngram recent[%d]: %v", i, err)
		}
	}

	// Confirm we have at least 4 engrams before pruning (may be more due to
	// any background initialization).
	totalBefore := store.GetVaultCount(ctx, ws)
	if totalBefore < 4 {
		t.Fatalf("expected at least 4 engrams before prune, got %d", totalBefore)
	}

	// Set RetentionDays=14 so 30-day-old engrams are pruned.
	if err := as.SetVaultConfig(auth.VaultConfig{
		Name:   vaultName,
		Public: true,
		Plasticity: &auth.PlasticityConfig{
			RetentionDays: ptr(float32(14)),
		},
	}); err != nil {
		t.Fatalf("SetVaultConfig: %v", err)
	}

	pruned, err := eng.PruneVault(ctx, vaultName)
	if err != nil {
		t.Fatalf("PruneVault: %v", err)
	}

	// Expect exactly 2 pruned (only the old ones, regardless of any extra engrams).
	if pruned != 2 {
		t.Errorf("expected 2 pruned (the 30-day-old engrams), got %d", pruned)
	}

	// After pruning: totalBefore - 2 should remain.
	countAfter := store.GetVaultCount(ctx, ws)
	expectedAfter := totalBefore - 2
	if countAfter != expectedAfter {
		t.Errorf("expected %d remaining after retention prune, got %d", expectedAfter, countAfter)
	}
}

func TestPruneWorker_GracefulShutdown(t *testing.T) {
	eng, _, _, cleanup := testEnvWithAuth(t)
	defer cleanup()

	done := make(chan struct{})
	go func() {
		eng.Stop()
		close(done)
	}()

	select {
	case <-done:
		// Stop() returned — prune worker exited cleanly
	case <-time.After(5 * time.Second):
		t.Fatal("eng.Stop() did not return within 5s — prune worker goroutine may be leaked")
	}
}

// TestPruneVault_VaultIsolation verifies that pruning one vault does not affect
// engrams in another vault.
func TestPruneVault_VaultIsolation(t *testing.T) {
	eng, as, store, cleanup := testEnvWithAuth(t)
	defer cleanup()
	ctx := context.Background()

	const vaultA = "prune-vault-a"
	const vaultB = "prune-vault-b"

	wsA := store.VaultPrefix(vaultA)
	wsB := store.VaultPrefix(vaultB)

	if err := store.WriteVaultName(wsA, vaultA); err != nil {
		t.Fatalf("WriteVaultName vaultA: %v", err)
	}
	if err := store.WriteVaultName(wsB, vaultB); err != nil {
		t.Fatalf("WriteVaultName vaultB: %v", err)
	}

	// Write 5 engrams to vault-A.
	for i := 0; i < 5; i++ {
		engram := &storage.Engram{
			Concept: "vault-a concept",
			Content: "vault-a content",
		}
		if _, err := store.WriteEngram(ctx, wsA, engram); err != nil {
			t.Fatalf("WriteEngram vault-a[%d]: %v", i, err)
		}
	}

	// Write 2 engrams to vault-B (no limit).
	for i := 0; i < 2; i++ {
		engram := &storage.Engram{
			Concept: "vault-b concept",
			Content: "vault-b content",
		}
		if _, err := store.WriteEngram(ctx, wsB, engram); err != nil {
			t.Fatalf("WriteEngram vault-b[%d]: %v", i, err)
		}
	}

	// Snapshot counts before prune.
	countABefore := store.GetVaultCount(ctx, wsA)
	countBBefore := store.GetVaultCount(ctx, wsB)

	if countABefore < 5 {
		t.Fatalf("vault-A: expected at least 5 engrams before prune, got %d", countABefore)
	}
	if countBBefore < 2 {
		t.Fatalf("vault-B: expected at least 2 engrams before prune, got %d", countBBefore)
	}

	// Set MaxEngrams=3 only on vault-A.
	if err := as.SetVaultConfig(auth.VaultConfig{
		Name:   vaultA,
		Public: true,
		Plasticity: &auth.PlasticityConfig{
			MaxEngrams: ptr(3),
		},
	}); err != nil {
		t.Fatalf("SetVaultConfig vaultA: %v", err)
	}

	// Prune only vault-A.
	_, err := eng.PruneVault(ctx, vaultA)
	if err != nil {
		t.Fatalf("PruneVault vault-A: %v", err)
	}

	// Vault-A must be trimmed to MaxEngrams (3).
	countA := store.GetVaultCount(ctx, wsA)
	if countA != 3 {
		t.Errorf("vault-A: expected 3 engrams after prune, got %d", countA)
	}

	// Vault-B must be completely untouched.
	countB := store.GetVaultCount(ctx, wsB)
	if countB != countBBefore {
		t.Errorf("vault-B: expected %d engrams (untouched), got %d", countBBefore, countB)
	}
}

// TestAssocDecay_PrunesWeakEdges verifies that DecayAssocWeights (called from
// the prune worker) decays weak association edges while keeping strong ones.
//
// Dynamic floor semantics (Task 5): an edge that falls below minWeight is NOT
// deleted if its PeakWeight > 0. Instead it is clamped to PeakWeight * 0.05.
// An edge written at weight 0.03 seeds PeakWeight=0.03, so its floor is
// 0.03 * 0.05 = 0.0015. The edge survives, clamped to the floor.
func TestAssocDecay_PrunesWeakEdges(t *testing.T) {
	_, _, store, cleanup := testEnvWithAuth(t)
	defer cleanup()
	ctx := context.Background()

	const vaultName = "assoc-decay-test"
	ws := store.VaultPrefix(vaultName)
	if err := store.WriteVaultName(ws, vaultName); err != nil {
		t.Fatalf("WriteVaultName: %v", err)
	}

	// Create two engrams to serve as association endpoints.
	engA := &storage.Engram{Concept: "node A", Content: "content A"}
	engB := &storage.Engram{Concept: "node B", Content: "content B"}
	engC := &storage.Engram{Concept: "node C", Content: "content C"}

	idA, err := store.WriteEngram(ctx, ws, engA)
	if err != nil {
		t.Fatalf("WriteEngram A: %v", err)
	}
	idB, err := store.WriteEngram(ctx, ws, engB)
	if err != nil {
		t.Fatalf("WriteEngram B: %v", err)
	}
	idC, err := store.WriteEngram(ctx, ws, engC)
	if err != nil {
		t.Fatalf("WriteEngram C: %v", err)
	}

	// Write a strong association (A→B, weight 0.8) and a weak one (A→C, weight 0.03).
	if err := store.WriteAssociation(ctx, ws, idA, idB, &storage.Association{
		TargetID: idB, Weight: 0.8, Confidence: 1.0, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("WriteAssociation A→B: %v", err)
	}
	if err := store.WriteAssociation(ctx, ws, idA, idC, &storage.Association{
		TargetID: idC, Weight: 0.03, Confidence: 1.0, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("WriteAssociation A→C: %v", err)
	}

	// Verify both edges exist before decay.
	wAB, _ := store.GetAssocWeight(ctx, ws, idA, idB)
	wAC, _ := store.GetAssocWeight(ctx, ws, idA, idC)
	if wAB == 0 {
		t.Fatal("expected A→B association to exist before decay")
	}
	if wAC == 0 {
		t.Fatal("expected A→C association to exist before decay")
	}

	// Run decay with factor=0.95, minWeight=0.05.
	// A→B: 0.8 * 0.95 = 0.76 (survives above threshold)
	// A→C: 0.03 * 0.95 = 0.0285 < 0.05, but PeakWeight=0.03 → floor=0.0015 → clamped (not deleted)
	removed, err := store.DecayAssocWeights(ctx, ws, 0.95, 0.05, 0.0)
	if err != nil {
		t.Fatalf("DecayAssocWeights: %v", err)
	}
	// Dynamic floor: weak edge is clamped, not deleted.
	if removed != 0 {
		t.Errorf("expected 0 edges removed (dynamic floor clamps weak edges), got %d", removed)
	}

	// Strong edge should survive with decayed weight.
	wAB, _ = store.GetAssocWeight(ctx, ws, idA, idB)
	if wAB == 0 {
		t.Error("strong edge A→B should survive decay")
	}
	if wAB > 0.8 || wAB < 0.7 {
		t.Errorf("expected A→B weight ~0.76, got %f", wAB)
	}

	// Weak edge should be clamped to dynamic floor (PeakWeight * 0.05 = 0.03 * 0.05 = 0.0015),
	// not deleted. It must still exist (weight > 0) and be at or near the floor.
	wAC, _ = store.GetAssocWeight(ctx, ws, idA, idC)
	if wAC == 0 {
		t.Error("weak edge A→C should be clamped to dynamic floor, not deleted")
	}
	const expectedFloor = float32(0.03 * 0.05) // 0.0015
	if wAC > expectedFloor+0.001 {
		t.Errorf("weak edge A→C weight %f exceeds expected floor ~%f", wAC, expectedFloor)
	}
}

// TestPruneVault_ImportanceProtection_Measured is the quantified value proof for
// importance-aware pruning: a batch of important decisions that are ALSO the
// coldest, lowest-relevance memories in the vault (exactly what a recency/
// relevance pruner deletes first) must ALL survive heavy memory pressure, while
// the same number of noise memories is dropped instead. It logs the measured
// before/after so the value is a number, not a vibe.
func TestPruneVault_ImportanceProtection_Measured(t *testing.T) {
	eng, as, store, cleanup := testEnvWithAuth(t)
	defer cleanup()
	ctx := context.Background()

	const vaultName = "importance-measure"
	ws := store.VaultPrefix(vaultName)
	if err := store.WriteVaultName(ws, vaultName); err != nil {
		t.Fatalf("WriteVaultName: %v", err)
	}

	now := time.Now()
	const nImportant, nNoise = 8, 24

	// Important decisions: explicit importance 0.9, but the COLDEST in the vault —
	// lowest relevance (front of the LowestRelevanceIDs prefilter) and oldest
	// access (lowest B(M)). A recency/relevance pruner targets these FIRST.
	important := make([]storage.ULID, 0, nImportant)
	for i := 0; i < nImportant; i++ {
		id, err := store.WriteEngram(ctx, ws, &storage.Engram{
			Concept:    "important-decision",
			Content:    "a decision that must not be forgotten",
			Relevance:  0.0,
			CreatedAt:  now.Add(-120 * 24 * time.Hour),
			LastAccess: now.Add(-100 * 24 * time.Hour),
			Importance: 0.9,
		})
		if err != nil {
			t.Fatalf("write important[%d]: %v", i, err)
		}
		important = append(important, id)
	}
	// Noise: unimportant, but FRESHER and higher relevance — a recency pruner
	// would keep these over the important-but-cold decisions.
	for i := 0; i < nNoise; i++ {
		if _, err := store.WriteEngram(ctx, ws, &storage.Engram{
			Concept:    "noise",
			Content:    "low-value chatter",
			Relevance:  0.5,
			CreatedAt:  now,
			LastAccess: now,
		}); err != nil {
			t.Fatalf("write noise[%d]: %v", i, err)
		}
	}

	total := store.GetVaultCount(ctx, ws)
	// Squeeze hard: keep only ~a third. Prune must drop far more than nImportant,
	// so a recency pruner would necessarily delete every important-but-cold one.
	keep := int(total) - (nImportant + nNoise/2) // drops 8 + 12 = 20
	if err := as.SetVaultConfig(auth.VaultConfig{
		Name: vaultName, Public: true,
		Plasticity: &auth.PlasticityConfig{MaxEngrams: ptr(keep)},
	}); err != nil {
		t.Fatalf("SetVaultConfig: %v", err)
	}

	pruned, err := eng.PruneVault(ctx, vaultName)
	if err != nil {
		t.Fatalf("PruneVault: %v", err)
	}

	survived := 0
	for _, id := range important {
		if _, err := store.GetEngram(ctx, ws, id); err == nil {
			survived++
		}
	}
	t.Logf("MEASURE importance-protection: vault %d -> pruned %d (target drop ~%d). "+
		"Important decisions (the %d COLDEST, lowest-relevance memories — a recency pruner's first targets): %d/%d survived.",
		total, pruned, int(total)-keep, nImportant, survived, nImportant)
	if survived != nImportant {
		t.Errorf("importance protection failed: %d/%d important decisions survived; a recency pruner drops these first", survived, nImportant)
	}
	if pruned < nImportant {
		t.Errorf("prune did not exert real pressure: only %d dropped", pruned)
	}
}
