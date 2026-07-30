package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// All fixtures below are INVENTED synthetic values — no real vault data.

// TestAttachSelfKnowledge_Stale: a result carrying a supersession stamp yields
// stale=true and surfaces its current_version in the self_knowledge block.
func TestAttachSelfKnowledge_Stale(t *testing.T) {
	item := mbp.ActivationItem{
		ID:             "id-old",
		SupersededBy:   "id-mid",
		CurrentVersion: "id-head",
	}
	m := activationToMemory(&item)
	attachSelfKnowledge(&m, &item)

	if m.SelfKnowledge == nil {
		t.Fatal("expected self_knowledge block")
	}
	if !m.SelfKnowledge.Stale {
		t.Error("expected stale=true for a superseded result")
	}
	if m.SelfKnowledge.CurrentVersion != "id-head" {
		t.Errorf("current_version = %q, want id-head", m.SelfKnowledge.CurrentVersion)
	}
}

// TestAttachSelfKnowledge_NotStale: a result with no supersession is not stale.
func TestAttachSelfKnowledge_NotStale(t *testing.T) {
	item := mbp.ActivationItem{ID: "id-fresh"}
	m := activationToMemory(&item)
	attachSelfKnowledge(&m, &item)

	if m.SelfKnowledge == nil {
		t.Fatal("expected self_knowledge block")
	}
	if m.SelfKnowledge.Stale {
		t.Error("expected stale=false for a non-superseded result")
	}
}

// TestAttachSelfKnowledge_Contradicts: contradicts_ids carried on the
// ActivationItem is surfaced into the self_knowledge block.
func TestAttachSelfKnowledge_Contradicts(t *testing.T) {
	item := mbp.ActivationItem{ID: "id-a", ContradictsIDs: []string{"id-b"}}
	m := activationToMemory(&item)
	attachSelfKnowledge(&m, &item)

	if m.SelfKnowledge == nil || len(m.SelfKnowledge.ContradictsIDs) != 1 ||
		m.SelfKnowledge.ContradictsIDs[0] != "id-b" {
		t.Fatalf("expected contradicts_ids=[id-b], got %+v", m.SelfKnowledge)
	}
}

// TestSelfKnowledge_OmittedWhenNotAttached is the backward-compat guard: a
// Memory built WITHOUT attachSelfKnowledge (flag off) must serialize with no
// self_knowledge key at all — old clients see today's exact response.
func TestSelfKnowledge_OmittedWhenNotAttached(t *testing.T) {
	item := mbp.ActivationItem{ID: "id-x", ContradictsIDs: []string{"id-y"}}
	m := activationToMemory(&item) // flag off: attachSelfKnowledge NOT called

	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), "self_knowledge") {
		t.Fatalf("self_knowledge must be absent when flag off, got: %s", b)
	}
}
