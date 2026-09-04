package migrate

import (
	"fmt"
	"log/slog"

	"github.com/cockroachdb/pebble"
	"github.com/vmihailenco/msgpack/v5"

	"github.com/scrypster/muninndb/internal/prefix"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

// v7 (#894) — re-key relationship records from the 1-byte predicate
// discriminant to the 8-byte keys.PredicateHash.
//
// The defect: the 0x21 key carried the predicate as ONE byte produced by an
// 11-entry hardcoded vocabulary map (relTypeBytes); every other predicate
// string — including four of the ten the enrich plugin's own default prompt
// suggests — folded to 0xFF. Two unmapped predicates asserted from one engram
// about the same entity pair therefore produced the SAME key, and Pebble's
// last-write-wins silently destroyed the loser. No reader can recover bytes
// that were never written.
//
// The fix replaces the discriminant with PredicateHash(relType) (49-byte key),
// so every distinct predicate string gets its own key. This migration re-keys
// existing records to that layout. The key byte was write-only — no code ever
// decoded it back to a string (every reader consumes the value's RelType) —
// which is what makes the layout change safe.
//
// DISCRIMINATOR. Key length alone: 42 bytes = legacy (rekey), 49 bytes =
// already migrated (skip), anything else = skipped with the count logged.
// 0x21 has exactly one record family, so length is an exact discriminator —
// v3 needed a value-length gate only because two families shared a prefix.
//
// FAIL-LOUD. A value that cannot be decoded as a RelationshipRecord returns
// an error and leaves the version unstamped (v3's "never silently orphan"
// rule): a corrupted relationship must block the migration loudly, not be
// dropped.
//
// CRASH SAFETY. The Set(newKey, val) and Delete(oldKey) ride ONE batch per
// record, committed with pebble.Sync every 500 records — Pebble batch
// atomicity means every record lives at exactly one layout at any instant. A
// torn/crashed migration leaves a 42/49 mixture and an unstamped version; the
// next open re-runs v7 over the remainder and converges. Duplicates are
// impossible because the source key is deleted in the same batch the
// destination is written. A completed pass finds no 42-byte keys and no-ops
// (idempotent, including via --force-migration-rerun, where v2 re-runs first
// and skips every 49-byte key by its own length guard).
//
// HISTORICAL LOSS. Records already destroyed by a pre-fix collision are
// unrecoverable — only the survivor's value exists, and this migration re-keys
// survivors. Identical to STO-15's accepted residual.
//
// CONCURRENCY. Runner.Run executes inside Open, before any transport is
// serving and before the engine's workers start, so no writer races this.
//
// DOWNGRADE / MIXED VERSIONS. Both directions are hazardous.
//
//   - Old binary, upgraded DB: refuse-newer blocks it structurally (stored 7 >
//     that binary's MaxRegisteredVersion 6 → refuse to start).
//   - Upgraded leader → pre-upgrade follower: the follower applies 49-byte log
//     entries byte-transparently but its 42-length-guarded consumers
//     (deleteEntityLinks, RelinkRelationshipEntity) silently skip them, so
//     relationship cleanup on hard-delete/entity-merge is missed until the
//     follower is upgraded.
//   - PRE-UPGRADE LEADER → upgraded, v7-stamped follower (the direction
//     refuse-newer cannot see): the replication applier commits replicated
//     batch reprs byte-verbatim (applier.go SetRepr, no key-length or version
//     gate), so the old leader's legacy 42-byte relationship writes land AFTER
//     v7 stamped, and a plain reopen never re-runs v7 — permanent strays. A
//     re-upsert of the same assertion yields two rows (42+49 keys), a merge
//     leaves the stale-name row behind (RelinkRelationshipEntity skips it),
//     and a hard-delete orphans the stray's two 0x26 entries.
//
// OPERATOR RULE: upgrade the leader first during rolling upgrades, then
// followers promptly. If a replica-first upgrade already happened, run
// `muninn start --force-migration-rerun` on the upgraded nodes and restart —
// v7's length discriminator re-keys the replicated strays on the forced pass
// (pinned by TestMigrationV7_ForceRerunRekeysReplicatedStrays).
func RekeyRelationshipPredicateHash(db *pebble.DB) error {
	iter, err := db.NewIter(&pebble.IterOptions{
		LowerBound: []byte{prefix.Relationship},
		UpperBound: []byte{prefix.Relationship + 1},
	})
	if err != nil {
		return fmt.Errorf("reltype predicate hash: new iter: %w", err)
	}
	defer iter.Close()

	const batchSize = 500
	const legacyKeyLen = 42 // 0x21(1)|ws(8)|engramID(16)|fromHash(8)|relTypeByte(1)|toHash(8)

	batch := db.NewBatch()
	batchCount := 0
	// migrated/alreadyMigrated/skipped are ADVISORY log counts, nothing more:
	// the correctness mechanism is the per-key length discrimination above,
	// never these counters. An iterator that revisits a just-committed
	// destination key (which Pebble may surface after an interim batch commit)
	// can double-count alreadyMigrated; a count that drifts by a few is
	// expected and harmless, and no behavior may ever gate on these numbers.
	migrated, alreadyMigrated, skipped := 0, 0, 0

	for valid := iter.First(); valid; valid = iter.Next() {
		k := iter.Key()
		switch {
		case len(k) == keys.RelationshipKeyLen:
			// 49 bytes — written by the post-fix layout (or a prior partial
			// pass of this migration). Nothing to do.
			alreadyMigrated++
			continue
		case len(k) != legacyKeyLen:
			skipped++
			continue
		}

		// Decode the value with the frozen copy; the VALUE's RelType string —
		// never the legacy key byte — is authoritative for the destination
		// predicate hash. That is the whole point: the byte was lossy.
		val, err := iter.ValueAndErr()
		if err != nil {
			batch.Close()
			return fmt.Errorf("reltype predicate hash: iter value: %w", err)
		}
		var rec v7RelationshipRecord
		if err := msgpack.Unmarshal(val, &rec); err != nil {
			batch.Close()
			return fmt.Errorf("reltype predicate hash: undecodable relationship value at key %x "+
				"(refusing to migrate past corrupted data; version stays unstamped): %w", k, err)
		}

		var ws [8]byte
		var engramID [16]byte
		var fromHash, toHash [8]byte
		copy(ws[:], k[1:9])
		copy(engramID[:], k[9:25])
		copy(fromHash[:], k[25:33])
		copy(toHash[:], k[34:42]) // legacy offsets: relTypeByte occupied [33:34]

		newKey := keys.RelationshipKey(ws, engramID, fromHash, keys.PredicateHash(rec.RelType), toHash)

		// Value bytes verbatim — no re-encode, no read-modify-write.
		if err := batch.Set(newKey, val, nil); err != nil {
			batch.Close()
			return fmt.Errorf("reltype predicate hash: set 49-byte key: %w", err)
		}
		oldKey := make([]byte, legacyKeyLen)
		copy(oldKey, k)
		if err := batch.Delete(oldKey, nil); err != nil {
			batch.Close()
			return fmt.Errorf("reltype predicate hash: delete legacy key: %w", err)
		}

		batchCount++
		migrated++
		if batchCount >= batchSize {
			if err := batch.Commit(pebble.Sync); err != nil {
				batch.Close()
				return fmt.Errorf("reltype predicate hash: commit batch: %w", err)
			}
			batch.Close()
			batch = db.NewBatch()
			batchCount = 0
		}
	}
	if err := iter.Error(); err != nil {
		batch.Close()
		return fmt.Errorf("reltype predicate hash: iter: %w", err)
	}

	if batchCount > 0 {
		if err := batch.Commit(pebble.Sync); err != nil {
			batch.Close()
			return fmt.Errorf("reltype predicate hash: commit final batch: %w", err)
		}
	}
	batch.Close()

	slog.Info("reltype predicate hash migration complete",
		"migrated", migrated,
		"already_migrated", alreadyMigrated,
		"skipped", skipped,
	)
	return nil
}

// v7RelationshipRecord mirrors storage.RelationshipRecord's msgpack shape. It
// is a deliberate frozen copy (v6's legacyEntityRecord precedent): a migration
// decodes the format that is on disk, not whatever the live struct becomes
// later. Pinned against the live encoder by
// TestV7FrozenRecordMatchesLiveEncoder.
type v7RelationshipRecord struct {
	FromEntity string  `msgpack:"from_entity"`
	ToEntity   string  `msgpack:"to_entity"`
	RelType    string  `msgpack:"rel_type"`
	Weight     float32 `msgpack:"weight"`
	Source     string  `msgpack:"source"`
	UpdatedAt  int64   `msgpack:"updated_at"`
}

// legacyRelationshipKey builds the pre-#894 42-byte 0x21 key: 0x21 | ws(8) |
// engramID(16) | fromHash(8) | relTypeByte(1) | toHash(8). Used by the v7
// tests to plant historical input; the layout is pinned by literal bytes in
// TestV7LegacyLayoutPinned (it cannot be built from the live constructor,
// which now emits the 49-byte shape).
func legacyRelationshipKey(ws [8]byte, engramID [16]byte, fromHash [8]byte, relTypeByte byte, toHash [8]byte) []byte {
	k := make([]byte, 42)
	k[0] = prefix.Relationship
	copy(k[1:9], ws[:])
	copy(k[9:25], engramID[:])
	copy(k[25:33], fromHash[:])
	k[33] = relTypeByte
	copy(k[34:42], toHash[:])
	return k
}
