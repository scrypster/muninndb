package activation

import (
	"context"
	"fmt"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/index/fts"
	"github.com/scrypster/muninndb/internal/storage"
)

// ---------------------------------------------------------------------------
// Honest clock, increment 1 (activation-level pins).
//
// Before this increment ACT-R's base-level read an IN-MEMORY cache timestamp
// (EngramLastAccessNs) that any load reset and a restart wiped, so the same
// persisted memory scored like a fresh one in a warm process and was gated out
// in a cold one. The base-level is now the neutral constant bLevelCap for every
// engram: a memory's ACT-R score is content match x confidence (plus
// association boosts), and nothing about age, access history, cache state or
// uptime enters it. Fixtures are invented (trail maintenance).
// ---------------------------------------------------------------------------

// hcScoreOne scores a single candidate through phase6Score with the given
// weights and returns its components. A single candidate keeps the per-query
// max-rescale at 1 whenever Raw <= 1, so Final is the unscaled quantity.
func hcScoreOne(t *testing.T, eng *storage.Engram, cacheNs int64, w *Weights, c fusedCandidate) ScoreComponents {
	t.Helper()
	store := newInternalStubStore()
	store.addEngram(eng)
	if cacheNs != 0 {
		store.lastAccessNs[eng.ID] = cacheNs
	}
	e := newTestActivationEngine(store)
	defer e.Close()
	c.id = eng.ID
	res, err := e.phase6Score(context.Background(), &ActivateRequest{
		MaxResults: 5,
		Threshold:  -1, // score card for every candidate
		Weights:    w,
	}, [8]byte{}, []fusedCandidate{c}, nil, &phase1Result{queryStr: "waterbar"})
	if err != nil {
		t.Fatalf("phase6Score: %v", err)
	}
	if len(res.Activations) != 1 {
		t.Fatalf("got %d activations, want 1", len(res.Activations))
	}
	return res.Activations[0].Components
}

// TestHonestClock_AgeIsNotAnACTRInput: under ACT-R, varying creation age,
// persisted LastAccess, AccessCount and the in-memory cache stamp must not move
// Final or AbsoluteScore by a single bit, for every shipped preset decay and
// across a Hebbian boost grid; and AbsoluteScore is exactly min(CM,1)*conf.
func TestHonestClock_AgeIsNotAnACTRInput(t *testing.T) {
	now := time.Now()
	day := 24 * time.Hour
	type variant struct {
		name        string
		created     time.Duration // age at creation
		lastAccess  time.Duration // age of persisted LastAccess (0 = == created)
		accessCount uint32
		cacheAge    time.Duration // age of the L1 stamp (-1 = no stamp)
	}
	variants := []variant{
		{"fresh never accessed", time.Minute, 0, 0, -1},
		{"400d never accessed cold", 400 * day, 0, 0, -1},
		{"400d never accessed warm stamp", 400 * day, 0, 0, time.Second},
		{"250d one old access", 300 * day, 250 * day, 1, -1},
		{"30d heavily used", 30 * day, 2 * day, 20, -1},
		{"2y heavily used warm", 730 * day, time.Hour, 50, time.Second},
	}
	for _, d := range []float32{0.2, 0.3, 0.5, 0.8} {
		for _, heb := range []float64{0, 0.25, 1.0} {
			var wantFinal, wantAbs float64
			for i, v := range variants {
				created := now.Add(-v.created)
				la := created
				if v.lastAccess != 0 {
					la = now.Add(-v.lastAccess)
				}
				eng := &storage.Engram{
					Concept: "waterbar", Content: "waterbar on switchback four",
					Confidence: 0.9, Stability: 30,
					CreatedAt: created, LastAccess: la, AccessCount: v.accessCount,
				}
				var cacheNs int64
				if v.cacheAge >= 0 {
					cacheNs = now.Add(-v.cacheAge).UnixNano()
				}
				comp := hcScoreOne(t, eng, cacheNs, &Weights{
					UseACTR: true, SemanticSimilarity: 0.6, FullTextRelevance: 0.4, ACTRDecay: d,
				}, fusedCandidate{ftsScore: 0.8, vectorScore: 0.9, hebbianBoost: heb})
				if want := math.Min(comp.ContentMatch, 1) * comp.Confidence; math.Abs(comp.AbsoluteScore-want) > 1e-12 {
					t.Errorf("d=%.1f heb=%.2f %s: AbsoluteScore %.6f, want min(CM,1)*conf = %.6f",
						d, heb, v.name, comp.AbsoluteScore, want)
				}
				if i == 0 {
					wantFinal, wantAbs = comp.Final, comp.AbsoluteScore
					continue
				}
				if comp.Final != wantFinal || comp.AbsoluteScore != wantAbs {
					t.Errorf("d=%.1f heb=%.2f %s: Final %.6f abs %.6f, but %s scored Final %.6f abs %.6f — age/access/cache moved an ACT-R score",
						d, heb, v.name, comp.Final, comp.AbsoluteScore, variants[0].name, wantFinal, wantAbs)
				}
			}
		}
	}
}

// TestHonestClock_ACTRReportsAppliedFactors: the decay/recency/access numbers
// on an ACT-R score card must be the ones scoring applied — 1.0 for the
// (neutral) decay and recency multipliers, 0 for access frequency — never a
// computed-but-unapplied factor. Before this increment a 400-day memory
// reported decay_factor 0.05 on a row whose applied base-level was the cap.
func TestHonestClock_ACTRReportsAppliedFactors(t *testing.T) {
	now := time.Now()
	eng := &storage.Engram{
		Concept: "waterbar", Content: "waterbar on switchback four", Confidence: 1, Stability: 30,
		CreatedAt: now.Add(-400 * 24 * time.Hour), AccessCount: 7,
	}
	comp := hcScoreOne(t, eng, 0, &Weights{UseACTR: true, SemanticSimilarity: 0.6, FullTextRelevance: 0.4},
		fusedCandidate{ftsScore: 1.0, vectorScore: 0.95})
	if comp.DecayFactor != 1 || comp.Recency != 1 || comp.AccessFrequency != 0 {
		t.Errorf("ACT-R score card reports decay_factor=%.4f recency=%.4f access_frequency=%.4f; "+
			"the applied values are 1, 1 and 0 (ACT-R applies no age, recency or access term)",
			comp.DecayFactor, comp.Recency, comp.AccessFrequency)
	}
	// The applied base-level multiplier, measured: Raw/ContentMatch with no boost.
	if m := comp.Raw / comp.ContentMatch; math.Abs(m-1) > 1e-12 {
		t.Errorf("applied ACT-R prior = %.6f, want exactly 1 (neutral base level)", m)
	}
}

// TestHonestClock_WhyNeverCitesDecayUnderACTR: buildWhy must not name a signal
// ACT-R does not apply. With DecayFactor reported as the neutral 1.0 it would
// otherwise win the "dominant signal" contest on almost every row.
func TestHonestClock_WhyNeverCitesDecayUnderACTR(t *testing.T) {
	c := ScoreComponents{SemanticSimilarity: 0.4, FullTextRelevance: 0.3, DecayFactor: 1, Recency: 1, Confidence: 1}
	for i := 0; i < 50; i++ { // map iteration is randomized; sample it
		if why := buildWhy(&storage.Engram{}, c, nil, nil, "waterbar", true); strings.Contains(why, "decay") {
			t.Fatalf("ACT-R why cites decay: %q", why)
		}
	}
}

// TestHonestClock_AccessNeverLessFindableThanNone: a memory read once long ago
// is exactly as findable as its never-read twin — same AbsoluteScore, same
// Final — at every shipped decay. Before this increment the two scored
// differently (access rescued one from the cold cliff); the rejected
// "persisted clock, no floor" variant made the accessed one LESS findable.
func TestHonestClock_AccessNeverLessFindableThanNone(t *testing.T) {
	now := time.Now()
	day := 24 * time.Hour
	for _, d := range []float32{0.2, 0.3, 0.5, 0.8} {
		for _, ageDays := range []int{1, 10, 40, 250, 900} {
			created := now.Add(-time.Duration(ageDays+30) * day)
			never := &storage.Engram{Concept: "c", Content: "c", Confidence: 1, Stability: 30,
				CreatedAt: created, LastAccess: created}
			once := &storage.Engram{Concept: "c", Content: "c", Confidence: 1, Stability: 30,
				CreatedAt: created, LastAccess: now.Add(-time.Duration(ageDays) * day), AccessCount: 1}
			w := &Weights{UseACTR: true, SemanticSimilarity: 0.6, FullTextRelevance: 0.4, ACTRDecay: d}
			c := fusedCandidate{ftsScore: 1.0}
			a := hcScoreOne(t, never, 0, w, c)
			b := hcScoreOne(t, once, 0, w, c)
			if a.AbsoluteScore != b.AbsoluteScore || a.Final != b.Final {
				t.Errorf("d=%.1f access %dd ago: accessed abs %.6f final %.6f vs never-accessed abs %.6f final %.6f — access changed findability",
					d, ageDays, b.AbsoluteScore, b.Final, a.AbsoluteScore, a.Final)
			}
			if b.AbsoluteScore < 0.1 {
				t.Errorf("d=%.1f access %dd ago: lexical-exact match abs %.4f is below the 0.1 gate", d, ageDays, b.AbsoluteScore)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// One tie rule: exact score ties break NEWEST-first (ULID descending), at the
// final sorts and at every candidate cut that precedes them.
// ---------------------------------------------------------------------------

// TestHonestClock_NewestFirstAtTheCandidateCut: N memories with IDENTICAL text
// and different ages, N well above the per-index candidate cap. Every one of
// them scores identically, so the only thing that decides which reach the
// caller is the tie rule at the FTS topK cut and the RecentActive cut — which
// run BEFORE the final sort. The newest must come back, newest first, on every
// fusion path.
func TestHonestClock_NewestFirstAtTheCandidateCut(t *testing.T) {
	dir, err := os.MkdirTemp("", "muninndb-honest-clock-cut-*")
	if err != nil {
		t.Fatal(err)
	}
	db, err := storage.OpenPebble(dir, storage.DefaultOptions())
	if err != nil {
		os.RemoveAll(dir)
		t.Fatal(err)
	}
	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 256})
	t.Cleanup(func() { store.Close(); os.RemoveAll(dir) })
	idx := fts.New(db)
	ws := store.VaultPrefix("honest-clock-cut")
	ctx := context.Background()

	const n, perIndex, limit = 60, 12, 5
	now := time.Now()
	var ids []storage.ULID // oldest first
	for i := 0; i < n; i++ {
		// Ages from 2 hours to n*2 hours: all well inside any decay window
		// that might otherwise decide, so this isolates the TIE rule.
		created := now.Add(-time.Duration(n-i) * 2 * time.Hour)
		eng := &storage.Engram{
			Concept: "waterbar cleared", Content: "the crew cleared the waterbar",
			Confidence: 1, CreatedAt: created, LastAccess: created,
		}
		id, err := store.WriteEngram(ctx, ws, eng)
		if err != nil {
			t.Fatalf("WriteEngram: %v", err)
		}
		if err := idx.IndexEngram(ws, [16]byte(id), eng.Concept, "", eng.Content, nil); err != nil {
			t.Fatalf("IndexEngram: %v", err)
		}
		ids = append(ids, id)
	}
	want := make([]storage.ULID, 0, limit)
	for i := n - 1; i >= n-limit; i-- {
		want = append(want, ids[i])
	}

	act := New(store, NewFTSAdapter(idx), nil, NewNoopEmbedder())
	defer act.Close()
	for name, w := range map[string]*Weights{
		"actr":         {UseACTR: true, SemanticSimilarity: 0.6, FullTextRelevance: 0.4},
		"rrf":          {UseRRFFusion: true, DisableACTR: true, SemanticSimilarity: 0.6, FullTextRelevance: 0.4},
		"weighted_sum": {DisableACTR: true, SemanticSimilarity: 0.6, FullTextRelevance: 0.4},
	} {
		t.Run(name, func(t *testing.T) {
			res, err := act.Run(ctx, &ActivateRequest{
				VaultPrefix: ws, Context: []string{"waterbar cleared"},
				MaxResults: limit, Threshold: -1, CandidatesPerIndex: perIndex,
				Weights: w, ReadOnly: true,
			})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			got := make([]storage.ULID, 0, len(res.Activations))
			for _, a := range res.Activations {
				got = append(got, a.Engram.ID)
			}
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("identical memories, cap %d < n %d: got %v, want the %d NEWEST newest-first %v",
					perIndex, n, hcIndexes(got, ids), limit, hcIndexes(want, ids))
			}
		})
	}

	// The time-bounded pool (created_after) and the RecentActive pool are the
	// only sources for a query no index matches: every in-range memory is an
	// equal member, so the k*3 range cut and the k bucket cut decide alone.
	t.Run("time-filtered pools", func(t *testing.T) {
		res, err := act.Run(ctx, &ActivateRequest{
			VaultPrefix: ws, Context: []string{"zzqx"},
			MaxResults: limit, Threshold: -1, CandidatesPerIndex: perIndex,
			Weights:  &Weights{UseRRFFusion: true, DisableACTR: true},
			ReadOnly: true,
			Filters:  []Filter{{Field: "created_after", Op: "gte", Value: now.Add(-time.Duration(n+1) * 2 * time.Hour)}},
		})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		got := make([]storage.ULID, 0, len(res.Activations))
		for _, a := range res.Activations {
			got = append(got, a.Engram.ID)
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("time-filtered recall, cap %d < n %d: got %v, want the %d NEWEST newest-first %v",
				perIndex, n, hcIndexes(got, ids), limit, hcIndexes(want, ids))
		}
	})
}

// hcIndexes renders ids as their creation index (0 = oldest) for a readable
// failure message.
func hcIndexes(got, all []storage.ULID) []int {
	pos := make(map[storage.ULID]int, len(all))
	for i, id := range all {
		pos[id] = i
	}
	out := make([]int, len(got))
	for i, id := range got {
		if p, ok := pos[id]; ok {
			out[i] = p
		} else {
			out[i] = -1
		}
	}
	return out
}

// TestHonestClock_RRFFusionTieIsNewestFirst: phase 3's fused list feeds the
// Hebbian top-50 and traversal seeds, so its order is a cut too. Exactly tied
// rrf scores (same rank in parallel lists, or one-list members at equal rank
// sums) must order newest-first, independent of map/insertion order.
func TestHonestClock_RRFFusionTieIsNewestFirst(t *testing.T) {
	now := time.Now()
	var a, b, c storage.ULID
	a = storage.NewULIDWithTime(now.Add(-3 * time.Hour)) // oldest
	b = storage.NewULIDWithTime(now.Add(-2 * time.Hour))
	c = storage.NewULIDWithTime(now.Add(-1 * time.Hour)) // newest
	// a and c each sit at rank 0 of a list with the same k, so their rrf
	// scores are exactly equal; b is alone at rank 1.
	sets := &candidateSets{
		time: []storage.ULID{a, b},
		tag:  []storage.ULID{c},
	}
	for run := 0; run < 30; run++ {
		fused := phase3RRF(sets)
		if len(fused) != 3 {
			t.Fatalf("fused %d, want 3", len(fused))
		}
		if fused[0].rrfScore != fused[1].rrfScore {
			t.Fatalf("fixture: top two rrf scores differ (%.8f vs %.8f)", fused[0].rrfScore, fused[1].rrfScore)
		}
		if fused[0].id != c || fused[1].id != a {
			t.Fatalf("tied rrf scores ordered %v, %v; want newest (%v) before oldest (%v)", fused[0].id, fused[1].id, c, a)
		}
	}
}

// TestHonestClock_ReportedFactorsAreAppliedOnEveryFusionPath: decay_factor,
// recency and access_frequency are documented as the factor scoring APPLIED.
// rrf applies none of them, so 0 there read as "fully decayed"; it must report
// the neutral values (1, 1, 0) like ACT-R. CGDN applies only the decay factor.
// weighted_sum applies all three, so it reports the real values.
func TestHonestClock_ReportedFactorsAreAppliedOnEveryFusionPath(t *testing.T) {
	now := time.Now()
	const ageDays, stability = 40.0, 30.0
	mk := func() *storage.Engram {
		at := now.Add(-time.Duration(ageDays*24) * time.Hour)
		return &storage.Engram{Concept: "c", Content: "c", Confidence: 1, Stability: stability,
			CreatedAt: at, LastAccess: at, AccessCount: 5}
	}
	wsDecay := math.Max(0.05, math.Exp(-ageDays/stability))
	wsRecency := math.Exp(-ageDays * math.Ln2 / 7)
	wsAccess := math.Log1p(5) / math.Log1p(100)
	for _, tc := range []struct {
		name                  string
		w                     *Weights
		decay, recency, acces float64
	}{
		{"actr", &Weights{UseACTR: true, SemanticSimilarity: 0.6, FullTextRelevance: 0.4}, 1, 1, 0},
		{"rrf", &Weights{UseRRFFusion: true, DisableACTR: true}, 1, 1, 0},
		{"cgdn", &Weights{UseCGDN: true, DisableACTR: true, SemanticSimilarity: 0.5, FullTextRelevance: 0.3}, wsDecay, 1, 0},
		{"weighted_sum", &Weights{DisableACTR: true, SemanticSimilarity: 0.35, FullTextRelevance: 0.25,
			DecayFactor: 0.2, AccessFrequency: 0.05, Recency: 0.05}, wsDecay, wsRecency, wsAccess},
	} {
		comp := hcScoreOne(t, mk(), 0, tc.w, fusedCandidate{ftsScore: 0.8, vectorScore: 0.9, rrfScore: 0.03})
		if math.Abs(comp.DecayFactor-tc.decay) > 1e-9 || math.Abs(comp.Recency-tc.recency) > 1e-9 || math.Abs(comp.AccessFrequency-tc.acces) > 1e-9 {
			t.Errorf("%s: reported decay=%.4f recency=%.4f access=%.4f, applied are %.4f %.4f %.4f",
				tc.name, comp.DecayFactor, comp.Recency, comp.AccessFrequency, tc.decay, tc.recency, tc.acces)
		}
	}
}
