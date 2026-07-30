package engine

import (
	"testing"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// All fixtures below are INVENTED synthetic memories — no real vault content,
// tags, entities, or IDs. The contradiction detector is pure text, so these
// exercise the wiring without any stored data.

func item(id, concept, content string) mbp.ActivationItem {
	return mbp.ActivationItem{ID: id, Concept: concept, Content: content}
}

// contradictsSet returns item i's ContradictsIDs as a set for order-independent
// assertions.
func contradictsSet(it mbp.ActivationItem) map[string]bool {
	s := make(map[string]bool, len(it.ContradictsIDs))
	for _, id := range it.ContradictsIDs {
		s[id] = true
	}
	return s
}

// TestAnnotateContradictions_NumericSwap: two returned results that assert a
// swapped value in the same slot must each name the other in ContradictsIDs.
func TestAnnotateContradictions_NumericSwap(t *testing.T) {
	items := []mbp.ActivationItem{
		item("id-a", "widget batch limit", "the widget batch limit is 100 per request"),
		item("id-b", "widget batch limit", "the widget batch limit is 250 per request"),
	}
	annotateContradictions(items)

	if !contradictsSet(items[0])["id-b"] {
		t.Fatalf("expected id-a to contradict id-b, got %v", items[0].ContradictsIDs)
	}
	if !contradictsSet(items[1])["id-a"] {
		t.Fatalf("expected id-b to contradict id-a, got %v", items[1].ContradictsIDs)
	}
}

// TestAnnotateContradictions_PolarityFlip: an antonym pair over a shared subject
// (enabled vs disabled) must be flagged both ways.
func TestAnnotateContradictions_PolarityFlip(t *testing.T) {
	items := []mbp.ActivationItem{
		item("id-a", "beta gadget flag", "the beta gadget flag is enabled for all tenants"),
		item("id-b", "beta gadget flag", "the beta gadget flag is disabled for all tenants"),
	}
	annotateContradictions(items)

	if !contradictsSet(items[0])["id-b"] || !contradictsSet(items[1])["id-a"] {
		t.Fatalf("expected mutual contradiction, got a=%v b=%v",
			items[0].ContradictsIDs, items[1].ContradictsIDs)
	}
}

// TestAnnotateContradictions_ParaphraseNotFlagged is the decisive privacy-safe
// case: two same-subject paraphrases that agree must NOT be flagged. This is the
// property that separates "conflict" from mere "similar".
func TestAnnotateContradictions_ParaphraseNotFlagged(t *testing.T) {
	items := []mbp.ActivationItem{
		item("id-a", "sprocket cache policy", "the sprocket cache holds session tokens"),
		item("id-b", "sprocket cache policy", "the sprocket cache stores session tokens"),
	}
	annotateContradictions(items)

	if len(items[0].ContradictsIDs) != 0 || len(items[1].ContradictsIDs) != 0 {
		t.Fatalf("paraphrases must not be flagged, got a=%v b=%v",
			items[0].ContradictsIDs, items[1].ContradictsIDs)
	}
}

// TestAnnotateContradictions_UnrelatedNotFlagged: results about different
// subjects, even both carrying numbers, are never compared (shared-subject gate).
func TestAnnotateContradictions_UnrelatedNotFlagged(t *testing.T) {
	items := []mbp.ActivationItem{
		item("id-a", "doohickey timeout", "the doohickey timeout is 30 seconds"),
		item("id-b", "gizmo retry count", "the gizmo retry count is 5 attempts"),
	}
	annotateContradictions(items)

	if len(items[0].ContradictsIDs) != 0 || len(items[1].ContradictsIDs) != 0 {
		t.Fatalf("unrelated results must not be flagged, got a=%v b=%v",
			items[0].ContradictsIDs, items[1].ContradictsIDs)
	}
}

// TestAnnotateContradictions_OrderInvariant is the HARD invariant: the pass must
// never change result order, membership, or count — it only appends to
// ContradictsIDs. We snapshot the ID sequence, run the pass over a mixed set
// (one conflicting pair, one unrelated), and assert the sequence is identical.
func TestAnnotateContradictions_OrderInvariant(t *testing.T) {
	items := []mbp.ActivationItem{
		item("id-0", "flange torque spec", "the flange torque spec is 40 newton meters"),
		item("id-1", "unrelated topic alpha", "the alpha module ships on tuesday"),
		item("id-2", "flange torque spec", "the flange torque spec is 55 newton meters"),
		item("id-3", "unrelated topic beta", "the beta module ships on friday"),
	}
	before := make([]string, len(items))
	for i, it := range items {
		before[i] = it.ID
	}

	annotateContradictions(items)

	if len(items) != len(before) {
		t.Fatalf("count changed: %d -> %d", len(before), len(items))
	}
	for i, it := range items {
		if it.ID != before[i] {
			t.Fatalf("order changed at %d: %q != %q", i, it.ID, before[i])
		}
	}
	// Sanity: the conflicting pair WAS detected (so the test isn't vacuous).
	if !contradictsSet(items[0])["id-2"] || !contradictsSet(items[2])["id-0"] {
		t.Fatalf("expected id-0<->id-2 conflict to be detected, got 0=%v 2=%v",
			items[0].ContradictsIDs, items[2].ContradictsIDs)
	}
	// And the unrelated items were left clean.
	if len(items[1].ContradictsIDs) != 0 || len(items[3].ContradictsIDs) != 0 {
		t.Fatalf("unrelated items should be clean, got 1=%v 3=%v",
			items[1].ContradictsIDs, items[3].ContradictsIDs)
	}
}
