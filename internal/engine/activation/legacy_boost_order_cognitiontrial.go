//go:build cognitiontrial

package activation

import "os"

// TRIAL-ONLY SEAM. Build tag `cognitiontrial`, which CI never passes and no
// release build passes. MUNINN_TRIAL_LEGACY_BOOST_ORDER=1 restores the
// pre-COG-35 order on the ACT-R path — association boosts folded into the
// score that orders the rows, and phase 6 cutting at `limit` before the
// post-pipeline phases — so one commit built twice yields both arms of an A/B
// on the same store. MUNINN_TRIAL_LEGACY_ACTR_CLOCK=1 implies it: that arm is
// the whole pre-honest-clock engine, which ordered by boosted score. Read once
// at start-up: the arm of a running process never changes.
var legacyOrder = os.Getenv("MUNINN_TRIAL_LEGACY_BOOST_ORDER") == "1" || legacyClock

func legacyBoostOrder() bool { return legacyOrder }
