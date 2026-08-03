package engine

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/cognitive"
	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/engine/trigger"
	"github.com/scrypster/muninndb/internal/index/fts"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// testEnvWithContradictWorker is testEnv plus a live ContradictWorker whose
// flush interval is compressed from the production 30s to flushInterval, so
// detection behaviour can be asserted in a couple of seconds rather than by
// sleeping past a production interval.
func testEnvWithContradictWorker(t *testing.T, flushInterval time.Duration) (*Engine, func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "muninndb-contradiction-*")
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

	cw := cognitive.NewContradictWorker(cognitive.NewContradictStoreAdapter(store))
	cw.Worker.SetFlushInterval(flushInterval)

	ctx, cancel := context.WithCancel(context.Background())
	workerDone := make(chan struct{})
	go func() { defer close(workerDone); _ = cw.Worker.Run(ctx) }()

	eng := NewEngine(EngineConfig{
		Store: store, FTSIndex: ftsIdx, ActivationEngine: actEngine,
		TriggerSystem: trigSystem, Embedder: embedder,
		ContradictWorker: cw.Worker,
	})
	return eng, func() {
		cancel()
		<-workerDone
		eng.Stop()
		store.Close()
		os.RemoveAll(dir)
	}
}

// TestContradictionDetection_SurvivesAnActiveSession is the engine-level RED
// repro for #764 D1.
//
// The declared path is wired correctly — Link submits a matrix-eligible
// pseudo-pair to ContradictWorker, which flags the 0x0A marker. What broke it
// was the generic Worker's flush ticker resetting on every received item:
// Engine.Write submits a ContradictItem on every remember, so an agent doing
// ordinary session work held the flush off indefinitely and the declared
// contradiction sat at pending forever. Both round-7 evaluators hit exactly
// this shape (polling past 35s, and 60+ seconds across calls, while continuing
// to work).
//
// The test reproduces the session, not the wall-clock: it links a contradiction
// and then keeps writing ordinary memories for the whole window. Under the bug
// the count is zero at every poll regardless of scheduling, so the failing
// direction cannot flake.
//
// The write rate matters and is asserted, not assumed. Two things can flush the
// batch: the ticker (the broken one) and reaching batchSize (50 items, the
// escape hatch that made an earlier draft of this test pass at bb10f30). A real
// agent session writes a handful of memories a minute — enough to keep resetting
// a 30s ticker, nowhere near 50 items — so the test writes fast enough to hold
// the ticker off and few enough to stay well under the size flush, and fails
// loudly if the second condition ever stops holding.
func TestContradictionDetection_SurvivesAnActiveSession(t *testing.T) {
	const (
		flushInterval = 300 * time.Millisecond
		sessionLen    = 3 * time.Second
		writeEvery    = 100 * time.Millisecond
		// cognitive.NewContradictWorker's batchSize. Staying under it is what
		// keeps this a test of the TICKER.
		contradictBatchSize = 50
	)
	eng, cleanup := testEnvWithContradictWorker(t, flushInterval)
	defer cleanup()
	ctx := context.Background()

	a, err := eng.Write(ctx, &mbp.WriteRequest{Vault: "test", Concept: "request timeout limit",
		Content: "the request timeout limit is 180ms"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := eng.Write(ctx, &mbp.WriteRequest{Vault: "test", Concept: "request timeout limit revised",
		Content: "the request timeout limit is 320ms"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Link(ctx, &mbp.LinkRequest{Vault: "test", SourceID: b.ID, TargetID: a.ID,
		RelType: uint16(storage.RelContradicts)}); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(sessionLen)
	// 3 items are already in flight (two Writes + the Link).
	submitted := 3
	for i := 0; time.Now().Before(deadline); i++ {
		// Ordinary session traffic: every Write submits a ContradictItem.
		if _, err := eng.Write(ctx, &mbp.WriteRequest{Vault: "test",
			Concept: fmt.Sprintf("note-%d", i), Content: fmt.Sprintf("unrelated note %d", i)}); err != nil {
			t.Fatal(err)
		}
		submitted++
		if submitted >= contradictBatchSize {
			t.Fatalf("test submitted %d items — enough to trigger the batchSize flush, which would make this pass for the wrong reason; slow the write rate", submitted)
		}
		rep, err := eng.GetContradictionReport(ctx, "test")
		if err != nil {
			t.Fatal(err)
		}
		if rep.DetectedCount == 1 {
			return // detected while the session stayed busy — the fixed behaviour
		}
		time.Sleep(writeEvery)
	}
	t.Fatalf("RED (#764 D1): declared contradiction never detected in %v of an active session (flush interval %v)", sessionLen, flushInterval)
}

// TestContradictionDeclaration_IsImmediatelyVisible pins the half that already
// worked and must keep working: the declaration is durable at Link return and
// the report says so without waiting for any worker.
func TestContradictionDeclaration_IsImmediatelyVisible(t *testing.T) {
	eng, cleanup := testEnvWithContradictWorker(t, time.Hour) // worker will never flush
	defer cleanup()
	ctx := context.Background()

	a, _ := eng.Write(ctx, &mbp.WriteRequest{Vault: "test", Concept: "limit", Content: "the limit is 180ms"})
	b, _ := eng.Write(ctx, &mbp.WriteRequest{Vault: "test", Concept: "limit-new", Content: "the limit is 320ms"})
	if _, err := eng.Link(ctx, &mbp.LinkRequest{Vault: "test", SourceID: b.ID, TargetID: a.ID,
		RelType: uint16(storage.RelContradicts)}); err != nil {
		t.Fatal(err)
	}

	rep, err := eng.GetContradictionReport(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Pairs) != 1 {
		t.Fatalf("want 1 reported pair immediately after link, got %d: %+v", len(rep.Pairs), rep.Pairs)
	}
	if rep.Pairs[0].Status != ContradictionDeclared {
		t.Errorf("status = %q, want %q — the declaration is durable at Link return, so it is not 'pending'",
			rep.Pairs[0].Status, ContradictionDeclared)
	}
	if rep.Pairs[0].ConfidencePenalty != ContradictionPenaltyPending {
		t.Errorf("confidence_penalty = %q, want %q", rep.Pairs[0].ConfidencePenalty, ContradictionPenaltyPending)
	}
}
