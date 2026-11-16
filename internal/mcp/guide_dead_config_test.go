package mcp

import (
	"testing"

	"github.com/scrypster/muninndb/internal/auth"
)

// TestGuide_DoesNotReportConfigNothingReads: temporal_enabled,
// auto_link_neighbors, temporal_halflife and relevance_floor are resolved
// into ResolvedPlasticity but no runtime path reads them. The guide printed
// "Temporal decay: enabled/disabled" from temporal_enabled — a statement about
// behaviour that the flag does not control. Flipping a dead field must not
// change a single byte of the guide.
func TestGuide_DoesNotReportConfigNothingReads(t *testing.T) {
	base := auth.ResolvePlasticity(nil)
	want := generateGuide("railway", base, engineStats{EngramCount: 3}, "")
	for name, mutate := range map[string]func(r *auth.ResolvedPlasticity){
		"temporal_enabled":    func(r *auth.ResolvedPlasticity) { r.TemporalEnabled = !r.TemporalEnabled },
		"auto_link_neighbors": func(r *auth.ResolvedPlasticity) { r.AutoLinkNeighbors = !r.AutoLinkNeighbors },
		"temporal_halflife":   func(r *auth.ResolvedPlasticity) { r.TemporalHalflife = r.TemporalHalflife*3 + 1 },
		"relevance_floor":     func(r *auth.ResolvedPlasticity) { r.RelevanceFloor = 1 - r.RelevanceFloor },
	} {
		r := base
		mutate(&r)
		if got := generateGuide("railway", r, engineStats{EngramCount: 3}, ""); got != want {
			t.Errorf("guide output changes with %s, a field no runtime path reads — it is reporting configuration that has no effect", name)
		}
	}
}
