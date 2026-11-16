//go:build cognitiontrial

package hnsw

import "os"

// TRIAL-ONLY SEAM. Build tag `cognitiontrial`, which CI never passes and no
// release build passes. With MUNINN_TRIAL_LEGACY_HNSW_SELECT=1 every index the
// trial binary creates uses the pre-fix rules: the M nearest candidates at
// insert, the keep nearest at back-link pruning, entry-point promotion only on
// a strictly higher level, the load-time entry from key order, and only the
// forward-reachability load checks. Read once at start-up: the arm of a
// running process never changes. Persisted lists are the same format in both
// arms, so one data directory can be served by either.
var legacySelect = os.Getenv("MUNINN_TRIAL_LEGACY_HNSW_SELECT") == "1"

func legacySelectArm() bool { return legacySelect }
