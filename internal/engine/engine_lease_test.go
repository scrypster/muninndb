package engine

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/transport/mbp"
)

func writeWorkItem(t *testing.T, eng *Engine, vault, content string) string {
	t.Helper()
	resp, err := eng.Write(context.Background(), &mbp.WriteRequest{
		Vault:   vault,
		Concept: "work item",
		Content: content,
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	return resp.ID
}

func TestClaimAcquireRefreshConflict(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()
	id := writeWorkItem(t, eng, "test", "claimable work")

	// Acquire on a free engram.
	res, err := eng.Claim(ctx, "test", id, "hostA:s1", 60)
	if err != nil {
		t.Fatalf("Claim acquire: %v", err)
	}
	if res.Status != LeaseAcquired || res.Owner != "hostA:s1" {
		t.Fatalf("acquire = %+v, want acquired/hostA:s1", res)
	}

	// Same owner re-claims -> refreshed, heartbeat advances.
	res2, err := eng.Claim(ctx, "test", id, "hostA:s1", 60)
	if err != nil {
		t.Fatalf("Claim refresh: %v", err)
	}
	if res2.Status != LeaseRefreshed {
		t.Fatalf("refresh status = %v, want refreshed", res2.Status)
	}
	if res2.Heartbeat < res.Heartbeat {
		t.Fatalf("refresh heartbeat did not advance")
	}

	// A different owner hits a live lease -> conflict, learns the current holder.
	res3, err := eng.Claim(ctx, "test", id, "hostB:s2", 60)
	if err != nil {
		t.Fatalf("Claim conflict: %v", err)
	}
	if res3.Status != LeaseConflict || res3.Owner != "hostA:s1" {
		t.Fatalf("conflict = %+v, want conflict/hostA:s1", res3)
	}
}

func TestClaimReclaimsStaleLease(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()
	id := writeWorkItem(t, eng, "test", "reclaimable work")

	// Plant a foreign lease whose heartbeat is well past its TTL (crashed owner).
	ws := eng.store.VaultPrefix("test")
	ulid, _ := storage.ParseULID(id)
	stale := storage.Lease{
		Owner:      "hostDead:s0",
		Heartbeat:  time.Now().Add(-1 * time.Hour).UnixNano(),
		TTLSeconds: 60,
	}
	if _, err := eng.store.CompareAndSet(ctx, ws, ulid, storage.CASCondition{}, storage.CASMutation{Lease: &stale}); err != nil {
		t.Fatalf("plant stale lease: %v", err)
	}

	res, err := eng.Claim(ctx, "test", id, "hostNew:s9", 60)
	if err != nil {
		t.Fatalf("Claim reclaim: %v", err)
	}
	if res.Status != LeaseReclaimed || res.Owner != "hostNew:s9" {
		t.Fatalf("reclaim = %+v, want reclaimed/hostNew:s9", res)
	}
}

func TestReleaseIdempotentAndOwnerScoped(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()
	id := writeWorkItem(t, eng, "test", "releasable work")

	if _, err := eng.Claim(ctx, "test", id, "hostA:s1", 60); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	// A non-owner release does not touch the lease.
	rel, err := eng.Release(ctx, "test", id, "hostB:s2")
	if err != nil {
		t.Fatalf("Release non-owner: %v", err)
	}
	if rel.Released || rel.Owner != "hostA:s1" {
		t.Fatalf("non-owner release = %+v, want released=false owner=hostA:s1", rel)
	}

	// The owner releases -> cleared.
	rel, err = eng.Release(ctx, "test", id, "hostA:s1")
	if err != nil {
		t.Fatalf("Release owner: %v", err)
	}
	if !rel.Released {
		t.Fatalf("owner release did not clear the lease")
	}

	// Releasing again is an idempotent no-op.
	rel, err = eng.Release(ctx, "test", id, "hostA:s1")
	if err != nil {
		t.Fatalf("Release idempotent: %v", err)
	}
	if rel.Released {
		t.Fatalf("second release should report nothing cleared")
	}

	// After release the engram is claimable again.
	res, err := eng.Claim(ctx, "test", id, "hostB:s2", 60)
	if err != nil {
		t.Fatalf("Claim after release: %v", err)
	}
	if res.Status != LeaseAcquired {
		t.Fatalf("post-release claim = %v, want acquired", res.Status)
	}
}

func TestClaimConcurrentSingleAcquirer(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()
	id := writeWorkItem(t, eng, "test", "contended work")

	const n = 24
	var wg sync.WaitGroup
	var mu sync.Mutex
	acquired := 0
	conflicts := 0

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			owner := "host:" + string(rune('a'+i))
			res, err := eng.Claim(ctx, "test", id, owner, 60)
			if err != nil {
				t.Errorf("Claim: %v", err)
				return
			}
			mu.Lock()
			switch res.Status {
			case LeaseAcquired:
				acquired++
			case LeaseConflict:
				conflicts++
			}
			mu.Unlock()
		}(i)
	}
	wg.Wait()

	if acquired != 1 {
		t.Fatalf("expected exactly one acquirer, got %d (conflicts=%d)", acquired, conflicts)
	}
	if conflicts != n-1 {
		t.Fatalf("expected %d conflicts, got %d", n-1, conflicts)
	}
}

// TestLifecycleStateTOCTOU is the regression test for the compare-and-set fix:
// many concurrent guarded transitions from the same expected state must produce
// exactly one winner — an unconditional read-then-write would let several through.
func TestLifecycleStateTOCTOU(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()
	id := writeWorkItem(t, eng, "test", "transitionable work")

	active := "active"
	completed := "completed"

	const n = 24
	var wg sync.WaitGroup
	var mu sync.Mutex
	applied := 0

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := eng.CompareAndSet(ctx, "test", id, &active, &completed)
			if err != nil {
				t.Errorf("CompareAndSet: %v", err)
				return
			}
			if res.Applied {
				mu.Lock()
				applied++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if applied != 1 {
		t.Fatalf("expected exactly one applied transition, got %d", applied)
	}
}

func TestRecallHonoursOwnershipLease(t *testing.T) {
	eng, cleanup := testEnv(t)
	defer cleanup()
	ctx := context.Background()
	id := writeWorkItem(t, eng, "test", "leased recall target unicorn")
	awaitFTS(t, eng)

	recall := func(caller string, includeLeased bool) bool {
		t.Helper()
		resp, err := eng.Activate(ctx, &mbp.ActivateRequest{
			Vault:         "test",
			Context:       []string{"unicorn"},
			MaxResults:    10,
			Threshold:     0.01,
			CallerOwner:   caller,
			IncludeLeased: includeLeased,
		})
		if err != nil {
			t.Fatalf("Activate: %v", err)
		}
		for _, a := range resp.Activations {
			if a.ID == id {
				return true
			}
		}
		return false
	}

	// Baseline: unleased engram is visible to anyone.
	if !recall("hostA:s1", false) {
		t.Fatalf("unleased engram should be visible")
	}

	// hostB checks it out.
	if _, err := eng.Claim(ctx, "test", id, "hostB:s2", 300); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	if recall("hostA:s1", false) {
		t.Fatalf("engram leased by hostB must be hidden from hostA")
	}
	if !recall("hostB:s2", false) {
		t.Fatalf("owner hostB must still see its own leased engram")
	}
	if !recall("hostA:s1", true) {
		t.Fatalf("include_leased must reveal the leased engram to hostA")
	}

	// Make the lease stale by rewinding its heartbeat; an expired lease hides nothing.
	ws := eng.store.VaultPrefix("test")
	ulid, _ := storage.ParseULID(id)
	stale := storage.Lease{Owner: "hostB:s2", Heartbeat: time.Now().Add(-1 * time.Hour).UnixNano(), TTLSeconds: 60}
	if _, err := eng.store.CompareAndSet(ctx, ws, ulid, storage.CASCondition{}, storage.CASMutation{Lease: &stale}); err != nil {
		t.Fatalf("plant stale lease: %v", err)
	}
	if !recall("hostA:s1", false) {
		t.Fatalf("expired lease must not hide the engram")
	}
}
