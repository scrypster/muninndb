package storage

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/prefix"
	"github.com/scrypster/muninndb/internal/storage/erf"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

// incrementWS returns a copy of ws with the value incremented by 1 (big-endian),
// suitable for use as an exclusive upper bound in Pebble iterator range scans.
// Returns an error if ws is all 0xFF, since incrementing would wrap to all zeros
// and a DeleteRange/scan would cover the wrong keyspace.
func incrementWS(ws [8]byte) ([8]byte, error) {
	var allFF [8]byte
	for i := range allFF {
		allFF[i] = 0xFF
	}
	if ws == allFF {
		return [8]byte{}, fmt.Errorf("vault workspace prefix overflow: all-0xFF is unsupported")
	}
	out := ws
	for i := 7; i >= 0; i-- {
		out[i]++
		if out[i] != 0 {
			break
		}
	}
	return out, nil
}

// vaultScopedPrefixes lists every prefix that is scoped by a vault workspace.
// 0x01 (engrams) is handled separately with ERF decode/modify/encode.
// 0x0E (VaultMetaKey), 0x0F (VaultNameIndexKey), 0x11 (DigestFlagsKey) are
// global or handled explicitly; skip them here.
// 0x12 (CoherenceKey) is handled separately (reset for clone; merged for merge).
// 0x13 (VaultWeightsKey) is intentionally omitted — weights are vault-specific
// and must not be inherited from the source.
var vaultScopedSwapPrefixes = []byte{
	prefix.Meta, prefix.AssocFwd, prefix.AssocRev, prefix.FTSPosting,
	prefix.Trigram, prefix.HNSWNode, prefix.FTSStats, prefix.TermStats,
	prefix.Contradiction, prefix.StateIndex, prefix.TagIndex, prefix.CreatorIndex,
	prefix.RelevanceBucket, prefix.AssocWeightIndex, prefix.VaultCount, prefix.Provenance,
	prefix.BucketMigration, prefix.ContentHash, prefix.RawTagRange, prefix.UpsertKey,
}

const cloneBatchSize = 512

// CloneVaultData copies all engrams and index data from wsSource to wsTarget.
//
// For engrams (0x01 prefix), the ERF blob is decoded, AccessCount is reset to 0
// and LastAccess is CARRIED OVER from the source (falling back to CreatedAt only
// when the source's own LastAccess is unset, via normalizeEngramTimes) — then
// re-encoded before writing. See the block comment at the reset for why the
// counter is reset but the timestamp is not (#810).
// For all other vault-scoped prefixes the key prefix bytes (bytes 1–8) are
// replaced with wsTarget bytes.  The 9-byte VaultCountKey (0x15 | ws) is
// skipped and written at the end with the computed count.
//
// After copy:
//   - VaultCountKey[wsTarget] is written with the number of engrams copied.
//   - CoherenceKey[wsTarget] is written with zeroed [7]int64 counters.
//
// NOTE: WriteVaultName must be called by the caller (under vaultOpsMu) before
// launching the goroutine that calls CloneVaultData. The name cache entry is
// cleared here so the caller's prior WriteVaultName persists correctly.
//
// onCopy is called after every batch commit with the running engram total.
// Returns the number of engrams copied.
func (ps *PebbleStore) CloneVaultData(
	ctx context.Context,
	wsSource, wsTarget [8]byte,
	onCopy func(copied int64),
) (int64, error) {
	wsSourceNext, err := incrementWS(wsSource)
	if err != nil {
		return 0, fmt.Errorf("clone: %w", err)
	}

	var copiedEngrams int64

	// ---- Phase 1: Copy engrams (0x01) with ERF decode → reset → re-encode ----
	{
		lo := make([]byte, 9)
		lo[0] = prefix.Engram
		copy(lo[1:], wsSource[:])
		hi := make([]byte, 9)
		hi[0] = prefix.Engram
		copy(hi[1:], wsSourceNext[:])

		iter, err := ps.db.NewIter(&pebble.IterOptions{LowerBound: lo, UpperBound: hi})
		if err != nil {
			return 0, fmt.Errorf("clone: create engram iter: %w", err)
		}

		batch := ps.db.NewBatch()
		batchCount := 0

		for valid := iter.First(); valid; valid = iter.Next() {
			// I1: Check for engine shutdown / context cancellation.
			select {
			case <-ctx.Done():
				batch.Close()
				iter.Close()
				return copiedEngrams, ctx.Err()
			default:
			}

			k := iter.Key()
			if len(k) < 25 { // 1 prefix + 8 ws + 16 ULID minimum
				continue
			}

			rawVal := make([]byte, len(iter.Value()))
			copy(rawVal, iter.Value())

			erfEng, decErr := erf.Decode(rawVal)
			if decErr != nil {
				// Skip corrupt entries — do not abort the entire clone.
				slog.Warn("clone: skipping corrupt engram", "key", fmt.Sprintf("%x", k), "err", decErr)
				continue
			}

			// Access metadata for the clone: the COUNTER is reset, the
			// TIMESTAMP is carried over. The two are not the same kind of
			// thing, and #810 is the proof.
			//
			// AccessCount has a meaningful zero — 0 genuinely means "no reads
			// in this vault yet" — it degrades gracefully (ACT-R consumes
			// ln(n+1), bounded by the base-level cap) and it recovers, because
			// the first read of each engram in the clone starts rebuilding it.
			//
			// LastAccess has NO representation for "never", which is the whole
			// of #810: the format stores uint64(t.UnixNano()), so time.Time{}
			// round-trips to 1754-08-30, whose IsZero() is false. Any value
			// invented here is therefore a false statement about WHEN, it never
			// recovers (an engram never read in the clone carries it forever),
			// and it is re-consumed on every recall. The first attempt stored
			// the zero time directly: recency pinned to 0, the decay factor
			// pinned to its 0.05 floor, and the first recall on a fresh
			// weighted_sum clone returned nothing, silently. The second
			// attempt (LastAccess := CreatedAt, via normalizeEngramTimes)
			// removed the sentinel but reached the SAME arithmetic by another
			// route on any vault that is actually used: #682's ReinforceOnRead
			// keeps LastAccess fresh on old memories, and rewinding it to a
			// CreatedAt months back is an equally large daysSince. Measured
			// break points on a one-recall cold-cache clone: DEFAULT ACT-R at 7
			// days of source age, weighted_sum at ~45, rrf unaffected.
			//
			// So: carry the source's LastAccess. It keeps the clone
			// recall-equivalent to its source, and it preserves the RELATIVE
			// recency ordering across the vault, which is what recency actually
			// consumes. normalizeEngramTimes still runs, and still supplies
			// CreatedAt when the SOURCE record's own LastAccess is unset —
			// which is how a vault cloned before #810 heals as it is copied.
			erfEng.AccessCount = 0
			erfEng.CreatedAt, erfEng.UpdatedAt, erfEng.LastAccess =
				normalizeEngramTimes(erfEng.CreatedAt, erfEng.UpdatedAt, erfEng.LastAccess)

			encoded, encErr := erf.Encode(erfEng)
			if encErr != nil {
				slog.Warn("clone: skipping engram re-encode failed", "key", fmt.Sprintf("%x", k), "err", encErr)
				continue
			}

			// Build target engram key: swap ws bytes.
			newKey := make([]byte, len(k))
			newKey[0] = k[0]
			copy(newKey[1:9], wsTarget[:])
			copy(newKey[9:], k[9:])

			batch.Set(newKey, encoded, nil)
			copiedEngrams++
			batchCount++

			if batchCount >= cloneBatchSize {
				if err := batch.Commit(pebble.NoSync); err != nil {
					batch.Close()
					iter.Close()
					return copiedEngrams, fmt.Errorf("clone: commit engram batch: %w", err)
				}
				batch.Close()
				batch = ps.db.NewBatch()
				batchCount = 0
				if onCopy != nil {
					onCopy(copiedEngrams)
				}
			}
		}
		iter.Close()

		if batchCount > 0 {
			if err := batch.Commit(pebble.NoSync); err != nil {
				batch.Close()
				return copiedEngrams, fmt.Errorf("clone: commit final engram batch: %w", err)
			}
		}
		batch.Close()
		if onCopy != nil {
			onCopy(copiedEngrams)
		}
	}

	// ---- Phase 2: Copy other vault-scoped prefixes with key-prefix swap ----
	for _, p := range vaultScopedSwapPrefixes {
		lo := make([]byte, 9)
		lo[0] = p
		copy(lo[1:], wsSource[:])
		hi := make([]byte, 9)
		hi[0] = p
		copy(hi[1:], wsSourceNext[:])

		iter, err := ps.db.NewIter(&pebble.IterOptions{LowerBound: lo, UpperBound: hi})
		if err != nil {
			return copiedEngrams, fmt.Errorf("clone: iter prefix 0x%02X: %w", p, err)
		}

		batch := ps.db.NewBatch()
		batchCount := 0

		for valid := iter.First(); valid; valid = iter.Next() {
			// I1: Check for engine shutdown / context cancellation.
			select {
			case <-ctx.Done():
				batch.Close()
				iter.Close()
				return copiedEngrams, ctx.Err()
			default:
			}

			k := iter.Key()

			// Skip the 9-byte VaultCountKey (prefix.VaultCount | ws[8]) — written separately.
			if p == prefix.VaultCount && len(k) == 9 {
				continue
			}

			newKey := make([]byte, len(k))
			newKey[0] = k[0]
			copy(newKey[1:9], wsTarget[:])
			copy(newKey[9:], k[9:])

			rawVal := make([]byte, len(iter.Value()))
			copy(rawVal, iter.Value())

			batch.Set(newKey, rawVal, nil)
			batchCount++

			if batchCount >= cloneBatchSize {
				if err := batch.Commit(pebble.NoSync); err != nil {
					batch.Close()
					iter.Close()
					return copiedEngrams, fmt.Errorf("clone: commit batch prefix 0x%02X: %w", p, err)
				}
				batch.Close()
				batch = ps.db.NewBatch()
				batchCount = 0
			}
		}
		iter.Close()

		if batchCount > 0 {
			if err := batch.Commit(pebble.NoSync); err != nil {
				batch.Close()
				return copiedEngrams, fmt.Errorf("clone: commit final prefix 0x%02X: %w", p, err)
			}
		}
		batch.Close()
	}

	// ---- Phase 3: VaultCountKey for target ----
	// (WriteVaultName was already called by the engine under vaultOpsMu before
	// this goroutine was launched; the cache entry was written there.)

	// ---- Phase 4: Write computed VaultCountKey for target ----
	// Encode as BigEndian int64 (matches getOrInitCounter read path in impl.go).
	vaultCountKey := keys.VaultCountKey(wsTarget)
	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, uint64(copiedEngrams))
	if err := ps.db.Set(vaultCountKey, buf, pebble.NoSync); err != nil {
		return copiedEngrams, fmt.Errorf("clone: write vault count: %w", err)
	}
	// Seed the in-memory counter so GetVaultCount is immediately correct.
	vc := ps.getOrInitCounter(ctx, wsTarget)
	vc.count.Store(copiedEngrams)

	// ---- Phase 5: Reset coherence for target (write zeroed [7]int64) ----
	if err := ps.WriteCoherence(wsTarget, [7]int64{}); err != nil {
		return copiedEngrams, fmt.Errorf("clone: reset coherence: %w", err)
	}

	return copiedEngrams, nil
}

// MergeVaultData copies all engrams and index data from wsSource into wsTarget.
//
// Unlike CloneVaultData, engram access metadata (AccessCount, LastAccess) is
// preserved as-is.  Before writing a 0x01 engram key the target is checked for
// an existing entry: if one is found the source engram is skipped (ULID
// collision) and a WARN is logged.
//
// After copy, the coherence counters from both vaults are summed field-by-field
// and written back to the target.
//
// VaultCountKey for the target is updated with the post-merge count.
// onCopy is called after every batch commit with the running engram total.
// Returns the number of engrams merged (collision-skipped engrams not counted).
func (ps *PebbleStore) MergeVaultData(
	ctx context.Context,
	wsSource, wsTarget [8]byte,
	onCopy func(copied int64),
) (int64, error) {
	wsSourceNext, err := incrementWS(wsSource)
	if err != nil {
		return 0, fmt.Errorf("merge: %w", err)
	}

	var mergedEngrams int64

	// ---- Phase 1: Merge engrams (0x01) with collision detection ----
	{
		lo := make([]byte, 9)
		lo[0] = prefix.Engram
		copy(lo[1:], wsSource[:])
		hi := make([]byte, 9)
		hi[0] = prefix.Engram
		copy(hi[1:], wsSourceNext[:])

		iter, err := ps.db.NewIter(&pebble.IterOptions{LowerBound: lo, UpperBound: hi})
		if err != nil {
			return 0, fmt.Errorf("merge: create engram iter: %w", err)
		}

		batch := ps.db.NewBatch()
		batchCount := 0

		for valid := iter.First(); valid; valid = iter.Next() {
			// I1: Check for engine shutdown / context cancellation.
			select {
			case <-ctx.Done():
				batch.Close()
				iter.Close()
				return mergedEngrams, ctx.Err()
			default:
			}

			k := iter.Key()
			if len(k) < 25 {
				continue
			}

			// Build target key.
			newKey := make([]byte, len(k))
			newKey[0] = k[0]
			copy(newKey[1:9], wsTarget[:])
			copy(newKey[9:], k[9:])

			// ULID collision check: does the target already have this engram?
			existing, closer, getErr := ps.db.Get(newKey)
			if getErr == nil {
				closer.Close()
				_ = existing
				// Target already has this ULID — keep target version.
				if len(k) >= 25 {
					var id [16]byte
					copy(id[:], k[9:25])
					slog.Warn("merge: ULID collision, keeping target version",
						"vault_source", fmt.Sprintf("%x", wsSource[:]),
						"vault_target", fmt.Sprintf("%x", wsTarget[:]),
						"ulid", fmt.Sprintf("%x", id[:]),
					)
				}
				continue
			} else if !errors.Is(getErr, pebble.ErrNotFound) {
				// Real I/O error — abort the merge.
				batch.Close()
				iter.Close()
				return mergedEngrams, fmt.Errorf("merge: check target engram: %w", getErr)
			}

			rawVal := make([]byte, len(iter.Value()))
			copy(rawVal, iter.Value())

			batch.Set(newKey, rawVal, nil)
			mergedEngrams++
			batchCount++

			if batchCount >= cloneBatchSize {
				if err := batch.Commit(pebble.NoSync); err != nil {
					batch.Close()
					iter.Close()
					return mergedEngrams, fmt.Errorf("merge: commit engram batch: %w", err)
				}
				batch.Close()
				batch = ps.db.NewBatch()
				batchCount = 0
				if onCopy != nil {
					onCopy(mergedEngrams)
				}
			}
		}
		iter.Close()

		if batchCount > 0 {
			if err := batch.Commit(pebble.NoSync); err != nil {
				batch.Close()
				return mergedEngrams, fmt.Errorf("merge: commit final engram batch: %w", err)
			}
		}
		batch.Close()
		if onCopy != nil {
			onCopy(mergedEngrams)
		}
	}

	// ---- Phase 2: Copy other vault-scoped prefixes with per-prefix strategy ----
	//
	// Per-prefix strategy (I4):
	//   0x08, 0x09  — FTS global/per-term stats: SKIP (rebuilt by reindexVault).
	//   0x13        — Vault scoring weights: KEEP_TARGET (skip source).
	//   0x17        — Bucket migration state: KEEP_TARGET (skip source).
	//   0x15        — VaultCountKey (len==9): SKIP (recomputed in Phase 4).
	//                 Episode keys (len>9): UNION — write if target doesn't have it.
	//   default     — UNION: write source key only if target doesn't already have it.
	for _, p := range vaultScopedSwapPrefixes {
		// Prefix-level skip: FTS stats and keeper-target prefixes.
		switch p {
		case prefix.FTSStats, prefix.TermStats:
			// FTS global stats / per-term stats — rebuilt by reindexVault after merge.
			continue
		case prefix.VaultWeights:
			// Vault scoring weights — keep target's weights, don't overwrite with source.
			continue
		case prefix.BucketMigration:
			// Bucket migration state — keep target state, don't overwrite.
			continue
		}

		lo := make([]byte, 9)
		lo[0] = p
		copy(lo[1:], wsSource[:])
		hi := make([]byte, 9)
		hi[0] = p
		copy(hi[1:], wsSourceNext[:])

		iter, err := ps.db.NewIter(&pebble.IterOptions{LowerBound: lo, UpperBound: hi})
		if err != nil {
			return mergedEngrams, fmt.Errorf("merge: iter prefix 0x%02X: %w", p, err)
		}

		batch := ps.db.NewBatch()
		batchCount := 0

		for valid := iter.First(); valid; valid = iter.Next() {
			// I1: Check for engine shutdown / context cancellation.
			select {
			case <-ctx.Done():
				batch.Close()
				iter.Close()
				return mergedEngrams, ctx.Err()
			default:
			}

			k := iter.Key()

			// Build target key first (needed for existence checks below).
			newKey := make([]byte, len(k))
			newKey[0] = k[0]
			copy(newKey[1:9], wsTarget[:])
			copy(newKey[9:], k[9:])

			// Per-key strategy based on prefix.
			switch p {
			case prefix.VaultCount:
				// VaultCountKey (9 bytes): skip — recomputed in Phase 4.
				// Episode keys (>9 bytes): UNION.
				if len(k) == 9 {
					continue
				}
				// Fall through to UNION check below.
			}

			// UNION: write source value only if target does not already have this key.
			_, closer, checkErr := ps.db.Get(newKey)
			if checkErr == nil {
				closer.Close()
				continue // target already has this key — keep target value
			} else if !errors.Is(checkErr, pebble.ErrNotFound) {
				// Real I/O error — abort the merge.
				batch.Close()
				iter.Close()
				return mergedEngrams, fmt.Errorf("merge: check target key 0x%02X: %w", p, checkErr)
			}

			rawVal := make([]byte, len(iter.Value()))
			copy(rawVal, iter.Value())

			batch.Set(newKey, rawVal, nil)
			batchCount++

			if batchCount >= cloneBatchSize {
				if err := batch.Commit(pebble.NoSync); err != nil {
					batch.Close()
					iter.Close()
					return mergedEngrams, fmt.Errorf("merge: commit batch prefix 0x%02X: %w", p, err)
				}
				batch.Close()
				batch = ps.db.NewBatch()
				batchCount = 0
			}
		}
		iter.Close()

		if batchCount > 0 {
			if err := batch.Commit(pebble.NoSync); err != nil {
				batch.Close()
				return mergedEngrams, fmt.Errorf("merge: commit final prefix 0x%02X: %w", p, err)
			}
		}
		batch.Close()
	}

	// ---- Phase 3: Merge coherence counters (sum source + target) ----
	srcCoh, _, srcErr := ps.ReadCoherence(wsSource)
	if srcErr != nil {
		return mergedEngrams, fmt.Errorf("merge: read source coherence: %w", srcErr)
	}
	dstCoh, _, dstErr := ps.ReadCoherence(wsTarget)
	if dstErr != nil {
		return mergedEngrams, fmt.Errorf("merge: read target coherence: %w", dstErr)
	}
	var merged [7]int64
	for i := range merged {
		merged[i] = srcCoh[i] + dstCoh[i]
	}
	if err := ps.WriteCoherence(wsTarget, merged); err != nil {
		return mergedEngrams, fmt.Errorf("merge: write merged coherence: %w", err)
	}

	// ---- Phase 4: Update VaultCountKey for target ----
	// Count is the post-merge total: existing target count + newly merged engrams.
	existingCount := ps.GetVaultCount(ctx, wsTarget)
	newTotal := existingCount + mergedEngrams
	vaultCountKey := keys.VaultCountKey(wsTarget)
	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, uint64(newTotal))
	if err := ps.db.Set(vaultCountKey, buf, pebble.NoSync); err != nil {
		return mergedEngrams, fmt.Errorf("merge: write vault count: %w", err)
	}
	// Update in-memory counter.
	vc := ps.getOrInitCounter(ctx, wsTarget)
	vc.count.Store(newTotal)

	return mergedEngrams, nil
}
