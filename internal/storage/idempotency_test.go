package storage

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

// TestPurgeExpiredIdempotency verifies that PurgeExpiredIdempotency deletes
// stale receipts and leaves fresh ones intact, returning the correct count.
func TestPurgeExpiredIdempotency(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	maxAge := time.Hour

	// Stale: created more than maxAge ago.
	staleTime := time.Now().Add(-2 * maxAge).UnixNano()
	staleIDs := []string{"stale-op-1", "stale-op-2", "stale-op-3"}
	// Write receipts directly to control CreatedAt; WriteIdempotency always uses time.Now().
	for _, opID := range staleIDs {
		receipt := IdempotencyReceipt{
			EngramID:  "engram-" + opID,
			CreatedAt: staleTime,
		}
		val, err := json.Marshal(receipt)
		if err != nil {
			t.Fatalf("marshal stale receipt: %v", err)
		}
		key := keys.IdempotencyKey(opID)
		if err := store.db.Set(key, val, pebble.NoSync); err != nil {
			t.Fatalf("write stale receipt %q: %v", opID, err)
		}
	}

	// Fresh: created just now (well within maxAge).
	freshTime := time.Now().UnixNano()
	freshIDs := []string{"fresh-op-1", "fresh-op-2"}
	for _, opID := range freshIDs {
		receipt := IdempotencyReceipt{
			EngramID:  "engram-" + opID,
			CreatedAt: freshTime,
		}
		val, err := json.Marshal(receipt)
		if err != nil {
			t.Fatalf("marshal fresh receipt: %v", err)
		}
		key := keys.IdempotencyKey(opID)
		if err := store.db.Set(key, val, pebble.NoSync); err != nil {
			t.Fatalf("write fresh receipt %q: %v", opID, err)
		}
	}

	deleted, err := store.PurgeExpiredIdempotency(ctx, maxAge)
	if err != nil {
		t.Fatalf("PurgeExpiredIdempotency: %v", err)
	}
	if deleted != 3 {
		t.Errorf("expected 3 deleted, got %d", deleted)
	}

	// Stale receipts must be gone.
	for _, opID := range staleIDs {
		r, err := store.CheckIdempotency(ctx, opID)
		if err != nil {
			t.Fatalf("CheckIdempotency(%q): %v", opID, err)
		}
		if r != nil {
			t.Errorf("stale receipt %q still present after purge", opID)
		}
	}

	// Fresh receipts must survive.
	for _, opID := range freshIDs {
		r, err := store.CheckIdempotency(ctx, opID)
		if err != nil {
			t.Fatalf("CheckIdempotency(%q): %v", opID, err)
		}
		if r == nil {
			t.Errorf("fresh receipt %q was incorrectly purged", opID)
		}
	}
}
