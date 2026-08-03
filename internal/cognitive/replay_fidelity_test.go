//go:build localassets

package cognitive

import (
	"context"
	"math"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// TestCognitionTrial_ReplayFidelity — the empirical basis of the cognition
// trial's replay argument, and its pre-registered U3 stop condition: if this
// fails, the reconstruction is not the thing it claims to be and no arm number
// may be quoted.
//
// THE CLAIM UNDER TEST. 90 days of recorded recall events can be replayed
// through the real learning path, in minutes, and produce the same association
// weights the live worker would have produced — because
//
//   - processBatch reads no clock, and
//   - its update is log-space multiplicative
//     (logNew = log(current) + signal*log(1+lr)), so exponents ADD and chunking
//     and ordering cannot change the final weight.
//
// That claim is what makes the replay legitimate rather than hand-waving, and
// it is asserted here rather than argued: the same co-activation sequence is
// driven (a) through the live async worker and (b) synchronously one event at a
// time, the way the replay driver does, and the resulting weights must agree.
//
// It also pins the second half of the replay's honesty: the replayed edges must
// carry the EVENT's time, not the wall clock. A replay that does not replay the
// forgetting fabricates a graph that never existed.
//
// This test runs in CI (`localassets`) rather than behind the `cognitiontrial`
// tag on purpose — it is synthetic, in-memory and costs milliseconds, and the
// trial's stop conditions must be checkable without a real vault.
// ReplayCoActivations itself is a no-transformation delegation to the
// processBatch driven here; see its comment.
//
// PRIVACY: every identifier and score below is synthetic and authored here.
// ---------------------------------------------------------------------------

// replayTrialWS is an arbitrary synthetic vault prefix.
var replayTrialWS = [8]byte{0x5A, 0x11, 0x02, 0x77, 0x30, 0x0C, 0xEE, 0x41}

// replayTrialEvents builds a deterministic co-activation sequence spread over
// 12 weeks, with overlapping engram sets so pairs accumulate across events —
// which is the case where batching could distort a weight if the update were
// not log-space multiplicative.
func replayTrialEvents() []CoActivationEvent {
	base := time.Unix(1_700_000_000, 0).UTC() // fixed instant: no wall-clock dependence
	ids := make([][16]byte, 6)
	for i := range ids {
		ids[i] = [16]byte{byte(0x10 + i)}
	}
	// (engram index triples, score triples) per event; hand-authored so the
	// overlap pattern is visible rather than generated.
	plan := []struct {
		idx    [3]int
		scores [3]float64
	}{
		{[3]int{0, 1, 2}, [3]float64{0.91, 0.83, 0.62}},
		{[3]int{1, 2, 3}, [3]float64{0.77, 0.71, 0.55}},
		{[3]int{0, 2, 4}, [3]float64{0.88, 0.66, 0.49}},
		{[3]int{2, 3, 5}, [3]float64{0.74, 0.58, 0.44}},
		{[3]int{0, 1, 3}, [3]float64{0.95, 0.80, 0.51}},
		{[3]int{1, 4, 5}, [3]float64{0.69, 0.53, 0.47}},
		{[3]int{0, 1, 2}, [3]float64{0.86, 0.79, 0.60}},
		{[3]int{3, 4, 5}, [3]float64{0.64, 0.57, 0.42}},
		{[3]int{0, 3, 4}, [3]float64{0.90, 0.61, 0.50}},
		{[3]int{1, 2, 5}, [3]float64{0.73, 0.68, 0.45}},
		{[3]int{0, 2, 3}, [3]float64{0.87, 0.65, 0.54}},
		{[3]int{2, 4, 5}, [3]float64{0.70, 0.52, 0.46}},
	}
	out := make([]CoActivationEvent, 0, len(plan))
	for i, p := range plan {
		ev := CoActivationEvent{
			WS: replayTrialWS,
			At: base.Add(time.Duration(i) * 7 * 24 * time.Hour),
		}
		for k := 0; k < 3; k++ {
			ev.Engrams = append(ev.Engrams, CoActivatedEngram{ID: ids[p.idx[k]], Score: p.scores[k]})
		}
		out = append(out, ev)
	}
	return out
}

func replayTrialWeights(store *recordingHebbianStore) map[[32]byte]float32 {
	store.mu.Lock()
	defer store.mu.Unlock()
	out := make(map[[32]byte]float32, len(store.weights))
	for k, v := range store.weights {
		out[k] = v
	}
	return out
}

// replayTrialStamps returns the last LastActivatedAt written per pair.
func replayTrialStamps(store *recordingHebbianStore) map[[32]byte]int32 {
	store.mu.Lock()
	defer store.mu.Unlock()
	out := make(map[[32]byte]int32)
	for _, b := range store.batches {
		for _, u := range b {
			out[pairKeyBytes(u.Src, u.Dst)] = u.LastActivatedAt
		}
	}
	return out
}

func TestCognitionTrial_ReplayFidelity(t *testing.T) {
	events := replayTrialEvents()

	// ARM (a): the live path. Submit every event to a running worker and let
	// Stop() flush the pending batch — the production shape, in which many
	// events land in ONE processBatch call.
	liveStore := newRecordingHebbianStore()
	live := NewHebbianWorker(liveStore) // auto-starts its run loop
	for _, ev := range events {
		live.Submit(ev)
	}
	live.Stop() // flushes the pending batch (worker.go)

	// ARM (b): the replay path. One event per processBatch call, synchronously,
	// exactly what ReplayCoActivations does — the maximally-fragmented batching
	// the argument has to survive.
	replayStore := newRecordingHebbianStore()
	replay := NewHebbianWorker(replayStore)
	// Stop the auto-started run loop FIRST (nothing has been submitted, so the
	// flush is a no-op) so this goroutine is the only one calling processBatch.
	// The replay driver does the same: determinism by construction.
	replay.Stop()
	for _, ev := range events {
		if err := replay.processBatch(context.Background(), []CoActivationEvent{ev}); err != nil {
			t.Fatalf("processBatch: %v", err)
		}
	}

	liveW := replayTrialWeights(liveStore)
	replayW := replayTrialWeights(replayStore)

	if len(liveW) == 0 {
		t.Fatal("the live arm learned nothing — the fixture proves nothing")
	}
	if len(liveW) != len(replayW) {
		t.Fatalf("edge COUNT differs: live %d, replay %d — the replay did not see the same pairs",
			len(liveW), len(replayW))
	}
	const tol = 1e-4
	for k, lw := range liveW {
		rw, ok := replayW[k]
		if !ok {
			t.Errorf("pair %x present in the live arm and absent from the replay", k[:8])
			continue
		}
		if math.Abs(float64(lw)-float64(rw)) > tol {
			t.Errorf("pair %x: live weight %.9f vs replay %.9f (|d| > %g) — batching "+
				"changed what was learned, so the log-space multiplicative argument "+
				"the replay rests on does not hold", k[:8], lw, rw, tol)
		}
	}

	// The stamps. Every replayed edge must carry the recorded event time of the
	// LAST event that touched it, never the wall clock.
	wantStamp := make(map[[32]byte]int32)
	for _, ev := range events {
		for i := 0; i < len(ev.Engrams); i++ {
			for j := i + 1; j < len(ev.Engrams); j++ {
				pk := canonicalPair(ev.Engrams[i].ID, ev.Engrams[j].ID)
				k := pairKeyBytes(pk.a, pk.b)
				if at := int32(ev.At.Unix()); at > wantStamp[k] {
					wantStamp[k] = at
				}
			}
		}
	}
	gotStamp := replayTrialStamps(replayStore)
	now := int32(time.Now().Unix())
	for k, want := range wantStamp {
		got, ok := gotStamp[k]
		if !ok {
			t.Errorf("pair %x never written by the replay", k[:8])
			continue
		}
		if got != want {
			t.Errorf("pair %x: LastActivatedAt %d, want %d (the recorded event time)", k[:8], got, want)
		}
		if got >= now-60 {
			t.Errorf("pair %x: LastActivatedAt %d is wall-clock-recent — a replay that "+
				"stamps everything 'now' erases the interleaved forgetting and "+
				"fabricates a graph that never existed", k[:8], got)
		}
	}
}
