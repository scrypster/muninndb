package engine

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// TestExplain_DiagnosticActivationDoesNoListScaledWork — the default-run
// guard for explain's cost (5ee397d, a6e32c9). Explain's diagnostic
// activation scores every candidate (threshold -1, 1,000 rows) to find ONE
// memory's card, so three phases must not scale with that list (the third:
// the supersession and contradiction windows examine no more rows than a
// default recall's):
//
//   - the COG-25 currency pass (pairwise over the examined rows) must not run
//     at all — it changes no score;
//   - the entity boost must seed and inject exactly as a default recall does,
//     never admit every memory sharing any seed entity (threshold -1).
//
// The work is COUNTED, never timed: the opt-in ratio test is the only timing
// measure, and a wall-clock bound here would flake on a loaded machine. The
// premise run (the same request without the diagnostic marker) proves the
// fixture would expose both regressions.
func TestExplain_DiagnosticActivationDoesNoListScaledWork(t *testing.T) {
	c := newCFEnv(t)
	const vault = "trail-explain-guard"
	f := cfBuild(c, vault)
	ctx := context.Background()
	ws := c.store.ResolveVaultPrefix(vault)
	// Twenty memories that share one moderately common place with the top
	// match and none of the query's words: their entity evidence is above the
	// noise floor but below a default recall's threshold, so only a
	// threshold -1 boost would inject them.
	for i := 0; i < 20; i++ {
		c.write(vault, fmt.Sprintf("ford crossing log %d", i), "stepping stones at the ford were reset", time.Duration(70+i)*time.Hour, "Brindle Ford")
	}
	if err := c.store.WriteEntityEngramLink(ctx, ws, f.strong[0], "Brindle Ford"); err != nil {
		t.Fatal(err)
	}
	// One declared contradiction, so the contradiction phase runs at all
	// (it is skipped on a vault with none).
	if _, err := c.eng.Link(ctx, &mbp.LinkRequest{Vault: vault, SourceID: f.middle[9].String(), TargetID: f.middle[10].String(),
		RelType: uint16(storage.RelContradicts)}); err != nil {
		t.Fatal(err)
	}
	// Past 1,000 memories the candidate pool is ~sqrt(N) per index, so the
	// ford memories are outside it and can only arrive by injection. The
	// filler is unindexed and newer, so it fills the recency lane.
	_ = c.store.GetVaultCount(ctx, ws)
	filler := make([]storage.EngramBatchItem, 1050)
	for i := range filler {
		at := time.Now().Add(-time.Duration(len(filler)-i) * time.Second)
		filler[i] = storage.EngramBatchItem{WSPrefix: ws, Engram: &storage.Engram{Concept: fmt.Sprintf("boardwalk survey %d", i),
			Content: "puncheon planks counted", Confidence: 1, CreatedAt: at, UpdatedAt: at, LastAccess: at}}
	}
	if _, errs := c.store.WriteEngramBatch(ctx, filler); errs != nil {
		for _, err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	run := func(cctx context.Context, req *mbp.ActivateRequest) *recallCostCounter {
		t.Helper()
		var cnt recallCostCounter
		if _, err := c.eng.Activate(withRecallCostCounter(cctx, &cnt), req); err != nil {
			t.Fatal(err)
		}
		return &cnt
	}
	diagReq := func() *mbp.ActivateRequest {
		return &mbp.ActivateRequest{Vault: vault, Context: []string{cfQuery}, MaxResults: explainMaxCandidates, Threshold: -1, ExcludeAssociated: true, ReadOnly: true}
	}
	def := run(ctx, &mbp.ActivateRequest{Vault: vault, Context: []string{cfQuery}, MaxResults: explainRecallLimit, ReadOnly: true})
	undiag := run(ctx, diagReq())
	diag := run(withExplainTarget(ctx, f.middle[3]), diagReq())
	t.Logf("default recall: pairs=%d injectReads=%d; threshold -1 without the diagnostic path: pairs=%d injectReads=%d; diagnostic: pairs=%d injectReads=%d",
		def.currencyPairs.Load(), def.boostInjectReads.Load(), undiag.currencyPairs.Load(), undiag.boostInjectReads.Load(),
		diag.currencyPairs.Load(), diag.boostInjectReads.Load())
	t.Logf("windows (supersession/contradiction): default %d/%d, threshold -1 without the diagnostic path %d/%d, diagnostic %d/%d",
		def.supersessionWindow.Load(), def.contradictionWindow.Load(), undiag.supersessionWindow.Load(), undiag.contradictionWindow.Load(),
		diag.supersessionWindow.Load(), diag.contradictionWindow.Load())
	if undiag.currencyPairs.Load() <= def.currencyPairs.Load() || undiag.boostInjectReads.Load() <= def.boostInjectReads.Load() ||
		undiag.supersessionWindow.Load() <= int64(explainRecallLimit+supersessionMargin) ||
		undiag.contradictionWindow.Load() <= int64(explainRecallLimit+contradictionMargin) ||
		def.contradictionWindow.Load() == 0 {
		t.Fatalf("premise: a threshold -1 activation without the diagnostic path must do more currency pairs, injection reads and window rows than a default recall, and the contradiction phase must run")
	}
	// A default recall examines at most limit+margin rows in each window
	// (here it has only 20 gated rows, so it examines 20); explain's
	// threshold -1 list is longer, so the bound is the default recall's
	// CAP, not its count on this query.
	if got, want := diag.supersessionWindow.Load(), int64(explainRecallLimit+supersessionMargin); got > want {
		t.Errorf("explain's diagnostic activation examined %d rows in the supersession window; a default recall examines at most %d", got, want)
	}
	if got, want := diag.contradictionWindow.Load(), int64(explainRecallLimit+contradictionMargin); got > want {
		t.Errorf("explain's diagnostic activation examined %d rows in the contradiction window; a default recall examines at most %d", got, want)
	}
	if n := diag.currencyPairs.Load(); n != 0 {
		t.Errorf("explain's diagnostic activation ran the currency pass over %d row pairs; it must skip it (no score changes)", n)
	}
	if got, want := diag.boostInjectReads.Load(), def.boostInjectReads.Load(); got > want {
		t.Errorf("explain's diagnostic activation read %d entity-boost injection candidates; a default recall reads %d — the boost must inject as a default recall does", got, want)
	}
}
