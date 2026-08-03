package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// ---------------------------------------------------------------------------
// Issue #769 — a carried concept is a silent assertion about the NEW content.
//
// What the issue's stated mechanism claims is already false: `concept` IS a
// declared `muninn_evolve` parameter, `handleEvolve` reads it, `EvolveAt`
// honours it, and the response echoes what was actually stored. Verified
// against the code and pinned by TestEvolveTool_DeclaresConceptParameter and
// TestEvolveAdapter_ReportsStoredConcept.
//
// The real residual is the LOUDNESS half. Inheriting the predecessor's concept
// is the right default — most evolves sharpen a fact without changing what it
// is about — but the response said nothing, so a caller who replaced the
// substance and left the label alone got a successor whose concept describes
// the OLD fact, presented as current by every concept-surfacing view. Silent,
// plausible-looking, and wrong: the project's worst failure class.
// ---------------------------------------------------------------------------

// A carried concept is announced, and the warning NAMES the carried label so
// the caller can see what is now being asserted about the new content.
func TestEvolveAdapter_WarnsWhenConceptIsCarried(t *testing.T) {
	eng, adapter, cleanup := newRealEngineAdapter(t)
	defer cleanup()
	ctx := context.Background()

	w, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   "evolve",
		Concept: "Deploy cadence: Fridays",
		Content: "We ship on Fridays",
	})
	if err != nil {
		t.Fatalf("seed write: %v", err)
	}

	res, err := adapter.Evolve(ctx, "evolve", w.ID, "We ship on Tuesdays",
		"Friday deploys caused weekend pages", nil, "", nil, nil, timeZero())
	if err != nil {
		t.Fatalf("Evolve: %v", err)
	}
	joined := strings.Join(res.Warnings, " | ")
	if joined == "" {
		t.Fatalf("#769 NOT FIXED: evolve replaced the content and carried the predecessor's concept "+
			"%q forward with NO warning — a concept-surfacing view now presents the OLD claim as current",
			"Deploy cadence: Fridays")
	}
	if !strings.Contains(joined, "Deploy cadence: Fridays") {
		t.Errorf("the carried-concept warning must NAME the carried label so the caller can judge it; got %q", joined)
	}
	if !strings.Contains(joined, "concept") {
		t.Errorf("the carried-concept warning must name the parameter to set; got %q", joined)
	}
}

// The warning is for the SILENT case only. A caller who supplied a concept made
// the assertion deliberately and must not be nagged — a warning on every evolve
// is a warning on none.
func TestEvolveAdapter_NoWarningWhenConceptSupplied(t *testing.T) {
	eng, adapter, cleanup := newRealEngineAdapter(t)
	defer cleanup()
	ctx := context.Background()

	w, err := eng.Write(ctx, &mbp.WriteRequest{
		Vault:   "evolve",
		Concept: "Deploy cadence: Fridays",
		Content: "We ship on Fridays",
	})
	if err != nil {
		t.Fatalf("seed write: %v", err)
	}
	res, err := adapter.Evolve(ctx, "evolve", w.ID, "We ship on Tuesdays",
		"Friday deploys caused weekend pages", nil, "Deploy cadence: Tuesdays", nil, nil, timeZero())
	if err != nil {
		t.Fatalf("Evolve: %v", err)
	}
	for _, warning := range res.Warnings {
		if strings.Contains(warning, "concept") {
			t.Errorf("an explicit concept must not warn; got %q", warning)
		}
	}
}

// The parameter the issue claimed was missing. Pinned so a future tools.go edit
// cannot quietly remove it and reopen #769 with its original mechanism.
func TestEvolveTool_DeclaresConceptParameter(t *testing.T) {
	for _, tool := range allToolDefinitions() {
		if tool.Name != "muninn_evolve" {
			continue
		}
		schema, ok := tool.InputSchema.(map[string]any)
		if !ok {
			t.Fatalf("muninn_evolve input schema is not a map")
		}
		props, ok := schema["properties"].(map[string]any)
		if !ok {
			t.Fatalf("muninn_evolve input schema has no properties map")
		}
		if _, ok := props["concept"]; !ok {
			t.Fatalf("muninn_evolve does not declare a 'concept' parameter — the successor's label "+
				"can then only ever be the predecessor's (#769). properties=%v", props)
		}
		return
	}
	t.Fatal("muninn_evolve is not in the full toolset")
}
