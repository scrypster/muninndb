//go:build localassets

package activation_test

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"sort"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/engine/activation"
	"github.com/scrypster/muninndb/internal/index/fts"
	embedpkg "github.com/scrypster/muninndb/internal/plugin/embed"
	"github.com/scrypster/muninndb/internal/storage"
)

// ---------------------------------------------------------------------------
// THE LABELED RECALL QUERY SET — the thing every abstention/floor tuning
// decision in this repo has so far been made without.
//
// WHY THIS FILE EXISTS. b (COG-26's 0.520), the 0.6/0.4 combiner and the 0.1
// engine threshold have each been moved on the evidence of a handful of
// hand-picked probes. A hands-on evaluation then reported four straightforward
// known-answer recall failures — two wrong abstentions on paraphrases, two
// unresponsive answers where the system should have abstained. None of those
// four shapes was represented in any in-tree measurement. A single-number
// harness (abstention_gate_measure_test.go: 12 answerable / 16 unanswerable,
// one difficulty, FTS stubbed empty) cannot tell you WHERE a change helps or
// hurts, so every proposal arrives as an argument rather than a measurement.
//
// This file is the query set and the measuring instrument. It is the
// deliverable whether or not any scoring change ships behind it.
//
// WHAT IS LABELED.
//   48 synthetic engrams across five loosely-related domains (field research
//   operations, programme/release decisions, telemetry & infrastructure,
//   staff policy, lab & hardware). Loosely-related on purpose: a corpus of
//   mutually unrelated notes makes abstention trivially easy and hides the
//   only interesting failure — the topically-adjacent near miss.
//
//   30 ANSWERABLE queries, each with a gold engram, GRADED BY DIFFICULTY:
//     - rqNearVerbatim: the query reuses the stored wording. The easy case;
//       it exists to catch a change that buys hard-paraphrase recall by
//       breaking the cases that already worked.
//     - rqModerate: reworded, some content words survive. Both channels
//       (semantic + lexical) carry partial signal.
//     - rqHard: reformulated with essentially no content-word overlap. The
//       semantic channel is the ONLY channel. This band contains the two
//       reconstructed wrong-abstention failures (rqHard #1 and #2).
//
//   20 UNANSWERABLE queries, in three kinds, because "unanswerable" is not
//   one problem:
//     - rqOOD: out-of-domain nonsense. The easy case (and the only kind the
//       previous harness had).
//     - rqAdjacent: TOPICALLY ADJACENT but unanswerable — the corpus knows
//       about release codenames but not about signing-key passwords. This is
//       the shape that produced "returned a related-but-unresponsive memory";
//       it is the hard case and the one an abstention gate is actually for.
//     - rqStale: present-tense questions about live state ("which sensors are
//       stale right now") whose answer is not, and cannot be, in a corpus of
//       standing facts — the shape left behind after a valid-time filter
//       correctly removes an expired fact. The correct answer is abstention,
//       not the surrounding policy notes.
//
// READ BEFORE QUOTING A NUMBER.
//   - ONE corpus, ONE embed model (bundled bge-small-en-v1.5), 50 queries. The
//     absolute values are this corpus's; the DIFFERENCES between arms are what
//     the file is for. Nothing here licenses tuning a shipped constant — see
//     CLAUDE.md principle 11.
//   - The rqStale labels are a judgment call and the honest place to disagree.
//     Labelling "which sensors are stale at the moment" NONE asserts that
//     returning the standing staleness RULE is a wrong answer to a question
//     about current state. That is the behaviour the evaluation complained
//     about, and it is why the band is broken out separately rather than
//     folded into the headline FPR: a reader who thinks the rule IS a
//     reasonable answer can discount that column and read the rqAdjacent one,
//     where the corpus genuinely does not contain the answer at all.
//   - CandidatesPerIndex is raised to 60 so the whole corpus is in every
//     query's pool. Every miss reported here is therefore a GATE decision, not
//     a retrieval-pool truncation.
//   - Engram IDs are pinned (rqDeterministicULID) because ranking metrics are
//     sensitive to score ties and the engine breaks ties by ID.
//
// PRIVACY. Every engram, query and label in this file is synthetic, authored
// here, and describes no real person, system, credential or organisation. No
// vault data, no user text, nothing copied from anywhere.
// ---------------------------------------------------------------------------

// rqCorpus is the labeled corpus. Order is load-bearing only in that engram
// IDs are derived from the index (see rqBuildFixture) to keep tie-breaking
// deterministic across runs.
var rqCorpus = []struct{ concept, content string }{
	// --- field research operations -----------------------------------------
	{"Offline endurance of field kits", "Field researchers can operate approximately 72 hours offline before the survey kit must be resynchronised with the base station."},
	{"Base station resynchronisation window", "Resynchronisation with the base station takes about forty minutes and must complete before the next survey window opens."},
	{"Survey kit battery swap procedure", "Each survey kit carries two swappable battery packs; the depleted pack is rotated into the solar charger at the end of the daily transect."},
	{"Transect sampling cadence", "Transects are walked twice daily, once at dawn and once two hours before dusk, to capture both activity peaks."},
	{"Specimen labelling convention", "Specimen jars are labelled with the transect code, the ordinal sample number, and the collector's initials, in that order."},
	{"Weather abort criteria for field days", "Field days are aborted when sustained wind exceeds forty knots or visibility drops below two hundred metres."},
	{"Satellite uplink cost policy", "The satellite uplink is billed per megabyte, so bulk imagery is held on the kit and uploaded only from the base station."},
	{"Field trauma kit contents", "Every field team carries a trauma kit with a tourniquet, a splint, and a snakebite pressure bandage."},
	{"Radio check schedule", "Field teams call in a radio check on the hour; two missed checks trigger the overdue-party protocol."},
	{"Camp waste carry-out rule", "All camp waste including greywater is carried out; nothing organic is buried at the survey sites."},

	// --- programme decisions & releases ------------------------------------
	{"Violet Basin siting decision", "After the second review the team chose the Violet Basin site for the northern station, accepting the longer approach road in exchange for year-round access."},
	{"Release codename Amber Kestrel", "The autumn release ships under the codename Amber Kestrel; the codename is public and appears in the changelog."},
	{"Codename rotation policy", "Release codenames rotate through a bird list; a codename is never reused across two consecutive years."},
	{"Deferred decision on the eastern annex", "The eastern annex build was deferred a full cycle pending a geotechnical survey."},
	{"Vendor selection for mast hardware", "Mast hardware was awarded to the second-lowest bidder because the lowest could not meet the galvanising specification."},
	{"Decision log retention", "Decision records are retained for seven years and are never edited in place; corrections are appended as new entries."},
	{"Approval threshold for capital spend", "Capital spend above forty thousand requires two signatures, one of which must be the programme lead."},
	{"Change freeze around releases", "No configuration changes land in the seventy-two hours before a release cut."},
	{"Rollback authority", "Any on-call engineer may roll back a release without further approval; the post-mortem is mandatory."},
	{"Naming of the southern station", "The southern station was named Kettle Flats after the landform, over the objection that it duplicates a nearby toponym."},

	// --- telemetry & infrastructure ----------------------------------------
	{"Telemetry retention tiers", "Raw telemetry is kept hot for fourteen days, then rolled into hourly aggregates held for two years."},
	{"Alert routing after hours", "Alerts raised outside business hours page the on-call rotation directly rather than filing a ticket."},
	{"Sensor heartbeat interval", "Each sensor emits a heartbeat every thirty seconds; three consecutive misses mark the sensor as stale."},
	{"Ingest backpressure behaviour", "When the ingest queue exceeds its high-water mark the collector sheds the lowest-priority streams first."},
	{"Dashboard refresh cadence", "Operations dashboards refresh on a sixty-second cycle; a manual refresh is rate-limited to once per ten seconds."},
	{"Metric naming scheme", "Metric names are dotted, lowercase, and end with the unit, so latency metrics end in milliseconds."},
	{"Clock skew tolerance for ingest", "Samples arriving more than five minutes ahead of the collector clock are rejected as skewed."},
	{"Cold storage export format", "Aggregates exported to cold storage are written as columnar files partitioned by day."},
	{"Sampling rate for high-cardinality series", "High-cardinality series are sampled at one in ten to keep the index within budget."},
	{"Synthetic probe coverage", "Synthetic probes exercise the read path from three regions every five minutes."},

	// --- staff policy -------------------------------------------------------
	{"Remote work expectation", "Staff are expected on site two days a week; the remaining days may be worked remotely."},
	{"Field allowance eligibility", "A field allowance is payable for every night spent away from the home station, claimed on the monthly return."},
	{"Training budget per person", "Each person has an annual training budget that does not roll over into the following year."},
	{"On-call compensation", "On-call weeks are compensated as a flat stipend plus time off in lieu for any night call-out."},
	{"Probation review timing", "New staff have a formal review at three months and again at six months before confirmation."},
	{"Equipment loan agreement", "Personal loans of programme equipment require a signed agreement and are limited to fourteen days."},
	{"Conference travel approval", "Conference travel needs approval from the line manager and, if international, from the programme lead."},
	{"Parental leave top-up", "The programme tops up statutory parental leave to full pay for the first twelve weeks."},
	{"Volunteer day allocation", "Everyone may take two paid volunteer days a year with a recognised conservation organisation."},

	// --- lab & hardware -----------------------------------------------------
	{"Autoclave cycle validation", "The autoclave is validated monthly with a biological indicator run at the longest cycle."},
	{"Cold room temperature alarm", "The cold room alarms if it drifts outside two to eight degrees for more than fifteen minutes."},
	{"Microscope objective cleaning", "Objectives are cleaned only with lens tissue and the approved solvent, never with ethanol."},
	{"Balance calibration interval", "Analytical balances are calibrated by an external service every six months."},
	{"Chemical waste segregation", "Halogenated and non-halogenated solvent waste go into separate labelled carboys."},
	{"Fume hood face velocity", "Fume hood face velocity is checked annually and must fall between 0.4 and 0.6 metres per second."},
	{"Glassware breakage reporting", "Broken glassware is logged so the consumables budget tracks actual attrition."},
	{"Sample freezer inventory audit", "The minus eighty freezers are inventoried each quarter and unlabelled samples are discarded."},
	{"Gas cylinder restraint rule", "Every gas cylinder is chained at two points regardless of size or how briefly it is in the room."},
}

// rqBand grades an answerable query by how much lexical help it gives.
type rqBand int

const (
	rqNearVerbatim rqBand = iota
	rqModerate
	rqHard
)

func (b rqBand) String() string {
	switch b {
	case rqNearVerbatim:
		return "near-verbatim"
	case rqModerate:
		return "moderate"
	default:
		return "hard-paraphrase"
	}
}

var rqBands = []rqBand{rqNearVerbatim, rqModerate, rqHard}

// rqAnswerable: 30 queries with a gold engram each, 10 per difficulty band.
var rqAnswerable = []struct {
	query string
	gold  string
	band  rqBand
}{
	// --- near-verbatim (10) -------------------------------------------------
	{"how long can field researchers operate offline", "Offline endurance of field kits", rqNearVerbatim},
	{"telemetry retention tiers", "Telemetry retention tiers", rqNearVerbatim},
	{"sensor heartbeat interval", "Sensor heartbeat interval", rqNearVerbatim},
	{"what is the release codename Amber Kestrel", "Release codename Amber Kestrel", rqNearVerbatim},
	{"cold room temperature alarm range", "Cold room temperature alarm", rqNearVerbatim},
	{"gas cylinder restraint rule", "Gas cylinder restraint rule", rqNearVerbatim},
	{"approval threshold for capital spend", "Approval threshold for capital spend", rqNearVerbatim},
	{"fume hood face velocity check", "Fume hood face velocity", rqNearVerbatim},
	{"on-call compensation for night call-outs", "On-call compensation", rqNearVerbatim},
	{"the Violet Basin siting decision", "Violet Basin siting decision", rqNearVerbatim},

	// --- moderate paraphrase (10) ------------------------------------------
	{"how many days a week do we have to be in the office", "Remote work expectation", rqModerate},
	{"what happens if a sensor stops sending heartbeats", "Sensor heartbeat interval", rqModerate},
	{"who is allowed to roll back a release", "Rollback authority", rqModerate},
	{"how often are the analytical balances calibrated", "Balance calibration interval", rqModerate},
	{"how should solvent waste be separated", "Chemical waste segregation", rqModerate},
	{"when do we stop making configuration changes before shipping", "Change freeze around releases", rqModerate},
	{"how are specimen jars labelled", "Specimen labelling convention", rqModerate},
	{"what happens when the ingest queue gets too full", "Ingest backpressure behaviour", rqModerate},
	{"how often do the operations dashboards update", "Dashboard refresh cadence", rqModerate},
	{"when is a field day called off because of weather", "Weather abort criteria for field days", rqModerate},

	// --- hard paraphrase, ~zero content-word overlap (10) -------------------
	// #1 and #2 are the two reconstructed wrong-abstention failures.
	{"How long can field researchers work while completely disconnected?", "Offline endurance of field kits", rqHard},
	{"What was the basin decision?", "Violet Basin siting decision", rqHard},
	{"if a couple of scheduled check-ins are missed, what kicks in", "Radio check schedule", rqHard},
	{"can I take a piece of programme kit home for a month", "Equipment loan agreement", rqHard},
	{"does unused money for courses carry into next year", "Training budget per person", rqHard},
	{"what do people get for nights spent away from home", "Field allowance eligibility", rqHard},
	{"how do we keep the bill down when sending pictures back from the bush", "Satellite uplink cost policy", rqHard},
	{"is there any way to correct something already written in an old record", "Decision log retention", rqHard},
	{"what happens to samples nobody put a name on", "Sample freezer inventory audit", rqHard},
	{"how do we stop drowning in too many distinct label combinations", "Sampling rate for high-cardinality series", rqHard},
}

// rqKind grades an unanswerable query by how hard abstaining is.
type rqKind int

const (
	rqOOD rqKind = iota
	rqAdjacent
	rqStale
)

func (k rqKind) String() string {
	switch k {
	case rqOOD:
		return "out-of-domain"
	case rqAdjacent:
		return "topically-adjacent"
	default:
		return "present-tense/stale"
	}
}

var rqKinds = []rqKind{rqOOD, rqAdjacent, rqStale}

// rqUnanswerable: 20 queries whose gold answer is NONE.
var rqUnanswerable = []struct {
	query string
	kind  rqKind
	// note records WHY it is unanswerable and what it is adjacent to, so a
	// future reader can tell a mislabelled query from a real leak.
	note string
}{
	// --- topically adjacent but unanswerable (8) ----------------------------
	// #1 is the reconstructed "deleted password -> returned the release
	// codename memory" failure: the corpus has a codename, and knows nothing
	// about any credential.
	{"what is the password for the release signing key", rqAdjacent, "corpus has a public release CODENAME, no credential of any kind"},
	{"what is the wifi password at the northern station", rqAdjacent, "corpus sites the northern station; no network credentials"},
	{"who signed off the Violet Basin geotechnical report", rqAdjacent, "corpus records the siting decision and a deferred geotech survey, never a signatory"},
	{"what is the serial number of the autoclave", rqAdjacent, "corpus has an autoclave validation policy, no asset identifiers"},
	{"how many dollars is the field allowance per night", rqAdjacent, "corpus records allowance ELIGIBILITY, never a rate"},
	{"what is the maximum wind speed the survey kit antenna can survive", rqAdjacent, "corpus has a wind ABORT criterion for people, not a hardware rating"},
	{"which bird is next on the codename list", rqAdjacent, "corpus says codenames rotate through a bird list, never which bird"},
	{"how much does a replacement battery pack cost", rqAdjacent, "corpus has the battery swap PROCEDURE, no pricing"},

	// --- present-tense / live-state (4) ------------------------------------
	// The shape left behind when a valid-time filter has correctly excluded an
	// expired fact: the standing policy notes remain and must NOT be returned
	// as if they answered a question about right now.
	{"what is the ingest queue depth right now", rqStale, "standing backpressure policy exists; live depth does not"},
	{"which sensors are stale at the moment", rqStale, "standing staleness RULE exists; current sensor status does not"},
	{"which region is currently failing its synthetic probe", rqStale, "standing probe coverage exists; current failures do not"},
	{"what is today's cold room temperature reading", rqStale, "standing alarm band exists; today's reading does not"},

	// --- out-of-domain nonsense (8) ----------------------------------------
	{"seating chart for the amphibian ballet recital", rqOOD, ""},
	{"tasting notes for fermented basalt at the lighthouse gala", rqOOD, ""},
	{"knitting pattern for a nine-sleeved reversible barometer cozy", rqOOD, ""},
	{"warranty terms on a self-folding origami submarine hull", rqOOD, ""},
	{"pruning guide for the ornamental clockspring hedge in winter", rqOOD, ""},
	{"inventory count of left-handed metronomes in the archive annex", rqOOD, ""},
	{"revised bylaws governing the ceremonial polishing of borrowed fog", rqOOD, ""},
	{"nutritional breakdown of powdered moonlight sold by the furlong", rqOOD, ""},
}

// ---------------------------------------------------------------------------
// FIXTURE
// ---------------------------------------------------------------------------

// rqFixedAge pins the fixture's AGE rather than its timestamp, for the reason
// documented at abmFixedAge: pinning an absolute date pushes the ACT-R
// base-level term out of the saturating regime and stops the harness from
// reaching the condition it is meant to measure.
const rqFixedAge = 5 * time.Minute

func rqFixtureNow() time.Time { return time.Now().Add(-rqFixedAge) }

// rqDeterministicULID builds a ULID whose 6-byte timestamp prefix is the
// fixture instant and whose entropy is the corpus index. storage.NewULIDWithTime
// draws real entropy, which makes score ties break differently on every run;
// the ranking metrics here are sensitive to ties, so the IDs are pinned.
func rqDeterministicULID(at time.Time, idx int) storage.ULID {
	var u storage.ULID
	ms := uint64(at.UnixMilli())
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], ms)
	copy(u[0:6], buf[2:8])
	binary.BigEndian.PutUint32(u[12:16], uint32(idx+1))
	return u
}

// rqFixture wires the labeled corpus into a real ActivationEngine.
//
// withFTS selects the measurement CONDITION, and the two conditions answer
// different questions:
//
//   - withFTS=true builds a real internal/index/fts index over the corpus, so
//     ContentMatch's lexical channel carries real, calibrated coverage. This is
//     what production recall actually does and the only condition under which
//     a COMBINER change (how the two channels fuse) can be measured at all.
//   - withFTS=false stubs FTS empty, which is the previous harness's condition:
//     the semantic channel alone. It is retained because it isolates the floor.
type rqFix struct {
	eng       *activation.ActivationEngine
	byConcept map[string]storage.ULID
}

// `now` is passed in rather than taken from rqFixtureNow() inside, because two
// fixtures built for a counterfactual comparison must agree on their engram
// IDs — and the IDs are derived from `now`. (Taking it internally silently
// gave the two fixtures disjoint ID spaces, which made the fidelity check
// compare nothing while reporting a pool-dependence bug that did not exist.)
func rqBuildFixture(t *testing.T, embedder activation.Embedder, svc *embedpkg.EmbedService, withFTS bool, now time.Time) *rqFix {
	t.Helper()
	store := newStubStore()
	vecs := make(map[storage.ULID][]float32, len(rqCorpus))
	byConcept := make(map[string]storage.ULID, len(rqCorpus))

	var ftsIndex activation.FTSIndex = &stubFTS{}
	var ftsIdx *fts.Index
	if withFTS {
		dir := t.TempDir()
		db, err := storage.OpenPebble(dir, storage.DefaultOptions())
		if err != nil {
			t.Fatalf("OpenPebble: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		ftsIdx = fts.New(db)
		ftsIndex = activation.NewFTSAdapter(ftsIdx)
	}

	for i, n := range rqCorpus {
		e := &storage.Engram{
			ID:         rqDeterministicULID(now, i),
			Concept:    n.concept,
			Content:    n.content,
			Confidence: 1.0,
			Stability:  30.0,
			CreatedAt:  now,
		}
		store.writeEngram(e)
		if _, dup := byConcept[n.concept]; dup {
			t.Fatalf("duplicate concept %q in rqCorpus — gold labels must be unique", n.concept)
		}
		byConcept[n.concept] = e.ID

		// Production embeds "Concept + ' ' + Content" (retroactive.go:469).
		vec, err := svc.Embed(context.Background(), []string{n.concept + " " + n.content})
		if err != nil {
			t.Fatalf("embed %q: %v", n.concept, err)
		}
		vecs[e.ID] = vec

		if ftsIdx != nil {
			if err := ftsIdx.IndexEngram([8]byte{}, [16]byte(e.ID), e.Concept, "", e.Content, nil); err != nil {
				t.Fatalf("IndexEngram %q: %v", n.concept, err)
			}
		}
	}

	// Active-session condition, exactly as abmMeasureFixture: a primer engram
	// standing in for "something the agent surfaced a moment ago", with every
	// third corpus engram associated to it. Without this the Hebbian prior is
	// 1.0 everywhere and half the scoring path is untested. Hot set chosen over
	// SORTED concepts so it is the same third on every run.
	primer := &storage.Engram{
		ID:         rqDeterministicULID(now, len(rqCorpus)),
		Concept:    "prior turn of this session",
		Content:    "an unrelated memory surfaced a moment ago",
		Confidence: 1.0, Stability: 30.0, CreatedAt: now,
	}
	store.writeEngram(primer)
	hotKeys := make([]string, 0, len(byConcept))
	for k := range byConcept {
		hotKeys = append(hotKeys, k)
	}
	sort.Strings(hotKeys)
	for i, k := range hotKeys {
		if i%3 == 0 {
			store.assocs[byConcept[k]] = []storage.Association{{TargetID: primer.ID, Weight: 1.0}}
		}
	}

	eng := activation.New(store, ftsIndex, &bruteForceCosineIndex{vecs: vecs}, embedder)
	t.Cleanup(eng.Close)
	eng.AssocLog().Record(activation.LogEntry{
		VaultID: 0, At: now, EngramIDs: []storage.ULID{primer.ID}, Scores: []float64{1.0},
	})
	return &rqFix{eng: eng, byConcept: byConcept}
}

// ---------------------------------------------------------------------------
// PROBE — one pipeline run per query, from which every candidate arm is
// evaluated OFFLINE and EXACTLY.
//
// The probe runs at SemanticBaseline=0 (the identity transform) with the
// threshold effectively disabled, so EVERY candidate reports its raw cosine and
// its lexical coverage — including the ones today's b=0.520 floor zeroes out,
// which are exactly the ones a softer floor would rescue and which a probe at
// b=0.520 could not even see.
//
// The remaining term is the ACT-R contextual prior. It cannot be read back as
// Raw/ContentMatch: the reported Components.Raw has already been multiplied by
// the per-query 1/maxRaw rescale (engine.go pass 2 overwrites Raw AFTER the
// absolute gate is computed), so that ratio is prior x scale, not prior. It is
// instead computed here from the ACT-R definition and the reported boosts:
//
//	prior = softplus(B(M) + ACTRHebScale*(hebbian + transition)) / (1 + ln 2)
//
// with B(M) pinned at its saturation cap. That pin is not an assumption about
// the formula, it is a property of THIS fixture: every engram is 5 minutes old
// (rqFixedAge) and B(M) = ln(n) - d*ln(age/n) exceeds the cap for anything
// younger than ~73 minutes, so B(M) is the cap for every candidate in every
// query. TestRecallQuerySet_ReconstructionFidelity verifies the result against
// the live pipeline rather than trusting either step.
//
// prior is a property of the ENGRAM, not of the combiner, so any candidate
// combiner f can be scored exactly as
//
//	ContentMatch' = f(cos, fts)
//	Raw'          = ContentMatch' * prior
//	Absolute'     = min(Raw', ContentMatch', 1) * Confidence
//
// with ranking by Raw'*Confidence, which is what the pipeline ranks by. No arm
// re-runs the pipeline, every arm sees the identical candidate pool, and
// nothing is simulated. TestRecallQuerySet_ReconstructionFidelity proves the
// reconstruction reproduces the real b=0.520 pipeline bit-for-bit.
// ---------------------------------------------------------------------------

type rqCand struct {
	id          storage.ULID
	concept     string
	cos, fts    float64
	prior, conf float64
	// reported by the pipeline, used only by the fidelity check
	actualCM, actA, actRaw float64
}

// Mirrors of the ACT-R constants in activation/engine.go. Deliberately
// duplicated rather than exported: if engine.go's values drift, the fidelity
// test fails loudly here instead of this harness silently tracking them and
// reporting numbers for a formula nobody reviewed.
const (
	rqACTRDenominator = 1.6931471805599453 // 1 + softplus(0) = 1 + ln 2
	rqACTRHebScale    = 4.0                // resolvedWeights default
)

// rqBLevelCap is engine.go's bLevelCap: the unique B(M) at which
// softplus(B(M)) = actrDenominator, i.e. where raw = ContentMatch exactly.
var rqBLevelCap = math.Log(math.Exp(rqACTRDenominator) - 1)

func rqSoftplus(x float64) float64 { return math.Log(1 + math.Exp(x)) }

// rqPrior reconstructs the ACT-R contextual prior. See the block comment above
// for why B(M) is the cap for every engram in this fixture.
func rqPrior(hebbian, transition float64) float64 {
	return rqSoftplus(rqBLevelCap+rqACTRHebScale*(hebbian+transition)) / rqACTRDenominator
}

func rqProbeAt(t *testing.T, f *rqFix, query string, baseline float64) []rqCand {
	t.Helper()
	res, err := f.eng.Run(context.Background(), &activation.ActivateRequest{
		Context:    []string{query},
		Threshold:  1e-9, // effectively disabled; nothing upstream depends on it
		MaxResults: 200,
		// The default is 30 per index, which would truncate a 49-engram corpus
		// and make "the gold was not returned" ambiguous between the gate and
		// the pool. 60 puts the whole corpus in every query's pool, so every
		// miss reported here is a GATE decision.
		CandidatesPerIndex: 60,
		// COG-32: models a DEFAULT-preset vault, so the primer engram's
		// Hebbian priming reaches phase 4 as it did before the gate landed.
		HebbianEnabled: true,
		Weights: &activation.Weights{
			SemanticSimilarity: 0.6,
			FullTextRelevance:  0.4,
			SemanticBaseline:   float32(baseline),
			UseACTR:            true,
		},
	})
	if err != nil {
		t.Fatalf("Run(%q): %v", query, err)
	}
	out := make([]rqCand, 0, len(res.Activations))
	for _, s := range res.Activations {
		c := s.Components
		out = append(out, rqCand{
			id:       s.Engram.ID,
			concept:  s.Engram.Concept,
			cos:      c.SemanticSimilarityRaw,
			fts:      c.FullTextRelevance,
			prior:    rqPrior(c.HebbianBoost, c.TransitionBoost),
			conf:     c.Confidence,
			actualCM: c.ContentMatch,
			actA:     c.AbsoluteScore,
			actRaw:   c.Raw,
		})
	}
	return out
}

// ---------------------------------------------------------------------------
// CANDIDATE ARMS
// ---------------------------------------------------------------------------

// rqSemCal is the generalised COG-26 rescale with a gamma knee:
//
//	semCal = ((cos - b) / (1 - b))^gamma   clamped at 0
//
// gamma=1 is exactly today's rescaleSemantic. gamma<1 lifts the band just
// above b (softens the knee); gamma>1 suppresses it.
func rqSemCal(cos, b, gamma float64) float64 {
	if b <= 0 || b >= 1 {
		return math.Max(0, cos)
	}
	v := (cos - b) / (1 - b)
	if v <= 0 {
		return 0
	}
	if gamma == 1 {
		return v
	}
	return math.Pow(v, gamma)
}

type rqArm struct {
	name string
	thr  float64
	cm   func(cos, fts float64) float64
}

// rqEffectiveCosCutoff reports the cosine at which this arm's gate opens with
// NO lexical support — the single number that fully determines the arm's
// behaviour on a zero-overlap paraphrase. Computed by bisection on the arm's
// own combiner, so it can never drift from it.
func rqEffectiveCosCutoff(a rqArm) float64 {
	lo, hi := 0.0, 1.0
	if a.cm(hi, 0) < a.thr {
		return math.NaN() // gate never opens on semantics alone
	}
	for i := 0; i < 60; i++ {
		mid := (lo + hi) / 2
		if a.cm(mid, 0) >= a.thr {
			hi = mid
		} else {
			lo = mid
		}
	}
	return hi
}

// rqEffectiveFTSCutoff is the same for the lexical channel with no semantic
// support: the coverage a purely lexical hit needs to survive.
func rqEffectiveFTSCutoff(a rqArm) float64 {
	lo, hi := 0.0, 1.0
	if a.cm(0, hi) < a.thr {
		return math.NaN()
	}
	for i := 0; i < 60; i++ {
		mid := (lo + hi) / 2
		if a.cm(0, mid) >= a.thr {
			hi = mid
		} else {
			lo = mid
		}
	}
	return hi
}

func rqLinear(b, gamma float64) func(cos, fts float64) float64 {
	return func(cos, fts float64) float64 { return 0.6*rqSemCal(cos, b, gamma) + 0.4*fts }
}

func rqNoisyOR(b, gamma float64) func(cos, fts float64) float64 {
	return func(cos, fts float64) float64 {
		s := math.Min(1, rqSemCal(cos, b, gamma))
		f := math.Min(1, fts)
		return 1 - (1-s)*(1-f)
	}
}

func rqMax(b, gamma float64) func(cos, fts float64) float64 {
	return func(cos, fts float64) float64 { return math.Max(rqSemCal(cos, b, gamma), fts) }
}

// rqArms — every candidate is (combiner, floor, threshold) as ONE unit,
// because they are one calibration. An arm that changes the combiner without
// re-deriving the threshold is not a candidate, it is a bug.
//
// The noisy-OR / max arms sit at threshold 0.1667 rather than 0.10 for a
// derived reason, not a tuned one: at the measured bge-small out-of-domain
// noise ceiling (cos 0.596) semCal is (0.596-0.520)/0.480 = 0.1583, and an
// unweighted OR/max passes that straight through. 0.1667 is the smallest
// round threshold above it, i.e. the value that keeps the noise ceiling
// rejected — which happens to reproduce EXACTLY today's semantic-only cutoff
// (cos >= 0.600), so those arms differ from CURRENT only in how a lexical hit
// is treated.
var rqArms = []rqArm{
	{"CURRENT linear b=.520 thr=.10", 0.10, rqLinear(0.520, 1)},

	// (c) threshold-only, for completeness.
	{"T-only linear b=.520 thr=.08", 0.08, rqLinear(0.520, 1)},
	{"T-only linear b=.520 thr=.06", 0.06, rqLinear(0.520, 1)},

	// (a) softened knee / lowered floor. Held at thr=.10 so the floor is the
	// only thing that moved.
	{"Floor  linear b=.480 thr=.10", 0.10, rqLinear(0.480, 1)},
	{"Floor  linear b=.440 thr=.10", 0.10, rqLinear(0.440, 1)},
	{"Knee   linear b=.520 g=0.70 thr=.15", 0.15, rqLinear(0.520, 0.70)},
	{"Knee   linear b=.450 g=1.50 thr=.10", 0.10, rqLinear(0.450, 1.50)},

	// (b) combiner change WITH a re-derived threshold, as a coupled pair.
	{"OR     noisy b=.520 thr=.1667", 0.1667, rqNoisyOR(0.520, 1)},
	{"MAX          b=.520 thr=.1667", 0.1667, rqMax(0.520, 1)},

	// (d) coupled: combiner + floor + threshold together.
	{"OR     noisy b=.480 thr=.1667", 0.1667, rqNoisyOR(0.480, 1)},
	{"OR     noisy b=.520 thr=.1400", 0.1400, rqNoisyOR(0.520, 1)},
}

// rqApply scores and gates one query's candidate pool under one arm, returning
// the kept set in the pipeline's ranking order.
func rqApply(cands []rqCand, a rqArm) []rqCand {
	type sc struct {
		c    rqCand
		rank float64
	}
	kept := make([]sc, 0, len(cands))
	for _, c := range cands {
		cm := a.cm(c.cos, c.fts)
		if cm <= 0 {
			continue
		}
		raw := cm * c.prior
		absolute := math.Min(math.Min(raw, cm), 1.0) * c.conf
		if absolute < a.thr {
			continue
		}
		kept = append(kept, sc{c, raw * c.conf})
	}
	sort.SliceStable(kept, func(i, j int) bool {
		if kept[i].rank != kept[j].rank {
			return kept[i].rank > kept[j].rank
		}
		return string(kept[i].c.id[:]) < string(kept[j].c.id[:])
	})
	out := make([]rqCand, len(kept))
	for i, k := range kept {
		out[i] = k.c
	}
	return out
}

// rqGateValue is the value the threshold is compared against, exposed so the
// FP-margin metric can normalise a leak by its own arm's threshold.
func rqGateValue(c rqCand, a rqArm) float64 {
	cm := a.cm(c.cos, c.fts)
	return math.Min(math.Min(cm*c.prior, cm), 1.0) * c.conf
}

func rqNDCG5(kept []rqCand, gold storage.ULID) float64 {
	for r, c := range kept {
		if r >= 5 {
			break
		}
		if c.id == gold {
			return 1.0 / math.Log2(float64(r)+2.0)
		}
	}
	return 0
}

// ---------------------------------------------------------------------------
// RESULT ACCUMULATION
// ---------------------------------------------------------------------------

type rqBandCell struct {
	n         int
	ndcgSum   float64
	goldFound int
}

type rqKindCell struct {
	n        int
	falsePos int
	depthSum float64
	topSum   float64 // sum of top kept gate value on leaking queries
}

type rqArmResult struct {
	bands map[rqBand]*rqBandCell
	kinds map[rqKind]*rqKindCell
	// goldMissed records, per band, which queries lost their gold entirely —
	// the list a maintainer actually needs when a number moves.
	goldMissed map[rqBand][]string
}

func newRQArmResult() *rqArmResult {
	r := &rqArmResult{
		bands:      map[rqBand]*rqBandCell{},
		kinds:      map[rqKind]*rqKindCell{},
		goldMissed: map[rqBand][]string{},
	}
	for _, b := range rqBands {
		r.bands[b] = &rqBandCell{}
	}
	for _, k := range rqKinds {
		r.kinds[k] = &rqKindCell{}
	}
	return r
}

func (r *rqArmResult) totals() (ndcg, goldPct float64, n int) {
	var sum float64
	var found int
	for _, b := range rqBands {
		c := r.bands[b]
		n += c.n
		sum += c.ndcgSum
		found += c.goldFound
	}
	if n == 0 {
		return 0, 0, 0
	}
	return sum / float64(n), 100 * float64(found) / float64(n), n
}

func (r *rqArmResult) fpr() (pct float64, fp, n int) {
	for _, k := range rqKinds {
		c := r.kinds[k]
		n += c.n
		fp += c.falsePos
	}
	if n == 0 {
		return 0, 0, 0
	}
	return 100 * float64(fp) / float64(n), fp, n
}

// topFPMargin is the mean of (top kept gate value / this arm's threshold) over
// leaking queries. Raw gate values are NOT comparable across arms whose
// thresholds differ — an arm at thr=.1667 leaking at .17 is barely leaking,
// an arm at thr=.10 leaking at .17 is leaking confidently. The margin is the
// comparable quantity, and it is the one the acceptance rule is stated on.
func (r *rqArmResult) topFPMargin(thr float64) float64 {
	var sum float64
	var n int
	for _, k := range rqKinds {
		sum += r.kinds[k].topSum
		n += r.kinds[k].falsePos
	}
	if n == 0 {
		return 0
	}
	return (sum / float64(n)) / thr
}

func (r *rqArmResult) rawTopFP() float64 {
	var sum float64
	var n int
	for _, k := range rqKinds {
		sum += r.kinds[k].topSum
		n += r.kinds[k].falsePos
	}
	if n == 0 {
		return 0
	}
	return sum / float64(n)
}

// rqEvaluate runs the full labeled set through every arm.
func rqEvaluate(t *testing.T, f *rqFix) map[string]*rqArmResult {
	t.Helper()
	results := map[string]*rqArmResult{}
	for _, a := range rqArms {
		results[a.name] = newRQArmResult()
	}

	for _, q := range rqAnswerable {
		gold, ok := f.byConcept[q.gold]
		if !ok {
			t.Fatalf("gold concept %q is not in rqCorpus — label drift", q.gold)
		}
		cands := rqProbeAt(t, f, q.query, 0)
		for _, a := range rqArms {
			r := results[a.name]
			kept := rqApply(cands, a)
			cell := r.bands[q.band]
			cell.n++
			cell.ndcgSum += rqNDCG5(kept, gold)
			hit := false
			for _, c := range kept {
				if c.id == gold {
					hit = true
					break
				}
			}
			if hit {
				cell.goldFound++
			} else {
				r.goldMissed[q.band] = append(r.goldMissed[q.band], q.query)
			}
		}
	}

	for _, q := range rqUnanswerable {
		cands := rqProbeAt(t, f, q.query, 0)
		for _, a := range rqArms {
			r := results[a.name]
			kept := rqApply(cands, a)
			cell := r.kinds[q.kind]
			cell.n++
			if len(kept) > 0 {
				cell.falsePos++
				cell.depthSum += float64(len(kept))
				cell.topSum += rqGateValue(kept[0], a)
			}
		}
	}
	return results
}

func rqReport(t *testing.T, title string, results map[string]*rqArmResult) {
	t.Helper()
	t.Log("")
	t.Logf("================ %s ================", title)
	t.Logf("corpus=%d engrams | answerable=%d (10 per band) | unanswerable=%d (8 adjacent / 4 stale / 8 OOD)",
		len(rqCorpus), len(rqAnswerable), len(rqUnanswerable))
	t.Log("")
	t.Logf("%-38s %7s %7s | %-17s %-17s %-17s | %6s %6s %6s %6s | %6s %6s",
		"arm", "cosCut", "ftsCut", "near-verbatim", "moderate", "hard-paraphrase",
		"FPR", "adj", "stale", "ood", "topFP", "margin")
	t.Logf("%-38s %7s %7s | %-17s %-17s %-17s | %6s %6s %6s %6s | %6s %6s",
		"", "", "", "gold%/ndcg", "gold%/ndcg", "gold%/ndcg", "%", "%", "%", "%", "abs", "xthr")
	for _, a := range rqArms {
		r := results[a.name]
		cells := make([]string, 0, 3)
		for _, b := range rqBands {
			c := r.bands[b]
			cells = append(cells, fmt.Sprintf("%5.1f%% / %.4f",
				100*float64(c.goldFound)/float64(max1(c.n)), c.ndcgSum/float64(max1(c.n))))
		}
		fprPct, _, _ := r.fpr()
		kindPct := func(k rqKind) float64 {
			c := r.kinds[k]
			if c.n == 0 {
				return 0
			}
			return 100 * float64(c.falsePos) / float64(c.n)
		}
		t.Logf("%-38s %7.4f %7.4f | %-17s %-17s %-17s | %5.1f%% %5.1f%% %5.1f%% %5.1f%% | %6.4f %6.2f",
			a.name, rqEffectiveCosCutoff(a), rqEffectiveFTSCutoff(a),
			cells[0], cells[1], cells[2],
			fprPct, kindPct(rqAdjacent), kindPct(rqStale), kindPct(rqOOD),
			r.rawTopFP(), r.topFPMargin(a.thr))
	}

	// The overall line, for continuity with abstention_gate_measure_test.go.
	t.Log("")
	for _, a := range rqArms {
		r := results[a.name]
		ndcg, goldPct, _ := r.totals()
		fprPct, fp, n := r.fpr()
		t.Logf("  %-38s overall NDCG@5=%.4f gold-found=%.1f%%  FPR=%.1f%% (%d/%d)",
			a.name, ndcg, goldPct, fprPct, fp, n)
	}

	// The misses on the CURRENT arm, named. This is the list the evaluation
	// complaint was really about.
	cur := results[rqArms[0].name]
	t.Log("")
	t.Log("  CURRENT arm — answerable queries whose gold was dropped entirely:")
	for _, b := range rqBands {
		for _, q := range cur.goldMissed[b] {
			t.Logf("    [%-15s] %s", b, q)
		}
	}
}

func max1(n int) int {
	if n < 1 {
		return 1
	}
	return n
}

// ---------------------------------------------------------------------------
// THE MEASUREMENT
// ---------------------------------------------------------------------------

// TestMeasureRecallQuerySet reports the labeled set's numbers for every
// candidate arm, in both the FTS-live condition (what production does) and the
// semantic-only condition (what the previous harness measured).
//
// It is a MEASUREMENT, not a gate: it asserts only the invariants that make
// the numbers meaningful (labels resolve, the CURRENT arm is present, no arm
// achieves the degenerate "keep nothing" or "keep everything"). The acceptance
// rule is enforced separately in TestRecallQuerySet_AcceptanceRule so the
// pre-commitment is legible as its own artifact.
func TestMeasureRecallQuerySet(t *testing.T) {
	embedder, svc := realBGEEmbedder(t)

	now := rqFixtureNow()

	ftsLive := rqEvaluate(t, rqBuildFixture(t, embedder, svc, true, now))
	rqReport(t, "FTS LIVE (production condition: both channels carry signal)", ftsLive)

	semOnly := rqEvaluate(t, rqBuildFixture(t, embedder, svc, false, now))
	rqReport(t, "SEMANTIC ONLY (FTS stubbed empty — the previous harness's condition)", semOnly)

	// Sanity: the measurement is only meaningful if the arms actually differ.
	cur, _, _ := ftsLive[rqArms[0].name].totals()
	for _, a := range rqArms[1:] {
		if n, _, _ := ftsLive[a.name].totals(); n == cur {
			t.Logf("NOTE: arm %q is indistinguishable from CURRENT on overall NDCG (%.4f) — "+
				"expected for arms that differ only in the lexical channel when few queries have one", a.name, n)
		}
	}
}

// TestRecallQuerySet_ReconstructionFidelity proves the offline machinery.
//
// Every candidate arm above is evaluated from a SemanticBaseline=0 probe, with
// ContentMatch and the ACT-R prior reconstructed arithmetically. If that
// reconstruction is not exact, every number in the report is fiction. So: run
// the SAME query set against a SECOND, independently built fixture at the real
// b=0.520 and require three things of every candidate of every query.
//
//  1. GATE VALUE. The reconstructed ContentMatch and AbsoluteScore match the
//     pipeline's own reported values. This is what the FPR / gold-found
//     columns depend on.
//  2. POOL INDEPENDENCE. A candidate the probe saw but the real run did not
//     must be one the real run's gate zeroed (reconstructed ContentMatch = 0),
//     never one the pool dropped — otherwise the arms are not seeing the same
//     candidates and the counterfactual is invalid.
//  3. RANKING. The order induced by the reconstructed Raw' = ContentMatch x
//     prior reproduces the pipeline's own returned order. This is the check
//     that actually tests the reconstructed prior: the pipeline's reported Raw
//     is the prior times a per-query 1/maxRaw rescale, and a positive per-query
//     constant cannot change an order — so agreeing on the ORDER is exactly the
//     claim the NDCG@5 column needs, and a wrong ACTRHebScale, a wrong cap
//     assumption or a wrong softplus would all break it.
//
// Two separate fixtures (rather than two passes over one) because eng.Run
// mutates session state — a second pass over the same engine would not be
// comparing like with like.
func TestRecallQuerySet_ReconstructionFidelity(t *testing.T) {
	embedder, svc := realBGEEmbedder(t)
	now := rqFixtureNow()
	probeFix := rqBuildFixture(t, embedder, svc, true, now)
	realFix := rqBuildFixture(t, embedder, svc, true, now)

	queries := make([]string, 0, len(rqAnswerable)+len(rqUnanswerable))
	for _, q := range rqAnswerable {
		queries = append(queries, q.query)
	}
	for _, q := range rqUnanswerable {
		queries = append(queries, q.query)
	}

	const tol = 1e-6
	current := rqArms[0]
	checked, ordered := 0, 0
	for _, q := range queries {
		probe := rqProbeAt(t, probeFix, q, 0)
		live := rqProbeAt(t, realFix, q, semanticBaselineBGE)

		byID := make(map[storage.ULID]rqCand, len(live))
		for _, c := range live {
			byID[c.id] = c
		}
		reconstructed := make(map[storage.ULID]float64, len(probe))
		for _, p := range probe {
			cm := current.cm(p.cos, p.fts)
			reconstructed[p.id] = cm * p.prior * p.conf
			r, ok := byID[p.id]
			if !ok {
				// (2) pool independence.
				if cm > tol {
					t.Errorf("query %q: candidate %q has reconstructed ContentMatch=%.6f but is ABSENT "+
						"from the real b=%.3f run — the candidate pool is not baseline-independent and "+
						"the offline counterfactual is invalid", q, p.concept, cm, semanticBaselineBGE)
				}
				continue
			}
			// (1) gate value.
			checked++
			if math.Abs(cm-r.actualCM) > tol {
				t.Errorf("query %q candidate %q: reconstructed ContentMatch=%.9f, pipeline reported %.9f",
					q, p.concept, cm, r.actualCM)
			}
			if got := rqGateValue(p, current); math.Abs(got-r.actA) > tol {
				t.Errorf("query %q candidate %q: reconstructed AbsoluteScore=%.9f, pipeline reported %.9f "+
					"(cos=%.4f fts=%.4f prior=%.4f)", q, p.concept, got, r.actA, p.cos, p.fts, p.prior)
			}
		}

		// (3) ranking. live is already in the pipeline's returned order.
		for i := 1; i < len(live); i++ {
			hi, hiOK := reconstructed[live[i-1].id]
			lo, loOK := reconstructed[live[i].id]
			if !hiOK || !loOK {
				continue
			}
			ordered++
			if hi < lo-tol {
				t.Errorf("query %q: reconstructed ranking disagrees with the pipeline — %q (recon %.6f) "+
					"is returned above %q (recon %.6f). The reconstructed ACT-R prior is wrong, so every "+
					"NDCG@5 number in this file is wrong with it.",
					q, live[i-1].concept, hi, live[i].concept, lo)
			}
		}
	}
	if checked == 0 || ordered == 0 {
		t.Fatalf("fidelity check compared too little (%d scorings, %d adjacent pairs) — it proves nothing",
			checked, ordered)
	}
	t.Logf("reconstruction fidelity: %d candidate scorings and %d adjacent ranking pairs across %d "+
		"queries reproduced the real b=%.3f pipeline", checked, ordered, len(queries), semanticBaselineBGE)
}

// ---------------------------------------------------------------------------
// THE PRE-COMMITTED ACCEPTANCE RULE
//
// Written down BEFORE the numbers above were looked at. A candidate ships only
// if ALL of the following hold in the FTS-LIVE condition:
//
//	(1) hard-paraphrase gold-found improves by at least +20.0 percentage points
//	    over CURRENT — i.e. at least 2 of the 10 hard queries that abstain today
//	    come back. "Materially" needs a number; this is the number. A +10pp
//	    (one query) move on a 10-query band is one query, not an effect.
//	(2) overall FPR does not exceed CURRENT's by more than 2.0 percentage points.
//	(3) the top-false-positive MARGIN (top kept gate value / that arm's own
//	    threshold, averaged over leaking queries) does not rise above CURRENT's.
//	    Raw gate values are not comparable across arms with different
//	    thresholds; the margin is.
//	(4) neither the near-verbatim nor the moderate band LOSES gold-found. A
//	    change that trades working recall for hard recall is the same defect
//	    from the other side.
//
// If nothing passes, the honest negative result ships and the scoring path is
// left alone. That outcome is a success of this file, not a failure of it.
// ---------------------------------------------------------------------------

const (
	rqRuleHardGain   = 20.0 // percentage points, condition (1)
	rqRuleFPRSlack   = 2.0  // percentage points, condition (2)
	rqRuleMarginSlop = 1e-9 // condition (3) is "does not rise"
)

func TestRecallQuerySet_AcceptanceRule(t *testing.T) {
	embedder, svc := realBGEEmbedder(t)
	results := rqEvaluate(t, rqBuildFixture(t, embedder, svc, true, rqFixtureNow()))

	current := rqArms[0]
	cur := results[current.name]
	curHard := 100 * float64(cur.bands[rqHard].goldFound) / float64(max1(cur.bands[rqHard].n))
	curNear := 100 * float64(cur.bands[rqNearVerbatim].goldFound) / float64(max1(cur.bands[rqNearVerbatim].n))
	curMod := 100 * float64(cur.bands[rqModerate].goldFound) / float64(max1(cur.bands[rqModerate].n))
	curFPR, _, _ := cur.fpr()
	curMargin := cur.topFPMargin(current.thr)

	t.Logf("CURRENT baseline: hard gold-found=%.1f%% near=%.1f%% moderate=%.1f%% FPR=%.1f%% FP-margin=%.2fx",
		curHard, curNear, curMod, curFPR, curMargin)
	t.Logf("rule: hard >= %.1f%% (+%.1fpp), FPR <= %.1f%% (+%.1fpp), margin <= %.2fx, near/moderate not below %.1f%%/%.1f%%",
		curHard+rqRuleHardGain, rqRuleHardGain, curFPR+rqRuleFPRSlack, rqRuleFPRSlack, curMargin, curNear, curMod)

	var passed []string
	for _, a := range rqArms[1:] {
		r := results[a.name]
		hard := 100 * float64(r.bands[rqHard].goldFound) / float64(max1(r.bands[rqHard].n))
		near := 100 * float64(r.bands[rqNearVerbatim].goldFound) / float64(max1(r.bands[rqNearVerbatim].n))
		mod := 100 * float64(r.bands[rqModerate].goldFound) / float64(max1(r.bands[rqModerate].n))
		fpr, _, _ := r.fpr()
		margin := r.topFPMargin(a.thr)

		var fails []string
		if hard < curHard+rqRuleHardGain-1e-9 {
			fails = append(fails, fmt.Sprintf("(1) hard gold-found %.1f%% < %.1f%%", hard, curHard+rqRuleHardGain))
		}
		if fpr > curFPR+rqRuleFPRSlack+1e-9 {
			fails = append(fails, fmt.Sprintf("(2) FPR %.1f%% > %.1f%%", fpr, curFPR+rqRuleFPRSlack))
		}
		if margin > curMargin+rqRuleMarginSlop {
			fails = append(fails, fmt.Sprintf("(3) FP-margin %.2fx > %.2fx", margin, curMargin))
		}
		if near < curNear-1e-9 || mod < curMod-1e-9 {
			fails = append(fails, fmt.Sprintf("(4) near/moderate %.1f%%/%.1f%% below %.1f%%/%.1f%%", near, mod, curNear, curMod))
		}
		if len(fails) == 0 {
			passed = append(passed, a.name)
			t.Logf("PASS %-38s hard=%.1f%% FPR=%.1f%% margin=%.2fx", a.name, hard, fpr, margin)
		} else {
			t.Logf("fail %-38s %v", a.name, fails)
		}
	}

	// COMPLETENESS, in the other direction. The rule above was pre-committed
	// against the hypothesis that the dominant defect is FALSE ABSTENTION on
	// hard paraphrases. The measurement says otherwise (see the FPR-by-kind
	// columns), so a rule aimed only at recall could "correctly" reject a
	// candidate that fixed the real problem. The goalposts are NOT moved —
	// what ships is still decided by the pre-committed rule — but a strictly
	// DOMINATING arm (no metric worse, at least one better) would be a free
	// win in any direction, and its absence has to be established rather than
	// assumed. This block reports it; it does not gate on it.
	var dominating []string
	for _, a := range rqArms[1:] {
		r := results[a.name]
		hard := 100 * float64(r.bands[rqHard].goldFound) / float64(max1(r.bands[rqHard].n))
		near := 100 * float64(r.bands[rqNearVerbatim].goldFound) / float64(max1(r.bands[rqNearVerbatim].n))
		mod := 100 * float64(r.bands[rqModerate].goldFound) / float64(max1(r.bands[rqModerate].n))
		fpr, _, _ := r.fpr()
		margin := r.topFPMargin(a.thr)
		// NDCG is part of the comparison, not just gold-found. An arm can keep
		// every gold answer and still bury it below three near-misses; without
		// this the dominance report calls that a free win.
		noWorse := hard >= curHard-1e-9 && near >= curNear-1e-9 && mod >= curMod-1e-9 &&
			fpr <= curFPR+1e-9 && margin <= curMargin+1e-9
		better := hard > curHard+1e-9 || fpr < curFPR-1e-9 || margin < curMargin-1e-9
		for _, b := range rqBands {
			cc, rc := cur.bands[b], r.bands[b]
			curN := cc.ndcgSum / float64(max1(cc.n))
			armN := rc.ndcgSum / float64(max1(rc.n))
			if armN < curN-1e-9 {
				noWorse = false
			}
			if armN > curN+1e-9 {
				better = true
			}
		}
		if noWorse && better {
			dominating = append(dominating, a.name)
		}
	}
	if len(dominating) == 0 {
		t.Log("no candidate arm DOMINATES current either (every arm that improves one metric loses " +
			"another) — the trade is real, not an artifact of which direction the rule was written for")
	} else {
		t.Logf("NOTE: %v dominate(s) CURRENT on every metric measured here. It still does NOT ship on "+
			"this evidence, and the reasons are worth writing down: it fails the pre-committed rule "+
			"(no material hard-paraphrase gain), it leaves the actual defect untouched (the adjacent "+
			"and present-tense false positives are unchanged), and its gain is confined to ranking "+
			"quality and leak confidence on ONE 50-query synthetic set. Adopting a floor/exponent "+
			"because it won on one corpus is exactly the failure CLAUDE.md principle 11 names — a "+
			"number tuned on one sample vault imposing that vault's shape on every other. It is "+
			"recorded here as the one candidate worth re-deriving properly, per-vault, if this line "+
			"of work is picked up.", dominating)
	}

	if len(passed) == 0 {
		t.Log("")
		t.Log("VERDICT: no candidate passes the pre-committed rule. The honest negative result is the " +
			"finding, and it has two parts. (i) On the recall side, the semantic-only gate is a single " +
			"effective COSINE CUTOFF (the cosCut column) — combiner SHAPE is irrelevant when the lexical " +
			"channel is silent, so every floor/knee/threshold variant just slides along one ROC and the " +
			"hard-paraphrase recall it buys is paid for immediately in false positives. (ii) The bigger " +
			"finding is that the recall side was the wrong side: at the shipped calibration hard-paraphrase " +
			"gold-found is already high, while the false-positive rate on TOPICALLY-ADJACENT and " +
			"PRESENT-TENSE queries is 87.5% and 100%. The 6.2% FPR quoted from the previous harness is a " +
			"measurement on out-of-domain nonsense only — the easy kind — and it does not generalise to " +
			"the kind of unanswerable query an agent actually asks. No arm in this family fixes that " +
			"either. Nothing ships behind an argument; if a future candidate passes, this test names it.")
		return
	}
	t.Log("")
	t.Logf("VERDICT: %d candidate(s) pass the pre-committed rule: %v. Landing one requires the coupled "+
		"change (combiner + floor + threshold together) and a re-derivation of COG-26's b commentary.",
		len(passed), passed)
}

// TestRecallQuerySet_ReportedFailureShapes runs the four specific shapes the
// hands-on evaluation reported and records what the SHIPPED calibration
// actually does with each, one query at a time. The aggregate table above says
// where the failures live; this says whether the reported ones reproduce.
//
// Two of the four are asserted (they pass today, and this is the regression
// guard). Two are logged, not asserted: they are open defects at the shipped
// calibration, and a test that fails on a known unfixed defect is a permanently
// red test, which teaches a team to ignore it. The aggregate FPR columns in
// TestMeasureRecallQuerySet are where those two are actually tracked.
func TestRecallQuerySet_ReportedFailureShapes(t *testing.T) {
	embedder, svc := realBGEEmbedder(t)
	f := rqBuildFixture(t, embedder, svc, true, rqFixtureNow())
	current := rqArms[0]

	find := func(query string) []rqCand { return rqApply(rqProbeAt(t, f, query, 0), current) }
	rankOf := func(kept []rqCand, gold storage.ULID) int {
		for i, c := range kept {
			if c.id == gold {
				return i + 1
			}
		}
		return -1
	}

	// (1) and (2): the two reported WRONG ABSTENTIONS. Both are reconstructed
	// in the hard-paraphrase band. Both are answered correctly here — which is
	// itself a finding: the reported failures are not reproducible from the
	// query shape alone, so whatever produced them was a property of that
	// vault's content (competing near-neighbours) rather than of the floor.
	for _, tc := range []struct{ query, gold string }{
		{"How long can field researchers work while completely disconnected?", "Offline endurance of field kits"},
		{"What was the basin decision?", "Violet Basin siting decision"},
	} {
		kept := find(tc.query)
		r := rankOf(kept, f.byConcept[tc.gold])
		if r < 1 {
			t.Errorf("REGRESSION: %q no longer returns %q at the shipped calibration (%d results kept). "+
				"This query answered correctly when the labeled set was built; a floor or threshold "+
				"change has re-introduced the reported wrong-abstention.", tc.query, tc.gold, len(kept))
			continue
		}
		t.Logf("wrong-abstention shape ANSWERED: %-60q -> %s at rank %d", tc.query, tc.gold, r)
	}

	// (3) and (4): the two reported UNRESPONSIVE ANSWERS, where abstention was
	// the correct behaviour. Both still leak.
	for _, q := range []string{
		"what is the password for the release signing key",
		"what is the ingest queue depth right now",
	} {
		kept := find(q)
		if len(kept) == 0 {
			t.Logf("FIXED: %q now abstains — update this test and the FPR columns above", q)
			continue
		}
		t.Logf("OPEN DEFECT, unanswerable query returns %d result(s): %-50q -> top=%q gate=%.4f "+
			"(threshold %.4f)", len(kept), q, kept[0].concept, rqGateValue(kept[0], current), current.thr)
	}
}
