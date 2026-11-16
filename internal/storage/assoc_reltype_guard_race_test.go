package storage

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// TestWriteAssociation_ConcurrentRelTypeCollisionNeverBothSucceed pins STO-15
// under concurrency. Two writers put DIFFERENT RelTypes on the same
// (src, weight, dst) key at the same moment — in production, the write-time
// novelty worker's automatic `refines` edge and a caller's declared
// `supersedes` on the same pair. The guard read the key and then wrote, with
// nothing holding the two steps together, so both writers could pass the read,
// both were told OK, and the later commit silently replaced the earlier edge.
// When `refines` replaced `supersedes`, recall's version-head walk found no
// successor and presented a superseded memory as the current one.
//
// Exactly one of the two may succeed; the other must get
// ErrAssocRelTypeCollision; and the edge on disk must be the winner's.
func TestWriteAssociation_ConcurrentRelTypeCollisionNeverBothSucceed(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("reltype-race")
	now := time.Now()

	const rounds = 400
	bothOK, neitherOK, wrongSurvivor := 0, 0, 0
	for i := 0; i < rounds; i++ {
		src, dst := NewULID(), NewULID()
		seedEndpoints(t, store, ws, src, dst)

		rels := [2]RelType{RelSupersedes, RelRefines}
		var errs [2]error
		start := make(chan struct{})
		var wg sync.WaitGroup
		for w := 0; w < 2; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				<-start
				errs[w] = store.WriteAssociation(ctx, ws, src, dst, &Association{
					TargetID: dst, RelType: rels[w], Weight: 1.0, Confidence: 1.0, CreatedAt: now,
				})
			}(w)
		}
		close(start)
		wg.Wait()

		for w := 0; w < 2; w++ {
			if errs[w] != nil && !errors.Is(errs[w], ErrAssocRelTypeCollision) {
				t.Fatalf("round %d: writer %d: unexpected error %v", i, w, errs[w])
			}
		}
		switch {
		case errs[0] == nil && errs[1] == nil:
			bothOK++
			continue
		case errs[0] != nil && errs[1] != nil:
			neitherOK++
			continue
		}
		winner := rels[0]
		if errs[0] != nil {
			winner = rels[1]
		}
		rev, err := store.GetReverseAssociations(ctx, ws, dst, 0)
		if err != nil {
			t.Fatalf("round %d: GetReverseAssociations: %v", i, err)
		}
		if len(rev) != 1 || rev[0].RelType != winner {
			wrongSurvivor++
		}
	}
	if bothOK > 0 || neitherOK > 0 || wrongSurvivor > 0 {
		t.Fatalf("of %d rounds: both writers told OK (one edge silently replaced) = %d, both refused = %d, "+
			"stored edge is not the winner's = %d; want 0, 0, 0", rounds, bothOK, neitherOK, wrongSurvivor)
	}
}

// TestBatchWriteAssociation_CollisionCommittedInBetweenIsRefused is the same
// gap on the batch path, where it is wider: a StoreBatch checks the collision
// when the edge is QUEUED and writes it at Commit. An edge of a different
// RelType that commits on the same (src, weight, dst) key in between used to
// be replaced by the batch with no error from either write. No goroutines are
// needed to show it.
func TestBatchWriteAssociation_CollisionCommittedInBetweenIsRefused(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("reltype-batch-race")
	now := time.Now()

	src, dst := NewULID(), NewULID()
	seedEndpoints(t, store, ws, src, dst)

	batch := store.NewBatch()
	defer batch.Discard()
	if err := batch.WriteAssociation(ctx, ws, src, dst, &Association{
		TargetID: dst, RelType: RelSupersedes, Weight: 1.0, Confidence: 1.0, CreatedAt: now,
	}); err != nil {
		t.Fatalf("queue supersedes in the batch: %v", err)
	}

	// Another writer commits a different RelType on the same key first.
	if err := store.WriteAssociation(ctx, ws, src, dst, &Association{
		TargetID: dst, RelType: RelRefines, Weight: 1.0, Confidence: 1.0, CreatedAt: now,
	}); err != nil {
		t.Fatalf("direct refines write: %v", err)
	}

	err := batch.Commit()
	if !errors.Is(err, ErrAssocRelTypeCollision) {
		t.Fatalf("batch.Commit err = %v; want ErrAssocRelTypeCollision — the committed refines edge was silently replaced", err)
	}
	rev, gerr := store.GetReverseAssociations(ctx, ws, dst, 0)
	if gerr != nil {
		t.Fatalf("GetReverseAssociations: %v", gerr)
	}
	if len(rev) != 1 || rev[0].RelType != RelRefines {
		t.Fatalf("edges after the refused commit: %+v; want exactly the committed refines edge", rev)
	}
}

// TestBatchCommit_HoldsRelTypeGuardThroughCommit pins the third piece: the
// batch must HOLD the pairs' guard locks from its commit-time re-check through
// the commit itself. Re-checking and then releasing before the commit leaves
// the same gap one step later (both writers told OK in dozens of rounds out of
// 400 when the locks are released early). The batch carries an engram mutation
// too, which is the widest form of the window: Commit then applies, replicates
// and only afterwards releases.
func TestBatchCommit_HoldsRelTypeGuardThroughCommit(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ws := store.VaultPrefix("reltype-batch-commit-race")
	now := time.Now()

	const rounds = 400
	bothOK := 0
	for i := 0; i < rounds; i++ {
		src, dst := NewULID(), NewULID()
		seedEndpoints(t, store, ws, src, dst)

		batch := store.NewBatch()
		if err := batch.UpdateEngramState(ctx, ws, src, StatePaused); err != nil {
			t.Fatalf("round %d: queue mutation: %v", i, err)
		}
		if err := batch.WriteAssociation(ctx, ws, src, dst, &Association{
			TargetID: dst, RelType: RelSupersedes, Weight: 1.0, Confidence: 1.0, CreatedAt: now,
		}); err != nil {
			t.Fatalf("round %d: queue supersedes: %v", i, err)
		}

		var commitErr, directErr error
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			commitErr = batch.Commit()
		}()
		go func() {
			defer wg.Done()
			<-start
			directErr = store.WriteAssociation(ctx, ws, src, dst, &Association{
				TargetID: dst, RelType: RelRefines, Weight: 1.0, Confidence: 1.0, CreatedAt: now,
			})
		}()
		close(start)
		wg.Wait()
		batch.Discard()

		for name, err := range map[string]error{"batch commit": commitErr, "direct write": directErr} {
			if err != nil && !errors.Is(err, ErrAssocRelTypeCollision) {
				t.Fatalf("round %d: %s: unexpected error %v", i, name, err)
			}
		}
		if commitErr == nil && directErr == nil {
			bothOK++
		}
	}
	if bothOK > 0 {
		t.Fatalf("of %d rounds: the batch commit and the direct write were both told OK in %d (one edge silently replaced); want 0", rounds, bothOK)
	}
}
