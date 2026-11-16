package engine

import (
	"context"
	"strings"
	"testing"
)

// TestExplain_AssociationOnlyTargetIsExplained — explain evaluates content
// match. For a memory a default recall returns only in the `associated` block
// (it matched none of the query's words), explain must not be silent: the
// note says it is not a content match, that a non-matching memory can still
// be surfaced in `associated` through a returned row, and the shared-entity
// evidence its own run measured. RED before: would_return false, score 0 and
// an EMPTY note — indistinguishable from "unrelated". (Explain deliberately
// does not check membership in the block: that needs a second recall, over
// its cost bound — see Explain.) Fixtures are invented (trail maintenance).
func TestExplain_AssociationOnlyTargetIsExplained(t *testing.T) {
	c := newCFEnv(t)
	const vault = "trail-explain-assoc"
	f := cfBuild(c, vault)
	resp := c.activate(vault, cfQuery, explainRecallLimit, true, nil)
	if assocIndex(resp.Associated, f.injectee) < 0 {
		t.Fatalf("premise: a default recall carries the injectee in associated: %+v", resp.Associated)
	}
	ex, err := c.eng.Explain(context.Background(), vault, f.injectee.String(), []string{cfQuery}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("explain: scored=%v would_return=%v final=%.3f note=%q", ex.Scored, ex.WouldReturn, ex.FinalScore, ex.Note)
	if ex.WouldReturn {
		t.Errorf("an association-only memory is not a returned match")
	}
	for _, want := range []string{"CONTENT match", "`associated`", "shared-entity evidence (", "may appear there"} {
		if !strings.Contains(ex.Note, want) {
			t.Errorf("explain note for an association-only memory must contain %q: %q", want, ex.Note)
		}
	}
	// A memory with no entity evidence for this query still gets the general
	// statement, never an empty note — and is told it is not expected there.
	ex2, err := c.eng.Explain(context.Background(), vault, f.partner.String(), []string{cfQuery}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("explain partner: scored=%v would_return=%v note=%q", ex2.Scored, ex2.WouldReturn, ex2.Note)
	if ex2.WouldReturn || !strings.Contains(ex2.Note, "`associated`") || !strings.Contains(ex2.Note, "not expected there") {
		t.Errorf("a memory recall would not return, with no entity evidence, needs the general note: %q", ex2.Note)
	}
}
