package cognitive

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

// collisionBatchError is the shape storage.AssocBatchSkipError presents when
// some skips were STO-15 refusing a move onto another relation's key
// (CollisionSkips) and some were faults. Declared locally, as
// partialBatchError is.
type collisionBatchError struct {
	skipped, collided []int
}

func (e *collisionBatchError) SkippedUpdates() []int { return e.skipped }
func (e *collisionBatchError) CollisionSkips() []int { return e.collided }
func (e *collisionBatchError) Error() string {
	return fmt.Sprintf("skipped %v, of which %v kept their weight", e.skipped, e.collided)
}

type collisionHebbianStore struct {
	*selectiveHebbianStore
	collideSrc map[[16]byte]bool
	faultSrc   map[[16]byte]bool
}

func (s *collisionHebbianStore) UpdateAssocWeightBatch(_ context.Context, updates []AssocWeightUpdate) error {
	var skipped, collided []int
	for i, u := range updates {
		switch {
		case s.collideSrc[u.Src]:
			skipped = append(skipped, i)
			collided = append(collided, i)
		case s.faultSrc[u.Src]:
			skipped = append(skipped, i)
		}
	}
	if len(skipped) > 0 {
		return &collisionBatchError{skipped: skipped, collided: collided}
	}
	return nil
}

// TestProcessBatch_ARelationGuardSkipIsNotAFault: a store skips an update
// whose move would land on a key another relation holds, and the edge keeps
// its weight. That is the guard working, so the worker must not count it in
// Stats().Errors or log it as an ERROR on every flush the pair recurs in. Its
// callback is still suppressed (nothing moved). A real fault in the same
// batch is still reported.
//
// RED before the worker learned CollisionSkips: processBatch returned the
// error for the collision-only batch.
func TestProcessBatch_ARelationGuardSkipIsNotAFault(t *testing.T) {
	ws := [8]byte{7, 7, 7, 7, 7, 7, 7, 7}
	idA, idB, idC, idD := [16]byte{1}, [16]byte{2}, [16]byte{3}, [16]byte{4}
	for _, c := range []struct {
		name      string
		faultToo  bool
		wantError bool
	}{
		{"only a relation guard skip", false, false},
		{"a relation guard skip and a fault", true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			store := &collisionHebbianStore{
				selectiveHebbianStore: newSelectiveHebbianStore(),
				collideSrc:            map[[16]byte]bool{idA: true},
				faultSrc:              map[[16]byte]bool{},
			}
			if c.faultToo {
				store.faultSrc[idC] = true
			}
			var mu sync.Mutex
			fired := map[[16]byte]int{}
			hw := NewHebbianWorkerWithDB(store, nil, func(_ [8]byte, id [16]byte, _ string, _, _ float64) {
				mu.Lock()
				defer mu.Unlock()
				fired[id]++
			})
			defer hw.Stop()

			err := hw.processBatch(context.Background(), twoPairBatch(ws, idA, idB, idC, idD))
			if (err != nil) != c.wantError {
				t.Errorf("processBatch error = %v, want an error: %v", err, c.wantError)
			}
			var collided interface{ CollisionSkips() []int }
			if err != nil && !errors.As(err, &collided) {
				t.Errorf("the reported error lost the skip detail: %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if fired[idA] != 0 {
				t.Errorf("OnWeightUpdate fired for the pair that kept its weight")
			}
			if !c.faultToo && fired[idC] != 1 {
				t.Errorf("OnWeightUpdate fired %d times for the applied pair, want 1", fired[idC])
			}
		})
	}
}
