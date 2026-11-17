package replication

import (
	"testing"
)

// TestPromotion_BeforeBecomingCortexFinishesBeforeTheRoleChanges pins the
// promotion window for the vault counts (STO-30). A follower's applied
// records never move its counts, and a prune that runs as leader on the stale
// count deletes real memories. cmd/muninn resets every count in
// BeforeBecomingCortex, so the hook must have returned before the node
// reports itself leader (which is what opens the write gate and lets the
// prune tick through), and no entry may be applied between the hook and the
// flip.
func TestPromotion_BeforeBecomingCortexFinishesBeforeTheRoleChanges(t *testing.T) {
	for _, withApplier := range []bool{true, false} {
		name := "with applier"
		if !withApplier {
			name = "without applier"
		}
		t.Run(name, func(t *testing.T) {
			coord, _ := newTestCoordinator(t, "replica")
			asFollower(coord)
			if withApplier {
				applyThrough(t, coord, 0, 12)
			} else {
				coord.applier = nil
			}
			var calls int
			var sawLeader, applierFree, hookDone bool
			coord.BeforeBecomingCortex = func(epoch uint64) {
				calls++
				sawLeader = coord.IsLeader()
				if coord.applier != nil && coord.applier.mu.TryLock() {
					applierFree = true
					coord.applier.mu.Unlock()
				}
				hookDone = true
			}
			var doneWhenCortex bool
			coord.OnBecameCortex = func(uint64) { doneWhenCortex = hookDone }

			simulatePromotion(coord, 1)

			if calls != 1 {
				t.Fatalf("BeforeBecomingCortex ran %d times, want once", calls)
			}
			if sawLeader {
				t.Fatal("BeforeBecomingCortex ran after the node already reported itself leader: " +
					"a prune tick in between reads the follower's stale counts")
			}
			if applierFree {
				t.Fatal("BeforeBecomingCortex ran without the applier held: an entry could be applied " +
					"between the count reset and the role change")
			}
			if !coord.IsLeader() {
				t.Fatalf("role = %v after promotion, want primary", coord.Role())
			}
			if !doneWhenCortex {
				t.Fatal("OnBecameCortex ran before BeforeBecomingCortex finished")
			}
		})
	}
}

// A panicking BeforeBecomingCortex does not stop a claimed promotion.
func TestPromotion_BeforeBecomingCortexPanicStillPromotes(t *testing.T) {
	coord, _ := newTestCoordinator(t, "replica")
	asFollower(coord)
	coord.BeforeBecomingCortex = func(uint64) { panic("invented failure") }
	simulatePromotion(coord, 1)
	if !coord.IsLeader() {
		t.Fatalf("role = %v after a panicking hook, want primary", coord.Role())
	}
}
