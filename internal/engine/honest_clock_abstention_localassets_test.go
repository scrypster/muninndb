//go:build localassets

package engine

import (
	"context"
	"fmt"
	"os"
	"strings"
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
// Honest clock — the abstention guard, with the REAL bundled bge-small and a
// REAL HNSW index, BOTH channels on.
//
// Taking age out of the ACT-R prior removes what used to suppress an old
// memory's absolute score in a cold process. The gate expression is
// unchanged, so the honest expectation is "abstention exactly as a WARM
// engine had it", where everything pooled already sat at the cap. This pins:
//
//   - nonsense and content-free queries return the same thing cold and warm,
//     in every recall mode;
//   - a row returned for nonsense is never banded moderate or strong;
//   - content-free queries abstain outright;
//   - INCLUDING nonsense that shares a word with a stored memory, so the
//     lexical channel produces real evidence (asserted, so the arm is not
//     vacuously empty).
//
// Fixtures are invented (trail maintenance and beekeeping).
// ---------------------------------------------------------------------------

type hcRealEnv struct {
	t     *testing.T
	dir   string
	svc   *embedpkg.EmbedService
	store *storage.PebbleStore
	fts   *fts.Index
	eng   *Engine
}

func (h *hcRealEnv) open() {
	h.t.Helper()
	db, err := storage.OpenPebble(h.dir, storage.DefaultOptions())
	if err != nil {
		h.t.Fatal(err)
	}
	h.store = storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 1000})
	h.fts = fts.New(db)
	embedder := embedpkg.NewEmbedServiceAdapter(h.svc)
	reg := hnswpkg.NewRegistry(db)
	act := activation.New(h.store, activation.NewFTSAdapter(h.fts), activation.NewHNSWAdapter(reg), embedder)
	trig := trigger.New(h.store, trigger.NewFTSAdapter(h.fts), trigger.NewHNSWAdapter(reg), embedder)
	h.eng = NewEngine(EngineConfig{
		Store: h.store, FTSIndex: h.fts, ActivationEngine: act, TriggerSystem: trig,
		Embedder: embedder, HNSWRegistry: reg, EmbedModelName: "bge-small-en-v1.5",
		ContradictWorker: (*cognitive.Worker[cognitive.ContradictItem])(nil),
	})
}

func (h *hcRealEnv) close() {
	if h.eng != nil {
		h.eng.Stop()
		h.store.Close()
		h.eng = nil
	}
}

func newHCRealEnv(t *testing.T) *hcRealEnv {
	t.Helper()
	dir, err := os.MkdirTemp("", "muninndb-honest-clock-real-*")
	if err != nil {
		t.Fatal(err)
	}
	svc, err := embedpkg.NewEmbedService("local://")
	if err != nil {
		t.Fatalf("NewEmbedService: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := svc.Init(ctx, plugin.PluginConfig{DataDir: dir}); err != nil {
		os.RemoveAll(dir)
		t.Skipf("local embed provider init failed (assets missing? run make fetch-assets): %v", err)
	}
	h := &hcRealEnv{t: t, dir: dir, svc: svc}
	h.open()
	t.Cleanup(func() { h.close(); _ = svc.Close(); os.RemoveAll(dir) })
	return h
}

// writeAged writes an embedded memory backdated by age (a backdated
// created_at mints a backdated ULID), the way the server's write path does.
func (h *hcRealEnv) writeAged(concept, content string, age time.Duration) string {
	h.t.Helper()
	vec, err := h.svc.Embed(context.Background(), []string{concept + " " + content})
	if err != nil {
		h.t.Fatalf("embed: %v", err)
	}
	created := time.Now().Add(-age)
	resp, err := h.eng.Write(context.Background(), &mbp.WriteRequest{
		Vault: "default", Concept: concept, Content: content, Embedding: vec, CreatedAt: &created,
	})
	if err != nil {
		h.t.Fatalf("write: %v", err)
	}
	return resp.ID
}

func (h *hcRealEnv) recall(query, mode string) *mbp.ActivateResponse {
	h.t.Helper()
	resp, err := h.eng.Activate(context.Background(), &mbp.ActivateRequest{
		Vault: "default", Context: []string{query}, MaxResults: 10, ReadOnly: true, Mode: mode,
	})
	if err != nil {
		h.t.Fatalf("activate(%q, %q): %v", query, mode, err)
	}
	return resp
}

func hcRender(resp *mbp.ActivateResponse) string {
	var b strings.Builder
	for _, a := range resp.Activations {
		fmt.Fprintf(&b, "%s abs=%.6f band=%s; ", a.ID, a.ScoreComponents.AbsoluteScore, a.RelevanceBand)
	}
	return b.String()
}

func TestHonestClock_NonsenseAbstainsWarmAndCold(t *testing.T) {
	h := newHCRealEnv(t)
	day := 24 * time.Hour
	ids := map[string]string{}
	ids["waterbar"] = h.writeAged("waterbar drainage", "the waterbar on switchback four drains to the left side of the tread", 400*day)
	ids["blowdown"] = h.writeAged("blowdown clearing", "the crew cleared blowdown on segment two with a crosscut saw", 200*day)
	ids["hive"] = h.writeAged("hive inspection", "the north hive was inspected and the queen cells were removed", 120*day)
	ids["smoker"] = h.writeAged("smoker fuel", "pine needles burn cool and slow in the bee smoker", 60*day)
	ids["bridge"] = h.writeAged("bridge decking", "the footbridge decking was replaced with treated planks", 30*day)
	ids["honey"] = h.writeAged("honey harvest", "the autumn harvest filled eleven frames of capped honey", 3*day)
	h.eng.waitWriteTimeIdle()

	nonsense := []string{
		"purple elephant tax returns and a quantum saxophone",
		"velvet thunder accounting spreadsheet karaoke",
	}
	// Nonsense sharing a content word with a stored memory: the lexical channel
	// has real evidence for these.
	sharedWord := map[string]string{
		"switchback saxophone velvet invoice": "waterbar",
		"hive karaoke mortgage spreadsheet":   "hive",
	}
	contentFree := []string{"the and of it", "what is this about"}
	modes := []string{"", "balanced", "recent", "semantic", "deep"}

	type key struct{ q, m string }
	all := append(append([]string{}, nonsense...), contentFree...)
	for q := range sharedWord {
		all = append(all, q)
	}
	run := func() map[key]*mbp.ActivateResponse {
		out := map[key]*mbp.ActivateResponse{}
		for _, q := range all {
			for _, m := range modes {
				out[key{q, m}] = h.recall(q, m)
			}
		}
		return out
	}

	// Cold: a fresh process over the written store.
	h.close()
	h.open()
	cold := run()

	// Non-vacuity of the lexical arm: the shared word reaches its memory
	// through full-text evidence.
	for q, which := range sharedWord {
		ex, err := h.eng.Explain(context.Background(), "default", ids[which], []string{q}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !ex.Scored || ex.Components.FullTextRelevance <= 0 {
			t.Fatalf("premise: %q must carry lexical evidence for %s (scored=%v fts=%.4f) — otherwise this arm is vacuous",
				q, which, ex.Scored, ex.Components.FullTextRelevance)
		}
	}

	// Warm: every memory loaded through the cache (a stamping load), then the
	// same calls.
	ws := h.store.ResolveVaultPrefix("default")
	for _, id := range ids {
		uid, _ := storage.ParseULID(id)
		if _, err := h.store.GetEngram(context.Background(), ws, uid); err != nil {
			t.Fatal(err)
		}
	}
	warm := run()

	for k, c := range cold {
		w := warm[k]
		if hcRender(c) != hcRender(w) {
			t.Errorf("%q mode %q: cold and warm differ\ncold %s\nwarm %s", k.q, k.m, hcRender(c), hcRender(w))
		}
		for _, a := range c.Activations {
			if a.RelevanceBand == activation.RelevanceModerate || a.RelevanceBand == activation.RelevanceStrong {
				t.Errorf("%q mode %q: nonsense returned %s banded %q (abs %.4f)", k.q, k.m, a.Concept, a.RelevanceBand, a.ScoreComponents.AbsoluteScore)
			}
		}
		t.Logf("%-45q mode %-9q rows=%d %s", k.q, k.m, len(c.Activations), hcRender(c))
	}
	for _, q := range contentFree {
		for _, m := range modes {
			if n := len(cold[key{q, m}].Activations); n != 0 {
				t.Errorf("content-free %q mode %q returned %d rows, want abstention: %s", q, m, n, hcRender(cold[key{q, m}]))
			}
		}
	}
}

// TestHonestClock_EntityBoostSeedsOnlyFromRealEvidence — measured on a
// production install: on made-up-word queries, entity-boost injection turned a
// handful of WEAK rows into many more `uncalibrated / not_scored` rows (memories
// whose only claim was sharing a named entity with a weak row). Once the
// honest clock stopped gating old weak seeds out cold, noise seeded noise.
//
// Rule: an entity-boost seed may INJECT only if its own relevance band is
// moderate or strong (the band is calibrated against the vault's noise floor —
// no new constant). Weak rows seed no injection; boosting rows already in the
// set is unchanged. A response whose rows are all weak injects nothing.
func TestHonestClock_EntityBoostSeedsOnlyFromRealEvidence(t *testing.T) {
	h := newHCRealEnv(t)
	day := 24 * time.Hour
	hive := h.writeAged("hive inspection", "the north hive was inspected and the queen cells were removed", 120*day)
	h.writeAged("smoker fuel", "pine needles burn cool and slow in the bee smoker", 60*day)
	h.writeAged("honey harvest", "the autumn harvest filled eleven frames of capped honey", 3*day)
	bridge := h.writeAged("bridge decking", "the footbridge decking was replaced with treated planks", 30*day)
	h.writeAged("waterbar drainage", "the waterbar on switchback four drains to the left side of the tread", 400*day)
	h.eng.waitWriteTimeIdle()

	// One rare named entity shared by the hive memory and the (topically
	// unrelated) bridge memory: the bridge row can only arrive by injection.
	ctx := context.Background()
	ws := h.store.ResolveVaultPrefix("default")
	if err := h.store.UpsertEntityRecord(ctx, ws, storage.EntityRecord{Name: "Ridgeview Volunteers", Type: "organization", Source: "inline"}, "inline"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{hive, bridge} {
		uid, _ := storage.ParseULID(id)
		if err := h.store.WriteEntityEngramLink(ctx, ws, uid, "Ridgeview Volunteers"); err != nil {
			t.Fatal(err)
		}
	}
	h.close()
	h.open()

	find := func(resp *mbp.ActivateResponse, id string) *mbp.ActivationItem {
		for i := range resp.Activations {
			if resp.Activations[i].ID == id {
				return &resp.Activations[i]
			}
		}
		return nil
	}

	// Weak-only: a made-up query that shares one word with the hive memory.
	// include_weak: weak_only abstention would otherwise withhold this
	// response outright; the point here is what the boost injects into it.
	weak := h.activate(&mbp.ActivateRequest{Context: []string{"hive karaoke mortgage spreadsheet"}, IncludeWeak: true})
	t.Logf("weak query: %s", hcRender(weak))
	if hv := find(weak, hive); hv == nil || hv.RelevanceBand != activation.RelevanceWeak {
		t.Fatalf("premise: the hive memory must be returned as a WEAK row for the made-up query: %s", hcRender(weak))
	}
	for _, a := range weak.Activations {
		if a.RelevanceBandBasis == activation.BasisNotScored {
			t.Errorf("a weak-only response injected %s (%s/%s) on entity evidence alone", a.Concept, a.RelevanceBand, a.RelevanceBandBasis)
		}
	}

	// Strong seed: the exact topic. The entity-linked bridge memory is still
	// injected, with its component trace intact.
	strong := h.recall("north hive inspected queen cells removed", "")
	t.Logf("strong query: %s", hcRender(strong))
	if hv := find(strong, hive); hv == nil || (hv.RelevanceBand != activation.RelevanceStrong && hv.RelevanceBand != activation.RelevanceModerate) {
		t.Fatalf("premise: the hive memory must be a moderate/strong row for its own topic: %s", hcRender(strong))
	}
	br := find(strong, bridge)
	if br == nil {
		t.Fatalf("a strong seed must still inject its entity-linked memory: %s", hcRender(strong))
	}
	if br.RelevanceBandBasis != activation.BasisNotScored || br.ScoreComponents.EntityBoost <= 0 ||
		br.ScoreComponents.Final != br.Score {
		t.Errorf("injected row lost its trace: basis %q entity_boost %.4f final %.4f score %.4f",
			br.RelevanceBandBasis, br.ScoreComponents.EntityBoost, br.ScoreComponents.Final, br.Score)
	}
}

// weakOnlyFixture: a small calibrated vault (real bge-small, registered noise
// floor). "hive karaoke mortgage spreadsheet" reaches two memories only as
// WEAK rows (measured above); the hive memory's own topic is strong.
func weakOnlyFixture(t *testing.T) (*hcRealEnv, map[string]string) {
	h := newHCRealEnv(t)
	day := 24 * time.Hour
	ids := map[string]string{
		"hive":     h.writeAged("hive inspection", "the north hive was inspected and the queen cells were removed", 120*day),
		"smoker":   h.writeAged("smoker fuel", "pine needles burn cool and slow in the bee smoker", 60*day),
		"honey":    h.writeAged("honey harvest", "the autumn harvest filled eleven frames of capped honey", 3*day),
		"bridge":   h.writeAged("bridge decking", "the footbridge decking was replaced with treated planks", 30*day),
		"waterbar": h.writeAged("waterbar drainage", "the waterbar on switchback four drains to the left side of the tread", 400*day),
	}
	h.eng.waitWriteTimeIdle()
	h.close()
	h.open()
	return h, ids
}

const weakOnlyQuery = "hive karaoke mortgage spreadsheet"

func (h *hcRealEnv) activate(req *mbp.ActivateRequest) *mbp.ActivateResponse {
	h.t.Helper()
	req.Vault = "default"
	req.ReadOnly = true
	if req.MaxResults == 0 {
		req.MaxResults = 10
	}
	resp, err := h.eng.Activate(context.Background(), req)
	if err != nil {
		h.t.Fatal(err)
	}
	return resp
}

// TestWeakOnly_CalibratedWeakOnlyResponseAbstains — RED before: the two weak
// rows were returned. Now: withheld, loud, counted, and recoverable.
func TestWeakOnly_CalibratedWeakOnlyResponseAbstains(t *testing.T) {
	h, _ := weakOnlyFixture(t)
	opted := h.activate(&mbp.ActivateRequest{Context: []string{weakOnlyQuery}, IncludeWeak: true})
	if len(opted.Activations) == 0 {
		t.Fatalf("premise: the query must reach weak rows")
	}
	for _, a := range opted.Activations {
		if a.RelevanceBand != activation.RelevanceWeak {
			t.Fatalf("premise: every row must be weak, got %s=%s", a.Concept, a.RelevanceBand)
		}
	}
	resp := h.activate(&mbp.ActivateRequest{Context: []string{weakOnlyQuery}})
	if len(resp.Activations) != 0 || !resp.Abstained || resp.AbstainedReason != activation.AbstainWeakOnly {
		t.Fatalf("weak-only calibrated response: rows=%d abstained=%v reason=%q, want 0 rows, abstained, %q (%s)",
			len(resp.Activations), resp.Abstained, resp.AbstainedReason, activation.AbstainWeakOnly, hcRender(resp))
	}
	if resp.WeakAvailable != len(opted.Activations) {
		t.Errorf("weak_available=%d, want %d (the rows include_weak returns)", resp.WeakAvailable, len(opted.Activations))
	}
	// Explicit threshold, explicit filter, and deep keep returning the rows.
	for name, req := range map[string]*mbp.ActivateRequest{
		"explicit threshold": {Context: []string{weakOnlyQuery}, Threshold: 0.1},
		"mode deep":          {Context: []string{weakOnlyQuery}, Mode: "deep"},
		"time filter":        {Context: []string{weakOnlyQuery}, Filters: []mbp.Filter{{Field: "created_after", Op: "gte", Value: time.Now().Add(-1000 * 24 * time.Hour)}}},
	} {
		if r := h.activate(req); len(r.Activations) == 0 || r.AbstainedReason == activation.AbstainWeakOnly {
			t.Errorf("%s: weak rows must still be returned (rows=%d reason=%q)", name, len(r.Activations), r.AbstainedReason)
		}
	}
}

// TestWeakOnly_MixedResponseIsUntouched — all-or-nothing: one non-weak row
// returns every row, weak neighbours included, exactly as with include_weak.
func TestWeakOnly_MixedResponseIsUntouched(t *testing.T) {
	h, _ := weakOnlyFixture(t)
	q := []string{"inspecting the hive and its queen cells"}
	def := h.activate(&mbp.ActivateRequest{Context: q})
	opt := h.activate(&mbp.ActivateRequest{Context: q, IncludeWeak: true})
	weak, real := 0, 0
	for _, a := range def.Activations {
		if a.RelevanceBand == activation.RelevanceWeak {
			weak++
		} else {
			real++
		}
	}
	if weak == 0 || real == 0 {
		t.Fatalf("premise: need a mixed response (weak=%d non-weak=%d): %s", weak, real, hcRender(def))
	}
	if hcRender(def) != hcRender(opt) || def.Abstained {
		t.Fatalf("a mixed response must be returned whole\ndefault      %s\ninclude_weak %s", hcRender(def), hcRender(opt))
	}
}

// TestWeakOnly_ExplainAgrees — explain's would_return says what recall does.
func TestWeakOnly_ExplainAgrees(t *testing.T) {
	h, ids := weakOnlyFixture(t)
	ex, err := h.eng.Explain(context.Background(), "default", ids["hive"], []string{weakOnlyQuery}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ex.WouldReturn || ex.WithheldReason != activation.AbstainWeakOnly || !strings.Contains(ex.Note, "include_weak") {
		t.Fatalf("explain of a weak-only memory: would_return=%v withheld=%q note=%q; recall withholds it", ex.WouldReturn, ex.WithheldReason, ex.Note)
	}
	ex2, err := h.eng.Explain(context.Background(), "default", ids["hive"], []string{"north hive inspected queen cells removed"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !ex2.WouldReturn || ex2.WithheldReason != "" {
		t.Fatalf("explain of a strong match must still say would_return: %+v", ex2)
	}
}
