package activation

import (
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/storage"
)

// admissionOf classifies on the REASON a row entered, not on arithmetic (G6
// refute of #773, finding 1). The load-bearing case is the boundary:
// tagMatchFloor (0.1) numerically equals the COG-6 ACT-R default gate, so a
// fresh confidence-1.0 floored row's gated quantity lands EXACTLY ON the
// threshold — `gated < threshold` is false there, and only the floored flag
// keeps it a tag-filter admission.
//
// RED-sanity: at dda8a3e (three-arg admissionOf, classification on
// `gated < threshold` alone) the two floored cases at gated >= threshold below
// returned AdmissionScored.
func TestAdmissionOf(t *testing.T) {
	const threshold = 0.1 // the COG-6 ACT-R default — the value tagMatchFloor collides with
	tests := []struct {
		name      string
		gated     float64
		inTagPool bool
		floored   bool
		want      Admission
	}{
		{"scored well above the gate", 0.6, false, false, AdmissionScored},
		{"scored exactly on the gate, not a tag row", 0.1, false, false, AdmissionScored},
		{"tag row that cleared the gate on its own real evidence", 0.3, true, false, AdmissionScored},
		{"tag row below the gate on its own real evidence (S1 bypass)", 0.05, true, false, AdmissionTagFilter},
		// THE BOUNDARY: floored fresh confidence-1.0 row. gated == threshold,
		// so the arithmetic test alone calls it scored — but its ContentMatch
		// is the floor, not a measurement.
		{"floored tag row landing exactly ON the gate", 0.1, true, true, AdmissionTagFilter},
		{"floored tag row below the gate (confidence 0.9 twin)", 0.09, true, true, AdmissionTagFilter},
		// Unreachable in production (a floored row's absolute is capped at
		// tagMatchFloor by the min(Raw, ContentMatch) clamp), pinned anyway:
		// the flag must dominate whatever the arithmetic says.
		{"floored tag row above the gate", 0.2, true, true, AdmissionTagFilter},
		// floored without inTagPool cannot be produced (computeACTR floors only
		// when inTagPool); if it ever were, the filter did not admit this row,
		// so it must not be attributed to one.
		{"floored but not in the tag pool (unproducible)", 0.05, false, true, AdmissionScored},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := admissionOf(tt.gated, threshold, tt.inTagPool, tt.floored); got != tt.want {
				t.Errorf("admissionOf(%v, %v, tag=%v, floored=%v) = %v, want %v",
					tt.gated, threshold, tt.inTagPool, tt.floored, got, tt.want)
			}
		})
	}
}

// computeACTR is the ONE producer of ContentMatchFloored; computeComponents
// has no inTagPool input at all, so the flag is structurally unreachable from
// it and the CGDN/weighted-sum call sites that pass
// components.ContentMatchFloored through always pass false. This pins the
// producer's behavior on both sides of the floor.
func TestComputeACTR_SetsContentMatchFlooredOnlyWhenTheFloorFired(t *testing.T) {
	now := time.Now()
	eng := &storage.Engram{Confidence: 1.0, Stability: 30.0, CreatedAt: now, LastAccess: now}
	w := resolvedWeights{SemanticSimilarity: 0.6, FullTextRelevance: 0.4, UseACTR: true, ACTRDecay: 0.5, ACTRHebScale: DefaultACTRHebScale}

	tests := []struct {
		name        string
		fts         float64
		inTagPool   bool
		wantFloored bool
	}{
		{"content-unrelated tag row: floor fires", 0, true, true},
		{"tag row with real content evidence above the floor: no flooring", 0.9, true, false},
		{"content-unrelated non-tag row: no floor without a filter", 0, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := computeACTR(0, tt.fts, 0, 0, eng, 0, now, w, tt.inTagPool)
			if c.ContentMatchFloored != tt.wantFloored {
				t.Errorf("ContentMatchFloored = %v, want %v (ContentMatch %.4f)",
					c.ContentMatchFloored, tt.wantFloored, c.ContentMatch)
			}
			if tt.wantFloored && c.ContentMatch != tagMatchFloor {
				t.Errorf("floored ContentMatch = %v, want the tagMatchFloor %v", c.ContentMatch, tagMatchFloor)
			}
		})
	}
}
