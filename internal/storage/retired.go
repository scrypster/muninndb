package storage

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/prefix"
	"github.com/scrypster/muninndb/internal/storage/erf"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

// The retired set is an in-memory, per-vault mirror of which engrams are
// RETIRED — lifecycle state StateSoftDeleted (evolve-superseded versions,
// consolidated sources, plain forget) or StateArchived. A
// Link(relation=supersedes) target is NOT retired: Link stamps its ValidUntil
// and leaves it active, so it still competes for a slot (named residual). Retired engrams keep
// their HNSW vector and (for supersession) their FTS postings on purpose:
// COG-28 needs that evidence so a query worded like an old version still
// reaches the current head. What they must NOT do is take candidate slots
// from live memories in the index cut that happens BEFORE phase 6 can drop
// them (the retired-candidates defect: a 50-version chain took 44 of 45
// vector slots in a 2,000-memory vault). The recall pipeline consults this set
// to route through retired nodes without counting them as candidates.
//
// WHY IT LIVES IN THE STORE, AND WHERE IT IS DRIVEN FROM. Every commit of an
// engram record goes through commitBatch (cache_commit.go), which hands the
// batch to noteCommittedBatch WHETHER OR NOT Commit returned an error: every
// 0x02 metadata key the batch sets or deletes re-derives that engram's entry
// from the record now in Pebble. The batch only says which records MAY have
// changed; the record says what they are — so a batch that did not land
// changes nothing, and one that landed despite an error is not missed. No
// write path is enumerated, so an existing one nobody thought of (like the
// enrichment digest rewrite) is covered;
// TestRetiredSet_EveryRecordCommitGoesThroughCommitBatch scans this package's
// source to keep that true. Bulk raw-KV paths (vault clone, merge, import,
// clear) additionally RESET the vault's set.
//
// NO ON-DISK STATE. The set is derived: built lazily on a vault's first use by
// scanning the vault's 0x02 metadata records — the same records every
// incremental note re-reads — so the built set and the maintained set are the
// same function of the store and a fresh process derives what the running one
// holds (restart invariance). The 0x0B state index is deliberately NOT the
// source: it can diverge from the records (a stale-cache lost update leaves a
// record retired with no retired index entry, or the reverse), and a set
// built from it differed across a restart in the randomized differential.
//
// NOTES NEVER TRUST A PREVIOUS STATE. A note does not compare against what the
// writer believed the old state was (that came from a cache a snapshot read
// can refill with stale data); it re-reads the authoritative record, so two
// commits on one engram whose notes run out of order converge on the latest.
//
// REPLICATION FOLLOWERS apply the leader's batches straight to Pebble; the
// applier hands each applied repr to NoteCommittedRepr, and a full resnapshot
// calls ResetAllRetired (wired in cmd/muninn), so a follower's set follows the
// same rule as the leader's.
type retiredSet struct {
	mu    sync.RWMutex
	built bool
	ids   map[[16]byte]struct{}
}

// RetiredView is a read handle on one vault's retired set. Contains is safe
// for concurrent use with the store's maintenance hooks. The zero value (no
// set) reports nothing retired.
type RetiredView struct {
	s *retiredSet
}

// Contains reports whether id is currently retired in this vault.
func (v RetiredView) Contains(id [16]byte) bool {
	if v.s == nil {
		return false
	}
	v.s.mu.RLock()
	_, ok := v.s.ids[id]
	v.s.mu.RUnlock()
	return ok
}

// Len reports how many engrams are currently retired in this vault.
func (v RetiredView) Len() int {
	if v.s == nil {
		return 0
	}
	v.s.mu.RLock()
	n := len(v.s.ids)
	v.s.mu.RUnlock()
	return n
}

// RetiredIDs returns the vault's retired-set view, building it from the store
// on first use: one prefix scan over the vault's 0x02 metadata records
// (measured in TestRetiredSet_BuildCost). A build error leaves the set unbuilt
// so the next call retries, and is returned so recall can degrade to the
// pre-fix behaviour (nothing skipped) loudly rather than skip on partial data.
func (ps *PebbleStore) RetiredIDs(ctx context.Context, ws [8]byte) (RetiredView, error) {
	v, _ := ps.retired.LoadOrStore(ws, &retiredSet{})
	s := v.(*retiredSet)
	s.mu.RLock()
	built := s.built
	s.mu.RUnlock()
	if built {
		return RetiredView{s: s}, nil
	}
	// Build under the WRITE lock: a maintenance note that commits while the
	// scan runs blocks on this lock and applies after the build (notes are
	// idempotent re-reads), and a note that ran before the lock was taken saw
	// built=false and did nothing — the scan, which starts after its commit,
	// observes that state itself.
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.built {
		return RetiredView{s: s}, nil
	}
	// Scan the 0x01 ENGRAM records — the source of truth for liveness
	// (STO-23): recall's final filter and Read decide liveness from the same
	// record, so the set must too, or a store whose 0x01 and 0x02 disagree
	// hides a memory every reader calls live. Not the 0x0B state index
	// either: it can diverge from the records, and a set built from it
	// differed across a restart in the randomized differential. Measured in
	// TestRetiredSet_BuildCost.
	ids, err := ps.buildRetiredFromRecords(ctx, ws)
	if err != nil {
		return RetiredView{}, err
	}
	s.ids = ids
	s.built = true
	return RetiredView{s: s}, nil
}

// buildRetiredFromRecords derives a vault's retired set from its 0x01 engram
// records, both read from ONE snapshot. The 0x02 metadata records are walked
// alongside only as a consistency check: where the two disagree on
// liveness (a copy torn by a concurrent write before clone/merge read from a
// snapshot), 0x01 wins, the disagreement is counted
// (RetiredLivenessDisagreements) and one WARN is logged per vault. Nothing is
// rewritten.
func (ps *PebbleStore) buildRetiredFromRecords(ctx context.Context, ws [8]byte) (map[[16]byte]struct{}, error) {
	snap := ps.db.NewSnapshot()
	defer snap.Close()
	engLo := make([]byte, 9)
	engLo[0] = prefix.Engram
	copy(engLo[1:], ws[:])
	metaLo := make([]byte, 9)
	metaLo[0] = prefix.Meta
	copy(metaLo[1:], ws[:])

	engIt, err := snap.NewIter(&pebble.IterOptions{LowerBound: engLo, UpperBound: keys.PrefixUpperBound(append([]byte(nil), engLo...))})
	if err != nil {
		return nil, fmt.Errorf("build retired set: %w", err)
	}
	defer engIt.Close()
	metaIt, err := snap.NewIter(&pebble.IterOptions{LowerBound: metaLo, UpperBound: keys.PrefixUpperBound(append([]byte(nil), metaLo...))})
	if err != nil {
		return nil, fmt.Errorf("build retired set: %w", err)
	}
	defer metaIt.Close()

	ids := make(map[[16]byte]struct{})
	disagree := 0
	metaValid := metaIt.First()
	n := 0
	for valid := engIt.First(); valid; valid = engIt.Next() {
		if n++; n%4096 == 0 && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		k, v := engIt.Key(), engIt.Value()
		if len(k) != 25 || len(v) <= erf.OffsetState {
			continue
		}
		var id [16]byte
		copy(id[:], k[9:25])
		retired := isRetiredState(LifecycleState(v[erf.OffsetState]))
		if retired {
			ids[id] = struct{}{}
		}
		// Advance the metadata walk to this id (both are sorted by id).
		for metaValid {
			mk := metaIt.Key()
			if len(mk) != 25 {
				metaValid = metaIt.Next()
				continue
			}
			c := bytes.Compare(mk[9:25], id[:])
			if c < 0 {
				metaValid = metaIt.Next()
				continue
			}
			if c == 0 {
				mv := metaIt.Value()
				if len(mv) > erf.OffsetState && isRetiredState(LifecycleState(mv[erf.OffsetState])) != retired {
					disagree++
				}
				metaValid = metaIt.Next()
			}
			break
		}
	}
	if err := engIt.Error(); err != nil {
		return nil, fmt.Errorf("build retired set: %w", err)
	}
	if disagree > 0 {
		ps.retiredDisagreements.Add(int64(disagree))
		if _, warned := ps.retiredDisagreeWarned.LoadOrStore(ws, true); !warned {
			slog.Warn("storage: engram and metadata records disagree on liveness; trusting the engram record",
				"vault_prefix", fmt.Sprintf("%x", ws), "records", disagree)
		}
	}
	return ids, nil
}

// RetiredLivenessDisagreements reports how many engrams, across every
// retired-set build in this process, had a 0x02 metadata record that
// disagreed with their 0x01 engram record on liveness (0x01 was trusted).
func (ps *PebbleStore) RetiredLivenessDisagreements() int64 {
	return ps.retiredDisagreements.Load()
}

// recordRetired reads the authoritative 0x01 engram record for id — the
// record recall's filter and Read decide liveness from (STO-23) — and reports
// whether its lifecycle state is retired. A missing record (hard deleted) is
// not retired.
func (ps *PebbleStore) recordRetired(ws [8]byte, id [16]byte) (bool, error) {
	val, err := Get(ps.db, keys.EngramKey(ws, id))
	if err != nil {
		return false, err
	}
	if len(val) <= erf.OffsetState {
		return false, nil
	}
	return isRetiredState(LifecycleState(val[erf.OffsetState])), nil
}

func isRetiredState(s LifecycleState) bool {
	return s == StateSoftDeleted || s == StateArchived
}

// noteCommittedBatch drives the retired set from a batch whose commit has just
// been ATTEMPTED (commitBatch calls it on success and on error alike). Every
// 0x02 metadata key the batch sets or deletes names an engram whose lifecycle
// record may have changed; for each one the set entry is re-derived from the
// record as it is NOW in Pebble (see noteMeta). Nothing here consults a caller's
// "previous state", a cache, or a snapshot: an earlier design noted only when
// the writer's cached old state differed from the new one, and a recall's
// snapshot read refilling the metadata cache with a pre-forget "active" made a
// later restore look like a no-op, leaving a live memory in the set.
//
// A range deletion that covers metadata keys (vault clear) drops every set;
// the next reader rebuilds from the store.
func (ps *PebbleStore) noteCommittedBatch(b *pebble.Batch) {
	if b == nil || b.Empty() || !ps.anyRetiredSet() {
		return
	}
	// Every 0x01 or 0x02 key names an engram whose liveness may have changed;
	// each is re-derived once from its 0x01 record. Only a 0x01 SET's value
	// feeds noteMeta's fast path — a 0x02 value is not the liveness source.
	type touched struct {
		ws    [8]byte
		value []byte // the batch's 0x01 value, when it set one
	}
	var order [][16]byte
	seen := map[[16]byte]*touched{}
	r := b.Reader()
	for {
		kind, k, v, ok, err := r.Next()
		if !ok || err != nil {
			break
		}
		switch kind {
		case pebble.InternalKeyKindSet, pebble.InternalKeyKindDelete, pebble.InternalKeyKindSingleDelete:
			ws, id, isRec := parseRecordKey(k)
			if !isRec {
				continue
			}
			t := seen[id]
			if t == nil {
				t = &touched{ws: ws}
				seen[id] = t
				order = append(order, id)
			}
			if kind == pebble.InternalKeyKindSet && k[0] == prefix.Engram {
				t.value = v
			}
		case pebble.InternalKeyKindRangeDelete:
			if len(k) > 0 && k[0] <= prefix.Meta && (len(v) == 0 || v[0] >= prefix.Engram) {
				// A range inside one vault's 0x01 or 0x02 keyspace (a vault
				// clear issues one per prefix) resets only that vault.
				if len(k) >= 9 && len(v) >= 9 && k[0] == v[0] && (k[0] == prefix.Engram || k[0] == prefix.Meta) {
					var ws [8]byte
					copy(ws[:], k[1:9])
					if next, err := incrementWS(ws); err == nil && bytes.Equal(v[1:9], next[:]) {
						ps.resetRetired(ws)
						continue
					}
				}
				ps.ResetAllRetired()
			}
		}
	}
	for _, id := range order {
		t := seen[id]
		ps.noteMeta(t.ws, id, t.value, t.value != nil)
	}
}

// NoteCommittedRepr is noteCommittedBatch for a batch repr applied OUTSIDE
// this store — the replication applier on a follower, which writes the
// leader's batches straight to Pebble. Wired by cmd/muninn.
func (ps *PebbleStore) NoteCommittedRepr(repr []byte) {
	if len(repr) == 0 || !ps.anyRetiredSet() {
		return
	}
	b := ps.db.NewBatch()
	defer b.Close()
	if err := b.SetRepr(append([]byte(nil), repr...)); err != nil {
		// Unparseable: the safe answer is "rebuild from the store".
		ps.ResetAllRetired()
		return
	}
	ps.noteCommittedBatch(b)
}

// ResetAllRetired drops every vault's set; each is rebuilt from the store by
// its next reader. Used when local state was replaced wholesale (a follower's
// full resnapshot) and for any range deletion over engram metadata.
func (ps *PebbleStore) ResetAllRetired() {
	ps.retired.Range(func(k, _ any) bool {
		ps.retired.Delete(k)
		return true
	})
}

func (ps *PebbleStore) anyRetiredSet() bool {
	found := false
	ps.retired.Range(func(_, _ any) bool {
		found = true
		return false
	})
	return found
}

// parseRecordKey recognises an engram record key (0x01) or its metadata
// record key (0x02): ws|id, 25 bytes.
func parseRecordKey(k []byte) (ws [8]byte, id [16]byte, ok bool) {
	if len(k) != 25 || (k[0] != prefix.Meta && k[0] != prefix.Engram) {
		return ws, id, false
	}
	copy(ws[:], k[1:9])
	copy(id[:], k[9:25])
	return ws, id, true
}

// noteMeta updates one entry after an attempted write of its metadata record.
// The batch's value says what the write tried to store; the common case — an
// ACTIVE write of an id the set does not hold — needs nothing more (if it did
// not land, the record is unchanged and the set was already right about it). Otherwise the
// entry is re-derived from the record as it is now in Pebble, under the set's
// lock: two commits on one engram whose notes run out of order then both
// converge on the latest committed state, instead of the earlier note
// overwriting the later.
func (ps *PebbleStore) noteMeta(ws [8]byte, id [16]byte, committed []byte, set bool) {
	v, ok := ps.retired.Load(ws)
	if !ok {
		return
	}
	s := v.(*retiredSet)
	if set && len(committed) > erf.OffsetState && !isRetiredState(LifecycleState(committed[erf.OffsetState])) {
		s.mu.RLock()
		_, held := s.ids[id]
		s.mu.RUnlock()
		if !held {
			return
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.built {
		return
	}
	retired, err := ps.recordRetired(ws, id)
	if err != nil || !retired {
		// A read error drops the id: an engram the set wrongly calls retired
		// is invisible to default recall, so on doubt the safe side is "live".
		delete(s.ids, id)
		return
	}
	s.ids[id] = struct{}{}
}

// resetRetired drops a vault's set so the next reader rebuilds it from the
// store. Used by the bulk raw-KV paths (clear, clone, merge, import), which
// rewrite many records without passing through the per-engram hooks.
func (ps *PebbleStore) resetRetired(ws [8]byte) {
	ps.retired.Delete(ws)
}
