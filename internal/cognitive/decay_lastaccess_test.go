package cognitive

import (
	"context"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/storage/erf"
)

// TestDecayWorker_UnsetLastAccessIsNeverDecayed pins the guard on the one
// #810-shaped site whose damage would be PERSISTENT rather than per-query:
// processBatch writes its result back via UpdateRelevance.
//
// DecayWorker has no non-test caller today, so this is a pre-wiring guard, not
// a live-bug fix. That is exactly why it is guarded rather than annotated: the
// previous round left a comment telling a future author to add the guard before
// wiring, which is a policy check, and the round after that found a LIVE
// unguarded site (trigger.TriggerScore) the same comment had implicitly
// asserted did not exist. The guard costs one branch; the comment cost a
// defect.
//
// Without it, a candidate carrying either unset shape decays ~740,000 days'
// worth in a single pass and the vault is written down to DefaultFloor.
func TestDecayWorker_UnsetLastAccessIsNeverDecayed(t *testing.T) {
	for _, tc := range []struct {
		name       string
		lastAccess time.Time
	}{
		{"zero time", time.Time{}},
		{"1754 ERF overflow sentinel", time.Unix(0, erf.ZeroTimeSentinelNanos)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !storage.IsUnsetTimestamp(tc.lastAccess) {
				t.Fatalf("fixture %v is not an unset timestamp — the case is vacuous", tc.lastAccess)
			}
			var writes int
			var wroteRelevance float32
			store := &stubDecayStore{
				onUpdateRelevance: func(_ context.Context, _ [8]byte, _ [16]byte, rel, _ float32) error {
					writes++
					wroteRelevance = rel
					return nil
				},
			}
			dw := NewDecayWorker(store)
			batch := []DecayCandidate{{
				WS:          [8]byte{0xAA},
				ID:          [16]byte{1},
				CreatedAt:   tc.lastAccess,
				LastAccess:  tc.lastAccess,
				AccessCount: 0,
				Stability:   DefaultStability,
				Relevance:   1.0,
			}}
			if err := dw.processBatch(context.Background(), batch); err != nil {
				t.Fatalf("processBatch: %v", err)
			}
			if writes != 0 {
				t.Errorf("processBatch wrote relevance %v for an engram whose LastAccess is unset "+
					"(%d write(s)); want the candidate skipped. An unset LastAccess means \"never "+
					"accessed\", not \"accessed 740,000 days ago\" — decaying on it PERSISTS the "+
					"error to disk.", wroteRelevance, writes)
			}
		})
	}
}

// TestDecayWorker_HealthyLastAccessStillDecays is the other half: the guard
// must skip only the unset shapes, never suppress real decay.
func TestDecayWorker_HealthyLastAccessStillDecays(t *testing.T) {
	var writes int
	store := &stubDecayStore{
		onUpdateRelevance: func(_ context.Context, _ [8]byte, _ [16]byte, _, _ float32) error {
			writes++
			return nil
		},
	}
	dw := NewDecayWorker(store)
	batch := []DecayCandidate{{
		WS:         [8]byte{0xAA},
		ID:         [16]byte{1},
		CreatedAt:  time.Now().Add(-48 * time.Hour),
		LastAccess: time.Now().Add(-24 * time.Hour),
		Stability:  DefaultStability,
	}}
	if err := dw.processBatch(context.Background(), batch); err != nil {
		t.Fatalf("processBatch: %v", err)
	}
	if writes != 1 {
		t.Errorf("healthy candidate produced %d relevance writes, want 1 — the guard is over-broad", writes)
	}
}
