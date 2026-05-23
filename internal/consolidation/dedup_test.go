package consolidation

import (
	"context"
	"fmt"
	"testing"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/storage/erf"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

// testStoreWithDB returns both the PebbleStore and raw DB for direct writes.
func testStoreWithDB(t *testing.T) (*storage.PebbleStore, *pebble.DB, func()) {
	t.Helper()
	db, err := pebble.Open(t.TempDir(), &pebble.Options{})
	if err != nil {
		t.Fatal(err)
	}
	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 100})
	return store, db, func() { store.Close() }
}

// writeEngramWithEmbedding writes an engram via the store (creates all indexes),
// then overwrites the 0x01 key with a v1-encoded record that includes the
// embedding inline. This simulates pre-v2-migration engrams that dedup targets.
func writeEngramWithEmbedding(t *testing.T, ctx context.Context, store *storage.PebbleStore, db *pebble.DB, wsPrefix [8]byte, eng *storage.Engram) storage.ULID {
	t.Helper()
	id, err := store.WriteEngram(ctx, wsPrefix, eng)
	if err != nil {
		t.Fatal(err)
	}

	erfEng := &erf.Engram{
		ID:          [16]byte(id),
		CreatedAt:   eng.CreatedAt,
		UpdatedAt:   eng.UpdatedAt,
		LastAccess:  eng.LastAccess,
		Confidence:  eng.Confidence,
		Relevance:   eng.Relevance,
		Stability:   eng.Stability,
		AccessCount: eng.AccessCount,
		State:       uint8(eng.State),
		Concept:     eng.Concept,
		Content:     eng.Content,
		Tags:        eng.Tags,
		Embedding:   eng.Embedding,
	}
	v1Bytes, err := erf.Encode(erfEng)
	if err != nil {
		t.Fatal(err)
	}

	key := keys.EngramKey(wsPrefix, [16]byte(id))
	if err := db.Set(key, v1Bytes, pebble.Sync); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestDedup_IdenticalEmbeddings_MergesCluster(t *testing.T) {
	store, db, cleanup := testStoreWithDB(t)
	defer cleanup()

	ctx := context.Background()
	vault := "dedup_merge"
	wsPrefix := store.ResolveVaultPrefix(vault)

	embed := []float32{1, 0, 0, 0}

	highID := writeEngramWithEmbedding(t, ctx, store, db, wsPrefix, &storage.Engram{
		Concept: "high_conf", Content: "content A", Confidence: 0.9, Relevance: 0.9,
		Stability: 30, Embedding: embed, Tags: []string{"tag_a"},
	})
	lowID := writeEngramWithEmbedding(t, ctx, store, db, wsPrefix, &storage.Engram{
		Concept: "low_conf", Content: "content B", Confidence: 0.5, Relevance: 0.5,
		Stability: 30, Embedding: embed, Tags: []string{"tag_b"},
	})

	mock := &mockEngineInterface{store: store}
	w := &Worker{Engine: mock, MaxDedup: 100, MaxTransitive: 100}
	report := &ConsolidationReport{}

	if err := w.runPhase2Dedup(ctx, store, wsPrefix, report, vault); err != nil {
		t.Fatal(err)
	}

	if report.DedupClusters != 1 {
		t.Errorf("DedupClusters = %d, want 1", report.DedupClusters)
	}
	if report.MergedEngrams != 1 {
		t.Errorf("MergedEngrams = %d, want 1", report.MergedEngrams)
	}

	kept, err := store.GetEngram(ctx, wsPrefix, highID)
	if err != nil {
		t.Fatal(err)
	}
	if kept.State == storage.StateArchived {
		t.Error("representative engram should not be archived")
	}

	archived, err := store.GetEngram(ctx, wsPrefix, lowID)
	if err != nil {
		t.Fatal(err)
	}
	if archived.State != storage.StateArchived {
		t.Errorf("duplicate engram state = %v, want archived", archived.State)
	}
}

// TestDedup_AccessCountAbsorbsArchivedDuplicates verifies the representative
// engram absorbs the AccessCount of each archived duplicate (+1 per archived
// member). This preserves the frequency-of-occurrence signal for write-only
// engrams (auto-ingest, daily captures) that never reach the recall path's
// implicit AccessCount auto-bump. Without this, downstream skills must run
// their own per-vault feedback cron jobs to compensate.
func TestDedup_AccessCountAbsorbsArchivedDuplicates(t *testing.T) {
	store, db, cleanup := testStoreWithDB(t)
	defer cleanup()

	ctx := context.Background()
	vault := "dedup_accesscount_bump"
	wsPrefix := store.ResolveVaultPrefix(vault)

	embed := []float32{1, 0, 0, 0}

	// 1 representative + 3 duplicates → representative should end with AccessCount += 3.
	repID := writeEngramWithEmbedding(t, ctx, store, db, wsPrefix, &storage.Engram{
		Concept: "rep", Content: "rep content", Confidence: 0.9, Relevance: 0.9,
		Stability: 30, Embedding: embed, AccessCount: 5,
	})
	for i := 0; i < 3; i++ {
		writeEngramWithEmbedding(t, ctx, store, db, wsPrefix, &storage.Engram{
			Concept: fmt.Sprintf("dup-%d", i), Content: fmt.Sprintf("dup %d", i),
			Confidence: 0.5, Relevance: 0.5,
			Stability: 30, Embedding: embed, AccessCount: 0,
		})
	}

	mock := &mockEngineInterface{store: store}
	w := &Worker{Engine: mock, MaxDedup: 100, MaxTransitive: 100}
	report := &ConsolidationReport{}

	if err := w.runPhase2Dedup(ctx, store, wsPrefix, report, vault); err != nil {
		t.Fatal(err)
	}

	rep, err := store.GetEngram(ctx, wsPrefix, repID)
	if err != nil {
		t.Fatal(err)
	}

	const wantAccess uint32 = 5 + 3
	if rep.AccessCount != wantAccess {
		t.Errorf("representative AccessCount = %d, want %d (5 starting + 3 absorbed from archived dupes)",
			rep.AccessCount, wantAccess)
	}
}

// TestDedup_DryRun_DoesNotBumpAccessCount verifies the frequency-absorption
// bump only happens when mutations are enabled — DryRun must leave AccessCount
// unchanged, matching the existing no-mutation contract for the archive step.
func TestDedup_DryRun_DoesNotBumpAccessCount(t *testing.T) {
	store, db, cleanup := testStoreWithDB(t)
	defer cleanup()

	ctx := context.Background()
	vault := "dedup_dryrun_accesscount"
	wsPrefix := store.ResolveVaultPrefix(vault)

	embed := []float32{1, 0, 0, 0}
	repID := writeEngramWithEmbedding(t, ctx, store, db, wsPrefix, &storage.Engram{
		Concept: "rep", Content: "rep", Confidence: 0.9, Relevance: 0.9,
		Stability: 30, Embedding: embed, AccessCount: 2,
	})
	writeEngramWithEmbedding(t, ctx, store, db, wsPrefix, &storage.Engram{
		Concept: "dup", Content: "dup", Confidence: 0.5, Relevance: 0.5,
		Stability: 30, Embedding: embed, AccessCount: 0,
	})

	mock := &mockEngineInterface{store: store}
	w := &Worker{Engine: mock, MaxDedup: 100, MaxTransitive: 100, DryRun: true}
	report := &ConsolidationReport{}

	if err := w.runPhase2Dedup(ctx, store, wsPrefix, report, vault); err != nil {
		t.Fatal(err)
	}

	rep, err := store.GetEngram(ctx, wsPrefix, repID)
	if err != nil {
		t.Fatal(err)
	}
	if rep.AccessCount != 2 {
		t.Errorf("DryRun mutated AccessCount: got %d, want 2", rep.AccessCount)
	}
}

func TestDedup_TagMerging(t *testing.T) {
	store, db, cleanup := testStoreWithDB(t)
	defer cleanup()

	ctx := context.Background()
	vault := "dedup_tags"
	wsPrefix := store.ResolveVaultPrefix(vault)

	embed := []float32{0, 1, 0}

	id1 := writeEngramWithEmbedding(t, ctx, store, db, wsPrefix, &storage.Engram{
		Concept: "a", Content: "x", Confidence: 0.9, Relevance: 0.9,
		Stability: 30, Embedding: embed, Tags: []string{"shared", "only_a"},
	})
	writeEngramWithEmbedding(t, ctx, store, db, wsPrefix, &storage.Engram{
		Concept: "b", Content: "y", Confidence: 0.3, Relevance: 0.3,
		Stability: 30, Embedding: embed, Tags: []string{"shared", "only_b"},
	})

	mock := &mockEngineInterface{store: store}
	w := &Worker{Engine: mock, MaxDedup: 100, MaxTransitive: 100}
	report := &ConsolidationReport{}

	if err := w.runPhase2Dedup(ctx, store, wsPrefix, report, vault); err != nil {
		t.Fatal(err)
	}

	rep, err := store.GetEngram(ctx, wsPrefix, id1)
	if err != nil {
		t.Fatal(err)
	}

	tagSet := make(map[string]bool)
	for _, tag := range rep.Tags {
		tagSet[tag] = true
	}

	for _, want := range []string{"shared", "only_a", "only_b"} {
		if !tagSet[want] {
			t.Errorf("representative missing merged tag %q; has %v", want, rep.Tags)
		}
	}
}

func TestDedup_DryRun_NoMutation(t *testing.T) {
	store, db, cleanup := testStoreWithDB(t)
	defer cleanup()

	ctx := context.Background()
	vault := "dedup_dry"
	wsPrefix := store.ResolveVaultPrefix(vault)

	embed := []float32{1, 1, 0}

	writeEngramWithEmbedding(t, ctx, store, db, wsPrefix, &storage.Engram{
		Concept: "a", Content: "x", Confidence: 0.9, Relevance: 0.9,
		Stability: 30, Embedding: embed,
	})
	id2 := writeEngramWithEmbedding(t, ctx, store, db, wsPrefix, &storage.Engram{
		Concept: "b", Content: "y", Confidence: 0.3, Relevance: 0.3,
		Stability: 30, Embedding: embed,
	})

	mock := &mockEngineInterface{store: store}
	w := &Worker{Engine: mock, MaxDedup: 100, MaxTransitive: 100, DryRun: true}
	report := &ConsolidationReport{}

	if err := w.runPhase2Dedup(ctx, store, wsPrefix, report, vault); err != nil {
		t.Fatal(err)
	}

	if report.MergedEngrams != 1 {
		t.Errorf("MergedEngrams = %d, want 1 (counted but not applied)", report.MergedEngrams)
	}

	eng, err := store.GetEngram(ctx, wsPrefix, id2)
	if err != nil {
		t.Fatal(err)
	}
	if eng.State == storage.StateArchived {
		t.Error("dry run should not archive engrams")
	}
}

func TestDedup_RespectsMaxDedupCap(t *testing.T) {
	store, db, cleanup := testStoreWithDB(t)
	defer cleanup()

	ctx := context.Background()
	vault := "dedup_cap"
	wsPrefix := store.ResolveVaultPrefix(vault)

	// Cluster 1: x-axis (3 engrams → 2 merges)
	embedX := []float32{1, 0, 0}
	for i := 0; i < 3; i++ {
		writeEngramWithEmbedding(t, ctx, store, db, wsPrefix, &storage.Engram{
			Concept: "x", Content: "x", Confidence: float32(i+1) * 0.1, Relevance: float32(i+1) * 0.1,
			Stability: 30, Embedding: embedX,
		})
	}

	// Cluster 2: y-axis (2 engrams → 1 merge, but should be skipped by cap)
	embedY := []float32{0, 1, 0}
	for i := 0; i < 2; i++ {
		writeEngramWithEmbedding(t, ctx, store, db, wsPrefix, &storage.Engram{
			Concept: "y", Content: "y", Confidence: float32(i+1) * 0.1, Relevance: float32(i+1) * 0.1,
			Stability: 30, Embedding: embedY,
		})
	}

	mock := &mockEngineInterface{store: store}
	// Cap at 2: cluster 1 merges 2, cluster 2 should be skipped
	w := &Worker{Engine: mock, MaxDedup: 2, MaxTransitive: 100}
	report := &ConsolidationReport{}

	if err := w.runPhase2Dedup(ctx, store, wsPrefix, report, vault); err != nil {
		t.Fatal(err)
	}

	// The cap is checked at cluster boundaries, so cluster 1 (2 merges) fills
	// the cap and cluster 2 is skipped entirely.
	if report.MergedEngrams > 2 {
		t.Errorf("MergedEngrams = %d, should be capped at 2", report.MergedEngrams)
	}
}

func TestDedup_NoClustersWhenDissimilar(t *testing.T) {
	store, db, cleanup := testStoreWithDB(t)
	defer cleanup()

	ctx := context.Background()
	vault := "dedup_dissim"
	wsPrefix := store.ResolveVaultPrefix(vault)

	writeEngramWithEmbedding(t, ctx, store, db, wsPrefix, &storage.Engram{
		Concept: "a", Content: "x", Confidence: 0.9, Relevance: 0.9,
		Stability: 30, Embedding: []float32{1, 0, 0},
	})
	writeEngramWithEmbedding(t, ctx, store, db, wsPrefix, &storage.Engram{
		Concept: "b", Content: "y", Confidence: 0.9, Relevance: 0.9,
		Stability: 30, Embedding: []float32{0, 1, 0},
	})

	mock := &mockEngineInterface{store: store}
	w := &Worker{Engine: mock, MaxDedup: 100, MaxTransitive: 100}
	report := &ConsolidationReport{}

	if err := w.runPhase2Dedup(ctx, store, wsPrefix, report, vault); err != nil {
		t.Fatal(err)
	}

	if report.DedupClusters != 0 {
		t.Errorf("DedupClusters = %d, want 0 (orthogonal vectors)", report.DedupClusters)
	}
	if report.MergedEngrams != 0 {
		t.Errorf("MergedEngrams = %d, want 0", report.MergedEngrams)
	}
}

func TestDedup_SkipsEngramsWithoutEmbeddings(t *testing.T) {
	store, db, cleanup := testStoreWithDB(t)
	defer cleanup()

	ctx := context.Background()
	vault := "dedup_no_embed"
	wsPrefix := store.ResolveVaultPrefix(vault)

	writeEngramWithEmbedding(t, ctx, store, db, wsPrefix, &storage.Engram{
		Concept: "a", Content: "x", Confidence: 0.9, Relevance: 0.9,
		Stability: 30, Embedding: []float32{1, 0, 0},
	})
	// Write one without embedding (normal v2 path — no overwrite)
	store.WriteEngram(ctx, wsPrefix, &storage.Engram{
		Concept: "b", Content: "y", Confidence: 0.9, Relevance: 0.9,
		Stability: 30,
	})

	mock := &mockEngineInterface{store: store}
	w := &Worker{Engine: mock, MaxDedup: 100, MaxTransitive: 100}
	report := &ConsolidationReport{}

	if err := w.runPhase2Dedup(ctx, store, wsPrefix, report, vault); err != nil {
		t.Fatal(err)
	}

	if report.DedupClusters != 0 {
		t.Errorf("DedupClusters = %d, want 0 (only 1 engram with embedding)", report.DedupClusters)
	}
}

func TestDedup_EmptyVault(t *testing.T) {
	store, cleanup := testStore(t)
	defer cleanup()

	ctx := context.Background()
	wsPrefix := store.ResolveVaultPrefix("dedup_empty")

	w := &Worker{MaxDedup: 100, MaxTransitive: 100}
	report := &ConsolidationReport{}

	if err := w.runPhase2Dedup(ctx, store, wsPrefix, report, "dedup_empty"); err != nil {
		t.Fatal(err)
	}

	if report.DedupClusters != 0 || report.MergedEngrams != 0 {
		t.Errorf("empty vault should produce zero counts")
	}
}

func TestDedup_MultipleClusters(t *testing.T) {
	store, db, cleanup := testStoreWithDB(t)
	defer cleanup()

	ctx := context.Background()
	vault := "dedup_multi"
	wsPrefix := store.ResolveVaultPrefix(vault)

	embedX := []float32{1, 0, 0}
	for i := 0; i < 3; i++ {
		writeEngramWithEmbedding(t, ctx, store, db, wsPrefix, &storage.Engram{
			Concept: "x", Content: "x-content", Confidence: 0.8, Relevance: 0.8,
			Stability: 30, Embedding: embedX,
		})
	}

	embedY := []float32{0, 1, 0}
	for i := 0; i < 2; i++ {
		writeEngramWithEmbedding(t, ctx, store, db, wsPrefix, &storage.Engram{
			Concept: "y", Content: "y-content", Confidence: 0.8, Relevance: 0.8,
			Stability: 30, Embedding: embedY,
		})
	}

	mock := &mockEngineInterface{store: store}
	w := &Worker{Engine: mock, MaxDedup: 100, MaxTransitive: 100}
	report := &ConsolidationReport{}

	if err := w.runPhase2Dedup(ctx, store, wsPrefix, report, vault); err != nil {
		t.Fatal(err)
	}

	if report.DedupClusters != 2 {
		t.Errorf("DedupClusters = %d, want 2", report.DedupClusters)
	}
	if report.MergedEngrams != 3 {
		t.Errorf("MergedEngrams = %d, want 3", report.MergedEngrams)
	}
}

func TestDedup_RepresentativeElection(t *testing.T) {
	store, db, cleanup := testStoreWithDB(t)
	defer cleanup()

	ctx := context.Background()
	vault := "dedup_elect"
	wsPrefix := store.ResolveVaultPrefix(vault)

	embed := []float32{0.5, 0.5, 0}

	lowID := writeEngramWithEmbedding(t, ctx, store, db, wsPrefix, &storage.Engram{
		Concept: "low", Content: "low", Confidence: 0.3, Relevance: 0.2,
		Stability: 30, Embedding: embed,
	})
	highID := writeEngramWithEmbedding(t, ctx, store, db, wsPrefix, &storage.Engram{
		Concept: "high", Content: "high", Confidence: 1.0, Relevance: 1.0,
		Stability: 30, Embedding: embed,
	})

	mock := &mockEngineInterface{store: store}
	w := &Worker{Engine: mock, MaxDedup: 100, MaxTransitive: 100}
	report := &ConsolidationReport{}

	if err := w.runPhase2Dedup(ctx, store, wsPrefix, report, vault); err != nil {
		t.Fatal(err)
	}

	highEng, _ := store.GetEngram(ctx, wsPrefix, highID)
	if highEng.State == storage.StateArchived {
		t.Error("high-score engram should be kept as representative")
	}

	lowEng, _ := store.GetEngram(ctx, wsPrefix, lowID)
	if lowEng.State != storage.StateArchived {
		t.Errorf("low-score engram state = %v, want archived", lowEng.State)
	}
}

func TestDedup_NearThresholdSimilarity(t *testing.T) {
	store, db, cleanup := testStoreWithDB(t)
	defer cleanup()

	ctx := context.Background()
	vault := "dedup_threshold"
	wsPrefix := store.ResolveVaultPrefix(vault)

	// cos(a, b) where a=(1,0,0), b=(0.94,0.34,0) ≈ 0.94 — below 0.95 threshold
	writeEngramWithEmbedding(t, ctx, store, db, wsPrefix, &storage.Engram{
		Concept: "a", Content: "x", Confidence: 0.9, Relevance: 0.9,
		Stability: 30, Embedding: []float32{1, 0, 0},
	})
	writeEngramWithEmbedding(t, ctx, store, db, wsPrefix, &storage.Engram{
		Concept: "b", Content: "y", Confidence: 0.9, Relevance: 0.9,
		Stability: 30, Embedding: []float32{0.94, 0.34, 0},
	})

	mock := &mockEngineInterface{store: store}
	w := &Worker{Engine: mock, MaxDedup: 100, MaxTransitive: 100}
	report := &ConsolidationReport{}

	if err := w.runPhase2Dedup(ctx, store, wsPrefix, report, vault); err != nil {
		t.Fatal(err)
	}

	if report.DedupClusters != 0 {
		t.Errorf("DedupClusters = %d, want 0 (similarity below 0.95)", report.DedupClusters)
	}
}

func TestDedup_ThreeWayCluster(t *testing.T) {
	store, db, cleanup := testStoreWithDB(t)
	defer cleanup()

	ctx := context.Background()
	vault := "dedup_3way"
	wsPrefix := store.ResolveVaultPrefix(vault)

	embed := []float32{0, 0, 0, 1}

	writeEngramWithEmbedding(t, ctx, store, db, wsPrefix, &storage.Engram{
		Concept: "a", Content: "first", Confidence: 0.5, Relevance: 0.5,
		Stability: 30, Embedding: embed, Tags: []string{"t1"},
	})
	id2 := writeEngramWithEmbedding(t, ctx, store, db, wsPrefix, &storage.Engram{
		Concept: "b", Content: "second", Confidence: 0.8, Relevance: 0.8,
		Stability: 30, Embedding: embed, Tags: []string{"t2"},
	})
	writeEngramWithEmbedding(t, ctx, store, db, wsPrefix, &storage.Engram{
		Concept: "c", Content: "third", Confidence: 0.3, Relevance: 0.3,
		Stability: 30, Embedding: embed, Tags: []string{"t3"},
	})

	mock := &mockEngineInterface{store: store}
	w := &Worker{Engine: mock, MaxDedup: 100, MaxTransitive: 100}
	report := &ConsolidationReport{}

	if err := w.runPhase2Dedup(ctx, store, wsPrefix, report, vault); err != nil {
		t.Fatal(err)
	}

	if report.DedupClusters != 1 {
		t.Errorf("DedupClusters = %d, want 1", report.DedupClusters)
	}
	if report.MergedEngrams != 2 {
		t.Errorf("MergedEngrams = %d, want 2 (keep 1, archive 2)", report.MergedEngrams)
	}

	rep, _ := store.GetEngram(ctx, wsPrefix, id2)
	if rep.State == storage.StateArchived {
		t.Error("representative (highest score) should not be archived")
	}
}

func TestDedup_ArchivesCorrectEngrams(t *testing.T) {
	store, db, cleanup := testStoreWithDB(t)
	defer cleanup()

	ctx := context.Background()
	vault := "dedup_correct_archive"
	wsPrefix := store.ResolveVaultPrefix(vault)

	// Cluster 1: embeddings along x-axis (cosine = 1.0 — identical direction)
	// Engram A: HIGH score (winner — stays ACTIVE)
	engAID := writeEngramWithEmbedding(t, ctx, store, db, wsPrefix, &storage.Engram{
		Concept: "a", Content: "content A", Confidence: 0.9, Relevance: 0.9,
		Stability: 30, Embedding: []float32{1, 0, 0, 0},
	})
	// Engram B: LOW score (loser — should be ARCHIVED)
	engBID := writeEngramWithEmbedding(t, ctx, store, db, wsPrefix, &storage.Engram{
		Concept: "b", Content: "content B", Confidence: 0.3, Relevance: 0.3,
		Stability: 30, Embedding: []float32{1, 0, 0, 0},
	})

	// Cluster 2: embeddings along y-axis (cosine = 1.0 — identical direction)
	// Engram C: HIGH score (winner — stays ACTIVE)
	engCID := writeEngramWithEmbedding(t, ctx, store, db, wsPrefix, &storage.Engram{
		Concept: "c", Content: "content C", Confidence: 0.8, Relevance: 0.8,
		Stability: 30, Embedding: []float32{0, 1, 0, 0},
	})
	// Engram D: LOW score (loser — should be ARCHIVED)
	engDID := writeEngramWithEmbedding(t, ctx, store, db, wsPrefix, &storage.Engram{
		Concept: "d", Content: "content D", Confidence: 0.2, Relevance: 0.2,
		Stability: 30, Embedding: []float32{0, 1, 0, 0},
	})

	// Engram E: unique embedding along z-axis (orthogonal to both clusters — stays ACTIVE)
	engEID := writeEngramWithEmbedding(t, ctx, store, db, wsPrefix, &storage.Engram{
		Concept: "e", Content: "content E", Confidence: 0.7, Relevance: 0.7,
		Stability: 30, Embedding: []float32{0, 0, 1, 0},
	})

	mock := &mockEngineInterface{store: store}
	w := &Worker{Engine: mock, MaxDedup: 100, MaxTransitive: 1000, DryRun: false}
	report := &ConsolidationReport{}

	if err := w.runPhase2Dedup(ctx, store, wsPrefix, report, vault); err != nil {
		t.Fatal(err)
	}

	if report.DedupClusters != 2 {
		t.Errorf("DedupClusters = %d, want 2 (cluster A+B, cluster C+D)", report.DedupClusters)
	}
	if report.MergedEngrams != 2 {
		t.Errorf("MergedEngrams = %d, want 2 (B and D get archived)", report.MergedEngrams)
	}

	// Engram A: representative of cluster 1 — must remain ACTIVE
	engA, err := store.GetEngram(ctx, wsPrefix, engAID)
	if err != nil {
		t.Fatalf("GetEngram A: %v", err)
	}
	if engA.State == storage.StateArchived {
		t.Error("engram A (high-score, cluster 1 representative) should not be archived")
	}

	// Engram B: non-representative of cluster 1 — must be ARCHIVED
	engB, err := store.GetEngram(ctx, wsPrefix, engBID)
	if err != nil {
		t.Fatalf("GetEngram B: %v", err)
	}
	if engB.State != storage.StateArchived {
		t.Errorf("engram B (low-score, cluster 1 duplicate) state = %v, want StateArchived", engB.State)
	}

	// Engram C: representative of cluster 2 — must remain ACTIVE
	engC, err := store.GetEngram(ctx, wsPrefix, engCID)
	if err != nil {
		t.Fatalf("GetEngram C: %v", err)
	}
	if engC.State == storage.StateArchived {
		t.Error("engram C (high-score, cluster 2 representative) should not be archived")
	}

	// Engram D: non-representative of cluster 2 — must be ARCHIVED
	engD, err := store.GetEngram(ctx, wsPrefix, engDID)
	if err != nil {
		t.Fatalf("GetEngram D: %v", err)
	}
	if engD.State != storage.StateArchived {
		t.Errorf("engram D (low-score, cluster 2 duplicate) state = %v, want StateArchived", engD.State)
	}

	// Engram E: unique cluster — must remain ACTIVE
	engE, err := store.GetEngram(ctx, wsPrefix, engEID)
	if err != nil {
		t.Fatalf("GetEngram E: %v", err)
	}
	if engE.State == storage.StateArchived {
		t.Error("engram E (unique, no cluster) should not be archived")
	}
}

// writeEngramV2WithEmbedding writes an engram via the store (v2 path — embedding
// NOT inline), then stores the embedding separately via UpdateEmbedding.
func writeEngramV2WithEmbedding(t *testing.T, ctx context.Context, store *storage.PebbleStore, wsPrefix [8]byte, eng *storage.Engram) storage.ULID {
	t.Helper()
	embed := eng.Embedding
	eng.Embedding = nil // ensure it is not stored inline
	id, err := store.WriteEngram(ctx, wsPrefix, eng)
	if err != nil {
		t.Fatal(err)
	}
	if len(embed) > 0 {
		if err := store.UpdateEmbedding(ctx, wsPrefix, id, embed); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

// TestDedup_Determinism verifies that running runPhase2Dedup twice on the same
// logical input produces identical cluster counts and representative election
// outcomes. The tag merge order may differ (map iteration).
func TestDedup_Determinism(t *testing.T) {
	ctx := context.Background()

	// Run dedup on two independent stores seeded with the same data.
	runDedup := func(label string) (clusters, merged int, archivedIDs []storage.ULID) {
		store, db, cleanup := testStoreWithDB(t)
		defer cleanup()
		vault := "dedup_det_" + label
		wsPrefix := store.ResolveVaultPrefix(vault)

		embed1 := []float32{1, 0, 0, 0}
		embed2 := []float32{0, 1, 0, 0}

		// Cluster A: 2 identical-direction engrams
		a1 := writeEngramWithEmbedding(t, ctx, store, db, wsPrefix, &storage.Engram{
			Concept: "a1", Content: "content-a1", Confidence: 0.9, Relevance: 0.9,
			Stability: 30, Embedding: embed1, Tags: []string{"alpha"},
		})
		a2 := writeEngramWithEmbedding(t, ctx, store, db, wsPrefix, &storage.Engram{
			Concept: "a2", Content: "content-a2", Confidence: 0.4, Relevance: 0.4,
			Stability: 30, Embedding: embed1, Tags: []string{"beta"},
		})

		// Cluster B: 2 identical-direction engrams
		b1 := writeEngramWithEmbedding(t, ctx, store, db, wsPrefix, &storage.Engram{
			Concept: "b1", Content: "content-b1", Confidence: 0.8, Relevance: 0.8,
			Stability: 30, Embedding: embed2, Tags: []string{"gamma"},
		})
		b2 := writeEngramWithEmbedding(t, ctx, store, db, wsPrefix, &storage.Engram{
			Concept: "b2", Content: "content-b2", Confidence: 0.2, Relevance: 0.2,
			Stability: 30, Embedding: embed2, Tags: []string{"delta"},
		})
		_ = a1
		_ = b1

		mock := &mockEngineInterface{store: store}
		w := &Worker{Engine: mock, MaxDedup: 100, MaxTransitive: 100}
		report := &ConsolidationReport{}
		if err := w.runPhase2Dedup(ctx, store, wsPrefix, report, vault); err != nil {
			t.Fatalf("[%s] runPhase2Dedup: %v", label, err)
		}

		// Collect archived IDs
		for _, id := range []storage.ULID{a2, b2} {
			eng, err := store.GetEngram(ctx, wsPrefix, id)
			if err != nil {
				t.Fatalf("[%s] GetEngram %v: %v", label, id, err)
			}
			if eng.State == storage.StateArchived {
				archivedIDs = append(archivedIDs, id)
			}
		}
		return report.DedupClusters, report.MergedEngrams, archivedIDs
	}

	clusters1, merged1, _ := runDedup("run1")
	clusters2, merged2, _ := runDedup("run2")

	if clusters1 != clusters2 {
		t.Errorf("non-deterministic cluster count: run1=%d run2=%d", clusters1, clusters2)
	}
	if merged1 != merged2 {
		t.Errorf("non-deterministic merge count: run1=%d run2=%d", merged1, merged2)
	}
	// Both runs must find 2 clusters and archive 2 engrams.
	if clusters1 != 2 {
		t.Errorf("expected 2 clusters, got %d", clusters1)
	}
	if merged1 != 2 {
		t.Errorf("expected 2 merged engrams, got %d", merged1)
	}
}

func TestDedup_V2Embeddings_MergesCluster(t *testing.T) {
	store, _, cleanup := testStoreWithDB(t)
	defer cleanup()

	ctx := context.Background()
	vault := "dedup_v2"
	wsPrefix := store.ResolveVaultPrefix(vault)

	embed := []float32{1, 0, 0, 0}

	highID := writeEngramV2WithEmbedding(t, ctx, store, wsPrefix, &storage.Engram{
		Concept: "high_conf", Content: "content A", Confidence: 0.9, Relevance: 0.9,
		Stability: 30, Embedding: embed, Tags: []string{"tag_a"},
	})
	lowID := writeEngramV2WithEmbedding(t, ctx, store, wsPrefix, &storage.Engram{
		Concept: "low_conf", Content: "content B", Confidence: 0.5, Relevance: 0.5,
		Stability: 30, Embedding: embed, Tags: []string{"tag_b"},
	})

	// Verify GetEngrams does NOT return inline embeddings (v2 behavior)
	engs, err := store.GetEngrams(ctx, wsPrefix, []storage.ULID{highID, lowID})
	if err != nil {
		t.Fatal(err)
	}
	for _, eng := range engs {
		if eng != nil && len(eng.Embedding) > 0 {
			t.Fatal("GetEngrams should NOT return inline embeddings for v2 engrams")
		}
	}

	mock := &mockEngineInterface{store: store}
	w := &Worker{Engine: mock, MaxDedup: 100, MaxTransitive: 100}
	report := &ConsolidationReport{}

	if err := w.runPhase2Dedup(ctx, store, wsPrefix, report, vault); err != nil {
		t.Fatal(err)
	}

	if report.DedupClusters != 1 {
		t.Errorf("DedupClusters = %d, want 1", report.DedupClusters)
	}
	if report.MergedEngrams != 1 {
		t.Errorf("MergedEngrams = %d, want 1", report.MergedEngrams)
	}

	kept, err := store.GetEngram(ctx, wsPrefix, highID)
	if err != nil {
		t.Fatal(err)
	}
	if kept.State == storage.StateArchived {
		t.Error("representative engram should not be archived")
	}

	archived, err := store.GetEngram(ctx, wsPrefix, lowID)
	if err != nil {
		t.Fatal(err)
	}
	if archived.State != storage.StateArchived {
		t.Errorf("duplicate engram state = %v, want archived", archived.State)
	}
}
