package mcp

import (
	"context"
	"testing"

	"github.com/scrypster/muninndb/internal/engine"
)

// captureEntityStateEngine records the entityType passed to SetEntityState /
// SetEntityStateBatch so tests can assert the coercion behavior.
type captureEntityStateEngine struct {
	fakeEngine
	gotType  string
	gotTypes []string
}

func (e *captureEntityStateEngine) SetEntityState(_ context.Context, _, _, _, entityType string) error {
	e.gotType = entityType
	return nil
}

func (e *captureEntityStateEngine) SetEntityStateBatch(_ context.Context, ops []engine.EntityStateOp) []error {
	errs := make([]error, len(ops))
	for _, op := range ops {
		e.gotTypes = append(e.gotTypes, op.EntityType)
	}
	return errs
}

// captureApplyEnrichmentEngine records entity types passed through ApplyEnrichment.
type captureApplyEnrichmentEngine struct {
	fakeEngine
	gotTypes []string
}

func (e *captureApplyEnrichmentEngine) ApplyEnrichment(_ context.Context, _ string, req *ApplyEnrichmentRequest) (*ApplyEnrichmentResult, error) {
	for _, ent := range req.Entities {
		e.gotTypes = append(e.gotTypes, ent.Type)
	}
	return &ApplyEnrichmentResult{
		ID:            "01HVTESTAPPLY00000000000001",
		AppliedStages: []string{"entities"},
		UpdatedAt:     "2026-03-29T12:01:00Z",
		DigestFlags:   map[string]bool{"entities": true},
		Status:        "updated",
	}, nil
}

// TestEntityState_CoercesInvalidTypeLikeRemember asserts that muninn_entity_state
// coerces an unrecognised type to "other" (after lowercase/trim), matching the
// behavior of muninn_remember's inline entities path.
func TestEntityState_CoercesInvalidTypeLikeRemember(t *testing.T) {
	eng := &captureEntityStateEngine{}
	srv := newTestServerWith(eng)
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_entity_state","arguments":{"vault":"default","entity_name":"Modbus","state":"active","type":"Protocol "}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
	if eng.gotType != "other" {
		t.Errorf("entity_state invalid type: engine got %q, want \"other\" (coerced like remember)", eng.gotType)
	}
}

// TestEntityState_NormalizesValidType asserts a recognised type passes through
// after normalisation (lowercase/trim) rather than verbatim.
func TestEntityState_NormalizesValidType(t *testing.T) {
	eng := &captureEntityStateEngine{}
	srv := newTestServerWith(eng)
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_entity_state","arguments":{"vault":"default","entity_name":"PostgreSQL","state":"active","type":" Database "}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
	if eng.gotType != "database" {
		t.Errorf("entity_state valid type: engine got %q, want \"database\" (normalized)", eng.gotType)
	}
}

// TestEntityState_EmptyTypePreserved asserts an omitted type stays empty so the
// engine preserves the existing type (coercion must not turn "" into "other").
func TestEntityState_EmptyTypePreserved(t *testing.T) {
	eng := &captureEntityStateEngine{}
	srv := newTestServerWith(eng)
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_entity_state","arguments":{"vault":"default","entity_name":"PostgreSQL","state":"deprecated"}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
	if eng.gotType != "" {
		t.Errorf("entity_state omitted type: engine got %q, want \"\" (preserve existing)", eng.gotType)
	}
}

// TestEntityStateBatch_CoercesInvalidType asserts the batch path coerces too.
func TestEntityStateBatch_CoercesInvalidType(t *testing.T) {
	eng := &captureEntityStateEngine{}
	srv := newTestServerWith(eng)
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_entity_state_batch","arguments":{"vault":"default","operations":[{"entity_name":"Modbus","state":"active","type":"protocol"},{"entity_name":"Go","state":"active","type":"Language"}]}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
	if len(eng.gotTypes) != 2 {
		t.Fatalf("expected 2 ops captured, got %d", len(eng.gotTypes))
	}
	if eng.gotTypes[0] != "other" {
		t.Errorf("batch op[0] invalid type: got %q, want \"other\"", eng.gotTypes[0])
	}
	if eng.gotTypes[1] != "language" {
		t.Errorf("batch op[1] valid type: got %q, want \"language\"", eng.gotTypes[1])
	}
}

// TestApplyEnrichment_CoercesInvalidType asserts muninn_apply_enrichment coerces
// an unrecognised type to "other" (after lowercase/trim) — matching remember —
// instead of storing it verbatim.
func TestApplyEnrichment_CoercesInvalidType(t *testing.T) {
	eng := &captureApplyEnrichmentEngine{}
	srv := newTestServerWith(eng)
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_apply_enrichment","arguments":{"vault":"default","id":"01HVTESTAPPLY00000000000001","expected_updated_at":"2026-03-29T12:00:00Z","entities":[{"name":"Modbus","type":"Protocol "},{"name":"PostgreSQL","type":"Database"}]}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
	if len(eng.gotTypes) != 2 {
		t.Fatalf("expected 2 entities captured, got %d", len(eng.gotTypes))
	}
	if eng.gotTypes[0] != "other" {
		t.Errorf("apply_enrichment invalid type: got %q, want \"other\" (coerced like remember)", eng.gotTypes[0])
	}
	if eng.gotTypes[1] != "database" {
		t.Errorf("apply_enrichment valid type: got %q, want \"database\" (normalized)", eng.gotTypes[1])
	}
}

// TestNormalizeEntityType verifies the shared helper directly.
func TestNormalizeEntityType(t *testing.T) {
	cases := map[string]string{
		"Protocol ":  "other",
		" database ": "database",
		"LANGUAGE":   "language",
		"":           "",
		"other":      "other",
		"directive":  "other",
	}
	for in, want := range cases {
		if got := normalizeEntityType(in); got != want {
			t.Errorf("normalizeEntityType(%q) = %q, want %q", in, got, want)
		}
	}
}
