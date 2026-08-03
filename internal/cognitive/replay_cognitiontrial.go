//go:build cognitiontrial

package cognitive

import "context"

// TRIAL-ONLY SEAM. Build tag `cognitiontrial` — never passed by CI and never by
// a release build, so this file does not compile in any shipped binary.
//
// ReplayCoActivations applies a batch of recorded co-activation events through
// the REAL learning path, synchronously, bypassing the async worker entirely.
//
// WHY SYNCHRONOUS. The async path would also work — Worker.Stop() flushes the
// pending batch — but a synchronous entry point makes an offline replay
// deterministic BY CONSTRUCTION rather than by argument. The replay driver
// interleaves learning and decay at simulated times; "the worker will have
// flushed by now" is exactly the kind of timing assumption this repo has been
// burned by.
//
// WHY IT IS A ONE-LINER, and what that buys. It delegates to the unexported
// processBatch with no transformation whatsoever, so the replay is not a
// re-implementation of learning that could drift from it — it IS learning. That
// is also why the fidelity test for the replay
// (TestCognitionTrial_ReplayFidelity, `localassets`, so it runs in CI) exercises
// processBatch directly: it can, being in-package, and doing so keeps the
// fidelity check in the default CI job instead of behind a tag CI never passes.
// If this function ever grows a body, that test stops covering it and the
// coverage has to move with it.
//
// The caller supplies the worker so that LTP potentiation state accumulates
// across the whole replay, exactly as it does across a live session. Passing a
// store and constructing a worker per call would reset that state on every
// event.
//
// PRECONDITIONS the replay driver must satisfy, none of which this function can
// check:
//   - `hw` must have been Stop()ped first. NewHebbianWorker auto-starts a run
//     loop; stopping it before any Submit is a no-op flush and leaves this
//     goroutine the only caller of processBatch.
//   - Each event's At must be the recorded event time (that is what makes the
//     replayed edge stamps honest — see AssocWeightUpdate.LastActivatedAt).
//   - Decay must be driven separately at simulated times, via
//     storage.SetDecayClock + DecayAssocWeights. Learning alone reconstructs a
//     graph that never existed.
func ReplayCoActivations(ctx context.Context, hw *HebbianWorker, events []CoActivationEvent) error {
	return hw.processBatch(ctx, events)
}
