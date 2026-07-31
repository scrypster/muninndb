package mcp

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// The evolve [[markup]] fix DESTROYED CONTENT on every plain-text evolve.
//
// extractMarkupEntities returns ("", nil) when there is nothing to do — its
// documented contract, so callers can leave the content alone — and
// parseEnrichmentArgs honours that with a len(names)>0 guard. handleEvolve
// assigned the return unconditionally:
//
//	newContent, markupNames := extractMarkupEntities(newContent)
//
// so any evolve whose replacement text contained no [[markup]] — the COMMON
// case — superseded the predecessor and stored a successor with EMPTY content.
// The engine performs no empty-content validation on evolve, and the existing
// MCP evolve tests use capture fakes that never read content back, which is why
// the suite stayed green. Caught by the adversarial review of this branch
// (finding 1), not by a test.
//
// Second, subtler defect (finding 2): markup names were appended to
// evolveEntities unconditionally, and the engine treats ANY non-empty entity
// list on evolve as "REPLACE the carried set". So markup-only evolve silently
// dropped every predecessor entity link — a destructive side effect the caller
// never asked for, on the exact verb the fix claimed to make consistent.
//
// Semantics pinned here:
//   - brackets are ALWAYS stripped (verb consistency with remember);
//   - markup names become entities ONLY when the caller also supplied an
//     explicit entities[] (they have already opted into replace semantics);
//   - markup-only evolve preserves the carried set and SAYS SO in warnings —
//     loud, never silent (the project's standing rule).
type evolveCaptureEngine struct {
	fakeEngine
	gotContent  string
	gotEntities []mbp.InlineEntity
}

func (e *evolveCaptureEngine) Evolve(ctx context.Context, vault, oldID, newContent, reason string, embedding []float32, concept string, entities []mbp.InlineEntity, importance *float32, effectiveAt time.Time) (*WriteResult, error) {
	e.gotContent = newContent
	e.gotEntities = entities
	return &WriteResult{ID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", Concept: concept}, nil
}

func evolveCall(t *testing.T, eng *evolveCaptureEngine, newContent, extraArgs string) map[string]any {
	t.Helper()
	srv := newTestServerWith(eng)
	body := `{"jsonrpc":"2.0","method":"tools/call","id":1,"params":{"name":"muninn_evolve","arguments":{"vault":"default","id":"01ARZ3NDEKTSV4RRFFQ69G5FAV","new_content":"` + newContent + `"` + extraArgs + `}}}`
	w := postRPC(t, srv, body)
	resp := decodeResp(t, w.Body.String())
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
	return extractInnerJSON(t, resp)
}

func TestEvolve_PlainTextContentIsPreserved(t *testing.T) {
	eng := &evolveCaptureEngine{}
	evolveCall(t, eng, "The cache TTL is now 300 seconds.", `,"reason":"raised"`)
	if eng.gotContent != "The cache TTL is now 300 seconds." {
		t.Fatalf("plain-text evolve must pass the content through UNCHANGED — got %q. An empty "+
			"string here means the predecessor was superseded and the memory's text destroyed.",
			eng.gotContent)
	}
	if len(eng.gotEntities) != 0 {
		t.Errorf("plain-text evolve must pass no entities (carry preserved), got %v", eng.gotEntities)
	}
}

func TestEvolve_MarkupOnlyStripsButPreservesCarry(t *testing.T) {
	eng := &evolveCaptureEngine{}
	out := evolveCall(t, eng, "Moved the cache to [[Tidequill]].", `,"reason":"switched"`)
	if eng.gotContent != "Moved the cache to Tidequill." {
		t.Fatalf("markup must be stripped from stored content, got %q", eng.gotContent)
	}
	// No explicit entities[] => markup must NOT trigger the engine's
	// REPLACE-the-carried-set semantics.
	if len(eng.gotEntities) != 0 {
		t.Fatalf("markup-only evolve passed entities %v — the engine treats any non-empty list as "+
			"REPLACE, silently dropping every predecessor entity link", eng.gotEntities)
	}
	// And the limitation is LOUD: the caller is told the markup names were not
	// linked and how to link them.
	warnings, _ := out["warnings"].([]any)
	found := false
	for _, w := range warnings {
		if s, ok := w.(string); ok && strings.Contains(s, "Tidequill") && strings.Contains(s, "entities") {
			found = true
		}
	}
	if !found {
		t.Errorf("markup-only evolve must WARN that the markup names were not linked (carry "+
			"preserved) — silence here is the silent-substitution class; got warnings=%v", warnings)
	}
}

func TestEvolve_MarkupWithExplicitEntitiesAppends(t *testing.T) {
	eng := &evolveCaptureEngine{}
	evolveCall(t, eng, "Moved the cache to [[Tidequill]].",
		`,"reason":"switched","entities":[{"name":"CacheLayer","type":"service"}]`)
	if eng.gotContent != "Moved the cache to Tidequill." {
		t.Fatalf("markup must be stripped, got %q", eng.gotContent)
	}
	// Caller opted into replace by supplying entities[]; markup names append.
	names := map[string]bool{}
	for _, e := range eng.gotEntities {
		names[e.Name] = true
	}
	if !names["CacheLayer"] || !names["Tidequill"] {
		t.Fatalf("explicit entities + markup must yield both (replace already opted into); got %v",
			eng.gotEntities)
	}
	// And the markup-appended entity must carry a RESOLVED type — the pre-fix
	// code also appended the name, just with Type:"", so a names-only assertion
	// passes against the old behaviour and pins nothing (re-verification pass,
	// required change 1). With the fake engine the vault table is empty, so the
	// resolver's fallback "other" is the correct expectation here.
	for _, e := range eng.gotEntities {
		if e.Name == "Tidequill" && e.Type == "" {
			t.Fatalf("markup-appended entity %q has empty type — the vault-table resolution parity "+
				"with remember is not wired", e.Name)
		}
	}
}
