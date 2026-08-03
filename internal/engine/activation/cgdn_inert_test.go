package activation

import (
	"context"
	"testing"

	"github.com/scrypster/muninndb/internal/storage"
)

// TestPhase6Score_CGDN_InertAtAnyPositiveThreshold pins the #768 finding as a
// fact of the shipped code rather than as a comment that can rot.
//
// computeComponents — the component producer the CGDN path uses — never sets
// ScoreComponents.ContentMatch (only computeACTR does), so it keeps its Go
// zero value. CGDN's abstention gate is
//
//	absolute := min(min(Raw, ContentMatch), 1.0) * Confidence
//	if absolute < req.Threshold && !inTagPool { continue }
//
// which is `min(Raw, 0.0) * Confidence == 0.0` for every non-tag-pool row, so
// ANY positive threshold drops every candidate. This test drives the real
// phase6Score CGDN branch with a maximally strong candidate — perfect vector
// cosine, perfect FTS coverage — at the smallest representable positive
// threshold, and asserts zero activations.
//
// It cannot pass vacuously. Its RED control is the neighbouring
// TestPhase6Score_CGDNPath, which drives the SAME function over an
// equivalent fixture at Threshold: 0.0 and DOES get output: raise this test's
// threshold to 0.0 and it would pass trivially like that one; lower
// TestPhase6Score_CGDNPath's threshold above zero and it would start failing
// like this one. The two tests bound the defect exactly at zero.
//
// If this test starts failing because CGDN began emitting results, do not
// treat that as a silent improvement — ContentMatch was wired without also
// checking #805 (the Hebbian rescue floor sits 20x above the steady-state
// Hebbian edge weight, so a live CGDN would still discard nearly its entire
// Hebbian-linked population). Read docs/internals/decision-record.md
// (#768/#805) first, then update it, CLAUDE.md and docs/feature-reference.md,
// all of which currently describe CGDN as untested/experimental rather than
// as live.
func TestPhase6Score_CGDN_InertAtAnyPositiveThreshold(t *testing.T) {
	store := newInternalStubStore()
	e := newTestActivationEngine(store)
	defer e.Close()

	eng1 := &storage.Engram{
		Concept: "cgdn inert probe", Content: "cgdn inert probe content",
		Confidence: 1.0, Stability: 30.0, Relevance: 0.8,
		State: storage.StateActive,
	}
	store.addEngram(eng1)

	// A maximally strong candidate: perfect vector cosine and perfect FTS
	// coverage. If CGDN's content gate were honestly computed this would
	// clear any realistic threshold by a wide margin.
	fused := []fusedCandidate{{id: eng1.ID, rrfScore: 0.5, ftsScore: 1.0, vectorScore: 1.0}}
	p1 := &phase1Result{queryStr: "test"}

	const tinyPositiveThreshold = 1e-9

	result, err := e.phase6Score(context.Background(), &ActivateRequest{
		MaxResults: 10, Threshold: tinyPositiveThreshold,
		Weights: &Weights{
			UseCGDN:            true,
			SemanticSimilarity: 0.5,
			FullTextRelevance:  0.3,
		},
	}, [8]byte{}, fused, nil, p1)

	if err != nil {
		t.Fatalf("phase6Score with CGDN: %v", err)
	}
	if len(result.Activations) != 0 {
		t.Fatalf("phase6Score(CGDN) returned %d activation(s) at threshold %.1e for a maximally "+
			"strong candidate; #768 says CGDN cannot clear any positive threshold because "+
			"ContentMatch is never set on this path. If this is now wired correctly, see the "+
			"doc-update list in this test's comment before treating it as a fix.",
			len(result.Activations), tinyPositiveThreshold)
	}
}
