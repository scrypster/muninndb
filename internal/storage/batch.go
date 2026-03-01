package storage

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/provenance"
	"github.com/scrypster/muninndb/internal/storage/erf"
	"github.com/scrypster/muninndb/internal/storage/keys"
	"github.com/scrypster/muninndb/internal/wal"
)

// stateUpdate records a (workspace, id) pair whose state was changed via
// UpdateEngramState. Used by Commit to invalidate stale L1 cache entries.
type stateUpdate struct {
	ws [8]byte
	id ULID
}

// pebbleStoreBatch implements StoreBatch using a single pebble.Batch.
// All WriteEngram calls queue writes into the batch; Commit flushes them atomically.
type pebbleStoreBatch struct {
	ps        *PebbleStore
	batch     *pebble.Batch
	committed bool
	// pendingItems collects metadata needed for post-commit side effects
	// (vault counters, WAL entries, provenance). They are processed after Commit.
	pendingItems []batchPendingItem
	// stateUpdatedIDs tracks engrams whose state was changed by UpdateEngramState.
	// Their cache entries are invalidated in Commit after the batch flushes to Pebble.
	stateUpdatedIDs []stateUpdate
}

// batchPendingItem holds the data required for post-commit vault counter and
// provenance work for a single engram queued into the batch.
type batchPendingItem struct {
	wsPrefix [8]byte
	eng      *Engram
}

// NewBatch returns a new StoreBatch that queues engram writes atomically.
// The caller must call Commit or Discard exactly once on the returned value.
func (ps *PebbleStore) NewBatch() StoreBatch {
	return &pebbleStoreBatch{
		ps:    ps,
		batch: ps.db.NewBatch(),
	}
}

// WriteEngram queues all keys for eng into the batch (does not commit).
// It applies the same defaulting and encoding logic as PebbleStore.WriteEngram.
func (b *pebbleStoreBatch) WriteEngram(ctx context.Context, wsPrefix [8]byte, eng *Engram) error {
	// Apply defaults — same as PebbleStore.WriteEngram.
	if eng.ID == (ULID{}) {
		if !eng.CreatedAt.IsZero() {
			eng.ID = NewULIDWithTime(eng.CreatedAt)
		} else {
			eng.ID = NewULID()
		}
	}
	if eng.State == 0 {
		eng.State = StateActive
	}
	if eng.Confidence == 0 {
		eng.Confidence = 1.0
	}
	if eng.Stability == 0 {
		eng.Stability = 30.0
	}
	if eng.CreatedAt.IsZero() {
		eng.CreatedAt = time.Now()
	}
	if eng.UpdatedAt.IsZero() {
		eng.UpdatedAt = eng.CreatedAt
	}
	if eng.LastAccess.IsZero() {
		eng.LastAccess = eng.CreatedAt
	}

	erfEng := toERFEngram(eng)
	erfBytes, err := erf.EncodeV2(erfEng)
	if err != nil {
		return fmt.Errorf("batch encode engram: %w", err)
	}

	id16 := [16]byte(eng.ID)

	// 0x01: full engram record
	b.batch.Set(keys.EngramKey(wsPrefix, id16), erfBytes, nil)
	// 0x02: metadata-only slim form
	b.batch.Set(keys.MetaKey(wsPrefix, id16), erf.MetaKeySlice(erfBytes), nil)

	// 0x18: standalone embedding key
	if len(eng.Embedding) > 0 {
		params, quantized := erf.Quantize(eng.Embedding)
		paramsBuf := erf.EncodeQuantizeParams(params)
		embedBytes := make([]byte, 8+len(quantized))
		copy(embedBytes[:8], paramsBuf[:])
		for i, v := range quantized {
			embedBytes[8+i] = byte(v)
		}
		b.batch.Set(keys.EmbeddingKey(wsPrefix, id16), embedBytes, nil)
	}

	// 0x03/0x04/weight-index: association keys
	for _, assoc := range eng.Associations {
		av := encodeAssocValue(assoc.RelType, assoc.Confidence, assoc.CreatedAt, assoc.LastActivated)
		b.batch.Set(keys.AssocFwdKey(wsPrefix, id16, assoc.Weight, [16]byte(assoc.TargetID)), av[:], nil)
		b.batch.Set(keys.AssocRevKey(wsPrefix, [16]byte(assoc.TargetID), assoc.Weight, id16), av[:], nil)
		var wiBuf [4]byte
		binary.BigEndian.PutUint32(wiBuf[:], math.Float32bits(assoc.Weight))
		b.batch.Set(keys.AssocWeightIndexKey(wsPrefix, id16, [16]byte(assoc.TargetID)), wiBuf[:], nil)
	}

	// 0x0B: state index
	b.batch.Set(keys.StateIndexKey(wsPrefix, uint8(eng.State), id16), []byte{}, nil)
	// 0x0C: tag indexes
	for _, tag := range eng.Tags {
		b.batch.Set(keys.TagIndexKey(wsPrefix, keys.Hash(tag), id16), []byte{}, nil)
	}
	// 0x0D: creator index
	b.batch.Set(keys.CreatorIndexKey(wsPrefix, keys.Hash(eng.CreatedBy), id16), []byte{}, nil)
	// 0x10: relevance bucket key
	b.batch.Set(keys.RelevanceBucketKey(wsPrefix, eng.Relevance, id16), []byte{}, nil)

	// 0x22: LastAccess index — seed with LastAccess (= CreatedAt for new engrams).
	laMillis := eng.LastAccess.UnixMilli()
	laKey := keys.LastAccessIndexKey(wsPrefix, laMillis, id16)
	b.batch.Set(laKey, nil, nil)

	b.pendingItems = append(b.pendingItems, batchPendingItem{wsPrefix: wsPrefix, eng: eng})
	return nil
}

// WriteAssociation queues forward (0x03), reverse (0x04), and weight-index (0x14) keys
// for the association into the batch. Uses the same key-building and value-encoding
// logic as PebbleStore.WriteAssociation.
func (b *pebbleStoreBatch) WriteAssociation(ctx context.Context, ws [8]byte, src, dst ULID, assoc *Association) error {
	if b.committed {
		return fmt.Errorf("batch already committed")
	}
	av := encodeAssocValue(assoc.RelType, assoc.Confidence, assoc.CreatedAt, assoc.LastActivated)
	b.batch.Set(keys.AssocFwdKey(ws, [16]byte(src), assoc.Weight, [16]byte(dst)), av[:], nil)
	b.batch.Set(keys.AssocRevKey(ws, [16]byte(dst), assoc.Weight, [16]byte(src)), av[:], nil)
	var weightBuf [4]byte
	binary.BigEndian.PutUint32(weightBuf[:], math.Float32bits(assoc.Weight))
	b.batch.Set(keys.AssocWeightIndexKey(ws, [16]byte(src), [16]byte(dst)), weightBuf[:], nil)
	return nil
}

// WriteOrdinal queues the ordinal key for (parentID, childID) into the batch.
func (b *pebbleStoreBatch) WriteOrdinal(ctx context.Context, ws [8]byte, parentID, childID ULID, ordinal int32) error {
	if b.committed {
		return fmt.Errorf("batch already committed")
	}
	key := keys.OrdinalKey(ws, [16]byte(parentID), [16]byte(childID))
	var buf [4]byte
	binary.BigEndian.PutUint32(buf[:], uint32(ordinal))
	return b.batch.Set(key, buf[:], nil)
}

// UpdateEngramState queues a state update for an existing engram into the batch.
// Reads the current engram from the underlying store, sets its state, and queues
// updated 0x01 and 0x02 key writes plus the 0x0B state index transition.
func (b *pebbleStoreBatch) UpdateEngramState(ctx context.Context, ws [8]byte, id ULID, newState LifecycleState) error {
	if b.committed {
		return fmt.Errorf("batch already committed")
	}
	eng, err := b.ps.GetEngram(ctx, ws, id)
	if err != nil {
		return fmt.Errorf("update state: read engram: %w", err)
	}
	if eng == nil {
		return fmt.Errorf("update state: engram %s not found", id.String())
	}
	oldState := eng.State
	eng.State = newState
	eng.UpdatedAt = time.Now()

	erfEng := toERFEngram(eng)
	erfBytes, err := erf.EncodeV2(erfEng)
	if err != nil {
		return fmt.Errorf("update state: encode: %w", err)
	}
	id16 := [16]byte(id)

	// Transition 0x0B state index: remove old entry, write new entry.
	b.batch.Delete(keys.StateIndexKey(ws, uint8(oldState), id16), nil)
	b.batch.Set(keys.StateIndexKey(ws, uint8(newState), id16), []byte{}, nil)

	// Update 0x01 full engram record and 0x02 metadata slice.
	if err := b.batch.Set(keys.EngramKey(ws, id16), erfBytes, nil); err != nil {
		return fmt.Errorf("update state: set engram key: %w", err)
	}
	if err := b.batch.Set(keys.MetaKey(ws, id16), erf.MetaKeySlice(erfBytes), nil); err != nil {
		return err
	}
	// Track for cache invalidation in Commit.
	b.stateUpdatedIDs = append(b.stateUpdatedIDs, stateUpdate{ws: ws, id: id})
	return nil
}

// Commit atomically flushes all queued writes to Pebble and runs post-commit
// side effects (vault counters, WAL entries, provenance).
func (b *pebbleStoreBatch) Commit() error {
	if b.committed {
		return fmt.Errorf("batch already committed")
	}
	b.committed = true

	syncOption := pebble.Sync
	if b.ps.noSyncEngrams {
		syncOption = pebble.NoSync
	}
	if err := b.batch.Commit(syncOption); err != nil {
		return fmt.Errorf("batch commit: %w", err)
	}

	// Invalidate L1 cache entries for all engrams whose state was updated.
	// The batch has now been flushed to Pebble; any cached entry reflects the
	// pre-commit state and must be evicted so subsequent reads see the new state.
	for _, su := range b.stateUpdatedIDs {
		b.ps.cache.Delete(su.ws, su.id)
		b.ps.metaCache.Remove([16]byte(su.id))
	}

	// Post-commit side effects — mirrors PebbleStore.WriteEngram post-commit work.
	ctx := context.Background()
	for _, item := range b.pendingItems {
		ws := item.wsPrefix
		eng := item.eng

		vc := b.ps.getOrInitCounter(ctx, ws)
		newCount := vc.count.Add(1)
		if b.ps.counterFlush != nil {
			if current, ok := b.ps.vaultCounters.Load(ws); ok && current.(*vaultCounter) == vc {
				b.ps.counterFlush.Submit(ws, newCount)
			}
		}

		if b.ps.gc != nil {
			idBytes := [16]byte(eng.ID)
			vaultID := binary.BigEndian.Uint32(ws[:4])
			wal.AppendAsync(b.ps.gc, &wal.MOLEntry{
				OpType:  wal.OpEngramWrite,
				VaultID: vaultID,
				Payload: idBytes[:],
			})
		}

		if b.ps.provWork != nil {
			b.ps.provWork.Submit(ws, eng.ID, provenance.ProvenanceEntry{
				Timestamp: eng.CreatedAt,
				Source:    provenance.SourceHuman,
				AgentID:   eng.CreatedBy,
				Operation: "create",
			})
		}
	}

	return nil
}

// Discard releases the batch resources. Safe to call after Commit (idempotent).
func (b *pebbleStoreBatch) Discard() {
	if !b.committed {
		b.batch.Close()
	}
}
