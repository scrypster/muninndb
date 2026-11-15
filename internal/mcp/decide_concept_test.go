package mcp

import (
	"fmt"
	"strings"
	"testing"

	"github.com/scrypster/muninndb/internal/storage/erf"
)

func TestDecide_LongDecisionDerivesAConceptAndKeepsTheFullText(t *testing.T) {
	eng, srv, _, cleanup := newDebtServer(t)
	defer cleanup()
	const vault = "railway-decide"
	sentence := "We will re-lay the branch line with flat-bottom rail on concrete sleepers. "
	decision := strings.Repeat(sentence, 12) // ~900 bytes, well over the concept cap
	if len(decision) <= erf.MaxConceptBytes {
		t.Fatalf("fixture too short: %d", len(decision))
	}
	out := callTool(t, srv, "muninn_decide", fmt.Sprintf(`{"vault":%q,"decision":%q,"rationale":"bullhead chairs are no longer obtainable"}`, vault, decision))
	id, _ := out["id"].(string)
	if id == "" {
		t.Fatalf("decide failed on a long decision: %v", out)
	}
	concept, _ := out["concept"].(string)
	if len(concept) == 0 || len(concept) > erf.MaxConceptBytes {
		t.Fatalf("concept length %d, want 1..%d", len(concept), erf.MaxConceptBytes)
	}
	if !strings.HasSuffix(strings.TrimSpace(concept), ".") && !strings.HasSuffix(concept, "…") {
		t.Errorf("concept %q was not cut at a sentence or word boundary", concept)
	}
	if notes := notesOf(out); len(notes) == 0 || !strings.Contains(notes[0], "concept") {
		t.Errorf("notes = %v, want a note that the concept was derived", notes)
	}
	eng.WaitWriteTimeIdle()
	read := callTool(t, srv, "muninn_read", fmt.Sprintf(`{"vault":%q,"id":%q}`, vault, id))
	if c, _ := read["content"].(string); !strings.Contains(c, strings.TrimSpace(decision)) {
		t.Errorf("stored content does not contain the full decision text (len %d): got %d bytes", len(decision), len(c))
	}
}
