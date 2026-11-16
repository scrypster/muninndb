//go:build cognitiontrial

package rankorder

import "os"

// TRIAL-ONLY SEAM. Build tag `cognitiontrial`, which CI never passes and no
// release build passes. With MUNINN_TRIAL_LEGACY_ACTR_CLOCK=1 the trial binary
// reproduces the pre-honest-clock tie direction (oldest first) at every site,
// together with the activation package's legacy ACT-R clock, so one commit
// built twice yields both arms of an A/B. Read once at start-up: the arm of a
// running process never changes.
var legacy = os.Getenv("MUNINN_TRIAL_LEGACY_ACTR_CLOCK") == "1"

func legacyAscending() bool { return legacy }
