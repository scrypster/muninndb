package trigger

import (
	"math"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/storage/erf"
)

// TestTriggerScore_UnsetLastAccessDoesNotSilencePush is the fifth read-side
// guard site found by the #810 review round, and the only one on the "pushes to
// you when it matters" half of the product promise.
//
// TriggerScore's recency term is worth wRecency = 0.10 of the raw score. It is
// fed storage.EngramMeta straight off disk — worker.go's periodic sweep calls
// w.store.GetMetadata, which is exactly the sentinel-bearing population on a
// vault cloned before #810. With no unset-timestamp guard, daysSince came out
// ~740,000 for BOTH unset shapes (the plain zero time and the 1754 ERF
// overflow), exp(-daysSince * ln2 / 7) underflowed to 0, and the whole term
// vanished:
//
//	threshold=0.85  healthy score=0.9000 fires=true
//	threshold=0.85  zero-time score=0.8000 fires=false
//	threshold=0.85  1754-sentinel score=0.8000 fires=false
//
// The consequence is bounded and deterministic — not the 0.42-vs-0.5 silently
// empty recall — but it is silent: every subscription whose Threshold lands in
// (0.80*confidence, 0.90*confidence] fires on a healthy vault and never fires
// on a cloned one, with no warning on any surface. A never-accessed engram is
// "just written", i.e. maximally recent, which is what the guard restores.
func TestTriggerScore_UnsetLastAccessDoesNotSilencePush(t *testing.T) {
	// Inputs chosen so every non-recency term saturates: raw = 0.35 + 0.25 +
	// 0.20 + 0.00 + 0.10*recency, confidence 1.0. A credited recency term
	// scores 0.90; a silenced one scores 0.80. The threshold sits between them.
	const threshold = 0.85

	newMeta := func(la time.Time) *storage.EngramMeta {
		return &storage.EngramMeta{
			LastAccess:  la,
			Confidence:  1.0,
			Relevance:   1.0,
			AccessCount: 0,
		}
	}
	sub := &Subscription{Threshold: threshold}

	healthy, healthyFires := TriggerScore(sub, newMeta(time.Now()), 1.0, 1.0)
	if !healthyFires {
		t.Fatalf("healthy vault: score=%.4f fires=false, want fires=true — the fixture no longer "+
			"straddles the threshold and proves nothing", healthy)
	}
	if math.Abs(healthy-0.90) > 1e-6 {
		t.Fatalf("healthy score = %.6f, want 0.90 — the weights or saturation inputs moved; "+
			"retune the fixture so the recency term is the only thing in play", healthy)
	}

	for _, tc := range []struct {
		name       string
		lastAccess time.Time
	}{
		{"zero time (never went through the ERF encoder)", time.Time{}},
		{"1754 ERF overflow sentinel (a vault cloned before #810)", time.Unix(0, erf.ZeroTimeSentinelNanos)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !storage.IsUnsetTimestamp(tc.lastAccess) {
				t.Fatalf("fixture %v is not an unset timestamp — the case is vacuous", tc.lastAccess)
			}
			got, fires := TriggerScore(sub, newMeta(tc.lastAccess), 1.0, 1.0)
			if !fires {
				t.Errorf("unset LastAccess: score=%.4f fires=false at threshold %.2f, but the same "+
					"engram on a healthy vault scores %.4f and fires. The wRecency=0.10 term was "+
					"dropped because time.Since(sentinel) is ~740,000 days: a subscription that "+
					"pushes on a normal vault goes permanently silent on a cloned one.", got, threshold, healthy)
			}
			if math.Abs(got-healthy) > 1e-9 {
				t.Errorf("unset LastAccess score = %.6f, healthy score = %.6f: a never-accessed "+
					"engram must be scored as maximally recent, identically to one accessed now",
					got, healthy)
			}
		})
	}
}
