package activation

import (
	"context"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/storage"
)

// TestPhase6Score_ShadowScorer_DoesNotHonorTagPoolFloor is the RED-first pin
// for #776 (COG-28 amendment): shadow.go states in its own comment that "THE
// TAG-POOL BYPASS IS DELIBERATELY NOT APPLIED" to a shadow candidate, because
// a shadow is evidence, not a returned row, and letting a tag hit manufacture
// a substitution would admit a chain head on zero aboutness. But the ACT-R
// shadow scorer passed c.inTagPool straight into computeACTR, so a
// tag-pooled, content-unrelated superseded predecessor got the 0.1
// tagMatchFloor and cleared the shadow gate on the floor ALONE — exactly the
// bypass the comment says is refused.
//
// This candidate has zero vector/FTS signal (no genuine content match) and
// is tag-pooled only. Without the floor its ContentMatch is 0, its
// AbsoluteScore is 0, and it must NOT clear even a near-zero threshold.
func TestPhase6Score_ShadowScorer_DoesNotHonorTagPoolFloor(t *testing.T) {
	store := newInternalStubStore()
	e := newTestActivationEngine(store)
	defer e.Close()

	now := time.Now()
	predecessor := &storage.Engram{
		Concept: "stale", Content: "stale content",
		Confidence:  1.0,
		Stability:   30.0,
		AccessCount: 50,
		LastAccess:  now,
		State:       storage.StateSoftDeleted,
		// A closed ValidUntil is the declared-supersession signature
		// (hasSupersessionSignature): soft-deleted + non-zero ValidUntil.
		ValidUntil: now.Add(-time.Hour),
	}
	store.addEngram(predecessor)

	// Tag-pooled, but carries NO genuine content signal: vectorScore and
	// ftsScore are both zero, so only the tag-pool floor (if wrongly applied)
	// could rescue this candidate's absolute score above zero.
	fused := []fusedCandidate{{id: predecessor.ID, inTagPool: true, vectorScore: 0, ftsScore: 0}}
	p1 := &phase1Result{queryStr: "unrelated query"}

	result, err := e.phase6Score(context.Background(), &ActivateRequest{
		MaxResults: 10, Threshold: 0.01,
	}, [8]byte{}, fused, nil, p1)
	if err != nil {
		t.Fatalf("phase6Score: %v", err)
	}

	for _, sm := range result.ShadowMatches {
		if sm.Engram != nil && sm.Engram.ID == predecessor.ID {
			t.Fatalf("shadow scorer admitted a content-unrelated tag-pooled predecessor on the tag-pool floor alone: Gated=%v AbsoluteScore=%v (must be refused: the tag-pool bypass must not apply to shadows)",
				sm.Gated, sm.Components.AbsoluteScore)
		}
	}
}
