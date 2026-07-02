package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/scrypster/muninndb/internal/storage"
)

// LeaseStatus is the outcome of a Claim attempt.
type LeaseStatus string

const (
	// LeaseAcquired: the engram had no live owner and is now held by the caller.
	LeaseAcquired LeaseStatus = "acquired"
	// LeaseRefreshed: the caller already owned the lease; its heartbeat advanced.
	LeaseRefreshed LeaseStatus = "refreshed"
	// LeaseReclaimed: a foreign owner's lease was stale and the caller took over.
	LeaseReclaimed LeaseStatus = "reclaimed"
	// LeaseConflict: a foreign, live owner holds the lease; it was left untouched.
	LeaseConflict LeaseStatus = "conflict"
)

// ClaimResult reports the outcome of Claim. On conflict, Owner and Heartbeat
// describe the live foreign holder so the caller learns who owns it without a
// second read.
type ClaimResult struct {
	Status    LeaseStatus
	Owner     string
	Heartbeat int64
}

// ReleaseResult reports the outcome of Release.
type ReleaseResult struct {
	// Released is true only when this call cleared a lease the caller held.
	Released bool
	// Owner is the remaining holder, if any (empty once released or already free).
	Owner string
}

// CompareAndSetResult reports the outcome of a lifecycle-state CompareAndSet.
// State and Owner describe the engram's current values (post-apply on success,
// or the conflicting current values when Applied is false).
type CompareAndSetResult struct {
	Applied bool
	State   string
	Owner   string
}

// casMaxRetries bounds the optimistic retry loop when a lease changes mid-claim.
// Contention on a single work item is low (a handful of agents), so a small
// bound suffices; exhaustion is reported to the caller as a conflict.
const casMaxRetries = 5

// CompareAndSet atomically transitions an engram's lifecycle state only if its
// current state matches expectState (when provided). It is the general-purpose
// primitive that closes the lifecycle-state TOCTOU. Either bound may be nil:
// a nil expectState skips the guard; a nil setState leaves the state unchanged.
func (e *Engine) CompareAndSet(ctx context.Context, vault, id string, expectState, setState *string) (CompareAndSetResult, error) {
	ulid, err := storage.ParseULID(id)
	if err != nil {
		return CompareAndSetResult{}, fmt.Errorf("parse id: %w", err)
	}
	ws := e.store.ResolveVaultPrefix(vault)

	var cond storage.CASCondition
	var mut storage.CASMutation
	if expectState != nil {
		st, err := storage.ParseLifecycleState(*expectState)
		if err != nil {
			return CompareAndSetResult{}, err
		}
		cond.State = &st
	}
	if setState != nil {
		st, err := storage.ParseLifecycleState(*setState)
		if err != nil {
			return CompareAndSetResult{}, err
		}
		mut.State = &st
	}

	out, err := e.store.CompareAndSet(ctx, ws, ulid, cond, mut)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return CompareAndSetResult{}, ErrEngramNotFound
		}
		return CompareAndSetResult{}, err
	}
	return CompareAndSetResult{Applied: out.Applied, State: out.State.String(), Owner: out.Lease.Owner}, nil
}

// Claim takes or refreshes an advisory ownership lease on an engram so that a
// fleet of agents can treat vault memories as a work queue. owner must be a
// stable identity unique across hosts and sessions (conventionally
// "{host}:{session}"), and ttlSecs is the lease duration chosen for this unit of
// work. A live foreign lease is never overwritten — the caller gets a conflict
// naming the current holder.
func (e *Engine) Claim(ctx context.Context, vault, id, owner string, ttlSecs int64) (ClaimResult, error) {
	if owner == "" {
		return ClaimResult{}, fmt.Errorf("claim: owner must not be empty")
	}
	if ttlSecs <= 0 {
		return ClaimResult{}, fmt.Errorf("claim: ttl_secs must be positive")
	}
	ulid, err := storage.ParseULID(id)
	if err != nil {
		return ClaimResult{}, fmt.Errorf("parse id: %w", err)
	}
	ws := e.store.ResolveVaultPrefix(vault)

	for attempt := 0; attempt < casMaxRetries; attempt++ {
		cur, err := e.store.GetLease(ctx, ws, ulid)
		if err != nil {
			return ClaimResult{}, err
		}
		now := time.Now()

		// A live foreign lease blocks the claim — never overwrite it.
		if cur.Live(now) && cur.Owner != owner {
			return ClaimResult{Status: LeaseConflict, Owner: cur.Owner, Heartbeat: cur.Heartbeat}, nil
		}

		// Otherwise acquire (unleased), refresh (already ours) or reclaim (foreign
		// but stale). Guard on the exact lease observed so a concurrent refresh —
		// which advances the heartbeat — aborts our attempt and we re-evaluate.
		expect := cur
		next := storage.Lease{Owner: owner, Heartbeat: now.UnixNano(), TTLSeconds: ttlSecs}
		out, err := e.store.CompareAndSet(ctx, ws, ulid,
			storage.CASCondition{Lease: &expect},
			storage.CASMutation{Lease: &next})
		if err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				return ClaimResult{}, ErrEngramNotFound
			}
			return ClaimResult{}, err
		}
		if out.Applied {
			status := LeaseAcquired
			switch {
			case cur.Owner == owner:
				status = LeaseRefreshed
			case cur.Owner != "":
				status = LeaseReclaimed
			}
			return ClaimResult{Status: status, Owner: owner, Heartbeat: next.Heartbeat}, nil
		}
		// The lease changed under us; re-read and re-evaluate.
	}

	// Persistent contention: report the current holder rather than spin forever.
	cur, err := e.store.GetLease(ctx, ws, ulid)
	if err != nil {
		return ClaimResult{}, err
	}
	return ClaimResult{Status: LeaseConflict, Owner: cur.Owner, Heartbeat: cur.Heartbeat}, nil
}

// Release relinquishes a lease held by owner, making the engram immediately
// visible to recall again without waiting for the TTL. It is idempotent: if the
// engram is unleased or held by someone else, it is left untouched and
// Released is false.
func (e *Engine) Release(ctx context.Context, vault, id, owner string) (ReleaseResult, error) {
	if owner == "" {
		return ReleaseResult{}, fmt.Errorf("release: owner must not be empty")
	}
	ulid, err := storage.ParseULID(id)
	if err != nil {
		return ReleaseResult{}, fmt.Errorf("parse id: %w", err)
	}
	ws := e.store.ResolveVaultPrefix(vault)

	for attempt := 0; attempt < casMaxRetries; attempt++ {
		cur, err := e.store.GetLease(ctx, ws, ulid)
		if err != nil {
			return ReleaseResult{}, err
		}
		if cur.Owner == "" {
			return ReleaseResult{Released: false}, nil
		}
		if cur.Owner != owner {
			return ReleaseResult{Released: false, Owner: cur.Owner}, nil
		}

		expect := cur
		out, err := e.store.CompareAndSet(ctx, ws, ulid,
			storage.CASCondition{Lease: &expect},
			storage.CASMutation{Lease: &storage.Lease{}}) // empty owner clears the lease
		if err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				return ReleaseResult{}, ErrEngramNotFound
			}
			return ReleaseResult{}, err
		}
		if out.Applied {
			return ReleaseResult{Released: true}, nil
		}
		// Raced with another writer; re-read and re-evaluate.
	}
	return ReleaseResult{Released: false}, nil
}
