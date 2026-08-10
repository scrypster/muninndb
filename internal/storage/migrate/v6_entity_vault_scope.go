package migrate

import (
	"fmt"
	"log/slog"

	"github.com/cockroachdb/pebble"
	"github.com/vmihailenco/msgpack/v5"

	"github.com/scrypster/muninndb/internal/prefix"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

// v6 (#683) — re-key entity records from the global `0x1F|nameHash(8)` to the
// vault-scoped `0x1F|ws(8)|nameHash(8)`.
//
// The defect: 0x1F carried no workspace prefix while the links that back it
// (0x20 forward, 0x23 reverse, 0x26 relationship index) all did. Two vaults
// that mentioned the same entity name therefore shared ONE record. Two
// consequences, both live and verified:
//
//   - a wrong count. mention_count was the sum of every vault's upserts, so a
//     vault reported other tenants' mentions as its own — and entity IDF, which
//     divides that count by the LOCAL vault size, was a cross-vault numerator
//     over a single-vault denominator.
//   - a tenancy leak. A lookup by name from a vault with no links to the entity
//     still returned the other tenant's record — name, type, confidence,
//     first_seen, mention_count — with an empty engram list. An existence and
//     metadata oracle over another tenant's entity vocabulary. The lifecycle
//     write (`SetEntityState`) had the same shape in the other direction.
//
// WHAT HAPPENS TO mention_count
//
// The old number is a sum across every vault that ever mentioned the name, and
// splitting it has to put it somewhere. This migration RECOMPUTES it per vault
// from the 0x23 reverse index: the count becomes the number of distinct engrams
// in THAT vault that mention the entity.
//
// Why that and not the alternatives:
//
//   - Giving the whole total to one vault is silently wrong for every other
//     vault and wrong for that one too.
//   - Resetting to zero is honest but throws away information that is sitting
//     right there in an index we already have to scan.
//   - Recomputing is accurate, checkable against what the vault can actually
//     show you, and cheap: one pass over 0x23, no engram reads — O(entity
//     links), not O(engrams).
//
// It also matches how the live code already arbitrates: DecrementEntityMention
// Count treats the 0x23 reverse index — not the stored count — as the ground
// truth when deciding whether an entity is orphaned.
//
// WHAT AN OPERATOR LOSES: the pre-migration figure was an UPSERT counter, not a
// link count. Re-enrichment, replay, apply-enrichment and merge events each
// added one for the same engram, and a crash could leave it stale-high. Counts
// will therefore generally go DOWN, and the per-vault totals will usually sum
// to less than the old global number. mention_count changes meaning from "times
// this record was written" to "memories in this vault that mention it". That is
// the meaning every consumer already assumed (entity IDF, the prospective-cue
// ubiquity floor, the ListEntities ordering), so the recomputation makes those
// correct rather than changing them.
//
// # WHICH VAULTS A RECORD GOES TO
//
// The union of four positive references, in this order. The first three are
// genuinely per-vault; the fourth is NOT — read its caveat below.
//
//  1. 0x23 entity→engram reverse index — the mentions. Supplies the count.
//
//  2. 0x26 relationship entity index — curated relationships with no 0x20 link.
//
//  3. 0x2D prospective-intent cues — an armed intention naming the entity.
//
//  4. for a State=="merged" tombstone, the FULL vault set of its MergedInto
//     target (computed from 1–3), not filtered to any one vault. MergeEntity
//     relinks every engram and relationship away from A BEFORE marking A
//     merged, so every merge tombstone in the store has zero references of its
//     own; following the target is what keeps those tombstones instead of
//     dropping them all as "orphans".
//
//     CAVEAT: nothing in the keyspace records WHICH vault ran the merge, so
//     when the target has references from more than one vault, clause 4 gives
//     the tombstone to ALL of them — including a vault that has never
//     mentioned or referenced the merged-away name. That vault gets a full
//     `state="merged", merged_into=<target>` record purely because some other
//     vault performed the merge: the same read-oracle shape #683 otherwise
//     closes, reopened for this one record shape. Pre-migration, that vault
//     saw the identical record through the old global key, so this PRESERVES
//     an existing exposure rather than creating a new one, and there is no
//     information anywhere in the store to attribute the tombstone correctly —
//     dropping it for a vault in the fan-out whenever the target has more than
//     one vault would delete a redirect some OTHER vault in that same set
//     genuinely needs. Bounded (unreachable via ListEntities, which walks 0x20
//     and gets no forward link for the uninvolved vault; reachable only by an
//     exact-name lookup) but durable: UpsertEntityRecord preserves
//     State/MergedInto once State=="merged", so if the uninvolved vault later
//     legitimately acquires that name its record stays pinned as a stranger's
//     tombstone, and a subsequent MergeEntity on it fails with "already
//     merged" until manually cleared. See STO-17's residual paragraph. Pinned
//     (as intentional, not a regression trap) by
//     TestV6_TombstoneFansOutIntoUninvolvedVault.
//
// A record with no reference under any of the four is unreachable from every
// vault-scoped view today, and the live code already deletes exactly that shape
// (DecrementEntityMentionCount, on reaching 0 with an empty reverse index). It
// is dropped, logged by name at WARN so the operator has the list.
//
// CRASH SAFETY. Every destination write is Sync-committed BEFORE any legacy key
// is deleted, so no crash window leaves neither copy. Idempotent on re-run: a
// destination that already exists is left alone (a re-run must not clobber
// post-migration writes with stale legacy data) while the legacy key is still
// cleared, and a re-run after a completed pass simply finds no legacy keys.
//
// DELETION. Key-by-key behind IsLegacyEntityRecord, never a DeleteRange: the
// legacy and relocated keys share a prefix byte, so a range delete over 0x1F
// would destroy the records the migration just wrote (#726's lesson).
//
// CONCURRENCY. Runner.Run executes inside Open, before any transport is
// serving and before the engine's workers start, so no writer races this.
//
// DOWNGRADE. A vault migrated to v6 read by a binary that predates it would
// find no 0x1F record at any name and silently report every entity as unknown —
// resurrecting the empty-aggregate shape this fixes. Runner.Run's refuse-newer
// guard blocks it structurally (stored 6 > that binary's MaxRegisteredVersion
// 5 → refuse to start).
func VaultScopeEntityRecords(db *pebble.DB) error {
	refs, err := collectEntityVaultRefs(db)
	if err != nil {
		return err
	}
	legacy, err := collectLegacyEntityRecords(db)
	if err != nil {
		return err
	}
	if len(legacy) == 0 {
		return nil
	}
	resolveMergedTombstones(legacy, refs)
	if err := writeVaultScopedEntityRecords(db, legacy, refs); err != nil {
		return err
	}
	return deleteLegacyEntityRecords(db)
}

// legacyEntityRecord mirrors storage.EntityRecord's msgpack shape. It is a
// deliberate frozen copy: a migration decodes the format that is on disk, not
// whatever the live struct becomes later.
type legacyEntityRecord struct {
	Name         string  `msgpack:"name"`
	Type         string  `msgpack:"type"`
	Confidence   float32 `msgpack:"confidence"`
	Source       string  `msgpack:"source"`
	UpdatedAt    int64   `msgpack:"updated_at"`
	FirstSeen    int64   `msgpack:"first_seen"`
	MentionCount int32   `msgpack:"mention_count"`
	State        string  `msgpack:"state"`
	MergedInto   string  `msgpack:"merged_into"`
}

// entityVaultRefs holds, per entity name hash, the vaults that reference it and
// how many distinct engrams in each vault mention it. A vault present with
// count 0 is referenced by a relationship or an armed intention but by no
// engram link — a real reference with no mentions.
type entityVaultRefs map[[8]byte]map[[8]byte]int32

func (r entityVaultRefs) add(hash [8]byte, ws [8]byte, mentions int32) {
	byVault := r[hash]
	if byVault == nil {
		byVault = make(map[[8]byte]int32, 1)
		r[hash] = byVault
	}
	byVault[ws] += mentions
}

// IsLegacyEntityRecord positively identifies a pre-#683 entity record at
// `0x1F|nameHash(8)`.
//
// It is a POSITIVE test on a destructive path, and it is conjunctive:
//
//   - the key must be exactly the 9-byte legacy shape (a relocated record is 17
//     bytes, so this can never match the migration's own output);
//   - the value MUST msgpack-decode as an entity record with a non-empty name;
//   - that name MUST hash to the very address the record sits at.
//
// The last clause is the strong one: an unrelated 9-byte value under 0x1F would
// have to decode as an entity record AND carry the one name whose SipHash
// equals its own key suffix. Exported so the storage package can pin it against
// real UpsertEntityRecord output.
func IsLegacyEntityRecord(key, val []byte) bool {
	if len(key) != 9 || key[0] != prefix.Entity {
		return false
	}
	var rec legacyEntityRecord
	if err := msgpack.Unmarshal(val, &rec); err != nil {
		return false
	}
	if rec.Name == "" {
		return false
	}
	var addr [8]byte
	copy(addr[:], key[1:9])
	return keys.EntityNameHash(rec.Name) == addr
}

// collectEntityVaultRefs walks the three per-vault indexes that can reference an
// entity and builds the (nameHash → vault → mention count) map.
func collectEntityVaultRefs(db *pebble.DB) (entityVaultRefs, error) {
	refs := make(entityVaultRefs)

	// 0x23 | nameHash(8) | ws(8) | engramID(16) — one key per (entity, engram),
	// so counting keys counts distinct mentioning engrams.
	if err := scanFixedWidth(db, prefix.EntityReverseIndex, 33, func(k []byte) {
		var hash, ws [8]byte
		copy(hash[:], k[1:9])
		copy(ws[:], k[9:17])
		refs.add(hash, ws, 1)
	}); err != nil {
		return nil, fmt.Errorf("vault-scope entity records: scan 0x23: %w", err)
	}

	// 0x26 | ws(8) | entityHash(8) | engramID(16) — a relationship reference is
	// not a mention, so it registers the vault with no count.
	if err := scanFixedWidth(db, prefix.RelEntityIndex, 33, func(k []byte) {
		var ws, hash [8]byte
		copy(ws[:], k[1:9])
		copy(hash[:], k[9:17])
		refs.add(hash, ws, 0)
	}); err != nil {
		return nil, fmt.Errorf("vault-scope entity records: scan 0x26: %w", err)
	}

	// 0x2D | ws(8) | cueHash(8) | intentionID(16) — likewise a reference, not a
	// mention.
	if err := scanFixedWidth(db, prefix.ProspectiveIntent, 33, func(k []byte) {
		var ws, hash [8]byte
		copy(ws[:], k[1:9])
		copy(hash[:], k[9:17])
		refs.add(hash, ws, 0)
	}); err != nil {
		return nil, fmt.Errorf("vault-scope entity records: scan 0x2D: %w", err)
	}

	return refs, nil
}

// scanFixedWidth iterates one prefix and calls fn for every key of exactly
// wantLen bytes. Keys of any other width are skipped rather than guessed at.
func scanFixedWidth(db *pebble.DB, pfx byte, wantLen int, fn func(key []byte)) error {
	lower := []byte{pfx}
	iter, err := db.NewIter(&pebble.IterOptions{
		LowerBound: lower,
		UpperBound: keys.PrefixUpperBound(lower),
	})
	if err != nil {
		return err
	}
	defer iter.Close()
	for valid := iter.First(); valid; valid = iter.Next() {
		if len(iter.Key()) == wantLen {
			fn(iter.Key())
		}
	}
	return iter.Error()
}

// collectLegacyEntityRecords reads every positively-identified legacy record,
// keyed by its name hash.
func collectLegacyEntityRecords(db *pebble.DB) (map[[8]byte]legacyEntityRecord, error) {
	out := make(map[[8]byte]legacyEntityRecord)
	lower := []byte{prefix.Entity}
	iter, err := db.NewIter(&pebble.IterOptions{
		LowerBound: lower,
		UpperBound: keys.PrefixUpperBound(lower),
	})
	if err != nil {
		return nil, fmt.Errorf("vault-scope entity records: new iter: %w", err)
	}
	defer iter.Close()

	for valid := iter.First(); valid; valid = iter.Next() {
		val, err := iter.ValueAndErr()
		if err != nil {
			return nil, fmt.Errorf("vault-scope entity records: iter value: %w", err)
		}
		if !IsLegacyEntityRecord(iter.Key(), val) {
			continue
		}
		var rec legacyEntityRecord
		if err := msgpack.Unmarshal(val, &rec); err != nil {
			// Unreachable: IsLegacyEntityRecord already decoded it.
			return nil, fmt.Errorf("vault-scope entity records: decode %x: %w", iter.Key(), err)
		}
		var hash [8]byte
		copy(hash[:], iter.Key()[1:9])
		out[hash] = rec
	}
	if err := iter.Error(); err != nil {
		return nil, fmt.Errorf("vault-scope entity records: iter: %w", err)
	}
	return out, nil
}

// resolveMergedTombstones gives every merged-away record the vault set of the
// entity it was merged into. MergeEntity relinks all of A's engrams and
// relationships to B before stamping A "merged", so A has no references of its
// own and would otherwise look like garbage. Chains (A→B→C) resolve by walking
// to a target that has references, bounded to avoid a cycle.
func resolveMergedTombstones(legacy map[[8]byte]legacyEntityRecord, refs entityVaultRefs) {
	const maxChain = 16
	for hash, rec := range legacy {
		if rec.State != "merged" || rec.MergedInto == "" || len(refs[hash]) > 0 {
			continue
		}
		seen := map[[8]byte]struct{}{hash: {}}
		target := keys.EntityNameHash(rec.MergedInto)
		for hop := 0; hop < maxChain; hop++ {
			if _, loop := seen[target]; loop {
				break
			}
			seen[target] = struct{}{}
			if vaults := refs[target]; len(vaults) > 0 {
				// The tombstone belongs wherever the merge happened, with no
				// mentions of its own.
				for ws := range vaults {
					refs.add(hash, ws, 0)
				}
				break
			}
			next, ok := legacy[target]
			if !ok || next.State != "merged" || next.MergedInto == "" {
				break
			}
			target = keys.EntityNameHash(next.MergedInto)
		}
	}
}

// writeVaultScopedEntityRecords writes one relocated record per referencing
// vault, with mention_count recomputed from that vault's reverse-index links.
// All writes are Sync-committed before the caller deletes anything.
func writeVaultScopedEntityRecords(db *pebble.DB, legacy map[[8]byte]legacyEntityRecord, refs entityVaultRefs) error {
	const batchSize = 500
	batch := db.NewBatch()
	defer func() { batch.Close() }()

	written, skipped, dropped, pending := 0, 0, 0, 0
	for hash, rec := range legacy {
		vaults := refs[hash]
		if len(vaults) == 0 {
			slog.Warn("vault-scope entity records: dropping unreferenced entity record",
				"entity", rec.Name, "state", rec.State, "legacy_mention_count", rec.MentionCount)
			dropped++
			continue
		}
		for ws, mentions := range vaults {
			key := keys.EntityKey(ws, hash)
			if _, closer, err := db.Get(key); err == nil {
				// A re-run after a partial crash: newer state already lives
				// here and must not be clobbered by the stale legacy copy.
				closer.Close()
				skipped++
				continue
			} else if err != pebble.ErrNotFound {
				return fmt.Errorf("vault-scope entity records: read %x: %w", key, err)
			}

			scoped := rec
			scoped.MentionCount = mentions
			val, err := msgpack.Marshal(scoped)
			if err != nil {
				return fmt.Errorf("vault-scope entity records: marshal %q: %w", rec.Name, err)
			}
			if err := batch.Set(key, val, nil); err != nil {
				return fmt.Errorf("vault-scope entity records: set %x: %w", key, err)
			}
			written++
			pending++
			if pending >= batchSize {
				if err := batch.Commit(pebble.Sync); err != nil {
					return fmt.Errorf("vault-scope entity records: commit: %w", err)
				}
				batch.Close()
				batch = db.NewBatch()
				pending = 0
			}
		}
	}
	if pending > 0 {
		if err := batch.Commit(pebble.Sync); err != nil {
			return fmt.Errorf("vault-scope entity records: commit: %w", err)
		}
	}
	slog.Info("vault-scope entity records: relocated",
		"records", len(legacy), "written", written, "already_present", skipped, "dropped_unreferenced", dropped)
	return nil
}

// deleteLegacyEntityRecords removes every positively-identified legacy record.
// Runs only after every destination write is durable.
func deleteLegacyEntityRecords(db *pebble.DB) error {
	lower := []byte{prefix.Entity}
	iter, err := db.NewIter(&pebble.IterOptions{
		LowerBound: lower,
		UpperBound: keys.PrefixUpperBound(lower),
	})
	if err != nil {
		return fmt.Errorf("vault-scope entity records: delete iter: %w", err)
	}
	defer iter.Close()

	const batchSize = 1000
	batch := db.NewBatch()
	defer func() { batch.Close() }()

	deleted, kept, pending := 0, 0, 0
	for valid := iter.First(); valid; valid = iter.Next() {
		val, err := iter.ValueAndErr()
		if err != nil {
			return fmt.Errorf("vault-scope entity records: delete iter value: %w", err)
		}
		if !IsLegacyEntityRecord(iter.Key(), val) {
			kept++
			continue
		}
		if err := batch.Delete(append([]byte(nil), iter.Key()...), nil); err != nil {
			return fmt.Errorf("vault-scope entity records: delete: %w", err)
		}
		deleted++
		pending++
		if pending >= batchSize {
			if err := batch.Commit(pebble.Sync); err != nil {
				return fmt.Errorf("vault-scope entity records: commit deletes: %w", err)
			}
			batch.Close()
			batch = db.NewBatch()
			pending = 0
		}
	}
	if err := iter.Error(); err != nil {
		return fmt.Errorf("vault-scope entity records: delete iter: %w", err)
	}
	if pending > 0 {
		if err := batch.Commit(pebble.Sync); err != nil {
			return fmt.Errorf("vault-scope entity records: commit deletes: %w", err)
		}
	}
	slog.Info("vault-scope entity records: legacy keys removed", "deleted", deleted, "left_alone", kept)
	return nil
}
