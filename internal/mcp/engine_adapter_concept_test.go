package mcp

// Adapter-level regression coverage for #721: muninn_evolve's response must
// report the memory's concept back to the caller.
//
// TestResolveEvolveConcept (engine_adapter_test.go) pins the precedence rule
// inside resolveEvolveConcept but never calls mcpEngineAdapter.Evolve, so it
// stayed green even when the adapter never wired the helper's result into
// WriteResult.Concept at all — the actual #721 bug. This test stands up a
// real *engine.Engine and goes through the adapter method end-to-end so the
// wiring itself is under test, not just the helper.

import (
	"context"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/engine"
	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/engine/trigger"
	"github.com/scrypster/muninndb/internal/index/fts"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// newConceptAdapterEnv wires a real *engine.Engine (live PebbleStore + FTS)
// using only exported constructors — the same shape as
// rest.newRESTRetryEnrichEnv and grpc's newConfidenceAdapterEnv. AuthStore is
// required: the write path resolves vault plasticity and, for a vault with no
// explicit config, logs a WARN and proceeds — that WARN is expected here, not
// a failure. Shared with engine_adapter_decide_concept_test.go, whose
// muninn_decide regression test needs the identical harness.
func newConceptAdapterEnv(t *testing.T) (*mcpEngineAdapter, func()) {
	t.Helper()

	db, err := storage.OpenPebble(t.TempDir(), storage.DefaultOptions())
	if err != nil {
		t.Fatalf("OpenPebble: %v", err)
	}

	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 128})
	ftsIdx := fts.New(db)
	embedder := activation.NewNoopEmbedder()
	actEngine := activation.New(store, activation.NewFTSAdapter(ftsIdx), nil, embedder)
	trigSystem := trigger.New(store, trigger.NewFTSAdapter(ftsIdx), nil, embedder)
	eng := engine.NewEngine(engine.EngineConfig{
		Store:            store,
		AuthStore:        auth.NewStore(db),
		FTSIndex:         ftsIdx,
		ActivationEngine: actEngine,
		TriggerSystem:    trigSystem,
		Embedder:         embedder,
	})

	return &mcpEngineAdapter{eng: eng}, func() {
		eng.Stop()
		store.Close()
	}
}

// TestMCPEngineAdapterEvolve_SetsConcept is the adapter-level regression guard
// for #721: muninn_evolve's response must report the stored concept, whether
// it was inherited from the predecessor engram (caller omitted `concept`) or
// supplied explicitly on the evolve call.
func TestMCPEngineAdapterEvolve_SetsConcept(t *testing.T) {
	a, cleanup := newConceptAdapterEnv(t)
	defer cleanup()

	ctx := context.Background()
	seed, err := a.eng.Write(ctx, &mbp.WriteRequest{
		Vault:   "default",
		Concept: "Alice the explorer",
		Content: "Alice is planning a trip to the Andes",
	})
	if err != nil {
		t.Fatalf("seed Write: %v", err)
	}

	t.Run("inherited concept", func(t *testing.T) {
		got, err := a.Evolve(ctx, "default", seed.ID, "Alice postponed the trip to next spring",
			"schedule slipped", nil, "", nil, nil, time.Time{})
		if err != nil {
			t.Fatalf("Evolve: %v", err)
		}
		if got.Concept != "Alice the explorer" {
			t.Errorf("Concept = %q, want inherited %q", got.Concept, "Alice the explorer")
		}
	})

	t.Run("explicit concept", func(t *testing.T) {
		got, err := a.Evolve(ctx, "default", seed.ID, "Alice is now planning for the Alps instead",
			"destination changed", nil, "Alice the mountaineer", nil, nil, time.Time{})
		if err != nil {
			t.Fatalf("Evolve: %v", err)
		}
		if got.Concept != "Alice the mountaineer" {
			t.Errorf("Concept = %q, want explicit %q", got.Concept, "Alice the mountaineer")
		}
	})
}
