package activation_test

import (
	"context"
	"testing"

	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/storage"
)

// Unit coverage for the phase-6 exclude-tags drop (#713) exercised through the
// real activation pipeline (ActivationEngine.Run). Fixtures use invented
// synthetic tags/content only.

// runExclude runs an activation with the given ExcludeTags / Filters over a
// two-engram store (one tagged "noise-tag", one tagged "keep-tag") and returns
// which of the two surfaced. Both engrams enter the pipeline via the recent
// pool, so the only thing that removes one is the exclude-tags drop.
func runExclude(t *testing.T, exclude []string, filters []activation.Filter) (noise, keep bool) {
	t.Helper()
	store := newStubStore()
	noiseEng := &storage.Engram{Concept: "alpha", Content: "shared body", Relevance: 0.5, Tags: []string{"noise-tag"}}
	keepEng := &storage.Engram{Concept: "beta", Content: "shared body", Relevance: 0.5, Tags: []string{"keep-tag"}}
	store.writeEngram(noiseEng)
	store.writeEngram(keepEng)
	store.recent = []storage.ULID{noiseEng.ID, keepEng.ID}

	eng := newTestEngine(store, &stubFTS{}, &emptyHNSW{})
	result, err := eng.Run(context.Background(), &activation.ActivateRequest{
		Context:     []string{"anything"},
		Threshold:   0.0,
		MaxResults:  10,
		Weights:     rrfTagWeights(),
		Filters:     filters,
		ExcludeTags: exclude,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return containsID(result.Activations, noiseEng.ID), containsID(result.Activations, keepEng.ID)
}

// TestExcludeTags_DropsCandidate: a candidate carrying an excluded tag is
// dropped from ranking; the non-excluded candidate still surfaces.
func TestExcludeTags_DropsCandidate(t *testing.T) {
	noise, keep := runExclude(t, []string{"noise-tag"}, nil)
	if noise {
		t.Error("excluded-tag candidate leaked into ranking (#713 drop failed)")
	}
	if !keep {
		t.Error("non-excluded candidate was dropped — exclusion must be tag-scoped")
	}
}

// TestExcludeTags_DefaultIdentity: with no ExcludeTags, both candidates surface
// (byte-identical to today).
func TestExcludeTags_DefaultIdentity(t *testing.T) {
	noise, keep := runExclude(t, nil, nil)
	if !noise || !keep {
		t.Errorf("default (nil ExcludeTags) dropped a candidate: noise=%v keep=%v", noise, keep)
	}
}

// TestExcludeTags_ExplicitIncludeOverrides: an explicit tags_any include naming
// the excluded tag overrides the standing exclude.
func TestExcludeTags_ExplicitIncludeOverrides(t *testing.T) {
	noise, _ := runExclude(t, []string{"noise-tag"},
		[]activation.Filter{{Field: "tags_any", Value: []string{"noise-tag"}}})
	if !noise {
		t.Error("explicit tags_any include did not override the standing exclude (#713)")
	}
}

// TestExcludeTags_UnrelatedIncludeStillDrops: an explicit include of a DIFFERENT
// tag must not resurrect the excluded candidate — only an include naming the
// excluded tag overrides.
func TestExcludeTags_UnrelatedIncludeStillDrops(t *testing.T) {
	// tags_any names only "keep-tag"; the noise engram lacks it and would fail
	// that post-filter anyway, so assert on the keep engram's survival plus the
	// noise engram staying dropped under an unrelated include.
	noise, keep := runExclude(t, []string{"noise-tag"},
		[]activation.Filter{{Field: "tags_any", Value: []string{"keep-tag"}}})
	if noise {
		t.Error("excluded candidate resurfaced under an unrelated explicit include")
	}
	if !keep {
		t.Error("keep-tag candidate should survive its own explicit include")
	}
}
