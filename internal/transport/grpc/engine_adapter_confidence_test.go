// Package grpc_test covers the gRPC server + adapter surface.
//
// engine_adapter_confidence_test.go pins the AdjustConfidence adapter paths
// that the parse-failure tests (TestGRPCEngineAdapter_AdjustConfidence_Bad*)
// do not exercise — specifically the §D10 compose shape where both
// ContradictedById is non-empty AND Delta != 0. The existing happy-path
// coverage (TestServer_AdjustConfidence_Success) sits at the Server level
// with a mockEngine that intercepts before the adapter runs, so the
// adapter's ULID-parse + hasContra-derivation + engine dispatch for the
// both-set shape was previously unverified.
package grpc_test

import (
	"context"
	"os"
	"testing"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/engine"
	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/engine/trigger"
	"github.com/scrypster/muninndb/internal/index/fts"
	"github.com/scrypster/muninndb/internal/storage"
	transportgrpc "github.com/scrypster/muninndb/internal/transport/grpc"
	"github.com/scrypster/muninndb/internal/transport/mbp"
	pb "github.com/scrypster/muninndb/proto/gen/go/muninn/v1"
)

// newConfidenceAdapterEnv wires a real *engine.Engine (live PebbleStore +
// FTS) using only exported constructors — the same shape as
// engine.testEnv / rest.newRESTRetryEnrichEnv, duplicated here because the
// engine package's test helpers are internal. Returns the engine + adapter
// pair + a cleanup.
func newConfidenceAdapterEnv(t *testing.T) (*engine.Engine, transportgrpc.EngineAPI, func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "muninndb-grpc-conf-adapter-*")
	if err != nil {
		t.Fatal(err)
	}
	db, err := storage.OpenPebble(dir, storage.DefaultOptions())
	if err != nil {
		os.RemoveAll(dir)
		t.Fatal(err)
	}
	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 128})
	ftsIdx := fts.New(db)
	embedder := activation.NewNoopEmbedder()
	actEngine := activation.New(store, activation.NewFTSAdapter(ftsIdx), nil, embedder)
	trigSystem := trigger.New(store, trigger.NewFTSAdapter(ftsIdx), nil, embedder)
	eng := engine.NewEngine(engine.EngineConfig{
		Store: store, FTSIndex: ftsIdx, ActivationEngine: actEngine,
		TriggerSystem: trigSystem, Embedder: embedder,
	})
	adapter := transportgrpc.TestableNewEngineAdapter(eng)
	return eng, adapter, func() {
		eng.Stop()
		store.Close()
		os.RemoveAll(dir)
	}
}

// TestGRPCEngineAdapter_AdjustConfidence_Compose exercises the §D10 compose
// path through the adapter: ContradictedById non-empty + Delta != 0. Both
// fields must reach Engine.AdjustConfidence — the confidence changes AND the
// 0x0A contradiction marker pair is persisted. The adapter derives hasContra
// from `ContradictedById != ""`; combined with a non-zero Delta this is the
// shape that a bare EngramId+Delta happy-path test cannot reach.
//
// Regression guard: if the adapter ever stops forwarding ContradictedById
// (e.g. drops the parse or the hasContra derivation), the marker disappears
// and this test fails. If it stops forwarding Delta, the confidence stays
// unchanged and this test fails.
func TestGRPCEngineAdapter_AdjustConfidence_Compose(t *testing.T) {
	eng, adapter, cleanup := newConfidenceAdapterEnv(t)
	defer cleanup()

	ctx := context.Background()
	w1, err := eng.Write(ctx, &mbp.WriteRequest{Vault: "test", Concept: "a", Content: "alpha"})
	if err != nil {
		t.Fatalf("seed Write(1): %v", err)
	}
	w2, err := eng.Write(ctx, &mbp.WriteRequest{Vault: "test", Concept: "b", Content: "beta"})
	if err != nil {
		t.Fatalf("seed Write(2): %v", err)
	}
	id, err := storage.ParseULID(w1.ID)
	if err != nil {
		t.Fatalf("ParseULID(w1=%q): %v", w1.ID, err)
	}
	other, err := storage.ParseULID(w2.ID)
	if err != nil {
		t.Fatalf("ParseULID(w2=%q): %v", w2.ID, err)
	}

	// Pin id's confidence to a known prior so the delta is observable.
	ws := eng.Store().ResolveVaultPrefix("test")
	if err := eng.Store().UpdateConfidence(ctx, ws, id, 0.5); err != nil {
		t.Fatalf("seed UpdateConfidence: %v", err)
	}

	// Compose: delta=-0.2 AND ContradictedById non-empty.
	resp, err := adapter.AdjustConfidence(ctx, &pb.AdjustConfidenceRequest{
		Vault: "test", EngramId: w1.ID, Delta: -0.2,
		ContradictedById: w2.ID, Reason: "bridge",
	})
	if err != nil {
		t.Fatalf("AdjustConfidence compose: %v", err)
	}
	if resp.NewConfidence != 0.3 {
		t.Errorf("NewConfidence = %v, want 0.3 (delta applied)", resp.NewConfidence)
	}

	// The contradiction marker must be durable — proves ContradictedById flowed
	// through the adapter with hasContra=true. Read via the engine's own
	// public read path, not the raw store.
	pairs, err := eng.GetContradictions(ctx, "test")
	if err != nil {
		t.Fatalf("GetContradictions: %v", err)
	}
	want, wantRev := [2]storage.ULID{id, other}, [2]storage.ULID{other, id}
	found := false
	for _, p := range pairs {
		if p == want || p == wantRev {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("contradiction marker for {%s,%s} not persisted via compose path; pairs=%v",
			id, other, pairs)
	}
}

// TestCallerFromContext_Branches pins the caller-extraction contract used by
// the adapter. The audit log's "caller" field is load-bearing for #559 Rev 2
// §success criteria #7, so the three branches (label preferred, ID fallback,
// anonymous for the no-key path) are table-driven and RED-checked: drop any
// branch and the corresponding case fails.
func TestCallerFromContext_Branches(t *testing.T) {
	labelled := &auth.APIKey{ID: "k_abc123", Label: "ops-bridge", Mode: auth.ModeFull}
	idOnly := &auth.APIKey{ID: "k_xyz789", Label: "", Mode: auth.ModeFull}

	cases := []struct {
		name string
		ctx  context.Context
		want string
	}{
		{"label preferred", context.WithValue(context.Background(), auth.ContextAPIKey, labelled), "ops-bridge"},
		{"id fallback when label empty", context.WithValue(context.Background(), auth.ContextAPIKey, idOnly), "k_xyz789"},
		{"anonymous when no key", context.Background(), "anonymous"},
		{"anonymous when key is nil", context.WithValue(context.Background(), auth.ContextAPIKey, (*auth.APIKey)(nil)), "anonymous"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := transportgrpc.TestableCallerFromContext(c.ctx); got != c.want {
				t.Errorf("callerFromContext = %q, want %q", got, c.want)
			}
		})
	}
}
