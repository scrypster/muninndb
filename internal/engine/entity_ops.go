package engine

import (
	"context"
	"errors"
	"log/slog"
	"sort"

	"github.com/scrypster/muninndb/internal/storage"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

const (
	defaultEntityEngramLimit   = 20
	defaultListEntitiesLimit   = 50
	entityCoOccurrenceMinCount = 2
	entityCoOccurrenceTopN     = 20

	// liveSupportScanCap bounds how many LIVE engrams GetEntityAggregate reads
	// to decide which derived entity edges still have support (#780). It is a
	// work bound, not a calibration: nothing about a vault's data is inferred
	// from it. Past the cap the derivation is abandoned rather than applied to a
	// partial set — see filterStale.
	liveSupportScanCap = 500
)

// errEntityScanCapped stops the ScanEntityEngrams callback without being
// mistaken for a real failure. The previous sentinel was a fresh
// fmt.Errorf("limit reached") compared by MESSAGE, which a genuine store error
// carrying that text would have silently satisfied.
var errEntityScanCapped = errors.New("entity scan cap reached")

// EntityCoOccEntry is a named type for co-occurrence entries.
type EntityCoOccEntry struct {
	Name  string
	Count int
}

// EntityAggregateData holds the full aggregate view for a named entity.
// Used as the engine-layer return type; MCP adapter projects it to mcp.EntityAggregate.
type EntityAggregateData struct {
	Record      *storage.EntityRecord
	Engrams     []*storage.Engram
	Relations   []storage.RelationshipRecord
	CoOccurring []EntityCoOccEntry
}

// GetEntityAggregate returns the full aggregate view for a named entity.
func (e *Engine) GetEntityAggregate(ctx context.Context, vault, entityName string, limit int) (*EntityAggregateData, error) {
	if limit <= 0 {
		limit = defaultEntityEngramLimit
	}

	ws := e.store.ResolveVaultPrefix(vault)

	// 1. Entity metadata record — vault-scoped since #683. A vault that has no
	// record for this name gets "not found", even when another vault does: the
	// pre-#683 global record made this call an existence-and-metadata oracle
	// over every other tenant's entity vocabulary (nonzero mention_count and
	// first_seen with an empty engram list).
	rec, err := e.store.GetEntityRecord(ctx, ws, entityName)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return nil, nil // not found
	}

	// 2. Engrams that mention this entity (vault-scoped via ScanEntityEngrams
	// reverse index), and — from the same pass — the set of entities that a LIVE
	// engram still co-mentions with this one.
	//
	// The scan continues past `limit` (which bounds only what is RETURNED) up to
	// liveSupportScanCap, because steps 3 and 4 below need to know whether each
	// derived edge still has any live engram behind it. Its cost is the same kind
	// already paid on this path: ScanEntityRelationships below already iterates
	// every engram referencing the entity, unbounded, with a Pebble prefix
	// iterator each.
	var engrams []*storage.Engram
	subject := keys.NormalizeEntityName(entityName)
	livePartners := make(map[string]struct{})
	live, capped := 0, false
	scanErr := e.store.ScanEntityEngrams(ctx, entityName, func(gotWS [8]byte, id storage.ULID) error {
		if gotWS != ws {
			return nil // different vault — skip
		}
		if live >= liveSupportScanCap {
			capped = true
			return errEntityScanCapped // sentinel to stop scanning
		}
		eng, err := e.store.GetEngram(ctx, ws, id)
		if err != nil || eng == nil {
			return nil // skip missing/deleted
		}
		if eng.State == storage.StateSoftDeleted || eng.State == storage.StateArchived {
			return nil
		}
		live++
		if len(engrams) < limit {
			engrams = append(engrams, eng)
		}
		if err := e.store.ScanEngramEntities(ctx, ws, id, func(name string) error {
			// Keyed by the NORMALIZED name, which is what the 0x24 and 0x1F
			// ledgers canonicalize on (keys.EntityNameHash hashes
			// NormalizeEntityName). A raw string set would treat "Aurora
			// Platform" and "aurora platform" as different partners and drop a
			// LIVE edge whose ledger entry happens to carry the other casing.
			if n := keys.NormalizeEntityName(name); n != subject {
				livePartners[n] = struct{}{}
			}
			return nil
		}); err != nil {
			// A partner set we could not read is not evidence of absence: treat
			// the whole derivation as unusable rather than silently retiring
			// edges that may be live.
			capped = true
			return errEntityScanCapped
		}
		return nil
	})
	if scanErr != nil && !errors.Is(scanErr, errEntityScanCapped) {
		return nil, scanErr
	}

	// Whether the derived-edge views may be filtered at all. Beyond the cap the
	// live-partner set is a SUBSET of the truth, and filtering on a subset
	// deletes real edges — strictly worse than reporting a stale one. Degrade to
	// the unfiltered (pre-#780) behaviour and say so.
	filterStale := !capped
	if capped {
		slog.Warn("entity aggregate: live-support scan hit its cap; co-occurrence and relationship views are reported unfiltered and may include edges whose only support was superseded (#780)",
			"entity", entityName, "cap", liveSupportScanCap)
	}
	hasLiveSupport := func(other string) bool {
		if !filterStale {
			return true
		}
		_, ok := livePartners[keys.NormalizeEntityName(other)]
		return ok
	}

	// 3. Relationships involving this entity (vault-scoped).
	// ScanEntityRelationships uses the 0x26 index for O(engrams-referencing-entity) lookup
	// instead of the O(all vault relationships) full scan that ScanRelationships would do.
	//
	// #780: a relationship record is derived state, and it is retired with the
	// engram that asserted it — but only a HARD delete cascades, so an edge whose
	// every supporting engram was superseded outlived its support and was still
	// reported as current. Filtered against the live-support set above, which is
	// the same test the `engrams` field in this very response already applies;
	// without it the aggregate contradicts itself.
	//
	// Subject matching is on the NORMALIZED name throughout, matching step 1's
	// hash-keyed record lookup. The previous raw `==` meant a caller who asked
	// for "aurora platform" got the record (found by hash) with an EMPTY
	// relationships/co_occurring list (matched by string) — a silent partial
	// answer, fixed here as a consequence of needing the same key on both sides.
	var rels []storage.RelationshipRecord
	err = e.store.ScanEntityRelationships(ctx, ws, entityName, func(r storage.RelationshipRecord) error {
		other := r.ToEntity
		if keys.NormalizeEntityName(other) == subject {
			other = r.FromEntity
		}
		if !hasLiveSupport(other) {
			return nil
		}
		rels = append(rels, r)
		return nil
	})
	if err != nil {
		return nil, err
	}

	// 4. Co-occurring entities (vault-scoped), top-N by count.
	//
	// The 0x24 count is a MONOTONE capture-time ledger: incremented on every
	// write and carry, decremented only by DeleteEngram (hard delete). No
	// supersession can correct it, and no user action short of destroying the
	// record ever will — so PRESENCE is decided at read time, against live
	// support, rather than by a capture-time instruction that a superseded
	// engram could never have issued. The count itself is left alone: it is a
	// historical strength signal and is honest as one.
	var coEntries []EntityCoOccEntry
	err = e.store.ScanEntityClusters(ctx, ws, entityCoOccurrenceMinCount, func(nameA, nameB string, count int) error {
		switch {
		case keys.NormalizeEntityName(nameA) == subject:
			if hasLiveSupport(nameB) {
				coEntries = append(coEntries, EntityCoOccEntry{Name: nameB, Count: count})
			}
		case keys.NormalizeEntityName(nameB) == subject:
			if hasLiveSupport(nameA) {
				coEntries = append(coEntries, EntityCoOccEntry{Name: nameA, Count: count})
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(coEntries, func(i, j int) bool { return coEntries[i].Count > coEntries[j].Count })
	if len(coEntries) > entityCoOccurrenceTopN {
		coEntries = coEntries[:entityCoOccurrenceTopN]
	}

	return &EntityAggregateData{
		Record:      rec,
		Engrams:     engrams,
		Relations:   rels,
		CoOccurring: coEntries,
	}, nil
}

// ListEntities returns EntityRecord summaries sorted by mention_count descending.
func (e *Engine) ListEntities(ctx context.Context, vault string, limit int, state string) ([]storage.EntityRecord, error) {
	if limit <= 0 {
		limit = defaultListEntitiesLimit
	}

	ws := e.store.ResolveVaultPrefix(vault)

	var records []storage.EntityRecord
	err := e.store.ScanVaultEntityNames(ctx, ws, func(name string) error {
		rec, err := e.store.GetEntityRecord(ctx, ws, name)
		if err != nil || rec == nil {
			return nil // skip missing
		}
		if state != "" && rec.State != state {
			return nil // filter by state
		}
		records = append(records, *rec)
		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Slice(records, func(i, j int) bool {
		return records[i].MentionCount > records[j].MentionCount
	})
	if len(records) > limit {
		records = records[:limit]
	}
	return records, nil
}
