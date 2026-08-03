package engine

import (
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// COG-25 PRECISION SUITE — reproductions of the false supersession advisories
// reported across five independent hands-on evaluation rounds of the advisory
// channel:
//
//   - "advisory clustering called the unrelated project-identity memory a newer
//     version of the storage decision" (round 3),
//   - "it treated an unverified contradictory claim as potentially superseding
//     the explicitly evolved decision merely because it was newer" (rounds 4/5),
//   - "generated dangerously broad supersession hints" / "false version
//     clusters" on adjacent-topic memories (rounds 4/5).
//
// Every fixture below is SYNTHETIC (invented product/topic names); none of it
// mirrors real memory content. Each test both (a) attributes the leak to the
// specific gate that admitted the pair, so the diagnosis is pinned and not just
// the symptom, and (b) asserts the advisory channel is SILENT on it.
//
// The channel's entire value is that an annotation can be trusted. A false
// advisory is strictly worse than no advisory: it marks a live fact as
// possibly-stale, which is the "silently wrong" failure class the project ranks
// worst. These are therefore precision tests, and it is expected and correct
// that they make the heuristic fire less.

// refine links a RelRefines edge (the write-time novelty near-duplicate
// relation) between two engrams, which is one of the two ANCHOR side doors.
func (h *currencyTestHarness) refine(fromID, toID string) {
	h.t.Helper()
	if _, err := h.eng.Link(h.ctx, &mbp.LinkRequest{
		Vault: "default", SourceID: fromID, TargetID: toID,
		RelType: uint16(storage.RelRefines), Weight: 1.0,
	}); err != nil {
		h.t.Fatalf("Link refines %s->%s: %v", fromID, toID, err)
	}
}

// --- Case 1: the ENTITY-anchor side door ----------------------------------
//
// Round 3 verbatim: "Advisory clustering called the unrelated project-identity
// memory a newer version of the storage decision."
//
// Shape: two memories about genuinely different things (what the project IS vs.
// which storage engine it uses) that share ONE rare entity — the project name —
// which every memory in a project vault mentions. They share NO topic tags and
// carry NO version markers whatsoever. Nothing about them says "generations of
// the same fact".
//
// Pre-fix diagnosis (asserted below): currencySharedAnchor returns true on the
// ENTITY branch (engine_currency.go currencySharedAnchor), which returns early
// and therefore never reaches currencyTagAnchor — where the version-marker gate
// lives. The marker gate is thus BYPASSED, and the only remaining
// discriminators are a 0.70 cosine floor and a facet veto that cannot fire on
// low-df tags. Any two temporally-separated, vaguely-similar memories sharing
// one rare entity cluster.
func TestCurrencyPrecision_EntityAnchorSideDoor_UnrelatedTopics(t *testing.T) {
	h, cleanup := newCurrencyHarness(t)
	defer cleanup()
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	h.pad(110, "filler")

	// The project-identity memory.
	identity := h.write(writeOpts{
		concept:   "quillstone project identity",
		content:   "Quillstone is a single binary document service written in Go for small teams.",
		entities:  []string{"quillstone"},
		tags:      []string{"identity", "overview"},
		embedding: embedAt(0),
		validFrom: base,
	})
	// An unrelated storage decision, written later. No shared tags, no markers.
	decision := h.write(writeOpts{
		concept:   "quillstone storage engine choice",
		content:   "Quillstone stores documents in an embedded key value engine rather than a hosted database.",
		entities:  []string{"quillstone"},
		tags:      []string{"storage", "backend"},
		embedding: embedAt(40), // cos 0.766 — above the 0.70 sanity floor
		validFrom: base.Add(60 * 24 * time.Hour),
	})

	idEng := h.mustGet(identity)
	decEng := h.mustGet(decision)
	n := h.vaultCount()
	dfCache := map[string]int64{}

	// Attribution: the ENTITY anchor is what admits this pair, and the tag
	// anchor (which owns the marker gate) rejects it.
	if !h.eng.currencyEntityAnchor(h.ctx, h.ws, idEng.ID, decEng.ID, n) {
		t.Fatalf("fixture precondition: expected the shared rare entity to fire the entity anchor")
	}
	if h.eng.currencyTagAnchor(h.ctx, h.ws, idEng, decEng, currencyUbiquityCutpoint(n), dfCache) {
		t.Fatalf("fixture precondition: the tag anchor must reject this pair (no shared topic tags, no markers)")
	}
	if h.eng.currencyPassesSimilarityGate(h.ctx, h.ws, idEng, decEng) != true {
		t.Fatalf("fixture precondition: expected the loose cosine floor to admit this pair (it is not the discriminator)")
	}

	// The assertion: no cluster, and no advisory.
	ra, rb := clusterOf(h, identity, decision)
	if ra != "" && ra == rb {
		t.Fatalf("FALSE ADVISORY (round 3): the unrelated project-identity memory and the storage decision were clustered as versions of one fact (cluster %q). "+
			"Admitted by the ENTITY anchor, which bypasses the version-marker gate", ra)
	}
	out := h.apply(h.scored(identity, 0.9, decision, 0.9))
	if r := findCur(out, identity); r.PossiblySupersededBy != (storage.ULID{}) {
		t.Fatalf("FALSE ADVISORY: identity memory annotated possibly_superseded_by %s", r.PossiblySupersededBy)
	}
}

// --- Case 2: the RelRefines-anchor side door -------------------------------
//
// Same structural defect as case 1 on the other early-return branch. A
// RelRefines edge is written by the write-time novelty path and asserts
// "near-duplicate", NOT "new generation" — it is a much weaker claim than the
// annotation it is allowed to produce here. Because the RelRefines branch also
// returns before currencyTagAnchor, two near-duplicate notes with no version
// vocabulary at all get a supersession advisory purely for being written at
// different times.
func TestCurrencyPrecision_RelRefinesSideDoor_NoVersionVocabulary(t *testing.T) {
	h, cleanup := newCurrencyHarness(t)
	defer cleanup()
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	h.pad(110, "filler")

	a := h.write(writeOpts{
		concept:   "onboarding checklist note",
		content:   "New teammates get a laptop, a badge, and a walkthrough of the deploy runbook.",
		tags:      []string{"onboarding"},
		embedding: embedAt(0),
		validFrom: base,
	})
	b := h.write(writeOpts{
		concept:   "onboarding checklist note, expanded",
		content:   "New teammates get a laptop, a badge, a walkthrough of the deploy runbook, and a buddy.",
		tags:      []string{"onboarding"},
		embedding: embedAt(20),
		validFrom: base.Add(60 * 24 * time.Hour),
	})
	h.refine(b, a)

	aEng, bEng := h.mustGet(a), h.mustGet(b)
	n := h.vaultCount()
	dfCache := map[string]int64{}

	if !h.eng.currencyHasAssocEdge(h.ctx, h.ws, bEng.ID, aEng.ID, storage.RelRefines) {
		t.Fatalf("fixture precondition: expected the RelRefines edge to exist")
	}
	if h.eng.currencyTagAnchor(h.ctx, h.ws, aEng, bEng, currencyUbiquityCutpoint(n), dfCache) {
		t.Fatalf("fixture precondition: the tag anchor must reject this pair (one shared tag, no markers)")
	}

	ra, rb := clusterOf(h, a, b)
	if ra != "" && ra == rb {
		t.Fatalf("FALSE ADVISORY: two near-duplicate notes with NO version vocabulary were clustered as generations (cluster %q). "+
			"Admitted by the RelRefines anchor, which bypasses the version-marker gate", ra)
	}
}

// --- Case 3: the DECLARED-SUCCESSOR leak -----------------------------------
//
// Rounds 4/5 verbatim: "it treated an unverified contradictory claim as
// potentially superseding the explicitly evolved decision merely because it was
// newer". This is the most damaging of the three: a DECLARED successor exists,
// and the heuristic advised the opposite.
//
// Shape: old -> current is an EXPLICIT RelSupersedes chain (what evolve/link
// writes). claim is a later, unverified, contradictory note that happens to be
// tag-anchored to current. The heuristic clusters {current, claim}, crowns claim
// (max EffectiveValidFrom), and annotates current possibly_superseded_by claim.
//
// Pre-fix diagnosis: the explicit-edge suppression in engine_currency.go covers
// only the SUPERSEDED side — the emission guard checks the member's own
// SupersededBy/CurrentVersion and an edge between the member and the crown. The
// declared SUCCESSOR (current) has neither: it is the head of the chain, so it
// looks to the heuristic like an ordinary un-versioned engram, and the heuristic
// is free to point an advisory at it from an arbitrary newer memory.
func TestCurrencyPrecision_DeclaredSuccessor_NotAdvisedSuperseded(t *testing.T) {
	h, cleanup := newCurrencyHarness(t)
	defer cleanup()
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	h.pad(110, "filler")

	old := h.write(writeOpts{
		concept:   "quillstone sync policy v1",
		content:   "Quillstone sync policy: documents replicate to peers once per hour on the standard plan.",
		tags:      []string{"quillstone", "sync", "v1"},
		embedding: embedAt(0),
		validFrom: base,
	})
	current := h.write(writeOpts{
		concept:   "quillstone sync policy v2",
		content:   "Quillstone sync policy: documents replicate to peers continuously on the standard plan.",
		tags:      []string{"quillstone", "sync", "v2", "live"},
		embedding: embedAt(5),
		validFrom: base.Add(30 * 24 * time.Hour),
	})
	// The explicit, asserted chain — exactly what muninn_evolve / muninn_link write.
	h.supersede(current, old)

	// A LATER, unverified, contradictory claim. Tag-anchored to current and
	// newer than it, but nobody asserted anything about it.
	claim := h.write(writeOpts{
		concept:   "quillstone sync policy, unverified draft note",
		content:   "Quillstone sync policy: someone mentioned documents replicate to peers once per day on the standard plan.",
		tags:      []string{"quillstone", "sync", "draft"},
		embedding: embedAt(9),
		validFrom: base.Add(90 * 24 * time.Hour),
	})

	out := h.apply(h.scored(old, 0.9, current, 0.9, claim, 0.9))

	rCur := findCur(out, current)
	if rCur == nil {
		t.Fatal("current missing from results")
	}
	if rCur.PossiblySupersededBy != (storage.ULID{}) {
		t.Fatalf("FALSE ADVISORY (rounds 4/5): the DECLARED successor (explicit RelSupersedes head) was annotated "+
			"possibly_superseded_by %s — an unverified newer claim. The asserted chain must suppress the heuristic on BOTH sides",
			rCur.PossiblySupersededBy)
	}
	// The declared-superseded side must stay clean too (already pinned
	// elsewhere; re-asserted here so the whole chain is covered by one test).
	if rOld := findCur(out, old); rOld.PossiblySupersededBy != (storage.ULID{}) {
		t.Fatalf("FALSE ADVISORY: the declared-superseded engram was annotated possibly_superseded_by %s", rOld.PossiblySupersededBy)
	}
	// And it must never be crowned "newest of cluster": something explicitly
	// supersedes it, so it is provably not current.
	if rOld := findCur(out, old); rOld.NewestOfCluster {
		t.Fatalf("FALSE ADVISORY: an explicitly-superseded engram was crowned newest_of_cluster")
	}
}

// --- Case 4: broad hints on adjacent-topic memories ------------------------
//
// Rounds 4/5: "generated dangerously broad supersession hints" / "false version
// clusters". Two memories on ADJACENT topics that share the project entity and
// are similar in wording, but describe different subsystems. This is the volume
// case in a real vault: most memory pairs look like this, and none of them are
// versions of each other.
func TestCurrencyPrecision_AdjacentTopics_NoBroadHints(t *testing.T) {
	h, cleanup := newCurrencyHarness(t)
	defer cleanup()
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	h.pad(110, "filler")

	auth := h.write(writeOpts{
		concept:   "quillstone auth tokens",
		content:   "Quillstone issues scoped API tokens for the standard plan and rotates them every ninety days.",
		entities:  []string{"quillstone"},
		tags:      []string{"quillstone", "auth"},
		embedding: embedAt(0),
		validFrom: base,
	})
	billing := h.write(writeOpts{
		concept:   "quillstone billing plan",
		content:   "Quillstone bills the standard plan monthly and prorates upgrades every thirty days.",
		entities:  []string{"quillstone"},
		tags:      []string{"quillstone", "billing"},
		embedding: embedAt(38),
		validFrom: base.Add(45 * 24 * time.Hour),
	})

	ra, rb := clusterOf(h, auth, billing)
	if ra != "" && ra == rb {
		t.Fatalf("FALSE ADVISORY: adjacent-topic memories (auth vs billing) were clustered as versions of one fact (cluster %q)", ra)
	}
}

// --- Aggregate gate attribution (measurement instrument) -------------------
//
// TestCurrencyPrecision_GateAttribution walks EVERY pair in the seeded
// tag-anchored corpus and reports which gate rejected it, in the same order
// applyCurrencyAnnotation applies them. It is the aggregate counterpart to the
// per-case attribution above: it makes visible how much work each gate does and
// — critically for R4 — that the newly-universal marker gate is not merely
// redundant with the anchor and facet gates.
//
// It asserts only the properties that must hold (the genuine chain is still
// admitted; nothing outside it is), and LOGS the distribution, so it stays a
// measurement rather than a brittle pin on counts.
func TestCurrencyPrecision_GateAttribution(t *testing.T) {
	h, cleanup := newCurrencyHarness(t)
	defer cleanup()

	ids := seedTagAnchoredVault(h)
	corpus := map[string]string{
		"c1": ids.c1, "c2": ids.c2, "c3": ids.c3, "c4": ids.c4,
		"telem1": ids.telem1, "telem2": ids.telem2,
		"sandbox": ids.sandbox, "unrelated": ids.unrelated,
	}
	names := make([]string, 0, len(corpus))
	for k := range corpus {
		names = append(names, k)
	}
	sortStrings(names)

	n := h.vaultCount()
	cut := currencyUbiquityCutpoint(n)
	dfCache := map[string]int64{}
	counts := map[string]int{}
	var admitted []string

	for i := 0; i < len(names); i++ {
		for j := i + 1; j < len(names); j++ {
			a, b := h.mustGet(corpus[names[i]]), h.mustGet(corpus[names[j]])
			pair := names[i] + "/" + names[j]
			switch {
			case !h.eng.currencyTemporallySeparated(a, b):
				counts["temporal"]++
			case !h.eng.currencyPassesSimilarityGate(h.ctx, h.ws, a, b):
				counts["similarity"]++
			case !currencyVersionMarkerGate(a.Tags, b.Tags):
				counts["marker(R4 universal)"]++
			case !h.eng.currencySharedAnchor(h.ctx, h.ws, a, b, n, cut, dfCache):
				counts["anchor"]++
			case h.eng.currencyFacetConflict(h.ctx, h.ws, currencyFilterTypeLabels(a.Tags), currencyFilterTypeLabels(b.Tags), dfCache):
				counts["facet"]++
			case h.eng.currencyInDeclaredChain(h.ctx, h.ws, a.ID) || h.eng.currencyInDeclaredChain(h.ctx, h.ws, b.ID):
				counts["declared-chain(R4 widened)"]++
			default:
				counts["ADMITTED"]++
				admitted = append(admitted, pair)
			}
		}
	}

	t.Logf("gate attribution over %d pairs: %v", len(names)*(len(names)-1)/2, counts)
	t.Logf("admitted pairs: %v", admitted)

	// The genuine version chain must still be admitted — precision work must
	// not silence the case the channel exists for.
	// Measured: the two genuine chains, and only those — the widgetflow chain
	// (all six c1..c4 pairs) and the telemetry chain (telem1/telem2).
	wantAdmitted := map[string]bool{
		"c1/c2": true, "c1/c3": true, "c1/c4": true,
		"c2/c3": true, "c2/c4": true, "c3/c4": true,
		"telem1/telem2": true,
	}
	got := map[string]bool{}
	for _, p := range admitted {
		got[p] = true
	}
	for p := range wantAdmitted {
		if !got[p] {
			t.Errorf("genuine chain pair %s is no longer admitted — R4 over-narrowed", p)
		}
	}
	// And nothing outside the chain may be admitted.
	for _, p := range admitted {
		if !wantAdmitted[p] {
			t.Errorf("non-chain pair %s admitted", p)
		}
	}
	// The marker gate must be doing REAL work: if every pair it rejects were
	// also rejected by the anchor or facet gates, R4 would be cosmetic.
	if counts["marker(R4 universal)"] == 0 {
		t.Errorf("the universal marker gate rejected nothing on this corpus — it cannot be shown load-bearing here")
	}
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// mustGet fetches an engram by string ID, failing the test on error.
func (h *currencyTestHarness) mustGet(id string) *storage.Engram {
	h.t.Helper()
	eng, err := h.eng.store.GetEngram(h.ctx, h.ws, mustParseULIDCur(h.t, id))
	if err != nil || eng == nil {
		h.t.Fatalf("GetEngram %s: %v", id, err)
	}
	return eng
}
