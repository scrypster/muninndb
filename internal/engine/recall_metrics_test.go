package engine

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/index/fts"
	"github.com/scrypster/muninndb/internal/metrics"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// stubHNSW is a non-nil activation.HNSWIndex so phase1's embedder branch is
// entered (mirrors activation.minimalHNSW, unexported there — this is the
// internal/engine-package equivalent needed to drive Engine.activateCore's
// #606 metrics through the real pipeline instead of a hand-built result).
type stubHNSW struct{}

func (stubHNSW) Search(_ context.Context, _ [8]byte, _ []float32, _ int) ([]activation.ScoredID, error) {
	return nil, nil
}

// zeroVecEmbedder always returns an all-zero, non-error embedding — the
// "err==nil but unusable" degradation path phase1 treats identically to an
// unreachable backend (see engine/activation/engine.go's isZeroVector doc).
type zeroVecEmbedder struct{}

func (zeroVecEmbedder) Embed(_ context.Context, _ []string) ([]float32, error) {
	return make([]float32, 8), nil
}
func (zeroVecEmbedder) Tokenize(text string) []string { return []string{text} }

// testEnvWithHNSW is like testEnv but wires a non-nil HNSWIndex so phase1
// actually attempts to embed (testEnv's plain noopEmbedder path never
// reaches the degradation checks because activation.New's caller leaves hnsw
// nil, which phase1 short-circuits on).
func testEnvWithHNSW(t *testing.T, embedder activation.Embedder) (*Engine, func()) {
	t.Helper()
	dir := t.TempDir()
	db, err := storage.OpenPebble(dir, storage.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 1000})
	ftsIdx := fts.New(db)
	actEngine := activation.New(store, &ftsAdapter{ftsIdx}, stubHNSW{}, embedder)
	eng := NewEngine(EngineConfig{Store: store, FTSIndex: ftsIdx, ActivationEngine: actEngine, Embedder: embedder})
	return eng, func() {
		eng.Stop()
		store.Close()
	}
}

// TestActivate_SemanticDegraded_IncrementsRecallEmbedFallbackMetric proves
// #606: an activation call whose embedding could not be trusted (here, an
// all-zero vector from a non-erroring embedder — same degradation class as
// an unreachable backend) increments muninndb_recall_embed_fallback_total
// for that vault, in addition to the existing per-call SemanticDegraded
// response field.
func TestActivate_SemanticDegraded_IncrementsRecallEmbedFallbackMetric(t *testing.T) {
	eng, cleanup := testEnvWithHNSW(t, zeroVecEmbedder{})
	defer cleanup()
	ctx := context.Background()
	const vault = "recall-metrics-degraded-vault"

	if _, err := eng.Write(ctx, &mbp.WriteRequest{Vault: vault, Content: "some memory content for the vault"}); err != nil {
		t.Fatalf("Write: %v", err)
	}

	before := testutil.ToFloat64(metrics.RecallEmbedFallbackTotal.WithLabelValues(vault))

	resp, err := eng.Activate(ctx, &mbp.ActivateRequest{
		Vault:   vault,
		Context: []string{"some memory content"},
	})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if !resp.SemanticDegraded {
		t.Fatalf("expected SemanticDegraded=true on the response (all-zero embedding), got false")
	}

	after := testutil.ToFloat64(metrics.RecallEmbedFallbackTotal.WithLabelValues(vault))
	if after != before+1 {
		t.Fatalf("muninndb_recall_embed_fallback_total{vault=%q} = %v, want %v", vault, after, before+1)
	}
}

// TestActivate_HardError_IncrementsRecallErrorsMetric proves #606's error
// counter: a hard activation failure (here, a context already cancelled
// before Run) increments muninndb_recall_errors_total{vault,reason} — never
// silently counted only as a returned error with no operator-visible signal.
func TestActivate_HardError_IncrementsRecallErrorsMetric(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	const vault = "recall-metrics-error-vault"

	if _, err := eng.Write(context.Background(), &mbp.WriteRequest{Vault: vault, Content: "seed memory"}); err != nil {
		t.Fatalf("Write: %v", err)
	}

	before := testutil.ToFloat64(metrics.RecallErrorsTotal.WithLabelValues(vault, "timeout"))

	// A context whose deadline is already past: Run's early ctx.Err() guards
	// (and any deadline-derived sub-context) surface context.DeadlineExceeded.
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Nanosecond)
	defer cancel()
	time.Sleep(2 * time.Millisecond)

	_, err := eng.Activate(ctx, &mbp.ActivateRequest{
		Vault:   vault,
		Context: []string{"seed"},
	})
	if err == nil {
		t.Fatalf("expected Activate to fail with an already-expired context")
	}

	after := testutil.ToFloat64(metrics.RecallErrorsTotal.WithLabelValues(vault, "timeout"))
	if after != before+1 {
		t.Fatalf("muninndb_recall_errors_total{vault=%q,reason=timeout} = %v, want %v (err=%v)", vault, after, before+1, err)
	}
}
