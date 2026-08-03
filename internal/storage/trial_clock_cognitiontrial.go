//go:build cognitiontrial

package storage

import "time"

// TRIAL-ONLY SEAM. Build tag `cognitiontrial`, which CI never passes and no
// release build passes — this file does not compile, let alone run, in any
// shipped binary. Same mechanism as `scoringmeasure`
// (internal/engine/engine_scoring_weights_measure_test.go): the seam is
// STRUCTURALLY unreachable in production rather than merely undocumented.
//
// WHY IT HAS TO EXIST. `PebbleStore.decayNow` is private and already carries
// the comment "test-only seam; never set in production code". In-package tests
// set it directly, but the cognition trial's replay driver lives in another
// package and needs the same control: it replays 90 days of recorded
// co-activations in minutes, and association decay must be evaluated at the
// SIMULATED time of each pass or the reconstruction silently becomes a
// "no forgetting ever" graph.
//
// SetDecayClock sets the clock DecayAssocWeights evaluates against. Pass nil to
// restore wall time. It is read without synchronization by every decay pass, so
// it MUST be set before the store is shared across goroutines — setting it after
// a background worker has started is a data race.
func SetDecayClock(ps *PebbleStore, fn func() time.Time) {
	ps.decayNow = fn
}
