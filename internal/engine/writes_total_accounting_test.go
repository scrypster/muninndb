package engine

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/scrypster/muninndb/internal/metrics"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// writesTotal reads the process-global engram-write counter. Every assertion
// below is a DELTA, since the counter is shared across the test binary.
func writesTotal(t *testing.T) float64 {
	t.Helper()
	return testutil.ToFloat64(metrics.EngineWritesTotal)
}

func TestEngineWritesTotal_PlainWriteCountedOnce(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	before := writesTotal(t)
	if _, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault: "default", Concept: "plain", Content: "a plain write",
	}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := writesTotal(t) - before; got != 1 {
		t.Errorf("delta = %v, want 1", got)
	}
}

// upsertCreate delegates the write to e.Write, which counts on its own. Before
// the fix this path incremented in both places.
func TestEngineWritesTotal_UpsertCreateCountedOnce(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	before := writesTotal(t)
	if _, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault: "default", Concept: "u", Content: "upsert v1",
		UpsertMode: true, IdempotentID: "acct-key-1",
	}); err != nil {
		t.Fatalf("upsert create: %v", err)
	}
	if got := writesTotal(t) - before; got != 1 {
		t.Errorf("delta = %v, want 1 (2 = the delegated Write was counted twice)", got)
	}
}

// The identical-content branch is a documented no-op: "no write, no evolve, no
// index churn". Nothing is stored, so the engram counter must not move.
func TestEngineWritesTotal_UpsertNoOpNotCounted(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	req := func() *mbp.WriteRequest {
		return &mbp.WriteRequest{
			Vault: "default", Concept: "u2", Content: "identical content",
			UpsertMode: true, IdempotentID: "acct-key-2",
		}
	}
	if _, err := eng.Write(ctx, req()); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	afterCreate := writesTotal(t)

	resp, err := eng.Write(ctx, req())
	if err != nil {
		t.Fatalf("second upsert (no-op): %v", err)
	}
	if resp.Hint != "upsert-identical" {
		t.Fatalf("expected the no-op branch, got hint %q", resp.Hint)
	}
	if got := writesTotal(t) - afterCreate; got != 0 {
		t.Errorf("delta = %v, want 0 (nothing is written on the no-op path)", got)
	}
}

// Guard that the fix did not silently stop counting the upsert-evolve path,
// which is the only site that counts an evolve (evolveAtInternal does not).
func TestEngineWritesTotal_UpsertEvolveStillCounted(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	if _, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault: "default", Concept: "u3", Content: "original content",
		UpsertMode: true, IdempotentID: "acct-key-3",
	}); err != nil {
		t.Fatalf("upsert create: %v", err)
	}
	afterCreate := writesTotal(t)

	if _, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault: "default", Concept: "u3", Content: "CHANGED content",
		UpsertMode: true, IdempotentID: "acct-key-3",
	}); err != nil {
		t.Fatalf("upsert evolve: %v", err)
	}
	if got := writesTotal(t) - afterCreate; got != 1 {
		t.Errorf("delta = %v, want 1 (the evolve branch is the only counter for that path)", got)
	}
}

func TestEngineWritesTotal_BatchCountsEachEngram(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	before := writesTotal(t)
	_, errs := eng.WriteBatch(ctx, []*mbp.WriteRequest{
		{Vault: "default", Concept: "b1", Content: "batch one"},
		{Vault: "default", Concept: "b2", Content: "batch two"},
		{Vault: "default", Concept: "b3", Content: "batch three"},
	})
	for i, err := range errs {
		if err != nil {
			t.Fatalf("WriteBatch[%d]: %v", i, err)
		}
	}
	if got := writesTotal(t) - before; got != 3 {
		t.Errorf("delta = %v, want 3", got)
	}
}
