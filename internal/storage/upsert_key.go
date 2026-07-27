package storage

import (
	"context"
	"fmt"
	"time"

	"github.com/scrypster/muninndb/internal/storage/erf"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

// GetUpsertKey looks up the engram ID pinned to a given upsert key (the sha256
// of the caller's idempotent_id) within a vault. Returns (ULID{}, nil) when no
// mapping exists — the caller then creates a fresh engram. A present-but-malformed
// value is an error, never a silent miss (a corrupted index entry must fail loud).
//
// Mirrors GetContentHash in shape (content_hash.go); the durable forward index
// itself is populated by the compound UpsertEngram write. Issue #556.
func (ps *PebbleStore) GetUpsertKey(ctx context.Context, wsPrefix [8]byte, keyHash [32]byte) (ULID, error) {
	key := keys.UpsertKeyKey(wsPrefix, keyHash)
	val, err := Get(ps.db, key)
	if err != nil {
		return ULID{}, fmt.Errorf("get upsert key: %w", err)
	}
	if val == nil {
		return ULID{}, nil
	}
	if len(val) != 16 {
		return ULID{}, fmt.Errorf("upsert key value has unexpected length %d", len(val))
	}
	var id ULID
	copy(id[:], val)
	return id, nil
}

// UpsertEngram is the compound write behind upsert_mode (#556): it pins an
// engram to sha256(idempotent_id) on a single ULID — creating on a miss,
// merging in place on a hit. The engram record, the 0x28 content-hash entry,
// and the 0x2B upsert-key forward index commit in ONE Pebble batch
// (crash-atomic), unlike the default Write path where PutContentHash is a
// separate, non-co-committed commit.
//
// SEMANTICS (hit): cognitive/provenance fields are PRESERVED (CreatedAt,
// Confidence, Stability, AccessCount, LastAccess, Trust, CreatedBy, Relevance,
// Associations); content fields are OVERWRITTEN from the request (Concept,
// Content, Tags, TypeLabel, Summary, Embedding-if-supplied). Stale secondary
// indexes (removed tags, old content-hash) are swept in-batch. AccessCount is
// preserved, NOT bumped (differs from content-hash dedup).
//
// STATE GUARD: if the pinned engram is absent or not StateActive (soft-deleted,
// archived, hard-deleted), the pointer is treated as stale → create a fresh
// engram and re-point the index. Never merge into a tombstone (RedTeam #556
// Change-2: silent data loss otherwise).
//
// CONCURRENCY: the caller (Engine.Write) MUST hold the per-(vault, keyHash)
// stripe mutex across this call. Mirrors the engine's contentHashLock around
// the default dedup path; the storage layer trusts the caller for serialization
// (same trust model as content-hash dedup).
func (ps *PebbleStore) UpsertEngram(ctx context.Context, wsPrefix [8]byte, eng *Engram, keyHash [32]byte) (id ULID, created bool, err error) {
	existing, err := ps.GetUpsertKey(ctx, wsPrefix, keyHash)
	if err != nil {
		return ULID{}, false, fmt.Errorf("upsert lookup: %w", err)
	}
	if existing == (ULID{}) {
		return ps.upsertCreate(ctx, wsPrefix, eng, keyHash)
	}

	// Hit — load the pinned engram to decide merge vs stale-pointer-recreate.
	pinned, err := ps.GetEngram(ctx, wsPrefix, existing)
	if err != nil {
		return ULID{}, false, fmt.Errorf("upsert: load pinned engram: %w", err)
	}
	if pinned == nil || pinned.State != StateActive {
		// Stale pointer (tombstone or hard-deleted) — create fresh; the 0x2B
		// entry is re-pointed to the new id by upsertCreate's Set.
		return ps.upsertCreate(ctx, wsPrefix, eng, keyHash)
	}
	return ps.upsertMerge(wsPrefix, pinned, eng, keyHash)
}

// upsertCreate writes a fresh engram and co-commits the 0x28 content-hash and
// 0x2B upsert-key forward indexes in one Pebble batch. Used for both a true
// miss and the StateActive-guard recreate (which overwrites a stale 0x2B entry).
// Constructed in-package (not via the StoreBatch interface) so the raw
// pebble.Batch is reachable for the index Set calls.
func (ps *PebbleStore) upsertCreate(ctx context.Context, wsPrefix [8]byte, eng *Engram, keyHash [32]byte) (ULID, bool, error) {
	sb := &pebbleStoreBatch{ps: ps, batch: ps.db.NewBatch()}
	defer sb.Discard()
	if err := sb.WriteEngram(ctx, wsPrefix, eng); err != nil {
		return ULID{}, false, fmt.Errorf("upsert create: write engram: %w", err)
	}
	id16 := eng.ID // populated by WriteEngram
	sb.batch.Set(keys.ContentHashKey(wsPrefix, ContentHash(eng.Content)), id16[:], nil)
	sb.batch.Set(keys.UpsertKeyKey(wsPrefix, keyHash), id16[:], nil)
	if err := sb.Commit(); err != nil {
		return ULID{}, false, fmt.Errorf("upsert create: commit: %w", err)
	}
	return eng.ID, true, nil
}

// upsertMerge overwrites the content fields of an existing (StateActive) engram
// in place — same ULID, cognitive state preserved — and sweeps the secondary
// indexes that change (tags, content-hash) in one atomic batch. Associations
// (0x03/0x04) are left untouched: the live keys are authoritative and may carry
// Hebbian-updated weights the inline ERF does not, so re-writing them would
// duplicate keys at stale weight positions (the hazard DeleteEngram notes). The
// ERF record is re-encoded with the preserved associations inline.
func (ps *PebbleStore) upsertMerge(wsPrefix [8]byte, existing *Engram, req *Engram, keyHash [32]byte) (ULID, bool, error) {
	_ = keyHash // the 0x2B entry already points to this id; merge does not re-point it

	merged := *existing // shallow copy: preserves cognitive + provenance + associations
	merged.Concept = req.Concept
	merged.Content = req.Content
	merged.Tags = req.Tags
	merged.TypeLabel = req.TypeLabel
	merged.Summary = req.Summary
	merged.UpdatedAt = time.Now()
	if len(req.Embedding) > 0 {
		merged.Embedding = req.Embedding
	}

	erfBytes, err := erf.EncodeV2(toERFEngram(&merged))
	if err != nil {
		return ULID{}, false, fmt.Errorf("upsert merge: encode: %w", err)
	}

	sb := &pebbleStoreBatch{ps: ps, batch: ps.db.NewBatch()}
	defer sb.Discard()
	id16 := [16]byte(merged.ID)

	// Tag index sweep: delete removed tags, (re-)write current tags.
	newTag := make(map[string]bool, len(merged.Tags))
	for _, t := range merged.Tags {
		newTag[t] = true
	}
	for _, tag := range existing.Tags {
		if !newTag[tag] {
			sb.batch.Delete(keys.TagIndexKey(wsPrefix, keys.Hash(tag), id16), nil)
		}
	}
	for _, tag := range merged.Tags {
		sb.batch.Set(keys.TagIndexKey(wsPrefix, keys.Hash(tag), id16), []byte{}, nil)
	}

	// Content-hash index: re-point only if content changed.
	if existing.Content != merged.Content {
		sb.batch.Delete(keys.ContentHashKey(wsPrefix, ContentHash(existing.Content)), nil)
		sb.batch.Set(keys.ContentHashKey(wsPrefix, ContentHash(merged.Content)), id16[:], nil)
	}

	// Embedding: overwrite only if the caller supplied one (quantize inline —
	// mirrors pebbleStoreBatch.WriteEngram's embedding block).
	if len(req.Embedding) > 0 {
		params, quantized := erf.Quantize(merged.Embedding)
		paramsBuf := erf.EncodeQuantizeParams(params)
		embedBytes := make([]byte, 8+len(quantized))
		copy(embedBytes[:8], paramsBuf[:])
		for i, v := range quantized {
			embedBytes[8+i] = byte(v)
		}
		sb.batch.Set(keys.EmbeddingKey(wsPrefix, id16), embedBytes, nil)
	}

	// Overwrite the engram record + meta slice.
	sb.batch.Set(keys.EngramKey(wsPrefix, id16), erfBytes, nil)
	sb.batch.Set(keys.MetaKey(wsPrefix, id16), erf.MetaKeySlice(erfBytes), nil)

	// Invalidate the L1/meta caches — Commit flushes stateUpdatedIDs post-batch.
	sb.stateUpdatedIDs = append(sb.stateUpdatedIDs, stateUpdate{ws: wsPrefix, id: merged.ID})

	if err := sb.Commit(); err != nil {
		return ULID{}, false, fmt.Errorf("upsert merge: commit: %w", err)
	}
	return merged.ID, false, nil
}
