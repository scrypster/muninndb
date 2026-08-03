package engine

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/scrypster/muninndb/internal/transport/mbp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMergeGuard_SameStripeLockedOnce verifies that two entity names mapping to the
// same stripe result in a single lock acquisition (no self-deadlock).
func TestMergeGuard_SameStripeLockedOnce(t *testing.T) {
	var g mergeGuard

	// Build a map of stripe → first name seen, then look for a collision.
	// With 256 stripes and up to 10 000 distinct names, birthday probability
	// guarantees a collision well before exhausting the search.
	stripeToName := make(map[uint32]string, mergeGuardStripes)
	var nameA, nameB string
	for i := 0; i < 10_000; i++ {
		name := fmt.Sprintf("collision-search-%d", i)
		idx := g.stripeIndex(name)
		if existing, ok := stripeToName[idx]; ok {
			nameA, nameB = existing, name
			break
		}
		stripeToName[idx] = name
	}
	require.NotEmpty(t, nameA, "birthday paradox guarantees a stripe collision well before 10 000 names")

	// Must not deadlock — same stripe acquired exactly once.
	g.Lock(nameA, nameB)
	g.Unlock(nameA, nameB)
}

// TestMergeGuard_CanonicalOrderNeverDeadlocks verifies that Lock(A,B) and Lock(B,A)
// both acquire the same two stripes in the same canonical order — neither can deadlock.
func TestMergeGuard_CanonicalOrderNeverDeadlocks(t *testing.T) {
	var g mergeGuard

	// Find two names on different stripes.
	var nameA, nameB string
	for i := 0; i < 256; i++ {
		a := fmt.Sprintf("forward-%d", i)
		b := fmt.Sprintf("reverse-%d", i)
		if g.stripeIndex(a) != g.stripeIndex(b) {
			nameA, nameB = a, b
			break
		}
	}
	require.NotEmpty(t, nameA, "could not find two names on different stripes")

	// Acquire in both orders — neither should block because they use canonical ordering.
	done := make(chan struct{}, 2)

	go func() {
		g.Lock(nameA, nameB)
		g.Unlock(nameA, nameB)
		done <- struct{}{}
	}()
	go func() {
		g.Lock(nameB, nameA)
		g.Unlock(nameB, nameA)
		done <- struct{}{}
	}()

	<-done
	<-done
}

// TestMergeGuard_SerialisesConflictingMerges verifies that two concurrent merge-like
// operations on the same entity (A→B and A→C) are serialised: the second operation
// cannot begin its critical section until the first has completed.
//
// Run with -race to catch data races.
func TestMergeGuard_SerialisesConflictingMerges(t *testing.T) {
	var g mergeGuard

	const entityA = "shared-entity"
	const entityB = "target-b"
	const entityC = "target-c"

	// counter is incremented inside the critical section. If two goroutines overlap,
	// the race detector will flag it; if serialised correctly, the count will be 2.
	counter := 0
	var wg sync.WaitGroup

	for _, target := range []string{entityB, entityC} {
		target := target
		wg.Add(1)
		go func() {
			defer wg.Done()
			g.Lock(entityA, target)
			defer g.Unlock(entityA, target)
			counter++ // must be safe because A's stripe is held exclusively
		}()
	}
	wg.Wait()

	assert.Equal(t, 2, counter, "both merge operations must complete exactly once")
}

// TestMergeGuard_ConcurrentUnrelatedMergesDoNotBlock verifies that merges between
// entirely unrelated entity pairs proceed concurrently (no unnecessary serialisation).
func TestMergeGuard_ConcurrentUnrelatedMergesDoNotBlock(t *testing.T) {
	var g mergeGuard

	// Find two pairs that share no stripe.
	type pair struct{ a, b string }
	var p1, p2 pair
	found := false
	for i := 0; i < 256 && !found; i++ {
		for j := i + 1; j < 256 && !found; j++ {
			a1 := fmt.Sprintf("p1-alpha-%d", i)
			b1 := fmt.Sprintf("p1-beta-%d", i)
			a2 := fmt.Sprintf("p2-alpha-%d", j)
			b2 := fmt.Sprintf("p2-beta-%d", j)
			s1a, s1b := g.stripeIndex(a1), g.stripeIndex(b1)
			s2a, s2b := g.stripeIndex(a2), g.stripeIndex(b2)
			if s1a != s2a && s1a != s2b && s1b != s2a && s1b != s2b {
				p1, p2 = pair{a1, b1}, pair{a2, b2}
				found = true
			}
		}
	}
	if !found {
		t.Skip("could not find two fully independent entity pairs")
	}

	// Both critical sections must execute concurrently — signal via channel.
	bothInCritical := make(chan struct{})
	var once sync.Once
	var mu sync.Mutex
	inCritical := 0

	var wg sync.WaitGroup
	for _, p := range []pair{p1, p2} {
		p := p
		wg.Add(1)
		go func() {
			defer wg.Done()
			g.Lock(p.a, p.b)
			defer g.Unlock(p.a, p.b)

			mu.Lock()
			inCritical++
			if inCritical == 2 {
				once.Do(func() { close(bothInCritical) })
			}
			mu.Unlock()

			<-bothInCritical // hold the lock until both are inside
		}()
	}
	wg.Wait()
	// If we reach here without deadlock both goroutines entered concurrently.
}

// TestMergeGuard_IntegrationConcurrentMerge verifies that concurrent MergeEntity
// calls at the engine level targeting the same entity A produce a consistent final
// state — A is merged exactly once, and all its engrams are linked to one target.
//
// Run with -race.
func TestMergeGuard_IntegrationConcurrentMerge(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()

	ctx := context.Background()
	ws := eng.store.ResolveVaultPrefix("default")

	// Write entity A with two engrams, and separate target entities B and C.
	writeEntityEngram(t, eng, "default", "entity A first mention",
		mbp.InlineEntity{Name: "EntityA", Type: "service"})
	writeEntityEngram(t, eng, "default", "entity A second mention",
		mbp.InlineEntity{Name: "EntityA", Type: "service"})
	writeEntityEngram(t, eng, "default", "entity B mention",
		mbp.InlineEntity{Name: "EntityB", Type: "service"})
	writeEntityEngram(t, eng, "default", "entity C mention",
		mbp.InlineEntity{Name: "EntityC", Type: "service"})

	// Fire two conflicting merge operations concurrently.
	// One merges A→B, the other merges A→C.  The guard ensures they are
	// serialised on A's stripe: whichever runs second will find A already
	// state=merged and should return an error (entity not found / already merged).
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, target := range []string{"EntityB", "EntityC"} {
		target := target
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := eng.MergeEntity(ctx, "default", "EntityA", target, false)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)

	// Exactly one merge must succeed; the other must fail (A already merged).
	var successes, failures int
	for err := range errs {
		if err == nil {
			successes++
		} else {
			failures++
		}
	}
	assert.Equal(t, 1, successes, "exactly one concurrent merge must succeed")
	assert.Equal(t, 1, failures, "the second concurrent merge must fail (A already merged)")

	// Entity A must be state=merged with a single canonical target.
	recA, err := eng.store.GetEntityRecord(ctx, ws, "EntityA")
	require.NoError(t, err)
	require.NotNil(t, recA)
	assert.Equal(t, "merged", recA.State)
	assert.NotEmpty(t, recA.MergedInto, "MergedInto must be set to either B or C")
}
