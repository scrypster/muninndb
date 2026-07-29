package storage

import (
	"context"
	"errors"
	"fmt"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/prefix"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

// clearVaultDataPrefixes lists every vault-scoped data prefix that ClearVault
// deletes via range tombstones. It is hoisted to package scope so that the
// per-list partition guard (TestClearVaultDataPrefixes_Scope) can pin its
// membership directly.
//
// 0x0E (vault meta key), 0x0F (name index), 0x11 (digest flags), 0x1F (entity
// records) are intentionally excluded — they are global or name keys cleared
// separately by DeleteVaultNameOnly / entity prune. 0x23 (entity reverse index)
// is deleted row-by-row in deleteVaultEntityReverseIndex (the row layout embeds
// the vault prefix mid-key, so a prefix-range tombstone cannot target it).
var clearVaultDataPrefixes = []byte{
	prefix.Engram, prefix.Meta, prefix.AssocFwd, prefix.AssocRev,
	prefix.FTSPosting, prefix.Trigram, prefix.HNSWNode, prefix.FTSStats,
	prefix.TermStats, prefix.Contradiction, prefix.StateIndex, prefix.TagIndex,
	prefix.CreatorIndex, prefix.RelevanceBucket, prefix.Coherence, prefix.VaultWeights,
	prefix.AssocWeightIndex, prefix.VaultCount, prefix.Provenance, prefix.BucketMigration,
	prefix.EntityEngramLink, prefix.Relationship, prefix.LastAccess, prefix.CoOccurrence,
	prefix.ArchiveAssoc, prefix.RelEntityIndex, prefix.DreamState, prefix.ContentHash,
	prefix.RecallEvent, // recall events hold raw query text; must not outlive a cleared vault
	prefix.Lease,
	prefix.EvolveRepairMark,  // cleared vault has nothing to repair; next boot re-scans and re-marks
	prefix.RawTagRange,       // S1 ordered raw-tag index; must not resurrect on vault-name reuse
	prefix.ProspectiveIntent, // armed intentions (THE PUSH); a cleared vault must not keep firing notices
}

// ClearVault deletes all data keys for a vault using Pebble range tombstones.
// The vault name registration (0x0E, 0x0F) is preserved — use DeleteVaultNameOnly
// to remove those after clearing.
// Returns the vault engram count captured before deletion.
//
// Safe TOCTOU ordering:
//  1. Point-delete VaultCountKey (0x15, 9-byte) from Pebble FIRST to prevent a
//     concurrent writer from re-seeding the counter from the stale persisted value.
//  2. Evict the in-memory vaultCounters entry — any subsequent write that races
//     here now seeds from a scan of the (already range-tombstoned) key space → 0.
//  3. Commit the range tombstones for all vault-scoped data prefixes.
//  4. Evict all in-memory caches (L1, assocCache, metaCache, recentActiveCache).
//
// Prefixes cleared (vault-scoped): 0x01–0x0D, 0x10, 0x12–0x17,
// 0x20–0x22, 0x24–0x2D
// Prefixes NOT cleared (global or name keys):
//   - 0x0E vault meta key (preserved by Clear, deleted by DeleteVaultNameOnly)
//   - 0x0F name index    (global by name hash, deleted by DeleteVaultNameOnly)
//   - 0x11 digest flags  (globally keyed by ULID — orphans are acceptable)
//   - 0x1F entity records (global by entity hash; orphan records are pruned
//     after vault-scoped links are removed)
func (ps *PebbleStore) ClearVault(ctx context.Context, ws [8]byte) (int64, error) {
	// Capture count before anything is deleted.
	vaultCount := ps.GetVaultCount(ctx, ws)
	entityMentions, err := ps.collectVaultEntityMentions(ws)
	if err != nil {
		return 0, fmt.Errorf("clear vault: collect entity mentions: %w", err)
	}

	// Step 1: point-delete VaultCountKey from Pebble FIRST (prevents stale re-seed).
	// 0x15 | ws[8] = 9 bytes — the short form of the count key (not the EpisodeKey).
	vaultCountKey := keys.VaultCountKey(ws)
	if err := ps.db.Delete(vaultCountKey, pebble.NoSync); err != nil && !errors.Is(err, pebble.ErrNotFound) {
		return 0, fmt.Errorf("clear vault: delete count key: %w", err)
	}

	// Step 2: evict in-memory counter (any concurrent write now re-seeds from scan → 0).
	ps.vaultCounters.Delete(ws)
	// Also drain any pending coalescer entry so a concurrent 100ms flush cannot
	// write the stale count back to Pebble after the point-delete in Step 1.
	ps.counterFlush.Delete(ws)

	// Step 2b: drain any in-flight provenance writes (0x16 keys) so they land
	// BEFORE the range tombstones, not after. If they landed after the tombstones
	// they would be visible to iterators despite the vault being cleared.
	if ps.provWork != nil {
		ps.provWork.Drain()
	}

	// Step 3: DeleteRange for all vault-scoped data prefixes.
	// 0x0E (vault meta), 0x0F (name index), 0x11 (digest flags) are intentionally excluded.
	dataPrefixes := clearVaultDataPrefixes
	wsPlus, err := incrementWS(ws)
	if err != nil {
		return 0, fmt.Errorf("clear vault: %w", err)
	}
	batch := ps.db.NewBatch()
	defer batch.Close()
	for _, p := range dataPrefixes {
		lo := make([]byte, 9)
		lo[0] = p
		copy(lo[1:], ws[:])
		hi := make([]byte, 9)
		hi[0] = p
		copy(hi[1:], wsPlus[:])
		if err := batch.DeleteRange(lo, hi, nil); err != nil {
			// On error here, Steps 1-2 have already removed VaultCountKey from Pebble
			// and evicted the in-memory counter, but data keys remain.
			// Recovery on restart via getOrInitCounter scan is self-healing.
			return 0, fmt.Errorf("clear vault: delete range 0x%02X: %w", p, err)
		}
	}
	if err := ps.deleteVaultEntityReverseIndex(batch, ws); err != nil {
		return 0, fmt.Errorf("clear vault: delete entity reverse index: %w", err)
	}
	if err := batch.Commit(pebble.Sync); err != nil {
		return 0, fmt.Errorf("clear vault: commit: %w", err)
	}
	for name, count := range entityMentions {
		for i := 0; i < count; i++ {
			if err := ps.DecrementEntityMentionCount(ctx, name); err != nil {
				return 0, fmt.Errorf("clear vault: decrement entity %q: %w", name, err)
			}
		}
	}

	// Step 4: evict all in-memory caches.

	// L1 engram cache — vault-scoped by hex prefix.
	ps.cache.DeleteByVault(ws)

	// assocCache: keys are [24]byte = ws[8] + engramID[16].
	// Purge all entries whose first 8 bytes match the cleared vault prefix.
	for _, k := range ps.assocCache.Keys() {
		if [8]byte(k[:8]) == ws {
			ps.assocCache.Remove(k)
		}
	}

	// metaCache: keys are [16]byte (engramID only — not vault-scoped).
	// We cannot filter by vault, so clear all entries. The cache is a
	// read-through; evicting unrelated vaults only costs one extra Pebble read
	// per metadata access, which is acceptable.
	ps.metaCache.Purge()

	// recentActiveCache: keys are [8]byte (wsPrefix).
	ps.recentActiveCache.Delete(ws)

	return vaultCount, nil
}

func (ps *PebbleStore) collectVaultEntityMentions(ws [8]byte) (map[string]int, error) {
	prefixPre := make([]byte, 9)
	prefixPre[0] = prefix.EntityEngramLink
	copy(prefixPre[1:], ws[:])
	wsPlus, err := incrementWS(ws)
	if err != nil {
		return nil, err
	}
	upperBound := make([]byte, 9)
	upperBound[0] = prefix.EntityEngramLink
	copy(upperBound[1:], wsPlus[:])

	iter, err := ps.db.NewIter(&pebble.IterOptions{LowerBound: prefixPre, UpperBound: upperBound})
	if err != nil {
		return nil, err
	}
	defer iter.Close()

	mentions := make(map[string]int)
	for valid := iter.First(); valid; valid = iter.Next() {
		name := string(iter.Value())
		if name != "" {
			mentions[name]++
		}
	}
	if err := iter.Error(); err != nil {
		return nil, err
	}
	return mentions, nil
}

func (ps *PebbleStore) deleteVaultEntityReverseIndex(batch *pebble.Batch, ws [8]byte) error {
	iter, err := ps.db.NewIter(&pebble.IterOptions{
		LowerBound: []byte{prefix.EntityReverseIndex},
		UpperBound: []byte{prefix.CoOccurrence},
	})
	if err != nil {
		return err
	}
	defer iter.Close()

	for valid := iter.First(); valid; valid = iter.Next() {
		k := iter.Key()
		if len(k) != 33 {
			continue
		}
		var gotWS [8]byte
		copy(gotWS[:], k[9:17])
		if gotWS != ws {
			continue
		}
		keyCopy := make([]byte, len(k))
		copy(keyCopy, k)
		if err := batch.Delete(keyCopy, nil); err != nil {
			return err
		}
	}
	return iter.Error()
}

// DeleteVaultNameOnly removes the vault name registration keys (0x0E and 0x0F)
// and evicts the in-memory vault name caches.
// Must be called AFTER ClearVault so that the data keys are already gone.
func (ps *PebbleStore) DeleteVaultNameOnly(ctx context.Context, name string, ws [8]byte) error {
	// Point-delete 0x0E vault meta key (prefix → name mapping).
	if err := ps.db.Delete(keys.VaultMetaKey(ws), pebble.Sync); err != nil && !errors.Is(err, pebble.ErrNotFound) {
		return fmt.Errorf("delete vault name: remove meta key: %w", err)
	}
	// Point-delete 0x0F name index key (name → prefix mapping).
	if err := ps.db.Delete(keys.VaultNameIndexKey(name), pebble.Sync); err != nil && !errors.Is(err, pebble.ErrNotFound) {
		return fmt.Errorf("delete vault name: remove name index key: %w", err)
	}
	// Evict in-memory name caches.
	ps.vaultPrefixCache.Remove(name)
	ps.vaultNameWritten.Delete(ws)
	return nil
}
