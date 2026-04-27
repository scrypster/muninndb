package storage

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/oklog/ulid/v2"
	"github.com/scrypster/muninndb/internal/storage/erf"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

// recentActiveCacheTTL is how long RecentActive results are cached per vault.
const recentActiveCacheTTL = 1 * time.Second

// RecentActive returns up to topK engram IDs with the highest relevance in the vault.
// Uses the 0x10 bucket index for O(k) scanning instead of scanning all engrams.
// Results are cached per vault for recentActiveCacheTTL to avoid repeated SSTable scans.
func (ps *PebbleStore) RecentActive(ctx context.Context, wsPrefix [8]byte, topK int) ([]ULID, error) {
	// Check cache first.
	now := time.Now().UnixNano()
	if v, ok := ps.recentActiveCache.Load(wsPrefix); ok {
		entry := v.(*recentActiveCacheEntry)
		if now < entry.expires {
			return entry.ids, nil
		}
	}

	// Build lower bound for bucket key scan: 0x10 | wsPrefix | storedBucket=0x00 | id={0}
	// This gets us the highest relevance bucket first (bucket 0 = 1.0 relevance)
	lowerBound := keys.RelevanceBucketKey(wsPrefix, 1.0, [16]byte{})
	lowerBound = lowerBound[0 : 1+8+1] // 0x10 | wsPrefix | bucket byte

	// Build upper bound: 0x10 | wsPrefix | 0xFF | {FF...}
	// This keeps the scan within the wsPrefix namespace
	upperBound := make([]byte, 1+8+1+16)
	upperBound[0] = 0x10
	copy(upperBound[1:9], wsPrefix[:])
	upperBound[9] = 0xFF
	for i := 10; i < 26; i++ {
		upperBound[i] = 0xFF
	}

	iter, err := ps.pebbleReader(ctx).NewIter(&pebble.IterOptions{
		LowerBound: lowerBound,
		UpperBound: upperBound,
	})
	if err != nil {
		return nil, fmt.Errorf("recent active iter: %w", err)
	}
	defer iter.Close()

	ids := []ULID{}
	seen := make(map[ULID]struct{})
	count := 0

	for iter.First(); iter.Valid() && count < topK; iter.Next() {
		// Extract ULID from key
		// Key format: 0x10 | wsPrefix(8) | storedBucket(1) | id(16)
		key := iter.Key()
		if len(key) >= 26 {
			var id ULID
			copy(id[:], key[10:26])
			if _, dup := seen[id]; !dup {
				seen[id] = struct{}{}
				ids = append(ids, id)
				count++
			}
		}
	}

	// Cache the result for recentActiveCacheTTL.
	ps.recentActiveCache.Store(wsPrefix, &recentActiveCacheEntry{
		ids:     ids,
		expires: time.Now().Add(recentActiveCacheTTL).UnixNano(),
	})

	return ids, nil
}

// ListByState returns up to limit engram IDs whose state matches the given
// lifecycle state, scanned from the 0x0B state secondary index.
func (ps *PebbleStore) ListByState(ctx context.Context, wsPrefix [8]byte, state LifecycleState, limit int) ([]ULID, error) {
	return ps.ListByStateFrom(ctx, wsPrefix, state, ULID{}, limit)
}

// ListByStateFrom is the cursor-based variant of ListByState.
// afterID is the exclusive lower-bound cursor — pass a zero ULID to start from the beginning.
// The lower bound is computed as append(StateIndexKey(ws, state, afterID), 0x00), which
// creates a 27-byte key strictly greater than the 26-byte cursor key, excluding afterID.
// Returns at most limit IDs in state-index order (ULID / insertion order).
func (ps *PebbleStore) ListByStateFrom(ctx context.Context, wsPrefix [8]byte, state LifecycleState, afterID ULID, limit int) ([]ULID, error) {
	if limit <= 0 {
		return nil, nil
	}

	// Upper bound: the first key of the next state byte.
	// StateActive = 0x01 which is well below 255, but guard explicitly.
	if uint8(state) == 255 {
		return nil, nil
	}
	upper := keys.StateIndexKey(wsPrefix, uint8(state)+1, [16]byte{})

	// Lower bound: strictly after the cursor.
	// If afterID is zero (all-zero bytes), append(afterKey, 0x00) is a 27-byte key
	// greater than the all-zero 26-byte key — this correctly starts from the beginning
	// of the state partition for this vault.
	afterKey := keys.StateIndexKey(wsPrefix, uint8(state), afterID)
	lower := append(afterKey, 0x00) // 27 bytes > any 26-byte key with same prefix

	iter, err := ps.db.NewIter(&pebble.IterOptions{LowerBound: lower, UpperBound: upper})
	if err != nil {
		return nil, err
	}
	defer iter.Close()

	const idOffset = 10
	const keyLen = 26

	var ids []ULID
	for valid := iter.First(); valid && len(ids) < limit; valid = iter.Next() {
		k := iter.Key()
		if len(k) < keyLen {
			continue
		}
		var id ULID
		copy(id[:], k[idOffset:idOffset+16])
		ids = append(ids, id)
	}
	if err := iter.Error(); err != nil {
		return nil, err
	}
	return ids, nil
}

// ulidMinFromTime builds the minimum possible ULID for a given timestamp.
// Random bits all zero — smallest ULID at that millisecond.
func ulidMinFromTime(t time.Time) [16]byte {
	ms := ulid.Timestamp(t)
	var id ulid.ULID
	binary.BigEndian.PutUint32(id[0:4], uint32(ms>>16))
	binary.BigEndian.PutUint16(id[4:6], uint16(ms))
	return [16]byte(id)
}

// ulidMaxFromTime builds the maximum possible ULID for a given timestamp.
// Random bits all 0xFF — largest ULID at that millisecond.
func ulidMaxFromTime(t time.Time) [16]byte {
	ms := ulid.Timestamp(t)
	var id ulid.ULID
	binary.BigEndian.PutUint32(id[0:4], uint32(ms>>16))
	binary.BigEndian.PutUint16(id[4:6], uint16(ms))
	for i := 6; i < 16; i++ {
		id[i] = 0xFF
	}
	return [16]byte(id)
}

// EngramsByCreatedSince returns engrams created at or after since, ordered by
// creation time ascending (i.e. ULID order), with offset/limit for pagination.
// Since ULIDs embed a 48-bit millisecond timestamp in bytes 0-5, we can
// construct a lower-bound ULID and scan the 0x01 key range efficiently.
func (ps *PebbleStore) EngramsByCreatedSince(ctx context.Context, wsPrefix [8]byte, since time.Time, offset, limit int) ([]*Engram, error) {
	if limit <= 0 {
		limit = 50
	}

	// Build minimum ULID for the since timestamp (all random bits = 0x00).
	ms := ulid.Timestamp(since)
	var minID ulid.ULID
	binary.BigEndian.PutUint32(minID[0:4], uint32(ms>>16))
	binary.BigEndian.PutUint16(minID[4:6], uint16(ms))
	// bytes 6-15 remain zero (minimum random portion)

	// Build the scan bounds within the vault.
	// Lower: 0x01 | wsPrefix | minID
	// Upper: 0x01 | wsPrefix+1  (all keys in vault prefix)
	lowerKey := keys.EngramKey(wsPrefix, [16]byte(minID))

	upperWS := wsPrefix
	for i := 7; i >= 0; i-- {
		upperWS[i]++
		if upperWS[i] != 0 {
			break
		}
	}
	upperKey := make([]byte, 1+8)
	upperKey[0] = 0x01
	copy(upperKey[1:9], upperWS[:])

	iter, err := ps.db.NewIter(&pebble.IterOptions{
		LowerBound: lowerKey,
		UpperBound: upperKey,
	})
	if err != nil {
		return nil, err
	}
	defer iter.Close()

	var engrams []*Engram
	skipped := 0
	for valid := iter.First(); valid; valid = iter.Next() {
		key := iter.Key()
		if len(key) < 25 {
			continue
		}

		// Skip offset entries.
		if skipped < offset {
			skipped++
			continue
		}

		val := make([]byte, len(iter.Value()))
		copy(val, iter.Value())

		meta, concept, err := erf.DecodeMetaConcept(val)
		if err != nil {
			continue
		}

		var id ULID
		copy(id[:], key[9:25])
		engrams = append(engrams, &Engram{
			ID:        id,
			Concept:   concept,
			CreatedAt: meta.CreatedAt,
		})

		if len(engrams) >= limit {
			break
		}
	}
	return engrams, nil
}

// CountEngrams returns the total number of engrams across all vaults by scanning
// the 0x01 key prefix. Called once at startup to seed the in-memory counter.
func (ps *PebbleStore) CountEngrams(ctx context.Context) (int64, error) {
	iter, err := ps.db.NewIter(&pebble.IterOptions{
		LowerBound: []byte{0x01},
		UpperBound: []byte{0x02},
	})
	if err != nil {
		return 0, err
	}
	defer iter.Close()

	var count int64
	for valid := iter.First(); valid; valid = iter.Next() {
		if len(iter.Key()) >= 25 { // 1 prefix + 8 ws + 16 ulid
			count++
		}
	}
	return count, nil
}

// CountEngramsByDay counts engrams per UTC day between since and until
// (inclusive) for a single vault. It scans only the 0x01 key prefix and
// extracts the millisecond timestamp from each ULID without reading values,
// making it efficient even for large date ranges.
func (ps *PebbleStore) CountEngramsByDay(ctx context.Context, wsPrefix [8]byte, since, until time.Time) (map[string]int64, error) {
	minID := ulidMinFromTime(since)
	maxID := ulidMaxFromTime(until)

	lowerKey := keys.EngramKey(wsPrefix, minID)
	upperKey := keys.EngramKey(wsPrefix, maxID)
	// Pebble's UpperBound is exclusive. Appending a 0x00 byte ensures the
	// iterator includes keys that exactly match maxID.
	upperKey = append(upperKey, 0x00)

	iter, err := ps.db.NewIter(&pebble.IterOptions{
		LowerBound: lowerKey,
		UpperBound: upperKey,
	})
	if err != nil {
		return nil, err
	}
	defer iter.Close()

	counts := make(map[string]int64)
	for valid := iter.First(); valid; valid = iter.Next() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		key := iter.Key()
		if len(key) < 25 {
			continue
		}
		// Extract 48-bit ms timestamp from the ULID portion (bytes 9-14).
		ms := uint64(binary.BigEndian.Uint32(key[9:13]))<<16 | uint64(binary.BigEndian.Uint16(key[13:15]))
		t := time.Unix(int64(ms/1000), int64(ms%1000)*1e6).UTC()
		day := t.Format("2006-01-02")
		counts[day]++
	}
	if err := iter.Error(); err != nil {
		return nil, err
	}
	return counts, nil
}

// ListByStateInRange returns engram IDs with the given state created between since and until.
// Leverages ULID time-ordering in the state index for an O(results) scan.
func (ps *PebbleStore) ListByStateInRange(ctx context.Context, wsPrefix [8]byte, state LifecycleState, since, until time.Time, limit int) ([]ULID, error) {
	if limit <= 0 {
		limit = 50
	}
	minID := ulidMinFromTime(since)
	maxID := ulidMaxFromTime(until)
	lower := keys.StateIndexKey(wsPrefix, uint8(state), minID)
	upper := keys.StateIndexKey(wsPrefix, uint8(state), maxID)
	iter, err := ps.db.NewIter(&pebble.IterOptions{LowerBound: lower, UpperBound: upper})
	if err != nil {
		return nil, err
	}
	defer iter.Close()
	const idOffset = 10
	const keyLen = 26
	var ids []ULID
	for valid := iter.First(); valid && len(ids) < limit; valid = iter.Next() {
		k := iter.Key()
		if len(k) < keyLen {
			continue
		}
		var id ULID
		copy(id[:], k[idOffset:idOffset+16])
		ids = append(ids, id)
	}
	return ids, nil
}

// ListByTagInRange returns engram IDs with the given tag created between since and until.
// Leverages ULID time-ordering in the tag index for an O(results) scan.
func (ps *PebbleStore) ListByTagInRange(ctx context.Context, wsPrefix [8]byte, tag string, since, until time.Time, limit int) ([]ULID, error) {
	if limit <= 0 {
		limit = 50
	}
	tagHash := keys.Hash(tag)
	minID := ulidMinFromTime(since)
	maxID := ulidMaxFromTime(until)
	lower := keys.TagIndexKey(wsPrefix, tagHash, minID)
	upper := keys.TagIndexKey(wsPrefix, tagHash, maxID)
	iter, err := ps.db.NewIter(&pebble.IterOptions{LowerBound: lower, UpperBound: upper})
	if err != nil {
		return nil, err
	}
	defer iter.Close()
	const idOffset = 13 // 0x0C(1) + ws(8) + tagHash(4) = 13
	const keyLen = 29
	var ids []ULID
	for valid := iter.First(); valid && len(ids) < limit; valid = iter.Next() {
		k := iter.Key()
		if len(k) < keyLen {
			continue
		}
		var id ULID
		copy(id[:], k[idOffset:idOffset+16])
		ids = append(ids, id)
	}
	return ids, nil
}

// ListByCreatorInRange returns engram IDs by creator created between since and until.
// Leverages ULID time-ordering in the creator index for an O(results) scan.
func (ps *PebbleStore) ListByCreatorInRange(ctx context.Context, wsPrefix [8]byte, creator string, since, until time.Time, limit int) ([]ULID, error) {
	if limit <= 0 {
		limit = 50
	}
	creatorHash := keys.Hash(creator)
	minID := ulidMinFromTime(since)
	maxID := ulidMaxFromTime(until)
	lower := keys.CreatorIndexKey(wsPrefix, creatorHash, minID)
	upper := keys.CreatorIndexKey(wsPrefix, creatorHash, maxID)
	iter, err := ps.db.NewIter(&pebble.IterOptions{LowerBound: lower, UpperBound: upper})
	if err != nil {
		return nil, err
	}
	defer iter.Close()
	const idOffset = 13 // 0x0D(1) + ws(8) + creatorHash(4) = 13
	const keyLen = 29
	var ids []ULID
	for valid := iter.First(); valid && len(ids) < limit; valid = iter.Next() {
		k := iter.Key()
		if len(k) < keyLen {
			continue
		}
		var id ULID
		copy(id[:], k[idOffset:idOffset+16])
		ids = append(ids, id)
	}
	return ids, nil
}

// EngramIDsByCreatedRange returns engram IDs created between since and until,
// ordered by creation time (ULID order). Returns at most limit IDs.
// This is used for time-bounded candidate injection in the activation pipeline
// when since/before filters are present.
func (ps *PebbleStore) EngramIDsByCreatedRange(ctx context.Context, wsPrefix [8]byte, since, until time.Time, limit int) ([]ULID, error) {
	if limit <= 0 {
		limit = 50
	}

	// Build lower and upper ULID bounds from timestamps.
	minID := ulidMinFromTime(since)
	maxID := ulidMaxFromTime(until)

	// Build scan bounds within the vault 0x01 key range.
	// Lower: 0x01 | wsPrefix | minID
	// Upper: 0x01 | wsPrefix | maxID
	lowerKey := keys.EngramKey(wsPrefix, [16]byte(minID))

	// For upper bound, we need the next ULID after maxID, or just use maxID + epsilon.
	// Using the maxID directly works because pebble's UpperBound is exclusive.
	maxIDBytes := [16]byte(maxID)
	// Increment maxID by 1 to make it exclusive
	for i := 15; i >= 0; i-- {
		maxIDBytes[i]++
		if maxIDBytes[i] != 0 {
			break
		}
	}
	upperKey := keys.EngramKey(wsPrefix, maxIDBytes)

	iter, err := ps.pebbleReader(ctx).NewIter(&pebble.IterOptions{
		LowerBound: lowerKey,
		UpperBound: upperKey,
	})
	if err != nil {
		return nil, err
	}
	defer iter.Close()

	var ids []ULID
	for valid := iter.First(); valid && len(ids) < limit; valid = iter.Next() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		key := iter.Key()
		if len(key) < 25 { // 1 prefix + 8 ws + 16 ulid
			continue
		}

		// Extract ULID from key: key[9:25]
		var id ULID
		copy(id[:], key[9:25])
		ids = append(ids, id)
	}

	if err := iter.Error(); err != nil {
		return nil, err
	}
	return ids, nil
}

// MigrateBuckets migrates the relevance bucket index from the old 10-bucket scheme
// to the new 100-bucket scheme. Safe to call multiple times (idempotent).
// Uses cursor-based chunking so a crash mid-migration can resume safely.
const bucketMigrationVersion = uint8(2)

func (ps *PebbleStore) MigrateBuckets(ctx context.Context, wsPrefix [8]byte) error {
	migKey := keys.BucketMigrationKey(wsPrefix)

	// Check if already migrated
	val, err := Get(ps.db, migKey)
	if err == nil && len(val) >= 1 && val[0] >= bucketMigrationVersion {
		return nil
	}

	// Determine cursor (for crash-safe resume)
	var cursor [16]byte
	if len(val) >= 17 {
		copy(cursor[:], val[1:17])
	}

	lower := keys.MetaKey(wsPrefix, cursor)
	upperWS := wsPrefix
	for i := 7; i >= 0; i-- {
		upperWS[i]++
		if upperWS[i] != 0 {
			break
		}
	}
	upper := make([]byte, 1+8)
	upper[0] = 0x02
	copy(upper[1:9], upperWS[:])

	iter, err := ps.db.NewIter(&pebble.IterOptions{LowerBound: lower, UpperBound: upper})
	if err != nil {
		return fmt.Errorf("migrate buckets scan: %w", err)
	}
	defer iter.Close()

	type rebucketEntry struct {
		id        [16]byte
		relevance float32
		oldBucket uint8
	}

	const migrateChunkSize = 5000
	chunk := make([]rebucketEntry, 0, migrateChunkSize)
	var lastID [16]byte

	flushMigrationChunk := func(isFinal bool) error {
		if len(chunk) == 0 {
			return nil
		}
		batch := ps.db.NewBatch()
		defer batch.Close()

		for _, e := range chunk {
			// Delete the specific old-scheme bucket key
			oldKey := make([]byte, 1+8+1+16)
			oldKey[0] = 0x10
			copy(oldKey[1:9], wsPrefix[:])
			oldKey[9] = e.oldBucket
			copy(oldKey[10:26], e.id[:])
			batch.Delete(oldKey, nil)

			// Write new 100-bucket key (only if different from old key)
			newKey := keys.RelevanceBucketKey(wsPrefix, e.relevance, e.id)
			batch.Set(newKey, []byte{}, nil)
		}

		// Update migration state key
		var stateVal []byte
		if isFinal {
			stateVal = []byte{bucketMigrationVersion}
		} else {
			stateVal = make([]byte, 1+16)
			stateVal[0] = 0 // in progress
			copy(stateVal[1:], lastID[:])
		}
		batch.Set(migKey, stateVal, nil)

		if err := batch.Commit(pebble.NoSync); err != nil {
			return fmt.Errorf("migrate chunk commit: %w", err)
		}
		chunk = chunk[:0]
		return nil
	}

	for valid := iter.First(); valid; valid = iter.Next() {
		k := iter.Key()
		if len(k) < 25 {
			continue
		}
		var id [16]byte
		copy(id[:], k[9:25])
		lastID = id

		// Decode relevance from the metadata value (byte range OffsetRelevance:OffsetRelevance+4)
		v := iter.Value()
		if len(v) < erf.OffsetTablePos {
			continue
		}
		relevanceBits := binary.BigEndian.Uint32(v[erf.OffsetRelevance : erf.OffsetRelevance+4])
		relevance := math.Float32frombits(relevanceBits)

		// Compute old 10-bucket storedBucket value
		oldFloored := int(math.Floor(float64(relevance) * 10))
		if oldFloored < 0 {
			oldFloored = 0
		}
		if oldFloored > 9 {
			oldFloored = 9
		}
		oldBucket := uint8(9 - oldFloored)

		chunk = append(chunk, rebucketEntry{id: id, relevance: relevance, oldBucket: oldBucket})

		if len(chunk) >= migrateChunkSize {
			if err := flushMigrationChunk(false); err != nil {
				return err
			}
		}
	}
	if err := iter.Error(); err != nil {
		return fmt.Errorf("migrate buckets iter: %w", err)
	}
	return flushMigrationChunk(true)
}

// LowestRelevanceIDs returns up to topK engram IDs with the lowest relevance in the vault.
// Scans the 0x10 relevance bucket index in reverse order (bucket 9 = lowest relevance first).
// Used by the vault pruning sweep to identify candidates for eviction under MaxEngrams policy.
func (ps *PebbleStore) LowestRelevanceIDs(ctx context.Context, wsPrefix [8]byte, topK int) ([]ULID, error) {
	if topK <= 0 {
		return nil, nil
	}

	// Build scan bounds: 0x10 | wsPrefix | [0x00..0xFF]
	// Lower: 0x10 | wsPrefix | 0x00 | {0...}
	lowerBound := make([]byte, 1+8+1+16)
	lowerBound[0] = 0x10
	copy(lowerBound[1:9], wsPrefix[:])
	// bucket byte and id bytes remain 0x00 — minimum key in wsPrefix bucket space

	// Upper: 0x10 | wsPrefix | 0xFF | {FF...}
	upperBound := make([]byte, 1+8+1+16)
	upperBound[0] = 0x10
	copy(upperBound[1:9], wsPrefix[:])
	upperBound[9] = 0xFF
	for i := 10; i < 26; i++ {
		upperBound[i] = 0xFF
	}

	iter, err := ps.db.NewIter(&pebble.IterOptions{
		LowerBound: lowerBound,
		UpperBound: upperBound,
	})
	if err != nil {
		return nil, fmt.Errorf("lowest relevance iter: %w", err)
	}
	defer iter.Close()

	// Scan in reverse order: highest storedBucket byte first (= lowest relevance).
	var ids []ULID
	seen := make(map[ULID]struct{})
	for valid := iter.Last(); valid && len(ids) < topK; valid = iter.Prev() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		key := iter.Key()
		// Key format: 0x10 | wsPrefix(8) | storedBucket(1) | id(16) = 26 bytes
		if len(key) < 26 {
			continue
		}
		var id ULID
		copy(id[:], key[10:26])
		if _, dup := seen[id]; !dup {
			seen[id] = struct{}{}
			ids = append(ids, id)
		}
	}
	if err := iter.Error(); err != nil {
		return nil, fmt.Errorf("lowest relevance iter scan: %w", err)
	}
	return ids, nil
}
