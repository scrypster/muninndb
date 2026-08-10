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
// It is deliberately bumped in the STORE rather than in the engine's Link path.
// The engine path misses two writers that reach the same keys: the inline
// association writers used by Write/WriteBatch (an engram created with
// Associations attached), and — in cluster mode — replication applying a peer's
// write, which never calls Engine.Link. An engine-side hook list would have gone
// stale on a follower with no way to notice.
//
// NAMED RESIDUAL: association DELETION does not bump it. A contradicts edge that
// decays below AssocMinWeight and is pruned, or that is removed with a hard
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
