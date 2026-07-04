package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/scrypster/muninndb/internal/storage/keys"
)

// Lease is an advisory ownership lease attached to an engram, giving a fleet of
// agents that share a vault work-queue semantics over the memories they process.
// It is stored as a sidecar record (0x2A key prefix) next to the engram it
// guards — an attribute of the work-memory itself, not a free-standing lock.
// A zero Lease (empty Owner) means the engram is unleased.
//
// Staleness is a pure function of (Heartbeat, TTLSeconds, now) evaluated at read
// time: a lease is live only while now-heartbeat <= ttl, so a crashed owner's
// hold falls away automatically once the TTL elapses, with no background reaper
// and no action required from the dead owner.
type Lease struct {
	// Owner uniquely identifies the holder across hosts and sessions,
	// conventionally "{host}:{session}". Empty means no owner.
	Owner string `json:"owner"`
	// Heartbeat is the Unix-nanosecond server timestamp of the last claim/refresh.
	Heartbeat int64 `json:"heartbeat"`
	// TTLSeconds is the lease duration chosen by the acquirer. The lease is stale
	// once now-heartbeat exceeds it; it must be positive for a held lease.
	TTLSeconds int64 `json:"ttl"`
}

// IsZero reports whether no owner holds the lease.
func (l Lease) IsZero() bool { return l.Owner == "" }

// Expired reports whether the lease is stale at the given server time.
// An unowned lease, or one with a non-positive TTL, is always expired.
func (l Lease) Expired(now time.Time) bool {
	if l.Owner == "" || l.TTLSeconds <= 0 {
		return true
	}
	age := now.UnixNano() - l.Heartbeat
	return age > l.TTLSeconds*int64(time.Second)
}

// Live reports whether the lease is currently held (owned and not expired).
func (l Lease) Live(now time.Time) bool {
	return l.Owner != "" && !l.Expired(now)
}

// GetLease reads the lease sidecar for an engram. A missing record yields the
// zero Lease (unleased), never an error.
func (ps *PebbleStore) GetLease(ctx context.Context, wsPrefix [8]byte, id ULID) (Lease, error) {
	val, err := Get(ps.db, keys.LeaseKey(wsPrefix, [16]byte(id)))
	if err != nil {
		return Lease{}, fmt.Errorf("get lease: %w", err)
	}
	if val == nil {
		return Lease{}, nil
	}
	var l Lease
	if err := json.Unmarshal(val, &l); err != nil {
		return Lease{}, fmt.Errorf("decode lease: %w", err)
	}
	return l, nil
}

// GetLeases batch-reads lease sidecars, returning one Lease per input id in the
// same order (the zero Lease for any absent record). Used by the recall path to
// evaluate work-queue visibility for a page of candidates.
func (ps *PebbleStore) GetLeases(ctx context.Context, wsPrefix [8]byte, ids []ULID) ([]Lease, error) {
	out := make([]Lease, len(ids))
	for i, id := range ids {
		l, err := ps.GetLease(ctx, wsPrefix, id)
		if err != nil {
			return nil, err
		}
		out[i] = l
	}
	return out, nil
}

// putLease writes the lease sidecar in a replicated batch.
func (ps *PebbleStore) putLease(wsPrefix [8]byte, id ULID, l Lease) error {
	val, err := json.Marshal(l)
	if err != nil {
		return fmt.Errorf("marshal lease: %w", err)
	}
	batch := ps.db.NewBatch()
	defer batch.Close()
	if err := batch.Set(keys.LeaseKey(wsPrefix, [16]byte(id)), val, nil); err != nil {
		return fmt.Errorf("stage lease set: %w", err)
	}
	if err := batch.Commit(pebble.NoSync); err != nil {
		return fmt.Errorf("commit lease: %w", err)
	}
	ps.replicateBatch(batch)
	return nil
}

// deleteLease removes the lease sidecar in a replicated batch. Idempotent —
// deleting an absent lease is a no-op.
func (ps *PebbleStore) deleteLease(wsPrefix [8]byte, id ULID) error {
	batch := ps.db.NewBatch()
	defer batch.Close()
	if err := batch.Delete(keys.LeaseKey(wsPrefix, [16]byte(id)), nil); err != nil {
		return fmt.Errorf("stage lease delete: %w", err)
	}
	if err := batch.Commit(pebble.NoSync); err != nil {
		return fmt.Errorf("commit lease delete: %w", err)
	}
	ps.replicateBatch(batch)
	return nil
}

// CASCondition expresses the expected current value that CompareAndSet guards
// against. A nil field is ignored; every non-nil field must match for the
// mutation to apply.
type CASCondition struct {
	// State, when non-nil, requires the engram's current lifecycle state to equal it.
	State *LifecycleState
	// Lease, when non-nil, requires the engram's current lease to equal it exactly
	// (owner, heartbeat and ttl). A pointer to the zero Lease requires "unleased".
	// Comparing the whole lease — not just the owner — makes a concurrent refresh,
	// which advances the heartbeat, invalidate a stale-reclaim attempt.
	Lease *Lease
}

// CASMutation expresses the new value CompareAndSet applies when the condition holds.
type CASMutation struct {
	// State, when non-nil, is the new lifecycle state.
	State *LifecycleState
	// Lease, when non-nil, replaces the lease sidecar. A non-empty Owner writes the
	// lease; the zero Lease (empty Owner) removes it, making the engram immediately
	// visible to recall again.
	Lease *Lease
}

// CASOutcome reports the result of a CompareAndSet. On conflict (Applied=false),
// State and Lease carry the current observed values so the caller can retry or
// report the live owner without a second read.
type CASOutcome struct {
	Applied bool
	State   LifecycleState
	Lease   Lease
}

// CompareAndSet atomically applies mut to the engram identified by id, but only
// if its current (state, lease) satisfies cond. It holds a per-engram stripe
// lock across the read-compare-write so concurrent CompareAndSet calls on the
// same engram serialize — this closes the lifecycle-state TOCTOU and is the
// primitive the ownership lease (Claim/Release) is built on.
//
// Returns Applied=false with the current values when the condition does not hold.
// A missing engram yields ErrNotFound.
func (ps *PebbleStore) CompareAndSet(ctx context.Context, wsPrefix [8]byte, id ULID, cond CASCondition, mut CASMutation) (CASOutcome, error) {
	mu := ps.casLocks.For(id[:])
	mu.Lock()
	defer mu.Unlock()

	metas, err := ps.GetMetadata(ctx, wsPrefix, []ULID{id})
	if err != nil {
		return CASOutcome{}, fmt.Errorf("cas get metadata: %w", err)
	}
	if len(metas) == 0 || metas[0] == nil {
		return CASOutcome{}, fmt.Errorf("engram %w", ErrNotFound)
	}
	cur := metas[0]
	curLease, err := ps.GetLease(ctx, wsPrefix, id)
	if err != nil {
		return CASOutcome{}, err
	}

	// Evaluate the condition; on any mismatch, report the current values.
	if cond.State != nil && *cond.State != cur.State {
		return CASOutcome{Applied: false, State: cur.State, Lease: curLease}, nil
	}
	if cond.Lease != nil && *cond.Lease != curLease {
		return CASOutcome{Applied: false, State: cur.State, Lease: curLease}, nil
	}

	newState := cur.State
	newLease := curLease

	// Apply the state change through the shared metadata path so the 0x0B state
	// index, caches, provenance and replication all stay consistent.
	if mut.State != nil {
		updated := *cur
		updated.State = *mut.State
		updated.UpdatedAt = time.Now()
		if err := ps.UpdateMetadata(ctx, wsPrefix, id, &updated); err != nil {
			return CASOutcome{}, fmt.Errorf("cas update state: %w", err)
		}
		newState = *mut.State
	}

	// Apply the lease change: a non-empty owner writes it, the zero Lease clears it.
	if mut.Lease != nil {
		if mut.Lease.Owner == "" {
			if err := ps.deleteLease(wsPrefix, id); err != nil {
				return CASOutcome{}, err
			}
			newLease = Lease{}
		} else {
			if err := ps.putLease(wsPrefix, id, *mut.Lease); err != nil {
				return CASOutcome{}, err
			}
			newLease = *mut.Lease
		}
	}

	return CASOutcome{Applied: true, State: newState, Lease: newLease}, nil
}
