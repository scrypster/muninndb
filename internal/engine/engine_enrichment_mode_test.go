package engine

import (
	"context"
	"os"
	"testing"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/engine/trigger"
	"github.com/scrypster/muninndb/internal/index/fts"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// testEnvWithInlineMode creates a fully functional Engine whose "test-vault"
// vault is configured with the supplied inlineEnrichment mode string.
// This requires wiring an auth.Store so resolveVaultPlasticity can read the
// vault-level plasticity config.
//
// testEnv passes nil for auth.Store; this helper wires a real auth.Store so
// the engine can read per-vault InlineEnrichment config via resolveVaultPlasticity.
func testEnvWithInlineMode(t *testing.T, inlineEnrichment string) (*Engine, func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "muninndb-enrichment-mode-test-*")
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

	// Wire an auth.Store so the vault plasticity config is respected.
	authStore := auth.NewStore(db)
	ie := inlineEnrichment
	if err := authStore.SetVaultConfig(auth.VaultConfig{
		Name:   "test-vault",
		Public: true,
		Plasticity: &auth.PlasticityConfig{
			InlineEnrichment: &ie,
		},
	}); err != nil {
		store.Close()
		os.RemoveAll(dir)
		t.Fatalf("SetVaultConfig: %v", err)
	}

	embedder := &noopEmbedder{}
	actEngine := activation.New(store, &ftsAdapter{ftsIdx}, nil, embedder)
	trigSystem := trigger.New(store, &ftsTrigAdapter{ftsIdx}, nil, embedder)
	eng := NewEngine(EngineConfig{Store: store, AuthStore: authStore, FTSIndex: ftsIdx, ActivationEngine: actEngine, TriggerSystem: trigSystem, Embedder: embedder})

	return eng, func() {
		eng.Stop()
		store.Close()
		os.RemoveAll(dir)
	}
}

// TestWrite_DisabledMode_CallerSummaryStored verifies that when the vault
// InlineEnrichment mode is "disabled", a caller-provided Summary is still
// persisted to the engram.
//
// BUG: currently the engine treats "disabled" like "background_only" and
// silently drops caller enrichment data. This test FAILS before the fix.
func TestWrite_DisabledMode_CallerSummaryStored(t *testing.T) {
	eng, cleanup := testEnvWithInlineMode(t, "disabled")
	defer cleanup()
	ctx := context.Background()

	resp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   "test-vault",
		Concept: "test concept",
		Content: "some content",
		Summary: "caller provided summary",
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	readResp, err := eng.Read(ctx, &mbp.ReadRequest{
		Vault: "test-vault",
		ID:    resp.ID,
	})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	if readResp.Summary != "caller provided summary" {
		t.Errorf("Summary = %q, want %q", readResp.Summary, "caller provided summary")
	}
}

// TestWrite_BackgroundOnlyMode_CallerSummaryIgnored verifies that when the
// vault InlineEnrichment mode is "background_only", caller-provided Summary is
// NOT persisted (the LLM background enrichment wins instead).
//
// This is correct behavior and should PASS both before and after the fix.
func TestWrite_BackgroundOnlyMode_CallerSummaryIgnored(t *testing.T) {
	eng, cleanup := testEnvWithInlineMode(t, "background_only")
	defer cleanup()
	ctx := context.Background()

	resp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   "test-vault",
		Concept: "test concept",
		Content: "some content",
		Summary: "should be ignored",
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	readResp, err := eng.Read(ctx, &mbp.ReadRequest{
		Vault: "test-vault",
		ID:    resp.ID,
	})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	// LLM enrichment would win in production; in tests the background worker is
	// a no-op so summary will be empty either way.
	if readResp.Summary != "" {
		t.Errorf("Summary = %q, want empty string (background_only should ignore caller data)", readResp.Summary)
	}
}

// TestWriteBatch_DisabledMode_CallerSummaryStored verifies that when the vault
// InlineEnrichment mode is "disabled", a caller-provided Summary inside a
// WriteBatch call is still persisted to each engram.
//
// BUG: currently the engine's WriteBatch switch also drops caller enrichment
// data for "disabled" mode. This test FAILS before the fix.
func TestWriteBatch_DisabledMode_CallerSummaryStored(t *testing.T) {
	eng, cleanup := testEnvWithInlineMode(t, "disabled")
	defer cleanup()
	ctx := context.Background()

	reqs := []*mbp.WriteRequest{
		{
			Vault:   "test-vault",
			Concept: "batch concept",
			Content: "batch content",
			Summary: "batch caller summary",
		},
	}

	responses, errs := eng.WriteBatch(ctx, reqs)
	if errs[0] != nil {
		t.Fatalf("WriteBatch: %v", errs[0])
	}

	readResp, err := eng.Read(ctx, &mbp.ReadRequest{
		Vault: "test-vault",
		ID:    responses[0].ID,
	})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	if readResp.Summary != "batch caller summary" {
		t.Errorf("Summary = %q, want %q", readResp.Summary, "batch caller summary")
	}
}
