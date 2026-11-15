package mcp

import (
	"testing"
)

func TestLookupMode_KnownModes(t *testing.T) {
	for _, name := range []string{"semantic", "recent", "balanced", "deep"} {
		m, err := lookupMode(name)
		if err != nil {
			t.Errorf("lookupMode(%q): unexpected error: %v", name, err)
		}
		_ = m
	}
}

func TestLookupMode_UnknownMode(t *testing.T) {
	_, err := lookupMode("turbo")
	if err == nil {
		t.Error("lookupMode(unknown): expected error, got nil")
	}
}

func TestLookupMode_DeepPreset(t *testing.T) {
	m, err := lookupMode("deep")
	if err != nil {
		t.Fatalf("lookupMode(deep): %v", err)
	}
	if m.MaxHops != 4 {
		t.Errorf("deep MaxHops = %d, want 4", m.MaxHops)
	}
	if m.Threshold != 0.1 {
		t.Errorf("deep Threshold = %v, want 0.1", m.Threshold)
	}
}

func TestLookupMode_SemanticPreset(t *testing.T) {
	m, err := lookupMode("semantic")
	if err != nil {
		t.Fatalf("lookupMode(semantic): %v", err)
	}
	if m.Threshold != 0.3 {
		t.Errorf("semantic Threshold = %v, want 0.3", m.Threshold)
	}
	if m.SemanticSimilarity != 0.8 {
		t.Errorf("semantic SemanticSimilarity = %v, want 0.8", m.SemanticSimilarity)
	}
	if !m.DisableACTR {
		t.Error("semantic DisableACTR should be true")
	}
}

// TestLookupMode_RecentPreset: recent is an ALIAS of balanced — the empty
// preset (recent-mode orientation design v2, D1). It used to carry {Recency
// 0.7, Sem 0.3, MaxHops 1, Threshold 0.2}, which ACT-R could not read and
// which made topical recalls abstain; the MCP layer still forwards the mode
// string because it is the COG-29 orientation trigger.
func TestLookupMode_RecentPreset(t *testing.T) {
	m, err := lookupMode("recent")
	if err != nil {
		t.Fatalf("lookupMode(recent): %v", err)
	}
	if m != (RecallMode{}) {
		t.Errorf("recent must be the empty preset (alias of balanced), got %+v", m)
	}
}

func TestLookupMode_BalancedIsZero(t *testing.T) {
	m, err := lookupMode("balanced")
	if err != nil {
		t.Fatalf("lookupMode(balanced): %v", err)
	}
	if m.MaxHops != 0 || m.Threshold != 0 || m.SemanticSimilarity != 0 {
		t.Errorf("balanced should be zero-valued, got %+v", m)
	}
}

func TestLookupMode_DelegatesConsistently(t *testing.T) {
	// Verify MCP lookupMode returns the same values as auth.LookupRecallMode
	// to ensure the delegation wrapper doesn't lose or mangle fields.
	for _, name := range []string{"semantic", "recent", "balanced", "deep"} {
		m, err := lookupMode(name)
		if err != nil {
			t.Fatalf("lookupMode(%q): %v", name, err)
		}
		// Deep: MaxHops=4, Threshold=0.1
		// Semantic: SemanticSimilarity=0.8, FullTextRelevance=0.2
		// Recent: all zero (alias of balanced)
		switch name {
		case "deep":
			if m.MaxHops != 4 || m.Threshold != 0.1 {
				t.Errorf("deep mismatch: %+v", m)
			}
		case "semantic":
			if m.FullTextRelevance != 0.2 {
				t.Errorf("semantic FullTextRelevance = %v, want 0.2", m.FullTextRelevance)
			}
		case "recent":
			if m != (RecallMode{}) {
				t.Errorf("recent must be all-zero (alias of balanced), got %+v", m)
			}
		}
	}
}
