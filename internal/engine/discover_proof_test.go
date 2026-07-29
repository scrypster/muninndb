package engine

import (
	"context"
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/transport/mbp"
)

// discoverProofDays is the synthetic vault's window length. 365 in
// production-representative form; kept at 365 here too since the proof's
// four assertions are pinned against numbers computed at this exact T (see
// design doc §3). The null draw count is reduced from the production
// default (500) to keep this integration test CI-cheap; see nullItersForTest.
const discoverProofDays = 365

// nullItersForTest is LARGER than the production default (500), not
// smaller — an honest finding from building this proof. The circular-shift
// null over a T=365-day window has only T-1=364 distinct rotations, so its
// exact permutation p-value floor is ~1/T regardless of draw count. BH-FDR
// applied over m tests requires resolution finer than ~alpha/m to let a
// single true positive survive at q<=threshold: with the proof's m=128
// tests (2 domain-A entities x 32 domain-B entities x 2 lags), the floor
// must clear 1/(N+1) <= 0.05/128 ≈ 0.00039, i.e. N >~ 2560. This is a real
// interaction between (a) BH-FDR over ALL tests (never just survivors —
// principle: no p-hacking) and (b) a finite permutation space, not a
// tuning knob: production vaults with longer windows or fewer tested pairs
// need proportionally fewer draws; this proof's m and T are small enough
// that the draw count must go up to compensate. Documented here rather than
// silently shrunk to make the assertions pass.
const nullItersForTest = 4000

// buildDiscoverProofVault seeds a 365-day two-domain vault through the real
// write path (Engine.Write, per-day valid_from) — not storage pokes. If
// permuteMarketTimestamps is true, the market-domain (domain B) facts' day
// assignment is shuffled independently of the weather domain before ingest,
// which must destroy the planted lag-1 alignment while leaving every count
// (support denominators, entity mention counts, raw co-occurrence) identical
// — this is the RED / non-gameable half (assertion 4).
func buildDiscoverProofVault(t *testing.T, ctx context.Context, eng *Engine, permuteMarketTimestamps bool) {
	t.Helper()
	rng := rand.New(rand.NewSource(42))

	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	day := func(offset int) time.Time { return base.Add(time.Duration(offset) * 24 * time.Hour) }

	write := func(concept string, entityType, entityName string, offset int) {
		t.Helper()
		vf := day(offset)
		// Content must be unique per (entity, day) — identical Concept+Content
		// across writes collapses onto the same engram via the engine's
		// content-dedup path, which would silently merge 90 distinct
		// hot-day events into one and destroy the entire day-bucketed
		// signal this proof depends on. Real ingest naturally varies (each
		// day's fact differs); the date suffix reproduces that here.
		dated := fmt.Sprintf("%s (%s) [%s#%d]", concept, vf.Format("2006-01-02"), entityName, offset)
		req := &mbp.WriteRequest{
			Vault:     "default",
			Concept:   dated,
			Content:   dated,
			ValidFrom: &vf,
			Entities:  []mbp.InlineEntity{{Name: entityName, Type: entityType}},
		}
		if _, err := eng.Write(ctx, req); err != nil {
			t.Fatalf("seed write failed for %s@%d: %v", entityName, offset, err)
		}
	}

	// --- Weather domain (entity type "event") ---
	// hot-day: ~90 of 365 days, chosen deterministically (every ~4th day
	// with jitter) so the planted lag-1 signal has a real, non-trivial
	// marginal.
	hotDays := map[int]bool{}
	for offset := 0; offset < discoverProofDays; offset++ {
		if rng.Float64() < 90.0/365.0 {
			hotDays[offset] = true
		}
	}
	for offset := range hotDays {
		write("temperature above 70 today", "event", "hot-day", offset)
	}

	// daily-weather-summary: the popularity distractor — near-daily,
	// ~350/365 days, uncorrelated with anything except its own frequency.
	for offset := 0; offset < discoverProofDays; offset++ {
		if rng.Float64() < 350.0/365.0 {
			write("daily weather summary", "event", "daily-weather-summary", offset)
		}
	}

	// --- Markets domain (entity type "organization") ---
	// FLWR-rally: planted lag-1 signal. Present on ~80% of (hot-day + 1),
	// plus a 10% base rate on other days. Collected first as a set of
	// offsets, then (optionally) permuted before writing — this is the
	// mechanism the RED half exercises.
	flwrDays := map[int]bool{}
	for hd := range hotDays {
		nextDay := hd + 1
		if nextDay < discoverProofDays && rng.Float64() < 0.80 {
			flwrDays[nextDay] = true
		}
	}
	for offset := 0; offset < discoverProofDays; offset++ {
		if flwrDays[offset] {
			continue
		}
		if rng.Float64() < 0.10 {
			flwrDays[offset] = true
		}
	}

	// market-report: the second half of the popularity-distractor pair —
	// near-daily, ~340/365 days.
	marketReportDays := map[int]bool{}
	for offset := 0; offset < discoverProofDays; offset++ {
		if rng.Float64() < 340.0/365.0 {
			marketReportDays[offset] = true
		}
	}

	// 30 independent null market entities on independent random day-sets
	// (20-120 days each), no relationship to the weather domain whatsoever.
	nullEntityDays := map[string]map[int]bool{}
	for i := 0; i < 30; i++ {
		name := fmt.Sprintf("null-market-%02d", i)
		count := 20 + rng.Intn(101) // [20,120]
		days := map[int]bool{}
		for len(days) < count {
			days[rng.Intn(discoverProofDays)] = true
		}
		nullEntityDays[name] = days
	}

	if permuteMarketTimestamps {
		// Permute every market-domain entity's day-set independently by a
		// random derangement-ish shuffle: reassign each entity's SAME COUNT
		// of events to freshly drawn random days. Same content, same
		// per-entity counts (so marginals n_b are identical to the
		// unpermuted run), destroyed alignment with the weather domain.
		permute := func(days map[int]bool) map[int]bool {
			n := len(days)
			out := map[int]bool{}
			for len(out) < n {
				out[rng.Intn(discoverProofDays)] = true
			}
			return out
		}
		flwrDays = permute(flwrDays)
		marketReportDays = permute(marketReportDays)
		for name, days := range nullEntityDays {
			nullEntityDays[name] = permute(days)
		}
	}

	for offset := range flwrDays {
		write("flower stocks rose", "organization", "FLWR-rally", offset)
	}
	for offset := range marketReportDays {
		write("daily market report", "organization", "market-report", offset)
	}
	for name, days := range nullEntityDays {
		for offset := range days {
			write("independent market entity note", "organization", name, offset)
		}
	}
}

func discoverProofRequest() DiscoverRequest {
	return DiscoverRequest{
		Vault:   "default",
		DomainA: DiscoverDomain{EntityType: "event"},
		DomainB: DiscoverDomain{EntityType: "organization"},
		// Lags 0-1 only (not the production default of 7): the planted
		// signal lives at lag 1, and keeping the tested-pair count m small
		// is what lets a genuine signal clear BH-FDR at this window length
		// with a CI-affordable draw count — see nullItersForTest.
		MaxLagDays: 1,
		MinSupport: 3,
		EntityCap:  200,
		NullIters:  nullItersForTest,
		QThreshold: 0.05,
	}
}

func findCandidate(result *DiscoverResult, a, b string, lag int) *DiscoverCandidate {
	for i := range result.Candidates {
		c := &result.Candidates[i]
		if c.EntityA == a && c.EntityB == b && c.LagDays == lag {
			return c
		}
	}
	return nil
}

func topN(result *DiscoverResult, n int) []DiscoverCandidate {
	if len(result.Candidates) < n {
		n = len(result.Candidates)
	}
	return result.Candidates[:n]
}

// TestDiscover_PlantedSignalProof is the flagship RED-first proof. It seeds
// the design's planted two-domain vault, runs Discover, and checks the four
// hard assertions from the design doc §3. This is the RED test: it is
// expected to FAIL until Engine.Discover / runDiscover exist and are
// statistically correct (a broken null or missing FDR fails assertions
// 2/3; hardcoding or frequency-ranking fails 2 or 4).
func TestDiscover_PlantedSignalProof(t *testing.T) {
	ctx := context.Background()

	// ---- Real (unpermuted) vault: assertions 1, 2, 3 ----
	eng, cleanup := testEnv(t)
	defer cleanup()
	buildDiscoverProofVault(t, ctx, eng, false)

	result, err := eng.Discover(ctx, discoverProofRequest())
	if err != nil {
		t.Fatalf("Discover failed: %v", err)
	}
	if result.Reason != "" {
		t.Fatalf("Discover returned no candidates with reason: %q", result.Reason)
	}
	t.Logf("real vault: tested_pairs=%d window_days=%d dropped_below_support=%d dropped_fdr=%d",
		result.TestedPairs, result.WindowDays, result.DroppedBelowSupport, result.DroppedFDR)
	for i, c := range topN(result, 5) {
		t.Logf("  candidate[%d]: %s -> %s lag=%d support=%d lift=%.3f p=%.4f q=%.4f",
			i, c.EntityA, c.EntityB, c.LagDays, c.Support, c.Lift, c.PValue, c.QValue)
	}

	// Assertion 1: the planted hot-day -> FLWR-rally pair at lag 1 is in the
	// top-3, with approximately correct evidence.
	top3 := topN(result, 3)
	var planted *DiscoverCandidate
	for i := range top3 {
		if top3[i].EntityA == "hot-day" && top3[i].EntityB == "FLWR-rally" && top3[i].LagDays == 1 {
			planted = &top3[i]
		}
	}
	if planted == nil {
		t.Fatalf("ASSERTION 1 FAILED: planted pair hot-day->FLWR-rally@lag1 not in top-3; top-3=%v", top3)
	}
	if planted.Lift < 2.5 || planted.Lift > 4.0 {
		t.Errorf("ASSERTION 1 FAILED: planted pair lift=%.3f, want in [2.5,4.0]", planted.Lift)
	}
	if planted.Support < 60 {
		t.Errorf("ASSERTION 1 FAILED: planted pair support=%d, want >= 60", planted.Support)
	}
	if planted.PValue >= 0.01 {
		t.Errorf("ASSERTION 1 FAILED: planted pair p=%.4f, want < 0.01", planted.PValue)
	}
	if planted.QValue > 0.05 {
		t.Errorf("ASSERTION 1 FAILED: planted pair q=%.4f, want <= 0.05", planted.QValue)
	}

	// Assertion 2: the popularity distractor (daily-weather-summary x
	// market-report), despite having the vault's largest raw co-occurrence,
	// is REJECTED — not present in the surfaced candidates at all.
	for lag := 0; lag <= discoverProofRequest().MaxLagDays; lag++ {
		if c := findCandidate(result, "daily-weather-summary", "market-report", lag); c != nil {
			t.Errorf("ASSERTION 2 FAILED: popularity distractor surfaced at lag=%d: lift=%.3f p=%.4f q=%.4f",
				lag, c.Lift, c.PValue, c.QValue)
		}
	}
	// Independently verify the *reason* it's rejected: compute its raw stats
	// even though it's excluded from the surfaced list, confirming lift~1 and
	// p large (this is what makes the null a genuine gate, not a fluke).
	distractorLift, distractorP := recomputeDayLagStat(t, ctx, eng, "daily-weather-summary", "market-report", 0)
	if distractorLift < 0.85 || distractorLift > 1.15 {
		t.Errorf("ASSERTION 2 FAILED: distractor lift=%.3f, want ~1.0 (frequency artifact, no real signal)", distractorLift)
	}
	if distractorP <= 0.1 {
		t.Errorf("ASSERTION 2 FAILED: distractor p=%.4f, want > 0.1 (shuffling should not change a frequency artifact's score)", distractorP)
	}

	// Assertion 3: zero of the 30 independent null market entities appear at
	// q <= 0.05, anywhere, at any lag.
	nullSurfaced := 0
	for i := 0; i < 30; i++ {
		name := fmt.Sprintf("null-market-%02d", i)
		for _, c := range result.Candidates {
			if c.EntityB == name {
				nullSurfaced++
				t.Logf("ASSERTION 3: null entity %s surfaced: lag=%d lift=%.3f p=%.4f q=%.4f",
					name, c.LagDays, c.Lift, c.PValue, c.QValue)
			}
		}
	}
	if nullSurfaced != 0 {
		t.Errorf("ASSERTION 3 FAILED: %d/30 null market entities surfaced at q<=0.05, want 0", nullSurfaced)
	}

	// ---- Assertion 4 (RED / non-gameable half): rebuild the SAME vault
	// with market-domain timestamps permuted before ingest. The planted
	// pair must collapse: lift ~= 1, p > 0.05, absent from candidates. ----
	eng2, cleanup2 := testEnv(t)
	defer cleanup2()
	buildDiscoverProofVault(t, ctx, eng2, true)

	result2, err := eng2.Discover(ctx, discoverProofRequest())
	if err != nil {
		t.Fatalf("Discover (shuffled) failed: %v", err)
	}
	if c := findCandidate(result2, "hot-day", "FLWR-rally", 1); c != nil {
		t.Errorf("ASSERTION 4 FAILED: planted pair still surfaced after timestamp permutation: lift=%.3f p=%.4f q=%.4f",
			c.Lift, c.PValue, c.QValue)
	}
	shuffledLift, shuffledP := recomputeDayLagStat(t, ctx, eng2, "hot-day", "FLWR-rally", 1)
	t.Logf("shuffled vault: hot-day->FLWR-rally@lag1 lift=%.3f p=%.4f", shuffledLift, shuffledP)
	if shuffledLift < 0.7 || shuffledLift > 1.3 {
		t.Errorf("ASSERTION 4 FAILED: shuffled planted pair lift=%.3f, want ~1.0 +/- 0.3 (signal must live only in the timestamps)", shuffledLift)
	}
	if shuffledP <= 0.05 {
		t.Errorf("ASSERTION 4 FAILED: shuffled planted pair p=%.4f, want > 0.05", shuffledP)
	}
}

// recomputeDayLagStat runs Discover with a permissive min_support=1 and pulls
// out the raw lift/p for a specific (a,b,lag) triple even when it would not
// clear the default support floor or FDR gate — used to independently verify
// *why* a pair is excluded, not just that it is excluded.
func recomputeDayLagStat(t *testing.T, ctx context.Context, eng *Engine, a, b string, lag int) (lift, p float64) {
	t.Helper()
	req := discoverProofRequest()
	req.MinSupport = 1
	req.QThreshold = 1.0 // don't let FDR hide it from the raw-stat probe
	result, err := eng.Discover(ctx, req)
	if err != nil {
		t.Fatalf("recomputeDayLagStat: Discover failed: %v", err)
	}
	if c := findCandidate(result, a, b, lag); c != nil {
		return c.Lift, c.PValue
	}
	t.Fatalf("recomputeDayLagStat: pair %s->%s@lag%d not found even at min_support=1/q=1.0 — check entity/domain wiring", a, b, lag)
	return 0, 0
}
