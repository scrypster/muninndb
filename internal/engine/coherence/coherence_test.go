package coherence

import (
	"math"
	"sync"
	"testing"
)

func TestEmptyVaultScore(t *testing.T) {
	c := &VaultCounters{}
	score := c.Score()
	if score != 1.0 {
		t.Errorf("empty vault score: expected 1.0, got %f", score)
	}
}

func TestOrphanRatio(t *testing.T) {
	c := &VaultCounters{}
	// Write 10 engrams with no links
	for i := 0; i < 10; i++ {
		c.RecordWrite(0.5)
	}
	// All 10 should be orphans
	orphanCount := c.OrphanCount.Load()
	if orphanCount != 10 {
		t.Errorf("orphan count: expected 10, got %d", orphanCount)
	}
	totalEngrams := c.TotalEngrams.Load()
	if totalEngrams != 10 {
		t.Errorf("total engrams: expected 10, got %d", totalEngrams)
	}

	// Score should be penalized: penalty = 1.0*0.3 = 0.3, score = 1.0 - 0.3 = 0.7
	score := c.Score()
	expected := 0.7
	if math.Abs(score-expected) > 0.001 {
		t.Errorf("orphan penalty score: expected ~%f, got %f", expected, score)
	}
}

func TestLinkReducesOrphans(t *testing.T) {
	c := &VaultCounters{}
	c.RecordWrite(0.5)
	c.RecordWrite(0.5)

	// Should have 2 orphans initially
	if c.OrphanCount.Load() != 2 {
		t.Errorf("initial orphan count: expected 2, got %d", c.OrphanCount.Load())
	}

	// Create first link from engram 1 (isFirstLink=true, isRefines=false)
	c.RecordLinkCreated(true, false)

	// Should now have 1 orphan
	if c.OrphanCount.Load() != 1 {
		t.Errorf("after first link orphan count: expected 1, got %d", c.OrphanCount.Load())
	}

	// Create another link from engram 2 (isFirstLink=true, isRefines=false)
	c.RecordLinkCreated(true, false)

	// Should now have 0 orphans
	if c.OrphanCount.Load() != 0 {
		t.Errorf("after second link orphan count: expected 0, got %d", c.OrphanCount.Load())
	}
}

func TestContradictionDensity(t *testing.T) {
	c := &VaultCounters{}
	for i := 0; i < 10; i++ {
		c.RecordWrite(0.5)
	}

	// Score with no contradictions
	scoreNone := c.Score()

	// Add 3 contradictions
	c.RecordContradictionSet()
	c.RecordContradictionSet()
	c.RecordContradictionSet()

	scoreWithContradictions := c.Score()

	// penalty for contradictions = 0.3 * 0.3 = 0.09
	if scoreWithContradictions >= scoreNone {
		t.Errorf("contradictions should reduce score: before %f, after %f", scoreNone, scoreWithContradictions)
	}

	// Resolve contradictions
	c.RecordContradictionResolved()
	c.RecordContradictionResolved()
	c.RecordContradictionResolved()

	scoreAfterResolve := c.Score()

	// After resolving all, should match original
	if math.Abs(scoreAfterResolve-scoreNone) > 0.001 {
		t.Errorf("after contradiction resolve: expected ~%f, got %f", scoreNone, scoreAfterResolve)
	}
}

func TestRefinesDuplication(t *testing.T) {
	c := &VaultCounters{}
	for i := 0; i < 5; i++ {
		c.RecordWrite(0.5)
	}

	// First, resolve orphans by creating non-refines links
	c.RecordLinkCreated(true, false)
	c.RecordLinkCreated(true, false)
	c.RecordLinkCreated(true, false)
	c.RecordLinkCreated(true, false)
	c.RecordLinkCreated(true, false)

	scoreBefore := c.Score()

	// Now add REFINES links from other sources
	// This increases duplication without changing orphan ratio
	c.RecordLinkCreated(false, true)
	c.RecordLinkCreated(false, true)
	c.RecordLinkCreated(false, true)

	scoreAfter := c.Score()

	// duplicationPressure increases from 0 to 3/5 = 0.6, penalty += 0.6 * 0.2 = 0.12
	if scoreAfter >= scoreBefore {
		t.Errorf("refines should reduce score: before %f, after %f", scoreBefore, scoreAfter)
	}

	if c.RefinesCount.Load() != 3 {
		t.Errorf("refines count: expected 3, got %d", c.RefinesCount.Load())
	}
}

func TestVarianceComputation(t *testing.T) {
	// Test uniform confidence (variance near 0)
	c1 := &VaultCounters{}
	uniform := float32(0.75)
	for i := 0; i < 100; i++ {
		c1.recordConfidence(uniform)
	}
	v1 := c1.Variance()
	if v1 > 1e-10 {
		t.Errorf("uniform confidence variance: expected ~0, got %f", v1)
	}

	// Test mixed confidence (variance > 0)
	c2 := &VaultCounters{}
	c2.recordConfidence(0.0)
	c2.recordConfidence(1.0)
	v2 := c2.Variance()
	expected := 0.25 // variance of [0, 1] is ((0-0.5)^2 + (1-0.5)^2) / 2 = 0.25
	if math.Abs(v2-expected) > 0.001 {
		t.Errorf("mixed confidence variance: expected ~%f, got %f", expected, v2)
	}

	// Test single sample (variance should be 0)
	c3 := &VaultCounters{}
	c3.recordConfidence(0.5)
	v3 := c3.Variance()
	if v3 != 0 {
		t.Errorf("single sample variance: expected 0, got %f", v3)
	}
}

func TestScoreFormula(t *testing.T) {
	c := &VaultCounters{}

	// Manually set counters for a predictable scenario
	c.TotalEngrams.Store(100)
	c.OrphanCount.Store(30)    // orphan ratio = 0.3
	c.Contradictions.Store(10) // contradiction density = 0.1
	c.RefinesCount.Store(20)   // duplication pressure = 0.2
	c.ConfidenceN.Store(1)
	c.ConfidenceSum.Store(500000)   // 0.5
	c.ConfidenceSumSq.Store(250000) // 0.25

	// Expected calculation:
	// orphanRatio = 0.3
	// contradictionDensity = 0.1
	// duplicationPressure = 0.2
	// temporalVariance = clamped(0.25 - 0.25) = 0.0
	// penalty = 0.3*0.3 + 0.1*0.3 + 0.0*0.2 + 0.2*0.2 = 0.09 + 0.03 + 0 + 0.04 = 0.16
	// score = 1.0 - 0.16 = 0.84

	score := c.Score()
	expected := 0.84
	if math.Abs(score-expected) > 0.001 {
		t.Errorf("score formula: expected ~%f, got %f", expected, score)
	}
}

func TestSnapshot(t *testing.T) {
	c := &VaultCounters{}
	for i := 0; i < 50; i++ {
		c.RecordWrite(0.7)
	}
	// Create links for half of them
	for i := 0; i < 25; i++ {
		c.RecordLinkCreated(true, false)
	}
	// Add contradictions
	for i := 0; i < 5; i++ {
		c.RecordContradictionSet()
	}

	snapshot := c.Snapshot("test-vault")

	if snapshot.VaultName != "test-vault" {
		t.Errorf("snapshot vault name: expected 'test-vault', got %s", snapshot.VaultName)
	}
	if snapshot.TotalEngrams != 50 {
		t.Errorf("snapshot total engrams: expected 50, got %d", snapshot.TotalEngrams)
	}
	if snapshot.OrphanRatio != 0.5 {
		t.Errorf("snapshot orphan ratio: expected 0.5, got %f", snapshot.OrphanRatio)
	}
	if snapshot.ContradictionDensity != 0.1 {
		t.Errorf("snapshot contradiction density: expected 0.1, got %f", snapshot.ContradictionDensity)
	}
	if snapshot.DuplicationPressure != 0.0 {
		t.Errorf("snapshot duplication pressure: expected 0.0, got %f", snapshot.DuplicationPressure)
	}
	if snapshot.Score <= 0 || snapshot.Score >= 1.0 {
		t.Errorf("snapshot score should be in (0, 1.0), got %f", snapshot.Score)
	}
}

func TestRegistryGetOrCreate(t *testing.T) {
	reg := NewRegistry()

	c1 := reg.GetOrCreate("vault-a")
	c1.RecordWrite(0.5)

	c1Again := reg.GetOrCreate("vault-a")
	if c1 != c1Again {
		t.Errorf("same vault name should return same counter")
	}
	if c1Again.TotalEngrams.Load() != 1 {
		t.Errorf("should preserve writes to same vault counter")
	}

	c2 := reg.GetOrCreate("vault-b")
	if c2 == c1 {
		t.Errorf("different vault names should return different counters")
	}
	if c2.TotalEngrams.Load() != 0 {
		t.Errorf("new vault should start at 0 engrams")
	}
}

func TestRegistryConcurrent(t *testing.T) {
	reg := NewRegistry()
	const numGoroutines = 100
	const numVaults = 10
	const writesPerGoroutine = 50

	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	for g := 0; g < numGoroutines; g++ {
		go func(goroutineID int) {
			defer wg.Done()
			vaultName := "vault-" + string(byte('0'+(goroutineID%numVaults)))
			c := reg.GetOrCreate(vaultName)
			for i := 0; i < writesPerGoroutine; i++ {
				c.RecordWrite(0.5)
				c.RecordLinkCreated(true, false)
				c.RecordContradictionSet()
			}
		}(g)
	}

	wg.Wait()

	// Verify all vaults have correct aggregates
	snapshots := reg.Snapshots()
	if len(snapshots) != numVaults {
		t.Errorf("registry snapshots: expected %d vaults, got %d", numVaults, len(snapshots))
	}

	expectedEngrams := int64((numGoroutines / numVaults) * writesPerGoroutine)
	for _, snap := range snapshots {
		if snap.TotalEngrams != expectedEngrams {
			t.Errorf("vault %s: expected %d engrams, got %d", snap.VaultName, expectedEngrams, snap.TotalEngrams)
		}
	}
}

func TestVarianceWithFloatingPointRounding(t *testing.T) {
	c := &VaultCounters{}

	// Add many identical values to stress floating point accumulation
	const value = 0.123456789
	const iterations = 1000
	for i := 0; i < iterations; i++ {
		c.recordConfidence(value)
	}

	v := c.Variance()
	// Should be very close to 0, but protect against negative due to floating point
	if v < 0 {
		t.Errorf("variance should never be negative, got %f", v)
	}
	if v > 1e-8 {
		t.Errorf("uniform confidence should have near-zero variance, got %f", v)
	}
}

func TestScoreClipping(t *testing.T) {
	c := &VaultCounters{}

	// Set all penalizing metrics to their max
	c.TotalEngrams.Store(100)
	c.OrphanCount.Store(100)    // orphan ratio = 1.0 (clamped to 1.0)
	c.Contradictions.Store(100) // contradiction density = 1.0 (clamped to 1.0)
	c.RefinesCount.Store(100)   // duplication pressure = 1.0 (clamped to 1.0)
	c.ConfidenceN.Store(1)
	c.ConfidenceSum.Store(1000000)   // 1.0
	c.ConfidenceSumSq.Store(1000000) // 1.0

	// Expected:
	// penalty = 1.0*0.3 + 1.0*0.3 + 0*0.2 + 1.0*0.2 = 0.8
	// score = max(0, 1.0 - 0.8) = 0.2

	score := c.Score()
	expected := 0.2
	if math.Abs(score-expected) > 0.001 {
		t.Errorf("max penalty score: expected ~%f, got %f", expected, score)
	}

	// Score should never go negative
	if score < 0 {
		t.Errorf("score should be clamped to [0, 1], got %f", score)
	}
}

func TestRecordWriteIncrementsConfidence(t *testing.T) {
	c := &VaultCounters{}

	c.RecordWrite(0.8)
	c.RecordWrite(0.6)

	if c.ConfidenceN.Load() != 2 {
		t.Errorf("RecordWrite should update confidence: expected n=2, got %d", c.ConfidenceN.Load())
	}

	expectedSum := int64(0.8*1e6 + 0.6*1e6)
	actualSum := c.ConfidenceSum.Load()
	if actualSum != expectedSum {
		t.Errorf("confidence sum: expected %d, got %d", expectedSum, actualSum)
	}
}

func TestLinkDeletionRestoresOrphans(t *testing.T) {
	c := &VaultCounters{}
	c.RecordWrite(0.5)

	// Link created, orphan removed
	c.RecordLinkCreated(true, false)
	if c.OrphanCount.Load() != 0 {
		t.Errorf("after link creation: expected 0 orphans, got %d", c.OrphanCount.Load())
	}

	// Link deleted, orphan restored
	c.RecordLinkDeleted(true, false)
	if c.OrphanCount.Load() != 1 {
		t.Errorf("after link deletion: expected 1 orphan, got %d", c.OrphanCount.Load())
	}
}

func TestRefinesCountIncrementsAndDecrements(t *testing.T) {
	c := &VaultCounters{}

	c.RecordLinkCreated(true, true)
	c.RecordLinkCreated(true, true)
	if c.RefinesCount.Load() != 2 {
		t.Errorf("after 2 refines created: expected 2, got %d", c.RefinesCount.Load())
	}

	c.RecordLinkDeleted(false, true)
	if c.RefinesCount.Load() != 1 {
		t.Errorf("after 1 refines deleted: expected 1, got %d", c.RefinesCount.Load())
	}
}

func TestSnapshotWithEmptyVault(t *testing.T) {
	c := &VaultCounters{}
	snap := c.Snapshot("empty-vault")

	if snap.VaultName != "empty-vault" {
		t.Errorf("snapshot vault name: expected 'empty-vault', got %s", snap.VaultName)
	}
	if snap.Score != 1.0 {
		t.Errorf("empty vault snapshot score: expected 1.0, got %f", snap.Score)
	}
	if snap.TotalEngrams != 0 {
		t.Errorf("empty vault snapshot engrams: expected 0, got %d", snap.TotalEngrams)
	}
	if snap.OrphanRatio != 0 {
		t.Errorf("empty vault snapshot orphan ratio: expected 0, got %f", snap.OrphanRatio)
	}
}

func TestRegistrySnapshotsOrder(t *testing.T) {
	reg := NewRegistry()

	reg.GetOrCreate("zebra").RecordWrite(0.5)
	reg.GetOrCreate("apple").RecordWrite(0.5)
	reg.GetOrCreate("mango").RecordWrite(0.5)

	snapshots := reg.Snapshots()
	if len(snapshots) != 3 {
		t.Errorf("expected 3 snapshots, got %d", len(snapshots))
	}

	// Verify all vaults are in snapshots
	names := make(map[string]bool)
	for _, s := range snapshots {
		names[s.VaultName] = true
	}
	expectedNames := map[string]bool{"zebra": true, "apple": true, "mango": true}
	for name := range expectedNames {
		if !names[name] {
			t.Errorf("missing vault name in snapshots: %s", name)
		}
	}
}

func TestVarianceStability(t *testing.T) {
	c := &VaultCounters{}

	// Add some values
	values := []float32{0.1, 0.2, 0.3, 0.4, 0.5}
	for _, v := range values {
		c.recordConfidence(v)
	}

	v1 := c.Variance()

	// Call Variance again, should be the same
	v2 := c.Variance()

	if math.Abs(v1-v2) > 1e-15 {
		t.Errorf("variance not stable: first %f, second %f", v1, v2)
	}
}

func TestScoreBoundaries(t *testing.T) {
	c := &VaultCounters{}

	// Empty vault
	if c.Score() != 1.0 {
		t.Errorf("empty vault should score 1.0")
	}

	// Add one engram (orphan)
	c.RecordWrite(0.5)
	score1 := c.Score()
	if score1 < 0 || score1 > 1 {
		t.Errorf("score out of bounds: %f", score1)
	}
	if score1 >= 1.0 {
		t.Errorf("orphan engram should score less than 1.0")
	}

	// Create a link
	c.RecordLinkCreated(true, false)
	score2 := c.Score()
	if score2 < 0 || score2 > 1 {
		t.Errorf("score out of bounds: %f", score2)
	}
	if score2 <= score1 {
		t.Errorf("score should improve after link creation: before %f, after %f", score1, score2)
	}
}

// ---------------------------------------------------------------------------
// RenameVault tests
// ---------------------------------------------------------------------------

func TestRegistryRenameVault(t *testing.T) {
	reg := NewRegistry()

	// Create counters under "old" and record some writes.
	c := reg.GetOrCreate("old")
	c.RecordWrite(0.5)
	c.RecordWrite(0.7)
	c.RecordWrite(0.9)

	// Rename "old" -> "new".
	reg.RenameVault("old", "new")

	// GetOrCreate("new") should return the same counters with preserved data.
	cNew := reg.GetOrCreate("new")
	if cNew.TotalEngrams.Load() != 3 {
		t.Errorf("renamed vault TotalEngrams: expected 3, got %d", cNew.TotalEngrams.Load())
	}

	// GetOrCreate("old") should now return a fresh zeroed counter.
	cOld := reg.GetOrCreate("old")
	if cOld.TotalEngrams.Load() != 0 {
		t.Errorf("old vault after rename should be fresh: expected 0 engrams, got %d", cOld.TotalEngrams.Load())
	}
	if cOld == cNew {
		t.Errorf("old and new should be different counter instances after rename")
	}
}

func TestRegistryRenameVault_NoOp(t *testing.T) {
	reg := NewRegistry()

	// Pre-populate a vault that should not be affected.
	reg.GetOrCreate("existing").RecordWrite(0.5)

	// Rename a vault that does not exist — should not panic.
	reg.RenameVault("nonexistent", "target")

	// "existing" should be untouched.
	if reg.GetOrCreate("existing").TotalEngrams.Load() != 1 {
		t.Errorf("existing vault should be unchanged after no-op rename")
	}

	// "target" should not have been created by the no-op rename.
	snapshots := reg.Snapshots()
	for _, snap := range snapshots {
		if snap.VaultName == "target" {
			t.Errorf("no-op rename should not create a 'target' vault")
		}
	}

	// Total vaults should still be 1.
	if len(snapshots) != 1 {
		t.Errorf("expected 1 vault after no-op rename, got %d", len(snapshots))
	}
}

func TestRegistryRenameVault_PreservesCounters(t *testing.T) {
	reg := NewRegistry()

	c := reg.GetOrCreate("alpha")

	// Record a mix of writes, links, and contradictions.
	for i := 0; i < 15; i++ {
		c.RecordWrite(0.6)
	}
	for i := 0; i < 8; i++ {
		c.RecordLinkCreated(true, false)
	}
	for i := 0; i < 3; i++ {
		c.RecordLinkCreated(false, true) // refines links
	}
	for i := 0; i < 4; i++ {
		c.RecordContradictionSet()
	}

	// Snapshot the counters before rename via Serialize.
	dataBefore := c.Serialize()

	// Rename.
	reg.RenameVault("alpha", "beta")

	// Verify all counter values are preserved via Serialize on the renamed entry.
	cRenamed := reg.GetOrCreate("beta")
	dataAfter := cRenamed.Serialize()

	for i := 0; i < len(dataBefore); i++ {
		if dataBefore[i] != dataAfter[i] {
			t.Errorf("Serialize()[%d] mismatch after rename: before %d, after %d", i, dataBefore[i], dataAfter[i])
		}
	}

	// Double-check specific values.
	if cRenamed.TotalEngrams.Load() != 15 {
		t.Errorf("TotalEngrams: expected 15, got %d", cRenamed.TotalEngrams.Load())
	}
	if cRenamed.OrphanCount.Load() != 7 { // 15 writes - 8 first-link creations = 7
		t.Errorf("OrphanCount: expected 7, got %d", cRenamed.OrphanCount.Load())
	}
	if cRenamed.RefinesCount.Load() != 3 {
		t.Errorf("RefinesCount: expected 3, got %d", cRenamed.RefinesCount.Load())
	}
	if cRenamed.Contradictions.Load() != 4 {
		t.Errorf("Contradictions: expected 4, got %d", cRenamed.Contradictions.Load())
	}
}

func BenchmarkRecordWrite(b *testing.B) {
	c := &VaultCounters{}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.RecordWrite(0.5)
	}
}

func BenchmarkRecordLinkCreated(b *testing.B) {
	c := &VaultCounters{}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.RecordLinkCreated(i%2 == 0, i%3 == 0)
	}
}

func BenchmarkScore(b *testing.B) {
	c := &VaultCounters{}
	for i := 0; i < 1000; i++ {
		c.RecordWrite(0.5)
	}
	for i := 0; i < 100; i++ {
		c.RecordLinkCreated(true, i%2 == 0)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = c.Score()
	}
}

func BenchmarkSnapshot(b *testing.B) {
	c := &VaultCounters{}
	for i := 0; i < 1000; i++ {
		c.RecordWrite(0.5)
	}
	for i := 0; i < 100; i++ {
		c.RecordLinkCreated(true, i%2 == 0)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = c.Snapshot("bench-vault")
	}
}

func BenchmarkVariance(b *testing.B) {
	c := &VaultCounters{}
	for i := 0; i < 10000; i++ {
		c.recordConfidence(0.5)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = c.Variance()
	}
}

func BenchmarkRegistryConcurrentWrites(b *testing.B) {
	reg := NewRegistry()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for i := 0; i < b.N; i++ {
			vault := "vault-0"
			c := reg.GetOrCreate(vault)
			c.RecordWrite(0.5)
		}
	})
}

// ---------------------------------------------------------------------------
// Fix 5: Serialize / Restore round-trip
// ---------------------------------------------------------------------------

// TestSerializeRestoreRoundTrip verifies that serializing counters and
// restoring them into a fresh VaultCounters produces identical values.
func TestSerializeRestoreRoundTrip(t *testing.T) {
	c := &VaultCounters{}

	// Set up a realistic state.
	for i := 0; i < 20; i++ {
		c.RecordWrite(0.6)
	}
	for i := 0; i < 10; i++ {
		c.RecordLinkCreated(true, false)
	}
	for i := 0; i < 3; i++ {
		c.RecordContradictionSet()
	}
	c.RecordLinkCreated(false, true) // 1 REFINES link

	// Serialize.
	data := c.Serialize()

	// Restore into a fresh counter.
	c2 := &VaultCounters{}
	c2.Restore(data)

	// All counters must match.
	if c.TotalEngrams.Load() != c2.TotalEngrams.Load() {
		t.Errorf("TotalEngrams: orig %d, restored %d", c.TotalEngrams.Load(), c2.TotalEngrams.Load())
	}
	if c.OrphanCount.Load() != c2.OrphanCount.Load() {
		t.Errorf("OrphanCount: orig %d, restored %d", c.OrphanCount.Load(), c2.OrphanCount.Load())
	}
	if c.Contradictions.Load() != c2.Contradictions.Load() {
		t.Errorf("Contradictions: orig %d, restored %d", c.Contradictions.Load(), c2.Contradictions.Load())
	}
	if c.RefinesCount.Load() != c2.RefinesCount.Load() {
		t.Errorf("RefinesCount: orig %d, restored %d", c.RefinesCount.Load(), c2.RefinesCount.Load())
	}

	// Scores must be identical.
	if c.Score() != c2.Score() {
		t.Errorf("Score: orig %f, restored %f", c.Score(), c2.Score())
	}
}

// ---------------------------------------------------------------------------
// Fix 5: SerializeAll captures all vaults
// ---------------------------------------------------------------------------

// TestSerializeAllCapturesAllVaults verifies that SerializeAll returns one
// entry per registered vault and that each entry is correct.
func TestSerializeAllCapturesAllVaults(t *testing.T) {
	reg := NewRegistry()

	reg.GetOrCreate("vault-x").RecordWrite(0.5)
	reg.GetOrCreate("vault-x").RecordLinkCreated(true, false)
	reg.GetOrCreate("vault-y").RecordWrite(0.8)
	reg.GetOrCreate("vault-z").RecordWrite(0.3)

	all := reg.SerializeAll()

	if len(all) != 3 {
		t.Errorf("SerializeAll: expected 3 vaults, got %d", len(all))
	}
	if _, ok := all["vault-x"]; !ok {
		t.Error("SerializeAll missing vault-x")
	}
	if _, ok := all["vault-y"]; !ok {
		t.Error("SerializeAll missing vault-y")
	}
	if _, ok := all["vault-z"]; !ok {
		t.Error("SerializeAll missing vault-z")
	}

	// vault-x had 1 engram + 1 link — orphan count should be 0.
	x := &VaultCounters{}
	x.Restore(all["vault-x"])
	if x.TotalEngrams.Load() != 1 {
		t.Errorf("vault-x TotalEngrams: got %d, want 1", x.TotalEngrams.Load())
	}
	if x.OrphanCount.Load() != 0 {
		t.Errorf("vault-x OrphanCount: got %d, want 0", x.OrphanCount.Load())
	}
}

// ---------------------------------------------------------------------------
// Fix 5: RestoreVault populates a vault in the registry
// ---------------------------------------------------------------------------

// TestRestoreVaultSetsCounters verifies that RestoreVault sets counter values
// on a newly created registry entry so that subsequent Snapshots reflect them.
func TestRestoreVaultSetsCounters(t *testing.T) {
	src := &VaultCounters{}
	for i := 0; i < 42; i++ {
		src.RecordWrite(0.7)
	}
	for i := 0; i < 10; i++ {
		src.RecordLinkCreated(true, false)
	}

	data := src.Serialize()

	// Restore into a fresh registry.
	reg := NewRegistry()
	reg.RestoreVault("restored-vault", data)

	snaps := reg.Snapshots()
	if len(snaps) != 1 {
		t.Fatalf("expected 1 snapshot after RestoreVault, got %d", len(snaps))
	}
	snap := snaps[0]
	if snap.VaultName != "restored-vault" {
		t.Errorf("snapshot VaultName = %q, want 'restored-vault'", snap.VaultName)
	}
	if snap.TotalEngrams != 42 {
		t.Errorf("TotalEngrams = %d, want 42", snap.TotalEngrams)
	}
	// 42 writes, 10 first-link creations → 32 orphans remain.
	if snap.OrphanRatio < 0 || snap.OrphanRatio > 1 {
		t.Errorf("OrphanRatio = %f out of [0,1]", snap.OrphanRatio)
	}
}
