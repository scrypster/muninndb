package activation

import (
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/storage"
)

// Unit pins for the shadow-capture predicates. These live one layer below the
// engine-level COG-28 suite because two of the properties are not reachable
// through the full pipeline today (tag seeding never surfaces a soft-deleted
// engram, and no transport sets a structured filter alongside a superseded
// candidate) — an end-to-end assertion for them would pass with the guard
// deleted, which is worse than no test.

func shadowEngram(id byte, state storage.LifecycleState, validUntil time.Time, tags ...string) *storage.Engram {
	var u storage.ULID
	u[15] = id
	return &storage.Engram{
		ID: u, Concept: "c", Content: "content", State: state,
		ValidUntil: validUntil, Confidence: 1.0, Stability: 30.0,
		CreatedAt: time.Now(), LastAccess: time.Now(), Tags: tags,
	}
}

// TestHasSupersessionSignature pins exactly which stored states qualify. The
// signature is the cheap pre-filter that decides whether an engram may speak
// for a chain head at all, so widening it silently widens substitution.
func TestHasSupersessionSignature(t *testing.T) {
	now := time.Now()
	closed := now.Add(-time.Hour)
	future := now.Add(time.Hour)

	cases := []struct {
		name string
		eng  *storage.Engram
		want bool
	}{
		{"evolve signature: soft-deleted + closed stamp", shadowEngram(1, storage.StateSoftDeleted, closed), true},
		{"plain forget: soft-deleted, OPEN stamp (trash, not history)", shadowEngram(2, storage.StateSoftDeleted, time.Time{}), false},
		{"link-supersedes signature: active + elapsed stamp", shadowEngram(3, storage.StateActive, closed), true},
		{"active with a still-open window is simply current", shadowEngram(4, storage.StateActive, future), false},
		{"active, no stamp", shadowEngram(5, storage.StateActive, time.Time{}), false},
		{"archived is never a shadow (payload not guaranteed resident)", shadowEngram(6, storage.StateArchived, closed), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasSupersessionSignature(tc.eng, now); got != tc.want {
				t.Errorf("hasSupersessionSignature = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestShadowsEnabled pins the four request-level exclusions.
func TestShadowsEnabled(t *testing.T) {
	asOf := time.Now()
	cases := []struct {
		name string
		req  *ActivateRequest
		w    resolvedWeights
		want bool
	}{
		{"default ACT-R request", &ActivateRequest{Threshold: 0.1}, resolvedWeights{UseACTR: true}, true},
		{"as_of: the predecessor IS the answer", &ActivateRequest{Threshold: 0.1, AsOf: &asOf}, resolvedWeights{UseACTR: true}, false},
		{"include_invalid: history mode", &ActivateRequest{Threshold: 0.1, IncludeInvalid: true}, resolvedWeights{UseACTR: true}, false},
		{"rrf: 'cleared the bar' carries no information", &ActivateRequest{Threshold: 0.001}, resolvedWeights{UseRRFFusion: true}, false},
		{"explain's threshold=-1 diagnostic bypass", &ActivateRequest{Threshold: -1}, resolvedWeights{UseACTR: true}, false},
		{"threshold 0 is a real bar, not a bypass", &ActivateRequest{Threshold: 0}, resolvedWeights{UseACTR: true}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shadowsEnabled(tc.req, tc.w); got != tc.want {
				t.Errorf("shadowsEnabled = %v, want %v", got, tc.want)
			}
		})
	}
}

// scoreAt returns a scoreOne closure yielding a fixed gated value, so these
// tests exercise collectShadowMatches' own admission logic rather than the
// scoring formulas (which are the live paths', unchanged, by construction).
func scoreAt(gated float64) func(scoringCandidate, *storage.Engram) (float64, float64, ScoreComponents) {
	return func(c scoringCandidate, eng *storage.Engram) (float64, float64, ScoreComponents) {
		return gated, gated, ScoreComponents{AbsoluteScore: gated, Final: gated}
	}
}

// THE TAG-POOL BYPASS MUST NOT APPLY TO SHADOWS. On the live path inTagPool
// waives the relevance bar because the filter defines the returned SET. A
// shadow is not in the set — it is evidence — so a tag hit waiving the bar
// would let a chain head be injected on zero aboutness, which is exactly the
// promiscuity COG-28 must not have.
func TestCollectShadowMatches_TagPoolBypassIsNotApplied(t *testing.T) {
	eng := shadowEngram(1, storage.StateSoftDeleted, time.Now().Add(-time.Hour), "chore")
	shadows := map[storage.ULID]*storage.Engram{eng.ID: eng}
	all := []scoringCandidate{{id: eng.ID, inTagPool: true}}

	got := collectShadowMatches(all, shadows, &ActivateRequest{Threshold: 0.1}, scoreAt(0.02))
	if len(got) != 0 {
		t.Fatalf("a sub-threshold TAG-POOL candidate became a shadow (gated 0.02 vs threshold 0.10) — "+
			"inTagPool waives the bar for the returned set, never for evidence that manufactures an injection. got %d", len(got))
	}
	// Control: the same candidate, above the bar, IS a shadow. Without this the
	// assertion above would also hold if collection were simply broken.
	if got := collectShadowMatches(all, shadows, &ActivateRequest{Threshold: 0.1}, scoreAt(0.5)); len(got) != 1 {
		t.Fatalf("control: an above-threshold tag-pool candidate produced %d shadows, want 1", len(got))
	}
}

// A structured (MQL WHERE) predicate the caller applied must exclude a shadow
// too: an injection admitted by a filtered-out engram would land an ungoverned
// row in a structured query's results (#654's class, MQL edition).
func TestCollectShadowMatches_StructuredFilterExcludes(t *testing.T) {
	eng := shadowEngram(1, storage.StateSoftDeleted, time.Now().Add(-time.Hour))
	shadows := map[storage.ULID]*storage.Engram{eng.ID: eng}
	all := []scoringCandidate{{id: eng.ID}}

	req := &ActivateRequest{Threshold: 0.1, StructuredFilter: rejectAllFilter{}}
	if got := collectShadowMatches(all, shadows, req, scoreAt(0.9)); len(got) != 0 {
		t.Fatalf("a structured-filter-excluded engram became a shadow (%d) — a candidate the caller may not see "+
			"does not get to speak through a proxy", len(got))
	}
	req.StructuredFilter = nil
	if got := collectShadowMatches(all, shadows, req, scoreAt(0.9)); len(got) != 1 {
		t.Fatalf("control: without the filter the same candidate produced %d shadows, want 1", len(got))
	}
}

type rejectAllFilter struct{}

func (rejectAllFilter) Match(*storage.Engram) bool { return false }

// The cap bounds hot-path I/O (one reverse-assoc iterator per shadow) and must
// cut the LOWEST-scoring ones deterministically.
func TestCollectShadowMatches_CapKeepsTopScorersDeterministically(t *testing.T) {
	shadows := map[storage.ULID]*storage.Engram{}
	var all []scoringCandidate
	scores := map[storage.ULID]float64{}
	for i := 0; i < shadowMatchCap+8; i++ {
		eng := shadowEngram(byte(i+1), storage.StateSoftDeleted, time.Now().Add(-time.Hour))
		shadows[eng.ID] = eng
		all = append(all, scoringCandidate{id: eng.ID})
		scores[eng.ID] = float64(i+1) / 100.0
	}
	got := collectShadowMatches(all, shadows, &ActivateRequest{Threshold: 0.001},
		func(c scoringCandidate, e *storage.Engram) (float64, float64, ScoreComponents) {
			s := scores[c.id]
			return s, s, ScoreComponents{AbsoluteScore: s, Final: s}
		})
	if len(got) != shadowMatchCap {
		t.Fatalf("cap not applied: got %d shadows, want %d", len(got), shadowMatchCap)
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].Final < got[i].Final {
			t.Fatalf("shadows are not score-descending at %d: %.4f then %.4f", i, got[i-1].Final, got[i].Final)
		}
	}
	// The cut must drop the weakest, not an arbitrary 16.
	want := float64(len(all)-shadowMatchCap+1) / 100.0
	if got[len(got)-1].Final < want-1e-9 {
		t.Errorf("the cap dropped a STRONGER shadow than it kept: weakest kept %.4f, expected >= %.4f", got[len(got)-1].Final, want)
	}
}

// Zero-cost when empty: the default path must not allocate.
func TestCollectShadowMatches_NilWhenNoShadowCandidates(t *testing.T) {
	if got := collectShadowMatches([]scoringCandidate{{}}, nil, &ActivateRequest{Threshold: 0.1}, scoreAt(0.9)); got != nil {
		t.Fatalf("collectShadowMatches allocated on the no-shadow path: %v", got)
	}
}
