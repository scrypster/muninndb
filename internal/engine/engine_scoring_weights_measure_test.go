//go:build scoringmeasure

package engine

// READ-ONLY REAL-VAULT DRIVER for the ContentMatch-combiner measurement.
//
// BUILD TAG: `scoringmeasure`. CI builds and tests with `-tags localassets`
// (and `localassets,integration` for the cmd suite) and never with this tag, so
// this file does not compile — let alone run — in any CI job. It is a tool the
// owner runs by hand against a CLONE of a real vault.
//
// THE QUESTION IT SETTLES
//
// MuninnDB's live Phase 6 ContentMatch is LINEAR with hardcoded coefficients:
//
//	ContentMatch = 0.6*semCal + 0.4*ftsCoverage      (activation/engine.go:2211)
//
// That formula charges a MISSING estimator as evidence of irrelevance. A pure
// paraphrase — no lexical overlap with the query, which is the exact case
// retrieval-by-meaning exists to serve — is structurally capped at 0.6, no
// matter how perfect the semantic match. The coefficients are described in the
// source as "proven optimal ... hardcoded proven values" (engine.go:2354-2356)
// but landed inside a large omnibus commit (e51b985) with no recorded
// derivation and no corpus named. Principle #11 forbids exactly that.
//
// This driver is the measurement that must precede any change. Over a set of
// queries and a candidate pool it scores every candidate under four combiners
// and pre-commits to TWO metrics that pull in opposite directions, so no arm
// can win by sacrificing the other:
//
//	NDCG@5 on ANSWERABLE queries    — did the right memory rank high?
//	FPR    on UNANSWERABLE queries  — did the system abstain when it should?
//
// Any combiner more permissive than the live one (noisy-OR and max both are,
// everywhere) necessarily improves the first and worsens the second. A result
// is only interesting if the first improves MORE than the second worsens.
//
// THE ARMS
//
//	LINEAR    ContentMatch = 0.6*semCal + 0.4*fts        — the live formula
//	COSINE    ContentMatch = raw cosine, no prior        — the naive control
//	NOISY-OR  ContentMatch = 1-(1-semCal)(1-fts)         — absence-of-evidence
//	MAX       ContentMatch = max(semCal, fts)            — absence-of-evidence
//
// Each is run TWICE: once with the live Confidence multiplier, and once with
// CONFIDENCE ABLATED TO 1.0. The ablation is mandatory, not optional: a live
// contradiction bug can collapse an engram's Confidence to ~0.07, which alone
// suppresses it by 93% (Confidence multiplies straight into the final score,
// activation/engine.go:2295). Without the ablation the bug could silently
// decide which combiner "wins". If the conf-on and conf-off rankings of the
// arms disagree, the measurement is contaminated and NO conclusion may be drawn
// from the conf-on numbers.
//
// TWO CONDITIONS
//
//	FULL-SIGNAL  candidates carry their real FTS coverage for the query.
//	PARAPHRASE   FTS is forced to 0 for every candidate, simulating a query
//	             with zero lexical overlap. This is the condition the 0.6 cap
//	             is about, and no paraphrase corpus is needed to produce it.
//
// TWO THRESHOLDS — and this one is not cosmetic.
//
//	engine .05   activation/engine.go:547, what a library/direct caller gets.
//	SURFACE .5   mcp/handlers.go:392 and rest/server.go:1772, what every real
//	             agent gets when it omits `threshold`. TEN TIMES the engine's.
//
// The surface default is the gate that matters, and it exposes the MISSING-
// COSINE case: a memory with no embedding yet scores cosine 0, so ContentMatch
// = 0.4*fts <= 0.4 < 0.5, and is UNRETURNABLE through MCP or REST at defaults
// no matter how perfect the lexical match. That is the same defect as the
// paraphrase cap from the opposite direction — a missing estimator scored as a
// zero rather than as absence of evidence — and it is pinned in
// engine_scoring_weights_model_test.go. Measuring only at 0.05 would report
// numbers no agent ever sees.
//
// THE INDEXING-LAG CONFOUND (third in the list, after confidence collapse and
// the 0.6 cap). A corpus that is still being embedded contaminates everything:
// unembedded engrams are invisible to the full pipeline and unscoreable by the
// raw-cosine control, so the pipeline looks artificially bad and the control
// artificially good. This driver PROVES settlement rather than assuming it —
// it counts active engrams with no stored embedding, reports the fraction, and
// REFUSES TO RUN above 1% (override: MUNINN_SCOREW_MAX_UNEMBEDDED_FRAC).
//
// Exposure, for reference: the retroactive embed processor polls every 3s and
// backs off exponentially to a 3-MINUTE ceiling when idle
// (internal/plugin/retroactive.go:25,36,188-192). Its notifyCh wake path exists
// but NO WRITE PATH CALLS Notify() — the only caller is the processor itself
// when it hits maxBatchSize (retroactive.go:444). So on a vault that has been
// quiet for ~3 minutes, a newly-written memory can wait up to another ~3
// minutes for its embedding, not the ~3s seen on a busy vault.
//
// LABELS ARE MECHANICAL AND ASSIGNED BEFORE SCORING
//
//	answerable   query text = a sampled engram's own summary; the gold
//	             document is that engram, by identity.
//	unanswerable query text = a committed list of SYNTHETIC nonsense strings
//	             authored in this file. Nothing in the vault answers them, so
//	             any returned result above the relevance threshold is a false
//	             positive.
//
// No arm sees the label, the label cannot be influenced by the score, and the
// same labelled query set is scored by every arm — that is the sense in which
// this is blind. It is NOT human relevance judgment, and it does not claim to
// be: "gold = the engram the query text was drawn from" is a proxy that
// rewards finding the source document, which for the FULL-SIGNAL condition is
// an easy target. The PARAPHRASE condition is the discriminating one.
//
// PRIVACY: this driver prints AGGREGATE STATISTICS ONLY. It never prints an
// engram's content, concept, summary, tags, entities, ID, or the vault name,
// and it never writes a file. Every string literal in this file is synthetic.
//
// FIDELITY AND ITS LIMITS — read before believing a number:
//
//   - The pool is the union of the stored-embedding cosine neighborhood and
//     the FTS top-N. It is not the output of engine.Recall: no entity boost,
//     no tag seeding, no associative traversal, no PAS transitions, no
//     currency annotation, no time filters. Every arm sees the same pool, so
//     the COMPARISON is fair; the ABSOLUTE numbers are not "what recall
//     returns".
//   - Unembedded engrams are EXCLUDED from the pool (they cannot be scored by
//     the raw-cosine control at all), and their count is reported. So the
//     driver measures the combiners on a settled corpus; it does NOT measure
//     the missing-cosine case itself — that one is pinned analytically in
//     engine_scoring_weights_model_test.go, where it is exact.
//   - Hebbian and PAS-transition boosts are 0 in every arm. Those are the
//     cognition layer's strongest arguments for itself and they are absent, so
//     the driver's bias runs AGAINST the parts of the layer it cannot see.
//     Say so when quoting a result.
//   - The scorer is a replication (engine_scoring_weights_model_test.go), not
//     the live function. See that file's header for why, and for the pins.
//   - Queries are engram summaries, whose cosine distribution runs higher than
//     real user queries. Read absolute NDCG with that in mind; read the
//     DIFFERENCES between arms as the result.
//
// USAGE:
//
//	MUNINN_SCOREW_DATA_DIR=<clone dir containing pebble/> \
//	MUNINN_SCOREW_VAULT=<vault name> \
//	go test -tags 'localassets scoringmeasure' -run TestMeasureContentMatchCombiners \
//	    -timeout 60m -v ./internal/engine/
//
// Optional: MUNINN_SCOREW_QUERIES (default 100), MUNINN_SCOREW_POOL (50),
// MUNINN_SCOREW_MAX_ENGRAMS (5000), MUNINN_SCOREW_SEED (1),
// MUNINN_SCOREW_BASELINE (0.520), MUNINN_SCOREW_MIN_CONFIDENCE (0 = keep all),
// MUNINN_SCOREW_CONF_SUSPECT (0.5), MUNINN_SCOREW_MAX_UNEMBEDDED_FRAC (0.01).

import (
	"context"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/index/fts"
	"github.com/scrypster/muninndb/internal/plugin/embed"
	"github.com/scrypster/muninndb/internal/storage"
)

const (
	envSCWDataDir  = "MUNINN_SCOREW_DATA_DIR"
	envSCWVault    = "MUNINN_SCOREW_VAULT"
	envSCWQueries  = "MUNINN_SCOREW_QUERIES"
	envSCWPool     = "MUNINN_SCOREW_POOL"
	envSCWMaxEng   = "MUNINN_SCOREW_MAX_ENGRAMS"
	envSCWSeed     = "MUNINN_SCOREW_SEED"
	envSCWBaseline = "MUNINN_SCOREW_BASELINE"
	envSCWMinConf  = "MUNINN_SCOREW_MIN_CONFIDENCE"
	envSCWSuspect  = "MUNINN_SCOREW_CONF_SUSPECT"
	// envSCWMaxUnembedded overrides the settled-corpus guard. See the
	// INDEXING-LAG CONFOUND block in the driver body.
	envSCWMaxUnembedded = "MUNINN_SCOREW_MAX_UNEMBEDDED_FRAC"
)

// maxUnembeddedFrac is the default ceiling on the fraction of active engrams
// with no stored embedding. Above this the corpus has not settled and the
// driver refuses to produce a number.
const maxUnembeddedFrac = 0.01

// scwNonsenseQueries are the UNANSWERABLE probes. Every one is synthetic,
// authored here, and deliberately shaped like a plausible memory query
// (concrete nouns, a domain, an implied task) while describing something no
// vault could contain. Shaped-like-real matters: a string of random characters
// would abstain trivially and prove nothing. This mirrors COG-26's
// nonsense-query protocol (plugin/embed/baseline.go).
var scwNonsenseQueries = []string{
	"quarterly ballet recital seating chart for the amphibian troupe",
	"calibration log for the tin-whistle humidity organ in vault seven",
	"migration schedule of the municipal weathervane repair guild",
	"tasting notes for fermented basalt served at the lighthouse gala",
	"knitting pattern for a nine-sleeved reversible barometer cozy",
	"dispute resolution policy between the marzipan cartographers union",
	"maintenance interval for the brass hummingbird irrigation lathe",
	"seating protocol at the annual convention of retired sundials",
	"nutritional breakdown of powdered moonlight sold by the furlong",
	"escalation path for a misfiled complaint about a singing staircase",
	"warranty terms on a self-folding origami submarine hull",
	"attendance record for the subterranean kite-flying certification board",
	"recipe for lacquered thunder as prepared in the northern glassworks",
	"inventory count of left-handed metronomes in the archive annex",
	"revised bylaws governing the ceremonial polishing of borrowed fog",
	"latency budget for transmitting handwriting through candle smoke",
	"pruning guide for the ornamental clockspring hedge in winter",
	"insurance schedule covering damage by enthusiastic library ghosts",
	"onboarding checklist for apprentice cloud stackers, third cohort",
	"decommissioning plan for the obsolete velvet pressure gauge array",
}

func scwEnvRequired(t *testing.T, key string) string {
	t.Helper()
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		t.Fatalf("%s is required (see the header of engine_scoring_weights_measure_test.go)", key)
	}
	return v
}

func scwEnvInt(t *testing.T, key string, def int) int {
	t.Helper()
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		t.Fatalf("%s=%q: want a positive integer", key, v)
	}
	return n
}

func scwEnvFloat(t *testing.T, key string, def float64) float64 {
	t.Helper()
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f < 0 {
		t.Fatalf("%s=%q: want a non-negative float", key, v)
	}
	return f
}

// scwGuardDataDir refuses to open a data dir a running muninn holds. Pebble
// takes an exclusive LOCK so this would fail anyway; failing here gives the
// actionable message and steers the owner to the clone workflow.
func scwGuardDataDir(t *testing.T, dataDir string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(dataDir, "muninn.pid")); err == nil {
		t.Fatalf("%q contains muninn.pid — this looks like a LIVE data directory.\n"+
			"Take a clone first:\n"+
			"  muninn backup --data-dir <live> --output <clone>   # with the server stopped\n"+
			"then point %s at <clone>.", dataDir, envSCWDataDir)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "pebble")); err != nil {
		t.Fatalf("%q does not contain a pebble/ subdirectory — point %s at a data dir, not at the pebble dir itself",
			dataDir, envSCWDataDir)
	}
}

func scwCosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		x, y := float64(a[i]), float64(b[i])
		dot += x * y
		na += x * x
		nb += y * y
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// scwRanked returns pool indices ordered by final score desc, excluding any
// index the relevance threshold dropped — what live recall would return.
func scwRanked(final []float64, dropped []bool) []int {
	idx := make([]int, 0, len(final))
	for i := range final {
		if !dropped[i] {
			idx = append(idx, i)
		}
	}
	sort.SliceStable(idx, func(a, b int) bool { return final[idx[a]] > final[idx[b]] })
	return idx
}

// scwCell accumulates one (arm x condition) cell of the design.
type scwCell struct {
	answerable  int
	ndcgSum     float64
	goldFound   int // gold survived the threshold at any rank
	goldTop1    int
	unanswered  int
	falsePos    int     // an unanswerable query returned >=1 result
	fpDepthSum  float64 // how many results an unanswerable query returned
	top1ConfSum float64
	top1Count   int
}

// ndcgAt5 with a single graded-1 gold document: DCG = 1/log2(rank+1) when the
// gold appears in the top 5 of what the arm returned, 0 otherwise. IDCG = 1.
func ndcgAt5(ranked []int, gold int) float64 {
	for r, idx := range ranked {
		if r >= 5 {
			break
		}
		if idx == gold {
			return 1.0 / math.Log2(float64(r)+2.0)
		}
	}
	return 0
}

func TestMeasureContentMatchCombiners(t *testing.T) {
	ctx := context.Background()

	dataDir := scwEnvRequired(t, envSCWDataDir)
	vault := scwEnvRequired(t, envSCWVault)
	scwGuardDataDir(t, dataDir)

	nQueries := scwEnvInt(t, envSCWQueries, 100)
	poolN := scwEnvInt(t, envSCWPool, 50)
	maxEngrams := scwEnvInt(t, envSCWMaxEng, 5000)
	baseline := scwEnvFloat(t, envSCWBaseline, scwBaselineBGESmall)
	minConf := scwEnvFloat(t, envSCWMinConf, 0)
	suspectConf := scwEnvFloat(t, envSCWSuspect, 0.5)
	maxUnembedded := scwEnvFloat(t, envSCWMaxUnembedded, maxUnembeddedFrac)
	rng := rand.New(rand.NewSource(int64(scwEnvInt(t, envSCWSeed, 1))))

	db, err := storage.OpenPebble(filepath.Join(dataDir, "pebble"), storage.DefaultOptions())
	if err != nil {
		t.Fatalf("open pebble (is a muninn process holding the LOCK?): %v", err)
	}
	defer db.Close()
	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 10000})
	ws := store.ResolveVaultPrefix(vault)
	ftsIdx := fts.New(db) // read path only — Search never writes

	// Embedder: the bundled local model, extracting its assets to a TEMP dir.
	// Nothing is written to the vault clone.
	prov := &embed.LocalProvider{}
	if _, err := prov.Init(ctx, embed.ProviderHTTPConfig{DataDir: t.TempDir()}); err != nil {
		t.Fatalf("local embed provider init (built with -tags localassets, and `make fetch-assets` run?): %v", err)
	}
	defer prov.Close()
	embedOne := func(text string) ([]float32, error) {
		v, err := prov.EmbedBatch(ctx, []string{text})
		return v, err
	}

	ids, err := store.ListByState(ctx, ws, storage.StateActive, maxEngrams)
	if err != nil {
		t.Fatalf("ListByState: %v", err)
	}
	if len(ids) == 0 {
		t.Fatalf("vault has no active engrams under %s — wrong vault name?", envSCWVault)
	}
	engs, err := store.GetEngrams(ctx, ws, ids)
	if err != nil {
		t.Fatalf("GetEngrams: %v", err)
	}

	type node struct {
		id    storage.ULID
		in    scwInput
		emb   []float32
		qtext string // this engram's own summary, used only as a query; never printed
	}
	var nodes []node
	byID := map[storage.ULID]int{}
	confHist := map[string]int{}
	suspectCount, excludedCount, unembedded, totalActive := 0, 0, 0, 0
	for _, e := range engs {
		if e == nil {
			continue
		}
		totalActive++
		emb, err := store.GetEmbedding(ctx, ws, e.ID)
		if err != nil || len(emb) == 0 {
			// INDEXING-LAG CONFOUND: an engram with no stored embedding scores
			// cosine 0 in the live pipeline, so its ContentMatch is capped at
			// 0.4 and it is unreturnable at the 0.5 surface gate. Counted and
			// reported below; excluded from the pool because a candidate with
			// no vector cannot be scored by the raw-cosine control at all, and
			// silently keeping it would flatter that arm.
			unembedded++
			continue
		}
		conf := float64(e.Confidence)
		switch {
		case conf < 0.2:
			confHist["<0.2"]++
		case conf < 0.5:
			confHist["0.2-0.5"]++
		case conf < 0.8:
			confHist["0.5-0.8"]++
		default:
			confHist[">=0.8"]++
		}
		if conf < suspectConf {
			suspectCount++
		}
		if minConf > 0 && conf < minConf {
			excludedCount++
			continue
		}
		qt := strings.TrimSpace(e.Summary)
		if qt == "" {
			qt = strings.TrimSpace(e.Concept)
		}
		byID[e.ID] = len(nodes)
		nodes = append(nodes, node{
			id:    e.ID,
			emb:   emb,
			qtext: qt,
			in: scwInput{
				AccessCount: int64(e.AccessCount),
				LastAccess:  e.LastAccess,
				Confidence:  conf,
			},
		})
	}
	if len(nodes) < poolN+1 {
		t.Fatalf("only %d engrams carry a stored embedding (after the confidence filter) — need > %s (%d)",
			len(nodes), envSCWPool, poolN)
	}

	t.Logf("vault: %d active engrams, %d with embeddings | pool=%d queries=%d baseline=%.3f",
		len(engs), len(nodes), poolN, nQueries, baseline)

	// --- INDEXING-LAG CONFOUND: prove the corpus has settled ----------------
	// A freshly-seeded or still-catching-up corpus makes the full pipeline look
	// artificially bad and the raw-cosine control artificially good, because
	// unembedded engrams are invisible to the former and unscoreable by the
	// latter. This driver PROVES settlement rather than assuming it: it counts
	// engrams with no stored embedding and refuses to run if the fraction is
	// non-trivial.
	unembeddedFrac := float64(unembedded) / math.Max(float64(totalActive), 1)
	t.Logf("EMBEDDING COVERAGE: %d/%d active engrams have a stored embedding; %d (%.2f%%) do NOT",
		totalActive-unembedded, totalActive, unembedded, 100*unembeddedFrac)
	if unembeddedFrac > maxUnembedded {
		t.Fatalf("REFUSING to measure: %.2f%% of active engrams have no embedding (limit %.2f%%).\n"+
			"The retroactive embed processor has not caught up, so this corpus has NOT settled. "+
			"Any scoring number taken now is contaminated: unembedded engrams score cosine 0, cap at "+
			"ContentMatch 0.4, and are unreturnable at the 0.5 surface threshold regardless of combiner.\n"+
			"Wait for the processor to drain (it polls every 3s, backing off to 3min when idle, and NO "+
			"write path calls Notify()), then re-run. Override with %s only if you know why.",
			100*unembeddedFrac, 100*maxUnembedded, envSCWMaxUnembedded)
	}
	t.Logf("CONFIDENCE (pool-wide, BEFORE any exclusion): <0.2:%d  0.2-0.5:%d  0.5-0.8:%d  >=0.8:%d | below suspect %.2f: %d",
		confHist["<0.2"], confHist["0.2-0.5"], confHist["0.5-0.8"], confHist[">=0.8"], suspectConf, suspectCount)
	if minConf > 0 {
		t.Logf("CONFIDENCE EXCLUSION ACTIVE: %s=%.2f removed %d engrams from the pool.", envSCWMinConf, minConf, excludedCount)
	} else {
		t.Logf("CONFIDENCE EXCLUSION OFF: confidence is REPORTED, not removed. The conf-ablated arms below are the "+
			"primary read; if they disagree with the conf-on arms, the contradiction bug is driving the result and "+
			"the conf-on numbers must not be quoted. Set %s to also exclude.", envSCWMinConf)
	}

	// --- the design ---------------------------------------------------------
	// 4 combiners x {conf live, conf ablated} x {full-signal, paraphrase}
	//              x {engine threshold 0.05, SURFACE threshold 0.5}
	//
	// The threshold dimension is not cosmetic. Every MCP and REST agent that
	// omits `threshold` gets 0.5 (mcp/handlers.go:392, rest/server.go:1772),
	// ten times the engine's own default. Measuring only at 0.05 would report
	// numbers no real agent ever sees, and would hide the fact that the 0.4 FTS
	// coefficient sits BELOW the surface gate outright.
	combs := []scwCombiner{scwCombLinear, scwCombRawCosine, scwCombNoisyOR, scwCombMax}
	thresholds := []struct {
		name string
		v    float64
	}{
		{"engine .05", scwDefaultThreshold},
		{"SURFACE .5", scwSurfaceDefaultThreshold},
	}
	type armKey struct {
		comb    scwCombiner
		useConf bool
		para    bool
		thr     int
	}
	cells := map[armKey]*scwCell{}
	specOf := func(k armKey) scwArmSpec {
		return scwArmSpec{
			Name:          k.comb.String(),
			Comb:          k.comb,
			UsePrior:      k.comb != scwCombRawCosine,
			UseConfidence: k.useConf,
		}
	}
	for _, c := range combs {
		for _, uc := range []bool{true, false} {
			for _, pa := range []bool{false, true} {
				for ti := range thresholds {
					cells[armKey{c, uc, pa, ti}] = &scwCell{}
				}
			}
		}
	}

	now := time.Now()

	// buildPool assembles the candidate pool for a query vector + query text:
	// the top-N cosine neighborhood unioned with the FTS top-N, each candidate
	// carrying its real FTS coverage score (0 if FTS did not return it).
	buildPool := func(qv []float32, qtext string, exclude int) ([]scwInput, []storage.ULID) {
		ftsScores := map[storage.ULID]float64{}
		if hits, err := ftsIdx.Search(ctx, ws, qtext, poolN); err == nil {
			for _, h := range hits {
				ftsScores[storage.ULID(h.ID)] = h.Score
			}
		}
		type cand struct {
			i   int
			cos float64
		}
		cands := make([]cand, 0, len(nodes))
		for i := range nodes {
			if i == exclude || len(nodes[i].emb) != len(qv) {
				continue
			}
			cands = append(cands, cand{i, scwCosine(qv, nodes[i].emb)})
		}
		sort.SliceStable(cands, func(a, b int) bool {
			if cands[a].cos != cands[b].cos {
				return cands[a].cos > cands[b].cos
			}
			return cands[a].i < cands[b].i
		})
		keep := map[int]bool{}
		for i := 0; i < len(cands) && len(keep) < poolN; i++ {
			keep[cands[i].i] = true
		}
		for id := range ftsScores {
			if j, ok := byID[id]; ok && j != exclude {
				keep[j] = true
			}
		}
		cosOf := map[int]float64{}
		for _, c := range cands {
			cosOf[c.i] = c.cos
		}
		order := make([]int, 0, len(keep))
		for i := range keep {
			order = append(order, i)
		}
		sort.SliceStable(order, func(a, b int) bool {
			if cosOf[order[a]] != cosOf[order[b]] {
				return cosOf[order[a]] > cosOf[order[b]]
			}
			return order[a] < order[b]
		})
		pool := make([]scwInput, 0, len(order))
		poolIDs := make([]storage.ULID, 0, len(order))
		for _, i := range order {
			in := nodes[i].in
			in.Cosine = cosOf[i]
			in.FTS = ftsScores[nodes[i].id]
			pool = append(pool, in)
			poolIDs = append(poolIDs, nodes[i].id)
		}
		return pool, poolIDs
	}

	scoreAll := func(pool []scwInput, gold int, unanswerable bool) {
		for _, para := range []bool{false, true} {
			p := pool
			if para {
				p = make([]scwInput, len(pool))
				copy(p, pool)
				for i := range p {
					p[i].FTS = 0
				}
			}
			for _, c := range combs {
				for _, uc := range []bool{true, false} {
					for ti, thr := range thresholds {
						k := armKey{c, uc, para, ti}
						cell := cells[k]
						final, _, _, dropped := scwScoreQuery(p, specOf(k), baseline, thr.v, now)
						ranked := scwRanked(final, dropped)
						if unanswerable {
							cell.unanswered++
							if len(ranked) > 0 {
								cell.falsePos++
								cell.fpDepthSum += float64(len(ranked))
							}
							continue
						}
						cell.answerable++
						cell.ndcgSum += ndcgAt5(ranked, gold)
						for _, idx := range ranked {
							if idx == gold {
								cell.goldFound++
								break
							}
						}
						if len(ranked) > 0 {
							if ranked[0] == gold {
								cell.goldTop1++
							}
							cell.top1ConfSum += p[ranked[0]].Confidence
							cell.top1Count++
						}
					}
				}
			}
		}
	}

	// --- ANSWERABLE: query text is a sampled engram's own summary, gold = it.
	answered := 0
	for attempt := 0; attempt < nQueries*3 && answered < nQueries; attempt++ {
		seed := rng.Intn(len(nodes))
		if strings.TrimSpace(nodes[seed].qtext) == "" {
			continue
		}
		qv, err := embedOne(nodes[seed].qtext)
		if err != nil || len(qv) == 0 {
			continue
		}
		// The gold engram must be IN the pool for NDCG to be meaningful, so it
		// is not excluded — exclude=-1. This is retrieval of a known-present
		// document, the easiest honest formulation of "answerable".
		pool, poolIDs := buildPool(qv, nodes[seed].qtext, -1)
		gold := -1
		for i, id := range poolIDs {
			if id == nodes[seed].id {
				gold = i
				break
			}
		}
		if gold < 0 {
			continue // gold outside the pool: an ANN-recall miss, not a scoring result
		}
		scoreAll(pool, gold, false)
		answered++
	}

	// --- UNANSWERABLE: committed synthetic nonsense, no gold, any hit is a FP.
	for _, q := range scwNonsenseQueries {
		qv, err := embedOne(q)
		if err != nil || len(qv) == 0 {
			continue
		}
		pool, _ := buildPool(qv, q, -1)
		if len(pool) == 0 {
			continue
		}
		scoreAll(pool, -1, true)
	}

	if answered == 0 {
		t.Fatalf("no answerable query was scored — check the embedder and %s", envSCWVault)
	}

	// --- report -------------------------------------------------------------
	t.Log("")
	t.Log("=== RESULTS: NDCG@5 on answerable (higher better) vs FPR on unanswerable (lower better) ===")
	t.Log("A combiner is only interesting if it gains more NDCG than it loses in abstention.")
	for _, para := range []bool{false, true} {
		cond := "FULL-SIGNAL"
		if para {
			cond = "PARAPHRASE (FTS forced to 0 — the condition the 0.6 cap is about)"
		}
		for ti, thr := range thresholds {
			t.Logf("")
			t.Logf("--- condition: %s | threshold: %s ---", cond, thr.name)
			t.Logf("%-16s %-9s %8s %10s %10s %8s %10s %10s",
				"combiner", "conf", "NDCG@5", "gold-found", "gold-top1", "FPR", "FP-depth", "top1-conf")
			for _, c := range combs {
				for _, uc := range []bool{true, false} {
					cell := cells[armKey{c, uc, para, ti}]
					if cell.answerable == 0 {
						continue
					}
					a := float64(cell.answerable)
					fpr, depth := 0.0, 0.0
					if cell.unanswered > 0 {
						fpr = float64(cell.falsePos) / float64(cell.unanswered)
						if cell.falsePos > 0 {
							depth = cell.fpDepthSum / float64(cell.falsePos)
						}
					}
					conf := 0.0
					if cell.top1Count > 0 {
						conf = cell.top1ConfSum / float64(cell.top1Count)
					}
					label := "live"
					if !uc {
						label = "ABLATED"
					}
					t.Logf("%-16s %-9s %8.4f %9.1f%% %9.1f%% %7.1f%% %10.1f %10.3f",
						c.String(), label,
						cell.ndcgSum/a,
						100*float64(cell.goldFound)/a,
						100*float64(cell.goldTop1)/a,
						100*fpr, depth, conf)
				}
			}
		}
	}

	t.Log("")
	t.Logf("answerable queries scored: %d | unanswerable probes: %d", answered, len(scwNonsenseQueries))
	t.Log("HOW TO READ THIS:")
	t.Log("  1. Compare the ABLATED rows first. If ABLATED and live rank the combiners differently, " +
		"the confidence bug is driving the result — fix it and re-run before concluding anything.")
	t.Log("  2. In the PARAPHRASE condition, LINEAR's ContentMatch is capped at 0.6*semCal while noisy-OR " +
		"and max reach semCal. If LINEAR's gold-found rate is materially lower there, the 0.6 cap is " +
		"costing real recall.")
	t.Log("  3. Then check FPR in the same condition. COG-26's b=0.520 was calibrated AGAINST the 0.6 " +
		"linear combiner; a more permissive combiner needs b re-derived, not just swapped in.")
	t.Log("  4. Hebbian, PAS-transition, entity and tag signals are 0 here. This harness is biased AGAINST " +
		"the cognition layer's unmeasured parts. Quote it with that caveat attached.")
}
