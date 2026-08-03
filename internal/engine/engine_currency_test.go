package engine

import (
	"context"
	"fmt"
	"math"
	"sort"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// currencyTestHarness models the real sample-widgetflow case measured in
// #712/#716: a chain of versioned facts about the same topic, ORGANIZED BY
// TAGS (not entities — the real vault is entity-sparse) and carrying no
// explicit RelSupersedes link (that is exactly what makes the vault's
// existing supersedes machinery inert for them), plus legitimately-coexisting
// facets that must NOT be swept into the same cluster.
type currencyTestHarness struct {
	t   *testing.T
	eng *Engine
	ctx context.Context
	ws  [8]byte
}

func newCurrencyHarness(t *testing.T) (*currencyTestHarness, func()) {
	eng, cleanup := testEnv(t)
	return &currencyTestHarness{
		t:   t,
		eng: eng,
		ctx: context.Background(),
		ws:  eng.store.ResolveVaultPrefix("default"),
	}, cleanup
}

// writeOpts controls the knobs the currency heuristic reads.
type writeOpts struct {
	concept   string
	content   string
	entities  []string  // inline entity names attached to this engram
	tags      []string  // tags attached to this engram (the R2 discriminator)
	embedding []float32 // explicit embedding; nil = no vector (forces Jaccard fallback)
	validFrom time.Time
}

func (h *currencyTestHarness) write(o writeOpts) string {
	h.t.Helper()
	var ents []mbp.InlineEntity
	for _, name := range o.entities {
		ents = append(ents, mbp.InlineEntity{Name: name, Type: "topic"})
	}
	req := &mbp.WriteRequest{
		Vault:    "default",
		Concept:  o.concept,
		Content:  o.content,
		Entities: ents,
		Tags:     o.tags,
		// Embedding is deliberately NOT set on the write request: this test
		// engine's EngineConfig carries no HNSWRegistry (nil), and
		// Engine.Write's inline-embedding path calls e.hnswRegistry.Insert
		// unconditionally when req.Embedding is non-empty — a nil-registry
		// panic unrelated to anything this test is exercising. Explicit
		// control vectors are instead poked directly via
		// PebbleStore.UpdateEmbedding below (the 0x18 key only, no HNSW
		// side effects), which is exactly what currencySimilarity reads.
	}
	if !o.validFrom.IsZero() {
		vf := o.validFrom
		req.ValidFrom = &vf
		// CreatedAt (transaction time) must not be set into the future
		// (engine.validateCreatedAt); a future ValidFrom (application time)
		// is exactly the "planned/aspirational fact written today" case this
		// harness needs to model, so only backdate CreatedAt to match
		// ValidFrom when ValidFrom itself is not in the future.
		if !vf.After(time.Now()) {
			req.CreatedAt = &vf
		}
	}
	resp, err := h.eng.Write(h.ctx, req)
	if err != nil {
		h.t.Fatalf("write %q: %v", o.concept, err)
	}
	if len(o.embedding) > 0 {
		id := mustParseULIDCur(h.t, resp.ID)
		if err := h.eng.store.UpdateEmbedding(h.ctx, h.ws, id, o.embedding); err != nil {
			h.t.Fatalf("UpdateEmbedding %s: %v", resp.ID, err)
		}
	}
	return resp.ID
}

func (h *currencyTestHarness) supersede(newID, oldID string) {
	h.t.Helper()
	if _, err := h.eng.Link(h.ctx, &mbp.LinkRequest{
		Vault: "default", SourceID: newID, TargetID: oldID,
		RelType: uint16(storage.RelSupersedes), Weight: 1.0,
	}); err != nil {
		h.t.Fatalf("Link supersedes %s->%s: %v", newID, oldID, err)
	}
}

// scored builds a result set from (id, score) pairs, re-fetching the engram
// from the store so real ValidFrom/Content/Concept/Tags/embeddings are
// present — exactly the shape applyCurrencyAnnotation receives from the
// recall pipeline.
func (h *currencyTestHarness) scored(pairs ...any) []activation.ScoredEngram {
	h.t.Helper()
	if len(pairs)%2 != 0 {
		h.t.Fatalf("scored: need id,score pairs")
	}
	out := make([]activation.ScoredEngram, 0, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		id := pairs[i].(string)
		score := pairs[i+1].(float64)
		u, err := storage.ParseULID(id)
		if err != nil {
			h.t.Fatalf("parse %s: %v", id, err)
		}
		eng, err := h.eng.store.GetEngram(h.ctx, h.ws, u)
		if err != nil || eng == nil {
			h.t.Fatalf("GetEngram %s: %v", id, err)
		}
		out = append(out, activation.ScoredEngram{Engram: eng, Score: score})
	}
	return out
}

func (h *currencyTestHarness) vaultCount() int64 {
	h.t.Helper()
	return h.eng.store.GetVaultCount(h.ctx, h.ws)
}

func (h *currencyTestHarness) apply(results []activation.ScoredEngram) []activation.ScoredEngram {
	return h.eng.applyCurrencyAnnotation(h.ctx, h.ws, results, h.vaultCount(), 0)
}

// pad writes n throwaway engrams sharing none of the fixture's topic tags,
// so that vault-local document-frequency ratios (currencyUbiquityRatio) are
// measured against a REALISTIC vault size instead of a handful of rows —
// mirroring the real vault's tag+df structure (a topic tag at ~3% of the vault,
// an ambient tag at ~27%), not a toy vault where any 2-engram tag looks
// "ubiquitous".
func (h *currencyTestHarness) pad(n int, tag string) {
	h.t.Helper()
	for i := 0; i < n; i++ {
		h.write(writeOpts{
			concept:   "padding filler",
			content:   fmt.Sprintf("unrelated filler content entry number %d about nothing in particular", i),
			tags:      []string{tag},
			validFrom: time.Now(),
		})
	}
}

func rankOfCur(results []activation.ScoredEngram, id string) int {
	for i, r := range results {
		if r.Engram.ID.String() == id {
			return i
		}
	}
	return -1
}

func findCur(results []activation.ScoredEngram, id string) *activation.ScoredEngram {
	for i := range results {
		if results[i].Engram.ID.String() == id {
			return &results[i]
		}
	}
	return nil
}

// clusterOf applies the currency phase to a 2-fact scored set and returns each
// fact's VersionCluster key ("" if unclustered) — the shape the gate-pin
// negative controls assert on.
func clusterOf(h *currencyTestHarness, a, b string) (string, string) {
	out := h.apply(h.scored(a, 0.9, b, 0.9))
	ra, rb := findCur(out, a), findCur(out, b)
	var ca, cb string
	if ra != nil {
		ca = ra.VersionCluster
	}
	if rb != nil {
		cb = rb.VersionCluster
	}
	return ca, cb
}

func mustParseULIDCur(t *testing.T, s string) storage.ULID {
	t.Helper()
	u, err := storage.ParseULID(s)
	if err != nil {
		t.Fatalf("parse ULID %s: %v", s, err)
	}
	return u
}

// --- Synthetic vault: the REAL sample widgetflow shape (#716) -------------
//
// C1-C4: a genuine version chain about "widgetflow layout" — shared
// non-ubiquitous topic tags (widgetflow, structure), DIFFERING version-marker
// tags (four-zone/future -> v3/live -> v3/live/final), temporally
// separated, and — critically — ZERO entities (matching the measured real
// vault: the widgetflow facts carry no extracted entities at all).
//
// telem1/telem2: a genuine SECOND chain (telemetry-budget revisions) —
// included to prove the mechanism finds more than one narrative chain, and
// telem1 also plays the "cos 0.86 false pair" canary against the widgetflow
// chain: same topic tags, differing markers, HIGH cosine to the chain (an
// explicit embedding pins this, matching the measured real pair) — and must
// be excluded from the widgetflow chain ONLY by the facet-conflict gate
// (telemetry tag df >= 3), not by similarity or markers, which both
// admit it.
//
// sandbox: a genuinely different, coexisting widgetflow model (a real facet —
// sandbox df >= 3) that carries NO version markers at all — double-blocked
// (no markers AND facet conflict), matching the design's measured V2 canary.
//
// unrelated: shares no fixture tags at all — the silence control.
// Synthetic domain (no real-vault content). The fixture reproduces the tag
// STRUCTURE measured on a real vault — shared non-ubiquitous topic tags,
// competing version markers, and facet nouns at df>=3 — using a fully invented
// product ("widgetflow") so nothing here mirrors real memory content.
const (
	currencyChainTopicTag     = "widgetflow"
	currencyChainStructureTag = "layout"
	currencyTelemetryFacetTag = "telemetry"
	currencySandboxFacetTag   = "sandbox"
)

// embedAt returns a unit 2D embedding at the given angle in degrees, so
// pairwise cosine similarity between two fixture engrams is fully
// controllable (cosine(a,b) = cos(angleA - angleB)) without depending on the
// test engine's noopEmbedder (which never produces a real vector).
func embedAt(degrees float64) []float32 {
	rad := degrees * (math.Pi / 180.0)
	return []float32{float32(math.Cos(rad)), float32(math.Sin(rad))}
}

type currencyChainIDs struct {
	c1, c2, c3, c4 string // widgetflow version chain (no entities)
	telem1, telem2 string // genuine 2nd chain: telemetry-budget revisions
	sandbox        string // coexisting facet, no markers, must not cluster
	unrelated      string // silence control
}

func seedTagAnchoredVault(h *currencyTestHarness) currencyChainIDs {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	// Pad the vault so tag document-frequency ratios are measured against a
	// realistic N (currencyUbiquityRatio is a FIXED 10% production constant —
	// a toy vault of 10 engrams would make even a 2-occurrence tag "10%",
	// i.e. spuriously ubiquitous).
	h.pad(110, "filler")

	var ids currencyChainIDs

	ids.c1 = h.write(writeOpts{
		concept:   "current widgetflow layout",
		content:   "The widgetflow layout moves to a four zone model with a future rollout planned for the standard profile.",
		tags:      []string{currencyChainTopicTag, currencyChainStructureTag, "four-zone", "future"},
		embedding: embedAt(0),
		validFrom: base,
	})
	ids.c2 = h.write(writeOpts{
		concept:   "widgetflow zone rates",
		content:   "Widgetflow zone rates for the four zone model are now finalized for the standard profile rollout.",
		tags:      []string{currencyChainTopicTag, currencyChainStructureTag, "four-zone", "rates"},
		embedding: embedAt(6),
		validFrom: base.Add(30 * 24 * time.Hour),
	})
	ids.c3 = h.write(writeOpts{
		concept:   "widgetflow layout v3 regional rates",
		content:   "Widgetflow layout v3 upper middle rates are now live for the standard profile.",
		tags:      []string{currencyChainTopicTag, currencyChainStructureTag, "v3", "live"},
		embedding: embedAt(10),
		validFrom: base.Add(60 * 24 * time.Hour),
	})
	ids.c4 = h.write(writeOpts{
		concept:   "final widgetflow layout live",
		content:   "The final widgetflow layout v3 is now live for the standard profile.",
		tags:      []string{currencyChainTopicTag, currencyChainStructureTag, "v3", "live", "final"},
		embedding: embedAt(14),
		validFrom: base.Add(90 * 24 * time.Hour),
	})

	// Genuine second chain: telemetry-budget revisions. telem1 is ALSO the
	// false-pair canary against the widgetflow chain (cosine ~0.86 there,
	// matching the measured real coexisting-facet pair) — high similarity to
	// the chain, tags shared, markers differ, but the "telemetry" facet tag
	// (df>=3 once telem1+telem2+the filler below are counted) must block it.
	ids.telem1 = h.write(writeOpts{
		concept:   "telemetry budget tiered v2",
		content:   "The telemetry budget moves to a tiered v2 model for the standard profile rollout.",
		tags:      []string{currencyChainTopicTag, currencyChainStructureTag, currencyTelemetryFacetTag, "v2"},
		embedding: embedAt(30), // cos(30) ~= 0.87 to c1 at angle 0 — the ~0.86 canary
		validFrom: base.Add(45 * 24 * time.Hour),
	})
	ids.telem2 = h.write(writeOpts{
		concept:   "telemetry budget revision",
		content:   "The telemetry budget has been revised down from the prior tiered layout for the standard profile.",
		tags:      []string{currencyChainTopicTag, currencyChainStructureTag, currencyTelemetryFacetTag, "revision"},
		embedding: embedAt(32),
		validFrom: base.Add(75 * 24 * time.Hour),
	})
	// A third telemetry-tagged filler so the facet tag's vault-wide df reaches
	// currencyFacetDF (3) without itself joining any narrative — mirrors why a
	// genuine facet noun (df>=3) reliably clears the facet floor on a real vault
	// while incidental descriptors (df 1-2) don't.
	h.write(writeOpts{
		concept:   "telemetry pipeline notes",
		content:   "Telemetry pipeline notes for the new rollout cycle.",
		tags:      []string{currencyTelemetryFacetTag, "kickoff"},
		embedding: embedAt(33),
		validFrom: base.Add(20 * 24 * time.Hour),
	})

	// Sandbox: a genuinely coexisting facet (facet noun, df >= 3), shares the
	// topic tags, is temporally separated, and is even HIGH cosine to the chain
	// (so, like telem1, only tags can exclude it) — but carries NO version-marker
	// tags at all, so it is double-blocked (no markers AND facet conflict),
	// matching the design's measured no-marker canary.
	ids.sandbox = h.write(writeOpts{
		concept:   "sandbox quota layout",
		content:   "The sandbox quota layout for external testers is described here for the standard profile.",
		tags:      []string{currencyChainTopicTag, currencyChainStructureTag, currencySandboxFacetTag, "quota"},
		embedding: embedAt(20),
		validFrom: base.Add(50 * 24 * time.Hour),
	})
	for i := 0; i < 2; i++ {
		h.write(writeOpts{
			concept:   "sandbox program note",
			content:   fmt.Sprintf("Sandbox tester note number %d.", i),
			tags:      []string{currencySandboxFacetTag, "tester"},
			embedding: embedAt(21),
			validFrom: base.Add(time.Duration(55+i) * 24 * time.Hour),
		})
	}

	ids.unrelated = h.write(writeOpts{
		concept:   "office holiday schedule",
		content:   "The office is closed on the last Friday of every month for a team offsite.",
		tags:      []string{"office", "schedule"},
		embedding: embedAt(170), // opposite side of the circle: unrelated on every axis
		validFrom: base.Add(40 * 24 * time.Hour),
	})

	return ids
}

// --- RED: the facet-conflict gate is load-bearing --------------------------
//
// TestCurrencyAnnotation_FacetConflict_IsLoadBearing is the RED/GREEN pair
// the increment plan calls for: with the facet-conflict gate REMOVED,
// telem1 (cosine ~0.87 to the widgetflow chain, shares topic tags, differing
// markers) wrongly clusters with the chain — proving cosine + tags + markers
// alone cannot exclude a real coexisting facet, and the facet-df gate is the
// one doing the work.
func TestCurrencyAnnotation_FacetConflict_IsLoadBearing(t *testing.T) {
	h, cleanup := newCurrencyHarness(t)
	defer cleanup()

	ids := seedTagAnchoredVault(h)

	c1Eng, err := h.eng.store.GetEngram(h.ctx, h.ws, mustParseULIDCur(t, ids.c1))
	if err != nil {
		t.Fatal(err)
	}
	telemEng, err := h.eng.store.GetEngram(h.ctx, h.ws, mustParseULIDCur(t, ids.telem1))
	if err != nil {
		t.Fatal(err)
	}
	n := h.vaultCount()
	dfCache := map[string]int64{}

	// Precondition: without the facet gate, every OTHER gate admits this
	// pair (this is what makes it a genuine canary, not a trivial exclusion).
	if !h.eng.currencyTemporallySeparated(c1Eng, telemEng) {
		t.Fatalf("expected c1/telem1 to be temporally separated")
	}
	if !h.eng.currencyPassesSimilarityGate(h.ctx, h.ws, c1Eng, telemEng) {
		t.Fatalf("expected the similarity FLOOR to admit c1/telem1 (cos ~0.87) — floor is loose by design")
	}
	if !h.eng.currencySharedAnchor(h.ctx, h.ws, c1Eng, telemEng, n, currencyUbiquityCutpoint(h.vaultCount()), dfCache) {
		t.Fatalf("expected c1/telem1 to clear the tag anchor (shared topic tags + differing markers)")
	}
	// RED proof: the facet-conflict check itself reports a conflict for this
	// pair (its absence — i.e. skipping this call in applyCurrencyAnnotation
	// — is exactly what would let the false pair through).
	if !h.eng.currencyFacetConflict(h.ctx, h.ws, currencyFilterTypeLabels(c1Eng.Tags), currencyFilterTypeLabels(telemEng.Tags), dfCache) {
		t.Fatalf("expected the FACET-CONFLICT gate to flag c1/telem1 (telemetry df>=3, exclusive to telem1) — this is the gate that must exclude the cosine-admitted false pair")
	}

	// GREEN: the full pipeline, with the facet gate wired in, does not
	// cluster them.
	results := h.scored(ids.c1, 0.9, ids.telem1, 0.9)
	out := h.apply(results)
	r1, rImpl := findCur(out, ids.c1), findCur(out, ids.telem1)
	if r1.VersionCluster != "" && r1.VersionCluster == rImpl.VersionCluster {
		t.Fatalf("FALSE POSITIVE: c1 and telem1 (facet conflict) were clustered together")
	}
}

// --- GREEN: full acceptance suite ------------------------------------------

// TestCurrencyAnnotation_TagAnchoredChain_ClustersAndExcludesFacets is the
// core positive case on real-shaped data: c1-c4 (no entities, tag+marker
// organized) cluster; c4 (newest) is crowned; c1-c3 carry
// possibly_superseded_by -> c4; scores are unchanged; the sandbox facet and
// telem1/telem2 (a genuinely separate chain) stay OUT of the widgetflow
// cluster; the unrelated control carries nothing.
func TestCurrencyAnnotation_TagAnchoredChain_ClustersAndExcludesFacets(t *testing.T) {
	h, cleanup := newCurrencyHarness(t)
	defer cleanup()

	ids := seedTagAnchoredVault(h)

	results := h.scored(
		ids.c1, 0.911,
		ids.c2, 0.85,
		ids.c3, 0.87,
		ids.c4, 0.911,
		ids.telem1, 0.95,
		ids.telem2, 0.93,
		ids.sandbox, 0.90,
		ids.unrelated, 0.5,
	)
	before := make(map[string]float64, len(results))
	for _, r := range results {
		before[r.Engram.ID.String()] = r.Score
	}

	out := h.apply(results)

	r1, r2, r3, r4 := findCur(out, ids.c1), findCur(out, ids.c2), findCur(out, ids.c3), findCur(out, ids.c4)
	if r1 == nil || r2 == nil || r3 == nil || r4 == nil {
		t.Fatalf("expected c1-c4 present")
	}
	if !r4.NewestOfCluster {
		t.Fatalf("expected c4 (newest ValidFrom) to be newest_of_cluster; got %+v", r4)
	}
	for _, r := range []*activation.ScoredEngram{r1, r2, r3} {
		if r.PossiblySupersededBy.String() != ids.c4 {
			t.Fatalf("expected possibly_superseded_by == c4, got %s", r.PossiblySupersededBy.String())
		}
		if r.VersionCluster == "" || r.VersionCluster != r4.VersionCluster {
			t.Fatalf("expected c1-c4 to share one version_cluster key")
		}
	}
	if r4.ClusterSize != 4 {
		t.Fatalf("expected cluster_size 4, got %d", r4.ClusterSize)
	}

	// Zero score changes anywhere.
	for _, r := range out {
		id := r.Engram.ID.String()
		if want, ok := before[id]; ok && r.Score != want {
			t.Fatalf("score changed for %s: %v -> %v (annotation must never change scores)", id, want, r.Score)
		}
	}

	// Precision controls: neither the sandbox facet nor either
	// telemetry-budget fact join the widgetflow cluster.
	widgetCluster := r4.VersionCluster
	for _, id := range []string{ids.sandbox, ids.telem1, ids.telem2} {
		r := findCur(out, id)
		if r == nil {
			t.Fatalf("expected %s present", id)
		}
		if r.VersionCluster == widgetCluster && widgetCluster != "" {
			t.Fatalf("FALSE POSITIVE: %s was clustered into the widgetflow chain", id)
		}
	}
	rSandbox := findCur(out, ids.sandbox)
	if rSandbox.VersionCluster != "" {
		t.Fatalf("FALSE POSITIVE: sandbox (no version markers, real facet) got clustered at all: %+v", rSandbox)
	}

	// telem1/telem2 ARE a genuine second chain — they should cluster
	// with EACH OTHER (differing markers v2 vs revision, shared topic tags,
	// same facet so no conflict between them), just not with the widgetflow
	// chain.
	rTelem1, rTelem2 := findCur(out, ids.telem1), findCur(out, ids.telem2)
	if rTelem1.VersionCluster == "" || rTelem1.VersionCluster != rTelem2.VersionCluster {
		t.Fatalf("expected telem1/telem2 to form their own genuine version cluster; got %+v / %+v", rTelem1, rTelem2)
	}

	// Silence control: unrelated result carries nothing.
	ru := findCur(out, ids.unrelated)
	if ru == nil || ru.VersionCluster != "" || ru.NewestOfCluster || ru.PossiblySupersededBy != (storage.ULID{}) {
		t.Fatalf("expected unrelated result untouched, got %+v", ru)
	}
}

// TestCurrencyAnnotation_TieBreak_NewestFirstWithinCluster reproduces the
// exact real-vault symptom: c1 (obsolete) and c4 (current) tie at 0.911. The
// global ULID tiebreak is oldest-first; within a detected cluster it must
// flip to newest-EffectiveValidFrom-first, so c4 outranks c1 at the tie.
func TestCurrencyAnnotation_TieBreak_NewestFirstWithinCluster(t *testing.T) {
	h, cleanup := newCurrencyHarness(t)
	defer cleanup()

	ids := seedTagAnchoredVault(h)

	results := h.scored(ids.c1, 0.911, ids.c4, 0.911)
	out := h.apply(results)

	r1, r4 := rankOfCur(out, ids.c1), rankOfCur(out, ids.c4)
	if r4 >= r1 {
		t.Fatalf("expected current c4 to outrank obsolete c1 at the exact tie inside the detected cluster; got c1 rank %d, c4 rank %d", r1, r4)
	}
	if len(out) != 2 {
		t.Fatalf("expected both tied results still returned, got %d", len(out))
	}
}

// TestCurrencyAnnotation_TieBreak_UnrelatedTiesUnaffected proves the ordering
// change is scoped to detected clusters only: an exact tie between two
// results NOT in the same cluster still uses the plain ULID-ascending
// tiebreak (the #699 pin, unweakened).
func TestCurrencyAnnotation_TieBreak_UnrelatedTiesUnaffected(t *testing.T) {
	h, cleanup := newCurrencyHarness(t)
	defer cleanup()

	ids := seedTagAnchoredVault(h)

	results := h.scored(ids.sandbox, 0.7, ids.unrelated, 0.7)
	out := h.apply(results)

	var idsAsc []string
	for _, r := range results {
		idsAsc = append(idsAsc, r.Engram.ID.String())
	}
	sort.Strings(idsAsc)

	if out[0].Engram.ID.String() != idsAsc[0] {
		t.Fatalf("expected plain ULID-ascending tiebreak for an untied-cluster pair; got order %s, %s",
			out[0].Engram.ID.String(), out[1].Engram.ID.String())
	}
}

// TestCurrencyAnnotation_ExplicitSupersedesAlwaysWins: when an explicit
// RelSupersedes link exists between a pair, the heuristic must NEVER
// heuristically annotate that pair — the asserted signal always wins and is
// never second-guessed by the advisory one (COG-25).
func TestCurrencyAnnotation_ExplicitSupersedesAlwaysWins(t *testing.T) {
	h, cleanup := newCurrencyHarness(t)
	defer cleanup()

	ids := seedTagAnchoredVault(h)
	// c2 explicitly supersedes c1 — that pair must be excluded from the
	// heuristic entirely, even though it would otherwise cleanly clear every
	// gate.
	h.supersede(ids.c2, ids.c1)

	results := h.scored(ids.c1, 0.9, ids.c2, 0.9, ids.c3, 0.9, ids.c4, 0.9)
	out := h.apply(results)

	r1 := findCur(out, ids.c1)
	if r1.PossiblySupersededBy.String() == ids.c2 {
		t.Fatalf("explicit RelSupersedes pair (c1,c2) must not ALSO get the heuristic possibly_superseded_by annotation")
	}
}

// TestCurrencyAnnotation_ExplicitSupersedesCrown_NoTransitiveLeak pins the
// COG-25 TRANSITIVE leak. Blocking the direct pair from union-find (line ~225)
// is not sufficient: union-find rejoins an explicitly-superseded member into the
// cluster through OTHER heuristic pairs, and emission then points its
// possibly_superseded_by at the crown. When the crown itself is the asserted
// superseder, that member must carry NO heuristic annotation — the asserted
// signal is authoritative and must never be duplicated/second-guessed. The
// older ...AlwaysWins test only forbade pointing at the (non-crown) explicit
// partner, so it passed even while this leak was live (Opus fleet finding).
func TestCurrencyAnnotation_ExplicitSupersedesCrown_NoTransitiveLeak(t *testing.T) {
	h, cleanup := newCurrencyHarness(t)
	defer cleanup()

	ids := seedTagAnchoredVault(h)
	// c4 is newest → the crown. Assert it EXPLICITLY supersedes c1. c1 is thus
	// covered by the authoritative supersession path and must get no heuristic
	// annotation — yet union-find still joins it into the cluster via c1-c2/c1-c3.
	h.supersede(ids.c4, ids.c1)

	results := h.scored(ids.c1, 0.9, ids.c2, 0.9, ids.c3, 0.9, ids.c4, 0.9)
	out := h.apply(results)

	r1 := findCur(out, ids.c1)
	if r1 == nil {
		t.Fatal("c1 missing from results")
	}
	if r1.PossiblySupersededBy != (storage.ULID{}) {
		t.Fatalf("COG-25 transitive leak: c1 is explicitly superseded by the crown (c4) — it must carry NO heuristic possibly_superseded_by; got %s", r1.PossiblySupersededBy.String())
	}
}

// TestCurrencyAnnotation_DigitPrefixedFacet_NotExemptFromFacetVeto pins R3: a
// digit-prefixed FACET tag (e.g. "2024-cohort") is NOT a version marker, but the
// pre-R3 counted-structure regex (which matched a bare \d+ prefix) misread it as
// one, exempting it from the facet-conflict veto — the ONE gate R2.3 proved
// load-bearing against high-cosine coexisting facets. Result: two genuinely
// different facets falsely clustered. After R3 (spelled-out numbers only), the
// digit-facet is subject to the veto and the false cluster is blocked.
func TestCurrencyAnnotation_DigitPrefixedFacet_NotExemptFromFacetVeto(t *testing.T) {
	h, cleanup := newCurrencyHarness(t)
	defer cleanup()
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	h.pad(110, "filler")

	// Give the digit-prefixed facet tag df>=3 (currencyFacetDF) with two fillers.
	for i := 0; i < 2; i++ {
		h.write(writeOpts{
			concept: "cohort filler", content: fmt.Sprintf("cohort note %d", i),
			tags: []string{"2024-cohort", "note"}, embedding: embedAt(40),
			validFrom: base.Add(time.Duration(i) * time.Hour),
		})
	}
	// A: shares the topic tags, has a real marker (v2), AND the exclusive
	// digit-facet "2024-cohort" (df>=3) — a genuinely different facet from B.
	a := h.write(writeOpts{
		concept: "widgetflow layout draft", content: "The widgetflow layout draft for the 2024 cohort.",
		tags:      []string{currencyChainTopicTag, currencyChainStructureTag, "2024-cohort", "v2"},
		embedding: embedAt(0), validFrom: base,
	})
	// B: shares the topic tags, different markers, no digit-facet — high cosine
	// to A, so ONLY the facet veto can keep them apart.
	b := h.write(writeOpts{
		concept: "widgetflow layout live", content: "The widgetflow layout v3 is now live.",
		tags:      []string{currencyChainTopicTag, currencyChainStructureTag, "v3", "live"},
		embedding: embedAt(8), validFrom: base.Add(60 * 24 * time.Hour),
	})

	out := h.apply(h.scored(a, 0.9, b, 0.9))
	ra, rb := findCur(out, a), findCur(out, b)
	if ra.VersionCluster != "" && ra.VersionCluster == rb.VersionCluster {
		t.Fatalf("digit-prefixed facet \"2024-cohort\" (df>=3) escaped the facet veto — A and B falsely clustered (cluster %q)", ra.VersionCluster)
	}
}

// The next three tests pin gates that otherwise survive mutation (fleet finding):
// each sets up a pair that clears EVERY clustering gate except the one under test,
// then asserts NO cluster forms — so deleting/inverting that single gate turns the
// test RED. They are negative controls, the mutation-proof complement to the
// positive TagAnchoredChain test.

// TestCurrencyAnnotation_TemporalFloor_SameBreathNotClustered pins currencyTemporalFloor:
// two facts written < 1h apart are a same-breath batch, not generations.
func TestCurrencyAnnotation_TemporalFloor_SameBreathNotClustered(t *testing.T) {
	h, cleanup := newCurrencyHarness(t)
	defer cleanup()
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	h.pad(110, "filler")
	a := h.write(writeOpts{concept: "widgetflow layout a", content: "widgetflow layout v2 for the standard profile.",
		tags: []string{currencyChainTopicTag, currencyChainStructureTag, "v2"}, embedding: embedAt(0), validFrom: base})
	b := h.write(writeOpts{concept: "widgetflow layout b", content: "widgetflow layout v3 for the standard profile.",
		tags: []string{currencyChainTopicTag, currencyChainStructureTag, "v3"}, embedding: embedAt(5), validFrom: base.Add(30 * time.Minute)})
	ra, rb := clusterOf(h, a, b)
	if ra != "" && ra == rb {
		t.Fatalf("facts < currencyTemporalFloor apart (same-breath) must not cluster")
	}
}

// TestCurrencyAnnotation_CosineFloor_LowSimilarityNotClustered pins currencySimThreshold:
// tag structure alone must not cluster genuinely-dissimilar content (cosine < 0.70).
func TestCurrencyAnnotation_CosineFloor_LowSimilarityNotClustered(t *testing.T) {
	h, cleanup := newCurrencyHarness(t)
	defer cleanup()
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	h.pad(110, "filler")
	a := h.write(writeOpts{concept: "widgetflow layout a", content: "widgetflow layout v2 for the standard profile.",
		tags: []string{currencyChainTopicTag, currencyChainStructureTag, "v2"}, embedding: embedAt(0), validFrom: base})
	b := h.write(writeOpts{concept: "widgetflow layout b", content: "widgetflow layout v3 for the standard profile.",
		tags: []string{currencyChainTopicTag, currencyChainStructureTag, "v3"}, embedding: embedAt(60), validFrom: base.Add(60 * 24 * time.Hour)}) // cos(60)=0.5 < 0.70
	ra, rb := clusterOf(h, a, b)
	if ra != "" && ra == rb {
		t.Fatalf("facts below the cosine sanity floor (0.70) must not cluster on tag structure alone")
	}
}

// TestCurrencyAnnotation_TagShareMin_SingleSharedTagNotClustered pins currencyTagShareMin:
// one shared non-ubiquitous tag is necessary but not sufficient.
func TestCurrencyAnnotation_TagShareMin_SingleSharedTagNotClustered(t *testing.T) {
	h, cleanup := newCurrencyHarness(t)
	defer cleanup()
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	h.pad(110, "filler")
	a := h.write(writeOpts{concept: "widgetflow layout a", content: "widgetflow layout v2 for the standard profile.",
		tags: []string{currencyChainTopicTag, "v2"}, embedding: embedAt(0), validFrom: base}) // no structure tag
	b := h.write(writeOpts{concept: "widgetflow layout b", content: "widgetflow layout v3 for the standard profile.",
		tags: []string{currencyChainTopicTag, "v3"}, embedding: embedAt(5), validFrom: base.Add(60 * 24 * time.Hour)})
	ra, rb := clusterOf(h, a, b)
	if ra != "" && ra == rb {
		t.Fatalf("a single shared non-ubiquitous tag (< currencyTagShareMin) must not cluster")
	}
}

// TestCurrencyAnnotation_TieBreak_TransitiveAcrossClusterBoundary pins the fix
// for an intransitive tie-break comparator (final-review finding): within an
// exact-score tie, ordering cluster members newest-EVF-first but non-members by
// ULID in the SAME comparator is not a strict weak ordering — with X,Y in a
// cluster (EVF(X)>EVF(Y)) and a non-member Z whose ULID sorts between them, the
// relation cycles (X<Y by EVF, Y<Z by ULID, Z<X by ULID), so sort.SliceStable
// yields different orders for different input permutations. The fix sorts by a
// total order (Score desc, ULID asc) then reorders ONLY each cluster's tied
// members among their own slots. Assert: (1) deterministic across all input
// permutations, and (2) the newer cluster member (crown) precedes the older.
func TestCurrencyAnnotation_TieBreak_TransitiveAcrossClusterBoundary(t *testing.T) {
	h, cleanup := newCurrencyHarness(t)
	defer cleanup()
	h.pad(110, "filler")
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	// Written in the order Y, Z, X so ULID(Y) < ULID(Z) < ULID(X). Y,X share the
	// topic tags + differing markers (they cluster); Z is unrelated (no shared
	// tag) so it is a non-member whose ULID sits between the two cluster members.
	yID := h.write(writeOpts{concept: "widgetflow layout v2", content: "widgetflow layout v2 for the standard profile.",
		tags: []string{currencyChainTopicTag, currencyChainStructureTag, "v2"}, embedding: embedAt(0), validFrom: base})
	zID := h.write(writeOpts{concept: "unrelated office note", content: "the office parking policy changed this quarter.",
		tags: []string{"office", "parking"}, embedding: embedAt(150), validFrom: base.Add(20 * 24 * time.Hour)})
	xID := h.write(writeOpts{concept: "widgetflow layout v3 final", content: "widgetflow layout v3 is now live for the standard profile.",
		tags: []string{currencyChainTopicTag, currencyChainStructureTag, "v3", "final"}, embedding: embedAt(6), validFrom: base.Add(60 * 24 * time.Hour)})

	// All three at an EXACT score tie -> one tie group spanning the cluster boundary.
	perms := [][]string{
		{xID, yID, zID}, {xID, zID, yID}, {yID, xID, zID},
		{yID, zID, xID}, {zID, xID, yID}, {zID, yID, xID},
	}
	var canonical []string
	for pi, order := range perms {
		out := h.apply(h.scored(order[0], 0.9, order[1], 0.9, order[2], 0.9))
		got := []string{out[0].Engram.ID.String(), out[1].Engram.ID.String(), out[2].Engram.ID.String()}
		if pi == 0 {
			canonical = got
		} else if got[0] != canonical[0] || got[1] != canonical[1] || got[2] != canonical[2] {
			t.Fatalf("non-deterministic tie order: perm %v gave %v, want %v (intransitive comparator)", order, got, canonical)
		}
		// Crown (newer cluster member X) must precede the older member Y.
		if rankOfCur(out, xID) > rankOfCur(out, yID) {
			t.Fatalf("newer cluster member X must precede older member Y; got X@%d Y@%d", rankOfCur(out, xID), rankOfCur(out, yID))
		}
	}
}

// TestCurrencyAnnotation_FutureValidFrom_NeverCrowned: a cluster member whose
// EffectiveValidFrom is in the future must never be crowned
// newest_of_cluster, even though it is chronologically the latest — a
// planned/future fact is not "true now".
func TestCurrencyAnnotation_FutureValidFrom_NeverCrowned(t *testing.T) {
	h, cleanup := newCurrencyHarness(t)
	defer cleanup()

	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	h.pad(110, "filler")

	c1 := h.write(writeOpts{
		concept:   "widgetflow layout v2",
		content:   "Widgetflow layout v2 for the standard plan.",
		tags:      []string{currencyChainTopicTag, currencyChainStructureTag, "v2"},
		embedding: embedAt(0),
		validFrom: base,
	})
	c2 := h.write(writeOpts{
		concept:   "widgetflow layout v3 live",
		content:   "Widgetflow layout v3 is now live for the standard plan.",
		tags:      []string{currencyChainTopicTag, currencyChainStructureTag, "v3", "live"},
		embedding: embedAt(6),
		validFrom: base.Add(45 * 24 * time.Hour),
	})
	future := h.write(writeOpts{
		concept:   "widgetflow layout v4 planned",
		content:   "Widgetflow layout v4 is planned for a future rollout on the standard plan.",
		tags:      []string{currencyChainTopicTag, currencyChainStructureTag, "v4", "planned"},
		embedding: embedAt(10),
		validFrom: time.Now().Add(365 * 24 * time.Hour), // far future
	})

	results := h.scored(c1, 0.9, c2, 0.9, future, 0.9)
	out := h.apply(results)

	rf := findCur(out, future)
	if rf != nil && rf.NewestOfCluster {
		t.Fatalf("a future-ValidFrom member must never be crowned newest_of_cluster")
	}
	r2 := findCur(out, c2)
	if r2 != nil && !r2.NewestOfCluster {
		t.Fatalf("expected c2 (newest non-future member) to be crowned newest_of_cluster when the true newest is future-dated")
	}
}

// TestCurrencyAnnotation_ZeroWritesDuringAnnotation proves the phase is a
// pure read: vault engram count and every examined engram's reverse
// association count are unchanged after running the phase twice.
func TestCurrencyAnnotation_ZeroWritesDuringAnnotation(t *testing.T) {
	h, cleanup := newCurrencyHarness(t)
	defer cleanup()

	ids := seedTagAnchoredVault(h)
	// Drain the write-time async workers (autoassoc included) BEFORE the
	// before/after snapshots: the seed's autoassoc edges land asynchronously,
	// and with the real embedder up (-tags localassets) one can commit
	// between the two counts — the pin then reports the seeding's own edge
	// as a phantom write by the annotation phase (#777, ~1-in-10 under
	// -race+localassets, reproduced on develop). #722 doctrine: drain,
	// never race.
	h.eng.WaitWriteTimeIdle()
	countBefore := h.vaultCount()

	revCount := func(id string) int {
		u := mustParseULIDCur(t, id)
		rev, err := h.eng.store.GetReverseAssociations(h.ctx, h.ws, u, 256)
		if err != nil {
			t.Fatal(err)
		}
		return len(rev)
	}
	watch := []string{ids.c1, ids.c2, ids.c3, ids.c4, ids.telem1, ids.telem2, ids.sandbox, ids.unrelated}
	revBefore := map[string]int{}
	for _, id := range watch {
		revBefore[id] = revCount(id)
	}

	results := h.scored(
		ids.c1, 0.911, ids.c2, 0.85, ids.c3, 0.87, ids.c4, 0.911,
		ids.telem1, 0.95, ids.telem2, 0.93, ids.sandbox, 0.9, ids.unrelated, 0.5,
	)
	_ = h.apply(results)
	_ = h.apply(results) // run twice — no accumulating side effect

	if got := h.vaultCount(); got != countBefore {
		t.Fatalf("vault engram count changed: %d -> %d (annotation phase must never write)", countBefore, got)
	}
	for _, id := range watch {
		if got := revCount(id); got != revBefore[id] {
			t.Fatalf("reverse-association count changed for %s: %d -> %d", id, revBefore[id], got)
		}
	}
}

// TestCurrencyAnnotation_SilenceControl_UbiquitousTagPopulation is the
// design's "bookmark sweep" control: a large population of engrams sharing
// >=2 tags pairwise (mirroring the real vault's 374 ambient-bookmark / 367
// auto-hook engrams) must get ZERO currency annotations — the ubiquity-ratio
// exclusion (currencyUbiquityRatio) must hold even though every pair
// trivially shares tags, because those tags are ubiquitous, not anchoring.
func TestCurrencyAnnotation_SilenceControl_UbiquitousTagPopulation(t *testing.T) {
	h, cleanup := newCurrencyHarness(t)
	defer cleanup()

	var ids []string
	for i := 0; i < 40; i++ {
		// Content is deliberately VARIED per entry (distinct subject/topic/
		// context words keyed off i) so the write-time novelty detector's
		// Jaccard-over-term-fingerprint check does NOT fire a RelRefines
		// edge between these engrams — that KEPT anchor path intentionally
		// requires no version markers (an existing RelRefines edge is
		// "write-time novelty already vouched"), so genuinely near-duplicate
		// boilerplate is not a fair test of the TAG-anchor's ubiquity
		// exclusion specifically. Real ambient-bookmark/auto-hook engrams
		// are exactly this: same pair of process tags, unrelated content.
		ids = append(ids, h.write(writeOpts{
			concept: fmt.Sprintf("chk%d", i),
			// Every token below carries a per-entry-unique suffix, so NO two
			// entries share a single token — Jaccard(fingerprint) is exactly
			// 0 between any pair, guaranteeing no RelRefines edge forms
			// (which would anchor the pair independent of tags/markers, per
			// the KEPT v1 RelRefines path — not what this test isolates).
			content:   fmt.Sprintf("subj%d ctx%d ph%d wk%d nt%d", i*7%97, i*13%89, i*19%83, i*23%79, i*29%73),
			tags:      []string{"ambient-bookmark", "auto-hook"}, // ubiquitous across this population
			embedding: embedAt(float64((i * 37) % 360)),          // spread widely, not clustered
			validFrom: time.Now().Add(time.Duration(i) * 2 * time.Hour),
		}))
	}

	var pairs []any
	for _, id := range ids {
		pairs = append(pairs, id, 0.6)
	}
	results := h.scored(pairs...)
	out := h.apply(results)

	for _, r := range out {
		if r.VersionCluster != "" || r.NewestOfCluster || r.PossiblySupersededBy != (storage.ULID{}) {
			t.Fatalf("ubiquitous-tag population must get ZERO currency annotations (no marker tags, and their shared tags are ubiquitous, not anchoring); got %+v", r)
		}
	}
}

// TestCurrencyAnnotation_Latency bounds the O(K^2) pairwise cost including
// the new tag document-frequency lookups, with a realistic top-K (20).
func TestCurrencyAnnotation_Latency(t *testing.T) {
	h, cleanup := newCurrencyHarness(t)
	defer cleanup()

	ids := seedTagAnchoredVault(h)
	all := []string{ids.c1, ids.c2, ids.c3, ids.c4, ids.telem1, ids.telem2, ids.sandbox, ids.unrelated}
	for i := 0; i < 12; i++ {
		all = append(all, h.write(writeOpts{
			concept:   "padding",
			content:   "padding filler content number",
			tags:      []string{"padding-topic", "padding-marker"},
			validFrom: time.Now(),
		}))
	}
	var pairs []any
	for _, id := range all {
		pairs = append(pairs, id, 0.5)
	}
	results := h.scored(pairs...)

	start := time.Now()
	for i := 0; i < 50; i++ {
		_ = h.apply(results)
	}
	elapsed := time.Since(start) / 50

	// The budget is a COMPLEXITY-BLOWUP DETECTOR, not a performance benchmark.
	// What this test exists to catch is an accidental O(K^2)-or-worse rewrite of
	// applyCurrencyAnnotation, which shows up as orders of magnitude — not
	// percent. A tight wall-clock threshold cannot measure anything else here:
	// shared CI runners carry multi-x scheduling noise, so a near-miss failure
	// reports on the runner, never on the code.
	//
	// It was set tight and duly flaked, failing at 54.5ms against a 50ms budget
	// (9% over) on PRs that touched only internal/mcp — blocking merges with a
	// signal that carried no information. Sized generously here at ~10x the
	// observed real cost: a genuine complexity regression at K=20 still trips it
	// by a wide margin, while runner noise no longer can.
	budget := 50 * time.Millisecond
	if raceBuild {
		// The race detector's per-access instrumentation dominates at this
		// scale; widen further rather than flake the -race CI job on overhead
		// unrelated to the real-world cost being measured.
		budget = 500 * time.Millisecond
	}
	// Always report the measurement so slow drift stays visible in CI logs even
	// while it sits comfortably inside the (deliberately loose) budget.
	t.Logf("applyCurrencyAnnotation K=%d: %v per call (budget %v, race=%v)", len(all), elapsed, budget, raceBuild)
	if elapsed > budget {
		t.Fatalf("applyCurrencyAnnotation too slow for K=%d: %v per call (budget %v) — this budget is a "+
			"complexity-blowup detector sized ~10x above real cost, so exceeding it indicates an "+
			"algorithmic regression, not runner noise", len(all), elapsed, budget)
	}
}

// --- Unit-level regression coverage for the KEPT entity/RelRefines anchors -
//
// The entity and RelRefines anchor paths are kept from v1 (#716: measured
// harmless, they simply rarely fire on the real, entity-sparse vault). These
// tests pin that they still function directly, independent of the tag path.

func TestCurrencyEntityAnchor_SharedRareEntity_Anchors(t *testing.T) {
	h, cleanup := newCurrencyHarness(t)
	defer cleanup()

	h.pad(50, "filler")
	a := h.write(writeOpts{concept: "a", content: "alpha content about a rare topic", entities: []string{"RareRegistryEntity"}, validFrom: time.Now()})
	b := h.write(writeOpts{concept: "b", content: "beta content about the same rare topic", entities: []string{"RareRegistryEntity"}, validFrom: time.Now().Add(2 * time.Hour)})

	aEng, _ := h.eng.store.GetEngram(h.ctx, h.ws, mustParseULIDCur(t, a))
	bEng, _ := h.eng.store.GetEngram(h.ctx, h.ws, mustParseULIDCur(t, b))
	n := h.vaultCount()

	if !h.eng.currencyEntityAnchor(h.ctx, h.ws, aEng.ID, bEng.ID, n) {
		t.Fatalf("expected a shared rare entity to anchor the pair")
	}
}

func TestCurrencyEntityAnchor_NoSharedEntity_DoesNotAnchor(t *testing.T) {
	h, cleanup := newCurrencyHarness(t)
	defer cleanup()

	a := h.write(writeOpts{concept: "a", content: "alpha content", entities: []string{"EntityOne"}, validFrom: time.Now()})
	b := h.write(writeOpts{concept: "b", content: "beta content", entities: []string{"EntityTwo"}, validFrom: time.Now().Add(2 * time.Hour)})

	aEng, _ := h.eng.store.GetEngram(h.ctx, h.ws, mustParseULIDCur(t, a))
	bEng, _ := h.eng.store.GetEngram(h.ctx, h.ws, mustParseULIDCur(t, b))
	n := h.vaultCount()

	if h.eng.currencyEntityAnchor(h.ctx, h.ws, aEng.ID, bEng.ID, n) {
		t.Fatalf("expected no anchor when entities are disjoint")
	}
}

// --- Marker/facet unit tests (mechanical constants, pinned directly) -------

func TestCurrencyIsVersionMarker(t *testing.T) {
	cases := map[string]bool{
		"v2": true, "v3": true, "v1.1": true, "v2-3": true,
		"four-zone": true, "three-zone": true, "five-tier": true,
		"final": true, "live": true, "future": true, "revision": true,
		// R3: digit-prefixed tags are NOT markers — they collide with facet/ID
		// tags (form numbers, year cohorts). "5-tier" and "2024-cohort" must be
		// treated as ordinary (facet-eligible) tags, not version markers.
		"5-tier": false, "2024-cohort": false, "42-alpha": false,
		"widgetflow": false, "structure": false, "sandbox": false, "telemetry": false,
	}
	for tag, want := range cases {
		if got := currencyIsVersionMarker(tag); got != want {
			t.Errorf("currencyIsVersionMarker(%q) = %v, want %v", tag, got, want)
		}
	}
}

func TestCurrencyMarkerSetsEqual_IdenticalSetsAreSiblings(t *testing.T) {
	a := currencyMarkerSet([]string{"widgetflow", "four-zone"})
	b := currencyMarkerSet([]string{"structure", "four-zone"})
	if !currencyMarkerSetsEqual(a, b) {
		t.Fatalf("expected identical single-marker sets to compare equal (same-version siblings)")
	}
	c := currencyMarkerSet([]string{"structure", "v3", "live"})
	if currencyMarkerSetsEqual(a, c) {
		t.Fatalf("expected differing marker sets to compare unequal")
	}
}
