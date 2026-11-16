//go:build !cognitiontrial

package activation

import (
	"time"

	"github.com/scrypster/muninndb/internal/storage"
)

// legacyACTRClock is the cognition-trial control switch (honest clock,
// increment 1). In every shipped build it is the constant false — there is no
// runtime toggle, by design: a correctness fix that can be switched off
// invites switching it off when it misfires (decision record).
func legacyACTRClock() bool { return false }

// legacyComputeACTR exists in the shipped build only so phase 6 compiles
// against one signature. It is unreachable (legacyACTRClock is constant
// false) and, even if reached, discards the cache timestamp: the shipped
// binary contains no ACT-R path that reads the clock.
func legacyComputeACTR(vectorScore, ftsScore, hebbianBoost, transitionBoost float64, eng *storage.Engram,
	_ int64, now time.Time, w resolvedWeights, inTagPool bool) ScoreComponents {
	return computeACTR(vectorScore, ftsScore, hebbianBoost, transitionBoost, eng, now, w, inTagPool)
}
