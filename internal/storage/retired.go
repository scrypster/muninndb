package storage

import (
	"context"
	"fmt"
	"sync"

	"github.com/scrypster/muninndb/internal/storage/erf"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

// The retired set is an in-memory, per-vault mirror of which engrams are
// RETIRED — lifecycle state StateSoftDeleted (evolve/link-superseded versions,
// consolidated sources, plain forget) or StateArchived. Retired engrams keep
// their HNSW vector and (for supersession) their FTS postings on purpose:
// COG-28 needs that evidence so a query worded like an old version still
// reaches the current head. What they must NOT do is take candidate slots
// from live memories in the index cut that happens BEFORE phase 6 can drop
// them (the retired-candidates defect: a 50-version chain took 44 of 45
// vector slots in a 2,000-memory vault). The recall pipeline consults this set
// to route through retired nodes without counting them as candidates.
//
// WHY IT LIVES IN THE STORE. Every lifecycle transition commits here, at a
// handful of sites (UpdateMetadata — which CompareAndSet, restore and
// UpdateLifecycleState all route through — SoftDelete, DeleteEngram, the
// StoreBatch commit Evolve/Link/Consolidate use, WriteEngram(s)), so the
// mirror is updated at the commit point rather than at every engine call
// site that might change a state. Bulk raw-KV paths (vault clone, merge,
// import, clear) RESET the vault's set instead, so the next reader rebuilds it
// from the store.
//
// NO ON-DISK STATE. The set is derived: built lazily on a vault's first use by
// scanning the 0x0B state index for the two retired states, and each scanned
// id is VERIFIED against its 0x02 metadata record before it is admitted — a
// stale 0x0B entry must never hide a live memory (that is the dangerous
// direction; a retired engram missing from the set merely competes for a
// slot, which is the pre-fix behaviour). Because it is a pure function of the
// store, a fresh process derives the same set: restart invariance by
// construction.
//
// UPDATES RE-READ THE RECORD. A note after a commit does not trust the state
// the writer intended; it re-reads the authoritative 0x02 record. Two
// concurrent transitions on one engram whose notes run out of order therefore
// both converge on the latest committed state instead of the earlier note
// overwriting the later one.
//
// RESIDUAL (same class as the in-memory HNSW graph and the L1 cache): a
// replication FOLLOWER applies raw batches straight to Pebble and never calls
// these hooks, so a follower's set reflects whatever it built from its local
// store plus nothing after. A follower that serves recall must be restarted
// (or its vault reset) to pick up retirements applied by replication —
// exactly as its HNSW graph already must.
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
// on first use. The build is one prefix scan per retired state over the 0x0B
// index plus one 0x02 point read per hit (measured in
// TestRetiredSet_BuildCost). A build error leaves the set unbuilt so the next
// call retries, and is returned so recall can degrade to the pre-fix
// behaviour (nothing skipped) loudly rather than skip on partial data.
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
	ids := make(map[[16]byte]struct{})
	for _, st := range []LifecycleState{StateSoftDeleted, StateArchived} {
		err := ps.ScanEngramsByState(ctx, ws, st, func(id ULID) error {
			retired, err := ps.recordRetired(ws, [16]byte(id))
			if err != nil {
				return err
			}
			if retired {
				ids[[16]byte(id)] = struct{}{}
			}
			return nil
		})
		if err != nil {
			return RetiredView{}, fmt.Errorf("build retired set: %w", err)
		}
	}
	s.ids = ids
	s.built = true
	return RetiredView{s: s}, nil
}

// recordRetired reads the authoritative 0x02 metadata record for id and
// reports whether its lifecycle state is retired. A missing record (hard
// deleted) is not retired.
func (ps *PebbleStore) recordRetired(ws [8]byte, id [16]byte) (bool, error) {
	val, err := Get(ps.db, keys.MetaKey(ws, id))
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

// noteRetired re-derives id's membership from the authoritative record after a
// commit that may have changed its lifecycle state (or removed it). A vault
// whose set was never built is left alone — its first reader builds from the
// store. A read error DROPS the id from the set: an engram the set wrongly
// calls retired is invisible to default recall, so on doubt the safe side is
// "live" (it competes for a slot, as before the fix).
func (ps *PebbleStore) noteRetired(ws [8]byte, id [16]byte) {
	v, ok := ps.retired.Load(ws)
	if !ok {
		return
	}
	s := v.(*retiredSet)
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.built {
		return
	}
	retired, err := ps.recordRetired(ws, id)
	if err != nil || !retired {
		delete(s.ids, id)
		return
	}
	s.ids[id] = struct{}{}
}

// noteWritten is noteRetired for a full-record write whose committed state is
// known. The common case — an ACTIVE write of an id the set does not hold —
// needs no read at all, so ordinary writes pay one map probe.
func (ps *PebbleStore) noteWritten(ws [8]byte, id [16]byte, state LifecycleState) {
	if !isRetiredState(state) {
		v, ok := ps.retired.Load(ws)
		if !ok {
			return
		}
		s := v.(*retiredSet)
		s.mu.RLock()
		_, held := s.ids[id]
		s.mu.RUnlock()
		if !held {
			return
		}
	}
	ps.noteRetired(ws, id)
}

// resetRetired drops a vault's set so the next reader rebuilds it from the
// store. Used by the bulk raw-KV paths (clear, clone, merge, import), which
// rewrite many records without passing through the per-engram hooks.
func (ps *PebbleStore) resetRetired(ws [8]byte) {
	ps.retired.Delete(ws)
}
