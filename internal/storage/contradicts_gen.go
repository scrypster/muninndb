package storage

import "sync/atomic"

// contradictsGen is a per-vault counter of RelContradicts association WRITES
// seen by this store.
//
// It exists so a consumer can cache the (expensive, O(all forward associations))
// DeclaredContradictions scan and know, for free, when its answer can no longer
// be trusted. The scan's result is a pure function of the vault's RelContradicts
// edges, so a counter of writes to those edges is a complete invalidation signal
// for ADDITIONS — the direction that matters, because a missed addition means a
// declared conflict is silently not reported.
//
// It is deliberately bumped in the STORE rather than in the engine's Link path,
// because the engine path misses the inline association writers used by
// Write/WriteBatch (an engram created with Associations attached). Every bump
// happens AFTER the batch commit that makes the edge readable — a bump before
// commit advertises a write Pebble cannot serve yet, and a concurrent scan in
// that window would cache an EMPTY result under the FRESH generation, an
// under-report that never self-heals.
//
// NAMED RESIDUAL 1 — replication is NOT covered, by any store-level hook.
// replication.Applier commits raw Pebble batches through *pebble.DB, below this
// store entirely (the same layering that produced #869's stale L1 caches), so a
// follower's counter never moves for a leader's declaration. Consumers must not
// trust this counter on a cluster follower: the engine's debt scan cache
// bypasses itself there (Engine.SetReplicaProbe). The correct wiring is an
// applier-level invalidation callback — the fix shape #869 introduces for the
// L1/meta caches — and when that lands, this generation should ride the same
// hook and the follower bypass can be removed.
//
// NAMED RESIDUAL 2 — association DELETION does not bump it. A contradicts edge
// that decays below AssocMinWeight and is pruned, or that is removed with a hard
// delete of an endpoint, can leave a cached scan listing a pair whose edge is
// gone. That is an OVER-warn, and the resolution rule downstream drops a pair
// whose endpoint no longer exists as dangling, so it is bounded and never
// silences a live conflict. Under-warning is the failure this counter prevents;
// over-warning is the residual it accepts.
type contradictsGen struct {
	m atomic.Pointer[map[[8]byte]uint64]
}

// noteContradictsWrite bumps the vault's counter when relType is RelContradicts.
// A no-op for every other relation, which is the overwhelming majority of
// association writes (the Hebbian co-activation worker writes RelCoActivated on
// a hot path; making it invalidate this cache would have made the cache useless).
func (ps *PebbleStore) noteContradictsWrite(ws [8]byte, relType RelType) {
	if relType != RelContradicts {
		return
	}
	ps.contradictsGen.bump(ws)
}

// ContradictsWriteGen returns the vault's current RelContradicts write count.
// A cache holding a DeclaredContradictions result is valid exactly while this
// value is unchanged.
func (ps *PebbleStore) ContradictsWriteGen(ws [8]byte) uint64 {
	return ps.contradictsGen.get(ws)
}

// Copy-on-write map behind an atomic pointer: reads are on the debt readout's
// path and must be lock-free, writes happen only when a contradiction is
// declared (rare by construction).
func (g *contradictsGen) bump(ws [8]byte) {
	for {
		cur := g.m.Load()
		next := make(map[[8]byte]uint64, 1)
		if cur != nil {
			for k, v := range *cur {
				next[k] = v
			}
		}
		next[ws]++
		if g.m.CompareAndSwap(cur, &next) {
			return
		}
	}
}

func (g *contradictsGen) get(ws [8]byte) uint64 {
	cur := g.m.Load()
	if cur == nil {
		return 0
	}
	return (*cur)[ws]
}
