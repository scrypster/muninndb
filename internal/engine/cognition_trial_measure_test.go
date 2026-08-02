//go:build cognitiontrial

package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/auth"
	"github.com/scrypster/muninndb/internal/cognitive"
	"github.com/scrypster/muninndb/internal/config"
	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/index/fts"
	hnswpkg "github.com/scrypster/muninndb/internal/index/hnsw"
	"github.com/scrypster/muninndb/internal/plugin/embed"
	"github.com/scrypster/muninndb/internal/storage"
)

// ===========================================================================
// THE COGNITION TRIAL — read-only real-vault driver, Phase 1.
//
// BUILD TAG: `cognitiontrial`. CI builds and tests with `-tags localassets`
// (and `localassets,integration` for the cmd suite) and NEVER with this tag, so
// this file does not compile — let alone run — in any CI job or release build.
// It is a tool the owner runs by hand against CLONES of vaults they own. Run it
// as:
//
//	go test -tags 'localassets cognitiontrial' -run TestCognitionTrial \
//	    -timeout 4h ./internal/engine/...
//
// ---------------------------------------------------------------------------
// THE QUESTION IT SETTLES
//
// Does MuninnDB's cognitive layer — Hebbian co-activation boost, PAS predictive
// activation, and the ACT-R base-level prior — improve retrieval quality enough
// to justify its complexity, measured on REAL queries from REAL vaults?
//
// Every prior claim about this layer, positive or negative, was measured
// against a corrupted regime and is void: WeightComplement overflowed at weight
// exactly 1.0 (#757/#759), so every full-confidence edge was written at the
// weight-0.0 key position and read back as 0 since inception; and association
// decay was a per-pass multiplier on a 60s tick — a 13.5-MINUTE half-life —
// until #762/#766. Both fixed in v0.10.0. This is the first moment learned
// structure can survive long enough to be measured.
//
// A KILL VERDICT IS A LEGITIMATE, VALUABLE OUTCOME. The acceptance rule is
// pre-registered in cognition_trial_rule_test.go (ctDecide) and is not moved
// after numbers are seen.
//
// ---------------------------------------------------------------------------
// THE FIVE ARMS
//
//	FULL                 everything on                       LIVE run
//	NO-PAS               no transition candidates injected   LIVE run
//	NO-HEBBIAN           hebbianBoost := 0                   ARITHMETIC on FULL
//	BASE-LEVEL-ONLY      hebbianBoost := 0; transition := 0  ARITHMETIC on NO-PAS
//	CONTENT-MATCH-ONLY   contextualPrior := actrDenominator  ARITHMETIC on NO-PAS
//
// BASE-LEVEL-ONLY is the configuration a KILL VERDICT SHIPS (both boosts off,
// ACT-R base-level kept). It was added because no arm corresponded to it and the
// other four provably cannot reconstruct it under redundancy — so the trial was
// authorizing an action about which it measured nothing. It costs ZERO extra
// live runs.
//
// PAS must be a live run because it INJECTS CANDIDATES (activation phase 2), so
// disabling it changes the candidate SET, which no offline arithmetic can
// reproduce. Hebbian and base-level only RE-WEIGHT candidates already in the
// fused list, so zeroing their components offline is exact, not simulated.
// TestCognitionTrial_ArmReconstructionFidelity proves the arithmetic against
// the live pipeline on a synthetic corpus; this driver ALSO re-proves it per
// candidate on the real corpus before computing any arm (ctSelfCheck below).
//
// ---------------------------------------------------------------------------
// FIDELITY LIMITS — READ BEFORE QUOTING A NUMBER
//
//  1. G1 (replay) reconstructs only the RECALL-DRIVEN sub-graph. Declared links
//     (muninn_link, tree, RelSupersedes, Decide evidence), autoassoc neighbour
//     edges and dream/consolidation transitive inference are NOT in 0x29 and
//     cannot be replayed. Worse, the declared 1.0 edges are the exact class
//     #757 destroyed unidentifiably. G1 therefore systematically
//     UNDER-REPRESENTS declared structure. The composition table is mandatory
//     output and U4 pre-registers the floor.
//
//  2. G0 (as-is) under-measures for a DIFFERENT reason: the association graph is
//     flat today, because the old decay regime ground every edge to peak*0.05
//     and #766's fix is a ceiling that never raises. G0 and G1 are never
//     averaged; they are two different questions ("what does a user get today"
//     vs "what would exist if the bugs had never shipped").
//
//  3. THE CORPUS IS USED TWICE. Training signal (the events' ranked ID lists)
//     and evaluation queries (the events' Context text) come from the same
//     stream. Mitigated by forward-in-time held-out evaluation per checkpoint
//     plus a fixed global holdout excluded from replay. Any leakage that
//     survives INFLATES the cognitive arms — i.e. it biases toward SHIP, which
//     makes a KILL verdict MORE trustworthy and a SHIP verdict correspondingly
//     LESS. State this asymmetry in the write-up.
//
//  4. LTP state is session-local by design, so replay recomputes potentiation
//     from zero. Prefer vaults with ltp_threshold:0 for the primary read; an
//     LTP-enabled vault is a sensitivity check, not a headline.
//
//  5. A vault with hebbian_enabled:false has events but never learned.
//     Replaying it manufactures learning that never occurred — legitimate as a
//     counterfactual, illegitimate as a description of the vault. Every number
//     is labeled with the vault's actual resolved plasticity.
//
// ---------------------------------------------------------------------------
// PRIVACY — ABSOLUTE, AND STRUCTURAL WHERE POSSIBLE
//
//   - Real vaults may be measured; NO VAULT IS EVER NAMED. Reported as A/B/C via
//     MUNINN_COGTRIAL_LABEL, assigned by the operator at run time.
//   - The driver runs only against a CLONE. It REFUSES any directory containing
//     muninn.pid, and refuses a directory that resolves to the default live data
//     dir. Never a second handle on a live vault.
//   - The replay (G1) WRITES, so it demands a SEPARATE directory
//     (MUNINN_COGTRIAL_REPLAY_DIR) that must differ from the read-only clone.
//     Both copies are deleted by the operator after the run.
//   - VAULT SCOPING IS ENFORCED BY CONFIGURATION, NOT BY REMEMBERING.
//     MUNINN_COGTRIAL_OWNED_VAULTS is a required allowlist of vaults the
//     operator personally owns. Every vault read — by this driver and by the
//     judge, which reads real memory CONTENT — is constructed through ctScope,
//     whose only constructor validates membership. A vault absent from the
//     allowlist cannot be reached, and reducing to two vaults triggers U2,
//     which is the correct outcome.
//   - AGGREGATES ONLY in every log line: counts, means, NDCG/MRR, CIs,
//     composition tables. NEVER a query string, memory content, summary,
//     concept, tag, entity, engram ID or vault name.
//   - The frozen label file lives in the operator's scratch dir and stores
//     (sha256(query), memoryID, grade). It is NEVER committed; only its hash is
//     recorded, and the hash is recorded BEFORE any arm is scored.
//   - Every string authored in THIS FILE is synthetic. There are none that are
//     not: the driver reads its queries from the vault at run time.
//
// ===========================================================================

// --- environment -----------------------------------------------------------

const (
	envCTDataDir      = "MUNINN_COGTRIAL_DATA_DIR"     // read-only clone (G0)
	envCTReplayDir    = "MUNINN_COGTRIAL_REPLAY_DIR"   // throwaway writable clone (G1)
	envCTVault        = "MUNINN_COGTRIAL_VAULT"        // vault name inside the clone
	envCTOwnedVaults  = "MUNINN_COGTRIAL_OWNED_VAULTS" // comma-separated allowlist
	envCTLabel        = "MUNINN_COGTRIAL_LABEL"        // A | B | C
	envCTLabels       = "MUNINN_COGTRIAL_LABELS"       // frozen label file
	envCTAdjudication = "MUNINN_COGTRIAL_ADJUDICATION" // human-vs-judge subsample
	envCTMode         = "MUNINN_COGTRIAL_MODE"         // asis | replay
	envCTQueries      = "MUNINN_COGTRIAL_QUERIES"      // cap on evaluated queries
	envCTSeed         = "MUNINN_COGTRIAL_SEED"         // bootstrap + sampling seed
	envCTPoolDepth    = "MUNINN_COGTRIAL_POOL_DEPTH"   // pooling depth per arm
	envCTMaxUnembed   = "MUNINN_COGTRIAL_MAX_UNEMBEDDED"
	envCTBuckets      = "MUNINN_COGTRIAL_BUCKETS" // weekly checkpoints
)

const (
	// ctDefaultBuckets and friends are harness defaults. The POOL DEPTH is not
	// one: it is pre-registered (ctPreregistered.PoolDepth) because it decides
	// which queries are informative under D1, so the default IS the pin and a
	// deviation is a re-pre-registration, not a flag.
	ctDefaultBuckets      = 12
	ctDefaultMaxUnembed   = 0.01
	ctHoldoutFraction     = 0.20
	ctReplayDecayCadenceD = 1.0 // simulated days between replayed decay passes
)

func ctEnvRequired(t *testing.T, key string) string {
	t.Helper()
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		t.Fatalf("%s is required (see the header of cognition_trial_measure_test.go)", key)
	}
	return v
}

func ctEnvInt(t *testing.T, key string, def int) int {
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

func ctEnvFloat(t *testing.T, key string, def float64) float64 {
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

// --- vault scoping, enforced by construction --------------------------------

// ctScope is the ONLY way this driver names a vault. Its constructor is the
// single place the owned-vault allowlist is checked, so "we only read vaults the
// operator owns" is a property of the type rather than a promise someone has to
// remember. The judge harness takes a ctScope for the same reason: it reads real
// memory CONTENT, which is the most serious risk in the whole design.
type ctScope struct {
	vault string
	label string // A | B | C — the ONLY identifier that may ever be printed
	ws    [8]byte
}

func ctNewScope(t *testing.T, store *storage.PebbleStore, vault, label string) ctScope {
	t.Helper()
	allowed := map[string]struct{}{}
	for _, v := range strings.Split(ctEnvRequired(t, envCTOwnedVaults), ",") {
		if v = strings.TrimSpace(v); v != "" {
			allowed[v] = struct{}{}
		}
	}
	if len(allowed) == 0 {
		t.Fatalf("%s is empty — the trial may only touch vaults the operator personally owns "+
			"(design risk 1). An empty allowlist is not a wildcard.", envCTOwnedVaults)
	}
	if _, ok := allowed[vault]; !ok {
		t.Fatalf("the requested vault is NOT in %s. Refusing: the LLM judge reads real memory "+
			"content, so the trial runs only on vaults the operator personally owns. If a vault "+
			"cannot be included, it does not enter the trial — reducing to two vaults triggers "+
			"U2 and the trial is underpowered, which is the CORRECT outcome.", envCTOwnedVaults)
	}
	switch label {
	case "A", "B", "C":
	default:
		t.Fatalf("%s=%q: must be A, B or C. Vaults are never named in output.", envCTLabel, label)
	}
	return ctScope{vault: vault, label: label, ws: store.ResolveVaultPrefix(vault)}
}

// --- refusals ---------------------------------------------------------------

// ctGuardDataDir refuses anything that is not a clone.
func ctGuardDataDir(t *testing.T, dir, envName string) {
	t.Helper()
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("%s=%q: %v", envName, dir, err)
	}
	if _, err := os.Stat(filepath.Join(abs, "muninn.pid")); err == nil {
		t.Fatalf("%q contains muninn.pid — this looks like a LIVE data directory.\n"+
			"Stop the server and take a clone:\n"+
			"  muninn backup --data-dir <live> --output <clone>\n"+
			"then point %s at <clone>. The trial NEVER opens a second handle on a live vault.",
			abs, envName)
	}
	if home, err := os.UserHomeDir(); err == nil {
		def, _ := filepath.Abs(filepath.Join(home, ".muninn", "data"))
		if abs == def {
			t.Fatalf("%s resolves to the DEFAULT live data directory (%s). Refusing. Clone first.",
				envName, def)
		}
	}
	if _, err := os.Stat(filepath.Join(abs, "pebble")); err != nil {
		t.Fatalf("%q has no pebble/ subdirectory — point %s at a data dir, not at the pebble dir itself",
			abs, envName)
	}
}

// ctGuardNotClustered refuses a clustered vault. The Lobe path forwards
// co-activation refs to Cortex, so the local 0x29 stream may not match what was
// actually learned — the "RecallEvent.Entries == co-activation set" identity the
// whole replay rests on is verified for the single-node path ONLY (design risk 4).
func ctGuardNotClustered(t *testing.T, dir string) {
	t.Helper()
	cfg, err := config.LoadClusterConfig(dir)
	if err != nil {
		t.Fatalf("read cluster config from %q: %v", dir, err)
	}
	if cfg.Enabled {
		t.Fatalf("REFUSING: this data directory is configured for CLUSTER mode. On a Lobe node " +
			"co-activation refs are forwarded to Cortex, so the local 0x29 stream may not be " +
			"what was learned, and the replay would reconstruct a graph from an incomplete " +
			"training signal. Single-node vaults only.")
	}
}

// --- corpus -----------------------------------------------------------------

// ctEvent is one recorded recall, reduced to exactly what the trial needs.
// The query TEXT is kept in memory for the duration of the run (it is what the
// arms are run against) and is NEVER logged; only its hash leaves this process.
type ctEvent struct {
	at        time.Time
	queryText string
	queryHash string
	entries   []storage.ULID
	scores    []float32
}

func ctQueryHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// ctScanEvents reads the vault's 0x29 recall-event stream in ULID (event-time)
// order through the purpose gate. RecallPurposeTrial exists so that this read is
// deliberate, named and logged — do not reuse "calibration".
func ctScanEvents(t *testing.T, ctx context.Context, store *storage.PebbleStore, sc ctScope) []ctEvent {
	t.Helper()
	var out []ctEvent
	err := store.ScanRecallEvents(ctx, sc.ws, storage.RecallPurposeTrial,
		func(_ storage.ULID, ev *storage.RecallEvent) error {
			if ev == nil || len(ev.Entries) == 0 {
				return nil
			}
			q := strings.TrimSpace(strings.Join(ev.Context, " "))
			if q == "" {
				return nil
			}
			e := ctEvent{
				at:        time.Unix(0, ev.SurfacedAt),
				queryText: q,
				queryHash: ctQueryHash(strings.ToLower(q)),
			}
			for _, en := range ev.Entries {
				id, err := storage.ParseULID(en.ID)
				if err != nil {
					continue
				}
				e.entries = append(e.entries, id)
				e.scores = append(e.scores, en.Score)
			}
			if len(e.entries) > 0 {
				out = append(out, e)
			}
			return nil
		})
	if err != nil {
		t.Fatalf("ScanRecallEvents: %v", err)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].at.Before(out[j].at) })
	return out
}

// ctBucketise assigns each event to a weekly bucket index [0, buckets).
// Buckets are equal-width over the observed span, so an uneven 0x29 window
// (retention is 90 days but pruning is amortized every 256th write
// PROCESS-WIDE, so a vault's usable window may be shorter) produces uneven
// COUNTS, which are reported, rather than uneven WIDTHS, which would silently
// distort the trend.
func ctBucketise(events []ctEvent, buckets int) []int {
	idx := make([]int, len(events))
	if len(events) == 0 || buckets <= 0 {
		return idx
	}
	first, last := events[0].at, events[len(events)-1].at
	span := last.Sub(first)
	if span <= 0 {
		return idx
	}
	for i, e := range events {
		b := int(float64(buckets) * float64(e.at.Sub(first)) / float64(span))
		if b >= buckets {
			b = buckets - 1
		}
		idx[i] = b
	}
	return idx
}

// --- labels -----------------------------------------------------------------

// ctLabels is the FROZEN label set: (queryHash, memoryID) -> grade.
type ctLabels struct {
	grades map[string]map[string]int // queryHash -> memoryID -> grade
	hash   string                    // sha256 over the sorted triples
	n      int
}

// ctLoadLabels reads the frozen label file and computes its canonical hash. The
// hash is logged BEFORE any arm number is computed; §6 S5 requires it to match
// what was recorded when the labels were frozen.
//
// Format, one per line, tab-separated: sha256(query)\tmemoryID\tgrade
func ctLoadLabels(t *testing.T, path string) ctLabels {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s=%q: %v", envCTLabels, path, err)
	}
	lb := ctLabels{grades: map[string]map[string]int{}}
	var triples []string
	for lineNo, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) != 3 {
			t.Fatalf("%s line %d: want 3 tab-separated fields (queryHash, memoryID, grade)",
				path, lineNo+1)
		}
		g, err := strconv.Atoi(f[2])
		if err != nil || g < 0 || g > 3 {
			t.Fatalf("%s line %d: grade must be 0..3", path, lineNo+1)
		}
		if lb.grades[f[0]] == nil {
			lb.grades[f[0]] = map[string]int{}
		}
		lb.grades[f[0]][f[1]] = g
		triples = append(triples, f[0]+"\t"+f[1]+"\t"+f[2])
		lb.n++
	}
	sort.Strings(triples)
	sum := sha256.Sum256([]byte(strings.Join(triples, "\n")))
	lb.hash = hex.EncodeToString(sum[:])
	return lb
}

// ctLoadAdjudication reads the human-vs-judge subsample and computes the §3c
// gate. Format, tab-separated: judgeGrade\thumanGrade (one pair per line).
//
// This runs and is REPORTED BEFORE any arm is scored. An unrun gate is U1, not
// a missing detail.
func ctLoadAdjudication(t *testing.T, path string) ctJudgeCalibration {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s=%q: %v", envCTAdjudication, path, err)
	}
	var judge, human []int
	for lineNo, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) != 2 {
			t.Fatalf("%s line %d: want 2 tab-separated fields (judgeGrade, humanGrade)", path, lineNo+1)
		}
		j, err1 := strconv.Atoi(f[0])
		h, err2 := strconv.Atoi(f[1])
		if err1 != nil || err2 != nil {
			t.Fatalf("%s line %d: grades must be integers", path, lineNo+1)
		}
		judge = append(judge, j)
		human = append(human, h)
	}
	k, fpr, n, hRel, hIrrel := ctCohensKappa(judge, human)
	return ctJudgeCalibration{
		Ran: n > 0, Kappa: k, FPR: fpr, N: n,
		HumanRelevant: hRel, HumanIrrelevant: hIrrel,
	}
}

// --- arm arithmetic ---------------------------------------------------------
//
// The SAME formulas TestCognitionTrial_ArmReconstructionFidelity proves against
// the live pipeline. The ACT-R constants are duplicated deliberately rather than
// exported: if the engine's values drift, the fidelity test and ctSelfCheck both
// fail loudly instead of this driver silently tracking them and reporting
// numbers for a formula nobody reviewed.

const (
	ctDenominatorM = 1.6931471805599453 // actrDenominator = 1 + softplus(0)
	ctACTRDecayM   = 0.5
	ctAgeFloorD    = 1.0 / (24.0 * 60.0)
)

var ctBLevelCapM = math.Log(math.Exp(ctDenominatorM) - 1)

func ctSoftplusM(x float64) float64 { return math.Log(1 + math.Exp(x)) }

func ctBaseLevelM(accessCount uint32, lastAccess, now time.Time) float64 {
	ageDays := math.Max(now.Sub(lastAccess).Hours()/24.0, ctAgeFloorD)
	n := float64(accessCount + 1)
	return math.Min(math.Log(n)-ctACTRDecayM*math.Log(math.Max(ageDays, ctAgeFloorD)/n), ctBLevelCapM)
}

// ctRow is one captured candidate. contentMatch is captured ONCE and consumed
// identically by every arm — COG-5 is not just unchanged, it is provably so.
type ctRow struct {
	id           storage.ULID
	contentMatch float64
	baseLevel    float64
	hebbian      float64
	transition   float64
	confidence   float64
	gotRaw       float64 // as reported, for the self-check
}

func ctArmRawFull(r ctRow, hebScale float64) float64 {
	return r.contentMatch * ctSoftplusM(r.baseLevel+hebScale*r.hebbian+hebScale*r.transition) / ctDenominatorM
}

func ctArmRawNoHebbian(r ctRow, hebScale float64) float64 {
	return r.contentMatch * ctSoftplusM(r.baseLevel+hebScale*r.transition) / ctDenominatorM
}

// ctArmRawBaseLevelOnly is the FIFTH ARM (D2): base-level prior ON, both boosts
// arithmetically zeroed. It is applied to the NO-PAS live run, exactly the
// relationship CONTENT-MATCH-ONLY already has to NO-PAS, so it costs ZERO
// additional live runs.
//
// It exists because it is THE CONFIGURATION A KILL VERDICT SHIPS —
// hebbian_enabled:false + predictive_activation:false with the ACT-R base-level
// prior kept — and nothing else measured it. Delta_HP = FULL - BASE-LEVEL-ONLY
// is therefore what a kill costs, and the four other arms cannot reconstruct it:
// the bound is [max(Delta_H, Delta_P), Delta_C], which on a real vault spans
// essentially the whole range.
func ctArmRawBaseLevelOnly(r ctRow, _ float64) float64 {
	return r.contentMatch * ctSoftplusM(r.baseLevel) / ctDenominatorM
}

func ctArmRawContentOnly(r ctRow, _ float64) float64 { return r.contentMatch }

// ctRank orders rows by arm score descending, breaking ties by ID exactly as the
// pipeline does, and returns memory-ID strings for the metric functions.
func ctRank(rows []ctRow, arm func(ctRow, float64) float64, hebScale float64) []string {
	idx := make([]int, len(rows))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool {
		ra := arm(rows[idx[a]], hebScale) * rows[idx[a]].confidence
		rb := arm(rows[idx[b]], hebScale) * rows[idx[b]].confidence
		if ra != rb {
			return ra > rb
		}
		return rows[idx[a]].id.String() < rows[idx[b]].id.String()
	})
	out := make([]string, 0, len(idx))
	for _, i := range idx {
		out = append(out, rows[i].id.String())
	}
	return out
}

// ctSelfCheck re-proves the reconstruction ON THE REAL CORPUS before any arm is
// computed: the FULL arm must reproduce the pipeline's reported Raw for every
// captured candidate, up to the per-query 1/maxRaw rescale the pipeline applies
// when anything saturates. A synthetic fidelity test is necessary but not
// sufficient — this is the check on the data actually being measured.
//
// Returns the number of candidates that disagreed.
func ctSelfCheck(rows []ctRow, hebScale float64, tol float64) int {
	reconMax := 0.0
	recon := make([]float64, len(rows))
	for i, r := range rows {
		recon[i] = ctArmRawFull(r, hebScale)
		if recon[i] > reconMax {
			reconMax = recon[i]
		}
	}
	scale := 1.0
	if reconMax > 1.0 {
		scale = 1.0 / reconMax
	}
	bad := 0
	for i, r := range rows {
		if math.Abs(math.Min(recon[i]*scale, 1.0)-r.gotRaw) > tol {
			bad++
		}
	}
	return bad
}

// --- the replay driver ------------------------------------------------------

// ctReplay rebuilds the recall-driven association sub-graph on the WRITABLE
// clone by replaying the recorded co-activation sets through the real learning
// path, INTERLEAVED with decay at simulated times.
//
// Why interleaved, and why this is the whole point: UpdateAssocWeightBatch used
// to stamp lastActivated = time.Now(), so a bare replay would leave every edge
// looking freshly reinforced, decay would find ~0 elapsed, and the result would
// be a "no forgetting ever" graph — exactly the thing MuninnDB claims NOT to be.
// A replay that does not replay the forgetting fabricates a graph that never
// existed. #779 threads CoActivationEvent.At through to the write, and
// storage.SetDecayClock lets decay be evaluated at the simulated instant.
//
// The decay cadence is a free parameter and provably does not change the result:
// COG-27 made decay an ABSOLUTE elapsed-time ceiling rather than a per-pass
// multiplier, so any grid of evaluation times over the same interval yields the
// same weight. That is a direct dividend of #766 and it is what makes the
// interleaving defensible.
//
// A LIMIT THE MONOTONIC DECAY ANCHOR IMPOSES, stated here rather than
// discovered later. `lastActivated` is now monotonically non-decreasing at the
// storage writers (COG-27's amendment: a remotely-produced stamp must not move
// a live edge's decay anchor backwards). The replay writes onto a clone that
// still holds the BASELINE graph, whose edges carry their real, recent
// `lastActivated` — so for a pair that already exists in G0, a replayed
// historical stamp is clamped forward to the baseline's, and that pair's
// replayed forgetting is suppressed. Pairs the replay CREATES are unaffected
// (an absent edge has no anchor to clamp against), which is why the fidelity
// pins stay green.
//
// The consequence is that `replay` mode reconstructs a graph that forgets LESS
// than production did, on exactly the edges it shares with the baseline — a
// SHIP-ward bias, and it belongs in the design's both-directions bias table
// beside the over-deep-seeding one. It is not fixed here for two reasons: the
// alternative is to let a stale stamp collapse a live edge irreversibly, which
// is the product defect this constraint exists to close; and the clean fix is
// to build G1 from a CLEARED association namespace rather than on top of G0,
// which is a change to what the replay arm means and belongs with the
// transition-table reconstruction that is already deferred. `asis` is the
// runnable configuration and is untouched — it trains nothing.
//
// checkpoint is called at each weekly boundary with the bucket index.
func ctReplay(
	t *testing.T,
	ctx context.Context,
	store *storage.PebbleStore,
	sc ctScope,
	events []ctEvent,
	buckets []int,
	resolved auth.ResolvedPlasticity,
	checkpoint func(bucket int),
) {
	t.Helper()
	if len(events) == 0 {
		t.Fatal("replay: no events")
	}

	var ltp *cognitive.LTPConfig
	if resolved.LTPThreshold > 0 {
		ltp = &cognitive.LTPConfig{Threshold: resolved.LTPThreshold, WeightFloor: resolved.LTPWeightFloor}
	}

	hw := cognitive.NewHebbianWorker(cognitive.NewHebbianStoreAdapter(store))
	hw.Stop() // no run loop: the replay is the only caller of the learning path
	defer hw.Stop()

	virtualNow := events[0].at
	storage.SetDecayClock(store, func() time.Time { return virtualNow })
	defer storage.SetDecayClock(store, nil)

	halfLife := time.Duration(float64(resolved.AssocHalfLifeDays) * 24 * float64(time.Hour))
	decayOn := resolved.HebbianEnabled && resolved.AssocDecayFactor > 0 && halfLife > 0
	cadence := time.Duration(ctReplayDecayCadenceD * 24 * float64(time.Hour))
	lastDecay := virtualNow
	lastBucket := -1

	for i, ev := range events {
		virtualNow = ev.at

		// Decay first: forgetting that happened BEFORE this recall must be
		// applied before the recall reinforces anything.
		for decayOn && virtualNow.Sub(lastDecay) >= cadence {
			lastDecay = lastDecay.Add(cadence)
			saved := virtualNow
			virtualNow = lastDecay
			if _, err := store.DecayAssocWeights(ctx, sc.ws, halfLife, 0.01, 0); err != nil {
				t.Fatalf("replay decay pass: %v", err)
			}
			virtualNow = saved
		}

		engrams := make([]cognitive.CoActivatedEngram, len(ev.entries))
		for k, id := range ev.entries {
			engrams[k] = cognitive.CoActivatedEngram{ID: [16]byte(id), Score: float64(ev.scores[k])}
		}
		if err := cognitive.ReplayCoActivations(ctx, hw, []cognitive.CoActivationEvent{{
			WS: sc.ws, At: ev.at, Engrams: engrams, LTP: ltp,
		}}); err != nil {
			t.Fatalf("ReplayCoActivations: %v", err)
		}

		if buckets[i] != lastBucket {
			if lastBucket >= 0 && checkpoint != nil {
				checkpoint(lastBucket)
			}
			lastBucket = buckets[i]
		}
	}
	if lastBucket >= 0 && checkpoint != nil {
		checkpoint(lastBucket)
	}
}

// ctEdgeComposition counts a vault's association edges by RelType. Mandatory
// output: G1 can only produce the Hebbian/relates class, so the share of the
// baseline graph in declared / autoassoc / consolidation RelTypes is exactly the
// share the reconstruction structurally cannot reach (U4).
func ctEdgeComposition(t *testing.T, ctx context.Context, store *storage.PebbleStore, sc ctScope, ids []storage.ULID) (total int, byType map[storage.RelType]int) {
	t.Helper()
	byType = map[storage.RelType]int{}
	const batch = 256
	for i := 0; i < len(ids); i += batch {
		j := i + batch
		if j > len(ids) {
			j = len(ids)
		}
		m, err := store.GetAssociations(ctx, sc.ws, ids[i:j], 1000)
		if err != nil {
			t.Fatalf("GetAssociations: %v", err)
		}
		for _, as := range m {
			for _, a := range as {
				total++
				byType[a.RelType]++
			}
		}
	}
	return total, byType
}

// ctUnreplayableFrac is the share of edges whose RelType the replay structurally
// cannot produce. The Hebbian worker only ever writes the generic co-activation
// class; everything else comes from declared links, autoassoc or consolidation.
//
// IT RETURNS NaN ON AN EMPTY BASELINE, NOT 0. With total == 0 the ratio is 0/0
// and there is no composition to report; returning 0 said "none of this vault's
// baseline edges are unreplayable", which is the most reassuring thing this
// function can say and is a statement about a graph nobody counted. It read as a
// passed U4 ceiling — at exactly the same moment BaselineEdges == 0 stopped the
// replay-fraction floor from being formed, so BOTH halves of U4 fell silent
// together on one cause. The rule now types both: see ctDecide's U4 block.
//
// A REAL 0.0 (total > 0 and every edge in the replayable class) is a
// measurement, is still returned as 0, and still passes.
func ctUnreplayableFrac(total int, byType map[storage.RelType]int) float64 {
	if total <= 0 {
		return math.NaN()
	}
	replayable := byType[storage.RelRelatesTo]
	return float64(total-replayable) / float64(total)
}

// TestCognitionTrialUnreplayableFracIsUndefinedOnAnEmptyBaseline pins the
// producer half of U4's absence-typing. The rule half — the clause that REJECTS
// a non-finite ratio instead of comparing it against the ceiling and getting
// "no objection" — is in ctDecide and is covered by
// TestCognitionTrialRule_UnmeasurableUnreplayableFracIsNotNoObjection, which
// runs in the default CI job. This one needs the harness tag because
// ctUnreplayableFrac lives with the edge-composition census that feeds it.
func TestCognitionTrialUnreplayableFracIsUndefinedOnAnEmptyBaseline(t *testing.T) {
	if got := ctUnreplayableFrac(0, map[storage.RelType]int{}); !math.IsNaN(got) {
		t.Errorf("ctUnreplayableFrac(0, ...) = %v, want NaN. 0/0 is not 'none of this vault's "+
			"edges are unreplayable' — that is the most reassuring thing this function can say "+
			"and it would be saying it about a graph nobody counted, at exactly the moment "+
			"BaselineEdges == 0 also stops U4's replay-fraction floor from being formed", got)
	}
	if got := ctUnreplayableFrac(0, nil); !math.IsNaN(got) {
		t.Errorf("ctUnreplayableFrac(0, nil) = %v, want NaN", got)
	}
	// A REAL zero is a measurement and must survive as one, or the fix would have
	// traded a silent no-objection for a false objection on a perfectly replayed
	// vault.
	if got := ctUnreplayableFrac(400, map[storage.RelType]int{storage.RelRelatesTo: 400}); got != 0 {
		t.Errorf("a fully replayable baseline reported %v, want exactly 0", got)
	}
	if got := ctUnreplayableFrac(400, map[storage.RelType]int{storage.RelRelatesTo: 100}); got != 0.75 {
		t.Errorf("300 of 400 edges unreplayable reported %v, want 0.75", got)
	}
}

// ===========================================================================
// THE DRIVER
// ===========================================================================

func TestCognitionTrial(t *testing.T) {
	ctx := context.Background()

	dataDir := ctEnvRequired(t, envCTDataDir)
	vault := ctEnvRequired(t, envCTVault)
	label := ctEnvRequired(t, envCTLabel)
	labelsPath := ctEnvRequired(t, envCTLabels)
	adjPath := ctEnvRequired(t, envCTAdjudication)
	mode := strings.TrimSpace(os.Getenv(envCTMode))
	if mode == "" {
		mode = "asis"
	}
	if mode != "asis" && mode != "replay" {
		t.Fatalf("%s=%q: want asis or replay", envCTMode, mode)
	}
	poolDepth := ctEnvInt(t, envCTPoolDepth, ctPreregistered.PoolDepth)
	// THE INSTRUMENT PIN, checked before a single query is scored.
	//
	// D1's exclusion is arm-neutral, but the POOL is the union of the arms'
	// top-poolDepth, so the arm SET and the DEPTH decide WHICH queries have a
	// defined NDCG at all — and therefore where every absolute bar S1 and K1 read
	// falls. poolDepth is env-configurable; a run with a different one is not the
	// pre-registered instrument and its numbers are not comparable to another
	// vault's. Fail loudly here rather than silently shifting the bars.
	if msg := ctInstrumentPinViolation(ctArmNames, poolDepth); msg != "" {
		t.Fatalf("PRE-REGISTRATION: %s\n(%s=%d)", msg, envCTPoolDepth, poolDepth)
	}
	nBuckets := ctEnvInt(t, envCTBuckets, ctDefaultBuckets)
	maxQueries := ctEnvInt(t, envCTQueries, 0)
	seed := int64(ctEnvInt(t, envCTSeed, 1))
	maxUnembed := ctEnvFloat(t, envCTMaxUnembed, ctDefaultMaxUnembed)

	ctGuardDataDir(t, dataDir, envCTDataDir)
	ctGuardNotClustered(t, dataDir)

	openDir := dataDir
	if mode == "replay" {
		replayDir := ctEnvRequired(t, envCTReplayDir)
		ctGuardDataDir(t, replayDir, envCTReplayDir)
		ctGuardNotClustered(t, replayDir)
		a, _ := filepath.Abs(dataDir)
		b, _ := filepath.Abs(replayDir)
		if a == b {
			t.Fatalf("%s and %s are the same directory. The replay WRITES; it must run on a "+
				"SECOND, throwaway copy so the as-is control (G0) stays untouched and readable.",
				envCTDataDir, envCTReplayDir)
		}
		openDir = replayDir
	}

	db, err := storage.OpenPebble(filepath.Join(openDir, "pebble"), storage.DefaultOptions())
	if err != nil {
		t.Fatalf("open pebble (is a muninn process holding the LOCK?): %v", err)
	}
	defer db.Close()
	store := storage.NewPebbleStore(db, storage.PebbleStoreConfig{CacheSize: 10000})
	defer store.Close()

	sc := ctNewScope(t, store, vault, label)
	authStore := auth.NewStore(db)
	vaultCfg, cfgErr := authStore.GetVaultConfig(sc.vault)
	var resolved auth.ResolvedPlasticity
	if cfgErr == nil {
		resolved = auth.ResolvePlasticity(vaultCfg.Plasticity)
	} else {
		resolved = auth.ResolvePlasticity(nil)
	}

	t.Logf("=== COGNITION TRIAL — vault %s, mode %s ===", sc.label, mode)
	t.Logf("RESOLVED PLASTICITY: hebbian_enabled=%v predictive_activation=%v "+
		"assoc_half_life_days=%.1f assoc_decay_factor=%.3f ltp_threshold=%d "+
		"actr_decay=%.3f actr_heb_scale=%.3f scoring_fusion=%s",
		resolved.HebbianEnabled, resolved.PredictiveActivation, resolved.AssocHalfLifeDays,
		resolved.AssocDecayFactor, resolved.LTPThreshold, resolved.ACTRDecay,
		resolved.ACTRHebScale, resolved.ScoringFusion)
	if !resolved.HebbianEnabled {
		t.Logf("NOTE: this vault has hebbian_enabled=false. It has events but NEVER LEARNED. " +
			"Replaying it manufactures learning that did not occur — legitimate as a " +
			"counterfactual, ILLEGITIMATE as a description of the vault. Do not average it " +
			"with a learning vault. (Since COG-32 the read side is off here too.)")
	}
	if resolved.LTPThreshold > 0 {
		t.Logf("NOTE: ltp_threshold=%d. LTP state is session-local by design, so the replay "+
			"recomputes potentiation from zero. Treat this vault as a SENSITIVITY CHECK, "+
			"not a headline (design risk 5).", resolved.LTPThreshold)
	}

	// --- corpus settlement --------------------------------------------------
	ids, err := store.ListByState(ctx, sc.ws, storage.StateActive, 1_000_000)
	if err != nil {
		t.Fatalf("ListByState: %v", err)
	}
	if len(ids) == 0 {
		t.Fatalf("vault has no active engrams — wrong %s?", envCTVault)
	}
	unembedded := 0
	for _, id := range ids {
		emb, err := store.GetEmbedding(ctx, sc.ws, id)
		if err != nil || len(emb) == 0 {
			unembedded++
		}
	}
	frac := float64(unembedded) / float64(len(ids))
	t.Logf("CORPUS: %d active engrams, %d (%.2f%%) with no stored embedding",
		len(ids), unembedded, 100*frac)
	if frac > maxUnembed {
		t.Fatalf("REFUSING to measure: %.2f%% of active engrams have no embedding (limit %.2f%%). "+
			"The corpus has NOT settled; the semantic channel is silent for those rows and every "+
			"arm's numbers are contaminated. Wait for the retroactive embed processor to drain, "+
			"then re-run. Override with %s only if you know why.",
			100*frac, 100*maxUnembed, envCTMaxUnembed)
	}

	// --- events -------------------------------------------------------------
	events := ctScanEvents(t, ctx, store, sc)
	if len(events) == 0 {
		t.Fatalf("no recall events in 0x29 for this vault. The feature postdates #573 and " +
			"retention is 90 days with amortized pruning, so a vault may simply have none yet.")
	}
	buckets := ctBucketise(events, nBuckets)
	perBucket := make([]int, nBuckets)
	for _, b := range buckets {
		perBucket[b]++
	}
	t.Logf("EVENTS: %d over %s (first→last). Per-bucket counts: %v",
		len(events), events[len(events)-1].at.Sub(events[0].at).Round(time.Hour), perBucket)

	// Dedup by normalized query text; count distinct events for U2.
	seenQuery := map[string]int{}
	var distinct []ctEvent
	var distinctBucket []int
	for i, e := range events {
		if _, ok := seenQuery[e.queryHash]; ok {
			continue
		}
		seenQuery[e.queryHash] = i
		distinct = append(distinct, e)
		distinctBucket = append(distinctBucket, buckets[i])
	}
	t.Logf("DEDUP: %d distinct queries from %d events", len(distinct), len(events))

	// --- the holdout, fixed BEFORE any arm is scored ------------------------
	// Deterministic from the query hash so the split is reproducible from the
	// run log and identical across G0 and G1 — the two graphs must be judged on
	// the same queries or the comparison is meaningless. Training and evaluation
	// draw on the same event stream (design risk 3); the holdout is excluded
	// from the replay.
	held := map[string]bool{}
	for _, e := range distinct {
		if ctHoldoutBit(e.queryHash, seed) < ctHoldoutFraction {
			held[e.queryHash] = true
		}
	}
	var evalQueries []ctEvent
	var evalBucket []int
	for i, e := range distinct {
		if held[e.queryHash] {
			evalQueries = append(evalQueries, e)
			evalBucket = append(evalBucket, distinctBucket[i])
		}
	}
	// A CAP MUST NOT RESHAPE THE TIME SPAN. evalQueries is in event-time order,
	// so `evalQueries[:maxQueries]` keeps the OLDEST queries and drops the
	// newest — which empties every trailing weekly bucket and manufactures
	// exactly the sparse-tail shape the trend is most easily fooled by. The
	// subsample is taken at an even stride across the whole ordered list
	// instead, so it spans the same interval as the full set and every bucket
	// keeps its share. Deterministic, so a truncated run reproduces itself.
	if maxQueries > 0 && len(evalQueries) > maxQueries {
		total := len(evalQueries)
		sampled := make([]ctEvent, 0, maxQueries)
		sampledBucket := make([]int, 0, maxQueries)
		for k := 0; k < maxQueries; k++ {
			i := k * total / maxQueries
			sampled = append(sampled, evalQueries[i])
			sampledBucket = append(sampledBucket, evalBucket[i])
		}
		t.Logf("SUBSAMPLE: %s=%d — taking every %.1fth query at an even stride across the full "+
			"time span (%d held out), NOT the first %d, which would drop the newest queries and "+
			"empty the trailing weekly buckets.",
			envCTQueries, maxQueries, float64(total)/float64(maxQueries), total, maxQueries)
		evalQueries, evalBucket = sampled, sampledBucket
	}
	t.Logf("HOLDOUT: %d of %d distinct queries held out for evaluation (%.0f%% target), "+
		"excluded from replay", len(evalQueries), len(distinct), 100*ctHoldoutFraction)

	// --- the label set, hashed and logged BEFORE any arm number -------------
	labels := ctLoadLabels(t, labelsPath)
	judge := ctLoadAdjudication(t, adjPath)
	t.Logf("LABELS: %d (queryHash, memoryID, grade) triples", labels.n)
	t.Logf("LABEL SET SHA-256: %s", labels.hash)
	t.Logf("JUDGE CALIBRATION (run BEFORE any arm was scored): kappa=%.3f fpr=%.1f%% n=%d "+
		"human-relevant=%d human-irrelevant=%d\n"+
		"  [gate: kappa >= %.2f AND fpr <= %.0f%% AND n >= %d AND both human marginals >= 1]",
		judge.Kappa, 100*judge.FPR, judge.N, judge.HumanRelevant, judge.HumanIrrelevant,
		ctPreregistered.MinJudgeKappa, 100*ctPreregistered.MaxJudgeFPR,
		ctPreregistered.MinAdjudicatedPairs)
	if judge.HumanIrrelevant == 0 || judge.HumanRelevant == 0 {
		t.Logf("WARNING: every adjudicated pair falls on ONE side of the grade>=2 binarization. "+
			"kappa reads 1.000 by the degenerate 1-pe==0 branch and the FPR denominator is "+
			"empty (%d human-irrelevant pairs). This is U1 — §3c's planted negatives are "+
			"missing from the subsample.", judge.HumanIrrelevant)
	}
	t.Logf("NO ARM NUMBER IS COMPUTED ABOVE THIS LINE. The label hash and the judge gate are " +
		"now in the run log; §6 S5 requires the hash to match what was frozen.")
	judge.LabelHashMatches = strings.EqualFold(
		strings.TrimSpace(os.Getenv("MUNINN_COGTRIAL_LABELS_SHA256")), labels.hash)
	if !judge.LabelHashMatches {
		t.Logf("WARNING: MUNINN_COGTRIAL_LABELS_SHA256 does not match the computed label hash. "+
			"S5 will FAIL. Set it to %s if this IS the frozen set.", labels.hash)
	}

	// --- G1: replay ---------------------------------------------------------
	baseIDs := ids
	baseTotal, baseByType := ctEdgeComposition(t, ctx, store, sc, baseIDs)
	t.Logf("G0 EDGE COMPOSITION: %d edges total", baseTotal)
	ctLogComposition(t, "G0", baseTotal, baseByType)

	replayedEdges := 0
	if mode == "replay" {
		// Only events NOT in the holdout train the graph.
		var train []ctEvent
		var trainBucket []int
		for i, e := range events {
			if held[e.queryHash] {
				continue
			}
			train = append(train, e)
			trainBucket = append(trainBucket, buckets[i])
		}
		t.Logf("REPLAY: %d training events (holdout excluded), %d weekly checkpoints",
			len(train), nBuckets)
		ctReplay(t, ctx, store, sc, train, trainBucket, resolved, func(b int) {
			t.Logf("  checkpoint W%02d applied", b)
		})
		replayedEdges, _ = ctEdgeComposition(t, ctx, store, sc, baseIDs)
		t.Logf("G1 EDGE COMPOSITION: %d edges total (was %d)", replayedEdges, baseTotal)
	}

	// --- the live arms ------------------------------------------------------
	ftsIdx := fts.New(db)
	hnswReg := hnswpkg.NewRegistry(db)
	prov := &embed.LocalProvider{}
	if _, err := prov.Init(ctx, embed.ProviderHTTPConfig{DataDir: t.TempDir()}); err != nil {
		t.Fatalf("local embed provider init (built with -tags localassets, and `make fetch-assets` run?): %v", err)
	}
	defer prov.Close()

	actEngine := activation.New(store, activation.NewFTSAdapter(ftsIdx),
		activation.NewHNSWAdapter(hnswReg), &ctEmbedder{prov: prov})
	defer actEngine.Close()
	actEngine.SetTransitionStore(store.TransitionCache())

	// resolved.ACTRHebScale is ALWAYS populated by auth.ResolvePlasticity (from
	// the preset, or from an explicit override clamped to [0, 50]), so there is
	// no "unset" to fall back from and the value is passed through untouched.
	// The `if hebScale == 0 { hebScale = DefaultACTRHebScale }` guard that stood
	// here was the THIRD instance of the substitution d17a884 removed from two
	// production sites — and the worst-placed one: a vault configured
	// `actr_heb_scale: 0` (cognition deliberately off) would have been MEASURED
	// at 4.0, i.e. the trial would have judged the exact configuration whose
	// behaviour it exists to judge as though it were the opposite one.
	// TestACTRHebScale_NoZeroSubstitutionAnywhere now fails on a fourth.
	hebScale := resolved.ACTRHebScale
	scaleF32 := float32(hebScale)
	if hebScale == 0 {
		t.Logf("NOTE: actr_heb_scale is 0 on this vault — the cognitive prior's Hebbian and " +
			"transition terms are OFF by configuration. FULL and NO-HEBBIAN are therefore the " +
			"same arm here and Delta_H is structurally 0. That is the vault's real behaviour " +
			"and is measured as such; it is NOT substituted with the 4.0 default.")
	}

	runArm := func(query string, pas bool) []ctRow {
		res, err := actEngine.Run(ctx, &activation.ActivateRequest{
			VaultPrefix:    sc.ws,
			VaultID:        wsVaultID(sc.ws),
			Context:        []string{query},
			Threshold:      -1, // diagnostic bypass: score every candidate
			MaxResults:     200,
			HebbianEnabled: resolved.HebbianEnabled,
			PASEnabled:     pas,
			ReadOnly:       true, // COG-11: never write from a measurement
			Weights: &activation.Weights{
				SemanticSimilarity: 0.6,
				FullTextRelevance:  0.4,
				UseACTR:            true,
				ACTRHebScale:       &scaleF32,
			},
		})
		if err != nil {
			t.Fatalf("activation run: %v", err)
		}
		now := time.Now()
		rows := make([]ctRow, 0, len(res.Activations))
		for _, a := range res.Activations {
			c := a.Components
			rows = append(rows, ctRow{
				id:           a.Engram.ID,
				contentMatch: c.ContentMatch,
				baseLevel:    ctBaseLevelM(a.Engram.AccessCount, a.Engram.LastAccess, now),
				hebbian:      c.HebbianBoost,
				transition:   c.TransitionBoost,
				confidence:   c.Confidence,
				gotRaw:       c.Raw,
			})
		}
		return rows
	}

	series := ctNewQuerySeries(ctArmNames)
	selfCheckBad, selfCheckTotal := 0, 0
	unlabeled := 0

	for qi, e := range evalQueries {
		grades := labels.grades[e.queryHash]
		if len(grades) == 0 {
			unlabeled++
			continue
		}
		full := runArm(e.queryText, true)
		noPAS := runArm(e.queryText, false)

		// COG-5, asserted rather than asserted-in-prose: every arm consumes the
		// identical captured contentMatch. A difference here is a bug in the
		// harness, not a result.
		selfCheckBad += ctSelfCheck(full, hebScale, 1e-6)
		selfCheckBad += ctSelfCheck(noPAS, hebScale, 1e-6)
		selfCheckTotal += len(full) + len(noPAS)

		rk := func(rows []ctRow, arm func(ctRow, float64) float64) []string {
			r := ctRank(rows, arm, hebScale)
			if len(r) > poolDepth {
				r = r[:poolDepth]
			}
			return r
		}
		ranked := map[string][]string{
			ctArmNameFull:          rk(full, ctArmRawFull),
			ctArmNameNoPAS:         rk(noPAS, ctArmRawFull),
			ctArmNameNoHebbian:     rk(full, ctArmRawNoHebbian),
			ctArmNameBaseLevelOnly: rk(noPAS, ctArmRawBaseLevelOnly),
			ctArmNameContentOnly:   rk(noPAS, ctArmRawContentOnly),
		}
		// D1: NDCG is UNDEFINED when EVERY POOLED GRADE IS 0 (not merely when
		// nothing is RELEVANT — grade 1 is below the binarization and still
		// carries gain, so an all-grade-1 pool is defined and is scored),
		// and it is undefined for EVERY arm simultaneously (the pool is shared).
		// The definedness is therefore a property of the QUERY, recorded once and
		// applied identically to every arm — which is what makes the exclusion
		// arm-neutral and ungameable.
		defined := true
		for _, name := range ctArmNames {
			v, ok := ctNDCGAt10(ranked[name], grades)
			if !ok {
				defined = false
			}
			series.NDCG[name] = append(series.NDCG[name], v)
			series.MRR[name] = append(series.MRR[name], ctMRR(ranked[name], grades))
		}
		series.Defined = append(series.Defined, defined)
		series.Bucket = append(series.Bucket, evalBucket[qi])
	}

	if unlabeled > 0 {
		t.Logf("NOTE: %d held-out queries had no labels and were SKIPPED (not scored as zero). "+
			"They count against the U2 sample-size gate.", unlabeled)
	}
	t.Logf("RECONSTRUCTION SELF-CHECK ON THE REAL CORPUS: %d/%d candidates disagreed with the "+
		"live pipeline (tolerance 1e-6)", selfCheckBad, selfCheckTotal)
	fidelityOK := selfCheckBad == 0
	if !fidelityOK {
		t.Logf("U3: the offline reconstruction does NOT reproduce the live pipeline on this " +
			"corpus. Every arm number below is fiction. Reporting it anyway, marked, rather " +
			"than hiding it.")
	}

	// --- THE ZERO-RELEVANCE CENSUS, BEFORE ANY DELTA -------------------------
	// D1 item 6. These are recalls that RETURNED items every one of which the
	// judge graded 0 — the 0x29 event is only written when
	// len(items) > 0, so no abstention can be hiding in this count. It is a
	// retrieval-quality number in its own right and the only visible symptom of
	// a too-conservative judge: U1 gates the judge's false-POSITIVE rate and
	// nothing gates its false-negative rate.
	scored := len(series.Defined)
	informative := series.ctInformativeCount()
	zeroRel := series.ctZeroRelevanceCount()
	zrFrac := 0.0
	if scored > 0 {
		zrFrac = float64(zeroRel) / float64(scored)
	}
	t.Logf("")
	t.Logf("ZERO-RELEVANCE CENSUS: %d scored queries = %d informative + %d zero-relevance "+
		"(%.1f%%). Zero-relevance queries are EXCLUDED from every delta and from the U2/U5 "+
		"power gates (D1). They are not abstentions — an abstention cannot appear in this "+
		"population — they are recalls whose every POOLED GRADE IS 0. Not merely 'nothing "+
		"relevant': grade 1 is below the relevance binarization but carries NDCG gain, so an "+
		"all-grade-1 pool is DEFINED, retained and scored.",
		scored, informative, zeroRel, 100*zrFrac)
	if zrFrac > 0.5 {
		t.Logf("NOTE: over half of all scored queries were zero-relevance. That is either a real " +
			"retrieval-quality result or a too-conservative judge, and U1 cannot tell you which " +
			"— it gates the judge's false-POSITIVE rate only. Adjudicate before quoting any delta.")
	}

	// --- the vault result, built by the rule layer ---------------------------
	// ctVaultFromSeries owns the exclusion, so the dilution-invariance property
	// test proves it about the code that actually runs.
	vr := ctVaultFromSeries(sc.label, series, len(distinct), nBuckets, seed)
	vr.BaselineEdges = baseTotal
	vr.ReplayedEdges = replayedEdges
	vr.UnreplayableFrac = ctUnreplayableFrac(baseTotal, baseByType)
	// S6's shuffled-seed null is the instrument-repair increment's arm; until it
	// lands, "not run" is the honest value and K4 will say so.
	vr.ShuffledSeedNull = ctNullNotRun

	t.Logf("")
	t.Logf("%-22s %8s %8s   (means over the %d INFORMATIVE queries)", "ARM", "NDCG@10", "MRR",
		informative)
	for _, name := range ctArmNames {
		t.Logf("%-22s %8.4f %8.4f", name,
			ctMean(ctInformative(series.Defined, series.NDCG[name])),
			ctMean(ctInformative(series.Defined, series.MRR[name])))
	}
	t.Logf("")
	t.Logf("Delta_C  (FULL - CONTENT-MATCH-ONLY) = %+.4f  95%% CI [%+.4f, %+.4f]  sigma_d=%.4f  n=%d",
		vr.DeltaC.Point, vr.DeltaC.CILower, vr.DeltaC.CIUpper, vr.DeltaC.SDOfDiff, vr.DeltaC.N)
	t.Logf("Delta_HP (FULL - BASE-LEVEL-ONLY)    = %+.4f  95%% CI [%+.4f, %+.4f]  <- WHAT A KILL COSTS",
		vr.DeltaHP.Point, vr.DeltaHP.CILower, vr.DeltaHP.CIUpper)
	t.Logf("Delta_H  (FULL - NO-HEBBIAN)         = %+.4f  95%% CI [%+.4f, %+.4f]  MRR %+.4f over N=%d",
		vr.DeltaH.Point, vr.DeltaH.CILower, vr.DeltaH.CIUpper, vr.MRRDeltaH.Point, vr.MRRDeltaH.N)
	t.Logf("Delta_P  (FULL - NO-PAS)             = %+.4f  95%% CI [%+.4f, %+.4f]  MRR %+.4f over N=%d",
		vr.DeltaP.Point, vr.DeltaP.CILower, vr.DeltaP.CIUpper, vr.MRRDeltaP.Point, vr.MRRDeltaP.N)
	t.Logf("REPORTED, GATES NOTHING — Delta_C over ALL %d scored queries (zero-relevance ones "+
		"included as paired 0-vs-0) = %+.4f 95%% CI [%+.4f, %+.4f]. It is the DILUTED number: "+
		"including a point mass at zero shrinks the mean and the SE by the same factor, so the "+
		"t-statistic never moves while the estimate slides across S1's and K1's absolute bars.",
		vr.DeltaCAllQueriesDiluted.N, vr.DeltaCAllQueriesDiluted.Point,
		vr.DeltaCAllQueriesDiluted.CILower, vr.DeltaCAllQueriesDiluted.CIUpper)
	// D5: the NDCG-level additivity relations are a DIAGNOSTIC, not a gate. A
	// break names a net-harmful mechanism; it does not mean the ablation is
	// broken. The arithmetic form of the relation is checked on RAW SCORES by
	// TestCognitionTrial_ArmReconstructionFidelity, and per-candidate on this
	// very corpus by ctSelfCheck above.
	if msgs := ctAdditivityDiagnostic(vr); len(msgs) > 0 {
		for _, m := range msgs {
			t.Logf("DIAGNOSTIC (a finding about the mechanisms; gates nothing): %s", m)
		}
	} else {
		t.Logf("Ablation additivity holds: max(Delta_H, Delta_P) %+.4f <= Delta_HP %+.4f <= "+
			"Delta_C %+.4f. The width of that interval is exactly the information the four "+
			"marginals could not have supplied.",
			math.Max(vr.DeltaH.Point, vr.DeltaP.Point), vr.DeltaHP.Point, vr.DeltaC.Point)
	}
	if vr.DeltaC.SDOfDiff > 0.20 {
		t.Logf("NOTE: sigma_d = %.4f > 0.20. The pre-registered n was computed at sigma_d ~= 0.15; "+
			"at this dispersion n must rise to %.0f or the trial is UNDERPOWERED (U5).",
			vr.DeltaC.SDOfDiff, 7.84*vr.DeltaC.SDOfDiff*vr.DeltaC.SDOfDiff/0.0009)
	}

	// EMPTY BUCKETS ARE OMITTED, NOT ZEROED. ctMean(nil) is 0, so building a
	// dense per-bucket series scores a week with no evaluated queries as a
	// measured Delta_C of exactly 0 and regresses it — which is how six real
	// +0.04 buckets plus six empty ones manufacture a +0.005 slope with a CI
	// lower bound of +0.003 on data that does not exist. See ctBucketDeltas; U6
	// refuses the series outright when too few buckets in the trailing window
	// survive. The guard stays even though D3 demoted S3 to descriptive: an
	// absent bucket is absent data whether or not a verdict depends on it.
	t.Logf("PER-BUCKET BREAKDOWN (DESCRIPTIVE — this is one measurement against the FINAL graph, " +
		"sliced by week; it is NOT a trend across checkpoints and gates nothing since D3):")
	for _, bd := range vr.DeltaCByBucket {
		t.Logf("  bucket W%02d: Delta_C %+.4f over %d queries", bd.Bucket, bd.Mean, bd.NQueries)
	}
	if len(vr.OmittedBuckets) > 0 {
		t.Logf("  %d of %d weekly buckets had NO evaluated query and are OMITTED "+
			"(indexes %v). They are absent data, not a Delta_C of zero.",
			len(vr.OmittedBuckets), nBuckets, vr.OmittedBuckets)
	}

	// --- verdict ------------------------------------------------------------
	// SINGLE VAULT. ctDecide requires three; running it here reports the U2 gate
	// truthfully rather than letting a single-vault run look conclusive. The
	// operator collects three vault results and calls ctDecide once across them.
	t.Logf("")
	t.Logf("VERDICT (THIS VAULT ALONE — the rule requires 3; a single-vault run is U2 by " +
		"construction and is reported as such):")
	t.Logf("%s", ctDecide([]ctVaultResult{vr}, judge, fidelityOK))
	t.Logf("")
	t.Logf("To conclude: run this driver on three owned vaults (labels A/B/C), collect the " +
		"per-vault lines above, and apply ctDecide across all three. No default changes on " +
		"anything but a SHIP or a KILL, and UNDERPOWERED changes nothing at all.")
}

// --- small helpers ----------------------------------------------------------

// ctEmbedder adapts the bundled local provider to activation.Embedder. Nothing
// is written to the clone: assets extract to a temp dir.
type ctEmbedder struct{ prov *embed.LocalProvider }

func (e *ctEmbedder) Embed(ctx context.Context, texts []string) ([]float32, error) {
	return e.prov.EmbedBatch(ctx, texts)
}

func (e *ctEmbedder) Tokenize(text string) []string { return strings.Fields(text) }

// ctHoldoutBit maps a query hash to a deterministic value in [0,1). Seeded so a
// run log reproduces its own split, and stable across G0/G1 so the two graphs
// are judged on identical queries.
func ctHoldoutBit(queryHash string, seed int64) float64 {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%d", queryHash, seed)))
	v := uint64(0)
	for i := 0; i < 8; i++ {
		v = v<<8 | uint64(sum[i])
	}
	return float64(v%1_000_000) / 1_000_000.0
}

func ctLogComposition(t *testing.T, tag string, total int, byType map[storage.RelType]int) {
	t.Helper()
	if total == 0 {
		t.Logf("  %s: no edges", tag)
		return
	}
	types := make([]int, 0, len(byType))
	for rt := range byType {
		types = append(types, int(rt))
	}
	sort.Ints(types)
	for _, rt := range types {
		n := byType[storage.RelType(rt)]
		t.Logf("  %s reltype 0x%04X: %6d (%5.1f%%)%s", tag, rt, n, 100*float64(n)/float64(total),
			ctIf(storage.RelType(rt) != storage.RelRelatesTo, "  <- replay CANNOT produce this class"))
	}
}
