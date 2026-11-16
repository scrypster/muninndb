// Package writetwin holds the pure rule behind the write-time twin check
// (COG-36): the mode switch, the "very close" threshold resolution, the
// per-vault homogeneity guard, the recent-write ring, and the pre-committed
// acceptance verdict the offline replay reports.
//
// It deliberately knows nothing about the engine or a transport. The engine
// (internal/engine/engine_write_twin.go) feeds it vectors and resolves names;
// the MCP layer (internal/mcp/write_twin.go) renders what it found. Keeping
// the rule here lets the live path and the offline replay (replay.go) share
// one definition of "twin" — the replay is only evidence if it runs the rule
// that will ship.
package writetwin

import (
	"math"
	"strings"
)

// Mode is the process-wide switch, read once at startup from
// MUNINN_WRITE_RELATED. Off is the default and costs nothing on any path.
type Mode int

const (
	// Off: no ring, no lookup, no log. Byte-identical responses.
	Off Mode = iota
	// Shadow: compute and LOG/COUNT what would have been said; show nothing.
	Shadow
	// Inline ("1"): Shadow, plus the related block on single muninn_remember
	// responses — only for full-mode credentials, and only in a vault whose
	// operator has opted it in (plasticity write_twin_inline) after that
	// vault's own replay check passed.
	Inline
)

func (m Mode) String() string {
	switch m {
	case Shadow:
		return "shadow"
	case Inline:
		return "1"
	default:
		return "off"
	}
}

// ParseMode reads the MUNINN_WRITE_RELATED value. ok=false means the value was
// not recognised; the caller WARNs and runs Off (an unrecognised switch for a
// feature that writes to responses must not turn it on).
func ParseMode(s string) (m Mode, ok bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "off", "0", "false":
		return Off, true
	case "shadow":
		return Shadow, true
	case "1", "on", "inline":
		return Inline, true
	default:
		return Off, false
	}
}

// Rule constants. Each is a product budget or a scope choice stated in the
// design (v3 §0-§3), not a value tuned on a vault.
const (
	// RingCap bounds the per-vault ring of recent write vectors.
	RingCap = 512
	// MaxShown is the most candidates a block names.
	MaxShown = 2
	// HistoricOverFetch is how many HNSW neighbours the shadow-only historic
	// lookup reads before head resolution (retired versions crowd top-K).
	HistoricOverFetch = 50
	// HistoricKeep is how many historic heads the shadow log records.
	HistoricKeep = 3
	// MaxBlockBytes caps what the related block adds to a response.
	MaxBlockBytes = 1000
	// MaxConceptRunes caps the concept echoed per candidate.
	MaxConceptRunes = 80

	// GuardMinSample is the fewest vectored, non-successor recent writes the
	// homogeneity guard needs before it will call a vault discriminating.
	GuardMinSample = 500
	// GuardMaxSample bounds the guard's per-pass work.
	GuardMaxSample = 1000
	// NagBudget is the most flags per write the push may cost: 5 per 100.
	NagBudget = 0.05

	// OverrideMin / OverrideMax clamp the per-vault threshold override
	// (COG-2: clamped, never rejected).
	OverrideMin = 0.50
	OverrideMax = 0.999
)

// ClampOverride clamps a per-vault threshold override into
// [OverrideMin, OverrideMax]. NaN clamps to OverrideMax (the least intrusive
// value) rather than propagating.
func ClampOverride(v float64) float64 {
	switch {
	case math.IsNaN(v):
		return OverrideMax
	case v < OverrideMin:
		return OverrideMin
	case v > OverrideMax:
		return OverrideMax
	}
	return v
}

// Threshold is the resolved "very close" bar for one vault.
type Threshold struct {
	Value  float64
	Source string // "override" | "model_hint:<model>"
	// OK is false when neither an override nor a model hint exists: the
	// vault runs shadow-only and reports no_model_hint.
	OK bool
}

// ResolveThreshold applies the rule: the per-vault override if set (already
// clamped by plasticity resolution; clamped again here so a caller that
// bypasses resolution cannot slip a wild value in), else the model hint.
// An unregistered model gets NO value — never a guess (COG-26's rule).
func ResolveThreshold(override *float64, model string, hint func(string) (float64, bool)) Threshold {
	if override != nil {
		return Threshold{Value: ClampOverride(*override), Source: "override", OK: true}
	}
	if hint != nil {
		if v, ok := hint(model); ok {
			return Threshold{Value: v, Source: "model_hint:" + model, OK: true}
		}
	}
	return Threshold{}
}

// Guard statuses.
const (
	GuardOK             = "ok"
	GuardColdStart      = "cold_start"
	GuardTooHomogeneous = "too_homogeneous"
)

// GuardVerdict decides whether T discriminates in a vault. rank1 holds, for
// each sampled recent write, the cosine of its nearest EARLIER neighbour
// (NaN when it has none). The statistic is the would-flag rate: the share of
// ordinary writes that would already clear T. It is a rate over at least
// GuardMinSample values, not an extreme quantile, so a handful of templated
// writes cannot move it (v2's q99-of-200 was just the second-largest value).
func GuardVerdict(rank1 []float64, t float64) (status string, share float64) {
	return GuardVerdictMin(rank1, t, GuardMinSample)
}

// GuardVerdictMin is GuardVerdict with the minimum sample size as a
// parameter. Production always passes GuardMinSample; it exists so a test of
// the store wiring need not write 500 engrams.
func GuardVerdictMin(rank1 []float64, t float64, minSample int) (status string, share float64) {
	if len(rank1) < minSample || len(rank1) == 0 {
		return GuardColdStart, 0
	}
	n := 0
	for _, c := range rank1 {
		if !math.IsNaN(c) && c >= t {
			n++
		}
	}
	share = float64(n) / float64(len(rank1))
	if share > NagBudget {
		return GuardTooHomogeneous, share
	}
	return GuardOK, share
}

// Acceptance decisions for the per-vault replay check (v3 §6.i). The rule is
// pre-committed: it is written down before any vault is replayed, and the
// replay prints which clause fired.
const (
	DecisionEnable         = "ENABLE"
	DecisionVeto           = "VETO"
	DecisionDoNotEnable    = "DO_NOT_ENABLE"
	DecisionPendingJudging = "PENDING_JUDGING"

	// MinFlagsToEnable: fewer flags in the replay window means the problem is
	// too small there to measure, or precision would be underpowered.
	MinFlagsToEnable = 40
	// MinPrecisionLB is the Wilson 95% lower bound the judged precision must
	// clear.
	MinPrecisionLB = 0.70
)

// Verdict is the replay's acceptance outcome for one vault.
type Verdict struct {
	Decision string   `json:"decision"`
	Reasons  []string `json:"reasons"`
}

// Decide applies the pre-committed outcomes. judged/positive are the blind
// judgements of flagged pairs (controls excluded); judged=0 means judging has
// not happened yet. Every clause that fires is reported, strongest first.
func Decide(flags, writes int, guardStatus string, judged, positive int) Verdict {
	var veto, dne []string
	rate := 0.0
	if writes > 0 {
		rate = float64(flags) / float64(writes)
	}
	if rate > NagBudget {
		veto = append(veto, "flags exceed the nag budget of 5 per 100 writes")
	}
	if guardStatus == GuardTooHomogeneous {
		veto = append(veto, "homogeneity guard: too_homogeneous")
	}
	if judged > 0 {
		if lb := WilsonLower(positive, judged); lb < MinPrecisionLB {
			veto = append(veto, "precision Wilson lower bound under 0.70")
		}
	}
	if flags < MinFlagsToEnable {
		dne = append(dne, "fewer than 40 flags in the window")
	}
	if guardStatus == GuardColdStart {
		dne = append(dne, "homogeneity guard: cold_start (fewer than 500 vectored writes)")
	}
	switch {
	case len(veto) > 0:
		return Verdict{Decision: DecisionVeto, Reasons: append(veto, dne...)}
	case len(dne) > 0:
		return Verdict{Decision: DecisionDoNotEnable, Reasons: dne}
	case judged == 0:
		return Verdict{Decision: DecisionPendingJudging, Reasons: []string{"rate and volume pass; blind-judge the pairs file, then re-run with --labels"}}
	default:
		return Verdict{Decision: DecisionEnable, Reasons: []string{"all pre-committed clauses pass"}}
	}
}

// WilsonLower is the lower bound of the two-sided 95% Wilson score interval
// for k successes in n trials (the interval Gate 0 reported).
func WilsonLower(k, n int) float64 {
	if n <= 0 {
		return 0
	}
	const z = 1.959963984540054
	p := float64(k) / float64(n)
	nf := float64(n)
	den := 1 + z*z/nf
	center := (p + z*z/(2*nf)) / den
	half := z * math.Sqrt(p*(1-p)/nf+z*z/(4*nf*nf)) / den
	return center - half
}
