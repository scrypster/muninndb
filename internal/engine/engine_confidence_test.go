package engine

import (
	"context"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/cognitive"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// -----------------------------------------------------------------
// Test helpers — mirror the engine-test harness used by Engine.Link
// and Engine.Forget (testEnv in engine_test.go). Confidence is seeded
// directly through Store().UpdateConfidence so each test starts from a
// known prior, independent of Write's default-confidence logic.
// -----------------------------------------------------------------

// seedEngineWithEngram wires up a real engine (live PebbleStore + FTS),
// writes one engram to vault "test", and pins its confidence to the given
// value. Returns the engine, the resolved vault prefix, and the engram ID.
func seedEngineWithEngram(t *testing.T, confidence float32) (*Engine, [8]byte, storage.ULID) {
	t.Helper()
	eng, cleanup := testEnv(t)
	t.Cleanup(cleanup)

	ctx := context.Background()
	resp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   "test",
		Concept: "seed",
		Content: "seed engram for AdjustConfidence tests",
	})
	if err != nil {
		t.Fatalf("seed Write: %v", err)
	}
	id, err := storage.ParseULID(resp.ID)
	if err != nil {
		t.Fatalf("seed ParseULID(%q): %v", resp.ID, err)
	}
	ws := eng.Store().ResolveVaultPrefix("test")
	if err := eng.Store().UpdateConfidence(ctx, ws, id, confidence); err != nil {
		t.Fatalf("seed UpdateConfidence: %v", err)
	}
	return eng, ws, id
}

// seedSecondEngram writes a second engram into the same vault and returns its
// ULID. Used by contradiction tests that need a distinct "other" endpoint.
func seedSecondEngram(t *testing.T, eng *Engine, _ [8]byte) storage.ULID {
	t.Helper()
	resp, err := eng.Write(context.Background(), &mbp.WriteRequest{
		Vault:   "test",
		Concept: "other",
		Content: "second engram for AdjustConfidence contradiction tests",
	})
	if err != nil {
		t.Fatalf("seedSecond Write: %v", err)
	}
	id, err := storage.ParseULID(resp.ID)
	if err != nil {
		t.Fatalf("seedSecond ParseULID(%q): %v", resp.ID, err)
	}
	return id
}

// vaultOf returns the vault name that resolves to ws. The seed always uses
// vault "test", so the inverse mapping is constant.
func vaultOf(_ [8]byte) string { return "test" }

// readConfidence reads the persisted confidence for an engram directly from
// the store, bypassing any engine cache invariants the engine holds.
func readConfidence(t *testing.T, eng *Engine, ws [8]byte, id storage.ULID) float32 {
	t.Helper()
	got, err := eng.Store().GetConfidence(context.Background(), ws, id)
	if err != nil {
		t.Fatalf("readConfidence: %v", err)
	}
	return got
}

// recordingConfidenceWorker is a test double for the engine's ConfidenceWorker.
// It wraps a real *cognitive.Worker[cognitive.ConfidenceUpdate] (the type
// Engine.SetCognitiveWorkers requires) whose processFn appends every processed
// batch into cw.submits. A goroutine runs Worker.Run so Submit is non-blocking
// and non-dropping, just like production. drain() cancels the run context —
// Run's shutdown path drains the input channel and flushes one final batch —
// then waits for the goroutine to return, guaranteeing every Submit made
// before drain is reflected in submits. Call drain() before asserting on
// cw.submits.
type recordingConfidenceWorker struct {
	mu      sync.Mutex
	submits []cognitive.ConfidenceUpdate

	worker   *cognitive.Worker[cognitive.ConfidenceUpdate]
	cancel   context.CancelFunc
	done     chan struct{}
	initOnce sync.Once
}

// AsWorker lazily constructs the underlying *cognitive.Worker on first call
// (so a bare &recordingConfidenceWorker{} is usable, matching the test
// injection pattern: e.SetCognitiveWorkers(nil, nil, cw.AsWorker())).
func (r *recordingConfidenceWorker) AsWorker() *cognitive.Worker[cognitive.ConfidenceUpdate] {
	r.initOnce.Do(func() {
		r.worker = cognitive.NewWorker[cognitive.ConfidenceUpdate](
			100, 1, 100*time.Millisecond,
			func(_ context.Context, batch []cognitive.ConfidenceUpdate) error {
				r.mu.Lock()
				defer r.mu.Unlock()
				r.submits = append(r.submits, batch...)
				return nil
			},
		)
		ctx, cancel := context.WithCancel(context.Background())
		r.cancel = cancel
		r.done = make(chan struct{})
		go func() {
			defer close(r.done)
			r.worker.Run(ctx) //nolint:errcheck
		}()
	})
	return r.worker
}

// drain cancels the worker's run context and waits for its shutdown flush
// to complete. After drain returns, every Submit made before the call is
// reflected in submits. Safe to call even if AsWorker was never invoked.
func (r *recordingConfidenceWorker) drain() {
	if r.cancel != nil {
		r.cancel()
		<-r.done
	}
}

// -----------------------------------------------------------------
// Tests
// -----------------------------------------------------------------

// TestEngineAdjustConfidence_Clamps verifies the delta is clamped to [0,1].
// Each case re-pins the prior to 0.5 so the wants reflect a clean starting point
// (a sequential run against the same engram would otherwise accumulate).
func TestEngineAdjustConfidence_Clamps(t *testing.T) {
	e, ws, id := seedEngineWithEngram(t, 0.5)

	cases := []struct{ delta, want float32 }{
		{0.2, 0.7},  // mid-range
		{1.0, 1.0},  // upper clamp
		{-0.9, 0.0}, // lower clamp
	}
	for _, c := range cases {
		if err := e.Store().UpdateConfidence(context.Background(), ws, id, 0.5); err != nil {
			t.Fatalf("re-seed UpdateConfidence: %v", err)
		}
		got, err := e.AdjustConfidence(context.Background(), vaultOf(ws), id, c.delta, id, false, "t", "test")
		if err != nil {
			t.Fatalf("delta %v: %v", c.delta, err)
		}
		if got != c.want {
			t.Errorf("delta %v: got %v, want %v", c.delta, got, c.want)
		}
	}
}

// TestEngineAdjustConfidence_RejectsNaNAndInf (RED-sanity: the guard is load-bearing).
func TestEngineAdjustConfidence_RejectsNaNAndInf(t *testing.T) {
	e, ws, id := seedEngineWithEngram(t, 0.5)
	for _, bad := range []float32{float32(math.NaN()), float32(math.Inf(1)), float32(math.Inf(-1))} {
		if _, err := e.AdjustConfidence(context.Background(), vaultOf(ws), id, bad, id, false, "t", "test"); err == nil {
			t.Fatalf("delta %v: expected error, got nil", bad)
		}
	}
}

// TestEngineAdjustConfidence_RejectsSelfContradiction (RED-sanity).
func TestEngineAdjustConfidence_RejectsSelfContradiction(t *testing.T) {
	e, ws, id := seedEngineWithEngram(t, 0.5)
	if _, err := e.AdjustConfidence(context.Background(), vaultOf(ws), id, -0.1, id, true, "t", "test"); err == nil {
		t.Fatal("expected self-contradiction error, got nil")
	}
}

// TestEngineAdjustConfidence_NotFoundOnUnknownEngram.
func TestEngineAdjustConfidence_NotFoundOnUnknownEngram(t *testing.T) {
	e, ws, _ := seedEngineWithEngram(t, 0.5)
	unknown := storage.NewULID()
	if _, err := e.AdjustConfidence(context.Background(), vaultOf(ws), unknown, -0.1, unknown, false, "t", "test"); err == nil {
		t.Fatal("expected NotFound for unknown engram, got nil")
	}
}

// TestEngineAdjustConfidence_NotFoundOnUnknownContradictionTarget.
func TestEngineAdjustConfidence_NotFoundOnUnknownContradictionTarget(t *testing.T) {
	e, ws, id := seedEngineWithEngram(t, 0.5)
	ghost := storage.NewULID()
	if _, err := e.AdjustConfidence(context.Background(), vaultOf(ws), id, -0.1, ghost, true, "t", "test"); err == nil {
		t.Fatal("expected NotFound for unknown contradicted_by_id, got nil")
	}
}

// TestEngineAdjustConfidence_RejectionMutatesNothing (validation-before-write).
// Every rejection path leaves the engram's confidence unchanged.
func TestEngineAdjustConfidence_RejectionMutatesNothing(t *testing.T) {
	e, ws, id := seedEngineWithEngram(t, 0.5)
	_, _ = e.AdjustConfidence(context.Background(), vaultOf(ws), id, float32(math.NaN()), id, false, "t", "test")
	if got := readConfidence(t, e, ws, id); got != 0.5 {
		t.Fatalf("confidence mutated after NaN rejection: %v", got)
	}
}

// TestEngineAdjustConfidence_BareDeltaDoesNotSubmit verifies §D3 carve-out:
// hasContra=false submits nothing to the ConfidenceWorker.
func TestEngineAdjustConfidence_BareDeltaDoesNotSubmit(t *testing.T) {
	e, ws, id := seedEngineWithEngram(t, 0.5)
	cw := &recordingConfidenceWorker{}
	e.SetCognitiveWorkers(nil, nil, cw.AsWorker())

	if _, err := e.AdjustConfidence(context.Background(), vaultOf(ws), id, 0.1, id, false, "t", "test"); err != nil {
		t.Fatal(err)
	}
	cw.drain()
	if len(cw.submits) != 0 {
		t.Fatalf("bare delta submitted %d ConfidenceUpdates, want 0", len(cw.submits))
	}
}

// TestEngineAdjustConfidence_ContradictionSubmitsEvidenceForBoth verifies the
// OnFound mirror: hasContra=true submits EvidenceContradiction for BOTH engrams.
func TestEngineAdjustConfidence_ContradictionSubmitsEvidenceForBoth(t *testing.T) {
	e, ws, id := seedEngineWithEngram(t, 0.5)
	other := seedSecondEngram(t, e, ws)
	cw := &recordingConfidenceWorker{}
	e.SetCognitiveWorkers(nil, nil, cw.AsWorker())

	if _, err := e.AdjustConfidence(context.Background(), vaultOf(ws), id, 0.0, other, true, "rag-bridge", "test"); err != nil {
		t.Fatal(err)
	}
	cw.drain()
	if len(cw.submits) != 2 {
		t.Fatalf("expected 2 EvidenceContradiction submits, got %d", len(cw.submits))
	}
	for _, s := range cw.submits {
		if s.Source != "external_contradiction" {
			t.Errorf("source = %q, want external_contradiction", s.Source)
		}
	}
}

// TestEngineAdjustConfidence_ContradictionPersistsMarker verifies the 0x0A
// contradiction marker is actually durable after Engine.AdjustConfidence
// returns, read back through the engine's own public read path
// (Engine.GetContradictions). The storage-layer test
// (TestUpdateConfidenceWithContradiction_AtomicConfidenceAndMarker) already
// pins the batched write; this test pins the engine contract — that a caller
// who passes hasContra=true can observe the marker via the engine surface,
// not just the raw store. Regression guard: if a future refactor of
// AdjustConfidence drops the contradiction arm of the composed write (e.g.,
// calls UpdateConfidence instead of UpdateConfidenceWithContradiction), the
// marker disappears and this test fails.
func TestEngineAdjustConfidence_ContradictionPersistsMarker(t *testing.T) {
	e, ws, id := seedEngineWithEngram(t, 0.5)
	other := seedSecondEngram(t, e, ws)

	if _, err := e.AdjustConfidence(context.Background(), vaultOf(ws), id, -0.1, other, true, "rag-bridge", "test"); err != nil {
		t.Fatalf("AdjustConfidence: %v", err)
	}

	pairs, err := e.GetContradictions(context.Background(), vaultOf(ws))
	if err != nil {
		t.Fatalf("GetContradictions: %v", err)
	}
	want := [2]storage.ULID{id, other}
	wantRev := [2]storage.ULID{other, id}
	found := false
	for _, p := range pairs {
		if p == want || p == wantRev {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("contradiction marker for {%s,%s} not persisted; pairs=%v", id, other, pairs)
	}
}

// NOTE on the engine-layer lost-update test (deliberately absent here, present
// at the storage layer — see TestUpdateConfidenceWithContradiction_NoLostUpdate-
// UnderConcurrentDeltas in internal/storage/confidence_caslock_test.go).
//
// #559's lost-update fix lives entirely in the storage method: the read+add+
// clamp moved inside the stripe lock. Engine.AdjustConfidence is now a thin
// pass-through — it validates (NaN/Inf, self-contradiction, existence) and
// forwards the delta to UpdateConfidenceWithContradiction, using the returned
// (prior, newConf) for the audit. TestEngineAdjustConfidence_Clamps and the
// other Task-3 engine tests cover the engine path end-to-end and pass as-is.
//
// A direct engine-layer concurrency test (50 goroutines calling
// Engine.AdjustConfidence concurrently) is blocked by a SEPARATE pre-existing
// race the detector flags under -race: Engine.AdjustConfidence's existence
// check (unlocked e.store.GetMetadata at engine.go:2532) races against another
// caller's post-commit cache.Set/metaCache.Remove inside UpdateConfidence-
// WithContradiction. That cache race is independent of #559 (the stripe lock
// correctly serializes the confidence RMW; only the auxiliary cache mutation
// and the existence-check read race) and is out of scope for this PR. The
// storage-layer test is the load-bearing proof of the lost-update fix.
