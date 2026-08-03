package mcp

import (
	"context"
	"testing"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// #773 R4 — the two places a #764-class field silently disappears on THIS
// surface, neither of which the engine-level test can see:
//
//  1. convert.go's activationToMemory. Before this increment it mapped only
//     SemanticSimilarity, SemanticSimilarityRaw and EntityBoost, so
//     absolute_score and content_match — computed on every row and present on
//     MBP and REST — were structurally invisible to every MCP agent, reachable
//     only inside annotations.substitution_basis on a COG-28 substituted row.
//     And convert.go's "should I allocate an annotations object at all"
//     predicate has already dropped a field once, which is why relevance_band
//     is TOP-LEVEL and this test uses a row carrying NO other annotation.
//
//  2. handleRecall's hand-built result map, which is NOT a mirror of
//     mbp.ActivateResponse — a field added to the struct alone reaches REST
//     and vanishes here.
type relevanceFakeEngine struct {
	fakeEngine
	resp *mbp.ActivateResponse
}

func (e *relevanceFakeEngine) Activate(ctx context.Context, req *mbp.ActivateRequest) (*mbp.ActivateResponse, error) {
	return e.resp, nil
}

// weakResponse is the round-8 shape as the engine hands it to MCP: a row whose
// displayed score is high, whose stored confidence is 1, and whose ABSOLUTE
// evidence is under the vault's noise band. No annotations of any kind.
func weakResponse() *mbp.ActivateResponse {
	return &mbp.ActivateResponse{
		TotalFound: 1,
		Activations: []mbp.ActivationItem{{
			ID:            "01ARZ3NDEKTSV4RRFFQ69G5FAV",
			Concept:       "studio lighting rig",
			Content:       "The studio lighting rig uses three softboxes and a bounce card.",
			Score:         0.98,
			Confidence:    1,
			RelevanceBand: "weak",
			ScoreComponents: mbp.ScoreComponents{
				SemanticSimilarity: 0.09,
				AbsoluteScore:      0.14,
				ContentMatch:       0.14,
			},
		}},
	}
}

func recallOnce(t *testing.T, resp *mbp.ActivateResponse) map[string]any {
	t.Helper()
	srv := newTestServerWith(&relevanceFakeEngine{resp: resp})
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_recall","arguments":{"vault":"default","context":["anything"]}}}`
	r := decodeResp(t, postRPC(t, srv, body).Body.String())
	if r.Error != nil {
		t.Fatalf("unexpected error: %v", r.Error)
	}
	return extractInnerJSON(t, r)
}

func TestRecallOverMCP_RelevanceBandAndSummary(t *testing.T) {
	out := recallOnce(t, weakResponse())

	memories, _ := out["memories"].([]any)
	if len(memories) != 1 {
		t.Fatalf("want 1 memory, got %v", out["memories"])
	}
	m, _ := memories[0].(map[string]any)

	// Trap 1 — the row carries NO annotations object at all, which is exactly
	// the case the allocation predicate drops.
	if _, hasAnn := m["annotations"]; hasAnn {
		t.Fatal("fixture invalid: the row must carry no annotations, or this does not " +
			"exercise the allocation predicate")
	}
	if m["relevance_band"] != "weak" {
		t.Errorf("relevance_band = %v, want \"weak\" — the ONE field on the row that says "+
			"this is not an answer, and it must not depend on any other annotation "+
			"being present", m["relevance_band"])
	}

	// The honest quantities, previously unreachable from MCP entirely.
	if m["absolute_score"] != 0.14 {
		t.Errorf("absolute_score = %v, want 0.14 — the cross-query-comparable number "+
			"behind the band", m["absolute_score"])
	}
	if m["content_match"] != 0.14 {
		t.Errorf("content_match = %v, want 0.14", m["content_match"])
	}
	// And the three quantities remain DISTINCT on the wire, which is the whole
	// point: score is query-relative, relevance_band is absolute, confidence is
	// truth-belief.
	if m["score"] == m["absolute_score"] {
		t.Error("fixture invalid: score and absolute_score must differ, or the misread " +
			"this increment fixes is not represented")
	}
	if m["confidence"] != 1.0 {
		t.Errorf("confidence = %v, want 1 — the field agents were misreading as retrieval "+
			"certainty must be unchanged (D2: do not rename it, supply the missing one)", m["confidence"])
	}

	// Trap 2 — the hand-built map. The response-level weak-band block is
	// DEFERRED (it failed its pre-committed acceptance rule; see
	// engine/engine_relevance.go), so it must NOT appear. If it is ever
	// shipped, it goes through handleRecall's hand-built map, which is not a
	// mirror of mbp.ActivateResponse — this is the note that says so.
	if _, ok := out["relevance_summary"]; ok {
		t.Error("relevance_summary is shipped, but it was deferred on a measured, " +
			"pre-committed rule — either the deferral was reverted without updating " +
			"the record in engine_relevance.go, or this test is stale")
	}
}

// The band composes with, never clobbers, the existing per-response blocks.
func TestRecallOverMCP_BandComposesWithConflict(t *testing.T) {
	resp := weakResponse()
	resp.Conflict = &mbp.ConflictBlock{Unresolved: true, Warning: "two memories disagree"}

	out := recallOnce(t, resp)
	if _, ok := out["conflict"].(map[string]any); !ok {
		t.Error("the conflict block vanished")
	}
	memories, _ := out["memories"].([]any)
	m, _ := memories[0].(map[string]any)
	if m["relevance_band"] != "weak" {
		t.Errorf("relevance_band = %v on a row in a conflicted response, want weak — "+
			"\"is this disputed\" and \"did this match\" are different questions and "+
			"neither annotation may displace the other", m["relevance_band"])
	}
}

// An abstained (empty) response carries no band anywhere: there is no row to
// band, and the abstention block is the whole message.
func TestRecallOverMCP_AbstainedResponseCarriesNoBand(t *testing.T) {
	out := recallOnce(t, &mbp.ActivateResponse{
		TotalFound: 0, Abstained: true, AbstainedReason: "below_threshold",
	})
	if out["abstained"] != true {
		t.Fatalf("expected an abstained response: %v", out)
	}
	if _, ok := out["relevance_summary"]; ok {
		t.Error("an abstained response must carry no relevance block")
	}
	if mem, _ := out["memories"].([]any); len(mem) != 0 {
		t.Errorf("abstained response carried memories: %v", mem)
	}
}

// mcp.Memory is shared with muninn_read, so relevance_band is omitempty. A read
// must therefore emit no band at all rather than an empty-string one.
func TestReadOverMCP_EmitsNoRelevanceBand(t *testing.T) {
	m := readResponseToMemory(&mbp.ReadResponse{
		ID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", Concept: "c", Content: "x", Confidence: 1,
	})
	if m.RelevanceBand != "" || m.RelevanceBandBasis != "" {
		t.Errorf("muninn_read emitted a relevance band (%q/%q): a band is a RETRIEVAL "+
			"statement about a query, and a direct read answers no query",
			m.RelevanceBand, m.RelevanceBandBasis)
	}
}
