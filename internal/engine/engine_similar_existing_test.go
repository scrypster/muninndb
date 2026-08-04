package engine

import (
	"context"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// ---------------------------------------------------------------------------
// COG-34 (#712 remainder) — RED-5: the advisory path performs ZERO store
// writes (COG-33/COG-11's "observe-safe" shape, COG-34's own text).
//
// This is the one RED control that does NOT need a real embedder: it only
// needs Engine.Activate to return at least one row (so the write sites its
// ReadOnly flag gates have something to gate), which plain FTS keyword
// overlap under the noop test embedder already supplies. Cheap and
// unit-level, per the CI budget note in the design record.
//
// Every string below is synthetic, authored for this test only.
// ---------------------------------------------------------------------------

// recallEventCount scopes the "did this write anything" check to the ONE
// persisted side effect similarExisting's self-query can reach in this
// harness (WriteRecallEvent, 0x29) — deliberately NOT a raw global Pebble
// write-byte counter. PebbleStore runs its OWN unrelated 100ms
// counterCoalescer background flush (vault engram counts,
// internal/storage/counter_coalescer.go) independent of anything this test
// does; an earlier version of this test compared
// PebbleMetrics().WAL.BytesWritten before/after and intermittently failed
// under -race when that timer happened to fire inside the measurement
// window — a real flake in the TEST's method, not in the mechanism (caught
// by a full-package -race run; a single-test run never reproduced it,
// which is exactly the shape of contamination from an unrelated background
// writer). Counting 0x29 rows for one vault is immune to it.
func recallEventCount(t *testing.T, ctx context.Context, store *storage.PebbleStore, ws [8]byte) int {
	t.Helper()
	n := 0
	if err := store.ScanRecallEvents(ctx, ws, storage.RecallPurposeCalibration, func(storage.ULID, *storage.RecallEvent) error {
		n++
		return nil
	}); err != nil {
		t.Fatalf("ScanRecallEvents: %v", err)
	}
	return n
}

// TestSimilarExisting_RED5_ObserveSafe_ZeroStoreWrites is RED-5: the
// similar_existing self-query must write nothing to the store, under -race.
// It is a genuine RED/GREEN pair on the SAME underlying Activate() pipeline
// call, not merely a passing assertion: with ReadOnly forced false on the
// self-query, Activate's recall-event persist (engine.go, gated
// `!actReq.ReadOnly && len(items) > 0`) fires and a 0x29 row is written;
// with the production default (ReadOnly: true), it does not. That is what
// makes read_only:true on the self-query load-bearing rather than
// decorative.
func TestSimilarExisting_RED5_ObserveSafe_ZeroStoreWrites(t *testing.T) {
	eng, store, cleanup := testEnvWithStore(t)
	defer cleanup()
	ctx := context.Background()

	// A candidate that FTS alone (no embedding needed) will surface for the
	// probe below: identical distinctive vocabulary, backdated so it clears
	// the temporal floor regardless of what the probe's own timestamp is.
	backdated := time.Now().Add(-3 * time.Hour)
	if _, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault: "default", Concept: "zephyrwood catalog entry",
		Content:   "The zephyrwood catalog entry lists lead time, unit cost, and the substitute SKU.",
		CreatedAt: &backdated,
	}); err != nil {
		t.Fatalf("write candidate: %v", err)
	}
	eng.waitWriteTimeIdle()

	probeEng := &storage.Engram{
		Concept:   "zephyrwood catalog entry",
		Content:   "The zephyrwood catalog entry lists lead time, unit cost, and the substitute SKU.",
		CreatedAt: time.Now(),
	}
	ws := store.ResolveVaultPrefix("default")
	probeID := storage.NewULID()

	t.Run("RED_read_only_flag_absent_writes_the_recall_event", func(t *testing.T) {
		restore := buildSimilarExistingRequestFn
		buildSimilarExistingRequestFn = func(vault string, eng *storage.Engram) *mbp.ActivateRequest {
			req := restore(vault, eng)
			req.ReadOnly = false // sabotage: the exact contract #846 closed
			return req
		}
		defer func() { buildSimilarExistingRequestFn = restore }()

		before := recallEventCount(t, ctx, store, ws)
		adv := eng.similarExisting(ctx, ws, "default", probeID, probeEng)
		after := recallEventCount(t, ctx, store, ws)
		t.Logf("RED arm: items=%d omittedBasis=%q recall_events %d -> %d", len(adv.Items), adv.OmittedBasis, before, after)
		if after == before {
			t.Fatalf("RED-5 did not go red: expected a persisted recall event with read_only forced false and >=1 candidate returned, saw none")
		}
	})

	t.Run("GREEN_production_default_writes_nothing", func(t *testing.T) {
		before := recallEventCount(t, ctx, store, ws)
		adv := eng.similarExisting(ctx, ws, "default", probeID, probeEng)
		after := recallEventCount(t, ctx, store, ws)
		t.Logf("GREEN arm: items=%d omittedBasis=%q recall_events %d -> %d", len(adv.Items), adv.OmittedBasis, before, after)
		if after != before {
			t.Fatalf("similar_existing persisted a recall event: count %d -> %d, want no change", before, after)
		}
	})
}

// TestSimilarExisting_RED5_ObserveSafe_ArchivedEdgeNotRestored is the NEW
// RED-5 arm the F1 refute named: the ORIGINAL RED-5 above was structurally
// blind to phase 4.75's lazy archive restore because its fixture had no
// archived edge for the Bloom filter to find. This fixture has one — two
// tag-linked, FTS-matchable engrams whose association is forced into the
// 0x25 archive namespace before the self-query runs — so the self-query's
// fused candidates include a Bloom-positive ID and phase 4.75 is actually
// reachable. The RED arm forces ReadOnly false on the self-query (the exact
// F1 contract) and shows a live 0x14 weight-index row appears; the GREEN arm
// is the production default and must leave the archive undisturbed.
func TestSimilarExisting_RED5_ObserveSafe_ArchivedEdgeNotRestored(t *testing.T) {
	eng, store, db := danglingEnv(t)
	ctx := context.Background()
	const vault = "similar-existing-red5-archive-probe"
	const sharedTag = "zephyrwood-planning-notes"

	w := danglingWriter(t, eng, vault, sharedTag)
	target := w("archive rotation policy", "The zephyrwood catalog entry lists lead time and unit cost.")
	_ = w("seed neighbour", "The zephyrwood catalog entry lists lead time, unit cost, and a substitute SKU.")
	_ = target

	ws := store.VaultPrefix(vault)
	if _, err := store.DecayAssocWeights(ctx, ws, time.Nanosecond, 0.001, 0.05); err != nil {
		t.Fatalf("archiving decay: %v", err)
	}
	if n := countPrefix(t, db, ws, 0x14); n != 0 {
		t.Fatalf("precondition: edge is not archived, %d live weight-index row(s) remain", n)
	}
	if n := countPrefix(t, db, ws, 0x25); n == 0 {
		t.Fatal("precondition: no edge reached the 0x25 archive")
	}

	probeEng := &storage.Engram{
		Concept:   "zephyrwood catalog entry",
		Content:   "The zephyrwood catalog entry lists lead time, unit cost, and a substitute SKU.",
		CreatedAt: time.Now(),
	}
	probeID := storage.NewULID()

	t.Run("RED_read_only_flag_absent_restores_the_archived_edge", func(t *testing.T) {
		restore := buildSimilarExistingRequestFn
		buildSimilarExistingRequestFn = func(vaultName string, eng *storage.Engram) *mbp.ActivateRequest {
			req := restore(vaultName, eng)
			req.ReadOnly = false // sabotage: the exact contract F1 closed
			return req
		}
		defer func() { buildSimilarExistingRequestFn = restore }()

		adv := eng.similarExisting(ctx, ws, vault, probeID, probeEng)
		n := countPrefix(t, db, ws, 0x14)
		t.Logf("RED arm: items=%d omittedBasis=%q live weight-index rows=%d", len(adv.Items), adv.OmittedBasis, n)
		if n == 0 {
			t.Fatalf("RED-5 (archive arm) did not go red: expected the archived edge restored to a live weight-index row with read_only forced false, saw none")
		}
	})

	t.Run("GREEN_production_default_leaves_archive_undisturbed", func(t *testing.T) {
		// Re-archive: the RED arm above restored the edge to live.
		if _, err := store.DecayAssocWeights(ctx, ws, time.Nanosecond, 0.001, 0.05); err != nil {
			t.Fatalf("re-archiving decay: %v", err)
		}
		if n := countPrefix(t, db, ws, 0x14); n != 0 {
			t.Fatalf("precondition: edge is not re-archived, %d live weight-index row(s) remain", n)
		}

		adv := eng.similarExisting(ctx, ws, vault, probeID, probeEng)
		n := countPrefix(t, db, ws, 0x14)
		t.Logf("GREEN arm: items=%d omittedBasis=%q live weight-index rows=%d", len(adv.Items), adv.OmittedBasis, n)
		if n != 0 {
			t.Fatalf("similar_existing's read_only:true self-query restored an archived edge: %d live weight-index row(s), want 0", n)
		}
	})
}
