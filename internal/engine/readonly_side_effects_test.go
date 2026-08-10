package engine

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/cognitive"
	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/engine/trigger"
	"github.com/scrypster/muninndb/internal/index/fts"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// ---------------------------------------------------------------------------
// #598 follow-up: activateCore resolves a single effective read-only decision
// (auth.ObserveFromContext(ctx) || req.ReadOnly, stored on actReq.ReadOnly) but
// three post-Run write sites — the recall-event persist, the Hebbian
// co-activation submit, and (transitively, via the prevActivation snapshot) the
// PAS transition record — gated on auth.ObserveFromContext(ctx) ALONE, ignoring
// an explicit req.ReadOnly from a full-mode credential. A full-mode caller
// asking for read_only:true therefore still bonds every returned engram
// pairwise and still persists a recall event — exactly the write side effects
// the muninn_recall tool description and COG-11 promise are suppressed.
//
// This does NOT resolve #598's headline claim (that recall retries are
// self-defeating because co-activation learning distorts later ranking) —
// that needs a measurement and stays a separate, open issue. This fixes only
// the read_only contract violation.
//
// PRIVACY: every vault/concept/content string below is synthetic and authored
// here — no real colleague, corpus, or product name.
// ---------------------------------------------------------------------------

// hebbianReadOnlyEnv wires an Engine with a REAL HebbianWorker (backed by the
// same Pebble store, via cognitive.NewHebbianStoreAdapter) so co-activation
// submission actually lands a weight change, drainable deterministically with
// hw.Stop() (mirrors TestHebbian_WeightStrengthenAfterCoActivation).
func hebbianReadOnlyEnv(t *testing.T) (eng *Engine, as *auth.Store, store *storage.PebbleStore, hw *cognitive.HebbianWorker, cleanup func()) {
	t.Helper()
	dir := t.TempDir()

	db, err := storage.OpenPebble(dir, storage.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	store = storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 1000})
	ftsIdx := fts.New(db)
	embedder := &noopEmbedder{}
	actEngine := activation.New(store, &ftsAdapter{ftsIdx}, nil, embedder)
	trigSystem := trigger.New(store, &ftsTrigAdapter{ftsIdx}, nil, embedder)
	as = auth.NewStore(db)
	eng = NewEngine(EngineConfig{Store: store, AuthStore: as, FTSIndex: ftsIdx, ActivationEngine: actEngine, TriggerSystem: trigSystem, Embedder: embedder})

	hebAdapter := cognitive.NewHebbianStoreAdapter(store)
	hw = cognitive.NewHebbianWorker(hebAdapter)
	eng.SetCognitiveWorkers(hw, nil, nil)

	return eng, as, store, hw, func() {
		hw.Stop()
		eng.Stop()
		store.Close()
	}
}

// seedReadOnlyPair writes a probe/partner engram pair into vault, wires a
// public vault config (default plasticity — Hebbian enabled), links the pair
// with a low starting association weight, and returns their IDs plus that
// weight.
func seedReadOnlyPair(t *testing.T, eng *Engine, as *auth.Store, store *storage.PebbleStore, vault string) (probeID, partnerID storage.ULID, initialWeight float32) {
	t.Helper()
	ctx := context.Background()

	if err := as.SetVaultConfig(auth.VaultConfig{Name: vault, Public: true}); err != nil {
		t.Fatalf("SetVaultConfig: %v", err)
	}

	probeResp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   vault,
		Concept: "kiln firing schedule",
		Content: "the bisque kiln ramps at eighty degrees an hour to cone zero four",
	})
	if err != nil {
		t.Fatalf("Write(probe): %v", err)
	}
	partnerResp, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   vault,
		Concept: "glaze mixing ratio",
		Content: "the celadon glaze is mixed at one part ash to two parts feldspar",
	})
	if err != nil {
		t.Fatalf("Write(partner): %v", err)
	}
	awaitFTS(t, eng)

	probeID, err = storage.ParseULID(probeResp.ID)
	if err != nil {
		t.Fatalf("ParseULID(probe): %v", err)
	}
	partnerID, err = storage.ParseULID(partnerResp.ID)
	if err != nil {
		t.Fatalf("ParseULID(partner): %v", err)
	}

	ws := store.ResolveVaultPrefix(vault)
	if err := store.WriteAssociation(ctx, ws, probeID, partnerID, &storage.Association{
		TargetID:   partnerID,
		RelType:    storage.RelRelatesTo,
		Weight:     0.1,
		Confidence: 1.0,
		CreatedAt:  time.Now(),
	}); err != nil {
		t.Fatalf("WriteAssociation: %v", err)
	}
	initialWeight, err = store.GetAssocWeight(ctx, ws, probeID, partnerID)
	if err != nil {
		t.Fatalf("GetAssocWeight: %v", err)
	}
	return probeID, partnerID, initialWeight
}

// TestActivateReadOnly_FullMode_SuppressesHebbianWrite is the RED test for the
// big one: a read_only:true recall from a FULL-mode credential must not
// strengthen the association between the engrams it returns.
func TestActivateReadOnly_FullMode_SuppressesHebbianWrite(t *testing.T) {
	eng, as, store, hw, cleanup := hebbianReadOnlyEnv(t)
	defer cleanup()
	ctx := context.Background()

	const vault = "readonly-fullmode-hebbian"
	probeID, partnerID, initialWeight := seedReadOnlyPair(t, eng, as, store, vault)

	resp, err := eng.Activate(withMode(auth.ModeFull), &mbp.ActivateRequest{
		Vault:      vault,
		Context:    []string{"kiln firing schedule", "glaze mixing ratio"},
		MaxResults: 10,
		Threshold:  0.001,
		ReadOnly:   true,
	})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if len(resp.Activations) < 2 {
		t.Fatalf("expected both probe and partner in activations, got %d", len(resp.Activations))
	}

	hw.Stop() // drains any pending co-activation submit deterministically

	ws := store.ResolveVaultPrefix(vault)
	after, err := store.GetAssocWeight(ctx, ws, probeID, partnerID)
	if err != nil {
		t.Fatalf("GetAssocWeight: %v", err)
	}
	if after != initialWeight {
		t.Errorf("read_only:true recall (full-mode credential) changed the "+
			"probe/partner association weight from %v to %v — Hebbian "+
			"co-activation was submitted despite req.ReadOnly=true", initialWeight, after)
	}
}

// TestActivate_NotReadOnly_StillHebbianStrengthens is the positive control:
// the fix must not neutralise Hebbian learning for an ordinary (non-read-only)
// recall — otherwise the "fix" would just be breaking the feature everywhere.
func TestActivate_NotReadOnly_StillHebbianStrengthens(t *testing.T) {
	eng, as, store, hw, cleanup := hebbianReadOnlyEnv(t)
	defer cleanup()
	ctx := context.Background()

	const vault = "readonly-fullmode-hebbian-control"
	probeID, partnerID, initialWeight := seedReadOnlyPair(t, eng, as, store, vault)

	resp, err := eng.Activate(withMode(auth.ModeFull), &mbp.ActivateRequest{
		Vault:      vault,
		Context:    []string{"kiln firing schedule", "glaze mixing ratio"},
		MaxResults: 10,
		Threshold:  0.001,
		ReadOnly:   false,
	})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if len(resp.Activations) < 2 {
		t.Fatalf("expected both probe and partner in activations, got %d", len(resp.Activations))
	}

	hw.Stop()

	ws := store.ResolveVaultPrefix(vault)
	after, err := store.GetAssocWeight(ctx, ws, probeID, partnerID)
	if err != nil {
		t.Fatalf("GetAssocWeight: %v", err)
	}
	if after <= initialWeight {
		t.Errorf("ordinary (read_only:false) recall did not strengthen the "+
			"probe/partner association (%v -> %v); the fix must not disable "+
			"Hebbian learning outright", initialWeight, after)
	}
}

// TestActivateReadOnly_ObserveMode_StillSuppresses is the regression guard in
// the OTHER direction: an observe-mode credential must keep suppressing
// Hebbian writes even when req.ReadOnly is left false (the two gates are
// distinct — a credential property and a per-call request — and both must
// independently suppress).
func TestActivateReadOnly_ObserveMode_StillSuppresses(t *testing.T) {
	eng, as, store, hw, cleanup := hebbianReadOnlyEnv(t)
	defer cleanup()
	ctx := context.Background()

	const vault = "readonly-observemode-hebbian"
	probeID, partnerID, initialWeight := seedReadOnlyPair(t, eng, as, store, vault)

	resp, err := eng.Activate(withMode(auth.ModeObserve), &mbp.ActivateRequest{
		Vault:      vault,
		Context:    []string{"kiln firing schedule", "glaze mixing ratio"},
		MaxResults: 10,
		Threshold:  0.001,
		ReadOnly:   false, // deliberately false: the CREDENTIAL alone must suppress
	})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if len(resp.Activations) < 2 {
		t.Fatalf("expected both probe and partner in activations, got %d", len(resp.Activations))
	}

	hw.Stop()

	ws := store.ResolveVaultPrefix(vault)
	after, err := store.GetAssocWeight(ctx, ws, probeID, partnerID)
	if err != nil {
		t.Fatalf("GetAssocWeight: %v", err)
	}
	if after != initialWeight {
		t.Errorf("observe-mode credential (req.ReadOnly=false) changed the "+
			"probe/partner association weight from %v to %v", initialWeight, after)
	}
}

// TestActivateReadOnly_FullMode_SuppressesRecallEvent is the RED test for the
// second write site: a read_only:true recall from a full-mode credential must
// not persist a 0x29 recall event. Persistence is observable from the
// outside without a purpose-gated read: a persisted event replaces the
// fast, non-persisted "q-..." query ID with a real event ULID (see
// (*Engine).fastQueryID and the persist branch in activateCore).
func TestActivateReadOnly_FullMode_SuppressesRecallEvent(t *testing.T) {
	eng, as, store, _, cleanup := hebbianReadOnlyEnv(t)
	defer cleanup()

	const vault = "readonly-fullmode-recallevent"
	_, _, _ = seedReadOnlyPair(t, eng, as, store, vault)

	resp, err := eng.Activate(withMode(auth.ModeFull), &mbp.ActivateRequest{
		Vault:      vault,
		Context:    []string{"kiln firing schedule", "glaze mixing ratio"},
		MaxResults: 10,
		Threshold:  0.001,
		ReadOnly:   true,
	})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if len(resp.QueryID) < 2 || resp.QueryID[:2] != "q-" {
		t.Errorf("read_only:true recall persisted a recall event (query_id %q "+
			"is not the unpersisted fast-ID form) — a pure read must not write "+
			"the signal calibration reads", resp.QueryID)
	}
}

// TestActivate_NotReadOnly_StillPersistsRecallEvent is the positive control
// for the recall-event site.
func TestActivate_NotReadOnly_StillPersistsRecallEvent(t *testing.T) {
	eng, as, store, _, cleanup := hebbianReadOnlyEnv(t)
	defer cleanup()

	const vault = "readonly-fullmode-recallevent-control"
	_, _, _ = seedReadOnlyPair(t, eng, as, store, vault)

	resp, err := eng.Activate(withMode(auth.ModeFull), &mbp.ActivateRequest{
		Vault:      vault,
		Context:    []string{"kiln firing schedule", "glaze mixing ratio"},
		MaxResults: 10,
		Threshold:  0.001,
		ReadOnly:   false,
	})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if len(resp.QueryID) >= 2 && resp.QueryID[:2] == "q-" {
		t.Errorf("ordinary recall did not persist a recall event (query_id %q); "+
			"the fix must not disable recall-event persistence outright", resp.QueryID)
	}
}

// ---------------------------------------------------------------------------
// Census: activateCore must resolve auth.ObserveFromContext exactly ONCE. Every
// post-Run write site must consult the single resolved decision (actReq.ReadOnly)
// instead of re-deriving it — so a future fifth write site that calls
// auth.ObserveFromContext(ctx) directly (the exact #598 bug pattern) fails this
// test immediately, rather than silently shipping a write site that ignores an
// explicit req.ReadOnly. Enumerated from the CODE (go/ast over the actual
// source), not a hand-maintained list — the TestAppendMode_MethodCensus shape.
// ---------------------------------------------------------------------------
func TestActivateCore_ObserveFromContextResolvedExactlyOnce(t *testing.T) {
	path := filepath.Join(".", "engine.go")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	var fn *ast.FuncDecl
	for _, decl := range file.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Name.Name != "activateCore" {
			continue
		}
		fn = fd
		break
	}
	if fn == nil {
		t.Fatal("activateCore not found in engine.go — did it move or get renamed?")
	}

	count := 0
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkgIdent, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		if pkgIdent.Name == "auth" && sel.Sel.Name == "ObserveFromContext" {
			count++
		}
		return true
	})

	if count != 1 {
		t.Errorf("activateCore calls auth.ObserveFromContext(ctx) %d times, want exactly 1. "+
			"A new post-Run write site must gate on the single resolved decision "+
			"(actReq.ReadOnly, computed once before Run) rather than calling "+
			"auth.ObserveFromContext(ctx) directly — a second direct call is exactly "+
			"the #598 bug pattern: a write site that ignores an explicit req.ReadOnly.", count)
	}
}
