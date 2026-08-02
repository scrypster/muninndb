//go:build localassets

package engine

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/cognitive"
	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/engine/trigger"
	"github.com/scrypster/muninndb/internal/index/fts"
	hnswpkg "github.com/scrypster/muninndb/internal/index/hnsw"
	"github.com/scrypster/muninndb/internal/plugin"
	embedpkg "github.com/scrypster/muninndb/internal/plugin/embed"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// ---------------------------------------------------------------------------
// GATE 1 — issue #763's own reproduction, with the REAL bundled
// bge-small-en-v1.5 and a REAL HNSW index.
//
// The failure is a WORDING failure: the evolve rewrote the vocabulary
// ("last-writer-wins" -> "field-level merging"), so a natural question about
// the current rule reaches the predecessor's vector and not the successor's. A
// fake or noop embedder cannot reproduce that — it has no vocabulary — which is
// why this gate is asset-gated while the chain-shape and visibility gates are
// not.
//
// Both fixtures below are synthetic (invented product wording), and nothing is
// read from a real vault.
// ---------------------------------------------------------------------------

// realEmbedEnv builds an Engine over a real Pebble store with the bundled ONNX
// embedder and a real HNSW registry — the production wiring, minus the server.
func realEmbedEnv(t *testing.T) (*Engine, func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "muninndb-versionhead-*")
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

	svc, err := embedpkg.NewEmbedService("local://")
	if err != nil {
		t.Fatalf("NewEmbedService: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := svc.Init(ctx, plugin.PluginConfig{DataDir: dir}); err != nil {
		store.Close()
		os.RemoveAll(dir)
		t.Skipf("local embed provider init failed (assets missing? run make fetch-assets): %v", err)
	}
	embedder := embedpkg.NewEmbedServiceAdapter(svc)

	hnswReg := hnswpkg.NewRegistry(db)
	actEngine := activation.New(store, activation.NewFTSAdapter(ftsIdx), activation.NewHNSWAdapter(hnswReg), embedder)
	trigSystem := trigger.New(store, trigger.NewFTSAdapter(ftsIdx), trigger.NewHNSWAdapter(hnswReg), embedder)
	eng := NewEngine(EngineConfig{
		Store: store, FTSIndex: ftsIdx, ActivationEngine: actEngine, TriggerSystem: trigSystem,
		Embedder: embedder, HNSWRegistry: hnswReg,
		EmbedModelName:   "bge-small-en-v1.5",
		HebbianWorker:    nil,
		ContradictWorker: (*cognitive.Worker[cognitive.ContradictItem])(nil),
	})
	return eng, func() {
		eng.Stop()
		store.Close()
		_ = svc.Close()
		os.RemoveAll(dir)
	}
}

type realEmbedHarness struct {
	t   *testing.T
	eng *Engine
	ctx context.Context
	ws  [8]byte
	svc *embedpkg.EmbedService
}

// writeEmbedded writes a memory and indexes its embedding exactly as the
// server's write path does (Concept + " " + Content, the retroactive
// processor's text).
func (h *realEmbedHarness) writeEmbedded(concept, content string) string {
	h.t.Helper()
	vec, err := h.svc.Embed(h.ctx, []string{concept + " " + content})
	if err != nil {
		h.t.Fatalf("embed: %v", err)
	}
	resp, err := h.eng.Write(h.ctx, &mbp.WriteRequest{
		Vault: "default", Concept: concept, Content: content, Embedding: vec,
	})
	if err != nil {
		h.t.Fatalf("write: %v", err)
	}
	return resp.ID
}

func (h *realEmbedHarness) evolveEmbedded(oldID, concept, content string) string {
	h.t.Helper()
	vec, err := h.svc.Embed(h.ctx, []string{concept + " " + content})
	if err != nil {
		h.t.Fatalf("embed: %v", err)
	}
	newID, err := h.eng.Evolve(h.ctx, "default", oldID, content, "clarified the merge rule", vec, concept)
	if err != nil {
		h.t.Fatalf("evolve: %v", err)
	}
	return newID.String()
}

func newRealEmbedHarness(t *testing.T) (*realEmbedHarness, func()) {
	t.Helper()
	dir := t.TempDir()
	svc, err := embedpkg.NewEmbedService("local://")
	if err != nil {
		t.Fatalf("NewEmbedService: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := svc.Init(ctx, plugin.PluginConfig{DataDir: dir}); err != nil {
		t.Skipf("local embed provider init failed (assets missing? run make fetch-assets): %v", err)
	}
	t.Cleanup(func() { _ = svc.Close() })

	eng, cleanup := realEmbedEnv(t)
	return &realEmbedHarness{
		t: t, eng: eng, ctx: context.Background(),
		ws: eng.store.ResolveVaultPrefix("default"), svc: svc,
	}, cleanup
}

// mergeQuery is phrased in the PREDECESSOR's vocabulary — "save the same
// record", "whose version wins" — which is what an agent asks in the days after
// a rewrite it did not author. The design doc's literal probe ("What is the
// current rule for MERGING simultaneous edits?") was measured here first and
// deliberately NOT used: it shares the word "merging" with the successor, so
// the successor comes back on its own merit even at 33f1230 and the test would
// pass with the mechanism deleted. That is recorded rather than papered over —
// a fixture that cannot reach the failure proves nothing (CLAUDE.md §3.3).
const mergeQuery = "When two people save the same record at once, whose version wins?"

const (
	mergePredecessorText = "Simultaneous edits are resolved last-writer-wins; the most recent save overwrites the whole record."
	// The rewrite changes the VOCABULARY, which is the whole shape of #763:
	// nothing in the successor's wording answers a question posed in the
	// predecessor's terms.
	mergeSuccessorText = "Concurrent revisions are reconciled by per-attribute reconciliation across the document tree."
)

// TestVersionHead_RealEmbedder_PredecessorWordingReturnsHead is call 18 from the
// round-6 evaluation, reproduced end to end: remember, evolve with a rewritten
// vocabulary, then ask the natural question at the vault's real default
// threshold, default mode, no rephrase and no deep mode.
//
// RED at 33f1230: the predecessor is soft-deleted so phase 6 discards it, and
// the successor's own wording does not reach the query — the truth is stored,
// current, and invisible.
func TestVersionHead_RealEmbedder_PredecessorWordingReturnsHead(t *testing.T) {
	h, cleanup := newRealEmbedHarness(t)
	defer cleanup()

	predecessor := h.writeEmbedded("conflict resolution policy", mergePredecessorText)
	successor := h.evolveEmbedded(predecessor, "conflict resolution policy", mergeSuccessorText)
	// An adjacent memory: the evaluation's failing call returned only this and
	// omitted the evolved decision entirely.
	h.writeEmbedded("editor presence indicator",
		"The editor shows a presence indicator when two people open the same document.")

	h.eng.waitWriteTimeIdle()
	resp, err := h.eng.Activate(h.ctx, &mbp.ActivateRequest{
		Vault: "default", Context: []string{mergeQuery}, MaxResults: 10,
	})
	if err != nil {
		t.Fatalf("activate: %v", err)
	}

	if itemByID(resp, predecessor) != nil {
		t.Errorf("the superseded predecessor was returned: %v", ids(resp))
	}
	head := itemByID(resp, successor)
	if head == nil {
		t.Fatalf("#763 NOT FIXED (call 18): %q returned %v and omitted the evolved decision %s entirely (abstained=%v/%q)",
			mergeQuery, ids(resp), successor, resp.Abstained, resp.AbstainedReason)
	}
	if head.SubstitutedFor != predecessor {
		t.Errorf("substituted_for = %q, want the predecessor %q", head.SubstitutedFor, predecessor)
	}
	if head.SubstitutionBasis == nil || head.SubstitutionBasis.AbsoluteScore < 0.1 {
		t.Errorf("substitution_basis = %+v, want an absolute_score at or above the 0.10 engine default — "+
			"substitution may only redirect evidence that cleared the caller's bar", head.SubstitutionBasis)
	}
	t.Logf("call 18 resolved: head=%s score=%.4f basis.absolute=%.4f basis.semantic=%.4f",
		successor, head.Score, head.SubstitutionBasis.AbsoluteScore, head.SubstitutionBasis.SemanticSimilarity)
}

// GATE 8b — the embed-lag window. Between the evolve commit and the successor's
// embedding landing, the successor is in FTS but NOT in HNSW while the
// predecessor is in both and excluded by the lifecycle cut, so a
// semantically-phrased query can reach NEITHER version. Substitution closes
// that window for free: the predecessor's vector is still in HNSW, so the
// evidence survives even though the successor's own vector does not exist yet.
// The response says so with head_not_indexed_yet.
func TestVersionHead_RealEmbedder_SubstitutesWithUnindexedHead(t *testing.T) {
	h, cleanup := newRealEmbedHarness(t)
	defer cleanup()

	predecessor := h.writeEmbedded("conflict resolution policy", mergePredecessorText)
	// Evolve with NO caller embedding and no retroactive processor running:
	// the successor has no vector at all, exactly the fresh-evolve window.
	successorID, err := h.eng.Evolve(h.ctx, "default", predecessor,
		mergeSuccessorText, "clarified", nil, "conflict resolution policy")
	if err != nil {
		t.Fatalf("evolve: %v", err)
	}
	successor := successorID.String()

	h.eng.waitWriteTimeIdle()
	resp, err := h.eng.Activate(h.ctx, &mbp.ActivateRequest{
		Vault: "default", Context: []string{mergeQuery}, MaxResults: 10,
	})
	if err != nil {
		t.Fatalf("activate: %v", err)
	}
	head := itemByID(resp, successor)
	if head == nil {
		t.Fatalf("the unembedded successor was not returned: %v (abstained=%v/%q) — substitution is what closes the "+
			"fresh-evolve window, and it did not", ids(resp), resp.Abstained, resp.AbstainedReason)
	}
	if head.SubstitutedFor != predecessor {
		t.Errorf("substituted_for = %q, want %q", head.SubstitutedFor, predecessor)
	}
	if !head.HeadNotIndexedYet {
		t.Errorf("head_not_indexed_yet is false on a successor with no stored embedding — " +
			"'not indexed yet' and 'not relevant' must be distinguishable at the surface (design §5.6)")
	}
}
