package engine

import (
	"context"
	"testing"

	"github.com/scrypster/muninndb/internal/metrics/latency"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// ---------------------------------------------------------------------------
// F2 (712-currency fix round): the COG-34 similar_existing self-query is
// scoped to the single muninn_remember (MCP) / MBP Write surface — the only
// two wire responses with a field to carry it. But every caller of that
// scope funnels through the SAME Engine.Write, including WriteBatch's
// per-item UpsertMode dispatch (writeUpsert -> upsertCreate -> Write) on
// exactly the bulk re-ingest pipeline the deferral names. Before the fix,
// a batch upsert item paid the self-query's real Activate() call and simply
// discarded the result — latency cost with no visible feature.
//
// Asserted via the latency instrumentation (internal/metrics/latency),
// not absence of output alone: a "similar_existing" sample recorded on the
// vault's tracker means Activate() was actually called for the hook.
//
// PRIVACY: vault/concept/content strings below are synthetic, authored here.
// ---------------------------------------------------------------------------

func TestWriteBatch_UpsertItem_SkipsSimilarExistingSelfQuery(t *testing.T) {
	eng, store, cleanup := testEnvWithStore(t)
	defer cleanup()
	ctx := context.Background()

	tracker := latency.New()
	eng.SetLatencyTracker(tracker)

	const vault = "similar-existing-batch-upsert-probe"
	ws := store.ResolveVaultPrefix(vault)

	// A pre-existing candidate the self-query would otherwise band `strong`
	// against the upsert item below (identical distinctive vocabulary).
	if _, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault: vault, Concept: "ledger reconciliation notes",
		Content: "Ledger reconciliation notes covering the outstanding balance and adjustment codes.",
	}); err != nil {
		t.Fatalf("write candidate: %v", err)
	}
	eng.WaitWriteTimeIdle()

	before := tracker.For(ws, "similar_existing").Count

	_, errs := eng.WriteBatch(ctx, []*mbp.WriteRequest{
		{
			Vault:        vault,
			Concept:      "ledger reconciliation notes",
			Content:      "Ledger reconciliation notes covering the outstanding balance and adjustment codes, revised.",
			UpsertMode:   true,
			IdempotentID: "ledger-recon-key-1",
		},
	})
	for i, err := range errs {
		if err != nil {
			t.Fatalf("WriteBatch item %d: %v", i, err)
		}
	}
	eng.WaitWriteTimeIdle()

	after := tracker.For(ws, "similar_existing").Count
	if after != before {
		t.Fatalf("F2 violated: a batch UpsertMode item paid the similar_existing self-query "+
			"(similar_existing samples %d -> %d) — WriteBatch's upsert dispatch must skip the "+
			"COG-34 hook entirely, not merely discard its result", before, after)
	}
}
