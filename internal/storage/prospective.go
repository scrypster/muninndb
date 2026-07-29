package storage

import (
	"context"
	"fmt"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/vmihailenco/msgpack/v5"

	"github.com/scrypster/muninndb/internal/storage/keys"
)

// Armed-intention index (0x2D, THE PUSH increment 1).
//
// An intention is a normal TypeGoal engram plus one 0x2D key per cue entity:
//
//	0x2D | ws(8) | EntityNameHash(cue)(8) | intentionID(16) = 33 bytes
//
// The index answers exactly one question — "which intentions are armed on
// this focal entity?" — as a single 17-byte-prefix scan. It is consulted ONLY
// inside tool handlers at the moment the agent is already processing the cue
// entity (spontaneous retrieval); nothing polls it (the #609 lesson).
//
// The value carries the full cue list so a one-shot fire can delete every
// sibling cue key, and so entity-merge can rewrite stale cue names.

// ArmedIntention is one armed intention as returned by ScanArmedForEntity.
type ArmedIntention struct {
	ID          ULID
	OneShot     bool
	CreatedAt   int64 // unix nanos
	FiredCount  uint32
	LastFiredAt int64 // unix nanos, 0 = never fired
	Cues        []string
}

// prospectiveIntentRecord is the msgpack value stored at each 0x2D key. The
// same value is duplicated across an intention's cue keys; MarkIntentionFired
// rewrites all of them in one batch so they cannot drift.
type prospectiveIntentRecord struct {
	OneShot     bool     `msgpack:"o"`
	CreatedAt   int64    `msgpack:"c"`
	FiredCount  uint32   `msgpack:"f"`
	LastFiredAt int64    `msgpack:"l"`
	Cues        []string `msgpack:"q"`
}

// ArmIntention writes one 0x2D armed-intention key per cue entity for the
// given intention engram. All keys are committed atomically in one batch.
// Cue identity is keys.NormalizeEntityName (same as every entity index).
func (ps *PebbleStore) ArmIntention(ctx context.Context, ws [8]byte, intentionID ULID, cues []string, oneShot bool) error {
	if len(cues) == 0 {
		return fmt.Errorf("arm intention: at least one cue is required")
	}
	rec := prospectiveIntentRecord{
		OneShot:   oneShot,
		CreatedAt: time.Now().UnixNano(),
		Cues:      cues,
	}
	val, err := msgpack.Marshal(rec)
	if err != nil {
		return fmt.Errorf("arm intention: marshal: %w", err)
	}
	batch := ps.db.NewBatch()
	defer batch.Close()
	for _, cue := range cues {
		k := keys.ProspectiveIntentKey(ws, keys.EntityNameHash(cue), [16]byte(intentionID))
		if err := batch.Set(k, val, nil); err != nil {
			return fmt.Errorf("arm intention: set cue %q: %w", cue, err)
		}
	}
	if err := batch.Commit(pebble.NoSync); err != nil {
		return fmt.Errorf("arm intention: commit: %w", err)
	}
	ps.replicateBatch(batch)
	return nil
}

// ScanArmedForEntity returns every intention armed on the given cue entity in
// vault ws. A missing/empty index returns (nil, nil).
func (ps *PebbleStore) ScanArmedForEntity(ctx context.Context, ws [8]byte, entityName string) ([]ArmedIntention, error) {
	iter, err := PrefixIterator(ps.db, keys.ProspectiveIntentPrefix(ws, keys.EntityNameHash(entityName)))
	if err != nil {
		return nil, fmt.Errorf("scan armed intentions: iter: %w", err)
	}
	defer iter.Close()

	var out []ArmedIntention
	for valid := iter.First(); valid; valid = iter.Next() {
		k := iter.Key()
		if len(k) != 33 { // 1 + 8 + 8 + 16
			continue
		}
		var rec prospectiveIntentRecord
		if err := msgpack.Unmarshal(iter.Value(), &rec); err != nil {
			continue // corrupt value: skip loudly-visible-in-logs is overkill for a scan; the engram gate re-verifies
		}
		var id [16]byte
		copy(id[:], k[17:33])
		out = append(out, ArmedIntention{
			ID:          ULID(id),
			OneShot:     rec.OneShot,
			CreatedAt:   rec.CreatedAt,
			FiredCount:  rec.FiredCount,
			LastFiredAt: rec.LastFiredAt,
			Cues:        rec.Cues,
		})
	}
	if err := iter.Error(); err != nil {
		return nil, fmt.Errorf("scan armed intentions: %w", err)
	}
	return out, nil
}

// IsIntentionArmedOnCue reports whether intentionID is itself armed on the
// given cue entity — i.e. whether the engram being scored is the very
// intention that would fire on this cue. Used to exclude an armed
// intention's own retrieved engram from contributing to its own focality
// (bug #693: without this, an intention's own TypeGoal engram — which
// carries its cue as a first-class entity — could self-satisfy the
// top-result or >=2-carrier focality rules in NoticesForRecall). A single
// point lookup on the reconstructed 0x2D key; no scan.
func (ps *PebbleStore) IsIntentionArmedOnCue(ctx context.Context, ws [8]byte, cueName string, intentionID ULID) (bool, error) {
	k := keys.ProspectiveIntentKey(ws, keys.EntityNameHash(cueName), [16]byte(intentionID))
	val, err := Get(ps.db, k)
	if err != nil {
		return false, fmt.Errorf("is intention armed on cue: %w", err)
	}
	return val != nil, nil
}

// MarkIntentionFired records a delivery of the intention. For a one-shot
// intention every cue key is deleted (the intention is consumed). For a
// recurring intention every cue key's record gets FiredCount+1 and a fresh
// LastFiredAt, in one atomic batch.
//
// cues must be the intention's full cue list (as returned in
// ArmedIntention.Cues) so every sibling key is covered.
//
// Concurrency: two concurrent fires of the same recurring intention can lose
// one count increment (last-writer-wins read-modify-write); two concurrent
// fires of a one-shot can both deliver once before the delete lands. Both are
// benign for an advisory notice channel — session dedup and the 2-notice cap
// bound the blast radius — and match the advisory-strength precedent (#576).
func (ps *PebbleStore) MarkIntentionFired(ctx context.Context, ws [8]byte, intentionID ULID, cues []string, oneShot bool) error {
	batch := ps.db.NewBatch()
	defer batch.Close()

	if oneShot {
		for _, cue := range cues {
			k := keys.ProspectiveIntentKey(ws, keys.EntityNameHash(cue), [16]byte(intentionID))
			if err := batch.Delete(k, nil); err != nil {
				return fmt.Errorf("mark intention fired: delete cue %q: %w", cue, err)
			}
		}
	} else {
		now := time.Now().UnixNano()
		for _, cue := range cues {
			k := keys.ProspectiveIntentKey(ws, keys.EntityNameHash(cue), [16]byte(intentionID))
			raw, err := Get(ps.db, k)
			if err != nil {
				return fmt.Errorf("mark intention fired: read cue %q: %w", cue, err)
			}
			if raw == nil {
				continue // key already gone (e.g. concurrent disarm) — nothing to bump
			}
			var rec prospectiveIntentRecord
			if err := msgpack.Unmarshal(raw, &rec); err != nil {
				continue
			}
			rec.FiredCount++
			rec.LastFiredAt = now
			val, err := msgpack.Marshal(rec)
			if err != nil {
				return fmt.Errorf("mark intention fired: marshal: %w", err)
			}
			if err := batch.Set(k, val, nil); err != nil {
				return fmt.Errorf("mark intention fired: set cue %q: %w", cue, err)
			}
		}
	}
	if err := batch.Commit(pebble.NoSync); err != nil {
		return fmt.Errorf("mark intention fired: commit: %w", err)
	}
	ps.replicateBatch(batch)
	return nil
}

// RelinkProspectiveIntent rewrites the 0x2D armed-intention index after an
// entity merge (oldName → newName), mirroring the 0x26 relink obligation in
// RelinkRelationshipEntity. Two things must move:
//
//  1. Keys under Hash(oldName) are deleted and re-written under Hash(newName).
//  2. EVERY record whose cue list names oldName — including sibling cue keys
//     of the same intention — has that list entry rewritten to newName, or a
//     later MarkIntentionFired would derive the old hash and leak/miss keys.
//
// Intentions are few, so this walks the vault's whole 0x2D range (a 9-byte
// prefix scan over intention keys only) rather than maintaining a reverse map.
func (ps *PebbleStore) RelinkProspectiveIntent(ctx context.Context, ws [8]byte, oldName, newName string) error {
	oldHash := keys.EntityNameHash(oldName)
	newHash := keys.EntityNameHash(newName)
	if oldHash == newHash {
		return nil // same canonical entity — nothing to do
	}
	oldNorm := keys.NormalizeEntityName(oldName)

	iter, err := PrefixIterator(ps.db, keys.ProspectiveIntentWorkspacePrefix(ws))
	if err != nil {
		return fmt.Errorf("relink prospective intent: iter: %w", err)
	}

	type update struct {
		oldKey []byte
		newKey []byte // nil = value-only rewrite in place
		newVal []byte
	}
	var updates []update

	for valid := iter.First(); valid; valid = iter.Next() {
		k := iter.Key()
		if len(k) != 33 {
			continue
		}
		var rec prospectiveIntentRecord
		if err := msgpack.Unmarshal(iter.Value(), &rec); err != nil {
			continue
		}
		cueChanged := false
		for i, c := range rec.Cues {
			if keys.NormalizeEntityName(c) == oldNorm {
				rec.Cues[i] = newName
				cueChanged = true
			}
		}
		var keyHash [8]byte
		copy(keyHash[:], k[9:17])
		keyMoves := keyHash == oldHash
		if !cueChanged && !keyMoves {
			continue
		}
		val, err := msgpack.Marshal(rec)
		if err != nil {
			iter.Close()
			return fmt.Errorf("relink prospective intent: marshal: %w", err)
		}
		u := update{oldKey: append([]byte(nil), k...), newVal: val}
		if keyMoves {
			var id [16]byte
			copy(id[:], k[17:33])
			u.newKey = keys.ProspectiveIntentKey(ws, newHash, id)
		}
		updates = append(updates, u)
	}
	if err := iter.Close(); err != nil {
		return fmt.Errorf("relink prospective intent: iter close: %w", err)
	}
	if len(updates) == 0 {
		return nil
	}

	batch := ps.db.NewBatch()
	defer batch.Close()
	for _, u := range updates {
		if u.newKey != nil {
			if err := batch.Delete(u.oldKey, nil); err != nil {
				return fmt.Errorf("relink prospective intent: delete: %w", err)
			}
			if err := batch.Set(u.newKey, u.newVal, nil); err != nil {
				return fmt.Errorf("relink prospective intent: set moved: %w", err)
			}
		} else {
			if err := batch.Set(u.oldKey, u.newVal, nil); err != nil {
				return fmt.Errorf("relink prospective intent: set in place: %w", err)
			}
		}
	}
	if err := batch.Commit(pebble.NoSync); err != nil {
		return fmt.Errorf("relink prospective intent: commit: %w", err)
	}
	ps.replicateBatch(batch)
	return nil
}
