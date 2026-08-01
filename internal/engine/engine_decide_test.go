package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/scrypster/muninndb/internal/storage"
)

// TestDecide_StoresDecisionTypedMemory: the dedicated decision-recording tool
// stored a FACT tagged "decision". Consequences that made it a real defect and
// not a cosmetic one:
//   - derived importance came out at the fact tier (0.4) instead of the
//     decision tier (0.6) — internal/storage/importance.go groups TypeDecision
//     with goal/constraint/identity;
//   - the memory was invisible to every type-based filter, so "show me the
//     decisions" could not find decisions recorded by muninn_decide.
//
// Same silent-downgrade class as #742/#743/#745, still live inside Decide.
//
// RED without the fix: MemoryType is TypeFact (the uint8 zero value) because
// Decide's WriteRequest never set it.
func TestDecide_StoresDecisionTypedMemory(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	res, err := eng.Decide(ctx, "decide-type",
		"Standardize on Go 1.23 across the sandbox services",
		"1.23 has the loopvar change we keep tripping over",
		nil, nil)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	stored, err := eng.GetEngram(ctx, "decide-type", res.ID)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if stored.MemoryType != storage.TypeDecision {
		t.Errorf("MemoryType = %v (%s), want TypeDecision", stored.MemoryType, stored.MemoryType.String())
	}
	if stored.TypeLabel != "decision" {
		t.Errorf("TypeLabel = %q, want %q", stored.TypeLabel, "decision")
	}
	// The "decision" tag stays: it is what existing recalls and the currency
	// heuristic (engine_currency.go) already key on.
	var tagged bool
	for _, tg := range stored.Tags {
		if tg == "decision" {
			tagged = true
		}
	}
	if !tagged {
		t.Error(`the "decision" tag must be preserved alongside the type`)
	}
}

// TestDecide_ContentStatesTheDecision: the decision text lived only in
// `concept`; `content` held rationale (+ alternatives) alone. Anything that
// reads the body — a recall snippet, an export, a summarizer, a human — got the
// reasoning without ever being told what was decided.
//
// RED without the fix: content == rationale, and does not contain the decision.
func TestDecide_ContentStatesTheDecision(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()

	const (
		decision  = "Standardize on Go 1.23 across the sandbox services"
		rationale = "1.23 has the loopvar change we keep tripping over"
	)
	res, err := eng.Decide(ctx, "decide-body", decision, rationale,
		[]string{"Stay on 1.21", "Jump straight to 1.24"}, nil)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	stored, err := eng.GetEngram(ctx, "decide-body", res.ID)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !strings.Contains(stored.Content, decision) {
		t.Errorf("content does not state the decision.\ncontent = %q", stored.Content)
	}
	if !strings.Contains(stored.Content, rationale) {
		t.Errorf("content lost the rationale.\ncontent = %q", stored.Content)
	}
	for _, alt := range []string{"Stay on 1.21", "Jump straight to 1.24"} {
		if !strings.Contains(stored.Content, alt) {
			t.Errorf("content lost alternative %q.\ncontent = %q", alt, stored.Content)
		}
	}
	// The decision must lead the body, not be buried after the reasoning.
	if !strings.HasPrefix(strings.TrimSpace(stored.Content), decision) {
		t.Errorf("content should open with the decision itself.\ncontent = %q", stored.Content)
	}
}
