//go:build cognitiontrial

package activation

import (
	"math"
	"os"
	"time"

	"github.com/scrypster/muninndb/internal/storage"
)

// TRIAL-ONLY SEAM. Build tag `cognitiontrial`, which CI never passes and no
// release build passes. MUNINN_TRIAL_LEGACY_ACTR_CLOCK=1 reproduces the
// pre-honest-clock engine for an A/B control arm: the decaying ACT-R base
// level, its read of the in-memory L1 cache timestamp, and (via
// internal/rankorder, which reads the same variable) the oldest-first tie
// direction at every cut and sort — together, so the control IS the old
// engine. Read once at start-up.
var legacyClock = os.Getenv("MUNINN_TRIAL_LEGACY_ACTR_CLOCK") == "1"

func legacyACTRClock() bool { return legacyClock }

// legacyComputeACTR is the pre-honest-clock computeACTR, verbatim in its
// base-level and reported components.
func legacyComputeACTR(vectorScore, ftsScore, hebbianBoost, transitionBoost float64, eng *storage.Engram,
	lastAccessNs int64, now time.Time, w resolvedWeights, inTagPool bool) ScoreComponents {
	contentMatch, semCal, floored := actrContentMatch(vectorScore, ftsScore, w, inTagPool)
	var lastAccess time.Time
	if lastAccessNs > 0 {
		lastAccess = time.Unix(0, lastAccessNs)
	} else {
		lastAccess = eng.LastAccess
	}
	if storage.IsUnsetTimestamp(lastAccess) {
		lastAccess = now
	}
	const ageFloorDays = 1.0 / (24.0 * 60.0)
	ageDays := math.Max(now.Sub(lastAccess).Hours()/24.0, ageFloorDays)
	n := float64(eng.AccessCount + 1)
	baseLevel := math.Log(n) - w.ACTRDecay*math.Log(math.Max(ageDays, ageFloorDays)/n)
	if baseLevel > bLevelCap {
		baseLevel = bLevelCap
	}
	raw := actrRaw(contentMatch, baseLevel, hebbianBoost, transitionBoost, w)
	conf := float64(eng.Confidence)
	return ScoreComponents{
		SemanticSimilarity:    semCal,
		SemanticSimilarityRaw: vectorScore,
		FullTextRelevance:     ftsScore,
		ContentMatch:          contentMatch,
		ContentMatchFloored:   floored,
		DecayFactor:           math.Max(0.05, math.Exp(-ageDays/math.Max(float64(eng.Stability), 1.0))),
		HebbianBoost:          hebbianBoost,
		TransitionBoost:       transitionBoost,
		AccessFrequency:       math.Log1p(float64(eng.AccessCount)) / math.Log1p(100),
		Recency:               math.Exp(-ageDays * math.Log(2) / 7.0),
		Confidence:            conf,
		Raw:                   raw,
		Final:                 raw * conf,
	}
}
