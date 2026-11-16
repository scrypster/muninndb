package engine

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"testing"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/transport/mbp"
	"github.com/stretchr/testify/require"
)

// mode="recent" is an ALIAS of balanced (recent-mode orientation design v2,
// D1/D2): the preset is empty, so it scores exactly like the engine defaults
// on every fusion mode — same IDs, order, scores, bands and abstention. Like
// every WIRE mode it REPLACES the vault's default recall mode, so it equals a
// no-mode recall only on a vault whose default mode is balanced (or unset);
// on a semantic- or deep-default vault it equals wire mode=balanced, not the
// no-mode call (the wire_recent_ignores_vault_default arm). It used to build a zero-base {Sem 0.3, FTS 0, Recency 0.7}
// vector whose Recency the ACT-R formula never reads, raise the bar to 0.2,
// drop the vault's ACTRDecay/HebScale, and flip a weighted_sum vault to ACT-R
// scoring — so a topical query that returns 10 rows in default mode returned
// 0 (below_threshold) in recent mode.
//
// The fixture is writeRRFThresholdFixture (10 lexically relevant engrams + 10
// unrelated ones, no embedder). Every call is ReadOnly so no learning or
// activation-log write can move scores between the compared calls.

// recallQueries: two topical (one strong, one weak lexical match whose
// absolute score sits between the 0.1 default bar and the old 0.2 recent
// bar), one nonsense, one content-free orientation phrase.
var recallQueries = []string{
	"thermal calibration widget",
	"device alignment",
	"zxqv blorptangle quuxified",
	"session start",
}

// recallRow is the comparable projection of one result row. ID is omitted
// when comparing across two vaults (different ULIDs, same write order).
// Floats holds Score followed by every ScoreComponents field.
type recallRow struct {
	ID, Concept, Band, Basis string
	HopPath                  []string
	Floats                   []float32
}

type recallOutcome struct {
	Rows             []recallRow
	TotalFound       int
	Abstained        bool
	AbstainedReason  string
	SemanticDegraded bool
}

func recallOutcomeOf(t *testing.T, eng *Engine, vault, query, mode string, withIDs bool) recallOutcome {
	t.Helper()
	resp, err := eng.Activate(context.Background(), &mbp.ActivateRequest{
		Vault:      vault,
		Context:    []string{query},
		MaxResults: 10,
		Mode:       mode,
		ReadOnly:   true,
	})
	require.NoError(t, err)
	out := recallOutcome{
		TotalFound:       resp.TotalFound,
		Abstained:        resp.Abstained,
		AbstainedReason:  resp.AbstainedReason,
		SemanticDegraded: resp.SemanticDegraded,
	}
	for _, a := range resp.Activations {
		r := recallRow{Concept: a.Concept, Band: a.RelevanceBand, Basis: a.RelevanceBandBasis, HopPath: a.HopPath}
		if withIDs {
			r.ID = a.ID
		}
		r.Floats = append(r.Floats, a.Score)
		cv := reflect.ValueOf(a.ScoreComponents)
		for i := 0; i < cv.NumField(); i++ {
			r.Floats = append(r.Floats, float32(cv.Field(i).Float()))
		}
		out.Rows = append(out.Rows, r)
	}
	return out
}

// wallClockScoreTolerance bounds how far a score may move between two
// back-to-back recalls of the SAME plan. The activation scorer reads
// time.Now() directly (no clock seam): the legacy recency/decay components
// drift by at most ln2/(7 days) ≈ 1.15e-6 per second of wall clock, which
// on a weighted_sum vault reaches Score and can flip a float32 ULP between
// two calls. 1e-5 covers ~9 s between the compared calls — far longer than
// a ReadOnly recall of 20 engrams takes even under -race — while staying
// four orders of magnitude below any preset effect (the bug this pins moved
// a row from 0.4 to absent). IDs, order, bands, abstention and counts are
// compared EXACTLY; ordering is unaffected by the drift because every row
// ages at the same rate and the formula is monotone in age. The exact
// plan-level equality (recent == balanced, reflect.DeepEqual) is
// TestRecallModeCensus R5, which has no clock in it.
const wallClockScoreTolerance = 1e-5

func requireSameOutcome(t *testing.T, want, got recallOutcome, format string, args ...any) {
	t.Helper()
	msg := fmt.Sprintf(format, args...)
	summary := fmt.Sprintf("%s\nwant: %d rows abstained=%v/%q total=%d\ngot:  %d rows abstained=%v/%q total=%d",
		msg, len(want.Rows), want.Abstained, want.AbstainedReason, want.TotalFound,
		len(got.Rows), got.Abstained, got.AbstainedReason, got.TotalFound)
	require.Equal(t, want.Abstained, got.Abstained, summary)
	require.Equal(t, want.AbstainedReason, got.AbstainedReason, summary)
	require.Equal(t, want.TotalFound, got.TotalFound, summary)
	require.Equal(t, want.SemanticDegraded, got.SemanticDegraded, summary)
	require.Len(t, got.Rows, len(want.Rows), summary)
	for i := range want.Rows {
		w, g := want.Rows[i], got.Rows[i]
		require.Equal(t, [4]string{w.ID, w.Concept, w.Band, w.Basis}, [4]string{g.ID, g.Concept, g.Band, g.Basis}, "%s (row %d)", msg, i)
		require.Equal(t, w.HopPath, g.HopPath, "%s (row %d hop path)", msg, i)
		require.Len(t, g.Floats, len(w.Floats))
		for j := range w.Floats {
			require.InDelta(t, w.Floats[j], g.Floats[j], wallClockScoreTolerance, "%s (row %d, float %d: 0=Score, then ScoreComponents fields in order)", msg, i, j)
		}
	}
}

func setupAliasVault(t *testing.T, eng *Engine, as *auth.Store, vault string, p *auth.PlasticityConfig) {
	t.Helper()
	require.NoError(t, as.SetVaultConfig(auth.VaultConfig{Name: vault, Public: true, Plasticity: p}))
	writeRRFThresholdFixture(t, eng, context.Background(), vault)
}

func TestActivate_RecentMode_EqualsDefault(t *testing.T) {
	eng, as, _, cleanup := testEnvWithAuth(t)
	defer cleanup()

	// ACT-R vault (the default fusion). On develop the topical queries
	// returned 10 rows in default mode and 0 in recent mode.
	t.Run("actr", func(t *testing.T) {
		setupAliasVault(t, eng, as, "alias-actr", nil)
		for _, q := range recallQueries {
			def := recallOutcomeOf(t, eng, "alias-actr", q, "", true)
			rec := recallOutcomeOf(t, eng, "alias-actr", q, "recent", true)
			requireSameOutcome(t, def, rec, "ACT-R vault, query %q: mode=recent must equal the default "+
				"(default %d rows, recent %d rows)", q, len(def.Rows), len(rec.Rows))
		}
	})

	// weighted_sum vault. On develop recent flipped scoring to ACT-R and
	// abstained. The alias now inherits the weighted_sum DEFAULT's behaviour,
	// INCLUDING its pre-existing admission of the nonsense and content-free
	// queries at ~0.7 against its 0.5 bar (follow-up F4, a property of the
	// legacy blended formula, not of the preset). This arm pins "equal to
	// default"; it does not endorse what the default returns.
	t.Run("weighted_sum", func(t *testing.T) {
		setupAliasVault(t, eng, as, "alias-ws", &auth.PlasticityConfig{ScoringFusion: ptr("weighted_sum")})
		for _, q := range recallQueries {
			def := recallOutcomeOf(t, eng, "alias-ws", q, "", true)
			rec := recallOutcomeOf(t, eng, "alias-ws", q, "recent", true)
			requireSameOutcome(t, def, rec, "weighted_sum vault, query %q: mode=recent must equal the default "+
				"(default %d rows, recent %d rows)", q, len(def.Rows), len(rec.Rows))
		}
	})

	// A vault whose DEFAULT RecallMode is "recent" (no wire mode) must behave
	// exactly like a vault whose default is "balanced". On develop the
	// vault-default path kept the resolved weights but raised the bar to 0.2,
	// so the weak lexical match (absolute ~0.167) was dropped.
	t.Run("vault_default_recent", func(t *testing.T) {
		setupAliasVault(t, eng, as, "alias-vrec", &auth.PlasticityConfig{RecallMode: ptr("recent")})
		setupAliasVault(t, eng, as, "alias-vbal", &auth.PlasticityConfig{RecallMode: ptr("balanced")})
		for _, q := range recallQueries {
			bal := recallOutcomeOf(t, eng, "alias-vbal", q, "", false)
			rec := recallOutcomeOf(t, eng, "alias-vrec", q, "", false)
			requireSameOutcome(t, bal, rec, "vault default RecallMode=recent, query %q: must equal a balanced-default vault "+
				"(balanced %d rows, recent %d rows)", q, len(bal.Rows), len(rec.Rows))
		}
	})

	// A wire mode replaces the vault's default recall mode. On an ACT-R
	// vault whose DEFAULT mode is semantic, the no-mode recall admits
	// nonsense (F1, reproduced) while wire recent and wire balanced both
	// abstain — so "recent equals the vault default" would be false, and
	// "recent equals balanced" is the true statement.
	t.Run("wire_recent_ignores_vault_default", func(t *testing.T) {
		for _, vm := range []string{"semantic", "deep"} {
			v := "alias-wire-over-" + vm
			setupAliasVault(t, eng, as, v, &auth.PlasticityConfig{RecallMode: ptr(vm)})
			for _, q := range recallQueries {
				bal := recallOutcomeOf(t, eng, v, q, "balanced", true)
				rec := recallOutcomeOf(t, eng, v, q, "recent", true)
				requireSameOutcome(t, bal, rec, "%s-default vault, query %q: wire recent must equal wire balanced", vm, q)
			}
		}
		noMode := recallOutcomeOf(t, eng, "alias-wire-over-semantic", recallQueries[2], "", true)
		rec := recallOutcomeOf(t, eng, "alias-wire-over-semantic", recallQueries[2], "recent", true)
		require.NotEmpty(t, noMode.Rows, "precondition (F1): the semantic-default vault's no-mode recall admits nonsense")
		require.True(t, rec.Abstained, "wire recent on a semantic-default vault scores like balanced and abstains on nonsense")
	})

	// rrf vault: CONTROL — passes before and after, because the rrf branch
	// already ignored the preset scalars and the preset threshold abstains
	// under rrf. Exact row equality is NOT asserted here: rrf ranking is not
	// deterministic across two IDENTICAL calls on this fixture (measured on
	// develop: the same no-mode query, run 4 times, returned 4 different
	// orders with different scores). That is pre-existing and separate from
	// this increment; the control therefore compares abstention, row count,
	// and — for the strong topical query, whose relevant set is unambiguous —
	// the returned ID set.
	t.Run("rrf_control", func(t *testing.T) {
		setupAliasVault(t, eng, as, "alias-rrf", &auth.PlasticityConfig{ScoringFusion: ptr("rrf")})
		for _, q := range recallQueries {
			def := recallOutcomeOf(t, eng, "alias-rrf", q, "", true)
			rec := recallOutcomeOf(t, eng, "alias-rrf", q, "recent", true)
			require.Equalf(t, def.Abstained, rec.Abstained, "rrf vault, query %q: abstention", q)
			require.Equalf(t, len(def.Rows), len(rec.Rows), "rrf vault, query %q: row count", q)
			if q == recallQueries[0] {
				require.Equalf(t, sortedIDs(def), sortedIDs(rec), "rrf vault, query %q: ID set", q)
			}
		}
	})
}

func sortedIDs(o recallOutcome) []string {
	ids := make([]string, 0, len(o.Rows))
	for _, r := range o.Rows {
		ids = append(ids, r.ID)
	}
	sort.Strings(ids)
	return ids
}

// TestActivate_RecallModeAbstentionControls (design v2 §3.3, T3) pins that
// the nonsense and content-free queries keep the abstention outcome they had
// on develop in every mode, so the alias cannot be mistaken for — or hide — a
// regression in the other modes. Passes before AND after the alias change,
// except the weighted_sum `recent` cell, which changes from abstaining to
// "whatever the weighted_sum default does" (D2) and is asserted as equality
// with default rather than as a fixed outcome.
func TestActivate_RecallModeAbstentionControls(t *testing.T) {
	eng, as, _, cleanup := testEnvWithAuth(t)
	defer cleanup()

	contentFree := recallQueries[2:] // nonsense, content-free
	modes := []string{"", "balanced", "semantic", "deep"}

	t.Run("actr_all_abstain", func(t *testing.T) {
		setupAliasVault(t, eng, as, "ctl-actr", nil)
		for _, q := range contentFree {
			for _, m := range append(modes, "recent") {
				o := recallOutcomeOf(t, eng, "ctl-actr", q, m, true)
				require.Truef(t, o.Abstained, "ACT-R vault, query %q, mode %q: must abstain", q, m)
				require.Equalf(t, "below_threshold", o.AbstainedReason, "ACT-R vault, query %q, mode %q", q, m)
				require.Emptyf(t, o.Rows, "ACT-R vault, query %q, mode %q", q, m)
			}
		}
	})

	// weighted_sum: the develop outcomes, recorded. "", balanced and deep
	// admit content-free rows at ~0.7 (F4 — the legacy blended formula;
	// when F4 is fixed these expectations move with it); semantic abstains
	// (zero-base vector, no decay/recency terms).
	t.Run("weighted_sum_unchanged", func(t *testing.T) {
		setupAliasVault(t, eng, as, "ctl-ws", &auth.PlasticityConfig{ScoringFusion: ptr("weighted_sum")})
		wantAbstain := map[string]bool{"": false, "balanced": false, "deep": false, "semantic": true}
		for _, q := range contentFree {
			for _, m := range modes {
				o := recallOutcomeOf(t, eng, "ctl-ws", q, m, true)
				require.Equalf(t, wantAbstain[m], o.Abstained, "weighted_sum vault, query %q, mode %q: abstention changed from develop", q, m)
			}
			def := recallOutcomeOf(t, eng, "ctl-ws", q, "", true)
			rec := recallOutcomeOf(t, eng, "ctl-ws", q, "recent", true)
			requireSameOutcome(t, def, rec, "weighted_sum vault, query %q: recent must equal default (D2)", q)
		}
	})

	t.Run("vault_default_recent_equals_balanced", func(t *testing.T) {
		setupAliasVault(t, eng, as, "ctl-vrec", &auth.PlasticityConfig{RecallMode: ptr("recent")})
		setupAliasVault(t, eng, as, "ctl-vbal", &auth.PlasticityConfig{RecallMode: ptr("balanced")})
		for _, q := range contentFree {
			for _, m := range append(modes, "recent") {
				rec := recallOutcomeOf(t, eng, "ctl-vrec", q, m, false)
				bal := recallOutcomeOf(t, eng, "ctl-vbal", q, m, false)
				requireSameOutcome(t, bal, rec, "query %q, wire mode %q: recent-default vault must equal balanced-default vault", q, m)
				require.Truef(t, rec.Abstained, "query %q, wire mode %q: ACT-R content-free must abstain", q, m)
			}
		}
	})
}
