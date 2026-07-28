package storage

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/prefix"
	"github.com/scrypster/muninndb/internal/provenance"
	"github.com/scrypster/muninndb/internal/storage/erf"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

// GetEngram reads a full engram record by ID.
func (ps *PebbleStore) GetEngram(ctx context.Context, wsPrefix [8]byte, id ULID) (*Engram, error) {
	// Check L1 cache first (vault-scoped to prevent cross-vault cache hits).
	if eng, found := ps.cache.Get(wsPrefix, id); found {
		return eng, nil
	}

	// Get from pebble
	key := keys.EngramKey(wsPrefix, [16]byte(id))
	val, err := Get(ps.db, key)
	if err != nil {
		return nil, fmt.Errorf("get engram: %w", err)
	}
	if val == nil {
		return nil, fmt.Errorf("engram %w", ErrNotFound)
	}

	// Decode
	erfEng, err := erf.Decode(val)
	if err != nil {
		return nil, fmt.Errorf("decode engram: %w", err)
	}

	// Convert back to storage.Engram
	eng := fromERFEngram(erfEng)

	// Cache it (vault-scoped).
	ps.cache.Set(wsPrefix, id, eng)

	return eng, nil
}

// EngramLastAccessNs returns the nanosecond timestamp of the last time the engram
// was served from the L1 cache. Returns 0 if not cached (caller should fall back to eng.LastAccess).
func (ps *PebbleStore) EngramLastAccessNs(wsPrefix [8]byte, id ULID) int64 {
	return ps.cache.LastAccessNs(wsPrefix, id)
}

// GetEngrams batch-reads full engram records.
//
// Fast path: L1-cached engrams are served without touching Pebble.
// Slow path: cache-cold IDs are read with a SINGLE Pebble iterator using
// sorted forward seeks — O(1) iterator open + N seeks instead of N snapshot
// acquisitions and N separate bloom-filter probes. OS readahead also kicks in
// as the seeks are sequential in key order.
//
// Missing engrams (deleted or dangling association edges) are returned as nil.
// Callers must check for nil before dereferencing.
func (ps *PebbleStore) GetEngrams(ctx context.Context, wsPrefix [8]byte, ids []ULID) ([]*Engram, error) {
	result := make([]*Engram, len(ids))

	// Phase 1: serve L1-cached engrams without touching Pebble.
	type uncachedEntry struct {
		resultIdx int
		id        ULID
		key       []byte
	}
	var uncached []uncachedEntry
	for i, id := range ids {
		if eng, found := ps.cache.Get(wsPrefix, id); found {
			result[i] = eng
		} else {
			uncached = append(uncached, uncachedEntry{
				resultIdx: i,
				id:        id,
				key:       keys.EngramKey(wsPrefix, [16]byte(id)),
			})
		}
	}
	if len(uncached) == 0 {
		return result, nil
	}

	// Phase 2: sort by key order so all Pebble seeks are strictly forward.
	sort.Slice(uncached, func(i, j int) bool {
		return bytes.Compare(uncached[i].key, uncached[j].key) < 0
	})

	// Phase 3: open ONE iterator spanning the range of needed keys.
	lower := uncached[0].key
	// Upper bound: copy the last key and increment its last byte.
	lastKey := uncached[len(uncached)-1].key
	upper := make([]byte, len(lastKey)+1) // +1 guarantees we include lastKey
	copy(upper, lastKey)
	carried := true
	for i := len(lastKey) - 1; i >= 0; i-- {
		upper[i]++
		if upper[i] != 0 {
			upper = upper[:len(lastKey)]
			carried = false
			break
		}
	}
	if carried {
		// All bytes were 0xFF and wrapped; restore lastKey and keep the +1 trailing 0x00.
		copy(upper, lastKey)
	}

	iter, err := ps.pebbleReader(ctx).NewIter(&pebble.IterOptions{
		LowerBound: lower,
		UpperBound: upper,
	})
	if err != nil {
		// Fallback: individual GetEngram calls.
		slog.Warn("storage: GetEngrams iterator open failed, falling back to individual reads", "err", err)
		for _, u := range uncached {
			eng, engErr := ps.GetEngram(ctx, wsPrefix, u.id)
			if engErr != nil {
				slog.Warn("storage: GetEngrams fallback read failed", "id", u.id, "err", engErr)
				continue
			}
			result[u.resultIdx] = eng
		}
		return result, nil
	}
	defer iter.Close()

	for _, u := range uncached {
		if iter.SeekGE(u.key); !iter.Valid() || !bytes.Equal(iter.Key(), u.key) {
			// Engram not found — dangling edge or soft-deleted; leave result[i] = nil.
			continue
		}
		val := make([]byte, len(iter.Value()))
		copy(val, iter.Value())
		erfEng, err := erf.Decode(val)
		if err != nil {
			continue
		}
		eng := fromERFEngram(erfEng)
		ps.cache.Set(wsPrefix, u.id, eng)
		result[u.resultIdx] = eng
	}

	return result, nil
}

// GetMetadata reads only the metadata fields for a batch of engrams.
// Uses a two-level cache: metaCache (metadata-only) → L1 engram cache → Pebble.
// Hot engrams (repeatedly activated) are served entirely from in-memory caches.
// Missing engrams (deleted or dangling) are returned as nil; callers must check.
func (ps *PebbleStore) GetMetadata(ctx context.Context, wsPrefix [8]byte, ids []ULID) ([]*EngramMeta, error) {
	result := make([]*EngramMeta, len(ids))
	for i, id := range ids {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		// Level 1: metadata-only cache (populated after first Pebble read).
		if meta, ok := ps.metaCache.Get([16]byte(id)); ok {
			result[i] = meta
			continue
		}

		// Level 2: full engram L1 cache — extract metadata fields without Pebble read.
		if eng, found := ps.cache.Get(wsPrefix, id); found {
			meta := &EngramMeta{
				ID:          eng.ID,
				CreatedAt:   eng.CreatedAt,
				UpdatedAt:   eng.UpdatedAt,
				LastAccess:  eng.LastAccess,
				Confidence:  eng.Confidence,
				Relevance:   eng.Relevance,
				Stability:   eng.Stability,
				AccessCount: eng.AccessCount,
				State:       eng.State,
				AssocCount:  uint16(len(eng.Associations)),
				EmbedDim:    eng.EmbedDim,
				MemoryType:  eng.MemoryType,
				Trust:       eng.Trust,
				ValidFrom:   eng.ValidFrom,
				ValidUntil:  eng.ValidUntil,
				Importance:  eng.Importance,
			}
			ps.metaCache.Add([16]byte(id), meta)
			result[i] = meta
			continue
		}

		// Slow path: read compact metadata key from Pebble (snapshot-aware).
		key := keys.MetaKey(wsPrefix, [16]byte(id))
		val, err := getFromReader(ps.pebbleReader(ctx), key)
		if err != nil {
			// Unexpected storage error — return it.
			return nil, fmt.Errorf("get metadata: %w", err)
		}
		if val == nil {
			// Engram not found — append nil and continue (matching GetEngrams pattern).
			result[i] = nil
			continue
		}

		erfMeta, err := erf.DecodeMeta(val)
		if err != nil {
			// Decode error is unexpected — return it (not a missing entry).
			return nil, fmt.Errorf("decode metadata: %w", err)
		}

		meta := &EngramMeta{
			ID:          ULID(erfMeta.ID),
			CreatedAt:   erfMeta.CreatedAt,
			UpdatedAt:   erfMeta.UpdatedAt,
			LastAccess:  erfMeta.LastAccess,
			Confidence:  erfMeta.Confidence,
			Relevance:   erfMeta.Relevance,
			Stability:   erfMeta.Stability,
			AccessCount: erfMeta.AccessCount,
			State:       LifecycleState(erfMeta.State),
			AssocCount:  erfMeta.AssocCount,
			EmbedDim:    EmbedDimension(erfMeta.EmbedDim),
			MemoryType:  MemoryType(erfMeta.MemoryType),
			Trust:       TrustLevel(erfMeta.Trust),
			ValidFrom:   erfMeta.ValidFrom,
			ValidUntil:  erfMeta.ValidUntil,
			Importance:  erfMeta.Importance,
		}
		// Populate metaCache so subsequent calls for this engram skip Pebble.
		ps.metaCache.Add([16]byte(id), meta)
		result[i] = meta
	}
	return result, nil
}

// UpdateMetadata writes only the metadata fields that changed.
// If the state changes, it also updates the 0x0B state secondary index.
// Patches the raw 0x01 bytes in-place (no full re-encode).
func (ps *PebbleStore) UpdateMetadata(ctx context.Context, wsPrefix [8]byte, id ULID, meta *EngramMeta) error {
	// Read slim metadata to detect state change (needed for index update).
	oldMetas, err := ps.GetMetadata(ctx, wsPrefix, []ULID{id})
	if err != nil {
		return err
	}
	if len(oldMetas) == 0 || oldMetas[0] == nil {
		return fmt.Errorf("engram %w", ErrNotFound)
	}
	oldState := oldMetas[0].State
	var prevLastAccessMillis int64
	if !oldMetas[0].LastAccess.IsZero() {
		prevLastAccessMillis = oldMetas[0].LastAccess.UnixMilli()
	}

	// Read raw 0x01 bytes without decoding the full ERF structure.
	engramKey := keys.EngramKey(wsPrefix, [16]byte(id))
	rawBytes, err := Get(ps.db, engramKey)
	if err != nil {
		return fmt.Errorf("get engram raw: %w", err)
	}
	if rawBytes == nil {
		return fmt.Errorf("engram %w", ErrNotFound)
	}

	// Patch all mutable metadata fields in-place and recompute CRC32.
	if err := erf.PatchAllMeta(rawBytes,
		meta.UpdatedAt, meta.LastAccess,
		meta.Confidence, meta.Relevance, meta.Stability,
		meta.AccessCount, uint8(meta.State),
	); err != nil {
		return fmt.Errorf("patch metadata: %w", err)
	}

	batch := ps.db.NewBatch()
	defer batch.Close()

	// Update state secondary index if the state changed.
	if oldState != meta.State {
		batch.Delete(keys.StateIndexKey(wsPrefix, uint8(oldState), [16]byte(id)), nil)
		batch.Set(keys.StateIndexKey(wsPrefix, uint8(meta.State), [16]byte(id)), []byte{}, nil)
	}

	batch.Set(engramKey, rawBytes, nil)
	metaKey := keys.MetaKey(wsPrefix, [16]byte(id))
	metaSlice248 := rawBytes
	if len(metaSlice248) > erf.MetaKeySize {
		metaSlice248 = metaSlice248[:erf.MetaKeySize]
	}
	batch.Set(metaKey, metaSlice248, nil)

	// Invalidate L1 cache and metadata cache BEFORE commit — cached structs are stale.
	ps.cache.Delete(wsPrefix, id)
	ps.metaCache.Remove([16]byte(id))

	if err := batch.Commit(pebble.NoSync); err != nil {
		return fmt.Errorf("commit batch: %w", err)
	}
	ps.replicateBatch(batch)

	// Update LastAccess index (best effort — index inconsistency is non-fatal).
	if !meta.LastAccess.IsZero() {
		newMillis := meta.LastAccess.UnixMilli()
		if newMillis != prevLastAccessMillis {
			_ = ps.WriteLastAccessEntry(ctx, wsPrefix, id, prevLastAccessMillis, newMillis)
		}
	}

	// Append provenance entry via persistent worker (best effort — drops if full).
	ps.provWork.Submit(wsPrefix, id, provenance.ProvenanceEntry{
		Timestamp: time.Now(),
		Source:    provenance.SourceInferred,
		AgentID:   "system:metadata-update",
		Operation: "update-meta",
		Note:      "",
	})

	return nil
}

// UpdateRelevance updates the relevance and stability of an engram.
// It moves the relevance bucket key (0x10) from the old bucket to the new bucket,
// and patches the raw 0x01 bytes in-place (no full re-encode).
func (ps *PebbleStore) UpdateRelevance(ctx context.Context, wsPrefix [8]byte, id ULID, relevance, stability float32) error {
	// Read slim metadata to get the old relevance for bucket key movement.
	metas, err := ps.GetMetadata(ctx, wsPrefix, []ULID{id})
	if err != nil {
		return err
	}
	if len(metas) == 0 || metas[0] == nil {
		return fmt.Errorf("engram %w", ErrNotFound)
	}
	oldRelevance := metas[0].Relevance

	// Read raw 0x01 bytes without decoding the full ERF structure.
	engramKey := keys.EngramKey(wsPrefix, [16]byte(id))
	rawBytes, err := Get(ps.db, engramKey)
	if err != nil {
		return fmt.Errorf("get engram raw: %w", err)
	}
	if rawBytes == nil {
		return fmt.Errorf("engram %w", ErrNotFound)
	}

	// Patch relevance/stability/updatedAt in-place and recompute CRC32.
	if err := erf.PatchRelevance(rawBytes, time.Now(), relevance, stability); err != nil {
		return fmt.Errorf("patch relevance: %w", err)
	}

	batch := ps.db.NewBatch()
	defer batch.Close()

	// Move relevance bucket key.
	batch.Delete(keys.RelevanceBucketKey(wsPrefix, oldRelevance, [16]byte(id)), nil)
	batch.Set(keys.RelevanceBucketKey(wsPrefix, relevance, [16]byte(id)), []byte{}, nil)

	// Write patched records.
	batch.Set(engramKey, rawBytes, nil)
	metaKey := keys.MetaKey(wsPrefix, [16]byte(id))
	metaEnd := erf.MetaKeySize
	if metaEnd > len(rawBytes) {
		metaEnd = len(rawBytes)
	}
	batch.Set(metaKey, rawBytes[:metaEnd], nil)

	// Invalidate L1 cache and metadata cache BEFORE commit — cached structs are stale.
	ps.cache.Delete(wsPrefix, id)
	ps.metaCache.Remove([16]byte(id))

	if err := batch.Commit(pebble.NoSync); err != nil {
		return fmt.Errorf("commit batch: %w", err)
	}
	ps.replicateBatch(batch)

	// Append provenance entry via persistent worker (best effort — drops if full).
	ps.provWork.Submit(wsPrefix, id, provenance.ProvenanceEntry{
		Timestamp: time.Now(),
		Source:    provenance.SourceInferred,
		AgentID:   "system:relevance-update",
		Operation: "update-relevance",
		Note:      "",
	})

	return nil
}

// UpdateTrust updates the trust label of an engram in-place using PatchTrust.
// Invalidates the L1 and metadata caches. Appends a provenance entry.
func (ps *PebbleStore) UpdateTrust(ctx context.Context, wsPrefix [8]byte, id ULID, trust TrustLevel) error {
	engramKey := keys.EngramKey(wsPrefix, [16]byte(id))
	rawBytes, err := Get(ps.db, engramKey)
	if err != nil {
		return fmt.Errorf("get engram raw: %w", err)
	}
	if rawBytes == nil {
		return fmt.Errorf("engram %w", ErrNotFound)
	}

	if err := erf.PatchTrust(rawBytes, uint8(trust)); err != nil {
		return fmt.Errorf("patch trust: %w", err)
	}

	batch := ps.db.NewBatch()
	defer batch.Close()

	batch.Set(engramKey, rawBytes, nil)
	metaKey := keys.MetaKey(wsPrefix, [16]byte(id))
	batch.Set(metaKey, erf.MetaKeySlice(rawBytes), nil)

	// Invalidate L1 and metadata caches before commit — cached structs are stale.
	ps.cache.Delete(wsPrefix, id)
	ps.metaCache.Remove([16]byte(id))

	if err := batch.Commit(pebble.NoSync); err != nil {
		return fmt.Errorf("commit batch: %w", err)
	}
	ps.replicateBatch(batch)

	ps.provWork.Submit(wsPrefix, id, provenance.ProvenanceEntry{
		Timestamp: time.Now(),
		Source:    provenance.SourceHuman,
		AgentID:   "system:set-trust",
		Operation: "update-trust",
		Note:      trust.String(),
	})

	return nil
}

// StampValidUntil sets (or clears, with the zero time) the ValidUntil field of
// an engram in place — the single invalidation primitive (COG-19: invalidation
// is always a stamp, never a delete). When onlyIfOpen is true, an
// already-closed window is left untouched and (false, nil) is returned —
// used by the RelSupersedes link stamp, which must not destroy an earlier
// window end. Patches the raw 0x01 bytes (no full re-encode), mirroring
// UpdateTrust.
func (ps *PebbleStore) StampValidUntil(ctx context.Context, wsPrefix [8]byte, id ULID, until time.Time, onlyIfOpen bool) (bool, error) {
	engramKey := keys.EngramKey(wsPrefix, [16]byte(id))
	rawBytes, err := Get(ps.db, engramKey)
	if err != nil {
		return false, fmt.Errorf("get engram raw: %w", err)
	}
	if rawBytes == nil {
		return false, fmt.Errorf("engram %w", ErrNotFound)
	}

	if onlyIfOpen && !erf.GetValidUntil(rawBytes).IsZero() {
		return false, nil
	}

	if err := erf.PatchValidUntil(rawBytes, until); err != nil {
		return false, fmt.Errorf("patch valid_until: %w", err)
	}

	batch := ps.db.NewBatch()
	defer batch.Close()

	batch.Set(engramKey, rawBytes, nil)
	metaKey := keys.MetaKey(wsPrefix, [16]byte(id))
	batch.Set(metaKey, erf.MetaKeySlice(rawBytes), nil)

	// Invalidate L1 and metadata caches before commit — cached structs are stale.
	ps.cache.Delete(wsPrefix, id)
	ps.metaCache.Remove([16]byte(id))

	if err := batch.Commit(pebble.NoSync); err != nil {
		return false, fmt.Errorf("commit batch: %w", err)
	}
	ps.replicateBatch(batch)

	note := "cleared"
	if !until.IsZero() {
		note = until.UTC().Format(time.RFC3339Nano)
	}
	ps.provWork.Submit(wsPrefix, id, provenance.ProvenanceEntry{
		Timestamp: time.Now(),
		Source:    provenance.SourceHuman,
		AgentID:   "system:valid-until",
		Operation: "stamp-valid-until",
		Note:      note,
	})

	return true, nil
}

// DeleteEngram performs a hard delete: removes the engram, all association keys,
// and all secondary indexes. Reads the engram first to gather index data.
func (ps *PebbleStore) DeleteEngram(ctx context.Context, wsPrefix [8]byte, id ULID) error {
	// Serialize against CompareAndSet on the same engram: hold the per-engram
	// stripe lock across the read + delete-batch-commit, mirroring what
	// CompareAndSet does. Otherwise a concurrent CAS can read the engram, this
	// delete can commit, and the CAS's later metadata/lease write lands after
	// the delete — resurrecting a record the caller believes is gone. Under the
	// lock the two paths serialize: a CAS that loses the race reads not-found
	// and writes nothing.
	//
	// The lock only needs to span the read and the batch commit: once the batch
	// is committed the metadata/lease are gone, so a later CAS reads not-found.
	// Post-commit cleanup (replication, cache, entity counts) is unlocked to
	// keep the stripe free during the O(n) entity work.
	mu := ps.casLocks.For(id[:])
	mu.Lock()

	// Read engram to collect secondary index data for cleanup.
	eng, err := ps.GetEngram(ctx, wsPrefix, id)
	if err != nil {
		// Not found or unreadable — attempt key-only delete as fallback.
		batch := ps.db.NewBatch()
		defer batch.Close()
		batch.Delete(keys.EngramKey(wsPrefix, [16]byte(id)), nil)
		batch.Delete(keys.MetaKey(wsPrefix, [16]byte(id)), nil)
		batch.Delete(keys.LeaseKey(wsPrefix, [16]byte(id)), nil)
		if err := batch.Commit(pebble.NoSync); err != nil {
			mu.Unlock()
			return err
		}
		mu.Unlock()
		ps.replicateBatch(batch)
		ps.cache.Delete(wsPrefix, id)
		return nil
	}

	batch := ps.db.NewBatch()
	defer batch.Close()

	// Primary records
	batch.Delete(keys.EngramKey(wsPrefix, [16]byte(id)), nil)
	batch.Delete(keys.MetaKey(wsPrefix, [16]byte(id)), nil)
	batch.Delete(keys.LeaseKey(wsPrefix, [16]byte(id)), nil)

	// Secondary indexes
	batch.Delete(keys.StateIndexKey(wsPrefix, uint8(eng.State), [16]byte(id)), nil)
	batch.Delete(keys.CreatorIndexKey(wsPrefix, keys.Hash(eng.CreatedBy), [16]byte(id)), nil)
	batch.Delete(keys.RelevanceBucketKey(wsPrefix, eng.Relevance, [16]byte(id)), nil)
	for _, tag := range eng.Tags {
		batch.Delete(keys.TagIndexKey(wsPrefix, keys.Hash(tag), [16]byte(id)), nil)
	}
	for _, tag := range eng.Tags {
		DeleteRawTagIndexEntry(batch, wsPrefix, tag, [16]byte(id))
	}

	// Association forward/reverse keys — scan live Pebble keys rather than
	// trusting the inline ERF associations, which may have stale weights if
	// the Hebbian worker has updated them since the engram was last written.
	//
	// Forward pass: scan 0x03|ws|id to find all associations FROM this engram.
	// Each hit gives us the actual current weight and targetID, so we can delete:
	//   - the forward key itself
	//   - the reverse key 0x04|ws|targetID|weight|id (uses actual weight)
	//   - the weight index key 0x14|ws|id|targetID
	fwdPrefix := keys.AssocFwdPrefixForID(wsPrefix, [16]byte(id))
	fwdIter, err := ps.db.NewIter(&pebble.IterOptions{
		LowerBound: fwdPrefix,
		UpperBound: append(append([]byte{}, fwdPrefix...), 0xFF),
	})
	if err == nil {
		for fwdIter.SeekGE(fwdPrefix); fwdIter.Valid(); fwdIter.Next() {
			k := fwdIter.Key()
			if len(k) < 25 || !bytes.Equal(k[:25], fwdPrefix) {
				break
			}
			if len(k) < 45 {
				continue
			}
			// Extract actual weight and targetID from the live key.
			var wc [4]byte
			copy(wc[:], k[25:29])
			weight := keys.WeightFromComplement(wc)
			var targetID [16]byte
			copy(targetID[:], k[29:45])

			batch.Delete(k, nil) // forward key (exact live key)
			batch.Delete(keys.AssocRevKey(wsPrefix, targetID, weight, [16]byte(id)), nil)
			batch.Delete(keys.AssocWeightIndexKey(wsPrefix, [16]byte(id), targetID), nil)
		}
		fwdIter.Close()
	}

	// Reverse pass: scan 0x04|ws|id to find all associations TO this engram
	// (from other engrams). Clean up the reverse index entries and the
	// corresponding forward keys in those other engrams.
	revPrefix := keys.AssocRevPrefixForID(wsPrefix, [16]byte(id))
	revIter, err := ps.db.NewIter(&pebble.IterOptions{
		LowerBound: revPrefix,
		UpperBound: append(append([]byte{}, revPrefix...), 0xFF),
	})
	if err == nil {
		for revIter.SeekGE(revPrefix); revIter.Valid(); revIter.Next() {
			k := revIter.Key()
			if len(k) < 25 || !bytes.Equal(k[:25], revPrefix) {
				break
			}
			if len(k) < 45 {
				continue
			}
			// Key layout: 0x04 | ws(8) | dstID(16) | weightComplement(4) | srcID(16)
			var wc [4]byte
			copy(wc[:], k[25:29])
			weight := keys.WeightFromComplement(wc)
			var srcID [16]byte
			copy(srcID[:], k[29:45])

			batch.Delete(k, nil) // reverse key
			batch.Delete(keys.AssocFwdKey(wsPrefix, srcID, weight, [16]byte(id)), nil)
			batch.Delete(keys.AssocWeightIndexKey(wsPrefix, srcID, [16]byte(id)), nil)
		}
		revIter.Close()
	}

	// Ordinal cleanup: scan all ordinal keys in this workspace and delete any where
	// this engram is the child (bytes [25:41] == id).
	// Key: 0x1E|ws(8)|parentID(16)|childID(16) = 41 bytes; childID at [25:41].
	ordinalPrefix := keys.OrdinalWorkspacePrefix(wsPrefix)
	ordIter, ordErr := PrefixIterator(ps.db, ordinalPrefix)
	if ordErr == nil {
		idBytes := [16]byte(id)
		for ordIter.First(); ordIter.Valid(); ordIter.Next() {
			k := ordIter.Key()
			if len(k) != 41 {
				continue
			}
			if bytes.Equal(k[25:41], idBytes[:]) {
				batch.Delete(k, nil)
			}
		}
		ordIter.Close()
	}

	// Also clean up ordinal keys where the deleted engram was a parent.
	// These are keys of the form 0x1E|ws|deletedID|childID.
	parentPrefix := keys.OrdinalPrefixForParent(wsPrefix, [16]byte(id))
	parentIter, parentIterErr := PrefixIterator(ps.db, parentPrefix)
	if parentIterErr == nil {
		for parentIter.First(); parentIter.Valid(); parentIter.Next() {
			batch.Delete(parentIter.Key(), nil)
		}
		parentIter.Close()
	}

	// Entity graph cleanup: remove 0x20 forward links, 0x23 reverse links,
	// and 0x21 relationship records sourced from this engram.
	entityNames, err := ps.deleteEntityLinks(wsPrefix, [16]byte(id), batch)
	if err != nil {
		slog.Warn("storage: entity link cleanup failed on delete, links may be orphaned", "engram", id.String(), "err", err)
	}

	if err := batch.Commit(pebble.NoSync); err != nil {
		mu.Unlock()
		return fmt.Errorf("delete engram: %w", err)
	}
	mu.Unlock()
	ps.replicateBatch(batch)

	ps.cache.Delete(wsPrefix, id)

	// Decrement MentionCount on each entity that was linked to this engram.
	// Done post-commit: if the process crashes here, counts will be slightly
	// high (stale) but no links remain, so the worst case is an entity
	// that isn't recognized as orphaned until the next decrement.
	// DecrementEntityMentionCount automatically deletes the 0x1F record when
	// the count reaches 0 and the 0x23 reverse index confirms no live links remain.
	for _, name := range entityNames {
		if err := ps.DecrementEntityMentionCount(ctx, name); err != nil {
			slog.Warn("storage: failed to decrement entity mention count on delete", "entity", name, "engram", id.String(), "err", err)
		}
	}

	// Decrement co-occurrence counts for every pair of entities that appeared
	// in this engram. Deletes the 0x24 key when the pair count reaches 0.
	// Capped at maxCoOccurrenceEntities to bound the O(n²) work on pathological
	// engrams; entities beyond the cap have stale counts (minor, consistent with
	// counts being best-effort across restarts).
	const maxCoOccurrenceEntities = 50
	coNames := entityNames
	if len(coNames) > maxCoOccurrenceEntities {
		slog.Warn("storage: engram has unusually many entities, co-occurrence cleanup capped",
			"engram", id.String(), "entity_count", len(entityNames), "cap", maxCoOccurrenceEntities)
		coNames = coNames[:maxCoOccurrenceEntities]
	}
	for i := 0; i < len(coNames); i++ {
		for j := i + 1; j < len(coNames); j++ {
			if err := ps.DecrementEntityCoOccurrence(ctx, wsPrefix, coNames[i], coNames[j]); err != nil {
				slog.Warn("storage: failed to decrement co-occurrence on delete", "a", coNames[i], "b", coNames[j], "engram", id.String(), "err", err)
			}
		}
	}

	// Decrement vault count synchronously to avoid a race where callers
	// observe a stale count after DeleteEngram returns.
	vc := ps.getOrInitCounter(ctx, wsPrefix)
	newCount := vc.count.Add(-1)
	if newCount < 0 {
		vc.count.Store(0)
		newCount = 0
	}
	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, uint64(newCount))
	if err := ps.db.Set(keys.VaultCountKey(wsPrefix), buf, pebble.Sync); err != nil {
		slog.Warn("storage: failed to persist vault count", "error", err)
	}

	return nil
}

// SoftDelete sets state to StateSoftDeleted and updates the record.
// It also transitions the 0x0B state secondary index from the old state to StateSoftDeleted.
//
// Takes the per-engram stripe lock (casLocks.For(id) — the SAME striped mutex
// CompareAndSet/DeleteEngram/UpdateConfidence/TouchAccess use) across the
// whole read-mutate-write. Previously this ran unlocked: GetEngram returns
// the L1 cache's live pointer (DomainCache.Get does not clone), and this
// method mutated eng.State/eng.UpdatedAt on that shared pointer in place with
// no synchronization — any concurrent reader of the same cached engram (e.g.
// TouchAccess's #682 reinforcement, or another SoftDelete/UpdateConfidence
// call) raced this write under -race. Mirrors UpdateConfidence's locking
// shape exactly, including dropping the cache entry before the authoritative
// GetEngram read so a racing DeleteEngram's stale cache entry can't be reused.
func (ps *PebbleStore) SoftDelete(ctx context.Context, wsPrefix [8]byte, id ULID) error {
	mu := ps.casLocks.For(id[:])
	mu.Lock()
	defer mu.Unlock()

	ps.cache.Delete(wsPrefix, id)

	// Read engram
	eng, err := ps.GetEngram(ctx, wsPrefix, id)
	if err != nil {
		return err
	}

	oldState := eng.State

	// Set state to soft deleted
	eng.State = StateSoftDeleted
	eng.UpdatedAt = time.Now()

	// Re-encode
	erfEng := toERFEngram(eng)
	erfBytes, err := erf.Encode(erfEng)
	if err != nil {
		return fmt.Errorf("encode engram: %w", err)
	}

	batch := ps.db.NewBatch()
	defer batch.Close()

	// Move state index entry: delete old, write new.
	oldStateKey := keys.StateIndexKey(wsPrefix, uint8(oldState), [16]byte(id))
	batch.Delete(oldStateKey, nil)
	newStateKey := keys.StateIndexKey(wsPrefix, uint8(StateSoftDeleted), [16]byte(id))
	batch.Set(newStateKey, []byte{}, nil)

	engramKey := keys.EngramKey(wsPrefix, [16]byte(id))
	batch.Set(engramKey, erfBytes, nil)

	metaKey := keys.MetaKey(wsPrefix, [16]byte(id))
	metaSlice437 := erfBytes
	if len(metaSlice437) > erf.MetaKeySize {
		metaSlice437 = metaSlice437[:erf.MetaKeySize]
	}
	batch.Set(metaKey, metaSlice437, nil)

	if err := batch.Commit(pebble.NoSync); err != nil {
		return fmt.Errorf("commit batch: %w", err)
	}
	ps.replicateBatch(batch)

	// Update cache (vault-scoped) and invalidate the metadata-only cache
	// so subsequent GetMetadata calls see the updated StateSoftDeleted state.
	// Note: entity links (0x20/0x23/0x21) are intentionally preserved on soft
	// delete so that Restore can return the engram with its entity associations
	// intact. Entity cleanup only happens on hard delete (DeleteEngram).
	ps.cache.Set(wsPrefix, id, eng)
	ps.metaCache.Remove([16]byte(id))

	return nil
}

// UpdateTags replaces the tag list on an engram, re-encodes the full record,
// and adds any new tag index entries. Old tag index entries for tags no longer
// present are left as orphans (safe: they point to a valid engram, just stale).
// For the dedup use-case (tags are always a superset) there are no removals.
func (ps *PebbleStore) UpdateTags(ctx context.Context, wsPrefix [8]byte, id ULID, tags []string) error {
	eng, err := ps.GetEngram(ctx, wsPrefix, id)
	if err != nil {
		return err
	}

	eng.Tags = tags
	eng.UpdatedAt = time.Now()

	erfEng := toERFEngram(eng)
	erfBytes, err := erf.Encode(erfEng)
	if err != nil {
		return fmt.Errorf("encode engram: %w", err)
	}

	batch := ps.db.NewBatch()
	defer batch.Close()

	engramKey := keys.EngramKey(wsPrefix, [16]byte(id))
	batch.Set(engramKey, erfBytes, nil)

	metaKey := keys.MetaKey(wsPrefix, [16]byte(id))
	metaSlice := erfBytes
	if len(metaSlice) > erf.MetaKeySize {
		metaSlice = metaSlice[:erf.MetaKeySize]
	}
	batch.Set(metaKey, metaSlice, nil)

	// Write tag index entries for all tags (idempotent for existing tags).
	for _, tag := range tags {
		batch.Set(keys.TagIndexKey(wsPrefix, keys.Hash(tag), [16]byte(id)), []byte{}, nil)
	}

	// Write raw-tag-range index entries for all tags (idempotent for existing
	// tags; like the 0x0C index above, stale entries for tags that are no
	// longer present are left as orphans — safe, since phase-6's
	// passesMetaFilter re-checks the real tag on the engram).
	for _, tag := range tags {
		if err := WriteRawTagIndexEntry(batch, wsPrefix, tag, [16]byte(id)); err != nil {
			return err
		}
	}

	// Invalidate L1 cache BEFORE commit — cached struct has stale tags.
	ps.cache.Delete(wsPrefix, id)
	ps.metaCache.Remove([16]byte(id))

	if err := batch.Commit(pebble.NoSync); err != nil {
		return fmt.Errorf("commit batch: %w", err)
	}
	ps.replicateBatch(batch)

	return nil
}

// GetEmbedding reads the quantized embedding from the 0x18 standalone key (ERF v2).
// Returns nil if no embedding is stored for this engram.
func (ps *PebbleStore) GetEmbedding(ctx context.Context, wsPrefix [8]byte, id ULID) ([]float32, error) {
	key := keys.EmbeddingKey(wsPrefix, [16]byte(id))
	val, err := Get(ps.db, key)
	if err != nil {
		return nil, fmt.Errorf("get embedding: %w", err)
	}
	if val == nil || len(val) < 8 {
		return nil, nil // no embedding stored
	}
	params := erf.DecodeQuantizeParams([8]byte(val[:8]))
	quantized := make([]int8, len(val)-8)
	for i := range quantized {
		quantized[i] = int8(val[8+i])
	}
	return erf.Dequantize(quantized, params), nil
}

// GetConfidence reads the confidence value from 0x02 metadata for an engram.
func (ps *PebbleStore) GetConfidence(ctx context.Context, wsPrefix [8]byte, id ULID) (float32, error) {
	key := keys.MetaKey(wsPrefix, [16]byte(id))
	val, err := Get(ps.db, key)
	if err != nil {
		return 0.0, fmt.Errorf("get metadata: %w", err)
	}
	if val == nil {
		return 0.0, fmt.Errorf("metadata %w", ErrNotFound)
	}

	// Decode metadata to extract confidence
	erfMeta, err := erf.DecodeMeta(val)
	if err != nil {
		return 0.0, fmt.Errorf("decode metadata: %w", err)
	}

	return erfMeta.Confidence, nil
}

// UpdateConfidence updates the confidence in 0x02 metadata (and 0x01 full engram).
// UpdateConfidence performs an ABSOLUTE set of an engram's confidence (unlike the
// delta-based UpdateConfidenceWithContradiction). It is a read-modify-write over
// the 0x01/0x02 keys and therefore races a concurrent DeleteEngram exactly like
// the delta primitive: a delete committing between this method's GetEngram read
// and its batch.Commit would be undone by the batch.Set(0x01|0x02), resurrecting
// the deleted engram's payload/meta keys without its 0x0B state index (the #594 /
// #626 resurrection class). The per-engram stripe lock (casLocks.For(id) — the
// SAME striped mutex CompareAndSet, DeleteEngram, and the delta primitive use)
// is held across the whole critical section, and the engram cache is dropped
// before the authoritative GetEngram read, so this path serializes with same-id
// delete/CAS and always reads committed Pebble state. Mirrors the locking shape
// of UpdateConfidenceWithContradiction above; only the set semantics differ.
func (ps *PebbleStore) UpdateConfidence(ctx context.Context, wsPrefix [8]byte, id ULID, confidence float32) error {
	mu := ps.casLocks.For(id[:])
	mu.Lock()
	// Drop any stale cache entry left by a racing DeleteEngram between its
	// batch.Commit and its post-commit cache.Delete, so the GetEngram below
	// reads authoritative Pebble state (see UpdateConfidenceWithContradiction).
	ps.cache.Delete(wsPrefix, id)

	// Read current engram
	eng, err := ps.GetEngram(ctx, wsPrefix, id)
	if err != nil {
		mu.Unlock()
		return err
	}

	// Update confidence
	eng.Confidence = confidence
	eng.UpdatedAt = time.Now()

	// Re-encode full engram
	erfEng := toERFEngram(eng)
	erfBytes, err := erf.Encode(erfEng)
	if err != nil {
		mu.Unlock()
		return fmt.Errorf("encode engram: %w", err)
	}

	// Write both keys
	batch := ps.db.NewBatch()
	defer batch.Close()

	engramKey := keys.EngramKey(wsPrefix, [16]byte(id))
	batch.Set(engramKey, erfBytes, nil)

	metaKey := keys.MetaKey(wsPrefix, [16]byte(id))
	metaSlice505 := erfBytes
	if len(metaSlice505) > erf.MetaKeySize {
		metaSlice505 = metaSlice505[:erf.MetaKeySize]
	}
	batch.Set(metaKey, metaSlice505, nil)

	if err := batch.Commit(pebble.NoSync); err != nil {
		mu.Unlock()
		return fmt.Errorf("commit batch: %w", err)
	}
	// Cache mutation under the stripe lock (see UpdateConfidenceWithContradiction):
	// otherwise a racing DeleteEngram's post-commit cache.Delete can land before
	// this cache.Set, re-caching an engram Pebble has already deleted.
	ps.cache.Set(wsPrefix, id, eng)
	// Invalidate metadata cache — cached metadata is stale.
	ps.metaCache.Remove([16]byte(id))
	mu.Unlock()

	ps.replicateBatch(batch)

	return nil
}

// TouchAccess bumps AccessCount (+1) and LastAccess (=now) for an engram,
// leaving every other metadata field untouched. It is the single reinforcement
// primitive for #682: RecordAccess, the content-hash dedup "reinforce" path,
// and the Read/feedback wiring all funnel through this method instead of doing
// their own unlocked GetEngram→UpdateMetadata round trip.
//
// Locking mirrors UpdateConfidence exactly (engram.go:799): the per-engram
// stripe lock (casLocks.For(id) — the SAME striped mutex CompareAndSet,
// DeleteEngram, and UpdateConfidence use) is held across the whole read-then-
// write, and the L1 cache entry is dropped before the authoritative GetEngram
// read, so this serializes with a same-id CompareAndSet/DeleteEngram instead of
// racing it (STO-2). All other fields (State, Confidence, Relevance,
// Stability, UpdatedAt) are read fresh under the lock and passed through to
// UpdateMetadata unchanged, so the 0x0B state index stays consistent with
// whatever state a concurrent CAS just committed (STO-3) — TouchAccess never
// has a stale view of State to accidentally index against.
//
// Trust, Confidence, and Importance are never part of the write: reinforcement
// moves only AccessCount/LastAccess, by construction — access frequency is not
// evidence of correctness (COG-10: co-activation never updates confidence) and
// not evidence of priority (COG-10 amendment: access never moves importance).
// NOTE: Stability deliberately passes through unchanged. An earlier draft added
// an inverse-retrieval-strength Stability gain here; it was deferred because
// Stability feeds the weighted_sum/RRF DecayFactor score component, so a
// write-on-read would silently change recall results in those modes. Any
// future reinforcement write here must be designed against all three scoring
// modes (ACT-R, weighted_sum, RRF) first.
func (ps *PebbleStore) TouchAccess(ctx context.Context, wsPrefix [8]byte, id ULID) error {
	mu := ps.casLocks.For(id[:])
	mu.Lock()
	defer mu.Unlock()

	// Drop any stale cache entry left by a racing DeleteEngram between its
	// batch.Commit and its post-commit cache.Delete (mirrors UpdateConfidence).
	ps.cache.Delete(wsPrefix, id)

	eng, err := ps.GetEngram(ctx, wsPrefix, id)
	if err != nil {
		return err
	}

	meta := &EngramMeta{
		State:       eng.State,
		Confidence:  eng.Confidence,
		Relevance:   eng.Relevance,
		Stability:   eng.Stability,
		AccessCount: eng.AccessCount + 1,
		UpdatedAt:   eng.UpdatedAt,
		LastAccess:  time.Now(),
	}
	return ps.UpdateMetadata(ctx, wsPrefix, id, meta)
}

// UpdateConfidenceWithContradiction atomically applies a confidence delta and,
// when hasContra, writes the 0x0A contradiction marker for the (id, other) pair
// in a single Pebble batch — no partial-failure window between the confidence
// write and the marker. Mirrors UpdateConfidence (0x01+0x02 via erf.Encode) and
// FlagContradiction (canonical-ordered 0x0A pair), composed.
//
// Delta-based (closes #559's lost-update race): the read+add+clamp happens
// INSIDE the locked section, not in the caller. The per-engram stripe lock
// (casLocks.For(id)) is acquired before the GetEngram read and held across
// batch.Commit — the same shape as PebbleStore.CompareAndSet (lease.go:157) and
// DeleteEngram. So N concurrent +delta calls on the same id serialize: each
// observes the prior writer's committed state and accumulates, instead of all
// reading the same pre-write value and overwriting each other. The method
// returns (prior, newConf) so the engine can audit without a second unlocked
// read that would re-open the race.
//
// Two failure classes the lock defends against:
//
//   - Lost update (#559): concurrent callers race the RMW. With the read inside
//     the lock, each +delta call sees its predecessor's commit and adds to it.
//   - Resurrection (#594 class): a racing DeleteEngram can commit between this
//     function's GetEngram read and its batch.Commit, after which the
//     batch.Set(0x01|0x02) + Commit resurrects the deleted engram's keys.
//
// Under the lock the function serializes with same-id CAS and delete paths.
// Post-commit cleanup (replicateBatch, cache, metaCache) runs unlocked to keep
// the stripe free during O(n) work — same shape as DeleteEngram.
//
// Cache invalidation before the read: DeleteEngram populates the engram cache
// during its own locked GetEngram read and only invalidates it AFTER mu.Unlock
// (post-commit). Without dropping the cache here, a racing DeleteEngram that
// committed-then-unlocked (but has not yet reached its post-commit cache.Delete)
// would leave this function's GetEngram reading a stale cached entry — which
// then gets written back via batch.Set(0x01|0x02), resurrecting the deleted
// engram. Dropping the cache under the lock forces a fresh Pebble read that
// observes the delete's committed state. Mirrors the authoritative-read-under-
// lock expectation without altering DeleteEngram's post-commit cleanup order.
//
// The caller is responsible for rejecting NaN/Inf deltas before invoking; this
// method assumes delta is finite.
func (ps *PebbleStore) UpdateConfidenceWithContradiction(ctx context.Context, wsPrefix [8]byte, id ULID, delta float32, other ULID, hasContra bool) (prior, newConf float32, err error) {
	mu := ps.casLocks.For(id[:])
	mu.Lock()
	// Drop any stale cache entry left by a racing DeleteEngram between its
	// batch.Commit and its post-commit cache.Delete, so the GetEngram below
	// reads authoritative Pebble state.
	ps.cache.Delete(wsPrefix, id)

	eng, err := ps.GetEngram(ctx, wsPrefix, id)
	if err != nil {
		mu.Unlock()
		return 0, 0, err
	}
	// Read+add+clamp UNDER the stripe lock — the lost-update fix (#559). The
	// engine-side read that used to live in Engine.AdjustConfidence is gone;
	// the prior value returned below comes from this locked read.
	prior = eng.Confidence
	newConf = prior + delta
	if newConf < 0 {
		newConf = 0
	} else if newConf > 1 {
		newConf = 1
	}
	eng.Confidence = newConf
	eng.UpdatedAt = time.Now()

	erfEng := toERFEngram(eng)
	erfBytes, err := erf.Encode(erfEng)
	if err != nil {
		mu.Unlock()
		return 0, 0, fmt.Errorf("encode engram: %w", err)
	}

	batch := ps.db.NewBatch()
	defer batch.Close()

	id16 := [16]byte(id)
	batch.Set(keys.EngramKey(wsPrefix, id16), erfBytes, nil)
	metaSlice := erfBytes
	if len(metaSlice) > erf.MetaKeySize {
		metaSlice = metaSlice[:erf.MetaKeySize]
	}
	batch.Set(keys.MetaKey(wsPrefix, id16), metaSlice, nil)

	if hasContra {
		aBytes := [16]byte(id)
		bBytes := [16]byte(other)
		if CompareULIDs(id, other) > 0 {
			aBytes, bBytes = bBytes, aBytes
		}
		batch.Set(keys.ContradictionKey(wsPrefix, 0, 0, aBytes), bBytes[:], nil)
		batch.Set(keys.ContradictionKey(wsPrefix, 0, 0, bBytes), aBytes[:], nil)
	}

	if err := batch.Commit(pebble.NoSync); err != nil {
		mu.Unlock()
		return 0, 0, fmt.Errorf("commit batch: %w", err)
	}
	// Cache mutation under the stripe lock (was post-Unlock): otherwise a
	// racing DeleteEngram's post-commit cache.Delete can land before this
	// cache.Set, which then re-caches an engram Pebble has already deleted
	// -- resurrecting it under a post-Wait read (caught on CI). Inside the
	// critical section the cache mutation is atomic with the commit;
	// DeleteEngram's cache.Delete always runs after we release.
	// replicateBatch stays post-commit (no local-cache effect).
	ps.cache.Set(wsPrefix, id, eng)
	ps.metaCache.Remove(id16)
	mu.Unlock()

	ps.replicateBatch(batch)

	return prior, newConf, nil
}

// toERFEngram converts storage.Engram to erf.Engram.
func toERFEngram(eng *Engram) *erf.Engram {
	erfAssocs := make([]erf.Association, len(eng.Associations))
	for i, a := range eng.Associations {
		erfAssocs[i] = erf.Association{
			TargetID:      [16]byte(a.TargetID),
			RelType:       uint16(a.RelType),
			Weight:        a.Weight,
			Confidence:    a.Confidence,
			CreatedAt:     a.CreatedAt,
			LastActivated: a.LastActivated,
		}
	}

	return &erf.Engram{
		ID:             [16]byte(eng.ID),
		CreatedAt:      eng.CreatedAt,
		UpdatedAt:      eng.UpdatedAt,
		LastAccess:     eng.LastAccess,
		Confidence:     eng.Confidence,
		Relevance:      eng.Relevance,
		Stability:      eng.Stability,
		AccessCount:    eng.AccessCount,
		State:          uint8(eng.State),
		EmbedDim:       uint8(eng.EmbedDim),
		Concept:        eng.Concept,
		CreatedBy:      eng.CreatedBy,
		Content:        eng.Content,
		Tags:           eng.Tags,
		Associations:   erfAssocs,
		Embedding:      eng.Embedding,
		Summary:        eng.Summary,
		KeyPoints:      eng.KeyPoints,
		MemoryType:     uint8(eng.MemoryType),
		TypeLabel:      eng.TypeLabel,
		Classification: eng.Classification,
		Trust:          uint8(eng.Trust),
		ValidFrom:      eng.ValidFrom,
		ValidUntil:     eng.ValidUntil,
		Importance:     eng.Importance,
	}
}

// fromERFEngram converts erf.Engram back to storage.Engram.
func fromERFEngram(e *erf.Engram) *Engram {
	assocs := make([]Association, len(e.Associations))
	for i, a := range e.Associations {
		assocs[i] = Association{
			TargetID:      ULID(a.TargetID),
			RelType:       RelType(a.RelType),
			Weight:        a.Weight,
			Confidence:    a.Confidence,
			CreatedAt:     a.CreatedAt,
			LastActivated: a.LastActivated,
		}
	}

	return &Engram{
		ID:             ULID(e.ID),
		CreatedAt:      e.CreatedAt,
		UpdatedAt:      e.UpdatedAt,
		LastAccess:     e.LastAccess,
		Confidence:     e.Confidence,
		Relevance:      e.Relevance,
		Stability:      e.Stability,
		AccessCount:    e.AccessCount,
		State:          LifecycleState(e.State),
		EmbedDim:       EmbedDimension(e.EmbedDim),
		Concept:        e.Concept,
		CreatedBy:      e.CreatedBy,
		Content:        e.Content,
		Tags:           e.Tags,
		Associations:   assocs,
		Embedding:      e.Embedding,
		Summary:        e.Summary,
		KeyPoints:      e.KeyPoints,
		MemoryType:     MemoryType(e.MemoryType),
		TypeLabel:      e.TypeLabel,
		Classification: e.Classification,
		Trust:          TrustLevel(e.Trust),
		ValidFrom:      e.ValidFrom,
		ValidUntil:     e.ValidUntil,
		Importance:     e.Importance,
	}
}

// ScanEngrams iterates over all engrams in the given vault workspace, calling fn for each.
// Iteration stops early if fn returns a non-nil error.
// For ERF v2 records, populates Embedding from the parallel 0x18 key space using a forward-seek join.
// Corrupt ERF records are skipped with a warning log.
func (ps *PebbleStore) ScanEngrams(ctx context.Context, ws [8]byte, fn func(*Engram) error) error {
	wsNext := ws
	for i := 7; i >= 0; i-- {
		wsNext[i]++
		if wsNext[i] != 0 {
			break
		}
	}

	lo := make([]byte, 9)
	lo[0] = prefix.Engram
	copy(lo[1:], ws[:])
	hi := make([]byte, 9)
	hi[0] = prefix.Engram
	copy(hi[1:], wsNext[:])

	iter, err := ps.db.NewIter(&pebble.IterOptions{LowerBound: lo, UpperBound: hi})
	if err != nil {
		return fmt.Errorf("scan engrams: create iter: %w", err)
	}
	defer iter.Close()

	// Second iterator for 0x18 embedding keys — sorted by ws|id, same order as 0x01.
	eLo := make([]byte, 9)
	eLo[0] = prefix.Embedding
	copy(eLo[1:], ws[:])
	eHi := make([]byte, 9)
	eHi[0] = prefix.Embedding
	copy(eHi[1:], wsNext[:])

	embedIter, err := ps.db.NewIter(&pebble.IterOptions{LowerBound: eLo, UpperBound: eHi})
	if err != nil {
		return fmt.Errorf("scan engrams: create embed iter: %w", err)
	}
	defer embedIter.Close()
	embedValid := embedIter.First()

	for valid := iter.First(); valid; valid = iter.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}

		k := iter.Key()
		if len(k) < 25 { // 1 prefix + 8 ws + 16 ULID minimum
			continue
		}

		rawVal := make([]byte, len(iter.Value()))
		copy(rawVal, iter.Value())

		erfEng, decErr := erf.Decode(rawVal)
		if decErr != nil {
			continue
		}

		eng := fromERFEngram(erfEng)

		// Advance embedding iterator to the matching id using a forward seek.
		var engID [16]byte
		copy(engID[:], k[9:25])
		for embedValid {
			ek := embedIter.Key()
			if len(ek) < 25 {
				embedValid = embedIter.Next()
				continue
			}
			var eID [16]byte
			copy(eID[:], ek[9:25])
			if eID == engID {
				// Matching embedding found — decode and attach.
				ev := make([]byte, len(embedIter.Value()))
				copy(ev, embedIter.Value())
				if len(ev) >= 8 {
					params := erf.DecodeQuantizeParams([8]byte(ev[:8]))
					quantized := make([]int8, len(ev)-8)
					for i := range quantized {
						quantized[i] = int8(ev[8+i])
					}
					eng.Embedding = erf.Dequantize(quantized, params)
				}
				embedValid = embedIter.Next()
				break
			} else if bytes.Compare(eID[:], engID[:]) > 0 {
				// Embedding iterator is ahead — this engram has no embedding key.
				break
			}
			embedValid = embedIter.Next()
		}

		if err := fn(eng); err != nil {
			return err
		}
	}
	return iter.Error()
}
