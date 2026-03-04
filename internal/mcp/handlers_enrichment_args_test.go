package mcp

import (
	"testing"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// TestApplyEnrichmentArgs_PlainStringEntityIsSkipped tests that a plain string
// entity is silently skipped (not stored as a valid entity).
//
// Before the fix: this passes but no warning is surfaced to the caller.
// After the fix: this still passes, but a malformed count is returned.
func TestApplyEnrichmentArgs_PlainStringEntityIsSkipped(t *testing.T) {
	args := map[string]any{
		"entities": []any{"PostgreSQL"}, // plain string, not map[string]any{"name":..., "type":...}
	}
	req := &mbp.WriteRequest{}
	applyEnrichmentArgs(args, req)
	if len(req.Entities) != 0 {
		t.Errorf("expected 0 entities (plain string should be skipped), got %d", len(req.Entities))
	}
}

func TestApplyEnrichmentArgs_PlainStringEntityMalformedCount(t *testing.T) {
	args := map[string]any{
		"entities": []any{
			"PostgreSQL",                                          // malformed: plain string
			map[string]any{"name": "Go", "type": "language"},    // valid
		},
	}
	req := &mbp.WriteRequest{}
	malformed := applyEnrichmentArgs(args, req)
	if malformed != 1 {
		t.Errorf("expected malformedCount=1, got %d", malformed)
	}
	if len(req.Entities) != 1 {
		t.Errorf("expected 1 valid entity, got %d", len(req.Entities))
	}
}
