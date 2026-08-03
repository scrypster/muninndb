package engine

import (
	"context"
	"fmt"
	"testing"

	"time"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/engine/vaultjob"
	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// TestClone_WeightedSumVault_FirstRecallNotSilentlyEmpty is the end-to-end
// regression for #810's severe symptom: on a `weighted_sum` vault, the FIRST
// recall after a clone returned ZERO results, silently.
//
// Mechanism: clone wrote uint64(time.Time{}.UnixNano()) into LastAccess, which
// decodes as 1754-08-30 — not IsZero(), so nothing caught it. computeComponents
// then saw daysSince ~= 740,000, giving recency=0 and decayFactor pinned at its
// 0.05 floor, scoring an otherwise-perfect row 0.42 against COG-6's 0.5
// weighted_sum default threshold.
//
// THE SHAPE OF THIS TEST IS THE TEST. THREE properties are load-bearing and
// must not be "simplified":
//
//   - EXACTLY ONE recall against the clone. The second recall passes either way,
//     because the failed first recall itself populates the L1 cache and
//     computeComponents prefers the cache's lastAccessNs over eng.LastAccess.
//     That self-masking is precisely how this survived to production.
//   - A COLD cache for the target vault. The clone writes engram bytes straight
//     into Pebble, so the target workspace is naturally cold here — do not add a
//     read of the cloned vault before the assertion.
//   - AN AGED, ACTIVELY-USED SOURCE. This one was MISSING, and its absence made
//     the test a fixture artifact for a whole review round: the engrams were
//     written seconds before cloning, which makes "LastAccess := CreatedAt"
//     indistinguishable from "LastAccess := now", so the first fix passed while
//     still destroying the recency of any memory older than a few days. A real
//     vault holds memories created long ago and kept alive by ACCESS — #682's
//     ReinforceOnRead (default true) fires TouchAccess on every read-by-id — and
//     that is the shape the bug survives in. Do not remove the backdated
//     CreatedAt or the TouchAccess; the break points are measured in
//     TestClone_AgedActivelyUsedSource_RecallEquivalent.
//
// Threshold is left at 0 so the engine's COG-6 fusion-aware coerce supplies the
// real production default (0.5 for weighted_sum). Hardcoding a low threshold
// here would make the test pass without the fix.
func TestClone_WeightedSumVault_FirstRecallNotSilentlyEmpty(t *testing.T) {
	eng, as, store, cleanup := testEnvWithAuth(t)
	defer cleanup()
	ctx := context.Background()

	const srcVault = "ws810-source"
	const dstVault = "ws810-clone"

	for _, name := range []string{srcVault, dstVault} {
		if err := as.SetVaultConfig(auth.VaultConfig{
			Name: name, Public: true,
			Plasticity: &auth.PlasticityConfig{ScoringFusion: ptr("weighted_sum")},
		}); err != nil {
			t.Fatalf("SetVaultConfig(%s): %v", name, err)
		}
	}

	// Backdated well past every measured break point (weighted_sum broke at
	// ~45 days of source age when LastAccess was rewound to CreatedAt).
	created := time.Now().Add(-400 * 24 * time.Hour)
	concepts := []string{
		"heron migration routes",
		"heron nesting season",
		"heron feeding behaviour",
	}
	var ids []storage.ULID
	for _, c := range concepts {
		resp, err := eng.Write(ctx, &mbp.WriteRequest{
			Vault:     srcVault,
			Concept:   c,
			Content:   "field notes on " + c,
			Tags:      []string{"birds"},
			CreatedAt: &created,
		})
		if err != nil {
			t.Fatalf("Write %q: %v", c, err)
		}
		id, err := storage.ParseULID(resp.ID)
		if err != nil {
			t.Fatalf("ParseULID(%q): %v", resp.ID, err)
		}
		ids = append(ids, id)
	}
	awaitFTS(t, eng)

	// The source vault is LIVE: reinforcement-on-read keeps LastAccess fresh on
	// memories whose CreatedAt is over a year old.
	srcWS := store.VaultPrefix(srcVault)
	for _, id := range ids {
		if err := store.TouchAccess(ctx, srcWS, id); err != nil {
			t.Fatalf("TouchAccess: %v", err)
		}
	}

	job, err := eng.StartClone(ctx, srcVault, dstVault)
	if err != nil {
		t.Fatalf("StartClone: %v", err)
	}
	finalJob := waitForJob(t, eng, job.ID, 30*time.Second)
	if finalJob.GetStatus() != vaultjob.StatusDone {
		t.Fatalf("clone job status = %q (err: %s)", finalJob.GetStatus(), finalJob.GetErr())
	}
	awaitFTS(t, eng)

	// ---- THE recall: first, only, cold cache, production default threshold ----
	resp, err := eng.Activate(ctx, &mbp.ActivateRequest{
		Vault:      dstVault,
		Context:    []string{"heron"},
		MaxResults: 10,
		// Threshold intentionally 0 => engine applies the weighted_sum default (0.5).
	})
	if err != nil {
		t.Fatalf("Activate on cloned weighted_sum vault: %v", err)
	}
	if len(resp.Activations) == 0 {
		t.Fatalf("first recall on a freshly cloned weighted_sum vault returned ZERO results — the #810 silently-empty recall")
	}

	// Control: the source vault, same fusion mode, same query, must also match.
	// If this is empty the fixture is wrong, not the clone.
	srcResp, err := eng.Activate(ctx, &mbp.ActivateRequest{
		Vault:      srcVault,
		Context:    []string{"heron"},
		MaxResults: 10,
	})
	if err != nil {
		t.Fatalf("Activate on source vault: %v", err)
	}
	if len(srcResp.Activations) == 0 {
		t.Fatal("control: source weighted_sum vault returned zero results — fixture does not exercise the path")
	}
	if len(resp.Activations) != len(srcResp.Activations) {
		t.Errorf("clone returned %d activations, source returned %d — a clone must be recall-equivalent to its source",
			len(resp.Activations), len(srcResp.Activations))
	}
}

// TestClone_AgedActivelyUsedSource_RecallEquivalent sweeps source-vault AGE
// across every fusion mode, because age is the axis the first #810 fix was
// blind to and the axis on which its symptom survived.
//
// What the first fix did: replaced the zero-time sentinel with
// `LastAccess := CreatedAt`. Correct for a newly created engram. Wrong for a
// COPY of an old one — it discards the access recency that reinforcement-on-read
// spends write bandwidth maintaining, and pins the clone back to a creation date
// months or years in the past. IsUnsetTimestamp does not fire (a real year),
// daysSince is large, recency collapses to ~0 and the decay factor sits on its
// floor: the identical arithmetic #810 named as the bug, reached by another
// route.
//
// Measured break points on the first fix (one recall, cold cache, threshold 0):
//
//	DEFAULT (ACT-R)   broke at   7 days of source age
//	weighted_sum      broke at ~45 days (30d still passed)
//	rrf               unaffected at 400 days (rank-based, no recency term)
//
// Note which mode is worse: the DEFAULT breaks SIX TIMES SOONER than
// weighted_sum. #810 and the first fix both presented the silently-empty recall
// as a weighted_sum symptom; that framing was backwards, and this sweep is what
// corrects it.
//
// See the never-accessed control below for what this test is deliberately NOT
// claiming.
func TestClone_AgedActivelyUsedSource_RecallEquivalent(t *testing.T) {
	for _, fusion := range []string{"", "weighted_sum", "rrf"} {
		for _, ageDays := range []int{1, 7, 45, 400} {
			label := fusion
			if label == "" {
				label = "DEFAULT-actr"
			}
			t.Run(fmt.Sprintf("%s/%03dd", label, ageDays), func(t *testing.T) {
				src, clone := agedCloneRecallCounts(t, fusion, ageDays, true)
				if src == 0 {
					t.Fatalf("control: source returned zero at %d days — the fixture stopped exercising the path", ageDays)
				}
				if clone != src {
					t.Fatalf("clone returned %d activations, source returned %d, at %d days of source age (fusion=%q). "+
						"A clone must be recall-equivalent to its source; clone==0 is the silently-empty recall of #810.",
						clone, src, ageDays, fusion)
				}
			})
		}
	}
}

// TestClone_AgedNeverAccessedSource_IsPreExistinglyUnrecallable is the control
// that keeps the claim above honest.
//
// An aged source that was NEVER read scores zero in BOTH vaults at 400 days.
// That is a pre-existing property of recency scoring — a memory written 400 days
// ago and never touched since has genuinely decayed — and it has nothing to do
// with cloning. Without this control the sweep above would look like it measured
// "aged vaults recall badly" rather than "cloning destroyed an actively-used
// vault's recency", and those are different findings with different fixes.
//
// The clone-specific defect requires an ACTIVELY USED source. That is the normal
// case, and it is the only case anyone clones.
func TestClone_AgedNeverAccessedSource_IsPreExistinglyUnrecallable(t *testing.T) {
	src, clone := agedCloneRecallCounts(t, "", 400, false)
	if clone != src {
		t.Errorf("clone=%d source=%d — even in the never-accessed case the clone must match its source", clone, src)
	}
	if src != 0 {
		t.Errorf("source returned %d at 400 days with no access at all, want 0. This test's premise — that an "+
			"aged never-read memory is already unrecallable INDEPENDENT of cloning — no longer holds, so the "+
			"sweep in TestClone_AgedActivelyUsedSource_RecallEquivalent has lost its control. Re-derive both.", src)
	}
}

// agedCloneRecallCounts writes three engrams backdated by ageDays into a source
// vault configured with the given fusion mode, optionally simulates a live vault
// by touching each engram's access time, clones it, and returns
// (source hits, clone hits) for one cold-cache recall against each.
//
// Threshold is left at 0 so the engine's fusion-aware coerce supplies the real
// production default per mode.
func agedCloneRecallCounts(t *testing.T, fusion string, ageDays int, touchSource bool) (srcHits, cloneHits int) {
	t.Helper()
	eng, as, store, cleanup := testEnvWithAuth(t)
	defer cleanup()
	ctx := context.Background()

	suffix := fmt.Sprintf("%d-%v", ageDays, touchSource)
	src := "aged-src-" + suffix
	dst := "aged-dst-" + suffix
	for _, name := range []string{src, dst} {
		if err := as.SetVaultConfig(auth.VaultConfig{
			Name: name, Public: true,
			Plasticity: &auth.PlasticityConfig{ScoringFusion: ptr(fusion)},
		}); err != nil {
			t.Fatalf("SetVaultConfig(%s): %v", name, err)
		}
	}

	created := time.Now().Add(-time.Duration(ageDays) * 24 * time.Hour)
	var ids []storage.ULID
	for _, c := range []string{"heron migration routes", "heron nesting season", "heron feeding behaviour"} {
		resp, err := eng.Write(ctx, &mbp.WriteRequest{
			Vault: src, Concept: c, Content: "field notes on " + c,
			Tags: []string{"birds"}, CreatedAt: &created,
		})
		if err != nil {
			t.Fatalf("Write %q: %v", c, err)
		}
		id, err := storage.ParseULID(resp.ID)
		if err != nil {
			t.Fatalf("ParseULID(%q): %v", resp.ID, err)
		}
		ids = append(ids, id)
	}
	awaitFTS(t, eng)

	if touchSource {
		ws := store.VaultPrefix(src)
		for _, id := range ids {
			if err := store.TouchAccess(ctx, ws, id); err != nil {
				t.Fatalf("TouchAccess: %v", err)
			}
		}
	}

	job, err := eng.StartClone(ctx, src, dst)
	if err != nil {
		t.Fatalf("StartClone: %v", err)
	}
	if fj := waitForJob(t, eng, job.ID, 30*time.Second); fj.GetStatus() != vaultjob.StatusDone {
		t.Fatalf("clone status=%q err=%s", fj.GetStatus(), fj.GetErr())
	}
	awaitFTS(t, eng)

	// The clone recall goes FIRST and exactly once: the target workspace cache
	// must still be cold (see the note on the e2e above).
	cloneResp, err := eng.Activate(ctx, &mbp.ActivateRequest{Vault: dst, Context: []string{"heron"}, MaxResults: 10})
	if err != nil {
		t.Fatalf("Activate clone: %v", err)
	}
	srcResp, err := eng.Activate(ctx, &mbp.ActivateRequest{Vault: src, Context: []string{"heron"}, MaxResults: 10})
	if err != nil {
		t.Fatalf("Activate source: %v", err)
	}
	t.Logf("fusion=%-13q ageDays=%-4d touched=%-5v source=%d clone=%d",
		fusion, ageDays, touchSource, len(srcResp.Activations), len(cloneResp.Activations))
	return len(srcResp.Activations), len(cloneResp.Activations)
}
