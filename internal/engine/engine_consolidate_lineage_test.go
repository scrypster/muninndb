package engine

import (
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/storage"
)

// ---------------------------------------------------------------------------
// Issue #779 — consolidation is content REPLACEMENT and must declare it.
//
// Consolidate replaces N memories with one merged memory. That is the same
// content-replacing act as evolve and Link(relation=supersedes), and both of
// those DECLARE it: a RelSupersedes edge plus a closed ValidUntil stamp on the
// record they replace. Consolidate archived its sources with a plain soft-delete
// — no edge, an OPEN ValidUntil — which is the signature COG-28 reads as
// "trash, not history".
//
// Consequence: consolidation was the one content-replacing operation excluded
// from the mechanism built to close exactly this hole. A query phrased against a
// source's wording reached a record the lifecycle cut discards, with nothing
// declaring where the content went, so recall returned nothing about a fact the
// merge was meant to PRESERVE. Unlike the embed-lag race the issue describes,
// that hole is permanent.
//
// Every fixture below is synthetic.
// ---------------------------------------------------------------------------

// seedConsolidation writes two sources whose wording is strongly lexical and
// merges them into text that shares as little of it as the merge plausibly can,
// so the FTS pool reaches the SOURCES and not the merged record — the #779
// shape, and the same shape seedRetentionChain uses for evolve.
func seedConsolidation(h *versionHeadHarness) (sourceA, sourceB, merged string) {
	h.t.Helper()
	sourceA = h.write("sweeper cadence",
		"Telemetry rows older than ninety days are purged nightly by the vacuum sweeper.")
	sourceB = h.write("sweeper owner",
		"The vacuum sweeper purge job is owned by the platform rota.")
	res, err := h.eng.Consolidate(h.ctx, "default", []string{sourceA, sourceB},
		"Aged instrumentation records are compacted into cold shards each quarter under platform rota ownership.")
	if err != nil {
		h.t.Fatalf("consolidate: %v", err)
	}
	if len(res.Archived) != 2 {
		h.t.Fatalf("both sources must be archived; archived=%v warnings=%v", res.Archived, res.Warnings)
	}
	return sourceA, sourceB, res.MergedID.String()
}

// GATE 1 (storage): a consolidated source carries the declared-supersession
// signature — a RelSupersedes edge from the merged engram and a CLOSED
// ValidUntil — exactly as an evolved predecessor does. Without both, COG-28
// cannot admit it as a predecessor at all.
func TestConsolidate_DeclaresSupersessionOnItsSources(t *testing.T) {
	h, cleanup := newVersionHeadHarness(t)
	defer cleanup()

	sourceA, sourceB, merged := seedConsolidation(h)
	mergedULID := h.mustParse(merged)

	for _, src := range []string{sourceA, sourceB} {
		srcULID := h.mustParse(src)
		eng, err := h.eng.store.GetEngram(h.ctx, h.ws, srcULID)
		if err != nil || eng == nil {
			t.Fatalf("read consolidated source %s: %v", src, err)
		}
		if eng.State != storage.StateSoftDeleted {
			t.Errorf("consolidated source %s state = %v, want soft_deleted", src, eng.State)
		}
		if eng.ValidUntil.IsZero() {
			t.Errorf("consolidated source %s has an OPEN ValidUntil — soft-deleted with an open stamp is "+
				"the plain-forget signature ('trash, not history'), so COG-28 can never admit it as a "+
				"predecessor and the merged content is unreachable from the source's wording", src)
		}
		// The declared edge, in the direction the reverse-association walk reads:
		// GetReverseAssociations(source) yields the engrams that supersede it.
		rev, err := h.eng.store.GetReverseAssociations(h.ctx, h.ws, srcULID, 64)
		if err != nil {
			t.Fatalf("reverse associations for %s: %v", src, err)
		}
		found := false
		for i := range rev {
			if rev[i].RelType == storage.RelSupersedes && rev[i].TargetID == mergedULID {
				found = true
			}
		}
		if !found {
			t.Errorf("no RelSupersedes edge from merged %s to consolidated source %s — consolidation "+
				"replaced the content without declaring it, so the lineage is unwalkable", merged, src)
		}
	}
}

// GATE 2 (recall, end to end): the whole point. A query phrased against a
// CONSOLIDATED SOURCE's wording must return the merged memory, substituted and
// attributed, exactly as the evolve case does. The source itself is never
// resurrected.
//
// This drives the full Activate pipeline rather than a hand-built result set:
// the source is dropped by phase 6's lifecycle cut before anything downstream
// can see it, so a harness that injected a shadow would test the half that was
// never broken.
func TestConsolidate_SourceWordingReturnsMergedMemory(t *testing.T) {
	h, cleanup := newVersionHeadHarness(t)
	defer cleanup()

	sourceA, _, merged := seedConsolidation(h)
	resp := h.recall(retentionQuery)

	if itemByID(resp, sourceA) != nil {
		t.Fatalf("the consolidated source was RETURNED (%v) — substitution redirects the evidence, it never resurrects the archived row", ids(resp))
	}
	head := itemByID(resp, merged)
	if head == nil {
		t.Fatalf("#779 NOT FIXED: query %q matched only a consolidated source's wording and the merged "+
			"memory was not returned — consolidation left a permanent recall hole over the fact it was "+
			"meant to preserve. got=%v abstained=%v/%q",
			retentionQuery, ids(resp), resp.Abstained, resp.AbstainedReason)
	}
	if head.SubstitutedFor != sourceA {
		t.Errorf("substituted_for = %q, want the consolidated source %q — a substituted row MUST say whose match admitted it", head.SubstitutedFor, sourceA)
	}
	if head.SubstitutionBasis == nil {
		t.Fatalf("substitution_basis is nil on a substituted row — the score components shown are the source's measurements and must be attributed")
	}
}

// A JOIN is not a FORK. Consolidation points MANY sources at ONE merged engram,
// so each source still has exactly one superseder and every chain resolves. The
// ambiguity refusal exists for a node with several *rival* superseders; pinning
// this stops a future reader from "fixing" the walk into refusing every merge.
func TestConsolidate_ManySourcesOneHeadIsNotAmbiguous(t *testing.T) {
	h, cleanup := newVersionHeadHarness(t)
	defer cleanup()

	sourceA, sourceB, merged := seedConsolidation(h)
	gate := newVisibilityGate(&activation.ActivateRequest{}, time.Now())
	nameable := func(*storage.Engram) bool { return true }

	for _, src := range []string{sourceA, sourceB} {
		walk := h.eng.resolveSupersessionHead(h.ctx, h.ws, h.mustParse(src), gate, nameable)
		if !walk.OK {
			t.Fatalf("chain from consolidated source %s did not resolve: reason=%q", src, walk.Reason)
		}
		if walk.Head.ID.String() != merged {
			t.Errorf("head for %s = %s, want the merged engram %s", src, walk.Head.ID, merged)
		}
	}
}

// A source that was ALREADY superseded gains a second superseder, which the
// walk correctly reports as a FORK: two rival replacements for one record, and
// nothing in the data says which is authoritative. Recall refuses and names the
// refusal rather than guessing — the designed response, and reachable only by a
// caller who consolidates a memory they have already evolved away.
//
// Pinned so the behaviour is a recorded decision rather than a surprise. The
// alternative — silently skipping such a source — was rejected: consolidate
// would then report it as archived while leaving it live and unlinked, which is
// the response-asserts-something-untrue class the survivor guard exists to
// prevent.
func TestConsolidate_AlreadySupersededSourceForksAndRefuses(t *testing.T) {
	h, cleanup := newVersionHeadHarness(t)
	defer cleanup()

	original := h.write("sweeper cadence",
		"Telemetry rows older than ninety days are purged nightly by the vacuum sweeper.")
	h.evolve(original, "sweeper cadence", "Telemetry rows are purged weekly instead.")

	other := h.write("sweeper owner", "The vacuum sweeper purge job is owned by the platform rota.")
	if _, err := h.eng.Consolidate(h.ctx, "default", []string{original, other},
		"Aged instrumentation records are compacted into cold shards each quarter."); err != nil {
		t.Fatalf("consolidate: %v", err)
	}

	gate := newVisibilityGate(&activation.ActivateRequest{}, time.Now())
	walk := h.eng.resolveSupersessionHead(h.ctx, h.ws, h.mustParse(original), gate,
		func(*storage.Engram) bool { return true })
	if walk.OK {
		t.Errorf("a source with two rival superseders resolved to %s — the walk must refuse a fork, not pick one", walk.Head.ID)
	}
	if walk.Reason != supersessionBlockAmbiguous {
		t.Errorf("refusal reason = %q, want %q — the abstention must name the fork so an agent can act on it",
			walk.Reason, supersessionBlockAmbiguous)
	}
}
