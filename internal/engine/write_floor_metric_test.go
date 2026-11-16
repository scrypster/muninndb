package engine

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/scrypster/muninndb/internal/metrics"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// missingSummaryCount reads the current value of the summary counter for a
// vault. The metric is process-global, so every assertion here is a DELTA.
func missingSummaryCount(t *testing.T, vault string) float64 {
	t.Helper()
	return testutil.ToFloat64(metrics.WritesMissingFieldTotal.WithLabelValues(vault, "summary"))
}

func TestObserveWriteFloor_SummaryPresence(t *testing.T) {
	tests := []struct {
		name    string
		summary string
		want    float64
	}{
		{"summary supplied", "a real one-line summary", 0},
		{"summary absent", "", 1},
		{"summary whitespace only", "   \t\n ", 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			eng, cleanup := testEnv(t)
			defer cleanup()
			ctx := context.Background()

			before := missingSummaryCount(t, "default")
			if _, err := eng.Write(ctx, &mbp.WriteRequest{
				Vault: "default", Concept: "c-" + tc.name, Content: "content for " + tc.name,
				Summary: tc.summary,
			}); err != nil {
				t.Fatalf("Write: %v", err)
			}
			if got := missingSummaryCount(t, "default") - before; got != tc.want {
				t.Errorf("delta = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestObserveWriteFloor_BatchCountsPerEngram(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	before := missingSummaryCount(t, "default")
	reqs := []*mbp.WriteRequest{
		{Vault: "default", Concept: "b1", Content: "batch one", Summary: "has a summary"},
		{Vault: "default", Concept: "b2", Content: "batch two"},
		{Vault: "default", Concept: "b3", Content: "batch three"},
	}
	_, errs := eng.WriteBatch(ctx, reqs)
	for i, err := range errs {
		if err != nil {
			t.Fatalf("WriteBatch[%d]: %v", i, err)
		}
	}
	if got := missingSummaryCount(t, "default") - before; got != 2 {
		t.Errorf("delta = %v, want 2 (only the two without a summary)", got)
	}
}

// Under background_only the caller's fields are discarded by design and a
// background stage owns the summary, so the write-time floor is not observed,
// even for a write that carried a summary (the maintainer's repro on #888).
// Covers both observation sites.
func TestObserveWriteFloor_BackgroundOnlyNotObserved(t *testing.T) {
	eng, cleanup := testEnvWithInlineMode(t, "background_only")
	defer cleanup()
	ctx := context.Background()

	before := missingSummaryCount(t, "test-vault")
	if _, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault: "test-vault", Concept: "bg1", Content: "background only, with summary",
		Summary: "a caller summary the vault discards",
	}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	_, errs := eng.WriteBatch(ctx, []*mbp.WriteRequest{
		{Vault: "test-vault", Concept: "bg2", Content: "batch, with summary", Summary: "discarded"},
		{Vault: "test-vault", Concept: "bg3", Content: "batch, without summary"},
	})
	for i, err := range errs {
		if err != nil {
			t.Fatalf("WriteBatch[%d]: %v", i, err)
		}
	}
	if got := missingSummaryCount(t, "test-vault") - before; got != 0 {
		t.Errorf("delta = %v, want 0 (background_only writes are not observed)", got)
	}
}

// Regression guard for the placement decision: upsertCreate delegates to Write,
// which observes on its own. Observing at BOTH sites would count one create
// twice. This asserts exactly one observation for one upsert-create.
func TestObserveWriteFloor_UpsertCreateNotDoubleCounted(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	before := missingSummaryCount(t, "default")
	if _, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault: "default", Concept: "u1", Content: "upsert v1",
		UpsertMode: true, IdempotentID: "upsert-key-1",
	}); err != nil {
		t.Fatalf("upsert create: %v", err)
	}
	if got := missingSummaryCount(t, "default") - before; got != 1 {
		t.Errorf("delta = %v, want exactly 1 (2 would mean the delegated Write was double-observed)", got)
	}
}

// Regression guard for the second placement decision: the identical-content
// upsert branch is a no-op ("no write, no evolve, no index churn"), so nothing
// was stored and nothing must be observed.
func TestObserveWriteFloor_UpsertNoOpNotCounted(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	req := func() *mbp.WriteRequest {
		return &mbp.WriteRequest{
			Vault: "default", Concept: "u2", Content: "identical content",
			UpsertMode: true, IdempotentID: "upsert-key-2",
		}
	}
	if _, err := eng.Write(ctx, req()); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	afterCreate := missingSummaryCount(t, "default")

	if _, err := eng.Write(ctx, req()); err != nil {
		t.Fatalf("second upsert (no-op): %v", err)
	}
	if got := missingSummaryCount(t, "default") - afterCreate; got != 0 {
		t.Errorf("delta = %v, want 0 (the no-op branch stores nothing)", got)
	}
}
