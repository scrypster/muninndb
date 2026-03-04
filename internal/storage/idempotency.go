package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

const idempotencyPurgeBatchSize = 1000

// IdempotencyReceipt is the value stored at an idempotency key.
type IdempotencyReceipt struct {
	EngramID  string `json:"engram_id"`
	CreatedAt int64  `json:"created_at"` // unix nanos
}

// CheckIdempotency looks up an op_id receipt. Returns nil, nil if not found.
func (ps *PebbleStore) CheckIdempotency(ctx context.Context, opID string) (*IdempotencyReceipt, error) {
	key := keys.IdempotencyKey(opID)
	val, err := Get(ps.db, key)
	if err != nil {
		return nil, fmt.Errorf("check idempotency: %w", err)
	}
	if val == nil {
		return nil, nil
	}
	var receipt IdempotencyReceipt
	if err := json.Unmarshal(val, &receipt); err != nil {
		return nil, fmt.Errorf("decode idempotency receipt: %w", err)
	}
	return &receipt, nil
}

// WriteIdempotency writes an idempotency receipt for op_id → engramID.
func (ps *PebbleStore) WriteIdempotency(ctx context.Context, opID, engramID string) error {
	receipt := IdempotencyReceipt{
		EngramID:  engramID,
		CreatedAt: time.Now().UnixNano(),
	}
	val, err := json.Marshal(receipt)
	if err != nil {
		return fmt.Errorf("marshal idempotency receipt: %w", err)
	}
	key := keys.IdempotencyKey(opID)
	return ps.db.Set(key, val, pebble.NoSync)
}

// PurgeExpiredIdempotency deletes idempotency receipts older than maxAge.
// It scans the 0x19 key prefix, deletes entries whose CreatedAt is before
// (now - maxAge), and batches deletes in groups of 1000. The ctx is checked
// between batches so the caller can cancel a long-running sweep.
// Returns the number of entries deleted.
func (ps *PebbleStore) PurgeExpiredIdempotency(ctx context.Context, maxAge time.Duration) (int, error) {
	cutoff := time.Now().Add(-maxAge).UnixNano()

	lower := []byte{0x19}
	upper := keys.PrefixUpperBound(lower)

	iter, err := ps.db.NewIter(&pebble.IterOptions{
		LowerBound: lower,
		UpperBound: upper,
	})
	if err != nil {
		return 0, fmt.Errorf("purge idempotency: new iter: %w", err)
	}
	defer iter.Close()

	var toDelete [][]byte
	for iter.SeekGE(lower); iter.Valid(); iter.Next() {
		k := iter.Key()
		if len(k) == 0 || k[0] != 0x19 {
			break
		}
		val := iter.Value()
		var receipt IdempotencyReceipt
		if err := json.Unmarshal(val, &receipt); err != nil {
			// Skip malformed receipts — don't delete, don't abort.
			continue
		}
		if receipt.CreatedAt < cutoff {
			keyCopy := make([]byte, len(k))
			copy(keyCopy, k)
			toDelete = append(toDelete, keyCopy)
		}
	}
	if err := iter.Error(); err != nil {
		return 0, fmt.Errorf("purge idempotency: iter scan: %w", err)
	}

	deleted := 0
	for i := 0; i < len(toDelete); i += idempotencyPurgeBatchSize {
		if err := ctx.Err(); err != nil {
			return deleted, err
		}
		end := i + idempotencyPurgeBatchSize
		if end > len(toDelete) {
			end = len(toDelete)
		}
		batch := ps.db.NewBatch()
		for _, k := range toDelete[i:end] {
			if err := batch.Delete(k, nil); err != nil {
				batch.Close()
				return deleted, fmt.Errorf("purge idempotency: batch delete: %w", err)
			}
		}
		if err := batch.Commit(pebble.NoSync); err != nil {
			batch.Close()
			return deleted, fmt.Errorf("purge idempotency: batch commit: %w", err)
		}
		batch.Close()
		deleted += end - i
	}
	return deleted, nil
}
