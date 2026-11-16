package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// TestActivationToMemory_CarriesAllAssociationEvidence — COG-35 reports
// association evidence beside the content score instead of folding it into
// the order, so every part of it must reach an MCP agent. hebbian_boost and
// transition_boost were computed on every row and on the MBP/REST wire, and
// activationToMemory (the only path from a row to MCP) dropped both.
func TestActivationToMemory_CarriesAllAssociationEvidence(t *testing.T) {
	m := activationToMemory(&mbp.ActivationItem{
		ID: "01TRAILMEMORY0000000000000", Concept: "waterbar", Score: 0.31,
		ScoreComponents: mbp.ScoreComponents{
			AbsoluteScore: 0.31, ContentMatch: 0.31,
			EntityBoost: 0.12, HebbianBoost: 0.4, TransitionBoost: 0.5,
		},
	})
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"entity_boost":0.12`, `"hebbian_boost":0.4`, `"transition_boost":0.5`} {
		if !strings.Contains(string(b), field) {
			t.Errorf("recall row is missing %s: %s", field, b)
		}
	}
}
